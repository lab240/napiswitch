package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// Running is the live state read from the kernel.
type Running struct {
	Conduit     string              `json:"conduit"`
	BridgeMAC   string              `json:"bridge_mac"`
	Bridge      RunBridge           `json:"bridge"`
	BridgeVLANs []RunVLAN           `json:"bridge_vlans"` // VLANs of br0 itself
	LocalFDB    map[int]bool        `json:"-"`            // VLANs with a local FDB entry for BridgeMAC
	Ports       map[string]*RunPort `json:"ports"`
	StaticFDB   []FDBEntry          `json:"static_fdb"`
	Mirror      *Mirror             `json:"mirror"`
	MRP         *RunMRP             `json:"mrp"`
	MRPAvail    bool                `json:"mrp_available"` // mrp_server answers
	Ethers      []string            `json:"ethers"`        // physical NICs outside the switch (WAN candidates)
	Net         *NetState           `json:"net"`           // nil when not read (tests)
}

type RunBridge struct {
	Present        bool   `json:"present"`
	VLANAware      bool   `json:"vlan_aware"`
	DefaultPVID    int    `json:"default_pvid"`
	STP            bool   `json:"stp"`
	Priority       int    `json:"priority"`
	HelloTime      int    `json:"hello_time"`
	ForwardDelay   int    `json:"forward_delay"`
	MaxAge         int    `json:"max_age"`
	AgeingTime     int    `json:"ageing_time"`
	Snooping       bool   `json:"igmp_snooping"`
	Querier        bool   `json:"igmp_querier"`
	BridgeID       string `json:"bridge_id"`
	RootID         string `json:"root_id"`
	RootPort       int    `json:"root_port"`
	RootPathCost   int    `json:"root_path_cost"`
	TopologyChange bool   `json:"topology_change"`
}

type RunPort struct {
	Name     string    `json:"name"`
	Index    int       `json:"index"` // ifindex
	HWPort   string    `json:"hw_port"`
	Up       bool      `json:"admin_up"`
	Oper     string    `json:"oper_state"`
	Master   string    `json:"master"`
	MAC      string    `json:"mac"`
	Alias    string    `json:"alias"`
	STPState string    `json:"stp_state"`
	Cost     int       `json:"stp_cost"`
	Priority int       `json:"stp_priority"`
	Learning bool      `json:"learning"`
	Isolated bool      `json:"isolated"`
	VLANs    []RunVLAN `json:"vlans"`
	Link     LinkInfo  `json:"link"`
}

type RunVLAN struct {
	ID       int  `json:"id"`
	PVID     bool `json:"pvid"`
	Untagged bool `json:"untagged"`
}

type LinkInfo struct {
	Up         bool     `json:"up"`
	Speed      int      `json:"speed"` // Mb/s, 0 = unknown
	Duplex     string   `json:"duplex"`
	Autoneg    bool     `json:"autoneg"`
	Supported  []string `json:"supported_modes"`
	Advertised []string `json:"advertised_modes"`
	Partner    []string `json:"partner_modes"`
	SQI        int      `json:"sqi"`     // signal quality, -1 = n/a
	SQIMax     int      `json:"sqi_max"` // -1 = n/a
	// Pause (802.3x flow control): what the port advertises ("No",
	// "Symmetric", ...), the ethtool -A setting and the negotiated result.
	PauseSupported  string `json:"pause_supported"`
	PauseAdvertised string `json:"pause_advertised"`
	PauseRx         bool   `json:"-"` // ethtool -A setting
	PauseTx         bool   `json:"-"`
	FlowControl     bool   `json:"flow_control_active"` // negotiated rx or tx pause
}

// PauseOn reports whether the port advertises (or forces) pause.
func (l LinkInfo) PauseOn() bool { return l.PauseAdvertised != "" && l.PauseAdvertised != "No" }

