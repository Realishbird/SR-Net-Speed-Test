package agent

import (
	"context"
	"encoding/json"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Realishbird/SR-Net-Speed-Test/internal/api"
)

type fakeLink struct {
	addrs []netip.Prefix
	mtu   int
	up    bool
}

// fakeNetplanOS is a host whose clock moves only when the helper sleeps.
type fakeNetplanOS struct {
	now     time.Time
	rdma    []string
	mgmt    map[string]bool
	links   map[string]fakeLink
	pingOK  bool
	genFail bool
	onApply func(f *fakeNetplanOS)
	cmds    []string
}

func (f *fakeNetplanOS) Has(string) bool { return true }
func (f *fakeNetplanOS) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.cmds = append(f.cmds, name+" "+strings.Join(args, " "))
	if name == "netplan" && args[0] == "generate" && f.genFail {
		return []byte("Error in network definition: bad"), os.ErrInvalid
	}
	if name == "netplan" && args[0] == "apply" && f.onApply != nil {
		f.onApply(f)
	}
	if name == "ip" && len(args) == 5 && args[0] == "addr" && args[1] == "del" {
		l := f.links[args[4]]
		var keep []netip.Prefix
		for _, a := range l.addrs {
			if a.String() != args[2] {
				keep = append(keep, a)
			}
		}
		l.addrs = keep
		f.links[args[4]] = l
	}
	return nil, nil
}
func (f *fakeNetplanOS) LinkState(dev string) ([]netip.Prefix, int, bool, error) {
	l := f.links[dev]
	return l.addrs, l.mtu, l.up, nil
}
func (f *fakeNetplanOS) Ping(context.Context, string, string) bool { return f.pingOK }
func (f *fakeNetplanOS) MgmtDevs(context.Context) map[string]bool  { return f.mgmt }
func (f *fakeNetplanOS) RDMADevs() []string                        { return f.rdma }
func (f *fakeNetplanOS) MAC(string) string                         { return "" }
func (f *fakeNetplanOS) Now() time.Time                            { return f.now }
func (f *fakeNetplanOS) Sleep(d time.Duration)                     { f.now = f.now.Add(d) }

const cloudInit = `# This file is generated from information provided by the datasource.
network:
  version: 2
  ethernets:
    enP7s7:
      dhcp4: true
    enp1s0f1np1:
      dhcp4: false
      optional: true
      mtu: 1500
      addresses:
      - 172.17.101.23/24
`

