package main

import (
	"fmt"
	"slices"
	"strconv"
)

// PlanOpts tunes Plan.
type PlanOpts struct {
	Bridge string
	// ResyncVLANs re-adds every port VLAN even when it looks unchanged. The
	// ksz driver sets configure_vlan_while_not_filtering = false: VLANs added
	// while the bridge was VLAN-unaware never reach the switch VLAN table,
	// so the kernel view cannot be trusted after the mode flips.
	ResyncVLANs bool
	// L3 files: runtime networkd directory, nft script, config path for headers.
	RunNetDir, FWFile, CfgPath string
}

func onoff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func b01(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// Plan returns the commands that turn run into want. It is pure: no I/O.
func Plan(want *Config, run *Running, o PlanOpts) []Cmd {
	cmds := []Cmd{}
	br := o.Bridge
	wb, rb := want.Bridge, run.Bridge

	// Bridge globals. Times are centiseconds for iproute2.
	var g []string
	add := func(changed bool, k, v string) {
		if changed {
			g = append(g, k, v)
		}
	}
	add(wb.STP.Enabled != rb.STP, "stp_state", b01(wb.STP.Enabled))
	add(wb.STP.Priority != rb.Priority, "priority", strconv.Itoa(wb.STP.Priority))
	add(wb.STP.HelloTime != rb.HelloTime, "hello_time", strconv.Itoa(wb.STP.HelloTime*100))
	add(wb.STP.ForwardDelay != rb.ForwardDelay, "forward_delay", strconv.Itoa(wb.STP.ForwardDelay*100))
	add(wb.STP.MaxAge != rb.MaxAge, "max_age", strconv.Itoa(wb.STP.MaxAge*100))
	// During an STP topology change the kernel reports the shortened ageing
	// time (= forward delay), not the configured one.
	add(wb.AgeingTime != rb.AgeingTime && !rb.TopologyChange, "ageing_time", strconv.Itoa(wb.AgeingTime*100))
	add(wb.IGMP.Snooping != rb.Snooping, "mcast_snooping", b01(wb.IGMP.Snooping))
	add(wb.IGMP.Querier != rb.Querier, "mcast_querier", b01(wb.IGMP.Querier))
	if len(g) > 0 {
		cmds = append(cmds, cmd(append([]string{"ip", "link", "set", "dev", br, "type", "bridge"}, g...)...))
	}

	resync := o.ResyncVLANs
	if wb.VLANAware && !rb.VLANAware {
		cmds = append(cmds, cmd("ip", "link", "set", "dev", br, "type", "bridge", "vlan_filtering", "1"))
		resync = true
	}

	if wb.VLANAware {
		for _, n := range want.PortNames() {
			if rp := run.Ports[n]; rp == nil || rp.Master != br {
				continue
			}
			cmds = append(cmds, planVLANs(n, false, desiredPortVLANs(want.Ports[n]), run.Ports[n].VLANs, resync)...)
		}
		mgmt := []RunVLAN{{ID: wb.MgmtVLAN, PVID: true, Untagged: true}}
		cmds = append(cmds, planVLANs(br, true, mgmt, run.BridgeVLANs, false)...)
		// The bridge does not create a VLAN-specific local FDB entry for its
		// own MAC when filtering is switched on at runtime; without it frames
		// to the management address are flooded instead of delivered.
		if !run.LocalFDB[wb.MgmtVLAN] || resync {
			cmds = append(cmds, cmdIgn("bridge", "fdb", "add", run.BridgeMAC, "dev", br, "self", "local", "vlan", strconv.Itoa(wb.MgmtVLAN)))
		}
	} else if rb.VLANAware {
		// Leaving 802.1Q mode: reset ports to the bridge default BEFORE
		// turning filtering off. The ksz driver only programs the switch
		// VLAN table while filtering is on, so a reset done afterwards
		// stays in the kernel only and the hardware keeps the old PVIDs
		// and memberships (ports cut off from each other).
		def := []RunVLAN{{ID: 1, PVID: true, Untagged: true}}
		for _, n := range want.PortNames() {
			if rp := run.Ports[n]; rp != nil && rp.Master == br {
				cmds = append(cmds, planVLANs(n, false, def, rp.VLANs, false)...)
			}
		}
		cmds = append(cmds, planVLANs(br, true, def, run.BridgeVLANs, false)...)
		cmds = append(cmds, cmd("ip", "link", "set", "dev", br, "type", "bridge", "vlan_filtering", "0"))
	}

	for _, n := range want.PortNames() {
		wp, rp := want.Ports[n], run.Ports[n]
		if rp == nil {
			continue
		}
		if wp.Enabled != rp.Up {
			state := "down"
			if wp.Enabled {
				state = "up"
			}
			cmds = append(cmds, cmd("ip", "link", "set", "dev", n, state))
		}
		if wp.Description != rp.Alias {
			cmds = append(cmds, cmd("ip", "link", "set", "dev", n, "alias", wp.Description))
		}
		if c, ok := planLink(n, wp, rp.Link); ok {
			cmds = append(cmds, c)
		}
		if rp.Master != br {
			continue // not a bridge port (e.g. sfp not yet in br0)
		}
		var bp []string
		if wp.STPCost > 0 && wp.STPCost != rp.Cost {
			bp = append(bp, "cost", strconv.Itoa(wp.STPCost))
		}
		if wp.STPPriority != rp.Priority {
			bp = append(bp, "priority", strconv.Itoa(wp.STPPriority))
		}
		if wp.Learning != rp.Learning {
			bp = append(bp, "learning", onoff(wp.Learning))
		}
		if wp.Isolated != rp.Isolated {
			bp = append(bp, "isolated", onoff(wp.Isolated))
		}
		if len(bp) > 0 {
			cmds = append(cmds, cmd(append([]string{"bridge", "link", "set", "dev", n}, bp...)...))
		}
	}

	cmds = append(cmds, planFDB(want, run)...)
	cmds = append(cmds, planMirror(want.Mirror, run.Mirror)...)
	cmds = append(cmds, planNetwork(want, run, o)...)
	return cmds
}

func desiredPortVLANs(p *Port) []RunVLAN {
	var v []RunVLAN
	if p.PVID > 0 {
		v = append(v, RunVLAN{ID: p.PVID, PVID: true, Untagged: true})
	}
	for _, t := range p.Tagged {
		v = append(v, RunVLAN{ID: t})
	}
	return v
}

// planVLANs adds new/changed VLANs first (so a PVID move never leaves the
// port without one), then deletes the rest. Re-adding an existing VLAN
// replaces its flags in the kernel.
func planVLANs(dev string, self bool, want, have []RunVLAN, resync bool) []Cmd {
	var cmds []Cmd
	tail := func(c []string) []string {
		if self {
			c = append(c, "self")
		}
		return c
	}
	if resync {
		for _, h := range have {
			cmds = append(cmds, cmdIgn(tail([]string{"bridge", "vlan", "del", "vid", strconv.Itoa(h.ID), "dev", dev})...))
		}
		have = nil
	}
	for _, w := range want {
		i := slices.IndexFunc(have, func(h RunVLAN) bool { return h.ID == w.ID })
		if i >= 0 && have[i] == w {
			continue
		}
		c := tail([]string{"bridge", "vlan", "add", "vid", strconv.Itoa(w.ID), "dev", dev})
		if w.PVID {
			c = append(c, "pvid")
		}
		if w.Untagged {
			c = append(c, "untagged")
		}
		cmds = append(cmds, cmd(c...))
	}
	for _, h := range have {
		if !slices.ContainsFunc(want, func(w RunVLAN) bool { return w.ID == h.ID }) {
			cmds = append(cmds, cmd(tail([]string{"bridge", "vlan", "del", "vid", strconv.Itoa(h.ID), "dev", dev})...))
		}
	}
	return cmds
}

func planLink(port string, wp *Port, l LinkInfo) (Cmd, bool) {
	if len(supportedSpeedModes(l.Supported)) == 0 {
		return Cmd{}, false // no copper PHY (sfp) or ethtool failed
	}
	speed, an := l.SpeedSetting()
	if speed == wp.Speed && (speed == "auto" || an == wp.Autoneg) {
		return Cmd{}, false
	}
	if wp.Speed == "auto" {
		return cmd("ethtool", "-s", port, "autoneg", "on", "advertise", advMask(supportedSpeedModes(l.Supported))), true
	}
	m := speedModes[wp.Speed]
	if wp.Autoneg {
		return cmd("ethtool", "-s", port, "autoneg", "on", "advertise", advMask([]string{m.mode})), true
	}
	return cmd("ethtool", "-s", port, "autoneg", "off", "speed", strconv.Itoa(m.speed), "duplex", m.duplex), true
}

// advMask converts link mode names to the legacy ethtool advertise bitmask.
func advMask(modes []string) string {
	bits := map[string]uint{"10baseT/Half": 0x1, "10baseT/Full": 0x2, "100baseT/Half": 0x4, "100baseT/Full": 0x8, "1000baseT/Full": 0x20}
	var m uint
	for _, x := range modes {
		m |= bits[x]
	}
	if m == 0 {
		m = 0x2f
	}
	return fmt.Sprintf("0x%x", m)
}

func planFDB(want *Config, run *Running) []Cmd {
	key := func(e FDBEntry) string { return fmt.Sprintf("%s/%s/%d", e.MAC, e.Port, e.VLAN) }
	args := func(op string, e FDBEntry) []string {
		a := []string{"bridge", "fdb", op, e.MAC, "dev", e.Port, "master"}
		if op == "add" {
			a = append(a, "static")
		}
		if e.VLAN > 0 {
			a = append(a, "vlan", strconv.Itoa(e.VLAN))
		}
		return a
	}
	have := map[string]bool{}
	for _, e := range run.StaticFDB {
		have[key(e)] = true
	}
	wantSet := map[string]bool{}
	var cmds []Cmd
	for _, e := range want.StaticFDB {
		wantSet[key(e)] = true
		if !have[key(e)] {
			cmds = append(cmds, cmd(args("add", e)...))
		}
	}
	for _, e := range run.StaticFDB {
		if !wantSet[key(e)] {
			cmds = append(cmds, cmd(args("del", e)...))
		}
	}
	return cmds
}

func mirrorEqual(a, b *Mirror) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Dest == b.Dest && slices.Equal(a.Sources, b.Sources)
}

// planMirror rebuilds the session with hardware-only (skip_sw) matchall
// filters: the switch has a single sniffer port, and a software fallback
// would push the mirrored traffic through the 100 Mbit/s CPU link.
func planMirror(want, have *Mirror) []Cmd {
	if mirrorEqual(want, have) {
		return nil
	}
	var cmds []Cmd
	touched := map[string]bool{}
	for _, m := range []*Mirror{have, want} {
		if m == nil {
			continue
		}
		for _, s := range m.Sources {
			if !touched[s.Port] {
				touched[s.Port] = true
				cmds = append(cmds, cmdIgn("tc", "qdisc", "del", "dev", s.Port, "clsact"))
			}
		}
	}
	if want == nil {
		return cmds
	}
	for _, s := range want.Sources {
		cmds = append(cmds, cmd("tc", "qdisc", "add", "dev", s.Port, "clsact"))
		for _, hook := range map[string][]string{"rx": {"ingress"}, "tx": {"egress"}, "both": {"ingress", "egress"}}[s.Direction] {
			cmds = append(cmds, cmd("tc", "filter", "add", "dev", s.Port, hook, "matchall", "skip_sw",
				"action", "mirred", "egress", "mirror", "dev", want.Dest))
		}
	}
	return cmds
}
