package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const ptpBaseConf = `[global]
twoStepFlag             0
time_stamping           p2p1step
network_transport       L2
delay_mechanism         P2P
domainNumber            0
priority1               64
priority2               248
[lan1]
`

func hasGlobalLine(conf, key, val string) bool {
	inGlobal := false
	for _, ln := range strings.Split(conf, "\n") {
		t := strings.TrimSpace(ln)
		if len(t) > 1 && t[0] == '[' {
			inGlobal = t == "[global]"
			continue
		}
		if inGlobal {
			f := strings.Fields(t)
			if len(f) == 2 && f[0] == key && f[1] == val {
				return true
			}
		}
	}
	return false
}

func TestBuildPTPConfSlave(t *testing.T) {
	out := buildPTPConf(ptpBaseConf, "slave", 128)
	if !hasGlobalLine(out, "slaveOnly", "1") {
		t.Errorf("slave mode must set slaveOnly 1:\n%s", out)
	}
	if !hasGlobalLine(out, "priority1", "128") {
		t.Errorf("priority1 not set to 128:\n%s", out)
	}
	// existing keys preserved, interface section kept
	if !hasGlobalLine(out, "network_transport", "L2") || !strings.Contains(out, "[lan1]") {
		t.Errorf("base config not preserved:\n%s", out)
	}
	// priority1 must appear exactly once (replaced, not duplicated)
	if n := strings.Count(out, "priority1 "); n != 1 {
		t.Errorf("priority1 appears %d times, want 1:\n%s", n, out)
	}
	if n := strings.Count(out, "slaveOnly"); n != 1 {
		t.Errorf("slaveOnly appears %d times, want 1:\n%s", n, out)
	}
}

func TestBuildPTPConfMaster(t *testing.T) {
	// start from a conf that had slaveOnly 1, switch to master
	in := strings.Replace(ptpBaseConf, "[lan1]", "slaveOnly               1\n[lan1]", 1)
	out := buildPTPConf(in, "master", 64)
	if !hasGlobalLine(out, "slaveOnly", "0") {
		t.Errorf("master mode must set slaveOnly 0:\n%s", out)
	}
	if !hasGlobalLine(out, "priority1", "64") {
		t.Errorf("priority1 not set to 64:\n%s", out)
	}
}

func TestReadPTPConfig(t *testing.T) {
	write := func(conf string) string {
		p := filepath.Join(t.TempDir(), "ptp.conf")
		if err := os.WriteFile(p, []byte(conf), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// master default (no slaveOnly)
	if c := readPTPConfig(write(ptpBaseConf)); c.Mode != "master" || c.Priority1 != 64 {
		t.Errorf("got %+v, want master/64", c)
	}
	// round-trip: build slave conf, read it back
	if c := readPTPConfig(write(buildPTPConf(ptpBaseConf, "slave", 200))); c.Mode != "slave" || c.Priority1 != 200 {
		t.Errorf("got %+v, want slave/200", c)
	}
	// missing file -> nil
	if c := readPTPConfig(filepath.Join(t.TempDir(), "nope.conf")); c != nil {
		t.Errorf("missing file should give nil, got %+v", c)
	}
}
