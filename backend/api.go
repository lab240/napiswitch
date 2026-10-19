package main

import (
	"bytes"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

const apiPrefix = "/api/v1"

// web holds the management UI, served without auth (it asks for the token
// itself and sends it with every API call).
//
//go:embed web
var web embed.FS

type API struct {
	m       *Manager
	token   string
	version string
	rmii    *rmiiFixer // nil with -rmii-fix=false
}

func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	h := func(pattern string, f func(*http.Request) (any, error)) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			v, err := f(r)
			if err != nil {
				writeErr(w, err, v)
				return
			}
			writeJSON(w, http.StatusOK, v)
		})
	}
	h("GET "+apiPrefix+"/health", func(*http.Request) (any, error) { return map[string]string{"status": "ok", "version": a.version}, nil })
	h("GET "+apiPrefix+"/system", a.system)
	h("GET "+apiPrefix+"/capabilities", a.capabilities)
	h("GET "+apiPrefix+"/events", func(*http.Request) (any, error) { return a.m.Events(), nil })

	h("GET "+apiPrefix+"/ports", a.ports)
	h("GET "+apiPrefix+"/ports/{name}", a.port)
	h("GET "+apiPrefix+"/ports/{name}/counters", a.counters)
	h("POST "+apiPrefix+"/ports/{name}/counters/clear", a.clearCounters)
	h("GET "+apiPrefix+"/vlans", a.vlans)
	h("GET "+apiPrefix+"/fdb", a.fdb)
	h("DELETE "+apiPrefix+"/fdb", a.flushFDB)
	h("GET "+apiPrefix+"/stp", a.stp)
	h("GET "+apiPrefix+"/multicast", a.multicast)
	h("GET "+apiPrefix+"/mirror", a.mirror)
	h("GET "+apiPrefix+"/mrp", a.mrp)
	h("GET "+apiPrefix+"/network", a.network)

	h("GET "+apiPrefix+"/config", func(*http.Request) (any, error) { return a.m.View(), nil })
	h("PUT "+apiPrefix+"/config", func(r *http.Request) (any, error) { return a.change(r, false) })
	h("PATCH "+apiPrefix+"/config", func(r *http.Request) (any, error) { return a.change(r, true) })
	h("POST "+apiPrefix+"/config/preview", a.preview)
	h("POST "+apiPrefix+"/config/confirm", func(*http.Request) (any, error) { return a.m.Confirm() })
	h("POST "+apiPrefix+"/config/rollback", func(*http.Request) (any, error) {
		return map[string]string{"status": "rolled back"}, a.m.Rollback()
	})
	h("POST "+apiPrefix+"/config/save", func(*http.Request) (any, error) {
		f, err := a.m.Save()
		return map[string]any{"saved_files": f}, err
	})
	ui, _ := fs.Sub(web, "web")
	mux.Handle("GET /", http.FileServerFS(ui))
	return a.auth(mux)
}

func (a *API) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.token != "" && strings.HasPrefix(r.URL.Path, apiPrefix+"/") && r.URL.Path != apiPrefix+"/health" {
			got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(a.token)) != 1 {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing or wrong bearer token"})
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

type httpError struct {
	code int
	msg  string
}

func (e *httpError) Error() string { return e.msg }

func notFound(f string, a ...any) error { return &httpError{404, fmt.Sprintf(f, a...)} }
func badReq(f string, a ...any) error   { return &httpError{400, fmt.Sprintf(f, a...)} }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, err error, partial any) {
	body := map[string]any{"error": err.Error()}
	code := http.StatusInternalServerError
	var he *httpError
	var ve *ValidationError
	var ae *ApplyError
	switch {
	case errors.As(err, &he):
		code = he.code
	case errors.As(err, &ve):
		code = http.StatusUnprocessableEntity
		body["problems"] = ve.Problems
	case errors.As(err, &ae):
		code = http.StatusConflict
		body["apply_error"] = ae
	case errors.Is(err, ErrPending), errors.Is(err, ErrNoPending):
		code = http.StatusConflict
	}
	if res, ok := partial.(*ApplyResult); ok && res != nil {
		body["warnings"] = nonNil(res.Warnings)
		if len(res.Commands) > 0 {
			body["commands"] = res.Commands
		}
	}
	writeJSON(w, code, body)
}

