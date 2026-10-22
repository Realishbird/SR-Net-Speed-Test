package agent

// Network changes to RDMA ports.
//
// The agent runs as the unprivileged srnet user and cannot write
// /etc/netplan, so a small root helper does it: install.sh adds
// srnet-netplan.path, .service and .timer, which run the root-owned
// /usr/local/bin/srnet-agent netplan-helper when the agent leaves a request
// in /var/lib/srnet/netplan, and every 5 minutes to refresh what the agent
// reports about each port's netplan setup.
//
// The helper only ever writes /etc/netplan/90-srnet.yaml, plus the ports it
// takes over removed from the other netplan files (netplan adds up the
// addresses from every file that defines a port, so an old address would
// stay). It refuses the management port and anything that is not an RDMA
// netdev, runs netplan generate and apply, and keeps the change only when
// every port shows its addresses, MTU and link, a fabric peer answers a ping
// from each port, and the agent has reached the controller since. Otherwise
// it puts every file back as it was and applies again.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Realishbird/SR-Net-Speed-Test/internal/api"
)

// NetplanHelperVersion is reported to the controller; bump it when the
// request or state format changes.
const NetplanHelperVersion = "1"

const (
	netplanManaged = "90-srnet.yaml"
	netplanHeader  = "# Managed by SR-Net-Speed-Test. Change these ports from the controller's Hosts page;\n# edits made here are kept for keys the controller does not set (routes, for example).\n"
)

// netplanEnv is where the helper and the agent find things. Tests point it
// at temporary directories and a fake system.
type netplanEnv struct {
	EtcDir   string // /etc/netplan
	ReqDir   string // /var/lib/srnet/netplan: the agent's request.json and alive
	StateDir string // /var/lib/srnet-netplan: the helper's state.json and backups (root only writes here)
	Sys      netplanOS
}

func defaultNetplanEnv() netplanEnv {
	return netplanEnv{EtcDir: "/etc/netplan", ReqDir: "/var/lib/srnet/netplan", StateDir: "/var/lib/srnet-netplan", Sys: realNetplanOS{}}
}

// netplanOS is the part of the system the helper asks about or changes
// besides files.
type netplanOS interface {
	Has(cmd string) bool
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
	LinkState(dev string) (addrs []netip.Prefix, mtu int, up bool, err error)
	Ping(ctx context.Context, dev, peer string) bool
	MgmtDevs(ctx context.Context) map[string]bool
	RDMADevs() []string
	MAC(dev string) string
	Now() time.Time
	Sleep(d time.Duration)
}

// netplanRequest is what the agent leaves for the helper.
type netplanRequest struct {
	Change  api.NetplanChange `json:"change"`
	Written time.Time         `json:"written"`
}

// netplanHelperState is what the helper writes for the agent to report.
type netplanHelperState struct {
	api.NetplanState
	Ports     map[string]api.PortNetplan `json:"ports"`
	Verifying int64                      `json:"verifying,omitempty"` // change being checked: the agent shows it still reaches the controller
}

// NetplanHelper is the root side: it processes a pending request, then
// refreshes the snapshot. systemd runs it (srnet-netplan.service).
func NetplanHelper(ctx context.Context) error {
	return defaultNetplanEnv().helper(ctx)
}