func newNetplanTest(t *testing.T) (netplanEnv, *fakeNetplanOS) {
	t.Helper()
	root := t.TempDir()
	e := netplanEnv{EtcDir: filepath.Join(root, "etc"), ReqDir: filepath.Join(root, "req"), StateDir: filepath.Join(root, "state")}
	for _, d := range []string{e.EtcDir, e.ReqDir, e.StateDir} {
		os.MkdirAll(d, 0o755)
	}
	if err := os.WriteFile(filepath.Join(e.EtcDir, "50-cloud-init.yaml"), []byte(cloudInit), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &fakeNetplanOS{
		now:  time.Date(2026, 10, 22, 18, 0, 0, 0, time.UTC),
		rdma: []string{"enp1s0f0np0", "enp1s0f1np1"},
		mgmt: map[string]bool{"enP7s7": true},
		links: map[string]fakeLink{
			"enp1s0f1np1": {addrs: []netip.Prefix{netip.MustParsePrefix("172.17.101.23/24")}, mtu: 1500, up: true},
		},
		pingOK: true,
	}
	e.Sys = f
	return e, f
}

var acerP2 = api.NetplanChange{ID: 7, Ports: []api.NetplanPort{{Netdev: "enp1s0f1np1", Addresses: []string{"172.27.101.23/24"}, MTU: 9000, Peers: []string{"172.27.101.1"}}}}

// touchAlive marks the agent as having reached the controller after the
// change was applied.
func touchAlive(t *testing.T, e netplanEnv, at time.Time) {
	t.Helper()
	p := filepath.Join(e.ReqDir, "alive")
	os.WriteFile(p, nil, 0o640)
	if err := os.Chtimes(p, at, at); err != nil {
		t.Fatal(err)
	}
}

func readYAML(t *testing.T, p string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(b, &m); err != nil {
		t.Fatalf("%s: %v", p, err)
	}
	return m
}

func ethernets(m map[string]any) map[string]any {
	n, _ := m["network"].(map[string]any)
	e, _ := n["ethernets"].(map[string]any)
	return e
}

func TestNetplanApplyTakesPortOver(t *testing.T) {
	e, f := newNetplanTest(t)
	f.onApply = func(f *fakeNetplanOS) {
		f.links["enp1s0f1np1"] = fakeLink{addrs: []netip.Prefix{netip.MustParsePrefix("172.27.101.23/24")}, mtu: 9000, up: true}
		touchAlive(t, e, f.now.Add(time.Second))
	}
	var st netplanHelperState
	res := e.apply(context.Background(), acerP2, &st)
	if res.Status != "applied" {
		t.Fatalf("status %s: %s %v", res.Status, res.Message, res.Steps)
	}
	ci := ethernets(readYAML(t, filepath.Join(e.EtcDir, "50-cloud-init.yaml")))
	if _, ok := ci["enp1s0f1np1"]; ok {
		t.Fatal("port left in 50-cloud-init.yaml: netplan would add the old address back")
	}
	if _, ok := ci["enP7s7"]; !ok {
		t.Fatal("the management port was removed from 50-cloud-init.yaml")
	}
	b, _ := os.ReadFile(filepath.Join(e.EtcDir, "50-cloud-init.yaml"))
	if !strings.HasPrefix(string(b), "# This file is generated") {
		t.Errorf("comment lost:\n%s", b)
	}
	port, _ := ethernets(readYAML(t, filepath.Join(e.EtcDir, netplanManaged)))["enp1s0f1np1"].(map[string]any)
	if port["mtu"] != 9000 || port["optional"] != true || port["dhcp4"] != false {
		t.Errorf("managed stanza %v: want mtu 9000, optional kept, dhcp4 false", port)
	}
	if a, _ := port["addresses"].([]any); len(a) != 1 || a[0] != "172.27.101.23/24" {
		t.Errorf("addresses %v", port["addresses"])
	}
	if fi, _ := os.Stat(filepath.Join(e.EtcDir, netplanManaged)); fi.Mode().Perm() != 0o600 {
		t.Errorf("managed file mode %v, netplan wants 0600", fi.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(e.StateDir, "backups", "7", "50-cloud-init.yaml")); err != nil {
		t.Errorf("no backup kept: %v", err)
	}
}

func TestNetplanRollsBackWhenChecksFail(t *testing.T) {
	cases := map[string]func(f *fakeNetplanOS, e netplanEnv){
		"address never shows": func(f *fakeNetplanOS, e netplanEnv) { touchAlive(t, e, f.now.Add(time.Second)) },
		"no peer answers": func(f *fakeNetplanOS, e netplanEnv) {
			f.pingOK = false
			f.links["enp1s0f1np1"] = fakeLink{addrs: []netip.Prefix{netip.MustParsePrefix("172.27.101.23/24")}, mtu: 9000, up: true}
			touchAlive(t, e, f.now.Add(time.Second))
		},
		"controller lost": func(f *fakeNetplanOS, e netplanEnv) {
			f.links["enp1s0f1np1"] = fakeLink{addrs: []netip.Prefix{netip.MustParsePrefix("172.27.101.23/24")}, mtu: 9000, up: true}
		},
	}
	for name, after := range cases {
		t.Run(name, func(t *testing.T) {
			e, f := newNetplanTest(t)
			f.onApply = func(f *fakeNetplanOS) {
				if len(f.cmds) < 4 { // generate, apply: the change; the second apply puts things back
					after(f, e)
				}
			}
			var st netplanHelperState
			res := e.apply(context.Background(), acerP2, &st)
			if res.Status != "rolled_back" {
				t.Fatalf("status %s: %s", res.Status, res.Message)
			}
			b, _ := os.ReadFile(filepath.Join(e.EtcDir, "50-cloud-init.yaml"))
			if string(b) != cloudInit {
				t.Errorf("50-cloud-init.yaml not restored:\n%s", b)
			}
			if _, err := os.Stat(filepath.Join(e.EtcDir, netplanManaged)); !os.IsNotExist(err) {
				t.Errorf("managed file left behind: %v", err)
			}
			if n := strings.Count(strings.Join(f.cmds, ","), "netplan apply"); n != 2 {
				t.Errorf("netplan apply ran %d times, want the change and the rollback: %v", n, f.cmds)
			}
			if st.Verifying != 0 {
				t.Error("still marked as verifying")
			}
		})
	}
}

func TestNetplanGenerateFailureChangesNothing(t *testing.T) {
	e, f := newNetplanTest(t)
	f.genFail = true
	var st netplanHelperState
	res := e.apply(context.Background(), acerP2, &st)
	if res.Status != "failed" || !strings.Contains(res.Message, "Error in network definition") {
		t.Fatalf("%s: %s", res.Status, res.Message)
	}
	if b, _ := os.ReadFile(filepath.Join(e.EtcDir, "50-cloud-init.yaml")); string(b) != cloudInit {
		t.Error("file not restored")
	}
	if strings.Contains(strings.Join(f.cmds, ","), "apply") {
		t.Error("netplan apply ran after generate failed")
	}
}

func TestNetplanRefusals(t *testing.T) {
	port := func(dev string, addrs ...string) api.NetplanChange {
		return api.NetplanChange{ID: 9, Ports: []api.NetplanPort{{Netdev: dev, Addresses: addrs}}}
	}
	cases := []struct {
		name string
		ch   api.NetplanChange
		file string
		want string
	}{
		{"management port", port("enP7s7", "10.0.0.5/24"), "", "never changed"},
		{"not RDMA", port("eth9", "10.0.0.5/24"), "", "not an RDMA port"},
		{"network address", port("enp1s0f1np1", "172.27.101.0/24"), "", "network or broadcast"},
		{"broadcast", port("enp1s0f1np1", "172.27.101.255/24"), "", "network or broadcast"},
		{"no prefix", port("enp1s0f1np1", "172.27.101.23"), "", "prefix length"},
		{"bad MTU", api.NetplanChange{ID: 9, Ports: []api.NetplanPort{{Netdev: "enp1s0f1np1", MTU: 100000}}}, "", "MTU"},
		{"injection", port("enp1s0f1np1\n  x", "172.27.101.23/24"), "", "not an interface name"},
		{"wildcard", port("enp1s0f0np0", "172.27.100.13/24"), "network:\n  ethernets:\n    all:\n      match:\n        name: enp*\n      mtu: 9000\n", "wildcard"},
		{"set-name", port("enp1s0f0np0", "172.27.100.13/24"), "network:\n  ethernets:\n    rdma0:\n      match:\n        name: enp1s0f0np0\n      set-name: enp1s0f0np0\n", "set-name"},
	}
	t.Run("management port unknown", func(t *testing.T) {
		e, f := newNetplanTest(t)
		f.mgmt = map[string]bool{}
		var st netplanHelperState
		if res := e.apply(context.Background(), acerP2, &st); res.Status != "refused" || !strings.Contains(res.Message, "management") {
			t.Errorf("%s: %s", res.Status, res.Message)
		}
	})
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, f := newNetplanTest(t)
			if c.file != "" {
				os.WriteFile(filepath.Join(e.EtcDir, "60-extra.yaml"), []byte(c.file), 0o600)
			}
			var st netplanHelperState
			res := e.apply(context.Background(), c.ch, &st)
			if res.Status != "refused" || !strings.Contains(res.Message, c.want) {
				t.Fatalf("%s: %s, want refused with %q", res.Status, res.Message, c.want)
			}
			if len(f.cmds) != 0 {
				t.Errorf("ran %v on a refused change", f.cmds)
			}
		})
	}
}