// SpeedSetting derives the config-style speed value from ethtool state.
func (l LinkInfo) SpeedSetting() (speed string, autoneg bool) {
	if !l.Autoneg {
		for k, m := range speedModes {
			if m.speed == l.Speed && strings.EqualFold(m.duplex, l.Duplex) {
				return k, false
			}
		}
		return "unknown", false
	}
	sup := supportedSpeedModes(l.Supported)
	adv := supportedSpeedModes(l.Advertised)
	if slices.Equal(sup, adv) {
		return "auto", true
	}
	if len(adv) == 1 {
		for k, m := range speedModes {
			if m.mode == adv[0] {
				return k, true
			}
		}
	}
	return "custom", true
}

func supportedSpeedModes(modes []string) []string {
	var r []string
	for _, m := range modes {
		for _, s := range speedModes {
			if s.mode == m {
				r = append(r, m)
			}
		}
	}
	sort.Strings(r)
	return r
}

type ipLink struct {
	Ifindex   int      `json:"ifindex"`
	Ifname    string   `json:"ifname"`
	Link      string   `json:"link"`
	Flags     []string `json:"flags"`
	Master    string   `json:"master"`
	Operstate string   `json:"operstate"`
	Address   string   `json:"address"`
	Ifalias   string   `json:"ifalias"`
	LinkType  string   `json:"link_type"`
	PhysPort  string   `json:"phys_port_name"`
	Linkinfo  struct {
		InfoKind      string          `json:"info_kind"`
		InfoData      json.RawMessage `json:"info_data"`
		InfoSlaveKind string          `json:"info_slave_kind"`
		InfoSlaveData json.RawMessage `json:"info_slave_data"`
	} `json:"linkinfo"`
}

type brInfo struct {
	ForwardDelay   float64 `json:"forward_delay"`
	HelloTime      float64 `json:"hello_time"`
	MaxAge         float64 `json:"max_age"`
	AgeingTime     float64 `json:"ageing_time"`
	StpState       int     `json:"stp_state"`
	Priority       int     `json:"priority"`
	VlanFiltering  int     `json:"vlan_filtering"`
	VlanDefault    int     `json:"vlan_default_pvid"`
	McastSnooping  int     `json:"mcast_snooping"`
	McastQuerier   int     `json:"mcast_querier"`
	BridgeID       string  `json:"bridge_id"`
	RootID         string  `json:"root_id"`
	RootPort       int     `json:"root_port"`
	RootPathCost   int     `json:"root_path_cost"`
	TopologyChange int     `json:"topology_change"`
	TCDetected     int     `json:"topology_change_detected"`
}

type brPortInfo struct {
	State    string `json:"state"`
	Priority int    `json:"priority"`
	Cost     int    `json:"cost"`
	Learning bool   `json:"learning"`
	Isolated bool   `json:"isolated"`
}

func centi(v float64) int { return int(math.Round(v / 100)) }

