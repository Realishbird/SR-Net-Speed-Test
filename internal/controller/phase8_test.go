package controller

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Realishbird/SR-Net-Speed-Test/internal/api"
)

func TestEnrolAllowed(t *testing.T) {
	e := EnrolSettings{CIDRs: []string{"172.27.100.0/24", " 10.0.0.0/8 ", "bogus"}}
	for ip, want := range map[string]bool{"172.27.100.17": true, "10.1.2.3": true, "172.27.101.17": false, "": false, "nope": false} {
		if got := enrolAllowed(e, ip); got != want {
			t.Errorf("enrolAllowed(%q) = %v", ip, got)
		}
	}
	if enrolAllowed(EnrolSettings{}, "10.0.0.1") {
		t.Error("no subnets should allow nobody")
	}
}

func TestApprovedHostsFilter(t *testing.T) {
	hosts := []hostRec{{Host: api.Host{ID: 1, Name: "a", Approved: true}}, {Host: api.Host{ID: 2, Name: "pending"}}, {Host: api.Host{ID: 3, Name: "c", Approved: true}}}
	got := approvedHosts(hosts)
	if len(got) != 2 || got[0].ID != 1 || got[1].ID != 3 {
		t.Errorf("approvedHosts = %+v", got)
	}
}

func TestSwitchLinks(t *testing.T) {
	switches := []SwitchConfig{{Name: "leaf1", Address: "10.0.0.1"}, {Name: "spine1", Address: "10.0.0.2"}}
	lldp := map[string]lldpEntry{
		"spine1|ethernet0":       {Switch: "leaf1", Port: "Ethernet48"}, // leaf sees the spine
		"leaf1|ethernet48":       {Switch: "spine1", Port: "Ethernet0"}, // spine sees the leaf (same cable)
		"spine1|ethernet4":       {Switch: "leaf1", Port: "Ethernet52"}, // second cable
		"spark-424f|enp1s0f0np0": {Switch: "leaf1", Port: "Ethernet0"},  // a host, not a switch
	}
	links := switchLinks(lldp, switches)
	if len(links) != 2 {
		t.Fatalf("links = %+v", links)
	}
	if links[0] != (api.TopoLink{A: "leaf1", APort: "Ethernet48", B: "spine1", BPort: "Ethernet0"}) || links[1].APort != "Ethernet52" {
		t.Errorf("links = %+v", links)
	}
	if got := switchLinks(lldp, switches[:1]); len(got) != 0 {
		t.Errorf("one switch has no inter-switch links: %+v", got)
	}
}

func TestReleaseImport(t *testing.T) {
	dir := t.TempDir()
	r := newReleases(dir, nil, newHub(), nil)
	bin := []byte("#!/bin/sh\necho agent\n")
	sum := sha256.Sum256(bin)
	sums := hex.EncodeToString(sum[:]) + "  srnet-agent-linux-amd64\n" + hex.EncodeToString(sum[:]) + "  srnet-agent-linux-arm64\n" + strings.Repeat("0", 64) + "  srnet-controller-linux-amd64\n"
	files := func() map[string]io.Reader {
		return map[string]io.Reader{"SHA256SUMS": strings.NewReader(sums), "srnet-agent-linux-amd64": bytes.NewReader(bin), "srnet-agent-linux-arm64": bytes.NewReader(bin)}
	}
	rel, err := r.Import("v9.9.9", files())
	if err != nil || rel.Version != "v9.9.9" || strings.Join(rel.Arches, ",") != "amd64,arm64" || rel.SHA256["amd64"] != hex.EncodeToString(sum[:]) {
		t.Fatalf("import: %+v %v", rel, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "v9.9.9", "srnet-agent-linux-arm64")); err != nil {
		t.Error("binary not stored")
	}
	if got := r.Mirrored(); len(got) != 1 || got[0].Version != "v9.9.9" {
		t.Errorf("mirrored = %+v", got)
	}
	if _, err := r.Import("", files()); err == nil {
		t.Error("missing version accepted")
	}
	f := files()
	f["srnet-agent-linux-amd64"] = strings.NewReader("tampered")
	if _, err := r.Import("v9.9.10", f); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Errorf("tampered binary: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "v9.9.10")); err == nil {
		t.Error("failed import left a release behind")
	}
	if _, err := r.Import("v9.9.11", map[string]io.Reader{"srnet-agent-linux-amd64": bytes.NewReader(bin)}); err == nil {
		t.Error("import without SHA256SUMS accepted")
	}
	if v := versionFromSums("abc  srnet-agent-v1.2.3-linux-amd64\n"); v != "v1.2.3" {
		t.Errorf("versionFromSums = %q", v)
	}
	if v := versionFromSums(sums); v != "" {
		t.Errorf("versionFromSums on plain names = %q", v)
	}
}