func (e netplanEnv) helper(ctx context.Context) error {
	if err := os.MkdirAll(e.StateDir, 0o755); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(e.StateDir, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	st := e.loadState()
	if st.Verifying != 0 {
		res := e.recoverInterrupted(ctx, st.Verifying)
		st.Last, st.Verifying = &res, 0
	}
	// The request is consumed: srnet-netplan.path starts this service
	// whenever request.json exists, and again after it ends while it still
	// does, so it must be gone before the (slow) apply.
	req, ok := e.readRequest()
	os.Remove(filepath.Join(e.ReqDir, "request.json"))
	if ok && (st.Last == nil || st.Last.ID != req.Change.ID) {
		if age := e.Sys.Now().Sub(req.Written); age > 2*time.Minute {
			// A request the helper never saw while it was fresh (it was not
			// installed, or the host was down) is not applied later: the
			// agent has long reported it as not picked up.
			st.Last = &api.NetplanResult{ID: req.Change.ID, Status: "refused", Message: "the request is too old to apply", At: e.Sys.Now()}
		} else {
			res := e.apply(ctx, req.Change, &st)
			st.Last = &res
		}
	}
	e.snapshot(ctx, &st)
	return e.saveState(st)
}

func (e netplanEnv) loadState() netplanHelperState {
	var st netplanHelperState
	if b, err := os.ReadFile(filepath.Join(e.StateDir, "state.json")); err == nil {
		json.Unmarshal(b, &st)
	}
	return st
}

func (e netplanEnv) saveState(st netplanHelperState) error {
	b, _ := json.MarshalIndent(st, "", " ")
	return writeAtomic(filepath.Join(e.StateDir, "state.json"), b, 0o644)
}

// readRequest reads the agent's request without following a symlink: the
// directory belongs to the unprivileged agent.
func (e netplanEnv) readRequest() (netplanRequest, bool) {
	var req netplanRequest
	f, err := os.OpenFile(filepath.Join(e.ReqDir, "request.json"), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return req, false
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return req, false
	}
	b, err := io.ReadAll(io.LimitReader(f, 64<<10))
	if err != nil || json.Unmarshal(b, &req) != nil || req.Change.ID == 0 {
		return req, false
	}
	return req, true
}

// writeAtomic writes through a temporary file in the same directory.
func writeAtomic(name string, b []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(name), "."+filepath.Base(name)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), name)
}

// netplanFiles lists the YAML files netplan reads from /etc/netplan, in
// the order it reads them.
func (e netplanEnv) netplanFiles() []string {
	files, _ := filepath.Glob(filepath.Join(e.EtcDir, "*.yaml"))
	sort.Strings(files)
	return files
}

// stanza is one ethernets entry that configures a port.
type stanza struct {
	key      string
	value    *yaml.Node
	renderer string // the stanza's own renderer, else its file's
	setName  string
	glob     bool // matched by a wildcard that may cover other ports too
}

func mapGet(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func scalar(n *yaml.Node) string {
	if n == nil || n.Kind != yaml.ScalarNode {
		return ""
	}
	return n.Value
}

// ethernetsOf returns a netplan document's ethernets mapping and the file's renderer.
func ethernetsOf(doc *yaml.Node) (*yaml.Node, string) {
	if doc == nil || doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, ""
	}
	network := mapGet(doc.Content[0], "network")
	return mapGet(network, "ethernets"), scalar(mapGet(network, "renderer"))
}

// findStanzas returns the entries of a document that configure dev: keyed
// by its name, or matching it by name (exact or wildcard) or MAC address.
func findStanzas(doc *yaml.Node, dev, mac string) []stanza {
	eth, renderer := ethernetsOf(doc)
	if eth == nil || eth.Kind != yaml.MappingNode {
		return nil
	}
	var out []stanza
	for i := 0; i+1 < len(eth.Content); i += 2 {
		key, val := eth.Content[i].Value, eth.Content[i+1]
		s := stanza{key: key, value: val, renderer: renderer, setName: scalar(mapGet(val, "set-name"))}
		if r := scalar(mapGet(val, "renderer")); r != "" {
			s.renderer = r
		}
		match := mapGet(val, "match")
		name, macMatch := scalar(mapGet(match, "name")), scalar(mapGet(match, "macaddress"))
		switch {
		case match == nil && key == dev:
		case s.setName == dev:
		case name == dev:
		case name != "" && strings.ContainsAny(name, "*?[") && globMatch(name, dev):
			s.glob = true
		case macMatch != "" && mac != "" && strings.EqualFold(macMatch, mac):
		default:
			continue
		}
		out = append(out, s)
	}
	return out
}

func globMatch(pattern, name string) bool {
	ok, err := path.Match(pattern, name)
	return err == nil && ok
}

// removeStanzas deletes the given keys from a document's ethernets.
func removeStanzas(doc *yaml.Node, keys map[string]bool) {
	eth, _ := ethernetsOf(doc)
	if eth == nil {
		return
	}
	var kept []*yaml.Node
	for i := 0; i+1 < len(eth.Content); i += 2 {
		if !keys[eth.Content[i].Value] {
			kept = append(kept, eth.Content[i], eth.Content[i+1])
		}
	}
	eth.Content = kept
	if len(kept) == 0 {
		eth.Style = yaml.FlowStyle // "ethernets: {}"
	}
}

