package controller

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Realishbird/SR-Net-Speed-Test/internal/api"
)

// Phase 8: scale and air gaps.
//
//   - Token-less enrolment: hosts from listed subnets may enrol without an
//     install token, either approved at once or parked on the Hosts page
//     until an admin approves them. Hundreds of hosts then install with one
//     fixed command (cloud-init, Ansible, a PXE post-script).
//   - Air-gapped installs: the controller serves install.sh and mirrored
//     agent releases itself, and a release can be uploaded from a file when
//     the controller cannot reach GitHub either.
//   - Fleet updates run several hosts at a time.
//   - Inter-switch links from the switches' own LLDP tables, for leaf/spine.

const schemaScale = `
ALTER TABLE agents ADD COLUMN IF NOT EXISTS approved BOOLEAN NOT NULL DEFAULT true;
`

//go:embed assets/install.sh
var installScript []byte

// EnrolSettings is the "enrol" setting.
type EnrolSettings struct {
	CIDRs       []string `json:"cidrs"`        // subnets whose hosts may enrol without a token
	AutoApprove bool     `json:"auto_approve"` // false: such hosts wait for approval on the Hosts page
}

// UpdateSettings is the "updates" setting.
type UpdateSettings struct {
	Parallel int `json:"parallel"` // hosts updated at once (1..20)
}

func (s *Server) loadScale(ctx context.Context) {
	if s.store == nil {
		return
	}
	var e EnrolSettings
	s.store.Setting(ctx, "enrol", &e)
	var u UpdateSettings
	s.store.Setting(ctx, "updates", &u)
	s.mu.Lock()
	s.enrol, s.updates = e, u
	s.mu.Unlock()
}

func (s *Server) enrolSettings() EnrolSettings {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.enrol
	if e.CIDRs == nil {
		e.CIDRs = []string{}
	}
	return e
}

// enrolAllowed says whether an address may enrol without a token.
func enrolAllowed(e EnrolSettings, ip string) bool {
	addr := net.ParseIP(ip)
	if addr == nil {
		return false
	}
	for _, c := range e.CIDRs {
		if _, n, err := net.ParseCIDR(strings.TrimSpace(c)); err == nil && n.Contains(addr) {
			return true
		}
	}
	return false
}

func (s *Store) SetApproved(ctx context.Context, id int64, on bool) error {
	tag, err := s.db.Exec(ctx, `UPDATE agents SET approved=$2 WHERE id=$1`, id, on)
	if err == nil && tag.RowsAffected() == 0 {
		err = errors.New("no such host")
	}
	return err
}

// TokenValid says an install token exists, is unexpired and has uses left,
// without spending one; the download endpoint uses it.
func (s *Store) TokenValid(ctx context.Context, hash string) bool {
	var n int
	err := s.db.QueryRow(ctx, `SELECT count(*) FROM enroll_tokens WHERE token_hash=$1 AND expires_at > now() AND uses_left > 0`, hash).Scan(&n)
	return err == nil && n > 0
}

// Routes.

func (s *Server) routesScale(mux *http.ServeMux) {
	mux.HandleFunc("GET /install.sh", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
		w.Write(installScript)
	})
	mux.HandleFunc("GET /dl/{version}/{file}", s.download)
	mux.HandleFunc("GET /api/v1/enrol", s.admin(func(w http.ResponseWriter, r *http.Request) { writeJSON(w, s.enrolSettings()) }))
	mux.HandleFunc("PUT /api/v1/enrol", s.admin(s.putEnrol))
	mux.HandleFunc("POST /api/v1/hosts/{id}/approve", s.admin(s.approveHost))
	mux.HandleFunc("GET /api/v1/updates/settings", s.admin(func(w http.ResponseWriter, r *http.Request) { writeJSON(w, s.updateSettings()) }))
	mux.HandleFunc("PUT /api/v1/updates/settings", s.admin(s.putUpdateSettings))
	mux.HandleFunc("POST /api/v1/releases/upload", s.admin(s.uploadRelease))
	mux.HandleFunc("GET /api/v1/install-commands", s.admin(s.installCommands))
}

func (s *Server) putEnrol(w http.ResponseWriter, r *http.Request) {
	var e EnrolSettings
	if !readJSON(w, r, &e) {
		return
	}
	e.CIDRs = cleanList(e.CIDRs)
	for _, c := range e.CIDRs {
		if _, _, err := net.ParseCIDR(c); err != nil {
			httpError(w, http.StatusBadRequest, fmt.Sprintf("%q is not a subnet (use 172.27.0.0/16)", c))
			return
		}
	}
	if err := s.store.SaveSetting(r.Context(), "enrol", e); err != nil {
		serverError(w, err)
		return
	}
	s.store.Audit(r.Context(), "enrol.settings", map[string]any{"cidrs": e.CIDRs, "auto_approve": e.AutoApprove, "by": who(r).Name})
	s.mu.Lock()
	s.enrol = e
	s.mu.Unlock()
	writeJSON(w, s.enrolSettings())
}

