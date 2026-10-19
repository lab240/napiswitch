package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// TEMPORARY WORKAROUND for a board bug, to be removed once all boards are
// reworked (R29 moved from GND to DVDIO). Delete this file, kmod/kszreg,
// the -rmii-fix/-kszreg-module flags and the kszreg step of install.sh.
//
// RMII clocking of the CPU port.
//
// On the NAPI-Switch prototype the SoC drives the 50 MHz RMII reference clock into the
// KSZ9567 TX_CLK6/REFCLKI6 pin (DT clock_in_out = "output"), but the board
// straps RXD6_1 low, which puts port 6 into RMII "Clock Mode": the switch
// then ignores REFCLKI6 and runs on its own 50 MHz derived from its crystal.
// Two free-running clocks on one RMII link corrupt frames in both
// directions (CRC errors growing with frame length).
//
// Setting bit 2 of XMII Port Control 1 (0x6301) selects RMII "Normal Mode",
// where the switch uses the reference clock on REFCLKI6, i.e. the SoC's.
// The debugfs regmap view is read-only, so the write goes through a tiny
// one-shot kernel module (kmod/kszreg) that uses the ksz driver's regmap.

const (
	regXMIICtrl1  = 0x6301
	xmiiRMIINorm  = 0x04 // 1 = RMII reference clock received on REFCLKI6
	xmiiIfaceMask = 0x03
	xmiiIfaceRMII = 0x01
)

// RMIIStatus is reported by GET /system.
type RMIIStatus struct {
	// Clock: "soc" (switch uses the SoC clock: correct), "switch" (switch
	// runs on its own clock while the SoC also drives one: frames get
	// corrupted), "n/a" (not an RMII port or the SoC takes the clock).
	Clock   string    `json:"clock"`
	Reg     string    `json:"xmii_ctrl1"`  // 0x6301 as read last
	Fixed   int       `json:"fixed_count"` // times the daemon switched it
	Checked time.Time `json:"checked"`
	Error   string    `json:"error,omitempty"`
}

type rmiiFixer struct {
	r       Runner
	log     *log.Logger
	regmaps string // /sys/kernel/debug/regmap
	devtree string // /sys/class/net (conduit -> device/of_node)
	module  string // kszreg.ko path, %s = kernel release
	conduit func() string

	mu sync.Mutex
	st RMIIStatus
}

func (f *rmiiFixer) Status() RMIIStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.st
}

// socDrivesClock reports whether the conduit MAC is set to output the RMII
// reference clock (Rockchip DT clock_in_out = "output").
func (f *rmiiFixer) socDrivesClock() bool {
	c := f.conduit()
	if c == "" {
		return false
	}
	b, err := os.ReadFile(filepath.Join(f.devtree, c, "device", "of_node", "clock_in_out"))
	return err == nil && strings.TrimRight(string(b), "\x00\n") == "output"
}

// Check reads the current mode and, when the switch is in RMII Clock Mode
// while the SoC drives the clock, switches it to Normal Mode.
func (f *rmiiFixer) Check() RMIIStatus {
	st := f.Status()
	st.Checked, st.Error = time.Now(), ""
	defer func() {
		f.mu.Lock()
		f.st = st
		f.mu.Unlock()
	}()
	dir, dev, err := kszRegmap(f.regmaps)
	if err != nil {
		st.Clock, st.Error = "unknown", err.Error()
		return st
	}
	v, err := readReg(dir, regXMIICtrl1)
	if err != nil {
		st.Clock, st.Error = "unknown", err.Error()
		return st
	}
	st.Reg = fmt.Sprintf("0x%02x", v)
	switch {
	case v&xmiiIfaceMask != xmiiIfaceRMII || !f.socDrivesClock():
		st.Clock = "n/a"
		return st
	case v&xmiiRMIINorm != 0:
		st.Clock = "soc"
		return st
	}
	st.Clock = "switch"
	mod := f.module
	if strings.Contains(mod, "%s") {
		rel, _ := os.ReadFile("/proc/sys/kernel/osrelease")
		mod = fmt.Sprintf(mod, strings.TrimSpace(string(rel)))
	}
	if _, err := os.Stat(mod); err != nil {
		st.Error = fmt.Sprintf("RMII clock mismatch, cannot fix: %v (run deploy/install.sh to build the module)", err)
		f.log.Printf("rmii: %s", st.Error)
		return st
	}
	// The module never stays loaded: insmod always "fails" with EAGAIN.
	_ = f.r.Exec(cmdIgn("insmod", mod, "dev="+dev, fmt.Sprintf("reg=0x%x", regXMIICtrl1),
		fmt.Sprintf("mask=0x%x", xmiiRMIINorm), fmt.Sprintf("val=0x%x", xmiiRMIINorm)))
	if v2, err := readReg(dir, regXMIICtrl1); err != nil {
		st.Error = err.Error()
	} else if st.Reg = fmt.Sprintf("0x%02x", v2); v2&xmiiRMIINorm != 0 {
		st.Clock = "soc"
		st.Fixed++
		f.log.Printf("rmii: switch port 6 was in RMII Clock Mode (0x%02x) while the SoC drives the clock; set Normal Mode (0x%02x)", v, v2)
	} else {
		st.Error = fmt.Sprintf("RMII Normal Mode write did not take effect (0x%02x)", v2)
		f.log.Printf("rmii: %s", st.Error)
	}
	return st
}

// Watch re-checks periodically: a switch reset or driver reprobe would
// bring the strap value back.
func (f *rmiiFixer) Watch(every time.Duration) {
	for range time.Tick(every) {
		f.Check()
	}
}