// managedFile is the controller-managed file's content.
type managedFile struct {
	Network struct {
		Version   int                       `yaml:"version"`
		Ethernets map[string]map[string]any `yaml:"ethernets"`
	} `yaml:"network"`
}

func renderManaged(eth map[string]map[string]any) []byte {
	var m managedFile
	m.Network.Version, m.Network.Ethernets = 2, eth
	b, _ := marshalYAML(m)
	return append([]byte(netplanHeader), b...)
}

// marshalYAML writes YAML indented by two spaces, as netplan files usually are.
func marshalYAML(v any) ([]byte, error) {
	var b strings.Builder
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	enc.Close()
	return []byte(b.String()), nil
}

// fileBackup is a file's content before the change; existed false means
// the change created it.
type fileBackup struct {
	data    []byte
	mode    os.FileMode
	existed bool
}

// apply makes one change. It never returns before the files are either
// changed and checked, or back as they were.
func (e netplanEnv) apply(ctx context.Context, ch api.NetplanChange, st *netplanHelperState) api.NetplanResult {
	res := api.NetplanResult{ID: ch.ID, At: e.Sys.Now()}
	step := func(f string, a ...any) { res.Steps = append(res.Steps, fmt.Sprintf(f, a...)) }
	refuse := func(f string, a ...any) api.NetplanResult {
		res.Status, res.Message = "refused", fmt.Sprintf(f, a...)
		return res
	}
	if err := ch.Validate(); err != nil {
		return refuse("%v", err)
	}
	if !e.Sys.Has("netplan") {
		return refuse("netplan is not installed on this host")
	}
	rdma := map[string]bool{}
	for _, d := range e.Sys.RDMADevs() {
		rdma[d] = true
	}
	mgmt := e.Sys.MgmtDevs(ctx)
	if len(mgmt) == 0 {
		return refuse("cannot tell which port carries the management connection (no default route and no route to the controller found), so nothing is changed")
	}
	for _, p := range ch.Ports {
		if mgmt[p.Netdev] {
			return refuse("%s carries the default route or the controller connection; it is never changed from here", p.Netdev)
		}
		if !rdma[p.Netdev] {
			return refuse("%s is not an RDMA port on this host", p.Netdev)
		}
	}

	managedPath := filepath.Join(e.EtcDir, netplanManaged)
	backups := map[string]fileBackup{}
	writes := map[string][]byte{}
	managed := map[string]map[string]any{}
	if b, err := os.ReadFile(managedPath); err == nil {
		var m managedFile
		if err := yaml.Unmarshal(b, &m); err != nil {
			return refuse("cannot read %s: %v", managedPath, err)
		}
		for k, v := range m.Network.Ethernets {
			managed[k] = v
		}
	}
	// Take the ports out of every other file, keeping their other settings.
	carried := map[string]map[string]any{}
	for _, f := range e.netplanFiles() {
		if f == managedPath {
			continue
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			return refuse("cannot read %s: %v", f, err)
		}
		var doc yaml.Node
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			return refuse("cannot parse %s: %v", f, err)
		}
		remove := map[string]bool{}
		for _, p := range ch.Ports {
			for _, s := range findStanzas(&doc, p.Netdev, e.Sys.MAC(p.Netdev)) {
				if s.glob {
					return refuse("%s matches %s with a wildcard (%s), which may cover other ports; narrow it by hand first", filepath.Base(f), p.Netdev, s.key)
				}
				if s.setName != "" {
					return refuse("%s renames %s (set-name); change that port by hand", filepath.Base(f), p.Netdev)
				}
				var m map[string]any
				if err := s.value.Decode(&m); err != nil {
					return refuse("cannot read %s in %s: %v", s.key, f, err)
				}
				c := carried[p.Netdev]
				if c == nil {
					c = map[string]any{}
					carried[p.Netdev] = c
				}
				for k, v := range m {
					c[k] = v
				}
				if s.renderer != "" && c["renderer"] == nil {
					c["renderer"] = s.renderer
				}
				remove[s.key] = true
				step("took %s out of %s", p.Netdev, filepath.Base(f))
				if strings.Contains(filepath.Base(f), "cloud-init") {
					step("%s comes from cloud-init; it is only rewritten when cloud-init sets up networking again", filepath.Base(f))
				}
			}
		}
		if len(remove) > 0 {
			removeStanzas(&doc, remove)
			out, err := marshalYAML(&doc)
			if err != nil {
				return refuse("cannot write %s: %v", f, err)
			}
			fi, _ := os.Stat(f)
			backups[f] = fileBackup{raw, fi.Mode().Perm(), true}
			writes[f] = out
		}
	}
	for _, p := range ch.Ports {
		s := map[string]any{}
		for k, v := range managed[p.Netdev] {
			s[k] = v
		}
		for k, v := range carried[p.Netdev] {
			s[k] = v
		}
		for _, k := range []string{"match", "set-name", "addresses", "dhcp4", "mtu"} {
			delete(s, k)
		}
		s["dhcp4"] = p.DHCP4
		if len(p.Addresses) > 0 {
			s["addresses"] = p.Addresses
		}
		if p.MTU > 0 {
			s["mtu"] = p.MTU
		}
		managed[p.Netdev] = s
	}
	if b, err := os.ReadFile(managedPath); err == nil {
		fi, _ := os.Stat(managedPath)
		backups[managedPath] = fileBackup{b, fi.Mode().Perm(), true}
	} else {
		backups[managedPath] = fileBackup{}
	}
	writes[managedPath] = renderManaged(managed)
	e.keepBackups(ch.ID, backups)

	restore := func() {
		for f, b := range backups {
			if !b.existed {
				os.Remove(f)
				continue
			}
			writeAtomic(f, b.data, b.mode)
		}
	}
	for f, b := range writes {
		mode := os.FileMode(0o600)
		if bk, ok := backups[f]; ok && bk.existed {
			mode = bk.mode
		}
		if err := writeAtomic(f, b, mode); err != nil {
			restore()
			res.Status, res.Message = "failed", fmt.Sprintf("cannot write %s: %v", f, err)
			return res
		}
	}
	step("wrote %s", managedPath)
	if out, err := e.Sys.Run(ctx, "netplan", "generate"); err != nil {
		restore()
		res.Status, res.Message = "failed", "netplan generate rejected the change, nothing was applied: "+firstLines(out, err)
		return res
	}
	st.Verifying = ch.ID
	e.saveState(*st)
	defer func() { st.Verifying = 0 }()
	applied := e.Sys.Now()
	if out, err := e.Sys.Run(ctx, "netplan", "apply"); err != nil {
		restore()
		e.Sys.Run(ctx, "netplan", "apply")
		res.Status, res.Message = "rolled_back", "netplan apply failed: "+firstLines(out, err)
		step("put the previous files back and applied them")
		return res
	}
	step("netplan apply")

	timeout := time.Duration(ch.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	if timeout > 5*time.Minute {
		timeout = 5 * time.Minute
	}
	var problems []string
	for {
		problems = e.check(ctx, ch, applied)
		if len(problems) == 0 || e.Sys.Now().Sub(applied) >= timeout {
			break
		}
		e.Sys.Sleep(2 * time.Second)
	}
	if len(problems) == 0 {
		res.Status = "applied"
		res.Message = fmt.Sprintf("applied and checked in %.0f s", e.Sys.Now().Sub(applied).Seconds())
		step("checks passed: addresses, MTU, link, peers, controller")
		return res
	}
	restore()
	e.Sys.Run(ctx, "netplan", "apply")
	step("checks failed after %.0f s; put the previous files back and applied them", timeout.Seconds())
	res.Status, res.Message = "rolled_back", strings.Join(problems, "; ")
	return res
}

