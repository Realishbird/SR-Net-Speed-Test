package controller

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Realishbird/SR-Net-Speed-Test/internal/api"
)

const githubRepo = "Realishbird/SR-Net-Speed-Test"

var versionRe = regexp.MustCompile(`^v\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`)

// releases mirrors agent builds from GitHub Releases under
// <DataDir>/releases/<version>/ and runs fleet update jobs one host at a time.
type releases struct {
	dir   string
	store *Store
	hub   *hub
	al    *alerter
	http  *http.Client

	mu       sync.Mutex
	versions map[int64]string      // agent id -> last reported version
	seen     map[int64]chan string // agent id -> waiter for a new version report
	job      *api.UpdateJob
}

func newReleases(dir string, s *Store, h *hub, al *alerter) *releases {
	return &releases{dir: dir, store: s, hub: h, al: al, http: &http.Client{Timeout: 5 * time.Minute},
		versions: map[int64]string{}, seen: map[int64]chan string{}}
}

// noteVersion is called from every inventory report.
func (r *releases) noteVersion(agentID int64, v string) {
	r.mu.Lock()
	r.versions[agentID] = v
	if c, ok := r.seen[agentID]; ok {
		select {
		case c <- v:
		default:
		}
	}
	r.mu.Unlock()
}

// Mirrored lists the versions on disk.
func (r *releases) Mirrored() []api.Release {
	entries, _ := os.ReadDir(r.dir)
	var out []api.Release
	for _, e := range entries {
		if !e.IsDir() || !versionRe.MatchString(e.Name()) {
			continue
		}
		rel := api.Release{Version: e.Name(), SHA256: map[string]string{}}
		info, _ := e.Info()
		if info != nil {
			rel.Mirrored = info.ModTime()
		}
		for _, arch := range []string{"amd64", "arm64"} {
			if sum := readSum(filepath.Join(r.dir, e.Name(), "SHA256SUMS"), "srnet-agent-linux-"+arch); sum != "" {
				if _, err := os.Stat(filepath.Join(r.dir, e.Name(), "srnet-agent-linux-"+arch)); err == nil {
					rel.Arches = append(rel.Arches, arch)
					rel.SHA256[arch] = sum
				}
			}
		}
		if len(rel.Arches) > 0 {
			out = append(out, rel)
		}
	}
	sort.Slice(out, func(i, j int) bool { return versionLess(out[j].Version, out[i].Version) })
	return out
}

// versionLess orders release tags numerically, so v0.1.9 sorts before
// v0.1.18 (as text it would not). A pre-release suffix sorts before the
// plain version; unparseable tags fall back to text order.
func versionLess(a, b string) bool {
	pa, sa := versionParts(a)
	pb, sb := versionParts(b)
	for i := range pa {
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	if (sa == "") != (sb == "") {
		return sa != ""
	}
	return a < b
}

func versionParts(v string) ([3]int, string) {
	var n [3]int
	v = strings.TrimPrefix(v, "v")
	v, suffix, _ := strings.Cut(v, "-")
	for i, p := range strings.SplitN(v, ".", 3) {
		if i < 3 {
			fmt.Sscanf(p, "%d", &n[i])
		}
	}
	return n, suffix
}

func readSum(sumsPath, name string) string {
	f, err := os.Open(sumsPath)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == name {
			return fields[0]
		}
	}
	return ""
}

// LatestOnGitHub asks the GitHub API for the newest release tag.
func (r *releases) LatestOnGitHub(ctx context.Context) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://api.github.com/repos/"+githubRepo+"/releases/latest", nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := r.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("github: %s", resp.Status)
	}
	var rel struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		return "", err
	}
	return rel.TagName, nil
}

// Fetch downloads a release's agent binaries and checksums into the mirror,
// verifying each binary against SHA256SUMS before keeping it.
func (r *releases) Fetch(ctx context.Context, version string) (api.Release, error) {
	if !versionRe.MatchString(version) {
		return api.Release{}, &userError{fmt.Errorf("version must look like v1.2.3, not %q", version)}
	}
	base := "https://github.com/" + githubRepo + "/releases/download/" + version + "/"
	dir := filepath.Join(r.dir, version)
	tmp := dir + ".tmp"
	os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return api.Release{}, err
	}
	defer os.RemoveAll(tmp)
	get := func(name string) error {
		req, _ := http.NewRequestWithContext(ctx, "GET", base+name, nil)
		resp, err := r.http.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return fmt.Errorf("%s: %s", name, resp.Status)
		}
		f, err := os.OpenFile(filepath.Join(tmp, name), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(f, resp.Body)
		return err
	}
	if err := get("SHA256SUMS"); err != nil {
		return api.Release{}, &userError{fmt.Errorf("release %s not found on GitHub (%v)", version, err)}
	}
	rel := api.Release{Version: version, SHA256: map[string]string{}, Mirrored: time.Now()}
	for _, arch := range []string{"amd64", "arm64"} {
		name := "srnet-agent-linux-" + arch
		want := readSum(filepath.Join(tmp, "SHA256SUMS"), name)
		if want == "" {
			continue
		}
		if err := get(name); err != nil {
			return api.Release{}, err
		}
		got, err := fileSHA256(filepath.Join(tmp, name))
		if err != nil {
			return api.Release{}, err
		}
		if got != want {
			return api.Release{}, fmt.Errorf("%s: checksum mismatch", name)
		}
		rel.Arches = append(rel.Arches, arch)
		rel.SHA256[arch] = want
	}
	if len(rel.Arches) == 0 {
		return api.Release{}, &userError{errors.New("release has no agent binaries")}
	}
	os.RemoveAll(dir)
	if err := os.Rename(tmp, dir); err != nil {
		return api.Release{}, err
	}
	return rel, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// BinaryPath is the file served to agents for a version and arch.
