package main

import (
	"encoding/json"
	"fmt"
	"net"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// Config is the desired switch configuration. It is what the API edits,
// what is saved to disk and what gets rendered into networkd drop-ins.
type Config struct {
	System    System           `json:"system"`
	Bridge    Bridge           `json:"bridge"`
	VLANs     []VLAN           `json:"vlans"`
	Ports     map[string]*Port `json:"ports"`
	StaticFDB []FDBEntry       `json:"static_fdb"`
	Mirror    *Mirror          `json:"mirror"`
	// MRP is the ring redundancy instance (IEC 62439-2), nil: off.
	MRP *MRP `json:"mrp"`
	// Network is the L3 setup (WAN, LAN address, router mode). nil: not
	// managed by the daemon, netplan's files apply.
	Network *Network `json:"network"`
}

type System struct {
	Name     string `json:"name"`
	Location string `json:"location"`
	Contact  string `json:"contact"`
}

type Bridge struct {
	// VLANAware switches the bridge to 802.1Q mode. When false, port VLAN
	// settings are kept in the config but not applied.
	VLANAware bool `json:"vlan_aware"`
	// MgmtVLAN is the VLAN the management interface (br0 itself) lives in,
	// untagged. Only used in 802.1Q mode.
	MgmtVLAN   int  `json:"mgmt_vlan"`
	AgeingTime int  `json:"ageing_time"` // seconds
	STP        STP  `json:"stp"`
	IGMP       IGMP `json:"igmp"`
}

type STP struct {
	Enabled      bool `json:"enabled"`
	Priority     int  `json:"priority"`      // 0..61440, step 4096
	HelloTime    int  `json:"hello_time"`    // seconds
	ForwardDelay int  `json:"forward_delay"` // seconds
	MaxAge       int  `json:"max_age"`       // seconds
}

type IGMP struct {
	Snooping bool `json:"snooping"`
	Querier  bool `json:"querier"`
}

type VLAN struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

type Port struct {
	Enabled     bool   `json:"enabled"`
	Description string `json:"description"`
	// Speed: auto, 1000full, 100full, 100half, 10full, 10half. With Autoneg
	// a fixed speed is negotiated as the only advertised mode; without it the
	// link is forced (not possible for 1000full).
	Speed   string `json:"speed"`
	Autoneg bool   `json:"autoneg"`
	// PVID is the untagged (native/access) VLAN, 0 for none. Tagged lists the
	// VLANs carried tagged. Access port = PVID only, trunk = PVID + Tagged.
	PVID        int   `json:"pvid"`
	Tagged      []int `json:"tagged"`
	Learning    bool  `json:"learning"`
	Isolated    bool  `json:"isolated"`
	STPCost     int   `json:"stp_cost"`     // 0 = automatic (not set)
	STPPriority int   `json:"stp_priority"` // 0..63
	// FlowControl advertises IEEE 802.3x pause in autonegotiation (forces it
	// on a fixed-speed link). Off by default, as on industrial switches
	// (Moxa EDS: "FDX Flow Ctrl" factory default Disabled): with pause a
	// slow port stalls every flow of the sender, not just its own.
	FlowControl bool `json:"flow_control"`
}

type FDBEntry struct {
	MAC  string `json:"mac"`
	Port string `json:"port"`
	VLAN int    `json:"vlan"` // 0 in VLAN-unaware mode
}

type Mirror struct {
	Dest    string         `json:"dest"`
	Sources []MirrorSource `json:"sources"`
}

type MirrorSource struct {
	Port      string `json:"port"`
	Direction string `json:"direction"` // rx, tx, both
}

// MRP is one Media Redundancy Protocol ring. The bridge runs it in
// software (the KSZ9567 has no MRP offload) through mrp_server.
type MRP struct {
	RingID    int    `json:"ring_id"`   // 1..
	Role      string `json:"role"`      // mrm (manager) or mrc (client)
	Primary   string `json:"primary"`   // ring port
	Secondary string `json:"secondary"` // ring port; the one taken down while the ring is (re)built
	Profile   int    `json:"profile"`   // recovery time profile, ms: 500 or 200
}

// mrpProfiles are the recovery profiles a software ring can keep: test
// frames every 50 ms (500) or 20 ms (200). The 30 and 10 ms profiles need
// 3.5 and 1 ms timers, beyond a kernel workqueue on this CPU.
var mrpProfiles = []int{500, 200}

func mrpEqual(a, b *MRP) bool {
	if a == nil || b == nil {
		return a == b
	}
	// mrp_server swaps primary and secondary at runtime (the first port up
	// becomes primary), so the ring is the same either way round.
	samePorts := (a.Primary == b.Primary && a.Secondary == b.Secondary) || (a.Primary == b.Secondary && a.Secondary == b.Primary)
	return a.RingID == b.RingID && a.Role == b.Role && a.Profile == b.Profile && samePorts
}

var speedModes = map[string]struct {
	speed  int
	duplex string
	mode   string // ethtool link mode name
}{
	"1000full": {1000, "full", "1000baseT/Full"},
	"100full":  {100, "full", "100baseT/Full"},
	"100half":  {100, "half", "100baseT/Half"},
	"10full":   {10, "full", "10baseT/Full"},
	"10half":   {10, "half", "10baseT/Half"},
}

func (c *Config) Clone() *Config {
	b, _ := json.Marshal(c)
	var n Config
	_ = json.Unmarshal(b, &n)
	return &n
}

func (c *Config) PortNames() []string {
	n := make([]string, 0, len(c.Ports))
	for k := range c.Ports {
		n = append(n, k)
	}
	sort.Strings(n)
	return n
}

func (c *Config) hasVLAN(id int) bool {
	return slices.ContainsFunc(c.VLANs, func(v VLAN) bool { return v.ID == id })
}

// Normalize sorts lists so that equal configs compare and render identically.
func (c *Config) Normalize() {
	sort.Slice(c.VLANs, func(i, j int) bool { return c.VLANs[i].ID < c.VLANs[j].ID })
	for _, p := range c.Ports {
		if p.Tagged == nil {
			p.Tagged = []int{}
		}
		sort.Ints(p.Tagged)
		p.Tagged = slices.Compact(p.Tagged)
		if p.Speed == "" {
			p.Speed = "auto"
		}
	}
	for i := range c.StaticFDB {
		c.StaticFDB[i].MAC = strings.ToLower(c.StaticFDB[i].MAC)
	}
	sort.Slice(c.StaticFDB, func(i, j int) bool {
		a, b := c.StaticFDB[i], c.StaticFDB[j]
		if a.Port != b.Port {
			return a.Port < b.Port
		}
		if a.VLAN != b.VLAN {
			return a.VLAN < b.VLAN
		}
		return a.MAC < b.MAC
	})
	if c.VLANs == nil {
		c.VLANs = []VLAN{}
	}
	if c.StaticFDB == nil {
		c.StaticFDB = []FDBEntry{}
	}
	if c.Mirror != nil {
		sort.Slice(c.Mirror.Sources, func(i, j int) bool { return c.Mirror.Sources[i].Port < c.Mirror.Sources[j].Port })
		if c.Mirror.Dest == "" && len(c.Mirror.Sources) == 0 {
			c.Mirror = nil
		}
	}
	if c.Network != nil {
		c.Network.Normalize()
	}
}

// ValidationError lists every problem found, so a UI can show them all at once.
type ValidationError struct{ Problems []string }

func (e *ValidationError) Error() string { return "invalid config: " + strings.Join(e.Problems, "; ") }

var descRe = regexp.MustCompile(`^[\x20-\x7e\p{L}\p{N}]*$`)

// Validate checks the config against the switch ports and hardware limits.
// It returns warnings (config is accepted) or a *ValidationError.
func (c *Config) Validate(ports []string) (warnings []string, err error) {
	var p []string
	bad := func(f string, a ...any) { p = append(p, fmt.Sprintf(f, a...)) }

	for _, f := range []struct{ name, v string }{{"system.name", c.System.Name}, {"system.location", c.System.Location}, {"system.contact", c.System.Contact}} {
		if len(f.v) > 64 || !descRe.MatchString(f.v) {
			bad("%s: up to 64 printable characters", f.name)
		}
	}

	seen := map[int]bool{}
	fid := map[int]int{}
	for _, v := range c.VLANs {
		if v.ID < 1 || v.ID > 4094 {
			bad("vlan %d: id must be 1..4094", v.ID)
		}
		if seen[v.ID] {
			bad("vlan %d: duplicate", v.ID)
		}
		seen[v.ID] = true
		if len(v.Name) > 32 || !descRe.MatchString(v.Name) {
			bad("vlan %d: name up to 32 printable characters", v.ID)
		}
		// KSZ9477 family: FID = VID & 0x7f, VLANs 128 apart share one MAC table.
		if o, ok := fid[v.ID&0x7f]; ok {
			warnings = append(warnings, fmt.Sprintf("vlans %d and %d share a hardware MAC learning table (VID mod 128)", o, v.ID))
		} else {
			fid[v.ID&0x7f] = v.ID
		}
	}

	b := c.Bridge
	if b.VLANAware {
		if !c.hasVLAN(b.MgmtVLAN) {
			bad("bridge.mgmt_vlan %d: not in vlans", b.MgmtVLAN)
		}
	}
	if b.AgeingTime < 10 || b.AgeingTime > 1000000 {
		bad("bridge.ageing_time: 10..1000000 s")
	}
	s := b.STP
	if s.Priority < 0 || s.Priority > 61440 || s.Priority%4096 != 0 {
		bad("bridge.stp.priority: 0..61440 in steps of 4096")
	}
	if s.HelloTime < 1 || s.HelloTime > 10 {
		bad("bridge.stp.hello_time: 1..10 s")
	}
	if s.ForwardDelay < 4 || s.ForwardDelay > 30 {
		bad("bridge.stp.forward_delay: 4..30 s")
	}
	if s.MaxAge < 6 || s.MaxAge > 40 {
		bad("bridge.stp.max_age: 6..40 s")
	}
	if !(2*(s.ForwardDelay-1) >= s.MaxAge && s.MaxAge >= 2*(s.HelloTime+1)) {
		bad("bridge.stp: 802.1D requires 2*(forward_delay-1) >= max_age >= 2*(hello_time+1)")
	}

	known := map[string]bool{}
	for _, n := range ports {
		known[n] = true
	}
	for _, n := range ports {
		if c.Ports[n] == nil {
			bad("port %s: missing", n)
		}
	}
	carried := map[int]bool{}
	for _, n := range c.PortNames() {
		pt := c.Ports[n]
		if !known[n] {
			bad("port %s: no such switch port", n)
			continue
		}
		if len(pt.Description) > 64 || !descRe.MatchString(pt.Description) {
			bad("port %s: description up to 64 printable characters", n)
		}
		if pt.Speed != "auto" {
			m, ok := speedModes[pt.Speed]
			if !ok {
				bad("port %s: speed must be auto, 1000full, 100full, 100half, 10full or 10half", n)
			} else if m.speed == 1000 && !pt.Autoneg {
				bad("port %s: 1000full requires autoneg", n)
			}
		}
		if pt.PVID != 0 {
			if !c.hasVLAN(pt.PVID) {
				bad("port %s: pvid %d not in vlans", n, pt.PVID)
			}
			carried[pt.PVID] = true
		}
		for _, t := range pt.Tagged {
			if !c.hasVLAN(t) {
				bad("port %s: tagged vlan %d not in vlans", n, t)
			}
			if t == pt.PVID {
				bad("port %s: vlan %d is both pvid and tagged", n, t)
			}
			carried[t] = true
		}
		if b.VLANAware && pt.PVID == 0 && len(pt.Tagged) == 0 && pt.Enabled {
			warnings = append(warnings, fmt.Sprintf("port %s: enabled but carries no vlan", n))
		}
		if pt.STPCost < 0 || pt.STPCost > 65535 {
			bad("port %s: stp_cost 0..65535", n)
		}
		if pt.STPPriority < 0 || pt.STPPriority > 63 {
			bad("port %s: stp_priority 0..63", n)
		}
	}
	if b.VLANAware && !carried[b.MgmtVLAN] {
		warnings = append(warnings, fmt.Sprintf("mgmt vlan %d is not carried by any port: management only via the USB interface", b.MgmtVLAN))
	}

	fdbSeen := map[string]bool{}
	for _, e := range c.StaticFDB {
		hw, perr := net.ParseMAC(e.MAC)
		switch {
		case perr != nil || len(hw) != 6:
			bad("static_fdb %s: bad MAC", e.MAC)
		case hw[0]&1 == 1:
			bad("static_fdb %s: multicast MAC not allowed (use IGMP snooping)", e.MAC)
		}
		if !known[e.Port] {
			bad("static_fdb %s: no such port %q", e.MAC, e.Port)
		}
		if b.VLANAware {
			if !c.hasVLAN(e.VLAN) {
				bad("static_fdb %s: vlan %d not in vlans", e.MAC, e.VLAN)
			} else if pt := c.Ports[e.Port]; pt != nil && pt.PVID != e.VLAN && !slices.Contains(pt.Tagged, e.VLAN) {
				bad("static_fdb %s: port %s is not a member of vlan %d", e.MAC, e.Port, e.VLAN)
			}
		} else if e.VLAN != 0 {
			bad("static_fdb %s: vlan must be 0 when bridge is not vlan_aware", e.MAC)
		}
		k := fmt.Sprintf("%s/%d", strings.ToLower(e.MAC), e.VLAN)
		if fdbSeen[k] {
			bad("static_fdb %s vlan %d: duplicate", e.MAC, e.VLAN)
		}
		fdbSeen[k] = true
	}

	if m := c.Mirror; m != nil {
		if !known[m.Dest] {
			bad("mirror.dest %q: no such port", m.Dest)
		}
		if len(m.Sources) == 0 {
			bad("mirror: at least one source (or mirror: null)")
		}
		srcSeen := map[string]bool{}
		for _, s := range m.Sources {
			if !known[s.Port] {
				bad("mirror source %q: no such port", s.Port)
			}
			if s.Port == m.Dest {
				bad("mirror source %s: is the destination", s.Port)
			}
			if s.Direction != "rx" && s.Direction != "tx" && s.Direction != "both" {
				bad("mirror source %s: direction rx, tx or both", s.Port)
			}
			if srcSeen[s.Port] {
				bad("mirror source %s: duplicate", s.Port)
			}
			srcSeen[s.Port] = true
		}
	}

	if r := c.MRP; r != nil {
		if r.RingID < 1 || r.RingID > 65535 {
			bad("mrp.ring_id: 1..65535")
		}
		if r.Role != "mrm" && r.Role != "mrc" {
			bad("mrp.role: mrm or mrc")
		}
		if !slices.Contains(mrpProfiles, r.Profile) {
			bad("mrp.profile: 500 or 200 (ms)")
		}
		for _, f := range []struct{ name, port string }{{"primary", r.Primary}, {"secondary", r.Secondary}} {
			switch pt := c.Ports[f.port]; {
			case !known[f.port]:
				bad("mrp.%s %q: no such port", f.name, f.port)
			case pt != nil && !pt.Enabled:
				bad("mrp.%s %s: port is disabled", f.name, f.port)
			}
			if m := c.Mirror; m != nil && m.Dest == f.port {
				bad("mrp.%s %s: is the mirror destination", f.name, f.port)
			}
		}
		if r.Primary == r.Secondary {
			bad("mrp: primary and secondary must differ")
		}
		if b.STP.Enabled {
			bad("mrp: the kernel runs either STP or MRP on a bridge - set bridge.stp.enabled false")
		}
	}

	if len(p) > 0 {
		return warnings, &ValidationError{p}
	}
	return warnings, nil
}

// DefaultConfig is a plain unmanaged-switch setup: all ports in VLAN 1.
func DefaultConfig(ports []string) *Config {
	c := &Config{
		Bridge: Bridge{MgmtVLAN: 1, AgeingTime: 300,
			STP:  STP{Enabled: true, Priority: 32768, HelloTime: 2, ForwardDelay: 15, MaxAge: 20},
			IGMP: IGMP{Snooping: true}},
		VLANs: []VLAN{{ID: 1, Name: "default"}},
		Ports: map[string]*Port{},
	}
	for _, n := range ports {
		c.Ports[n] = &Port{Enabled: true, Speed: "auto", Autoneg: true, PVID: 1, Learning: true, STPPriority: 32}
	}
	c.Normalize()
	return c
}