// check lists what is not yet as the change wants it.
func (e netplanEnv) check(ctx context.Context, ch api.NetplanChange, applied time.Time) []string {
	var problems []string
	for _, p := range ch.Ports {
		addrs, mtu, up, err := e.Sys.LinkState(p.Netdev)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", p.Netdev, err))
			continue
		}
		if !up {
			problems = append(problems, p.Netdev+" has no link")
		}
		for _, a := range p.Addresses {
			want, _ := netip.ParsePrefix(a)
			if !hasPrefix(addrs, want) {
				problems = append(problems, fmt.Sprintf("%s does not have %s", p.Netdev, a))
			}
		}
		if p.DHCP4 && len(p.Addresses) == 0 && !hasIPv4(addrs) {
			problems = append(problems, p.Netdev+" got no DHCP address")
		}
		if p.MTU > 0 && mtu != p.MTU {
			problems = append(problems, fmt.Sprintf("%s MTU is %d, not %d", p.Netdev, mtu, p.MTU))
		}
		if len(p.Peers) > 0 && up {
			answered := false
			for _, peer := range p.Peers {
				if e.Sys.Ping(ctx, p.Netdev, peer) {
					answered = true
					break
				}
			}
			if !answered {
				problems = append(problems, fmt.Sprintf("no fabric peer answered a ping from %s (%s)", p.Netdev, strings.Join(p.Peers, ", ")))
			}
		}
	}
	if fi, err := os.Stat(filepath.Join(e.ReqDir, "alive")); err != nil || !fi.ModTime().After(applied) {
		problems = append(problems, "the agent has not reached the controller since the change")
	}
	return problems
}

