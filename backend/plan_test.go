package main

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

var testPorts = []string{"lan1", "lan2", "lan3"}

func testRunning() *Running {
	all := []string{"10baseT/Half", "10baseT/Full", "100baseT/Half", "100baseT/Full", "1000baseT/Full"}
	run := &Running{
		Conduit: "trunk0", BridgeMAC: "f6:3e:43:2b:92:47", LocalFDB: map[int]bool{},
		Bridge: RunBridge{Present: true, STP: true, Priority: 32768, HelloTime: 2, ForwardDelay: 15, MaxAge: 20,
			AgeingTime: 300, Snooping: true, DefaultPVID: 1},
		BridgeVLANs: []RunVLAN{{ID: 1, PVID: true, Untagged: true}},
		Ports:       map[string]*RunPort{},
	}
	for _, n := range testPorts {
		run.Ports[n] = &RunPort{Name: n, Up: true, Master: "br0", Learning: true, Priority: 32, Cost: 100,
			VLANs: []RunVLAN{{ID: 1, PVID: true, Untagged: true}},
			Link:  LinkInfo{Autoneg: true, Supported: all, Advertised: all}}
	}
	return run
}

func strs(c []Cmd) []string {
	s := make([]string, len(c))
	for i, x := range c {
		s[i] = x.String()
	}
	return s
}

func TestImportedConfigIsNoop(t *testing.T) {
	run := testRunning()
	c := ImportConfig(run)
	if _, err := c.Validate(testPorts); err != nil {
		t.Fatal(err)
	}
	if p := Plan(c, run, PlanOpts{Bridge: "br0"}); len(p) != 0 {
		t.Fatalf("expected no commands, got %q", strs(p))
	}
}