// ReadRunning collects bridge, ports, VLANs, static FDB and mirror state.
func ReadRunning(r Runner, bridge string) (*Running, error) {
	out, err := r.Output("ip", "-j", "-d", "link", "show")
	if err != nil {
		return nil, err
	}
	var links []ipLink
	if err := json.Unmarshal(out, &links); err != nil {
		return nil, fmt.Errorf("ip link json: %w", err)
	}
	run := &Running{Ports: map[string]*RunPort{}, LocalFDB: map[int]bool{}}
	for _, l := range links {
		switch {
		case l.Ifname == bridge && l.Linkinfo.InfoKind == "bridge":
			var bi brInfo
			if err := json.Unmarshal(l.Linkinfo.InfoData, &bi); err != nil {
				return nil, fmt.Errorf("bridge info: %w", err)
			}
			run.BridgeMAC = strings.ToLower(l.Address)
			run.Bridge = RunBridge{Present: true, VLANAware: bi.VlanFiltering == 1, DefaultPVID: bi.VlanDefault,
				STP: bi.StpState != 0, Priority: bi.Priority,
				HelloTime: centi(bi.HelloTime), ForwardDelay: centi(bi.ForwardDelay), MaxAge: centi(bi.MaxAge),
				AgeingTime: centi(bi.AgeingTime), Snooping: bi.McastSnooping == 1, Querier: bi.McastQuerier == 1,
				BridgeID: bi.BridgeID, RootID: bi.RootID, RootPort: bi.RootPort, RootPathCost: bi.RootPathCost,
				TopologyChange: bi.TopologyChange != 0 || bi.TCDetected != 0}
		case l.Linkinfo.InfoKind == "dsa":
			var conduit struct {
				Conduit string `json:"conduit"`
			}
			_ = json.Unmarshal(l.Linkinfo.InfoData, &conduit)
			run.Conduit = conduit.Conduit
			p := &RunPort{Name: l.Ifname, Index: l.Ifindex, HWPort: l.PhysPort, Up: slices.Contains(l.Flags, "UP"),
				Oper: l.Operstate, Master: l.Master, MAC: strings.ToLower(l.Address), Alias: l.Ifalias, Priority: -1}
			if l.Linkinfo.InfoSlaveKind == "bridge" {
				var bp brPortInfo
				if err := json.Unmarshal(l.Linkinfo.InfoSlaveData, &bp); err == nil {
					p.STPState, p.Cost, p.Priority, p.Learning, p.Isolated = bp.State, bp.Cost, bp.Priority, bp.Learning, bp.Isolated
				}
			}
			run.Ports[l.Ifname] = p
		}
	}
	for _, l := range links {
		if l.LinkType == "ether" && l.Linkinfo.InfoKind == "" && l.Ifname != run.Conduit && l.Master == "" {
			run.Ethers = append(run.Ethers, l.Ifname)
		}
	}
	sort.Strings(run.Ethers)
	if !run.Bridge.Present {
		return run, fmt.Errorf("bridge %s not found", bridge)
	}

	if err := readVLANs(r, run, bridge); err != nil {
		return nil, err
	}
	if err := readFDB(r, run, bridge); err != nil {
		return nil, err
	}
	for _, p := range run.Ports {
		p.Link = readLink(r, p.Name)
	}
	run.Mirror = readMirror(r, run)
	run.MRP, run.MRPAvail = readMRP(r, bridge)
	return run, nil
}

// RunMRP is a ring as mrp_server reports it. Primary is the port the
// manager currently uses as primary (it may swap them).
type RunMRP struct {
	MRP
	State string `json:"state"` // MRM: AC_STAT1, PRM_UP, CHK_RO, CHK_RC; MRC: DE_IDLE, PT, DE, PT_IDLE
}

// mrpCLI is mrp_server's client (Microchip mrp). Without it MRP is unavailable.
var mrpCLI = "/usr/local/sbin/mrp"

// readMRP parses `mrp getmrp`: "key: value" pairs, a new ring at each
// "bridge:". Only the first ring of bridge is used.
func readMRP(r Runner, bridge string) (*RunMRP, bool) {
	out, err := r.Output(mrpCLI, "getmrp")
	if err != nil {
		return nil, false
	}
	return parseMRP(string(out), bridge), true
}

func parseMRP(out, bridge string) *RunMRP {
	f := strings.Fields(out)
	var rings []map[string]string
	for i := 0; i+1 < len(f); i++ {
		k, ok := strings.CutSuffix(f[i], ":")
		if !ok {
			continue
		}
		if k == "bridge" {
			rings = append(rings, map[string]string{})
		}
		if len(rings) > 0 {
			rings[len(rings)-1][k] = f[i+1]
			i++
		}
	}
	for _, m := range rings {
		if m["bridge"] != bridge {
			continue
		}
		id, _ := strconv.Atoi(m["ring_nr"])
		prof, _ := strconv.Atoi(m["ring_recv"])
		return &RunMRP{MRP: MRP{RingID: id, Role: strings.ToLower(m["ring_role"]), Primary: m["pport"],
			Secondary: m["sport"], Profile: prof}, State: m["ring_state"]}
	}
	return nil
}

// The PTP stack: pmc queries the running ptp4l, ptpConf names the
// interfaces (ptp4l numbers ports 1..N in the order of its config
// sections), ptpDev is the hardware clock the KSZ exposes.
var (
	ptpCLI     = "/usr/sbin/pmc"
	ptpConf    = "/etc/linuxptp/napi-ptp.conf"
	ptpDev     = "/dev/ptp0"
	ptpService = "ptp4l-napi" // systemd unit running ptp4l
)