func readBody(r *http.Request) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return nil, badReq("read body: %v", err)
	}
	return b, nil
}

// mergePatch applies an RFC 7396 JSON merge patch.
func mergePatch(doc, patch any) any {
	pm, ok := patch.(map[string]any)
	if !ok {
		return patch
	}
	dm, ok := doc.(map[string]any)
	if !ok {
		dm = map[string]any{}
	}
	for k, v := range pm {
		if v == nil {
			delete(dm, k)
		} else {
			dm[k] = mergePatch(dm[k], v)
		}
	}
	return dm
}

func decodeStrict(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return badReq("config json: %v", err)
	}
	return nil
}

// candidate builds the new config from the request body: a full config
// (PUT) or a merge patch over the applied config (PATCH).
func (a *API) candidate(body []byte, patch bool) (*Config, error) {
	var c Config
	if !patch {
		return &c, decodeStrict(body, &c)
	}
	var p any
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, badReq("patch json: %v", err)
	}
	cur, _ := json.Marshal(a.m.Applied())
	var doc any
	_ = json.Unmarshal(cur, &doc)
	merged, _ := json.Marshal(mergePatch(doc, p))
	return &c, decodeStrict(merged, &c)
}

func (a *API) change(r *http.Request, patch bool) (any, error) {
	body, err := readBody(r)
	if err != nil {
		return nil, err
	}
	c, err := a.candidate(body, patch)
	if err != nil {
		return nil, err
	}
	q := r.URL.Query()
	confirm := 0
	if s := q.Get("confirm"); s != "" {
		if confirm, err = strconv.Atoi(s); err != nil || confirm < 0 || confirm > 3600 {
			return nil, badReq("confirm: seconds 0..3600")
		}
	}
	save := q.Get("save") == "1" || q.Get("save") == "true"
	res, err := a.m.Apply(c, time.Duration(confirm)*time.Second, save)
	if err != nil {
		return res, err
	}
	return res, nil
}

func (a *API) preview(r *http.Request) (any, error) {
	body, err := readBody(r)
	if err != nil {
		return nil, err
	}
	c, err := a.candidate(body, r.URL.Query().Get("mode") != "replace")
	if err != nil {
		return nil, err
	}
	res, err := a.m.Preview(c)
	return res, err
}

// --- status views ---

type PortView struct {
	Name        string    `json:"name"`
	HWPort      string    `json:"hw_port"`
	Description string    `json:"description"`
	Enabled     bool      `json:"enabled"`
	MAC         string    `json:"mac"`
	Link        LinkInfo  `json:"link"`
	Speed       string    `json:"speed"`
	Autoneg     bool      `json:"autoneg"`
	Bridged     bool      `json:"bridged"`
	PVID        int       `json:"pvid"`
	Tagged      []int     `json:"tagged"`
	VLANs       []RunVLAN `json:"vlans_running"`
	STPState    string    `json:"stp_state"`
	STPCost     int       `json:"stp_cost"`
	STPPriority int       `json:"stp_priority"`
	Learning    bool      `json:"learning"`
	Isolated    bool      `json:"isolated"`
}

func (a *API) portViews() ([]PortView, *Running, error) {
	_, run, err := a.m.Ports()
	if err != nil {
		return nil, nil, err
	}
	cfg := a.m.Applied()
	var out []PortView
	for _, n := range sortedKeys(run.Ports) {
		rp := run.Ports[n]
		speed, an := rp.Link.SpeedSetting()
		v := PortView{Name: n, HWPort: rp.HWPort, Description: rp.Alias, Enabled: rp.Up, MAC: rp.MAC, Link: rp.Link,
			Speed: speed, Autoneg: an, Bridged: rp.Master == a.m.bridge, VLANs: rp.VLANs, STPState: rp.STPState,
			STPCost: rp.Cost, STPPriority: rp.Priority, Learning: rp.Learning, Isolated: rp.Isolated, Tagged: []int{}}
		if p := cfg.Ports[n]; p != nil {
			v.PVID, v.Tagged = p.PVID, p.Tagged
		}
		out = append(out, v)
	}
	return out, run, nil
}

