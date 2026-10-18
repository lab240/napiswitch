package main

import (
	"errors"
	"log"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func netConfig(mode string) *Config {
	c := DefaultConfig(testPorts)
	c.Network = DefaultNetwork("enx00e04c680026")
	c.Network.Mode = mode
	c.Normalize()
	return c
}

func mustContain(t *testing.T, name, got string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("%s: missing %q in:\n%s", name, w, got)
		}
	}
}

func mustNotContain(t *testing.T, name, got string, bad ...string) {
	t.Helper()
	for _, b := range bad {
		if strings.Contains(got, b) {
			t.Errorf("%s: unexpected %q in:\n%s", name, b, got)
		}
	}
}

// Switch mode with DHCP must reproduce what netplan generates today.
func TestRenderSwitchModeMatchesNetplan(t *testing.T) {
	f := RenderNetwork(netConfig("switch"), "br0", "/etc/napi-switch/config.json")
	mustContain(t, "wan", f[unitWAN], "[Match]\nName=enx00e04c680026\n", "DHCP=yes\n", "IPv6PrivacyExtensions=yes",
		"LinkLocalAddressing=ipv6", "[DHCPv4]\nRouteMetric=100\nUseMTU=yes\n")
	mustContain(t, "lan", f[unitLAN], "[Match]\nName=br0\n", "DHCP=ipv4\n", "ConfigureWithoutCarrier=yes",
		"[DHCPv4]\nRouteMetric=200\nUseMTU=yes\n")
	mustNotContain(t, "switch mode", f[unitWAN]+f[unitLAN], "IPMasquerade", "DHCPServer", "IPv4Forwarding", "Address=")
}

func TestRenderRouterMode(t *testing.T) {
	c := netConfig("router")
	c.Bridge.VLANAware = true
	c.Network.LAN.DHCPServer.StaticLeases = []StaticLease{{MAC: "02:00:00:00:00:10", Address: "192.168.1.10"}}
	f := RenderNetwork(c, "br0", "cfg")
	mustContain(t, "wan", f[unitWAN], "DHCP=yes\n", "IPv4Forwarding=yes\n")
	mustContain(t, "lan", f[unitLAN], "Address=192.168.1.1/24\n", "IPv4Forwarding=yes\n", "IPMasquerade=ipv4\n", "DHCPServer=yes\n",
		"[DHCPServer]\nPoolOffset=100\nPoolSize=100\nDefaultLeaseTimeSec=43200\n", "EmitRouter=yes\nEmitDNS=yes\nUplinkInterface=enx00e04c680026\n",
		"[DHCPServerStaticLease]\nMACAddress=02:00:00:00:00:10\nAddress=192.168.1.10\n",
		"[BridgeVLAN]\nVLAN=1\nPVID=1\nEgressUntagged=1\n")
	mustNotContain(t, "lan", f[unitLAN], "DHCP=ipv4", "RouteMetric")
}

func TestRenderStatic(t *testing.T) {
	c := netConfig("switch")
	c.Network.WAN = WAN{Interface: "enx0", Proto: "static", Address: "10.0.0.5/24", Gateway: "10.0.0.1", DNS: []string{"1.1.1.1"}}
	c.Network.LAN.Proto, c.Network.LAN.Address, c.Network.LAN.Gateway = "static", "192.168.30.81/24", "192.168.30.1"
	f := RenderNetwork(c, "br0", "cfg")
	mustContain(t, "wan", f[unitWAN], "DHCP=no\nAddress=10.0.0.5/24\nDNS=1.1.1.1\n", "[Route]\nGateway=10.0.0.1\nMetric=100\n")
	mustNotContain(t, "wan", f[unitWAN], "DHCPv4", "IPv6Privacy")
	mustContain(t, "lan", f[unitLAN], "DHCP=no\nAddress=192.168.30.81/24\n", "[Route]\nGateway=192.168.30.1\nMetric=200\n")
}

