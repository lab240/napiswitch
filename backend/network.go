package main

// L3: the WAN interface (USB NIC), the LAN address of the bridge, and router
// mode (NAT, DHCP server, firewall, port forwarding).
//
// networkd uses the first .network file, in lexical order over all its
// directories, whose [Match] fits a link. Our files sort before netplan's
// 10-netplan-*: the applied config goes to /run/systemd/network/03-napi-switchd-*,
// the saved one to /etc/systemd/network/04-napi-switchd-*. /run is empty after
// a reboot, so the saved file wins; with neither, netplan's files apply again.
// networkd itself does DHCP (client and server), NAT (IPMasquerade) and
// forwarding; only the firewall needs nft.

import (
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
)

const (
	netRunPrefix = "03-napi-switchd-"
	netEtcPrefix = "04-napi-switchd-"
	fwTable      = "napi_switchd"
	unitWAN      = "wan.network"
	unitLAN      = "lan.network"
)

type Network struct {
	// Mode: "switch" - the bridge is a plain LAN client (DHCP or static);
	// "router" - the bridge is the LAN gateway with NAT to the WAN.
	Mode     string   `json:"mode"`
	WAN      WAN      `json:"wan"`
	LAN      LAN      `json:"lan"`
	Firewall Firewall `json:"firewall"`
}

type WAN struct {
	Interface string   `json:"interface"`
	Proto     string   `json:"proto"`   // dhcp, static
	Address   string   `json:"address"` // static: 192.0.2.10/24
	Gateway   string   `json:"gateway"` // static, optional
	DNS       []string `json:"dns"`     // static; with dhcp replaces the DNS from the lease
}

type LAN struct {
	// Proto applies in switch mode only; in router mode the LAN is static.
	Proto      string     `json:"proto"`   // dhcp, static
	Address    string     `json:"address"` // 192.168.1.1/24
	Gateway    string     `json:"gateway"` // switch mode, static
	DNS        []string   `json:"dns"`     // switch mode
	DHCPServer DHCPServer `json:"dhcp_server"`
}

type DHCPServer struct {
	Enabled      bool          `json:"enabled"`
	PoolOffset   int           `json:"pool_offset"` // first address = subnet + offset
	PoolSize     int           `json:"pool_size"`
	LeaseTime    int           `json:"lease_time"` // seconds
	DNS          []string      `json:"dns"`        // empty: hand out the WAN DNS servers
	StaticLeases []StaticLease `json:"static_leases"`
}

type StaticLease struct {
	MAC     string `json:"mac"`
	Address string `json:"address"`
	Name    string `json:"name"` // label only
}

type Firewall struct {
	// Enabled filters traffic coming in from the WAN (router mode only):
	// replies and WANAllow services pass, everything else is dropped.
	Enabled      bool          `json:"enabled"`
	WANAllow     []string      `json:"wan_allow"` // ssh, web, ping
	PortForwards []PortForward `json:"port_forwards"`
}

type PortForward struct {
	Name     string `json:"name"`
	Proto    string `json:"proto"` // tcp, udp, both
	WANPort  int    `json:"wan_port"`
	Dest     string `json:"dest"`      // LAN host
	DestPort int    `json:"dest_port"` // 0 = same as wan_port
}

// NetState is the L3 part of the running state.
type NetState struct {
	Files    map[string]string `json:"-"` // unit -> effective content of our file ("" = none)
	InRun    map[string]bool   `json:"-"` // unit has a /run file
	NFT      bool              `json:"nft"`
	FWLoaded string            `json:"-"` // ruleset we loaded, "" = table absent
	WebPort  int               `json:"web_port"`
}

func DefaultNetwork(wan string) *Network {
	n := &Network{Mode: "switch",
		WAN: WAN{Interface: wan, Proto: "dhcp"},
		LAN: LAN{Proto: "dhcp", Address: "192.168.1.1/24",
			DHCPServer: DHCPServer{Enabled: true, PoolOffset: 100, PoolSize: 100, LeaseTime: 43200}},
		Firewall: Firewall{Enabled: true, WANAllow: []string{"ssh", "web", "ping"}}}
	n.Normalize()
	return n
}