func (a *API) ports(*http.Request) (any, error) {
	v, _, err := a.portViews()
	return v, err
}

func (a *API) port(r *http.Request) (any, error) {
	v, _, err := a.portViews()
	if err != nil {
		return nil, err
	}
	n := r.PathValue("name")
	if i := slices.IndexFunc(v, func(p PortView) bool { return p.Name == n }); i >= 0 {
		return v[i], nil
	}
	return nil, notFound("no such port %q", n)
}

func (a *API) checkPort(n string) error {
	ports, _, err := a.m.Ports()
	if err != nil {
		return err
	}
	if !slices.Contains(ports, n) {
		return notFound("no such port %q", n)
	}
	return nil
}

func (a *API) counters(r *http.Request) (any, error) {
	n := r.PathValue("name")
	if err := a.checkPort(n); err != nil {
		return nil, err
	}
	c, err := a.m.Counters(n)
	if err != nil {
		return nil, err
	}
	sum := func(keys ...string) uint64 {
		var s uint64
		for _, k := range keys {
			s += c[k]
		}
		return s
	}
	return map[string]any{
		"port": n,
		"summary": map[string]uint64{
			"rx_bytes": c["rx_total"], "tx_bytes": c["tx_total"],
			"rx_unicast": c["rx_ucast"], "rx_multicast": c["rx_mcast"], "rx_broadcast": c["rx_bcast"],
			"tx_unicast": c["tx_ucast"], "tx_multicast": c["tx_mcast"], "tx_broadcast": c["tx_bcast"],
			"rx_errors":   sum("rx_crc_err", "rx_align_err", "rx_symbol_err", "rx_undersize", "rx_oversize", "rx_fragments", "rx_jabbers"),
			"tx_errors":   sum("tx_late_col", "tx_exc_col"),
			"collisions":  sum("tx_single_col", "tx_mult_col", "tx_late_col", "tx_exc_col", "tx_total_col"),
			"rx_discards": c["rx_discards"], "tx_discards": c["tx_discards"],
			"rx_pause": c["rx_pause"], "tx_pause": c["tx_pause"],
		},
		"mib": c,
	}, nil
}

func (a *API) clearCounters(r *http.Request) (any, error) {
	n := r.PathValue("name")
	if err := a.checkPort(n); err != nil {
		return nil, err
	}
	return map[string]string{"status": "cleared"}, a.m.ClearCounters(n)
}

func (a *API) vlans(*http.Request) (any, error) {
	_, run, err := a.m.Ports()
	if err != nil {
		return nil, err
	}
	cfg := a.m.Applied()
	type member struct {
		Port     string `json:"port"`
		Untagged bool   `json:"untagged"`
		PVID     bool   `json:"pvid"`
	}
	type vv struct {
		ID      int      `json:"id"`
		Name    string   `json:"name"`
		Members []member `json:"members"`
		Mgmt    bool     `json:"mgmt"`
	}
	var out []vv
	for _, v := range cfg.VLANs {
		e := vv{ID: v.ID, Name: v.Name, Members: []member{}, Mgmt: cfg.Bridge.VLANAware && v.ID == cfg.Bridge.MgmtVLAN}
		for _, n := range sortedKeys(run.Ports) {
			for _, rv := range run.Ports[n].VLANs {
				if rv.ID == v.ID {
					e.Members = append(e.Members, member{n, rv.Untagged, rv.PVID})
				}
			}
		}
		out = append(out, e)
	}
	return map[string]any{"vlan_aware": run.Bridge.VLANAware, "vlans": out}, nil
}

