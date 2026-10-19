package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Health watchdog: the switch must work or reboot, never hang. A hung kernel
// (a task stuck on a lock, the switch path dead) still runs PID 1, which keeps
// feeding the hardware watchdog, so the board would stay up and useless.
//
// Every interval the daemon probes what a working switch needs:
//   - netlink answers (`ip link show <bridge>`): rtnl is not deadlocked;
//   - every port the bridge holds in blocking is blocked in the chip too
//     (KSZ port MSTP state register): the switch was not reset behind
//     Linux's back, which would open every blocked port into a loop;
//   - with a closed MRP ring (CHK_RC) the CPU keeps receiving: MRP_Test
//     frames alone are ~40/s, so a stalled conduit means a dead CPU path.
//
// A probe that does not return within hangAfter, or failMax failed probes in
// a row, reboots the board through sysrq: emergency sync + restart without
// the normal shutdown, which a hung kernel would hang in too. The systemd
// watchdog (WatchdogSec) is fed only while probes keep returning, so a stuck
// daemon is restarted by systemd.
//
// After a health reboot less than rebootBackoff ago, failed probes only log:
// a false positive must not turn into a reboot loop that locks the operator
// out. A hung probe always reboots.

const (
	kszPortMSTPState = 0x0b04 // port N: (N+1)<<12 | 0x0b04, MSTP entry 0
	kszPortTxEnable  = 0x04
	brStateBlocking  = "4"
)

// HealthStatus is the last probe result (GET /system "health").
type HealthStatus struct {
	OK       bool      `json:"ok"`
	Checked  time.Time `json:"checked"`
	Failures int       `json:"failures"`
	Error    string    `json:"error,omitempty"`
	Mode     string    `json:"mode"`
}

type health struct {
	r          Runner
	log        *log.Logger
	bridge     string
	netSys     string // /sys/class/net
	regmaps    string // /sys/kernel/debug/regmap
	conduit    func() string
	ringClosed func() bool
	hangFile   string // test hook: while it exists, probes hang

	interval, hangAfter, grace time.Duration
	failMax                    int
	stateFile                  string // records health reboots
	rebootBackoff              time.Duration
	reboot                     func(reason string)
	notify                     func(state string) error
	logOnly                    bool // -health=log: never reboot, only log

	mu       sync.Mutex
	st       HealthStatus
	lastDone time.Time // last probe that returned
	lastRx   uint64
	lastRxAt time.Time
	soft     bool // failed probes only log (recent health reboot)
}

func (h *health) Status() HealthStatus {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.st
}

// probe runs one check of everything; nil means healthy.
func (h *health) probe() error {
	if h.hangFile != "" {
		for {
			if _, err := os.Stat(h.hangFile); err != nil {
				break
			}
			time.Sleep(time.Second)
		}
	}
	if _, err := h.r.Output("ip", "-j", "link", "show", "dev", h.bridge); err != nil {
		return fmt.Errorf("netlink: %v", err)
	}
	if err := h.checkPortStates(); err != nil {
		return err
	}
	return h.checkCPURx()
}

// checkPortStates compares the bridge's blocking ports with the chip. A port
// can be in transition while it is read, so a mismatch is re-read once.
func (h *health) checkPortStates() error {
	dir, _, err := kszRegmap(h.regmaps)
	if err != nil {
		return nil // no ksz regmap (debugfs not mounted): nothing to compare
	}
	ports, _ := filepath.Glob(filepath.Join(h.netSys, h.bridge, "brif", "*"))
	for _, p := range ports {
		n := filepath.Base(p)
		reg, ok := h.mstpReg(n)
		if !ok {
			continue
		}
		bad := func() (bool, error) {
			st, err := os.ReadFile(filepath.Join(p, "state"))
			if err != nil || strings.TrimSpace(string(st)) != brStateBlocking {
				return false, nil
			}
			v, err := readReg(dir, reg)
			return err == nil && v&kszPortTxEnable != 0, err
		}
		if b, err := bad(); err != nil {
			return fmt.Errorf("ksz register 0x%04x: %v", reg, err)
		} else if b {
			time.Sleep(300 * time.Millisecond)
			if b, _ := bad(); b {
				return fmt.Errorf("%s is blocking in the bridge but forwards in the switch (0x%04x): switch reset?", n, reg)
			}
		}
	}
	return nil
}

// mstpReg maps a DSA user port (phys_port_name "p<index>") to its KSZ MSTP
// state register.
func (h *health) mstpReg(port string) (int, bool) {
	b, err := os.ReadFile(filepath.Join(h.netSys, port, "phys_port_name"))
	if err != nil {
		return 0, false
	}
	i, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(string(b)), "p"))
	if err != nil || i < 0 || i > 6 {
		return 0, false
	}
	return (i+1)<<12 | kszPortMSTPState, true
}