// The installer the controller serves must be the one in the repository.
func TestInstallScriptEmbeddedCopy(t *testing.T) {
	repo, err := os.ReadFile(filepath.Join("..", "..", "install.sh"))
	if err != nil {
		t.Skip("install.sh not beside the module")
	}
	if !bytes.Equal(repo, installScript) {
		t.Fatal("internal/controller/assets/install.sh differs from install.sh: run cp install.sh internal/controller/assets/install.sh")
	}
	for _, flag := range []string{"--source", "--skip-apt", "fetch_from_controller", "/dl/$VERSION/SHA256SUMS"} {
		if !bytes.Contains(installScript, []byte(flag)) {
			t.Errorf("install.sh lacks %s", flag)
		}
	}
}

func TestInstallScriptAndDownloads(t *testing.T) {
	s, srv := testServer(t)
	s.rel = newReleases(t.TempDir(), nil, s.hub, nil)
	r, _ := http.Get(srv.URL + "/install.sh")
	b, _ := io.ReadAll(r.Body)
	if r.StatusCode != 200 || !bytes.HasPrefix(b, []byte("#!/")) {
		t.Fatalf("install.sh: %d %.20q", r.StatusCode, b)
	}
	if r = doReq(t, "GET", srv.URL+"/dl/latest/SHA256SUMS", "", ""); r.StatusCode != 401 {
		t.Errorf("download without a token: %d", r.StatusCode)
	}
	tok := "srnet_" + randomToken(30)
	s.acc.tokens[hashSecret(tok)] = api.APIToken{ID: 1, Name: "ci", Role: "viewer"}
	if r = doReq(t, "GET", srv.URL+"/dl/latest/SHA256SUMS", tok, ""); r.StatusCode != 404 {
		t.Errorf("download with nothing mirrored: %d", r.StatusCode)
	}
	bin := []byte("agent")
	sum := sha256.Sum256(bin)
	s.rel.Import("v1.0.0", map[string]io.Reader{"SHA256SUMS": strings.NewReader(hex.EncodeToString(sum[:]) + "  srnet-agent-linux-amd64\n"), "srnet-agent-linux-amd64": bytes.NewReader(bin)})
	r = doReq(t, "GET", srv.URL+"/dl/latest/srnet-agent-linux-amd64", tok, "")
	b, _ = io.ReadAll(r.Body)
	if r.StatusCode != 200 || !bytes.Equal(b, bin) {
		t.Errorf("download latest: %d %q", r.StatusCode, b)
	}
	if r = doReq(t, "GET", srv.URL+"/dl/v1.0.0/srnet-agent-linux-arm64", tok, ""); r.StatusCode != 404 {
		t.Errorf("missing arch: %d", r.StatusCode)
	}
	if r = doReq(t, "GET", srv.URL+"/dl/v1.0.0/..%2F..%2Fetc%2Fpasswd", tok, ""); r.StatusCode != 400 && r.StatusCode != 404 {
		t.Errorf("traversal: %d", r.StatusCode)
	}
	// A host in an enrolment subnet needs no token at all.
	s.mu.Lock()
	s.enrol = EnrolSettings{CIDRs: []string{"127.0.0.0/8"}}
	s.mu.Unlock()
	if r = doReq(t, "GET", srv.URL+"/dl/latest/SHA256SUMS", "", ""); r.StatusCode != 200 {
		t.Errorf("download from an enrolment subnet: %d", r.StatusCode)
	}
}

func TestUpdateSettingsDefault(t *testing.T) {
	s, _ := testServer(t)
	if p := s.updateSettings().Parallel; p != 1 {
		t.Errorf("default parallelism = %d", p)
	}
	s.mu.Lock()
	s.updates = UpdateSettings{Parallel: 50}
	s.mu.Unlock()
	if p := s.updateSettings().Parallel; p != 20 {
		t.Errorf("parallelism should be capped at 20, got %d", p)
	}
}