func hasPrefix(have []netip.Prefix, want netip.Prefix) bool {
	for _, h := range have {
		if h.Addr() == want.Addr() && h.Bits() == want.Bits() {
			return true
		}
	}
	return false
}

func hasIPv4(have []netip.Prefix) bool {
	for _, h := range have {
		if h.Addr().Is4() {
			return true
		}
	}
	return false
}

func firstLines(out []byte, err error) string {
	s := strings.TrimSpace(string(out))
	if s == "" && err != nil {
		s = err.Error()
	}
	if lines := strings.Split(s, "\n"); len(lines) > 6 {
		s = strings.Join(lines[:6], "\n")
	}
	return s
}

// recoverInterrupted puts back the files of a change the helper was still
// checking when it stopped (a reboot, or the service killed), since nothing
// confirmed that change.
func (e netplanEnv) recoverInterrupted(ctx context.Context, id int64) api.NetplanResult {
	res := api.NetplanResult{ID: id, Status: "rolled_back", Message: "the helper stopped while checking the change; the previous files were put back", At: e.Sys.Now()}
	dir := filepath.Join(e.StateDir, "backups", fmt.Sprint(id))
	files, err := os.ReadDir(dir)
	if err != nil {
		res.Status, res.Message = "failed", "the helper stopped while checking the change and its backup is gone: "+err.Error()
		return res
	}
	for _, f := range files {
		name := f.Name()
		if base, ok := strings.CutSuffix(name, ".absent"); ok {
			os.Remove(filepath.Join(e.EtcDir, base))
			continue
		}
		if b, err := os.ReadFile(filepath.Join(dir, name)); err == nil {
			writeAtomic(filepath.Join(e.EtcDir, name), b, 0o600)
		}
	}
	e.Sys.Run(ctx, "netplan", "apply")
	res.Steps = []string{"put back the files saved in " + dir, "netplan apply"}
	return res
}

// keepBackups copies the files a change touches to the state directory, so
// the last few changes can be undone by hand.
func (e netplanEnv) keepBackups(id int64, backups map[string]fileBackup) {
	dir := filepath.Join(e.StateDir, "backups", fmt.Sprint(id))
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	for f, b := range backups {
		if b.existed {
			os.WriteFile(filepath.Join(dir, filepath.Base(f)), b.data, 0o600)
		} else {
			os.WriteFile(filepath.Join(dir, filepath.Base(f)+".absent"), nil, 0o600)
		}
	}
	old, _ := os.ReadDir(filepath.Join(e.StateDir, "backups"))
	sort.Slice(old, func(i, j int) bool { return old[i].Name() < old[j].Name() })
	for len(old) > 10 {
		os.RemoveAll(filepath.Join(e.StateDir, "backups", old[0].Name()))
		old = old[1:]
	}
}