// PTPConfig is the editable part of the ptp4l config: the BMCA role and the
// announced priority1. mode is "master" (can become grandmaster) or "slave"
// (slaveOnly, never a master). priority1 decides who wins BMCA (lower wins).
type PTPConfig struct {
	Mode      string `json:"mode"`
	Priority1 int    `json:"priority1"`
}

// readPTPConfig reads the mode/priority1 from napi-ptp.conf. Defaults to
// master/64 when the keys are absent (ptp4l's own defaults).
func readPTPConfig(path string) *PTPConfig {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	c := &PTPConfig{Mode: "master", Priority1: 64}
	inGlobal := false
	for _, ln := range strings.Split(string(b), "\n") {
		t := strings.TrimSpace(ln)
		if len(t) > 1 && t[0] == '[' {
			inGlobal = t == "[global]"
			continue
		}
		if !inGlobal {
			continue
		}
		f := strings.Fields(t)
		if len(f) < 2 {
			continue
		}
		switch f[0] {
		case "slaveOnly":
			if f[1] == "1" {
				c.Mode = "slave"
			}
		case "priority1":
			n, _ := strconv.Atoi(f[1])
			c.Priority1 = n
		}
	}
	return c
}

// buildPTPConf rewrites slaveOnly and priority1 in the [global] section of an
// existing ptp4l config, preserving every other line and the interface
// sections. Missing keys are appended to [global].
func buildPTPConf(existing, mode string, prio int) string {
	slave := "0"
	if mode == "slave" {
		slave = "1"
	}
	want := []struct{ k, v string }{{"slaveOnly", slave}, {"priority1", strconv.Itoa(prio)}}
	set := func(k string) (string, bool) {
		for _, w := range want {
			if w.k == k {
				return w.v, true
			}
		}
		return "", false
	}
	seen := map[string]bool{}
	var out []string
	inGlobal := false
	flushMissing := func() {
		for _, w := range want {
			if !seen[w.k] {
				out = append(out, fmt.Sprintf("%-23s %s", w.k, w.v))
				seen[w.k] = true
			}
		}
	}
	for _, ln := range strings.Split(existing, "\n") {
		t := strings.TrimSpace(ln)
		if len(t) > 1 && t[0] == '[' {
			if inGlobal {
				flushMissing() // append keys missing from [global] before leaving it
			}
			inGlobal = t == "[global]"
			out = append(out, ln)
			continue
		}
		if inGlobal {
			if f := strings.Fields(t); len(f) >= 1 {
				if v, ok := set(f[0]); ok {
					out = append(out, fmt.Sprintf("%-23s %s", f[0], v))
					seen[f[0]] = true
					continue
				}
			}
		}
		out = append(out, ln)
	}
	if inGlobal {
		flushMissing()
	}
	return strings.Join(out, "\n")
}

// RunPTP is the IEEE 1588 state read from ptp4l via pmc.
type RunPTP struct {
	ClockID    string    `json:"clock_id"`
	GM         string    `json:"gm"`        // grandmaster clock identity
	GMLocal    bool      `json:"gm_local"`  // this switch is the grandmaster
	Priority1  int       `json:"priority1"` // our announced priority1
	ClockClass int       `json:"clock_class"`
	OneStep    bool      `json:"one_step"`      // one-step TX (KSZ hardware)
	StepsRem   int       `json:"steps_removed"` // hops from the grandmaster
	OffsetNS   int64     `json:"offset_ns"`     // offset from master (slave only)
	Ports      []PTPPort `json:"ports"`
}

// PTPPort is one ptp4l port (a bridged interface) and its role.
type PTPPort struct {
	Name        string `json:"name"`
	State       string `json:"state"`         // MASTER, SLAVE, LISTENING, ...
	PeerDelayNS int64  `json:"peer_delay_ns"` // P2P peer mean path delay
}

