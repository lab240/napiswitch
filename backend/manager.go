package main

import (
	"errors"
	"fmt"
	"log"
	"net/netip"
	"os"
	"sync"
	"time"
)

var (
	ErrPending   = errors.New("a change is waiting for confirmation: confirm or roll back first")
	ErrNoPending = errors.New("no change is waiting for confirmation")
)

// ApplyError reports the failing command; the previous config was restored.
type ApplyError struct {
	Cmd      string `json:"command"`
	Err      string `json:"error"`
	Restored bool   `json:"restored"`
}

func (e *ApplyError) Error() string { return fmt.Sprintf("%s: %s", e.Cmd, e.Err) }

type Pending struct {
	Deadline time.Time `json:"deadline"`
	Save     bool      `json:"save"`
	prev     *Config
	timer    *time.Timer
}

// Manager serialises all configuration changes.
type Manager struct {
	mu      sync.Mutex
	r       Runner
	log     *log.Logger
	bridge  string
	paths   Paths
	webPort int
	applied *Config
	saved   *Config
	pending *Pending
	events  []Event

	cntMu   sync.Mutex
	cntBase map[string]map[string]uint64
}

type Event struct {
	Time time.Time `json:"time"`
	Msg  string    `json:"message"`
}

type ApplyResult struct {
	Commands []Cmd    `json:"commands"`
	Warnings []string `json:"warnings"`
	Pending  *Pending `json:"pending,omitempty"`
	Saved    []string `json:"saved_files,omitempty"`
	// Files maps each file a preview would write to its content.
	Files map[string]string `json:"files,omitempty"`
}

func NewManager(r Runner, l *log.Logger, bridge string, p Paths, webPort int) *Manager {
	return &Manager{r: r, log: l, bridge: bridge, paths: p, webPort: webPort, cntBase: map[string]map[string]uint64{}}
}

// state reads the running state including the L3 part.
func (m *Manager) state() (*Running, error) {
	run, err := ReadRunning(m.r, m.bridge)
	if err != nil {
		return nil, err
	}
	run.Net = readNetState(m.r, m.paths, m.webPort)
	return run, nil
}

func (m *Manager) opts(resync bool) PlanOpts {
	return PlanOpts{Bridge: m.bridge, ResyncVLANs: resync, RunNetDir: m.paths.RunNetDir, FWFile: m.paths.FWFile, CfgPath: m.paths.Config}
}

// validate checks c against the hardware and, for the L3 part, the live
// interfaces. prev is the config c replaces (nil at startup).
func (m *Manager) validate(c, prev *Config, run *Running, confirm time.Duration) ([]string, error) {
	w, err := c.Validate(sortedKeys(run.Ports))
	var problems []string
	if ve, ok := err.(*ValidationError); ok {
		problems = ve.Problems
	} else if err != nil {
		return w, err
	}
	if n := c.Network; n != nil {
		ctx := netCtx{Candidates: run.Ethers, NFT: run.Net != nil && run.Net.NFT, WebPort: m.webPort}
		if n.router() && n.WAN.Proto == "dhcp" && n.WAN.Interface != "" {
			st, _ := readIfStatus(m.r, n.WAN.Interface)
			for _, a := range st.Addresses {
				if p, err := netip.ParsePrefix(a); err == nil && p.Addr().Is4() {
					ctx.WANAddrs = append(ctx.WANAddrs, p)
				}
			}
		}
		for _, pn := range sortedKeys(run.Ports) {
			if p := run.Ports[pn]; p.Link.Up && p.Master == m.bridge {
				ctx.LinkPorts = append(ctx.LinkPorts, pn)
			}
		}
		nw, np := n.validate(ctx)
		w, problems = append(w, nw...), append(problems, np...)
	}
	if prev != nil {
		if prev.Network != nil && c.Network == nil {
			problems = append(problems, "network: cannot be handed back to netplan at runtime; use mode switch with dhcp")
		}
		if !jsonEqual(prev.Network, c.Network) && confirm == 0 {
			problems = append(problems, "network: address/WAN/router changes can cut access - apply them with confirm (e.g. ?confirm=60)")
		}
	}
	if len(problems) > 0 {
		return w, &ValidationError{problems}
	}
	return w, nil
}