func (r *releases) BinaryPath(version, arch string) (string, string, bool) {
	if !versionRe.MatchString(version) || (arch != "amd64" && arch != "arm64") {
		return "", "", false
	}
	p := filepath.Join(r.dir, version, "srnet-agent-linux-"+arch)
	sum := readSum(filepath.Join(r.dir, version, "SHA256SUMS"), "srnet-agent-linux-"+arch)
	if _, err := os.Stat(p); err != nil || sum == "" {
		return "", "", false
	}
	return p, sum, true
}

// StartJob updates the given hosts to version, one at a time, in the background.
func (r *releases) StartJob(ctx context.Context, version string, hosts []hostRec, o *Orchestrator) (*api.UpdateJob, error) {
	r.mu.Lock()
	if r.job != nil && r.job.Status == "running" {
		r.mu.Unlock()
		return nil, &userError{fmt.Errorf("update job %d is still running", r.job.ID)}
	}
	r.mu.Unlock()
	rel := r.find(version)
	if rel == nil {
		return nil, &userError{fmt.Errorf("version %s is not mirrored; fetch it first", version)}
	}
	job := &api.UpdateJob{Version: version, Status: "running"}
	for _, h := range hosts {
		st := api.UpdateHostStatus{HostID: h.ID, Name: h.Name, From: h.AgentVersion, Status: "pending"}
		switch {
		case h.AgentVersion == version:
			st.Status, st.Note = "skipped", "already on "+version
		case !r.hub.online(h.ID):
			st.Status, st.Note = "skipped", "offline"
		case rel.SHA256[h.Arch] == "":
			st.Status, st.Note = "skipped", "no "+h.Arch+" build in "+version
		}
		job.Hosts = append(job.Hosts, st)
	}
	if err := r.store.CreateUpdateJob(ctx, job); err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.job = job
	r.mu.Unlock()
	go r.runJob(job, hosts, rel, o)
	return job, nil
}

func (r *releases) find(version string) *api.Release {
	for _, rel := range r.Mirrored() {
		if rel.Version == version {
			return &rel
		}
	}
	return nil
}

func (r *releases) runJob(job *api.UpdateJob, hosts []hostRec, rel *api.Release, o *Orchestrator) {
	ctx := context.Background()
	byID := map[int64]hostRec{}
	for _, h := range hosts {
		byID[h.ID] = h
	}
	failed := 0
	for i := range job.Hosts {
		st := &job.Hosts[i]
		if st.Status != "pending" {
			continue
		}
		h := byID[st.HostID]
		st.Status = "updating"
		r.store.SaveUpdateJob(ctx, job)
		if err := r.updateOne(ctx, h, rel, o); err != nil {
			st.Status, st.Note = "failed", err.Error()
			failed++
			log.Printf("update %s to %s: %v", h.Name, rel.Version, err)
			r.al.open(ctx, api.Alert{Kind: "update_failed", Host: h.Name, Message: fmt.Sprintf("update of %s to %s failed: %v", h.Name, rel.Version, err)})
		} else {
			st.Status = "done"
			log.Printf("update %s to %s: done", h.Name, rel.Version)
			r.al.resolve(ctx, "update_failed", h.Name, "")
		}
		r.store.SaveUpdateJob(ctx, job)
	}
	now := time.Now()
	job.EndedAt = &now
	job.Status = "done"
	if failed > 0 {
		job.Status = "failed"
	}
	r.store.SaveUpdateJob(ctx, job)
}

func (r *releases) updateOne(ctx context.Context, h hostRec, rel *api.Release, o *Orchestrator) error {
	// Never interrupt a test: wait up to 10 minutes for the host's ports to be free.
	deadline := time.Now().Add(10 * time.Minute)
	for o.hostBusy(h.ID) {
		if time.Now().After(deadline) {
			return errors.New("host stayed busy with tests for 10 minutes")
		}
		time.Sleep(5 * time.Second)
	}
	inbox := make(chan msg, 4)
	t := api.Task{ID: newID(), Kind: api.TaskUpdate, Version: rel.Version, SHA256: rel.SHA256[h.Arch],
		URLPath: "/api/v1/agent/release/" + rel.Version + "/" + h.Arch}
	defer r.hub.forget(t.ID)
	wait := make(chan string, 1)
	r.mu.Lock()
	r.seen[h.ID] = wait
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.seen, h.ID)
		r.mu.Unlock()
	}()
	r.hub.send(h.ID, t, inbox)

	select {
	case <-time.After(3 * time.Minute):
		return errors.New("agent did not acknowledge the update within 3 minutes")
	case m := <-inbox:
		if m.event == nil || m.event.Type != api.EventUpdateApplied {
			msg := "agent refused the update"
			if m.event != nil && m.event.Message != "" {
				msg = m.event.Message
			}
			return errors.New(msg)
		}
	}
	// The agent restarts through the launcher; a healthy new binary reports its version within a minute.
	timeout := time.After(90 * time.Second)
	for {
		select {
		case <-timeout:
			return errors.New("agent did not come back on the new version within 90 s (launcher rolls back on the next restart)")
		case v := <-wait:
			if v == rel.Version {
				return nil
			}
		}
	}
}

func (r *releases) CurrentJob() *api.UpdateJob {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.job
}