func TestEnableVLANAwareResyncs(t *testing.T) {
	run := testRunning()
	c := ImportConfig(run)
	c.Bridge.VLANAware = true
	c.VLANs = append(c.VLANs, VLAN{ID: 10, Name: "plc"})
	c.Ports["lan2"].PVID = 10
	c.Ports["lan3"].Tagged = []int{10}
	got := strs(Plan(c, run, PlanOpts{Bridge: "br0"}))
	want := []string{
		"ip link set dev br0 type bridge vlan_filtering 1",
		"bridge vlan del vid 1 dev lan1", "bridge vlan add vid 1 dev lan1 pvid untagged",
		"bridge vlan del vid 1 dev lan2", "bridge vlan add vid 10 dev lan2 pvid untagged",
		"bridge vlan del vid 1 dev lan3", "bridge vlan add vid 1 dev lan3 pvid untagged", "bridge vlan add vid 10 dev lan3",
		"bridge fdb add f6:3e:43:2b:92:47 dev br0 self local vlan 1",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestDisableVLANAwareResetsPorts(t *testing.T) {
	run := testRunning()
	run.Bridge.VLANAware = true
	run.LocalFDB[1] = true
	run.Ports["lan2"].VLANs = []RunVLAN{{ID: 10, PVID: true, Untagged: true}}
	run.Ports["lan3"].VLANs = append(run.Ports["lan3"].VLANs, RunVLAN{ID: 10})
	c := ImportConfig(run)
	c.Bridge.VLANAware = false
	got := strs(Plan(c, run, PlanOpts{Bridge: "br0"}))
	want := []string{
		"ip link set dev br0 type bridge vlan_filtering 0",
		"bridge vlan add vid 1 dev lan2 pvid untagged", "bridge vlan del vid 10 dev lan2",
		"bridge vlan del vid 10 dev lan3",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got:\n%s", strings.Join(got, "\n"))
	}
}

func TestPVIDMoveAddsBeforeDelete(t *testing.T) {
	run := testRunning()
	run.Bridge.VLANAware = true
	run.LocalFDB[1] = true
	c := ImportConfig(run)
	c.VLANs = append(c.VLANs, VLAN{ID: 20})
	c.Ports["lan1"].PVID = 20
	got := strs(Plan(c, run, PlanOpts{Bridge: "br0"}))
	want := []string{"bridge vlan add vid 20 dev lan1 pvid untagged", "bridge vlan del vid 1 dev lan1"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q", got)
	}
}

func TestPortSettings(t *testing.T) {
	run := testRunning()
	c := ImportConfig(run)
	p := c.Ports["lan1"]
	p.Enabled, p.Description, p.Speed, p.Learning, p.Isolated, p.STPCost = false, "PLC cabinet 3", "100full", false, true, 19
	c.Ports["lan2"].Speed, c.Ports["lan2"].Autoneg = "10half", false
	c.Bridge.STP.Priority = 4096
	got := strs(Plan(c, run, PlanOpts{Bridge: "br0"}))
	want := []string{
		"ip link set dev br0 type bridge priority 4096",
		"ip link set dev lan1 down",
		"ip link set dev lan1 alias 'PLC cabinet 3'",
		"ethtool -s lan1 autoneg on advertise 0x8",
		"bridge link set dev lan1 cost 19 learning off isolated on",
		"ethtool -s lan2 autoneg off speed 10 duplex half",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got:\n%s", strings.Join(got, "\n"))
	}
}

func TestFDBAndMirror(t *testing.T) {
	run := testRunning()
	run.StaticFDB = []FDBEntry{{MAC: "00:11:22:33:44:55", Port: "lan1"}}
	c := ImportConfig(run)
	c.StaticFDB = []FDBEntry{{MAC: "00:11:22:33:44:66", Port: "lan2"}}
	c.Mirror = &Mirror{Dest: "lan3", Sources: []MirrorSource{{Port: "lan1", Direction: "both"}}}
	got := strs(Plan(c, run, PlanOpts{Bridge: "br0"}))
	want := []string{
		"bridge fdb add 00:11:22:33:44:66 dev lan2 master static",
		"bridge fdb del 00:11:22:33:44:55 dev lan1 master",
		"tc qdisc del dev lan1 clsact",
		"tc qdisc add dev lan1 clsact",
		"tc filter add dev lan1 ingress matchall skip_sw action mirred egress mirror dev lan3",
		"tc filter add dev lan1 egress matchall skip_sw action mirred egress mirror dev lan3",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got:\n%s", strings.Join(got, "\n"))
	}
	run.Mirror = c.Mirror
	c.Mirror = nil
	if got := strs(planMirror(nil, run.Mirror)); !slices.Equal(got, []string{"tc qdisc del dev lan1 clsact"}) {
		t.Fatalf("mirror removal: %q", got)
	}
}

func TestValidate(t *testing.T) {
	c := DefaultConfig(testPorts)
	c.Bridge.VLANAware = true
	c.Bridge.MgmtVLAN = 99
	c.VLANs = append(c.VLANs, VLAN{ID: 129}, VLAN{ID: 5000})
	c.Ports["lan1"].Tagged = []int{1, 7}
	c.Ports["lan2"].Speed, c.Ports["lan2"].Autoneg = "1000full", false
	c.Bridge.STP.MaxAge = 40
	c.StaticFDB = []FDBEntry{{MAC: "01:00:5e:00:00:01", Port: "lan9", VLAN: 1}}
	c.Mirror = &Mirror{Dest: "lan1", Sources: []MirrorSource{{Port: "lan1", Direction: "in"}}}
	w, err := c.Validate(testPorts)
	ve, ok := err.(*ValidationError)
	if !ok {
		t.Fatalf("expected validation error, got %v", err)
	}
	for _, s := range []string{"mgmt_vlan 99", "vlan 5000", "both pvid and tagged", "tagged vlan 7", "1000full requires autoneg",
		"802.1D", "multicast MAC", "no such port \"lan9\"", "is the destination", "direction"} {
		if !slices.ContainsFunc(ve.Problems, func(p string) bool { return strings.Contains(p, s) }) {
			t.Errorf("missing problem %q in %q", s, ve.Problems)
		}
	}
	if !slices.ContainsFunc(w, func(p string) bool { return strings.Contains(p, "1 and 129") }) {
		t.Errorf("missing shared-FID warning: %q", w)
	}
}

func TestRenderDropins(t *testing.T) {
	c := DefaultConfig(testPorts)
	c.Bridge.VLANAware = true
	c.VLANs = append(c.VLANs, VLAN{ID: 10})
	c.Ports["lan2"].PVID, c.Ports["lan2"].Tagged = 10, []int{1}
	c.Ports["lan3"].Enabled = false
	c.StaticFDB = []FDBEntry{{MAC: "00:11:22:33:44:55", Port: "lan2", VLAN: 10}}
	unit := func(kind, n string) string { return "10-netplan-" + n + "." + kind }
	f := RenderDropins(c, "br0", unit, "/etc/napi-switch/config.json")
	for path, must := range map[string][]string{
		"10-netplan-br0.netdev.d/" + dropinName:  {"VLANFiltering=yes", "STP=yes", "DefaultPVID=none", "MulticastSnooping=yes"},
		"10-netplan-br0.network.d/" + dropinName: {"[BridgeVLAN]\nVLAN=1\nPVID=1\nEgressUntagged=1"},
		"10-netplan-lan2.network.d/" + dropinName: {"[BridgeVLAN]\nVLAN=10\nPVID=10\nEgressUntagged=10", "[BridgeVLAN]\nVLAN=1\n",
			"[BridgeFDB]\nMACAddress=00:11:22:33:44:55\nAssociatedWith=master\nVLANId=10"},
		"10-netplan-lan3.network.d/" + dropinName: {"ActivationPolicy=down"},
	} {
		got, ok := f[path]
		if !ok {
			t.Fatalf("no %s in %v", path, f)
		}
		for _, s := range must {
			if !strings.Contains(got, s) {
				t.Errorf("%s: missing %q in:\n%s", path, s, got)
			}
		}
	}
}

func TestMergePatch(t *testing.T) {
	var doc, patch any
	_ = json.Unmarshal([]byte(`{"a":1,"b":{"c":2,"d":3},"e":[1,2]}`), &doc)
	_ = json.Unmarshal([]byte(`{"b":{"c":null,"x":4},"e":[3]}`), &patch)
	got, _ := json.Marshal(mergePatch(doc, patch))
	if string(got) != `{"a":1,"b":{"d":3,"x":4},"e":[3]}` {
		t.Fatalf("got %s", got)
	}
}

func TestSpeedSetting(t *testing.T) {
	all := []string{"10baseT/Half", "10baseT/Full", "100baseT/Half", "100baseT/Full", "1000baseT/Full"}
	for _, tc := range []struct {
		l     LinkInfo
		speed string
		an    bool
	}{
		{LinkInfo{Autoneg: true, Supported: all, Advertised: all}, "auto", true},
		{LinkInfo{Autoneg: true, Supported: all, Advertised: []string{"100baseT/Full"}}, "100full", true},
		{LinkInfo{Autoneg: false, Speed: 10, Duplex: "half", Supported: all}, "10half", false},
	} {
		if s, an := tc.l.SpeedSetting(); s != tc.speed || an != tc.an {
			t.Errorf("%+v: got %s/%v", tc.l, s, an)
		}
	}
}

func TestNonBridgedPortSkipped(t *testing.T) {
	run := testRunning()
	run.Ports["sfp"] = &RunPort{Name: "sfp", Priority: -1}
	c := ImportConfig(run)
	c.Bridge.VLANAware = true
	for _, x := range strs(Plan(c, run, PlanOpts{Bridge: "br0"})) {
		if strings.Contains(x, "sfp") {
			t.Fatalf("sfp touched: %s", x)
		}
	}
}

func TestAgeingIgnoredDuringTopologyChange(t *testing.T) {
	run := testRunning()
	c := ImportConfig(run)
	run.Bridge.AgeingTime, run.Bridge.TopologyChange = 15, true
	if p := Plan(c, run, PlanOpts{Bridge: "br0"}); len(p) != 0 {
		t.Fatalf("got %q", strs(p))
	}
}