func (s *Server) approveHost(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := s.store.SetApproved(r.Context(), id, true); err != nil {
		httpError(w, http.StatusNotFound, err.Error())
		return
	}
	s.store.Audit(r.Context(), "host.approve", map[string]any{"id": id, "by": who(r).Name})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) updateSettings() UpdateSettings {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.updates
	if u.Parallel < 1 {
		u.Parallel = 1
	}
	if u.Parallel > 20 {
		u.Parallel = 20
	}
	return u
}

func (s *Server) putUpdateSettings(w http.ResponseWriter, r *http.Request) {
	var u UpdateSettings
	if !readJSON(w, r, &u) {
		return
	}
	if u.Parallel < 1 || u.Parallel > 20 {
		httpError(w, http.StatusBadRequest, "update between 1 and 20 hosts at a time")
		return
	}
	if err := s.store.SaveSetting(r.Context(), "updates", u); err != nil {
		serverError(w, err)
		return
	}
	s.mu.Lock()
	s.updates = u
	s.mu.Unlock()
	writeJSON(w, u)
}

// download serves a mirrored agent binary or SHA256SUMS to install.sh on a
// host that cannot reach GitHub. It takes an unspent install token, the
// same one the enrolment that follows will use; "latest" is the newest
// mirrored release.
func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if tok == "" {
		tok = r.URL.Query().Get("token")
	}
	allowed := tok != "" && s.store != nil && s.store.TokenValid(r.Context(), hashSecret(tok))
	if !allowed {
		if p, ok := s.authenticate(r); ok && p.Role != "" {
			allowed = true
		}
	}
	if !allowed && !enrolAllowed(s.enrolSettings(), clientIP(r)) {
		httpError(w, http.StatusUnauthorized, "an install token (or an address in an enrolment subnet) is needed to download agent builds")
		return
	}
	ver, file := r.PathValue("version"), r.PathValue("file")
	if ver == "latest" {
		rels := s.rel.Mirrored()
		if len(rels) == 0 {
			httpError(w, http.StatusNotFound, "no release is mirrored on this controller yet (Fleet page: Mirror a release, or Upload release)")
			return
		}
		ver = rels[0].Version
	}
	if !versionRe.MatchString(ver) || (file != "SHA256SUMS" && !strings.HasPrefix(file, "srnet-agent-linux-")) || strings.ContainsAny(file, "/\\") {
		httpError(w, http.StatusBadRequest, "bad download path")
		return
	}
	path := filepath.Join(s.rel.dir, ver, file)
	if _, err := os.Stat(path); err != nil {
		httpError(w, http.StatusNotFound, fmt.Sprintf("%s of %s is not mirrored here", file, ver))
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeFile(w, r, path)
}

// uploadRelease takes the files of a GitHub release (SHA256SUMS and the
// agent binaries) as a multipart form, for a controller with no internet.
func (s *Server) uploadRelease(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(256 << 20); err != nil {
		httpError(w, http.StatusBadRequest, "expected a multipart form with SHA256SUMS and srnet-agent-linux-* files: "+err.Error())
		return
	}
	version := strings.TrimSpace(r.FormValue("version"))
	files := map[string]io.Reader{}
	var closers []io.Closer
	defer func() {
		for _, c := range closers {
			c.Close()
		}
	}()
	for _, fhs := range r.MultipartForm.File {
		for _, fh := range fhs {
			f, err := fh.Open()
			if err != nil {
				serverError(w, err)
				return
			}
			closers = append(closers, f)
			files[filepath.Base(fh.Filename)] = f
		}
	}
	rel, err := s.rel.Import(version, files)
	if err != nil {
		if userErr(w, err) {
			return
		}
	}
	s.store.Audit(r.Context(), "release.upload", map[string]any{"version": rel.Version, "arches": rel.Arches, "by": who(r).Name})
	writeJSON(w, rel)
}

// Import stores release files from readers, verifying each binary against
// SHA256SUMS exactly as Fetch does for GitHub downloads.
func (r *releases) Import(version string, files map[string]io.Reader) (api.Release, error) {
	sums, ok := files["SHA256SUMS"]
	if !ok {
		return api.Release{}, &userError{errors.New("SHA256SUMS is missing from the upload")}
	}
	sumsB, err := io.ReadAll(io.LimitReader(sums, 1<<20))
	if err != nil {
		return api.Release{}, err
	}
	if version == "" {
		version = versionFromSums(string(sumsB))
	}
	if !versionRe.MatchString(version) {
		return api.Release{}, &userError{fmt.Errorf("give the release version (v1.2.3), it is not in the files")}
	}
	dir := filepath.Join(r.dir, version)
	tmp := dir + ".tmp"
	os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return api.Release{}, err
	}
	defer os.RemoveAll(tmp)
	if err := os.WriteFile(filepath.Join(tmp, "SHA256SUMS"), sumsB, 0o644); err != nil {
		return api.Release{}, err
	}
	rel := api.Release{Version: version, SHA256: map[string]string{}, Mirrored: time.Now()}
	for _, arch := range []string{"amd64", "arm64"} {
		name := "srnet-agent-linux-" + arch
		src, ok := files[name]
		if !ok {
			continue
		}
		want := readSum(filepath.Join(tmp, "SHA256SUMS"), name)
		if want == "" {
			return api.Release{}, &userError{fmt.Errorf("%s is not listed in SHA256SUMS", name)}
		}
		f, err := os.OpenFile(filepath.Join(tmp, name), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return api.Release{}, err
		}
		_, err = io.Copy(f, io.LimitReader(src, 512<<20))
		f.Close()
		if err != nil {
			return api.Release{}, err
		}
		got, err := fileSHA256(filepath.Join(tmp, name))
		if err != nil {
			return api.Release{}, err
		}
		if got != want {
			return api.Release{}, &userError{fmt.Errorf("%s: checksum mismatch against SHA256SUMS", name)}
		}
		rel.Arches = append(rel.Arches, arch)
		rel.SHA256[arch] = want
	}
	if len(rel.Arches) == 0 {
		return api.Release{}, &userError{errors.New("no srnet-agent-linux-amd64 or -arm64 file in the upload")}
	}
	os.RemoveAll(dir)
	if err := os.Rename(tmp, dir); err != nil {
		return api.Release{}, err
	}
	log.Printf("release %s imported from upload (%s)", version, strings.Join(rel.Arches, ", "))
	return rel, nil
}