func TestRenderFirewall(t *testing.T) {
	c := netConfig("switch")
	if fw := RenderFirewall(c.Network, 8080); fw != "" {
		t.Fatalf("switch mode must have no firewall:\n%s", fw)
	}
	n := netConfig("router").Network
	n.Firewall.WANAllow = []string{"ssh"}
	n.Firewall.PortForwards = []PortForward{{Name: "cam", Proto: "tcp", WANPort: 8554, Dest: "192.168.1.20", DestPort: 554},
		{Proto: "both", WANPort: 5000, Dest: "192.168.1.21"}}
	fw := RenderFirewall(n, 8080)
	mustContain(t, "fw", fw, "table inet napi_switchd\ndelete table inet napi_switchd\n",
		"iifname != \"enx00e04c680026\" accept", "ct state established,related accept", "tcp dport 22 accept", "counter drop",
		"ct status dnat accept", "type nat hook prerouting priority dstnat",
		"iifname \"enx00e04c680026\" tcp dport 8554 dnat ip to 192.168.1.20:554",
		"iifname \"enx00e04c680026\" meta l4proto { tcp, udp } th dport 5000 dnat ip to 192.168.1.21:5000")
	mustNotContain(t, "fw", fw, "dport 8080", "echo-request")
	n.Firewall.Enabled = false
	fw = RenderFirewall(n, 8080)
	mustContain(t, "forwards only", fw, "chain prerouting")
	mustNotContain(t, "forwards only", fw, "chain input", "chain forward")
	n.Firewall.PortForwards = nil
	if RenderFirewall(n, 8080) != "" {
		t.Fatal("no firewall and no forwards must render nothing")
	}
}

func TestPoolRange(t *testing.T) {
	lan := netip.MustParsePrefix("192.168.1.1/24")
	for _, c := range []struct {
		start, end string
		off, size  int
		err        string
	}{
		{"192.168.1.100", "192.168.1.199", 100, 100, ""},
		{"192.168.1.2", "192.168.1.254", 2, 253, ""},
		{"192.168.1.50", "192.168.1.50", 50, 1, ""},
		{"192.168.1.0", "192.168.1.10", 0, 0, "must lie within"},
		{"192.168.1.10", "192.168.1.255", 0, 0, "must lie within"},
		{"192.168.2.10", "192.168.2.20", 0, 0, "must lie within"},
		{"192.168.1.20", "192.168.1.10", 0, 0, "before pool_start"},
		{"192.168.1.1", "192.168.1.10", 0, 0, "router address"},
		{"", "192.168.1.10", 0, 0, "IPv4 addresses"},
	} {
		off, size, err := poolRange(lan, DHCPServer{PoolStart: c.start, PoolEnd: c.end})
		if c.err == "" && (err != nil || off != c.off || size != c.size) {
			t.Errorf("%s-%s: got %d,%d,%v want %d,%d", c.start, c.end, off, size, err, c.off, c.size)
		}
		if c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)) {
			t.Errorf("%s-%s: error %v, want %q", c.start, c.end, err, c.err)
		}
	}
}