// readPTP reports the PTP stack: available is true when a hardware clock
// is present, running when ptp4l answers pmc. pmc fails fast when ptp4l
// is down, so this never blocks for long.
func readPTP(r Runner) (*RunPTP, bool, bool) {
	_, statErr := os.Stat(ptpDev)
	avail := statErr == nil
	out, err := r.Output(ptpCLI, "-u", "-b", "0",
		"GET DEFAULT_DATA_SET", "GET PARENT_DATA_SET", "GET CURRENT_DATA_SET", "GET PORT_DATA_SET")
	if err != nil {
		return nil, avail, false
	}
	p := parsePTP(string(out), ptpIfaces(ptpConf))
	if p == nil {
		return nil, avail, false
	}
	return p, avail, true
}

// ptpIfaces lists the interface names from a ptp4l config, in order, so
// pmc's 1-based port numbers map back to names.
func ptpIfaces(path string) []string {
	var names []string
	b, err := os.ReadFile(path)
	if err != nil {
		return names
	}
	for _, ln := range strings.Split(string(b), "\n") {
		ln = strings.TrimSpace(ln)
		if len(ln) > 2 && ln[0] == '[' && ln[len(ln)-1] == ']' {
			if s := ln[1 : len(ln)-1]; s != "global" {
				names = append(names, s)
			}
		}
	}
	return names
}

