package main

import (
	"net/url"
	"slices"
	"testing"
)

func TestMergeFDB(t *testing.T) {
	run := testRunning()
	ents := []fdbJSON{
		// learned by the bridge (frames reached the CPU) and by the chip
		{MAC: "AA:00:00:00:00:01", Ifname: "lan1", Master: "br0"},
		{MAC: "aa:00:00:00:00:01", Ifname: "lan1", VLAN: 1, Flags: []string{"self"}},
		// port-to-port only: in the chip table, not in the bridge
		{MAC: "aa:00:00:00:00:02", Ifname: "lan2", VLAN: 1, Flags: []string{"self"}},
		// port's own address, bridge multicast, CPU-side entries
		{MAC: "12:00:00:00:00:02", Ifname: "lan2", Master: "br0", State: "permanent"},
		{MAC: "33:33:00:00:00:01", Ifname: "br0", Flags: []string{"self"}, State: "permanent"},
		{MAC: "aa:00:00:00:00:03", Ifname: "lan3", Master: "br0", State: "static"},
	}
	got := mergeFDB(ents, run, "br0", url.Values{})
	want := []fdbEntryView{
		{"aa:00:00:00:00:01", "lan1", 0, "dynamic", true},
		{"aa:00:00:00:00:02", "lan2", 0, "dynamic", true},
		{"12:00:00:00:00:02", "lan2", 0, "local", false},
		{"aa:00:00:00:00:03", "lan3", 0, "static", false},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
	if got := mergeFDB(ents, run, "br0", url.Values{"port": {"lan2"}, "type": {"dynamic"}}); len(got) != 1 || got[0].MAC != "aa:00:00:00:00:02" {
		t.Fatalf("filter: %+v", got)
	}
	// 802.1Q mode keeps the VLAN the chip reports.
	run.Bridge.VLANAware = true
	if got := mergeFDB(ents[2:3], run, "br0", url.Values{}); got[0].VLAN != 1 {
		t.Fatalf("vlan-aware: %+v", got)
	}
}