func TestValidateNetwork(t *testing.T) {
	ctx := netCtx{Candidates: []string{"enx00e04c680026"}, WebPort: 8080, NFT: true}
	ok := netConfig("router").Network
	if w, p := ok.validate(ctx); len(p) > 0 {
		t.Fatalf("default router config rejected: %q (warnings %q)", p, w)
	}

	n := netConfig("router").Network
	n.WAN.Interface = "eth9"
	n.LAN.Address = "192.168.30.1/24"
	n.LAN.DHCPServer.PoolStart, n.LAN.DHCPServer.PoolEnd = "192.168.30.1", "192.168.30.50" // contains .1, the router
	n.LAN.DHCPServer.StaticLeases = []StaticLease{{MAC: "01:00:5e:00:00:01", Address: "10.1.1.1"}}
	n.Firewall.WANAllow = []string{"ssh", "telnet"}
	n.Firewall.PortForwards = []PortForward{{Proto: "tcp", WANPort: 22, Dest: "192.168.30.5"}, {Proto: "icmp", WANPort: 1, Dest: "8.8.8.8"}}
	ctx.WANAddrs = nil
	w, p := n.validate(netCtx{Candidates: ctx.Candidates, WANAddrs: mustPrefixes("192.168.30.82/24"), WebPort: 8080,
		LinkPorts: []string{"lan5"}})
	for _, s := range []string{"not an available interface", "overlaps the WAN network", "router address", "bad MAC",
		"must be a host in", "telnet", "already used", "proto tcp, udp or both", "must be a LAN host"} {
		if !slices.ContainsFunc(p, func(x string) bool { return strings.Contains(x, s) }) {
			t.Errorf("missing problem %q in %q", s, p)
		}
	}
	for _, s := range []string{"lan5 (link up)", "nftables is not installed"} {
		if !slices.ContainsFunc(w, func(x string) bool { return strings.Contains(x, s) }) {
			t.Errorf("missing warning %q in %q", s, w)
		}
	}

	s := netConfig("switch").Network
	s.WAN = WAN{Interface: "enx00e04c680026", Proto: "static", Address: "10.0.0.5/24", Gateway: "10.0.1.1", DNS: []string{"dns.google"}}
	s.LAN.Proto = "static"
	s.LAN.Address = "192.168.5.0/24"
	_, p = s.validate(ctx)
	for _, x := range []string{"lan.address: is the network or broadcast", "wan.gateway", "wan.dns"} {
		if !slices.ContainsFunc(p, func(y string) bool { return strings.Contains(y, x) }) {
			t.Errorf("missing problem %q in %q", x, p)
		}
	}
}

func mustPrefixes(s ...string) (r []netip.Prefix) {
	for _, x := range s {
		p, err := parseHost(x)
		if err != nil {
			panic(err)
		}
		r = append(r, p)
	}
	return r
}

func netRunning(files map[string]string, fw string) *Running {
	run := testRunning()
	run.Ethers = []string{"enx00e04c680026"}
	run.Net = &NetState{Files: files, InRun: map[string]bool{}, NFT: true, FWLoaded: fw, WebPort: 8080}
	return run
}

var netOpts = PlanOpts{Bridge: "br0", RunNetDir: "/run/systemd/network", FWFile: "/run/napi-switch/firewall.nft", CfgPath: "cfg"}

func TestPlanNetwork(t *testing.T) {
	// Unmanaged and nothing of ours on disk: no L3 commands.
	if p := planNetwork(DefaultConfig(testPorts), netRunning(map[string]string{}, ""), netOpts); len(p) != 0 {
		t.Fatalf("unmanaged: %q", strs(p))
	}
	// Taking over in switch mode: write both units, reload, reconfigure.
	c := netConfig("switch")
	got := strs(planNetwork(c, netRunning(map[string]string{}, ""), netOpts))
	want := []string{"write /run/systemd/network/03-napi-switchd-wan.network", "write /run/systemd/network/03-napi-switchd-lan.network",
		"networkctl reload", "networkctl reconfigure br0", "networkctl reconfigure enx00e04c680026"}
	if !slices.Equal(got, want) {
		t.Fatalf("takeover:\n%s", strings.Join(got, "\n"))
	}
	// Effective files equal to the desired ones (e.g. after boot from /etc): nothing to do.
	files := RenderNetwork(c, "br0", "cfg")
	if p := planNetwork(c, netRunning(files, ""), netOpts); len(p) != 0 {
		t.Fatalf("steady state: %q", strs(p))
	}
	// Switch -> router: only the LAN unit and WAN forwarding change, firewall gets loaded.
	r := netConfig("router")
	got = strs(planNetwork(r, netRunning(files, ""), netOpts))
	want = []string{"write /run/systemd/network/03-napi-switchd-wan.network", "write /run/systemd/network/03-napi-switchd-lan.network",
		"networkctl reload", "networkctl reconfigure br0", "networkctl reconfigure enx00e04c680026",
		"write /run/napi-switch/firewall.nft", "nft -f /run/napi-switch/firewall.nft"}
	if !slices.Equal(got, want) {
		t.Fatalf("router:\n%s", strings.Join(got, "\n"))
	}
	// Back to switch mode: firewall table removed, forwarding and masquerading undone.
	rf := RenderNetwork(r, "br0", "cfg")
	got = strs(planNetwork(c, netRunning(rf, RenderFirewall(r.Network, 8080)), netOpts))
	if !slices.Contains(got, "nft delete table inet napi_switchd") || !slices.Contains(got, "rm -f /run/napi-switch/firewall.nft") ||
		!slices.Contains(got, "sysctl -w net.ipv4.conf.all.forwarding=0") || !slices.Contains(got, "nft flush set ip io.systemd.nat masq_saddr") {
		t.Fatalf("router -> switch:\n%s", strings.Join(got, "\n"))
	}
	// WAN moved to another NIC: both the old and the new one are reconfigured.
	m := netConfig("switch")
	m.Network.WAN.Interface = "enx112233445566"
	got = strs(planNetwork(m, netRunning(files, ""), netOpts))
	if !slices.Contains(got, "networkctl reconfigure enx00e04c680026") || !slices.Contains(got, "networkctl reconfigure enx112233445566") {
		t.Fatalf("wan move:\n%s", strings.Join(got, "\n"))
	}
	// No nft on the board: firewall silently not planned (validation warns).
	run := netRunning(files, "")
	run.Net.NFT = false
	for _, x := range strs(planNetwork(r, run, netOpts)) {
		if strings.Contains(x, "nft") {
			t.Fatalf("nft planned without nft: %s", x)
		}
	}
}

