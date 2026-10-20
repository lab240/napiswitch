package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type okRunner struct{}

func (okRunner) Output(...string) ([]byte, error) { return nil, nil }
func (okRunner) Exec(Cmd) error                   { return nil }

// fakeSwitch: /sys/class/net with br0 + lan1 (p0) and a ksz regmap whose
// MSTP state register of port 1 is mstp; the CPU port (6) has tail tagging.
func fakeSwitch(t *testing.T, brState string, mstp int) *health {
	t.Helper()
	return fakeSwitchRegs(t, brState, map[int]int{0x1b04: mstp, 0x6020: kszPortTailTag})
}

func fakeSwitchRegs(t *testing.T, brState string, regs map[int]int) *health {
	t.Helper()
	d := t.TempDir()
	net, rm := filepath.Join(d, "net"), filepath.Join(d, "regmap", "spi2.0-8")
	for p, c := range map[string]string{
		"br0/brif/lan1/state":          brState + "\n",
		"lan1/phys_port_name":          "p0\n",
		"trunk0/statistics/rx_packets": "100\n",
	} {
		f := filepath.Join(net, p)
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var b strings.Builder
	for r := 0; r <= kszPorts<<12|kszPortCtrl0; r++ {
		fmt.Fprintf(&b, "%06x: %02x\n", r, regs[r])
	}
	if err := os.MkdirAll(rm, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(rm, "registers"), []byte(b.String()), 0o644)
	os.WriteFile(filepath.Join(rm, "name"), []byte("ksz-switch\n"), 0o644)
	return &health{r: okRunner{}, log: log.New(io.Discard, "", 0), bridge: "br0", netSys: net,
		regmaps: filepath.Join(d, "regmap"), conduit: func() string { return "trunk0" },
		stateFile: filepath.Join(d, "state", "health-reboot"), rebootBackoff: time.Minute,
		notify: func(string) error { return nil }}
}

func TestHealthPortStates(t *testing.T) {
	if err := fakeSwitch(t, "4", 0x01).probe(); err != nil {
		t.Errorf("blocked in bridge and chip: %v", err)
	}
	if err := fakeSwitch(t, "3", 0x06).probe(); err != nil {
		t.Errorf("forwarding in both: %v", err)
	}
	// Chip reset to defaults: the port forwards although the bridge blocks it.
	if err := fakeSwitch(t, "4", 0x06).probe(); err == nil || !strings.Contains(err.Error(), "lan1") {
		t.Errorf("blocking in bridge, forwarding in chip: want error, got %v", err)
	}
}

func TestHealthChipReset(t *testing.T) {
	// Defaults after a chip reset: no tail tagging anywhere. The ring is open
	// (CHK_RO), both ports forward in bridge and chip: only this check fails.
	err := fakeSwitchRegs(t, "3", map[int]int{0x1b04: 0x06}).probe()
	if err == nil || !strings.Contains(err.Error(), "tail tagging") {
		t.Errorf("chip reset: want tail-tagging error, got %v", err)
	}
	if err := fakeSwitchRegs(t, "3", map[int]int{0x1b04: 0x06, 0x6020: 0x07}).probe(); err != nil {
		t.Errorf("tail tagging on: %v", err)
	}
}

func TestHealthCPURx(t *testing.T) {
	h := fakeSwitch(t, "3", 0x06)
	closed := false
	h.ringClosed = func() bool { return closed }
	for i := 0; i < 2; i++ {
		h.lastRxAt = time.Now().Add(-5 * time.Second)
		if err := h.probe(); err != nil {
			t.Fatalf("open ring, no rx: %v", err)
		}
	}
	// Samples closer than 3 s (a hung probe came back next to a new one)
	// are not compared.
	closed = true
	if err := h.probe(); err != nil {
		t.Errorf("closed ring, samples 0 s apart: %v", err)
	}
	closed = true
	h.lastRxAt = time.Now().Add(-5 * time.Second)
	if err := h.probe(); err == nil {
		t.Error("closed ring, rx stalled: want error")
	}
	os.WriteFile(filepath.Join(h.netSys, "trunk0/statistics/rx_packets"), []byte("140\n"), 0o644)
	h.lastRxAt = time.Now().Add(-5 * time.Second)
	if err := h.probe(); err != nil {
		t.Errorf("closed ring, rx moving: %v", err)
	}
}

func runHealth(t *testing.T, h *health) (string, bool) {
	t.Helper()
	var mu sync.Mutex
	var reason string
	h.interval, h.hangAfter, h.grace, h.failMax = 10*time.Millisecond, 600*time.Millisecond, 0, 3
	h.reboot = func(r string) { mu.Lock(); reason = r; mu.Unlock() }
	done := make(chan struct{})
	go func() { h.Run(); close(done) }()
	select {
	case <-done:
		mu.Lock()
		defer mu.Unlock()
		return reason, true
	case <-time.After(3 * time.Second):
		return "", false
	}
}

func TestHealthRebootOnFailures(t *testing.T) {
	h := fakeSwitch(t, "4", 0x06)
	r, ok := runHealth(t, h)
	if !ok || !strings.Contains(r, "switch reset") {
		t.Fatalf("want reboot for switch reset, got %q (rebooted %v)", r, ok)
	}
	if _, err := os.Stat(h.stateFile); err != nil {
		t.Errorf("health reboot not recorded: %v", err)
	}
}

func TestHealthRebootOnHang(t *testing.T) {
	h := fakeSwitch(t, "4", 0x01)
	h.hangFile = filepath.Join(t.TempDir(), "hang")
	os.WriteFile(h.hangFile, nil, 0o644)
	if r, ok := runHealth(t, h); !ok || !strings.Contains(r, "hung") {
		t.Fatalf("want reboot for hung probe, got %q (rebooted %v)", r, ok)
	}
}

func TestHealthBackoffAfterRecentReboot(t *testing.T) {
	h := fakeSwitch(t, "4", 0x06)
	os.MkdirAll(filepath.Dir(h.stateFile), 0o755)
	os.WriteFile(h.stateFile, []byte("earlier\n"), 0o644)
	if r, ok := runHealth(t, h); ok {
		t.Fatalf("recent health reboot: failed probes must only log, got reboot %q", r)
	}
	if st := h.Status(); st.OK || st.Failures < 3 || !strings.HasPrefix(st.Mode, "log") {
		t.Errorf("status %+v", st)
	}
}
