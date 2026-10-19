// napi-switchd: management backend for the NAPI-Switch (KSZ9567 under Linux
// DSA). It exposes the switch as a REST API for a web UI: ports, VLANs,
// FDB, STP, IGMP snooping and port mirroring, with commit-confirm apply
// and persistence through systemd-networkd drop-ins.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

var version = "0.1.0"

func loadToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		return strings.TrimSpace(string(b)), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	t := hex.EncodeToString(buf)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	return t, os.WriteFile(path, []byte(t+"\n"), 0o600)
}

func main() {
	listen := flag.String("listen", ":8080", "HTTP listen address")
	cfgPath := flag.String("config", "/etc/napi-switch/config.json", "saved config")
	tokenFile := flag.String("token-file", "/etc/napi-switch/token", "bearer token file (created if missing), empty = no auth")
	bridge := flag.String("bridge", "br0", "bridge device")
	netDir := flag.String("netdir", "/etc/systemd/network", "networkd drop-in directory")
	runNetDir := flag.String("run-netdir", "/run/systemd/network", "runtime networkd directory (applied L3 config)")
	fwFile := flag.String("fw-file", "/run/napi-switch/firewall.nft", "nft script of the loaded firewall")
	dry := flag.Bool("dry-run", false, "log commands instead of executing them")
	rmiiFix := flag.Bool("rmii-fix", true, "TEMPORARY board-bug workaround: switch the KSZ CPU port to the SoC's RMII clock (no-op once R29 is reworked)")
	kszMod := flag.String("kszreg-module", "/usr/lib/napi-switch/kszreg-%s.ko", "one-shot register module (%s = kernel release)")
	mrp := flag.String("mrp", mrpCLI, "mrp_server client (Microchip mrp); MRP is unavailable without it")
	healthMode := flag.String("health", "reboot", "health watchdog: reboot (reboot the board when the switch hangs), log, off")
	healthState := flag.String("health-state", "/var/lib/napi-switch/health-reboot", "records the last health reboot")
	flag.Parse()
	mrpCLI = *mrp

	l := log.New(os.Stderr, "", 0)
	token := ""
	if *tokenFile != "" {
		var err error
		if token, err = loadToken(*tokenFile); err != nil {
			l.Fatalf("token: %v", err)
		}
	}
	paths := Paths{Config: *cfgPath, NetDir: *netDir,
		NetSearch: []string{*netDir, "/run/systemd/network", "/usr/lib/systemd/network"},
		RunNetDir: *runNetDir, FWFile: *fwFile}
	webPort := 0
	if _, p, err := net.SplitHostPort(*listen); err == nil {
		webPort, _ = strconv.Atoi(p)
	}
	m := NewManager(&sysRunner{log: l, dry: *dry}, l, *bridge, paths, webPort)
	if err := m.Start(90 * time.Second); err != nil {
		l.Fatalf("start: %v", err)
	}
	api := &API{m: m, token: token, version: version}
	go m.WatchMRP(time.Second)
	if *healthMode != "off" {
		h := &health{r: m.r, log: l, bridge: *bridge, netSys: "/sys/class/net", regmaps: "/sys/kernel/debug/regmap",
			conduit: func() string {
				if _, run, err := m.Ports(); err == nil {
					return run.Conduit
				}
				return ""
			},
			ringClosed: func() bool {
				r, ok := readMRP(m.r, *bridge)
				return ok && r != nil && r.State == "CHK_RC"
			},
			hangFile: "/run/napi-switch/health-hang",
			interval: 5 * time.Second, hangAfter: 25 * time.Second, grace: 30 * time.Second, failMax: 3,
			stateFile: *healthState, rebootBackoff: 15 * time.Minute,
			reboot: sysrqReboot, notify: sdNotify}
		h.logOnly = *healthMode == "log" || *dry
		go h.Run()
		api.health = h
	}
	if *rmiiFix {
		f := &rmiiFixer{r: m.r, log: l, regmaps: "/sys/kernel/debug/regmap", devtree: "/sys/class/net", module: *kszMod,
			conduit: func() string {
				if _, run, err := m.Ports(); err == nil {
					return run.Conduit
				}
				return ""
			}}
		st := f.Check()
		l.Printf("rmii: clock=%s xmii_ctrl1=%s %s", st.Clock, st.Reg, st.Error)
		go f.Watch(time.Minute)
		api.rmii = f
	}
	srv := &http.Server{Addr: *listen, Handler: api.Handler(), ReadHeaderTimeout: 10 * time.Second}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		l.Fatalf("listen: %v", err)
	}
	l.Printf("napi-switchd %s listening on %s", version, *listen)
	_ = sdNotify("READY=1")
	l.Fatal(srv.Serve(ln))
}