// snapshot records, for every RDMA port, which netplan files define it and
// how, leaving out anything secret.
func (e netplanEnv) snapshot(ctx context.Context, st *netplanHelperState) {
	st.Helper, st.At, st.Error = NetplanHelperVersion, e.Sys.Now(), ""
	if !e.Sys.Has("netplan") {
		st.Error = "netplan is not installed on this host"
	}
	mgmt := e.Sys.MgmtDevs(ctx)
	if len(mgmt) == 0 && st.Error == "" {
		st.Error = "cannot tell which port carries the management connection (no default route and no route to the controller found)"
	}
	ports := map[string]api.PortNetplan{}
	devs := e.Sys.RDMADevs()
	for _, d := range devs {
		ports[d] = api.PortNetplan{Mgmt: mgmt[d]}
	}
	managedPath := filepath.Join(e.EtcDir, netplanManaged)
	st.File = ""
	for _, f := range e.netplanFiles() {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		if f == managedPath {
			st.File = withoutAuth(raw)
		}
		var doc yaml.Node
		if yaml.Unmarshal(raw, &doc) != nil {
			continue
		}
		for _, d := range devs {
			for _, s := range findStanzas(&doc, d, e.Sys.MAC(d)) {
				pn := ports[d]
				pn.Files = append(pn.Files, filepath.Base(f))
				pn.Managed = pn.Managed || f == managedPath
				var m map[string]any
				if s.value.Decode(&m) == nil {
					delete(m, "auth") // 802.1X credentials
					b, _ := marshalYAML(map[string]any{s.key: m})
					pn.YAML += "# " + filepath.Base(f) + "\n" + string(b)
				}
				ports[d] = pn
			}
		}
	}
	st.Ports = ports
}

// withoutAuth is the managed file as the controller may see it: 802.1X
// credentials carried over from another file are left out.
func withoutAuth(raw []byte) string {
	var m map[string]any
	if yaml.Unmarshal(raw, &m) != nil {
		return ""
	}
	if n, ok := m["network"].(map[string]any); ok {
		if eth, ok := n["ethernets"].(map[string]any); ok {
			for _, v := range eth {
				if st, ok := v.(map[string]any); ok {
					delete(st, "auth")
				}
			}
		}
	}
	b, err := marshalYAML(m)
	if err != nil {
		return ""
	}
	return netplanHeader + string(b)
}

// Agent side.

// netplanState reads what the helper last wrote; nil when it is not installed.
func (e netplanEnv) state() *netplanHelperState {
	b, err := os.ReadFile(filepath.Join(e.StateDir, "state.json"))
	if err != nil {
		return nil
	}
	var st netplanHelperState
	if json.Unmarshal(b, &st) != nil {
		return nil
	}
	return &st
}

// addNetplan puts the helper's view into the inventory.
func addNetplan(inv *api.Inventory) {
	st := defaultNetplanEnv().state()
	if st == nil {
		return
	}
	s := st.NetplanState
	inv.Netplan = &s
	for i := range inv.Ports {
		if pn, ok := st.Ports[inv.Ports[i].Netdev]; ok {
			pn := pn
			inv.Ports[i].Netplan = &pn
		}
	}
}

// runNetplanChange hands a change to the helper and waits for its answer,
// showing it meanwhile when the agent last reached the controller (the
// sampler posts every second, so lastOK stays fresh while the path works).
func (e netplanEnv) runNetplanChange(ctx context.Context, ch api.NetplanChange, busy bool, lastOK func() time.Time) api.NetplanResult {
	res := api.NetplanResult{ID: ch.ID, At: e.Sys.Now()}
	if busy {
		res.Status, res.Message = "refused", "a test is running on this host"
		return res
	}
	before := e.state()
	if before == nil {
		res.Status, res.Message = "refused", "the netplan helper is not installed on this host; re-run install.sh to add it"
		return res
	}
	if err := ch.Validate(); err != nil {
		res.Status, res.Message = "refused", err.Error()
		return res
	}
	written := e.Sys.Now()
	b, _ := json.Marshal(netplanRequest{Change: ch, Written: written})
	reqPath := filepath.Join(e.ReqDir, "request.json")
	if err := writeAtomic(reqPath, b, 0o640); err != nil {
		res.Status, res.Message = "failed", "cannot leave the request for the helper: "+err.Error()
		return res
	}
	timeout := time.Duration(ch.TimeoutSec)*time.Second + 2*time.Minute
	if ch.TimeoutSec <= 0 {
		timeout = 90*time.Second + 2*time.Minute
	}
	alive := filepath.Join(e.ReqDir, "alive")
	for {
		// The helper keeps the change only once this file is newer than its
		// netplan apply: the agent's last successful call to the controller.
		if t := lastOK(); !t.IsZero() && t.After(written) {
			if os.Chtimes(alive, t, t) != nil {
				os.WriteFile(alive, nil, 0o640)
				os.Chtimes(alive, t, t)
			}
		}
		st := e.state()
		if st != nil && st.Last != nil && st.Last.ID == ch.ID {
			return *st.Last
		}
		waited := e.Sys.Now().Sub(written)
		if _, err := os.Lstat(reqPath); err == nil && waited > 45*time.Second {
			// Never picked up: take it back so a late start cannot apply it
			// after this change was reported as failed.
			os.Remove(reqPath)
			res.Status, res.Message = "failed", "the netplan helper did not pick up the request; check `systemctl status srnet-netplan.path` or re-run install.sh"
			return res
		}
		if waited > timeout {
			res.Status, res.Message = "failed", "the netplan helper did not answer in time; see `journalctl -u srnet-netplan` on the host"
			return res
		}
		select {
		case <-ctx.Done():
			res.Status, res.Message = "failed", "the agent stopped while waiting for the helper"
			return res
		case <-time.After(2 * time.Second):
		}
	}
}

