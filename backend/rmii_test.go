package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeKSZ is a regmap debugfs view with the registers around 0x6301 and an
// insmod that applies the module's update to it.
type fakeKSZ struct {
	t     *testing.T
	dir   string
	val   int
	cmds  []string
	takes bool // the write takes effect
}

func (k *fakeKSZ) write() {
	var b strings.Builder
	for r := 0; r <= 0x6302; r++ {
		v := 0
		if r == 0x6301 {
			v = k.val
		}
		fmt.Fprintf(&b, "%06x: %02x\n", r, v)
	}
	if err := os.WriteFile(filepath.Join(k.dir, "registers"), []byte(b.String()), 0o644); err != nil {
		k.t.Fatal(err)
	}
}

func (k *fakeKSZ) Output(...string) ([]byte, error) { return nil, nil }
func (k *fakeKSZ) Exec(c Cmd) error {
	k.cmds = append(k.cmds, c.String())
	if c.Args[0] == "insmod" && k.takes {
		k.val |= 0x04
		k.write()
	}
	return fmt.Errorf("insmod: Resource temporarily unavailable")
}

func newFixer(t *testing.T, reg int, clockInOut string, takes bool) (*rmiiFixer, *fakeKSZ) {
	root := t.TempDir()
	rm := filepath.Join(root, "regmap", "spi2.0-8")
	of := filepath.Join(root, "net", "trunk0", "device", "of_node")
	for _, d := range []string{rm, of, filepath.Join(root, "regmap", "spi2.0-16")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(rm, "name"), []byte("ksz-switch\n"), 0o644)
	os.WriteFile(filepath.Join(of, "clock_in_out"), []byte(clockInOut+"\x00"), 0o644)
	mod := filepath.Join(root, "kszreg.ko")
	os.WriteFile(mod, []byte("x"), 0o644)
	k := &fakeKSZ{t: t, dir: rm, val: reg, takes: takes}
	k.write()
	return &rmiiFixer{r: k, log: log.New(io.Discard, "", 0), regmaps: filepath.Join(root, "regmap"),
		devtree: filepath.Join(root, "net"), module: mod, conduit: func() string { return "trunk0" }}, k
}

func TestReadReg(t *testing.T) {
	_, k := newFixer(t, 0x41, "output", true)
	if v, err := readReg(k.dir, 0x6301); err != nil || v != 0x41 {
		t.Fatalf("got 0x%x %v", v, err)
	}
	if v, err := readReg(k.dir, 0x6300); err != nil || v != 0 {
		t.Fatalf("neighbour: 0x%x %v", v, err)
	}
}

func TestRMIIFixClockMode(t *testing.T) {
	f, k := newFixer(t, 0x41, "output", true)
	st := f.Check()
	if st.Clock != "soc" || st.Reg != "0x45" || st.Fixed != 1 || st.Error != "" {
		t.Fatalf("status %+v", st)
	}
	if len(k.cmds) != 1 || !strings.HasPrefix(k.cmds[0], "insmod ") || !strings.Contains(k.cmds[0], "dev=spi2.0 reg=0x6301 mask=0x4 val=0x4") {
		t.Fatalf("cmds %q", k.cmds)
	}
	// Second check: already in Normal Mode, nothing to do.
	if st = f.Check(); st.Clock != "soc" || st.Fixed != 1 || len(k.cmds) != 1 {
		t.Fatalf("recheck %+v %q", st, k.cmds)
	}
}

func TestRMIILeftAlone(t *testing.T) {
	for _, c := range []struct {
		name, clk string
		reg       int
		want      string
	}{
		{"already normal mode", "output", 0x45, "soc"},
		{"SoC takes the clock", "input", 0x41, "n/a"},
		{"RGMII port", "output", 0x40, "n/a"},
	} {
		f, k := newFixer(t, c.reg, c.clk, true)
		if st := f.Check(); st.Clock != c.want || len(k.cmds) != 0 {
			t.Errorf("%s: %+v %q", c.name, st, k.cmds)
		}
	}
}

func TestRMIIFixFailures(t *testing.T) {
	f, _ := newFixer(t, 0x41, "output", false)
	if st := f.Check(); st.Clock != "switch" || !strings.Contains(st.Error, "did not take effect") {
		t.Fatalf("write ignored: %+v", st)
	}
	f, k := newFixer(t, 0x41, "output", true)
	f.module = filepath.Join(filepath.Dir(f.module), "missing-%s.ko")
	if st := f.Check(); st.Clock != "switch" || !strings.Contains(st.Error, "cannot fix") || len(k.cmds) != 0 {
		t.Fatalf("no module: %+v", st)
	}
}