func (a *API) fdb(r *http.Request) (any, error) {
	ents, err := readFDBRaw(a.m.r, a.m.bridge)
	if err != nil {
		return nil, err
	}
	_, run, err := a.m.Ports()
	if err != nil {
		return nil, err
	}
	return map[string]any{"ageing_time": run.Bridge.AgeingTime,
		"entries": mergeFDB(ents, run, a.m.bridge, r.URL.Query())}, nil
}

type fdbEntryView struct {
	MAC  string `json:"mac"`
	Port string `json:"port"`
	VLAN int    `json:"vlan"`
	Type string `json:"type"`      // dynamic, static, local
	HW   bool   `json:"offloaded"` // present in the switch chip's table
}

// mergeFDB combines the bridge's software FDB (entries with master br0)
// with the switch chip's own table (entries flagged "self" on the ports,
// from the driver's fdb_dump). Traffic switched port to port never reaches
// the CPU, so those MACs exist only in the chip table.
func mergeFDB(ents []fdbJSON, run *Running, bridge string, q url.Values) []fdbEntryView {
	out := []fdbEntryView{}
	idx := map[string]int{}
	for _, e := range ents {
		if run.Ports[e.Ifname] == nil {
			continue // bridge's own and CPU-port entries
		}
		self := slices.Contains(e.Flags, "self") && e.Master == ""
		if !self && e.Master != bridge {
			continue
		}
		t := "dynamic"
		switch e.State {
		case "static":
			t = "static"
		case "permanent":
			t = "local" // the port's own address
		}
		vlan := e.VLAN
		if !run.Bridge.VLANAware {
			vlan = 0 // the chip reports its default FID as VLAN 1
		}
		x := fdbEntryView{strings.ToLower(e.MAC), e.Ifname, vlan, t, self || slices.Contains(e.Flags, "offload")}
		key := fmt.Sprintf("%s/%s/%d", x.MAC, x.Port, x.VLAN)
		if i, ok := idx[key]; ok {
			out[i].HW = out[i].HW || x.HW
			if out[i].Type == "dynamic" {
				out[i].Type = x.Type
			}
			continue
		}
		if p := q.Get("port"); p != "" && p != x.Port {
			continue
		}
		if v := q.Get("vlan"); v != "" && v != strconv.Itoa(x.VLAN) {
			continue
		}
		if t := q.Get("type"); t != "" && t != x.Type {
			continue
		}
		idx[key] = len(out)
		out = append(out, x)
	}
	return out
}

func (a *API) flushFDB(r *http.Request) (any, error) {
	ports, _, err := a.m.Ports()
	if err != nil {
		return nil, err
	}
	targets := ports
	if p := r.URL.Query().Get("port"); p != "" {
		if !slices.Contains(ports, p) {
			return nil, notFound("no such port %q", p)
		}
		targets = []string{p}
	}
	for _, p := range targets {
		// "bridge link set ... flush" is not accepted by iproute2 on the board.
		if err := a.m.r.Exec(cmd("ip", "link", "set", "dev", p, "type", "bridge_slave", "fdb_flush")); err != nil {
			return nil, err
		}
	}
	return map[string]any{"flushed": targets}, nil
}

func (a *API) stp(*http.Request) (any, error) {
	_, run, err := a.m.Ports()
	if err != nil {
		return nil, err
	}
	rootPort := ""
	for _, p := range run.Ports {
		if run.Bridge.RootPort != 0 && p.Index != 0 {
			// root_port is the bridge port number (1-based "no"), resolve via sysfs
			if b, err := os.ReadFile("/sys/class/net/" + p.Name + "/brport/port_no"); err == nil {
				if n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 0, 32); err == nil && int(n) == run.Bridge.RootPort {
					rootPort = p.Name
				}
			}
		}
	}
	type sp struct {
		Port     string `json:"port"`
		State    string `json:"state"`
		Cost     int    `json:"cost"`
		Priority int    `json:"priority"`
	}
	var ps []sp
	for _, n := range sortedKeys(run.Ports) {
		p := run.Ports[n]
		if p.Master == a.m.bridge {
			ps = append(ps, sp{n, p.STPState, p.Cost, p.Priority})
		}
	}
	b := run.Bridge
	return map[string]any{
		"enabled": b.STP, "protocol": "stp", "bridge_id": b.BridgeID, "root_id": b.RootID,
		"is_root": b.BridgeID == b.RootID, "root_port": rootPort, "root_path_cost": b.RootPathCost,
		"priority": b.Priority, "hello_time": b.HelloTime, "forward_delay": b.ForwardDelay, "max_age": b.MaxAge,
		"topology_change": b.TopologyChange, "ports": ps,
	}, nil
}