func (h *health) checkCPURx() error {
	c := ""
	if h.conduit != nil {
		c = h.conduit()
	}
	if c == "" {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(h.netSys, c, "statistics", "rx_packets"))
	if err != nil {
		return nil
	}
	rx, _ := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	// A probe that hung and came back may run next to a new one: compare
	// only samples at least 3 s apart.
	h.mu.Lock()
	prev, gap := h.lastRx, time.Since(h.lastRxAt)
	fresh := prev != 0 && gap < 3*time.Second
	if !fresh {
		h.lastRx, h.lastRxAt = rx, time.Now()
	}
	h.mu.Unlock()
	if fresh || prev == 0 || rx != prev {
		return nil
	}
	if h.ringClosed != nil && h.ringClosed() {
		return fmt.Errorf("%s received nothing for %v with a closed MRP ring: CPU path dead", c, gap.Round(time.Second))
	}
	return nil
}

// Run probes forever. Call it in its own goroutine.
func (h *health) Run() {
	h.soft = h.recentReboot()
	mode := "reboot"
	if h.logOnly {
		mode = "log"
	} else if h.soft {
		mode = "log (recent health reboot)"
		h.log.Printf("health: last health reboot less than %v ago: failed probes only log, hangs still reboot", h.rebootBackoff)
	}
	h.mu.Lock()
	h.st.Mode = mode
	h.mu.Unlock()
	go h.feed()
	time.Sleep(h.grace)
	fails := 0
	for {
		done := make(chan error, 1)
		go func() { done <- h.probe() }()
		var err error
		select {
		case err = <-done:
		case <-time.After(h.hangAfter):
			err = fmt.Errorf("health probe hung for %v", h.hangAfter)
			h.doReboot(err.Error())
			if !h.logOnly {
				return
			}
		}
		if err == nil {
			fails = 0
		} else {
			fails++
			h.log.Printf("health: probe failed (%d/%d): %v", fails, h.failMax, err)
		}
		h.mu.Lock()
		h.st = HealthStatus{OK: err == nil, Checked: time.Now(), Failures: fails, Mode: mode}
		if err != nil {
			h.st.Error = err.Error()
		}
		h.lastDone = time.Now()
		h.mu.Unlock()
		if fails >= h.failMax && !h.soft {
			h.doReboot(err.Error())
			if !h.logOnly {
				return
			}
		}
		time.Sleep(h.interval)
	}
}

// feed pings the systemd watchdog while probes keep returning (and during
// grace). Failed probes are handled by Run; a stuck probe loop is not.
func (h *health) feed() {
	start := time.Now()
	for range time.Tick(h.interval) {
		h.mu.Lock()
		ok := time.Since(start) < h.grace+h.hangAfter || time.Since(h.lastDone) < 2*h.interval+h.hangAfter
		h.mu.Unlock()
		if ok {
			_ = h.notify("WATCHDOG=1")
		}
	}
}

func (h *health) recentReboot() bool {
	fi, err := os.Stat(h.stateFile)
	return err == nil && time.Since(fi.ModTime()) < h.rebootBackoff
}

func (h *health) doReboot(reason string) {
	if h.logOnly {
		h.log.Printf("health: would reboot (-health=log): %s", reason)
		return
	}
	h.log.Printf("health: REBOOT: %s", reason)
	// The file write may block on a hung filesystem: do not wait for it.
	done := make(chan struct{})
	go func() {
		_ = os.MkdirAll(filepath.Dir(h.stateFile), 0o755)
		_ = os.WriteFile(h.stateFile, []byte(time.Now().Format(time.RFC3339)+" "+reason+"\n"), 0o644)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
	h.reboot(reason)
}

// sysrqReboot logs to the kernel log (the serial console keeps it), then
// syncs and restarts immediately through sysrq: no shutdown sequence that a
// hung kernel could block. /proc/sysrq-trigger works regardless of
// kernel.sysrq.
func sysrqReboot(reason string) {
	_ = os.WriteFile("/dev/kmsg", []byte("napi-switchd: health reboot: "+reason+"\n"), 0)
	trigger := func(c string) {
		done := make(chan struct{})
		go func() { _ = os.WriteFile("/proc/sysrq-trigger", []byte(c), 0); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
	}
	trigger("s")
	time.Sleep(2 * time.Second)
	trigger("b")
}

// sdNotify sends a state to systemd ($NOTIFY_SOCKET); no-op without it.
func sdNotify(state string) error {
	path := os.Getenv("NOTIFY_SOCKET")
	if path == "" {
		return nil
	}
	if strings.HasPrefix(path, "@") {
		path = "\x00" + path[1:]
	}
	c, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		return err
	}
	defer c.Close()
	_, err = c.Write([]byte(state))
	return err
}