// versionFromSums finds a version in a SHA256SUMS file whose entries carry
// one (srnet-controller-linux-amd64 has none, so this usually fails and the
// form's version field is used).
func versionFromSums(s string) string {
	return sumsVersionRe.FindString(s)
}

var sumsVersionRe = regexp.MustCompile(`\bv\d+\.\d+\.\d+\b`)

// installCommands renders the ways to install an agent: the usual command
// with a fresh token (script from GitHub), the same with the script and the
// binaries served by this controller for hosts without internet, and the
// token-less one for hosts in an enrolment subnet.
func (s *Server) installCommands(w http.ResponseWriter, r *http.Request) {
	hosts := tokenHosts(r.URL.Query().Get("hosts"))
	tok := randomToken(24)
	exp := time.Now().Add(time.Hour)
	if err := s.store.CreateToken(r.Context(), hashSecret(tok), exp, hosts); err != nil {
		serverError(w, err)
		return
	}
	fp, err := CAFingerprint(s.cfg.CACert)
	if err != nil {
		serverError(w, err)
		return
	}
	base := strings.TrimRight(s.cfg.PublicURL, "/")
	e := s.enrolSettings()
	out := map[string]any{
		"token": tok, "expires_at": exp, "hosts": hosts, "ca_sha256": fp,
		"github":      fmt.Sprintf("curl -fsSL %s | sudo bash -s -- --controller %s --token %s --ca-sha256 %s", s.cfg.InstallScriptURL, base, tok, fp),
		"offline":     fmt.Sprintf("curl -fsSLk %s/install.sh | sudo bash -s -- --controller %s --token %s --ca-sha256 %s --source controller", base, base, tok, fp),
		"mirrored":    len(s.rel.Mirrored()) > 0,
		"enrol_cidrs": e.CIDRs, "auto_approve": e.AutoApprove,
	}
	if len(e.CIDRs) > 0 {
		out["tokenless"] = fmt.Sprintf("curl -fsSL %s | sudo bash -s -- --controller %s --ca-sha256 %s", s.cfg.InstallScriptURL, base, fp)
	}
	s.store.Audit(r.Context(), "token.create", map[string]any{"expires_at": exp, "hosts": hosts})
	writeJSON(w, out)
}

// switchLinks finds cables between configured switches in the switches'
// own LLDP tables: a neighbour whose system name is another switch we know.
func switchLinks(lldp map[string]lldpEntry, switches []SwitchConfig) []api.TopoLink {
	seen := map[string]bool{}
	// The table's keys are lower-cased; the switches' own port names keep
	// their case in the values, so a port seen from the far end is spelled
	// from there when the far end reported the same cable.
	cased := map[string]string{}
	for _, e := range lldp {
		cased[strings.ToLower(e.Switch+"|"+e.Port)] = e.Port
	}
	var out []api.TopoLink
	for key, e := range lldp {
		name, port, ok := strings.Cut(key, "|")
		if !ok {
			continue
		}
		peer := resolveSwitchName(switches, name)
		if peer == "" || strings.EqualFold(peer, e.Switch) || len(switches) < 2 {
			continue
		}
		if p, ok := cased[strings.ToLower(peer+"|"+port)]; ok {
			port = p
		} else if strings.HasPrefix(port, "ethernet") {
			port = "Ethernet" + port[len("ethernet"):]
		}
		a, ap, b, bp := e.Switch, e.Port, peer, port
		if strings.ToLower(a) > strings.ToLower(b) {
			a, ap, b, bp = b, bp, a, ap
		}
		k := strings.ToLower(a + "/" + ap + ">" + b + "/" + bp)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, api.TopoLink{A: a, APort: ap, B: b, BPort: bp})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].A != out[j].A {
			return out[i].A < out[j].A
		}
		return portLess(out[i].APort, out[j].APort)
	})
	return out
}