func (n *Network) Normalize() {
	for _, l := range []*[]string{&n.WAN.DNS, &n.LAN.DNS, &n.LAN.DHCPServer.DNS, &n.Firewall.WANAllow} {
		if *l == nil {
			*l = []string{}
		}
	}
	slices.Sort(n.Firewall.WANAllow)
	n.Firewall.WANAllow = slices.Compact(n.Firewall.WANAllow)
	if n.LAN.DHCPServer.StaticLeases == nil {
		n.LAN.DHCPServer.StaticLeases = []StaticLease{}
	}
	for i := range n.LAN.DHCPServer.StaticLeases {
		n.LAN.DHCPServer.StaticLeases[i].MAC = strings.ToLower(n.LAN.DHCPServer.StaticLeases[i].MAC)
	}
	if n.Firewall.PortForwards == nil {
		n.Firewall.PortForwards = []PortForward{}
	}
}

func (n *Network) router() bool { return n != nil && n.Mode == "router" }

// --- validation ---

type netCtx struct {
	Candidates []string       // interfaces usable as WAN
	WANAddrs   []netip.Prefix // addresses the WAN has now (DHCP)
	LinkPorts  []string       // switch ports with link
	NFT        bool
	WebPort    int
}

func parseHost(s string) (netip.Prefix, error) {
	p, err := netip.ParsePrefix(s)
	if err != nil || !p.Addr().Is4() {
		return p, fmt.Errorf("must be an IPv4 address with prefix, e.g. 192.168.1.1/24")
	}
	if p.Bits() < 8 || p.Bits() > 30 {
		return p, fmt.Errorf("prefix length 8..30")
	}
	if p.Addr() == p.Masked().Addr() || p.Addr() == lastAddr(p) {
		return p, fmt.Errorf("is the network or broadcast address")
	}
	return p, nil
}