func parsePTP(out string, ifaces []string) *RunPTP {
	p := &RunPTP{}
	var cur *PTPPort
	byNum := map[int]*PTPPort{}
	atoi := func(s string) int { n, _ := strconv.Atoi(s); return n }
	toNS := func(s string) int64 { v, _ := strconv.ParseFloat(s, 64); return int64(v) }
	for _, ln := range strings.Split(out, "\n") {
		f := strings.Fields(ln)
		if len(f) < 2 {
			continue
		}
		switch f[0] {
		case "clockIdentity":
			if p.ClockID == "" {
				p.ClockID = f[1]
			}
		case "grandmasterIdentity":
			p.GM = f[1]
		case "priority1":
			p.Priority1 = atoi(f[1])
		case "clockClass":
			p.ClockClass = atoi(f[1])
		case "twoStepFlag":
			p.OneStep = f[1] == "0"
		case "stepsRemoved":
			p.StepsRem = atoi(f[1])
		case "offsetFromMaster":
			p.OffsetNS = toNS(f[1])
		case "portIdentity":
			num := 0
			if i := strings.LastIndexByte(f[1], '-'); i >= 0 {
				num = atoi(f[1][i+1:])
			}
			pp := &PTPPort{Name: fmt.Sprintf("порт %d", num)}
			if num >= 1 && num <= len(ifaces) {
				pp.Name = ifaces[num-1]
			}
			byNum[num] = pp
			cur = pp
		case "portState":
			if cur != nil {
				cur.State = f[1]
			}
		case "peerMeanPathDelay":
			if cur != nil {
				cur.PeerDelayNS = toNS(f[1])
			}
		}
	}
	if p.ClockID == "" && len(byNum) == 0 {
		return nil
	}
	nums := make([]int, 0, len(byNum))
	for n := range byNum {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	for _, n := range nums {
		p.Ports = append(p.Ports, *byNum[n])
	}
	p.GMLocal = p.GM != "" && p.GM == p.ClockID
	return p
}

func readVLANs(r Runner, run *Running, bridge string) error {
	out, err := r.Output("bridge", "-j", "vlan", "show")
	if err != nil {
		return err
	}
	var vl []struct {
		Ifname string `json:"ifname"`
		VLANs  []struct {
			VLAN    int      `json:"vlan"`
			VLANEnd int      `json:"vlanEnd"`
			Flags   []string `json:"flags"`
		} `json:"vlans"`
	}
	if err := json.Unmarshal(out, &vl); err != nil {
		return fmt.Errorf("bridge vlan json: %w", err)
	}
	for _, e := range vl {
		var list []RunVLAN
		for _, v := range e.VLANs {
			end := v.VLANEnd
			if end == 0 {
				end = v.VLAN
			}
			for id := v.VLAN; id <= end; id++ {
				list = append(list, RunVLAN{ID: id, PVID: slices.Contains(v.Flags, "PVID"), Untagged: slices.Contains(v.Flags, "Egress Untagged")})
			}
		}
		if e.Ifname == bridge {
			run.BridgeVLANs = list
		} else if p := run.Ports[e.Ifname]; p != nil {
			p.VLANs = list
		}
	}
	return nil
}

type fdbJSON struct {
	MAC    string   `json:"mac"`
	Ifname string   `json:"ifname"`
	VLAN   int      `json:"vlan"`
	Flags  []string `json:"flags"`
	Master string   `json:"master"`
	State  string   `json:"state"`
}

func readFDBRaw(r Runner, bridge string) ([]fdbJSON, error) {
	out, err := r.Output("bridge", "-j", "fdb", "show", "br", bridge)
	if err != nil {
		return nil, err
	}
	var e []fdbJSON
	if err := json.Unmarshal(out, &e); err != nil {
		return nil, fmt.Errorf("bridge fdb json: %w", err)
	}
	return e, nil
}

func readFDB(r Runner, run *Running, bridge string) error {
	ents, err := readFDBRaw(r, bridge)
	if err != nil {
		return err
	}
	for _, e := range ents {
		mac := strings.ToLower(e.MAC)
		if e.Ifname == bridge && mac == run.BridgeMAC && e.Master == bridge {
			run.LocalFDB[e.VLAN] = true
		}
		if e.State == "static" && e.Master == bridge && run.Ports[e.Ifname] != nil {
			run.StaticFDB = append(run.StaticFDB, FDBEntry{MAC: mac, Port: e.Ifname, VLAN: e.VLAN})
		}
	}
	return nil
}

func readLink(r Runner, port string) LinkInfo {
	li := LinkInfo{SQI: -1, SQIMax: -1, Supported: []string{}, Advertised: []string{}, Partner: []string{}}
	out, err := r.Output("ethtool", "--json", port)
	if err != nil {
		return li
	}
	var e []struct {
		Speed      json.RawMessage `json:"speed"`
		Duplex     string          `json:"duplex"`
		Autoneg    bool            `json:"auto-negotiation"`
		Link       bool            `json:"link-detected"`
		Supported  []string        `json:"supported-link-modes"`
		Advertised []string        `json:"advertised-link-modes"`
		Partner    []string        `json:"link-partner-advertised-link-modes"`
		SQI        *int            `json:"sqi"`
		SQIMax     *int            `json:"sqi-max"`
		PauseSup   string          `json:"supported-pause-frame-use"`
		PauseAdv   string          `json:"advertised-pause-frame-use"`
	}
	if json.Unmarshal(out, &e) != nil || len(e) == 0 {
		return li
	}
	x := e[0]
	li.Up, li.Autoneg = x.Link, x.Autoneg
	li.Supported, li.Advertised, li.Partner = nonNil(x.Supported), nonNil(x.Advertised), nonNil(x.Partner)
	li.Speed, _ = strconv.Atoi(string(x.Speed))
	if li.Up {
		li.Duplex = strings.ToLower(x.Duplex)
	}
	if !li.Up {
		li.Speed = 0
	}
	if x.SQI != nil {
		li.SQI = *x.SQI
	}
	if x.SQIMax != nil {
		li.SQIMax = *x.SQIMax
	}
	li.PauseSupported, li.PauseAdvertised = x.PauseSup, x.PauseAdv
	if out, err := r.Output("ethtool", "--json", "-a", port); err == nil {
		var a []struct {
			Rx, Tx     bool
			Negotiated *struct{ Rx, Tx bool }
		}
		if json.Unmarshal(out, &a) == nil && len(a) > 0 {
			li.PauseRx, li.PauseTx = a[0].Rx, a[0].Tx
			if n := a[0].Negotiated; n != nil && li.Up {
				li.FlowControl = n.Rx || n.Tx
			}
		}
	}
	return li
}

// readMirror reconstructs the mirror session from tc matchall/mirred filters.
func readMirror(r Runner, run *Running) *Mirror {
	m := &Mirror{}
	dirs := map[string]string{}
	for _, name := range sortedKeys(run.Ports) {
		for _, hook := range []string{"ingress", "egress"} {
			out, err := r.Output("tc", "-j", "filter", "show", "dev", name, hook)
			if err != nil {
				continue
			}
			var fl []struct {
				Kind    string `json:"kind"`
				Options struct {
					Actions []struct {
						Kind  string `json:"kind"`
						ToDev string `json:"to_dev"`
					} `json:"actions"`
				} `json:"options"`
			}
			if json.Unmarshal(out, &fl) != nil {
				continue
			}
			for _, f := range fl {
				for _, a := range f.Options.Actions {
					if f.Kind == "matchall" && a.Kind == "mirred" && a.ToDev != "" {
						m.Dest = a.ToDev
						d := map[string]string{"ingress": "rx", "egress": "tx"}[hook]
						if dirs[name] != "" && dirs[name] != d {
							d = "both"
						}
						dirs[name] = d
					}
				}
			}
		}
	}
	if len(dirs) == 0 {
		return nil
	}
	for _, n := range sortedKeys(dirs) {
		m.Sources = append(m.Sources, MirrorSource{Port: n, Direction: dirs[n]})
	}
	return m
}

func sortedKeys[V any](m map[string]V) []string {
	k := make([]string, 0, len(m))
	for n := range m {
		k = append(k, n)
	}
	sort.Strings(k)
	return k
}

// ReadCounters parses `ethtool -S` (switch MIB counters, plain text only).
func ReadCounters(r Runner, port string) (map[string]uint64, error) {
	out, err := r.Output("ethtool", "-S", port)
	if err != nil {
		return nil, err
	}
	c := map[string]uint64{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), ":")
		if !ok {
			continue
		}
		if n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64); err == nil {
			c[strings.TrimSpace(k)] = n
		}
	}
	return c, nil
}

