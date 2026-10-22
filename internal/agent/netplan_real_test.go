package agent

import (
	"context"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// realGenOS runs the real netplan generate against a temporary root, so
// the files the helper writes are checked by netplan itself. The test is
// skipped where netplan is not installed (CI).
type realGenOS struct {
	*fakeNetplanOS
	root string
	t    *testing.T
}

func (r realGenOS) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if name == "netplan" && args[0] == "generate" {
		out, err := exec.Command("netplan", "generate", "--root-dir", r.root).CombinedOutput()
		r.t.Logf("netplan generate: %v %s", err, out)
		return out, err
	}
	return r.fakeNetplanOS.Run(ctx, name, args...)
}

func TestNetplanFilesPassRealNetplan(t *testing.T) {
	if _, err := exec.LookPath("netplan"); err != nil {
		t.Skip("netplan is not installed")
	}
	for name, files := range map[string]map[string]string{
		"cloud-init": {"50-cloud-init.yaml": cloudInit},
		"networkmanager": {
			"01-network-manager-all.yaml": "network:\n  version: 2\n  renderer: NetworkManager\n",
			"90-NM-1234.yaml":             "network:\n  version: 2\n  ethernets:\n    NM-1234:\n      renderer: NetworkManager\n      match:\n        name: \"enp1s0f1np1\"\n      addresses:\n      - \"172.17.101.23/24\"\n      mtu: 1500\n      networkmanager:\n        uuid: \"1234abcd-0000-0000-0000-000000000000\"\n        name: \"P2\"\n",
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			etc := filepath.Join(root, "etc/netplan")
			os.MkdirAll(etc, 0o755)
			for f, c := range files {
				os.WriteFile(filepath.Join(etc, f), []byte(c), 0o600)
			}
			e, f := newNetplanTest(t)
			e.EtcDir = etc
			e.Sys = realGenOS{f, root, t}
			f.onApply = func(f *fakeNetplanOS) {
				f.links["enp1s0f1np1"] = fakeLink{addrs: []netip.Prefix{netip.MustParsePrefix("172.27.101.23/24")}, mtu: 9000, up: true}
				touchAlive(t, e, f.now.Add(time.Second))
			}
			var st netplanHelperState
			res := e.apply(context.Background(), acerP2, &st)
			if res.Status != "applied" {
				t.Fatalf("%s: %s %v", res.Status, res.Message, res.Steps)
			}
			out, err := exec.Command("netplan", "get", "--root-dir", root).CombinedOutput()
			t.Logf("netplan get:\n%s", out)
			if err != nil || strings.Contains(string(out), "172.17.101.23") || !strings.Contains(string(out), "172.27.101.23/24") {
				t.Errorf("merged config wrong: %v", err)
			}
			b, _ := os.ReadFile(filepath.Join(etc, "90-srnet.yaml"))
			t.Logf("90-srnet.yaml:\n%s", b)
		})
	}
}