func (a *API) multicast(*http.Request) (any, error) {
	out, err := a.m.r.Output("bridge", "-j", "-d", "mdb", "show", "dev", a.m.bridge)
	if err != nil {
		return nil, err
	}
	var mdb any
	if err := json.Unmarshal(out, &mdb); err != nil {
		return nil, err
	}
	_, run, err := a.m.Ports()
	if err != nil {
		return nil, err
	}
	return map[string]any{"snooping": run.Bridge.Snooping, "querier": run.Bridge.Querier, "mdb": mdb}, nil
}

func (a *API) mirror(*http.Request) (any, error) {
	_, run, err := a.m.Ports()
	if err != nil {
		return nil, err
	}
	return map[string]any{"running": run.Mirror, "sessions_max": 1}, nil
}

// mrp reports the ring as mrp_server sees it; available is false when
// mrp_server does not answer (not installed, or a kernel without MRP).
func (a *API) mrp(*http.Request) (any, error) {
	_, run, err := a.m.Ports()
	if err != nil {
		return nil, err
	}
	return map[string]any{"available": run.MRPAvail, "running": run.MRP, "profiles": mrpProfiles}, nil
}

// network reports the L3 state: WAN candidates with their addresses, the
// bridge (LAN), DHCP leases, and a suggested config for taking over from netplan.
func (a *API) network(*http.Request) (any, error) {
	_, run, err := a.m.Ports()
	if err != nil {
		return nil, err
	}
	cfg := a.m.Applied()
	n := cfg.Network
	st := readNetState(a.m.r, a.m.paths, a.m.webPort)
	ifs := []IfStatus{}
	leases := []Lease{}
	wan := ""
	for _, e := range run.Ethers {
		s, _ := readIfStatus(a.m.r, e)
		if n != nil && n.WAN.Interface == e {
			s.Role = "wan"
		}
		if wan == "" && s.Gateway != "" {
			wan = e // the NIC with the default route is the natural WAN
		}
		ifs = append(ifs, s)
	}
	if wan == "" && len(run.Ethers) > 0 {
		wan = run.Ethers[0]
	}
	s, l := readIfStatus(a.m.r, a.m.bridge)
	s.Role = "lan"
	ifs = append(ifs, s)
	leases = append(leases, l...)
	mode := "unmanaged"
	if n != nil {
		mode = n.Mode
	}
	return map[string]any{
		"managed": n != nil, "mode": mode, "interfaces": ifs, "wan_candidates": nonNil(run.Ethers),
		"dhcp_leases": leases, "nft_available": st.NFT, "firewall_active": st.FWLoaded != "",
		"web_port": a.m.webPort, "suggested": DefaultNetwork(wan),
	}, nil
}