func TestNetplanHelperRecoversInterruptedChange(t *testing.T) {
	e, f := newNetplanTest(t)
	f.onApply = func(*fakeNetplanOS) {} // the change never verifies
	var st netplanHelperState
	// Simulate a crash after the files were written: keep the backups, mark
	// the change as being checked and leave the new files in place.
	e.keepBackups(acerP2.ID, map[string]fileBackup{
		filepath.Join(e.EtcDir, "50-cloud-init.yaml"): {[]byte(cloudInit), 0o600, true},
		filepath.Join(e.EtcDir, netplanManaged):       {},
	})
	os.WriteFile(filepath.Join(e.EtcDir, "50-cloud-init.yaml"), []byte("network: {version: 2}\n"), 0o600)
	os.WriteFile(filepath.Join(e.EtcDir, netplanManaged), []byte("network: {version: 2}\n"), 0o600)
	st.Verifying = acerP2.ID
	if err := e.saveState(st); err != nil {
		t.Fatal(err)
	}
	if err := e.helper(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := e.loadState()
	if got.Last == nil || got.Last.Status != "rolled_back" || got.Verifying != 0 {
		t.Fatalf("state after recovery: %+v", got)
	}
	if b, _ := os.ReadFile(filepath.Join(e.EtcDir, "50-cloud-init.yaml")); string(b) != cloudInit {
		t.Error("file not restored from the backup")
	}
	if _, err := os.Stat(filepath.Join(e.EtcDir, netplanManaged)); !os.IsNotExist(err) {
		t.Error("managed file the change created is still there")
	}
}

func TestNetplanSnapshot(t *testing.T) {
	e, _ := newNetplanTest(t)
	os.WriteFile(filepath.Join(e.EtcDir, "60-auth.yaml"), []byte("network:\n  ethernets:\n    enp1s0f0np0:\n      auth:\n        password: hunter2\n      mtu: 9000\n"), 0o600)
	if err := e.helper(context.Background()); err != nil {
		t.Fatal(err)
	}
	st := e.loadState()
	if st.Helper != NetplanHelperVersion {
		t.Errorf("helper %q", st.Helper)
	}
	p2 := st.Ports["enp1s0f1np1"]
	if len(p2.Files) != 1 || p2.Files[0] != "50-cloud-init.yaml" || p2.Managed || !strings.Contains(p2.YAML, "172.17.101.23/24") {
		t.Errorf("P2 snapshot %+v", p2)
	}
	if p1 := st.Ports["enp1s0f0np0"]; strings.Contains(p1.YAML, "hunter2") || !strings.Contains(p1.YAML, "mtu: 9000") {
		t.Errorf("P1 snapshot leaks auth or misses mtu: %q", p1.YAML)
	}
	if got := withoutAuth([]byte("network:\n  ethernets:\n    p1:\n      auth: {password: hunter2}\n      mtu: 9000\n")); strings.Contains(got, "hunter2") || !strings.Contains(got, "mtu: 9000") {
		t.Errorf("managed file for the controller: %q", got)
	}
	fi, _ := os.Stat(filepath.Join(e.StateDir, "state.json"))
	if fi.Mode().Perm() != 0o644 {
		t.Errorf("state.json mode %v: the agent must be able to read it", fi.Mode().Perm())
	}
}

func TestNetplanHelperSkipsStaleAndDoneRequests(t *testing.T) {
	e, f := newNetplanTest(t)
	write := func(written time.Time) {
		b, _ := json.Marshal(netplanRequest{Change: acerP2, Written: written})
		os.WriteFile(filepath.Join(e.ReqDir, "request.json"), b, 0o640)
	}
	write(f.now.Add(-time.Hour))
	if err := e.helper(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st := e.loadState(); st.Last == nil || st.Last.Status != "refused" || len(f.cmds) != 0 {
		t.Fatalf("stale request: %+v, cmds %v", st.Last, f.cmds)
	}
	if _, err := os.Stat(filepath.Join(e.ReqDir, "request.json")); !os.IsNotExist(err) {
		t.Error("request not consumed: the path unit would start the helper again")
	}
	// The same change id is never processed twice.
	write(f.now)
	e.helper(context.Background())
	if len(f.cmds) != 0 {
		t.Errorf("processed change %d again: %v", acerP2.ID, f.cmds)
	}
	// A symlink planted by the unprivileged agent is not followed.
	os.Remove(filepath.Join(e.ReqDir, "request.json"))
	os.Symlink("/etc/shadow", filepath.Join(e.ReqDir, "request.json"))
	if _, ok := e.readRequest(); ok {
		t.Error("followed a symlink")
	}
}

func TestNetplanAgentSide(t *testing.T) {
	e, _ := newNetplanTest(t)
	real := e
	real.Sys = realNetplanOS{}
	if res := real.runNetplanChange(context.Background(), acerP2, true, time.Now); res.Status != "refused" || !strings.Contains(res.Message, "test is running") {
		t.Errorf("busy: %+v", res)
	}
	if res := real.runNetplanChange(context.Background(), acerP2, false, time.Now); res.Status != "refused" || !strings.Contains(res.Message, "install.sh") {
		t.Errorf("no helper: %+v", res)
	}
	// With the helper installed, the agent leaves the request and returns the
	// helper's answer, keeping the alive file fresh meanwhile.
	real.saveState(netplanHelperState{NetplanState: api.NetplanState{Helper: "1", At: time.Now()}})
	done := make(chan api.NetplanResult)
	go func() { done <- real.runNetplanChange(context.Background(), acerP2, false, time.Now) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, ok := real.readRequest(); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no request written")
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	os.Remove(filepath.Join(e.ReqDir, "request.json")) // the helper consumes it
	real.saveState(netplanHelperState{NetplanState: api.NetplanState{Helper: "1", At: time.Now(), Last: &api.NetplanResult{ID: acerP2.ID, Status: "applied"}}})
	select {
	case res := <-done:
		if res.Status != "applied" {
			t.Errorf("%+v", res)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("agent did not pick up the helper's answer")
	}
	if _, err := os.Stat(filepath.Join(e.ReqDir, "alive")); err != nil {
		t.Errorf("alive file: %v", err)
	}
}

// TestNetplanMoveTakesAddressOffOldPort moves a Spark's P2 address from the
// cable's first PCIe half to its second. netplan apply sets it on the new
// port but leaves it on the old one, so the helper takes it off there; on a
// rollback it takes it off the new port instead.
func TestNetplanMoveTakesAddressOffOldPort(t *testing.T) {
	addr := netip.MustParsePrefix("172.27.101.20/24")
	move := api.NetplanChange{ID: 9, Ports: []api.NetplanPort{
		{Netdev: "enp1s0f1np1", Addresses: []string{}, MTU: 9000},
		{Netdev: "enP2p1s0f1np1", Addresses: []string{"172.27.101.20/24"}, MTU: 9000, Peers: []string{"172.27.101.1"}},
	}}
	for _, ok := range []bool{true, false} {
		e, f := newNetplanTest(t)
		f.rdma = append(f.rdma, "enP2p1s0f1np1")
		f.links["enp1s0f1np1"] = fakeLink{addrs: []netip.Prefix{addr, netip.MustParsePrefix("fe80::1/64")}, mtu: 9000, up: true}
		f.links["enP2p1s0f1np1"] = fakeLink{mtu: 1500, up: true}
		f.pingOK = ok
		f.onApply = func(f *fakeNetplanOS) {
			if strings.Count(strings.Join(f.cmds, ","), "netplan apply") == 1 {
				f.links["enP2p1s0f1np1"] = fakeLink{addrs: []netip.Prefix{addr}, mtu: 9000, up: true}
				touchAlive(t, e, f.now.Add(time.Second))
			}
		}
		var st netplanHelperState
		res := e.apply(context.Background(), move, &st)
		cmds := strings.Join(f.cmds, ",")
		if ok {
			if res.Status != "applied" || !strings.Contains(cmds, "ip addr del 172.27.101.20/24 dev enp1s0f1np1") || strings.Contains(cmds, "fe80") {
				t.Errorf("move: %s %s %v", res.Status, res.Message, f.cmds)
			}
			if len(f.links["enP2p1s0f1np1"].addrs) != 1 {
				t.Errorf("new port lost its address: %v", f.links["enP2p1s0f1np1"])
			}
			continue
		}
		if res.Status != "rolled_back" || !strings.Contains(cmds, "ip addr del 172.27.101.20/24 dev enP2p1s0f1np1") {
			t.Errorf("rollback: %s %s %v", res.Status, res.Message, f.cmds)
		}
	}
}