func lastAddr(p netip.Prefix) netip.Addr {
	a := p.Masked().Addr().As4()
	v := uint32(a[0])<<24 | uint32(a[1])<<16 | uint32(a[2])<<8 | uint32(a[3])
	v |= (1 << (32 - p.Bits())) - 1
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

func addrOffset(p netip.Prefix, a netip.Addr) int {
	x, y := p.Masked().Addr().As4(), a.As4()
	u := func(b [4]byte) int { return int(b[0])<<24 | int(b[1])<<16 | int(b[2])<<8 | int(b[3]) }
	return u(y) - u(x)
}

func validIPs(l []string) bool {
	for _, s := range l {
		if a, err := netip.ParseAddr(s); err != nil || !a.Is4() && !a.Is6() {
			return false
		}
	}
	return true
}

var wanServices = map[string]bool{"ssh": true, "web": true, "ping": true}

func (n *Network) validate(ctx netCtx) (warnings, problems []string) {
	bad := func(f string, a ...any) { problems = append(problems, fmt.Sprintf(f, a...)) }
	warn := func(f string, a ...any) { warnings = append(warnings, fmt.Sprintf(f, a...)) }
	if n.Mode != "switch" && n.Mode != "router" {
		bad("network.mode: switch or router")
	}
	w := n.WAN
	if len(ctx.Candidates) > 0 && !slices.Contains(ctx.Candidates, w.Interface) {
		bad("network.wan.interface %q: not an available interface (%s)", w.Interface, strings.Join(ctx.Candidates, ", "))
	}
	var wanNet *netip.Prefix
	switch w.Proto {
	case "dhcp":
	case "static":
		p, err := parseHost(w.Address)
		if err != nil {
			bad("network.wan.address: %v", err)
		} else {
			wanNet = &p
			if w.Gateway != "" {
				g, err := netip.ParseAddr(w.Gateway)
				if err != nil || !p.Contains(g) || g == p.Addr() {
					bad("network.wan.gateway: must be an address in %s other than the WAN address", p.Masked())
				}
			} else if n.router() {
				warn("network.wan: no gateway - the LAN will have no internet access")
			}
		}
	default:
		bad("network.wan.proto: dhcp or static")
	}
	if !validIPs(w.DNS) {
		bad("network.wan.dns: IP addresses only")
	}

	l := n.LAN
	var lanNet netip.Prefix
	lanOK := false
	if n.router() || l.Proto == "static" {
		p, err := parseHost(l.Address)
		if err != nil {
			bad("network.lan.address: %v", err)
		} else {
			lanNet, lanOK = p, true
		}
	} else if l.Proto != "dhcp" {
		bad("network.lan.proto: dhcp or static")
	}
	if !n.router() && l.Proto == "static" && l.Gateway != "" && lanOK {
		if g, err := netip.ParseAddr(l.Gateway); err != nil || !lanNet.Contains(g) || g == lanNet.Addr() {
			bad("network.lan.gateway: must be an address in %s other than the LAN address", lanNet.Masked())
		}
	}
	if !validIPs(l.DNS) {
		bad("network.lan.dns: IP addresses only")
	}

	if !n.router() {
		return warnings, problems
	}

	// Router mode from here on.
	if w.Interface == "" {
		bad("network.wan.interface: required in router mode")
	}
	if lanOK {
		wans := slices.Clone(ctx.WANAddrs)
		if wanNet != nil {
			wans = []netip.Prefix{*wanNet}
		}
		for _, wp := range wans {
			if wp.Overlaps(lanNet) {
				bad("network.lan.address %s overlaps the WAN network %s: pick another LAN subnet", lanNet.Masked(), wp.Masked())
			}
		}
	}
	d := l.DHCPServer
	if d.Enabled && lanOK {
		hosts := 1<<(32-lanNet.Bits()) - 2
		if d.PoolOffset < 1 || d.PoolSize < 1 || d.PoolOffset+d.PoolSize-1 > hosts {
			bad("network.lan.dhcp_server: pool must fit in %s (offsets 1..%d)", lanNet.Masked(), hosts)
		} else if o := addrOffset(lanNet, lanNet.Addr()); o >= d.PoolOffset && o < d.PoolOffset+d.PoolSize {
			bad("network.lan.dhcp_server: the pool contains the router address %s", lanNet.Addr())
		}
		if d.LeaseTime < 60 || d.LeaseTime > 7*86400 {
			bad("network.lan.dhcp_server.lease_time: 60..604800 s")
		}
		if !validIPs(d.DNS) {
			bad("network.lan.dhcp_server.dns: IP addresses only")
		}
		macs, addrs := map[string]bool{}, map[netip.Addr]bool{}
		for _, s := range d.StaticLeases {
			hw, err := net.ParseMAC(s.MAC)
			if err != nil || len(hw) != 6 || hw[0]&1 == 1 {
				bad("static lease %q: bad MAC", s.MAC)
			}
			a, err := netip.ParseAddr(s.Address)
			if err != nil || !lanNet.Contains(a) || a == lanNet.Addr() || a == lanNet.Masked().Addr() || a == lastAddr(lanNet) {
				bad("static lease %s: address %q must be a host in %s other than the router", s.MAC, s.Address, lanNet.Masked())
			}
			if macs[s.MAC] || addrs[a] {
				bad("static lease %s %s: duplicate", s.MAC, s.Address)
			}
			macs[s.MAC], addrs[a] = true, true
			if len(s.Name) > 32 || !descRe.MatchString(s.Name) {
				bad("static lease %s: name up to 32 printable characters", s.MAC)
			}
		}
		if len(ctx.LinkPorts) > 0 {
			warn("DHCP server will answer on %s (link up): make sure no other network is connected there", strings.Join(ctx.LinkPorts, ", "))
		}
	}

	f := n.Firewall
	for _, s := range f.WANAllow {
		if !wanServices[s] {
			bad("network.firewall.wan_allow %q: ssh, web or ping", s)
		}
	}
	if f.Enabled && !slices.Contains(f.WANAllow, "ssh") && !slices.Contains(f.WANAllow, "web") {
		warn("firewall: ssh and web are closed from the WAN - management only from the LAN (%s)", l.Address)
	}
	// DNAT happens before input filtering: a forward on the management
	// port would take ssh or the web UI away from the WAN side.
	used := map[string]bool{}
	if slices.Contains(f.WANAllow, "ssh") {
		used["tcp/22"] = true
	}
	if slices.Contains(f.WANAllow, "web") && ctx.WebPort > 0 {
		used[fmt.Sprintf("tcp/%d", ctx.WebPort)] = true
	}
	for _, pf := range f.PortForwards {
		id := pf.Name
		if id == "" {
			id = strconv.Itoa(pf.WANPort)
		}
		if pf.Proto != "tcp" && pf.Proto != "udp" && pf.Proto != "both" {
			bad("port forward %s: proto tcp, udp or both", id)
		}
		if pf.WANPort < 1 || pf.WANPort > 65535 || pf.DestPort < 0 || pf.DestPort > 65535 {
			bad("port forward %s: ports 1..65535", id)
		}
		a, err := netip.ParseAddr(pf.Dest)
		if err != nil || !lanOK || !lanNet.Contains(a) || a == lanNet.Addr() {
			bad("port forward %s: dest %q must be a LAN host", id, pf.Dest)
		}
		if len(pf.Name) > 32 || !descRe.MatchString(pf.Name) {
			bad("port forward %s: name up to 32 printable characters", id)
		}
		for _, pr := range []string{"tcp", "udp"} {
			if pf.Proto != pr && pf.Proto != "both" {
				continue
			}
			k := fmt.Sprintf("%s/%d", pr, pf.WANPort)
			if used[k] {
				bad("port forward %s: WAN port %s is already used (forward or management)", id, k)
			}
			used[k] = true
		}
	}
	if !ctx.NFT && (f.Enabled || len(f.PortForwards) > 0) {
		warn("nftables is not installed: firewall and port forwarding are not applied (apt install nftables)")
	}
	return warnings, problems
}

// --- rendering ---

func netHeader(cfgPath string) string { return fmt.Sprintf(dropinHeader, cfgPath) }

// RenderNetwork returns unit -> networkd file content for our units.
func RenderNetwork(c *Config, bridge, cfgPath string) map[string]string {
	n := c.Network
	if n == nil {
		return map[string]string{}
	}
	files := map[string]string{}
	var s strings.Builder
	dns := func(l []string) {
		for _, d := range l {
			fmt.Fprintf(&s, "DNS=%s\n", d)
		}
	}

	w := n.WAN
	if w.Interface != "" {
		s.WriteString(netHeader(cfgPath))
		fmt.Fprintf(&s, "\n[Match]\nName=%s\n\n[Network]\nLinkLocalAddressing=ipv6\n", w.Interface)
		if w.Proto == "dhcp" {
			s.WriteString("DHCP=yes\nIPv6PrivacyExtensions=yes\n")
		} else {
			fmt.Fprintf(&s, "DHCP=no\nAddress=%s\n", w.Address)
		}
		dns(w.DNS)
		if n.router() {
			s.WriteString("IPv4Forwarding=yes\n")
		}
		if w.Proto == "dhcp" {
			s.WriteString("\n[DHCPv4]\nRouteMetric=100\nUseMTU=yes\n")
			if len(w.DNS) > 0 {
				s.WriteString("UseDNS=no\n\n[DHCPv6]\nUseDNS=no\n")
			}
		} else if w.Gateway != "" {
			fmt.Fprintf(&s, "\n[Route]\nGateway=%s\nMetric=100\n", w.Gateway)
		}
		files[unitWAN] = s.String()
	}

	s.Reset()
	l := n.LAN
	s.WriteString(netHeader(cfgPath))
	fmt.Fprintf(&s, "\n[Match]\nName=%s\n\n[Network]\nLinkLocalAddressing=ipv6\nConfigureWithoutCarrier=yes\n", bridge)
	switch {
	case n.router():
		fmt.Fprintf(&s, "Address=%s\nIPv4Forwarding=yes\nIPMasquerade=ipv4\nDHCPServer=%s\n", l.Address, yesno(l.DHCPServer.Enabled))
	case l.Proto == "static":
		fmt.Fprintf(&s, "DHCP=no\nAddress=%s\n", l.Address)
		dns(l.DNS)
	default:
		s.WriteString("DHCP=ipv4\n")
		dns(l.DNS)
	}
	switch {
	case n.router() && l.DHCPServer.Enabled:
		d := l.DHCPServer
		fmt.Fprintf(&s, "\n[DHCPServer]\nPoolOffset=%d\nPoolSize=%d\nDefaultLeaseTimeSec=%d\nMaxLeaseTimeSec=%d\nEmitRouter=yes\nEmitDNS=yes\n",
			d.PoolOffset, d.PoolSize, d.LeaseTime, d.LeaseTime)
		if len(d.DNS) > 0 {
			fmt.Fprintf(&s, "DNS=%s\n", strings.Join(d.DNS, " "))
		} else if w.Interface != "" {
			fmt.Fprintf(&s, "UplinkInterface=%s\n", w.Interface)
		}
		for _, sl := range d.StaticLeases {
			fmt.Fprintf(&s, "\n[DHCPServerStaticLease]\nMACAddress=%s\nAddress=%s\n", sl.MAC, sl.Address)
		}
	case n.router():
	case l.Proto == "static":
		if l.Gateway != "" {
			fmt.Fprintf(&s, "\n[Route]\nGateway=%s\nMetric=200\n", l.Gateway)
		}
	default:
		// Same subnet as the USB management NIC: keep that one preferred.
		s.WriteString("\n[DHCPv4]\nRouteMetric=200\nUseMTU=yes\n")
		if len(l.DNS) > 0 {
			s.WriteString("UseDNS=no\n")
		}
	}
	// This file replaces netplan's br0.network, and with it our drop-in there.
	if c.Bridge.VLANAware {
		m := c.Bridge.MgmtVLAN
		fmt.Fprintf(&s, "\n[BridgeVLAN]\nVLAN=%d\nPVID=%d\nEgressUntagged=%d\n", m, m, m)
	}
	files[unitLAN] = s.String()
	return files
}

// RenderFirewall returns the nft script for our table, "" when none is needed.
// It replaces the table atomically: create (no-op if present), delete, define.
func RenderFirewall(n *Network, webPort int) string {
	if !n.router() || (!n.Firewall.Enabled && len(n.Firewall.PortForwards) == 0) {
		return ""
	}
	f, wan := n.Firewall, strconv.Quote(n.WAN.Interface)
	var s strings.Builder
	s.WriteString("#!/usr/sbin/nft -f\n# Generated by napi-switchd - do not edit, use the API.\n")
	fmt.Fprintf(&s, "table inet %s\ndelete table inet %s\n\ntable inet %s {\n", fwTable, fwTable, fwTable)
	if f.Enabled {
		fmt.Fprintf(&s, "\tchain input {\n\t\ttype filter hook input priority filter; policy accept;\n\t\tiifname != %s accept\n", wan)
		s.WriteString("\t\tct state established,related accept\n\t\tct state invalid drop\n")
		s.WriteString("\t\tmeta l4proto ipv6-icmp accept\n\t\tudp dport { 68, 546 } accept\n")
		if slices.Contains(f.WANAllow, "ping") {
			s.WriteString("\t\ticmp type echo-request accept\n")
		}
		if slices.Contains(f.WANAllow, "ssh") {
			s.WriteString("\t\ttcp dport 22 accept\n")
		}
		if slices.Contains(f.WANAllow, "web") && webPort > 0 {
			fmt.Fprintf(&s, "\t\ttcp dport %d accept\n", webPort)
		}
		s.WriteString("\t\tcounter drop\n\t}\n\n")
		fmt.Fprintf(&s, "\tchain forward {\n\t\ttype filter hook forward priority filter; policy accept;\n\t\tiifname != %s accept\n", wan)
		s.WriteString("\t\tct state established,related accept\n\t\tct status dnat accept\n\t\tcounter drop\n\t}\n")
	}
	if len(f.PortForwards) > 0 {
		if f.Enabled {
			s.WriteString("\n")
		}
		s.WriteString("\tchain prerouting {\n\t\ttype nat hook prerouting priority dstnat; policy accept;\n")
		for _, pf := range f.PortForwards {
			proto := "meta l4proto { tcp, udp } th"
			if pf.Proto != "both" {
				proto = pf.Proto
			}
			dp := pf.DestPort
			if dp == 0 {
				dp = pf.WANPort
			}
			fmt.Fprintf(&s, "\t\tiifname %s %s dport %d dnat ip to %s:%d\n", wan, proto, pf.WANPort, pf.Dest, dp)
		}
		s.WriteString("\t}\n")
	}
	s.WriteString("}\n")
	return s.String()
}

// --- planning ---

func planNetwork(want *Config, run *Running, o PlanOpts) []Cmd {
	st := run.Net
	if st == nil {
		return nil
	}
	var cmds []Cmd
	desired := RenderNetwork(want, o.Bridge, o.CfgPath)
	var reconf []string
	for _, u := range []string{unitWAN, unitLAN} {
		d, have := desired[u], st.Files[u]
		if d == have {
			continue
		}
		path := filepath.Join(o.RunNetDir, netRunPrefix+u)
		if d == "" {
			cmds = append(cmds, removeFile(path))
		} else {
			cmds = append(cmds, writeFile(path, d))
		}
		if u == unitLAN {
			reconf = append(reconf, o.Bridge)
		} else {
			for _, txt := range []string{d, have} {
				if i := fileMatchName(txt); i != "" && !slices.Contains(reconf, i) {
					reconf = append(reconf, i)
				}
			}
		}
	}
	if len(reconf) > 0 {
		cmds = append(cmds, cmd("networkctl", "reload"))
		sort.Strings(reconf)
		for _, i := range reconf {
			cmds = append(cmds, cmdIgn("networkctl", "reconfigure", i))
		}
	}
	if st.NFT {
		fw := RenderFirewall(want.Network, st.WebPort)
		switch {
		case fw == st.FWLoaded:
		case fw == "":
			cmds = append(cmds, cmdIgn("nft", "delete", "table", "inet", fwTable), removeFile(o.FWFile))
		default:
			cmds = append(cmds, writeFile(o.FWFile, fw), cmd("nft", "-f", o.FWFile))
		}
	}
	return cmds
}

func fileMatchName(content string) string {
	sec := ""
	for _, l := range strings.Split(content, "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "[") {
			sec = l
		} else if v, ok := strings.CutPrefix(l, "Name="); ok && sec == "[Match]" {
			return v
		}
	}
	return ""
}

// --- state ---

func readNetState(r Runner, p Paths, webPort int) *NetState {
	st := &NetState{Files: map[string]string{}, InRun: map[string]bool{}, WebPort: webPort}
	for _, u := range []string{unitWAN, unitLAN} {
		if b, err := os.ReadFile(filepath.Join(p.RunNetDir, netRunPrefix+u)); err == nil {
			st.Files[u], st.InRun[u] = string(b), true
		} else if b, err := os.ReadFile(filepath.Join(p.NetDir, netEtcPrefix+u)); err == nil {
			st.Files[u] = string(b)
		}
	}
	if _, err := r.Output("nft", "--version"); err == nil {
		st.NFT = true
		if _, err := r.Output("nft", "list", "table", "inet", fwTable); err == nil {
			b, _ := os.ReadFile(p.FWFile)
			st.FWLoaded = string(b)
			if st.FWLoaded == "" {
				st.FWLoaded = "unknown" // table present, file lost: reload
			}
		}
	}
	return st
}

// saveNetwork writes (or removes) the persistent copies of our units.
func saveNetwork(c *Config, p Paths, bridge string) ([]string, error) {
	files := RenderNetwork(c, bridge, p.Config)
	var written []string
	for _, u := range []string{unitWAN, unitLAN} {
		path := filepath.Join(p.NetDir, netEtcPrefix+u)
		d, ok := files[u]
		if !ok {
			if err := os.Remove(path); err == nil {
				written = append(written, path+" (removed)")
			}
			continue
		}
		if b, err := os.ReadFile(path); err == nil && string(b) == d {
			written = append(written, path)
			continue
		}
		if err := writeAtomic(path, []byte(d), 0o644); err != nil {
			return written, err
		}
		written = append(written, path)
	}
	return written, nil
}

// --- live status (networkctl) ---

type ncLink struct {
	Name             string `json:"Name"`
	OperationalState string `json:"OperationalState"`
	CarrierState     string `json:"CarrierState"`
	HardwareAddress  []int  `json:"HardwareAddress"`
	Driver           string `json:"Driver"`
	NetworkFile      string `json:"NetworkFile"`
	Addresses        []struct {
		Family       int    `json:"Family"`
		Address      []int  `json:"Address"`
		PrefixLength int    `json:"PrefixLength"`
		ConfigSource string `json:"ConfigSource"`
		ScopeString  string `json:"ScopeString"`
	} `json:"Addresses"`
	Routes []struct {
		Family                  int   `json:"Family"`
		Destination             []int `json:"Destination"`
		DestinationPrefixLength int   `json:"DestinationPrefixLength"`
		Gateway                 []int `json:"Gateway"`
		Priority                int   `json:"Priority"`
	} `json:"Routes"`
	DNS []struct {
		Family  int   `json:"Family"`
		Address []int `json:"Address"`
	} `json:"DNS"`
	DHCPServer *struct {
		PoolOffset int `json:"PoolOffset"`
		PoolSize   int `json:"PoolSize"`
		Leases     []struct {
			ClientID        []int  `json:"ClientId"`
			Address         []int  `json:"Address"`
			Hostname        string `json:"Hostname"`
			HardwareAddress []int  `json:"HardwareAddress"`
			ExpirationRT    int64  `json:"ExpirationRealtimeUSec"`
		} `json:"Leases"`
	} `json:"DHCPServer"`
}

func ipString(b []int) string {
	x := make([]byte, len(b))
	for i, v := range b {
		x[i] = byte(v)
	}
	if a, ok := netip.AddrFromSlice(x); ok {
		return a.Unmap().String()
	}
	return ""
}

func macString(b []int) string {
	if len(b) != 6 {
		return ""
	}
	x := make(net.HardwareAddr, 6)
	for i, v := range b {
		x[i] = byte(v)
	}
	return x.String()
}

type IfStatus struct {
	Name        string   `json:"name"`
	Role        string   `json:"role"` // wan, lan, ""
	State       string   `json:"state"`
	Carrier     bool     `json:"carrier"`
	Speed       int      `json:"speed"`  // Mbit/s, 0 = unknown or no link
	Duplex      string   `json:"duplex"` // full, half, ""
	MAC         string   `json:"mac"`
	Driver      string   `json:"driver"`
	NetworkFile string   `json:"network_file"`
	Addresses   []string `json:"addresses"`
	DHCP        bool     `json:"dhcp"` // an address came from DHCP
	Gateway     string   `json:"gateway"`
	DNS         []string `json:"dns"`
}

type Lease struct {
	MAC      string `json:"mac"`
	Address  string `json:"address"`
	Hostname string `json:"hostname"`
	Expires  int64  `json:"expires"` // unix seconds, 0 = unknown
}

func readIfStatus(r Runner, name string) (IfStatus, []Lease) {
	st := IfStatus{Name: name, Addresses: []string{}, DNS: []string{}}
	out, err := r.Output("networkctl", "--json=short", "status", name)
	if err != nil {
		return st, nil
	}
	var l ncLink
	if json.Unmarshal(out, &l) != nil {
		return st, nil
	}
	st.State, st.MAC, st.Driver, st.NetworkFile = l.OperationalState, macString(l.HardwareAddress), l.Driver, l.NetworkFile
	st.Carrier = strings.Contains(l.CarrierState, "carrier") && l.CarrierState != "no-carrier"
	if st.Carrier {
		st.Speed, st.Duplex = sysLinkSpeed(name)
	}
	for _, a := range l.Addresses {
		if a.ScopeString == "link" || a.ScopeString == "host" {
			continue
		}
		st.Addresses = append(st.Addresses, fmt.Sprintf("%s/%d", ipString(a.Address), a.PrefixLength))
		if strings.HasPrefix(a.ConfigSource, "DHCP") {
			st.DHCP = true
		}
	}
	for _, rt := range l.Routes {
		if rt.Family == 2 && rt.DestinationPrefixLength == 0 && len(rt.Gateway) == 4 {
			st.Gateway = ipString(rt.Gateway)
		}
	}
	for _, d := range l.DNS {
		st.DNS = append(st.DNS, ipString(d.Address))
	}
	var leases []Lease
	if l.DHCPServer != nil {
		for _, x := range l.DHCPServer.Leases {
			mac := macString(x.HardwareAddress)
			if mac == "" && len(x.ClientID) == 7 && x.ClientID[0] == 1 {
				mac = macString(x.ClientID[1:])
			}
			leases = append(leases, Lease{MAC: mac, Address: ipString(x.Address), Hostname: x.Hostname, Expires: x.ExpirationRT / 1e6})
		}
	}
	return st, leases
}

// sysLinkSpeed reads the negotiated speed from sysfs; the kernel reports -1
// or fails the read when there is no link.
func sysLinkSpeed(name string) (int, string) {
	b, err := os.ReadFile("/sys/class/net/" + name + "/speed")
	if err != nil {
		return 0, ""
	}
	sp, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if sp < 0 {
		sp = 0
	}
	d, _ := os.ReadFile("/sys/class/net/" + name + "/duplex")
	dx := strings.TrimSpace(string(d))
	if dx == "unknown" {
		dx = ""
	}
	return sp, dx
}