func (a *API) system(*http.Request) (any, error) {
	cfg := a.m.Applied()
	host, _ := os.Hostname()
	kern, _ := os.ReadFile("/proc/sys/kernel/osrelease")
	up, _ := os.ReadFile("/proc/uptime")
	upS, _ := strconv.ParseFloat(strings.Fields(string(up) + " 0")[0], 64)
	temps := []float64{}
	for i := 0; ; i++ {
		b, err := os.ReadFile(fmt.Sprintf("/sys/class/thermal/thermal_zone%d/temp", i))
		if err != nil {
			break
		}
		t, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		temps = append(temps, float64(t)/1000)
	}
	addrs := map[string][]string{}
	if out, err := a.m.r.Output("ip", "-j", "addr", "show"); err == nil {
		var al []struct {
			Ifname   string `json:"ifname"`
			AddrInfo []struct {
				Local     string `json:"local"`
				Prefixlen int    `json:"prefixlen"`
				Scope     string `json:"scope"`
			} `json:"addr_info"`
		}
		_ = json.Unmarshal(out, &al)
		for _, l := range al {
			for _, x := range l.AddrInfo {
				if x.Scope == "global" {
					addrs[l.Ifname] = append(addrs[l.Ifname], fmt.Sprintf("%s/%d", x.Local, x.Prefixlen))
				}
			}
		}
	}
	_, run, err := a.m.Ports()
	if err != nil {
		return nil, err
	}
	// Health of the SoC <-> switch RMII link (CPU port).
	cpu := map[string]any{"interface": run.Conduit}
	if run.Conduit != "" {
		if c, err := ReadCounters(a.m.r, run.Conduit); err == nil {
			cpu["rx_crc_errors"] = c["mmc_rx_crc_error"]
			cpu["switch_rx_crc_errors"] = c["s00_p05_rx_crc_err"]
			cpu["switch_rx_align_errors"] = c["s00_p05_rx_align_err"]
		}
	}
	if a.rmii != nil {
		cpu["rmii"] = a.rmii.Status()
	}
	return map[string]any{
		"name": cfg.System.Name, "location": cfg.System.Location, "contact": cfg.System.Contact,
		"hostname": host, "model": "NAPI-Switch (Microchip KSZ9567)", "kernel": strings.TrimSpace(string(kern)),
		"daemon_version": a.version, "uptime_s": int(upS), "temperatures_c": temps,
		"bridge": a.m.bridge, "bridge_mac": run.BridgeMAC, "addresses": addrs, "cpu_port": cpu,
	}, nil
}

func (a *API) capabilities(*http.Request) (any, error) {
	ports, run, err := a.m.Ports()
	if err != nil {
		return nil, err
	}
	_, nftErr := a.m.r.Output("nft", "--version")
	nft := nftErr == nil
	pc := map[string]any{}
	for _, n := range ports {
		pc[n] = map[string]any{"speeds": speedNames(supportedSpeedModes(run.Ports[n].Link.Supported)),
			"copper": len(supportedSpeedModes(run.Ports[n].Link.Supported)) > 0}
	}
	return map[string]any{
		"switch":      "KSZ9567",
		"ports":       pc,
		"vlan":        map[string]any{"ids": "1-4094", "shared_learning_mod": 128, "qinq": false},
		"fdb":         map[string]any{"dynamic_max": 4096, "static": true},
		"stp":         map[string]any{"protocols": []string{"stp"}, "rstp": false, "mstp": false},
		"igmp":        map[string]any{"snooping": true, "querier": true},
		"mirror":      map[string]any{"sessions": 1, "directions": []string{"rx", "tx", "both"}},
		"mrp":         map[string]any{"available": run.MRPAvail, "rings": 1, "roles": []string{"mrm", "mrc"}, "profiles": mrpProfiles, "offload": false},
		"isolation":   true,
		"learning":    true,
		"jumbo":       false, // MTU change re-opens the CPU conduit, unsafe on this board
		"lag":         false,
		"rate_limit":  false,
		"qos":         map[string]any{"queues": 4, "planned": true},
		"cable_test":  false,
		"router":      map[string]any{"nat": true, "dhcp_server": true, "firewall": nft, "port_forwarding": nft},
		"apply_modes": []string{"immediate", "confirm"},
	}, nil
}

// speedNames maps ethtool link modes to config speed values, fastest first.
func speedNames(modes []string) []string {
	r := []string{}
	for _, n := range []string{"1000full", "100full", "100half", "10full", "10half"} {
		if slices.Contains(modes, speedModes[n].mode) {
			r = append(r, n)
		}
	}
	return r
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}