func (m *Manager) event(f string, a ...any) {
	e := Event{time.Now(), fmt.Sprintf(f, a...)}
	m.log.Print(e.Msg)
	m.events = append(m.events, e)
	if len(m.events) > 200 {
		m.events = m.events[len(m.events)-200:]
	}
}

// Start waits for the bridge, then applies the saved config (full VLAN
// resync) or, without one, adopts what is running.
func (m *Manager) Start(wait time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var run *Running
	var err error
	for end := time.Now().Add(wait); ; time.Sleep(2 * time.Second) {
		run, err = m.state()
		if err == nil && len(run.Ports) > 0 {
			break
		}
		if time.Now().After(end) {
			return fmt.Errorf("switch not ready: %v", err)
		}
	}
	saved, err := LoadConfig(m.paths.Config)
	if errors.Is(err, os.ErrNotExist) {
		m.applied = ImportConfig(run)
		m.event("no saved config, adopted the running state")
		return nil
	}
	if err != nil {
		m.applied = ImportConfig(run)
		m.event("saved config unreadable (%v), adopted the running state", err)
		return nil
	}
	if _, verr := m.validate(saved, nil, run, 0); verr != nil {
		m.applied = ImportConfig(run)
		m.event("saved config rejected (%v), adopted the running state", verr)
		return nil
	}
	m.saved = saved
	cmds := Plan(saved, run, m.opts(saved.Bridge.VLANAware))
	if i, err := m.exec(cmds); err != nil {
		m.event("startup apply failed at %q: %v", cmds[i], err)
		m.applied = m.importNow()
		return nil
	}
	m.applied = saved.Clone()
	m.event("saved config applied (%d commands)", len(cmds))
	return nil
}

func (m *Manager) importNow() *Config {
	run, err := ReadRunning(m.r, m.bridge)
	if err != nil {
		return m.applied
	}
	return ImportConfig(run)
}

func (m *Manager) exec(cmds []Cmd) (int, error) {
	for i, c := range cmds {
		if err := m.r.Exec(c); err != nil {
			return i, err
		}
	}
	return -1, nil
}

func (m *Manager) Ports() ([]string, *Running, error) {
	run, err := ReadRunning(m.r, m.bridge)
	if err != nil {
		return nil, nil, err
	}
	return sortedKeys(run.Ports), run, nil
}

// Preview validates and plans without changing anything.
func (m *Manager) Preview(c *Config) (*ApplyResult, error) {
	c.Normalize()
	run, err := m.state()
	if err != nil {
		return nil, err
	}
	// confirm=1: preview does not know the confirm time the UI will use
	w, err := m.validate(c, m.Applied(), run, 1)
	if err != nil {
		return &ApplyResult{Warnings: w}, err
	}
	cmds := Plan(c, run, m.opts(false))
	res := &ApplyResult{Commands: cmds, Warnings: nonNil(w), Files: map[string]string{}}
	for _, x := range cmds {
		if x.File != "" && !x.Remove {
			res.Files[x.File] = x.Content
		}
	}
	return res, nil
}