func TestFindUnitSkipsOwnFiles(t *testing.T) {
	d := t.TempDir()
	_ = os.WriteFile(filepath.Join(d, "04-napi-switchd-lan.network"), []byte("[Match]\nName=br0\n"), 0o644)
	_ = os.WriteFile(filepath.Join(d, "10-netplan-br0.network"), []byte("[Match]\nName=br0\n"), 0o644)
	if u := findUnit([]string{d}, "network", "br0"); u != "10-netplan-br0.network" {
		t.Fatalf("got %q", u)
	}
}

func TestSaveNetwork(t *testing.T) {
	d := t.TempDir()
	p := Paths{Config: filepath.Join(d, "config.json"), NetDir: d}
	c := netConfig("router")
	files, err := saveNetwork(c, p, "br0")
	if err != nil || len(files) != 2 {
		t.Fatalf("save: %v %q", err, files)
	}
	b, _ := os.ReadFile(filepath.Join(d, "04-napi-switchd-lan.network"))
	mustContain(t, "saved lan", string(b), "IPMasquerade=ipv4")
	c.Network = nil
	files, _ = saveNetwork(c, p, "br0")
	if len(files) != 2 || !strings.HasSuffix(files[0], "(removed)") {
		t.Fatalf("unmanage: %q", files)
	}
	if _, err := os.Stat(filepath.Join(d, "04-napi-switchd-wan.network")); err == nil {
		t.Fatal("wan unit not removed")
	}
}

type failRunner struct{}

func (failRunner) Output(...string) ([]byte, error) { return nil, errors.New("no") }
func (failRunner) Exec(Cmd) error                   { return nil }

func TestNetworkChangeNeedsConfirm(t *testing.T) {
	m := NewManager(failRunner{}, log.New(os.Stderr, "", 0), "br0", Paths{}, 8080)
	run := netRunning(map[string]string{}, "")
	prev := DefaultConfig(testPorts)
	c := netConfig("switch")
	if _, err := m.validate(c, prev, run, 0); err == nil || !strings.Contains(err.Error(), "with confirm") {
		t.Fatalf("expected confirm requirement, got %v", err)
	}
	if _, err := m.validate(c, prev, run, 60*time.Second); err != nil {
		t.Fatalf("with confirm: %v", err)
	}
	if _, err := m.validate(prev, c, run, 60*time.Second); err == nil || !strings.Contains(err.Error(), "handed back") {
		t.Fatalf("expected unmanage refusal, got %v", err)
	}
	// Non-network change with no confirm stays allowed.
	c2 := c.Clone()
	c2.Ports["lan1"].Description = "x"
	if _, err := m.validate(c2, c, run, 0); err != nil {
		t.Fatalf("port change: %v", err)
	}
}