// realNetplanOS is the live system.
type realNetplanOS struct{}

func (realNetplanOS) Has(cmd string) bool {
	_, err := exec.LookPath(cmd)
	if err != nil {
		for _, d := range []string{"/usr/sbin", "/sbin", "/usr/bin"} {
			if _, e := os.Stat(filepath.Join(d, cmd)); e == nil {
				return true
			}
		}
	}
	return err == nil
}

func (realNetplanOS) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	return cmd.CombinedOutput()
}

func (realNetplanOS) LinkState(dev string) ([]netip.Prefix, int, bool, error) {
	ifc, err := net.InterfaceByName(dev)
	if err != nil {
		return nil, 0, false, err
	}
	addrs, err := ifc.Addrs()
	if err != nil {
		return nil, 0, false, err
	}
	var out []netip.Prefix
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			ip, ok := netip.AddrFromSlice(n.IP)
			if !ok {
				continue
			}
			ones, _ := n.Mask.Size()
			out = append(out, netip.PrefixFrom(ip.Unmap(), ones))
		}
	}
	up := ifc.Flags&net.FlagUp != 0 && ifc.Flags&net.FlagRunning != 0
	return out, ifc.MTU, up, nil
}

func (r realNetplanOS) Ping(ctx context.Context, dev, peer string) bool {
	args := []string{"-c", "1", "-W", "1", "-I", dev, peer}
	if a, err := netip.ParseAddr(peer); err == nil && a.Is6() {
		args = append([]string{"-6"}, args...)
	}
	_, err := r.Run(ctx, "ping", args...)
	return err == nil
}

// MgmtDevs are the interfaces that carry a default route or the route to
// the controller.
func (r realNetplanOS) MgmtDevs(ctx context.Context) map[string]bool {
	out := map[string]bool{}
	type route struct {
		Dev string `json:"dev"`
	}
	add := func(b []byte) {
		var rs []route
		if json.Unmarshal(b, &rs) == nil {
			for _, x := range rs {
				if x.Dev != "" {
					out[x.Dev] = true
				}
			}
		}
	}
	for _, fam := range []string{"-4", "-6"} {
		b, _ := r.Run(ctx, "ip", "-j", fam, "route", "show", "default")
		add(b)
	}
	if cfg, err := LoadConfig(DefaultConfigPath); err == nil {
		if u, err := url.Parse(cfg.Controller); err == nil {
			ips, _ := net.DefaultResolver.LookupIPAddr(ctx, u.Hostname())
			for _, ip := range ips {
				b, _ := r.Run(ctx, "ip", "-j", "route", "get", ip.IP.String())
				add(b)
			}
		}
	}
	return out
}

func (realNetplanOS) RDMADevs() []string {
	dirs, _ := filepath.Glob(filepath.Join(sysRoot, "class/net/*/device/infiniband"))
	var out []string
	for _, d := range dirs {
		out = append(out, filepath.Base(filepath.Dir(filepath.Dir(d))))
	}
	sort.Strings(out)
	return out
}

func (realNetplanOS) MAC(dev string) string {
	ifc, err := net.InterfaceByName(dev)
	if err != nil {
		return ""
	}
	return ifc.HardwareAddr.String()
}

func (realNetplanOS) Now() time.Time        { return time.Now() }
func (realNetplanOS) Sleep(d time.Duration) { time.Sleep(d) }