// ImportConfig builds a config describing what is running now.
func ImportConfig(run *Running) *Config {
	ports := sortedKeys(run.Ports)
	c := DefaultConfig(nil)
	b := run.Bridge
	c.Bridge.VLANAware = b.VLANAware
	c.Bridge.AgeingTime = b.AgeingTime
	c.Bridge.STP = STP{Enabled: b.STP, Priority: b.Priority, HelloTime: b.HelloTime, ForwardDelay: b.ForwardDelay, MaxAge: b.MaxAge}
	c.Bridge.IGMP = IGMP{Snooping: b.Snooping, Querier: b.Querier}
	vids := map[int]bool{1: true}
	for _, v := range run.BridgeVLANs {
		if v.PVID {
			c.Bridge.MgmtVLAN = v.ID
		}
	}
	for _, n := range ports {
		rp := run.Ports[n]
		speed, an := rp.Link.SpeedSetting()
		if speed == "unknown" || speed == "custom" {
			speed, an = "auto", true
		}
		p := &Port{Enabled: rp.Up, Description: rp.Alias, Speed: speed, Autoneg: an, Learning: rp.Learning,
			Isolated: rp.Isolated, STPPriority: rp.Priority, Tagged: []int{}, FlowControl: rp.Link.PauseOn()}
		if p.STPPriority < 0 {
			p.STPPriority = 32
		}
		for _, v := range rp.VLANs {
			vids[v.ID] = true
			if v.PVID && v.Untagged {
				p.PVID = v.ID
			} else if !v.Untagged {
				p.Tagged = append(p.Tagged, v.ID)
			}
		}
		if !b.VLANAware && p.PVID == 0 {
			p.PVID = 1
		}
		c.Ports[n] = p
	}
	c.VLANs = nil
	for _, id := range sortedInts(vids) {
		name := ""
		if id == 1 {
			name = "default"
		}
		c.VLANs = append(c.VLANs, VLAN{ID: id, Name: name})
	}
	c.StaticFDB = append([]FDBEntry{}, run.StaticFDB...)
	c.Mirror = run.Mirror
	if run.MRP != nil {
		m := run.MRP.MRP
		c.MRP = &m
	}
	c.Normalize()
	return c
}

func sortedInts(m map[int]bool) []int {
	r := make([]int, 0, len(m))
	for k := range m {
		r = append(r, k)
	}
	sort.Ints(r)
	return r
}