// Apply switches the running config to c. With confirm > 0 the change is
// rolled back unless Confirm is called in time (save happens on confirm).
func (m *Manager) Apply(c *Config, confirm time.Duration, save bool) (*ApplyResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pending != nil {
		return nil, ErrPending
	}
	c.Normalize()
	run, err := m.state()
	if err != nil {
		return nil, err
	}
	prev := m.applied
	w, err := m.validate(c, prev, run, confirm)
	if err != nil {
		return &ApplyResult{Warnings: w}, err
	}
	cmds := Plan(c, run, m.opts(false))
	res := &ApplyResult{Commands: cmds, Warnings: nonNil(w)}
	if i, err := m.exec(cmds); err != nil {
		ae := &ApplyError{Cmd: cmds[i].String(), Err: err.Error()}
		ae.Restored = m.restore(prev) == nil
		m.event("apply failed at %q, previous config restored=%v", cmds[i], ae.Restored)
		return res, ae
	}
	m.applied = c.Clone()
	m.event("config applied (%d commands)", len(cmds))
	if confirm > 0 {
		p := &Pending{Deadline: time.Now().Add(confirm), Save: save, prev: prev}
		p.timer = time.AfterFunc(confirm, func() { m.expire(p) })
		m.pending = p
		res.Pending = p
		return res, nil
	}
	if save {
		res.Saved, err = m.saveLocked()
	}
	return res, err
}

func (m *Manager) restore(c *Config) error {
	run, err := m.state()
	if err != nil {
		return err
	}
	cmds := Plan(c, run, m.opts(false))
	var first error
	for _, x := range cmds {
		if err := m.r.Exec(x); err != nil && first == nil {
			first = err // keep going: restore as much as possible
		}
	}
	m.applied = c.Clone()
	return first
}

func (m *Manager) expire(p *Pending) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pending != p {
		return
	}
	m.pending = nil
	err := m.restore(p.prev)
	m.event("change not confirmed in time, rolled back (err=%v)", err)
}

func (m *Manager) Confirm() (*ApplyResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pending == nil {
		return nil, ErrNoPending
	}
	p := m.pending
	p.timer.Stop()
	m.pending = nil
	m.event("change confirmed")
	res := &ApplyResult{Commands: []Cmd{}, Warnings: []string{}}
	if p.Save {
		var err error
		res.Saved, err = m.saveLocked()
		return res, err
	}
	return res, nil
}

func (m *Manager) Rollback() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pending == nil {
		return ErrNoPending
	}
	p := m.pending
	p.timer.Stop()
	m.pending = nil
	err := m.restore(p.prev)
	m.event("change rolled back on request (err=%v)", err)
	return err
}

func (m *Manager) Save() ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pending != nil {
		return nil, ErrPending
	}
	return m.saveLocked()
}

func (m *Manager) saveLocked() ([]string, error) {
	files, err := SaveConfig(m.applied, m.paths, m.bridge)
	if err != nil {
		m.event("save failed: %v", err)
		return files, err
	}
	m.saved = m.applied.Clone()
	m.event("config saved")
	return files, nil
}

type ConfigView struct {
	Applied *Config  `json:"applied"`
	Saved   *Config  `json:"saved"`
	Unsaved bool     `json:"unsaved_changes"`
	Pending *Pending `json:"pending"`
}

func (m *Manager) View() ConfigView {
	m.mu.Lock()
	defer m.mu.Unlock()
	return ConfigView{Applied: m.applied.Clone(), Saved: m.saved, Pending: m.pending,
		Unsaved: m.saved == nil || !jsonEqual(m.saved, m.applied)}
}

func (m *Manager) Applied() *Config {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.applied.Clone()
}

func (m *Manager) Events() []Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Event{}, m.events...)
}

// Counters returns MIB counters minus the baseline taken by ClearCounters.
func (m *Manager) Counters(port string) (map[string]uint64, error) {
	c, err := ReadCounters(m.r, port)
	if err != nil {
		return nil, err
	}
	m.cntMu.Lock()
	defer m.cntMu.Unlock()
	if b := m.cntBase[port]; b != nil {
		for k, v := range c {
			if v >= b[k] {
				c[k] = v - b[k]
			}
		}
	}
	return c, nil
}

func (m *Manager) ClearCounters(port string) error {
	c, err := ReadCounters(m.r, port)
	if err != nil {
		return err
	}
	m.cntMu.Lock()
	m.cntBase[port] = c
	m.cntMu.Unlock()
	return nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
