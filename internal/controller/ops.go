package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Realishbird/SR-Net-Speed-Test/internal/api"
	"github.com/Realishbird/SR-Net-Speed-Test/internal/version"
)

// Phase 7 operations: backup and restore of everything an operator typed
// in, a daily backup file, and the acceptance report for a suite run.

// Backup is the portable form of the controller's configuration. Results
// and telemetry are not in it: they live in Postgres and VictoriaMetrics,
// which pg_dump and the VictoriaMetrics snapshot API cover; runs can be
// included for reference with ?runs=1 but are not restored.
type Backup struct {
	Format     int                        `json:"format"`
	CreatedAt  time.Time                  `json:"created_at"`
	Controller string                     `json:"controller"`
	PublicURL  string                     `json:"public_url"`
	Hosts      []BackupHost               `json:"hosts"`
	Fabrics    []api.FabricProfile        `json:"fabrics"`
	Suites     []api.Suite                `json:"suites"`
	Schedules  []api.Schedule             `json:"schedules"`
	Switches   []SwitchConfig             `json:"switches"`
	Settings   map[string]json.RawMessage `json:"settings"`
	APITokens  []BackupToken              `json:"api_tokens"`
	Runs       []api.Run                  `json:"runs,omitempty"`
}

type BackupHost struct {
	UID      string       `json:"uid"`
	Name     string       `json:"name"`
	Hostname string       `json:"hostname"`
	Tags     []string     `json:"tags"`
	Site     string       `json:"site"`
	Rack     string       `json:"rack"`
	Approved bool         `json:"approved"`
	Ports    []BackupPort `json:"ports"`
}

type BackupPort struct {
	RdmaDev      string  `json:"rdma_dev"`
	PortNum      int     `json:"port_num"`
	Netdev       string  `json:"netdev"`
	Label        string  `json:"label"`
	Fabric       string  `json:"fabric"`
	ExpectedGbps float64 `json:"expected_gbps"`
}

type BackupToken struct {
	Name      string    `json:"name"`
	Role      string    `json:"role"`
	Prefix    string    `json:"prefix"`
	TokenHash string    `json:"token_hash"`
	CreatedAt time.Time `json:"created_at"`
}

// RestoreResult counts what a restore applied.
type RestoreResult struct {
	Hosts, Ports, Fabrics, Suites, Schedules, Switches, Settings, Tokens int
	Problems                                                             []string `json:"problems"`
}

func (s *Server) makeBackup(ctx context.Context, withRuns bool) (Backup, error) {
	b := Backup{Format: 1, CreatedAt: time.Now(), Controller: version.Version, PublicURL: s.cfg.PublicURL, Settings: map[string]json.RawMessage{}}
	hosts, err := s.store.Hosts(ctx)
	if err != nil {
		return b, err
	}
	b.Hosts = []BackupHost{}
	for _, h := range hosts {
		bh := BackupHost{UID: h.UID, Name: h.Name, Hostname: h.Hostname, Tags: h.Tags, Site: h.Site, Rack: h.Rack, Approved: h.Approved, Ports: []BackupPort{}}
		for _, p := range h.Ports {
			bh.Ports = append(bh.Ports, BackupPort{RdmaDev: p.RdmaDev, PortNum: p.PortNum, Netdev: p.Netdev, Label: p.Label, Fabric: p.Fabric, ExpectedGbps: p.ExpectedGbps})
		}
		b.Hosts = append(b.Hosts, bh)
	}
	if b.Fabrics, err = s.store.FabricProfiles(ctx); err != nil {
		return b, err
	}
	if b.Suites, err = s.store.Suites(ctx); err != nil {
		return b, err
	}
	if b.Schedules, err = s.store.Schedules(ctx); err != nil {
		return b, err
	}
	b.Switches = s.switchList()
	if b.Settings, err = s.store.Settings(ctx); err != nil {
		return b, err
	}
	if b.APITokens, err = s.store.backupTokens(ctx); err != nil {
		return b, err
	}
	if withRuns {
		if b.Runs, err = s.store.Runs(ctx, 1000); err != nil {
			return b, err
		}
	}
	return b, nil
}

func (s *Store) backupTokens(ctx context.Context) ([]BackupToken, error) {
	rows, err := s.db.Query(ctx, `SELECT name, role, prefix, token_hash, created_at FROM api_tokens ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BackupToken{}
	for rows.Next() {
		var t BackupToken
		if err := rows.Scan(&t.Name, &t.Role, &t.Prefix, &t.TokenHash, &t.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) restoreToken(ctx context.Context, t BackupToken) (bool, error) {
	tag, err := s.db.Exec(ctx, `INSERT INTO api_tokens (name, role, prefix, token_hash, created_at) VALUES ($1,$2,$3,$4,$5) ON CONFLICT (token_hash) DO NOTHING`,
		t.Name, t.Role, t.Prefix, t.TokenHash, t.CreatedAt)
	return err == nil && tag.RowsAffected() > 0, err
}

// restore applies a backup on top of what exists: hosts are matched by uid
// then hostname, ports by RDMA device and port number, everything else by
// name or key. Nothing is deleted.
func (s *Server) restore(ctx context.Context, b Backup) (RestoreResult, error) {
	var res RestoreResult
	res.Problems = []string{}
	if b.Format != 1 {
		return res, &userError{fmt.Errorf("unknown backup format %d", b.Format)}
	}
	hosts, err := s.store.Hosts(ctx)
	if err != nil {
		return res, err
	}
	byUID, byHostname := map[string]hostRec{}, map[string]hostRec{}
	for _, h := range hosts {
		byUID[h.UID] = h
		byHostname[strings.ToLower(h.Hostname)] = h
	}
	for _, bh := range b.Hosts {
		h, ok := byUID[bh.UID]
		if !ok {
			h, ok = byHostname[strings.ToLower(bh.Hostname)]
		}
		if !ok {
			res.Problems = append(res.Problems, fmt.Sprintf("host %s (%s) is not enrolled here; its names were skipped", bh.Name, bh.Hostname))
			continue
		}
		tags := bh.Tags
		if tags == nil {
			tags = []string{}
		}
		if err := s.store.UpdateHost(ctx, h.ID, api.HostUpdate{Name: &bh.Name, Tags: &tags, Site: &bh.Site, Rack: &bh.Rack}); err != nil {
			return res, err
		}
		if bh.Approved && !h.Approved {
			s.store.SetApproved(ctx, h.ID, true)
		}
		res.Hosts++
		for _, bp := range bh.Ports {
			for _, p := range h.Ports {
				if p.RdmaDev == bp.RdmaDev && p.PortNum == bp.PortNum {
					if err := s.store.UpdatePort(ctx, p.ID, api.PortUpdate{Label: &bp.Label, Fabric: &bp.Fabric, ExpectedGbps: &bp.ExpectedGbps}); err != nil {
						return res, err
					}
					res.Ports++
				}
			}
		}
	}
	for _, f := range b.Fabrics {
		if err := s.store.SaveFabricProfile(ctx, f); err != nil {
			return res, err
		}
		res.Fabrics++
	}
	suites, _ := s.store.Suites(ctx)
	suiteID := map[string]int64{}
	for _, su := range suites {
		suiteID[su.Name] = su.ID
	}
	for _, su := range b.Suites {
		su.ID = suiteID[su.Name]
		if err := s.store.SaveSuite(ctx, &su); err != nil {
			return res, err
		}
		suiteID[su.Name] = su.ID
		res.Suites++
	}
	scheds, _ := s.store.Schedules(ctx)
	schedID := map[string]int64{}
	for _, sc := range scheds {
		schedID[sc.Name] = sc.ID
	}
	for _, sc := range b.Schedules {
		id, ok := suiteID[sc.SuiteName]
		if !ok {
			res.Problems = append(res.Problems, fmt.Sprintf("schedule %s: suite %q not found", sc.Name, sc.SuiteName))
			continue
		}
		sc.SuiteID, sc.ID = id, schedID[sc.Name]
		sc.LastRunAt, sc.NextRunAt = nil, nil
		if err := s.store.SaveSchedule(ctx, &sc); err != nil {
			return res, err
		}
		res.Schedules++
	}
	if len(b.Switches) > 0 {
		list := s.switchList()
		for _, sw := range b.Switches {
			replaced := false
			for i := range list {
				if strings.EqualFold(list[i].Name, sw.Name) {
					list[i], replaced = sw, true
				}
			}
			if !replaced {
				list = append(list, sw)
			}
			res.Switches++
		}
		if err := s.applySwitches(ctx, list, "switch.restore", map[string]any{"count": len(b.Switches)}); err != nil {
			return res, err
		}
	}
	for k, v := range b.Settings {
		var any json.RawMessage = v
		if err := s.store.SaveSetting(ctx, k, any); err != nil {
			return res, err
		}
		res.Settings++
	}
	for _, t := range b.APITokens {
		if t.TokenHash == "" {
			continue
		}
		if ok, err := s.store.restoreToken(ctx, t); err != nil {
			return res, err
		} else if ok {
			res.Tokens++
		}
	}
	s.loadAccess(ctx)
	s.notify.load(ctx)
	s.loadScale(ctx)
	return res, nil
}

// backupLoop writes a backup file once a day into <DataDir>/backups and
// keeps the newest 14, so a lost or mistyped setting can be put back from
// yesterday without a database restore.
func (s *Server) backupLoop(ctx context.Context) {
	dir := filepath.Join(s.cfg.DataDir, "backups")
	for {
		s.dailyBackup(ctx, dir)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Hour):
		}
	}
}

func (s *Server) dailyBackup(ctx context.Context, dir string) {
	var bs struct {
		Daily *bool `json:"daily"`
		Keep  int   `json:"keep"`
	}
	s.store.Setting(ctx, "backup", &bs)
	if bs.Daily != nil && !*bs.Daily {
		return
	}
	if bs.Keep <= 0 {
		bs.Keep = 14
	}
	name := filepath.Join(dir, "srnet-backup-"+time.Now().Format("2006-01-02")+".json")
	if _, err := os.Stat(name); err == nil {
		return
	}
	b, err := s.makeBackup(ctx, false)
	if err != nil {
		log.Printf("daily backup: %v", err)
		return
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		log.Printf("daily backup: %v", err)
		return
	}
	data, _ := json.MarshalIndent(b, "", " ")
	if err := os.WriteFile(name+".tmp", data, 0o600); err != nil {
		log.Printf("daily backup: %v", err)
		return
	}
	os.Rename(name+".tmp", name)
	log.Printf("daily backup written: %s", name)
	old, _ := filepath.Glob(filepath.Join(dir, "srnet-backup-*.json"))
	sort.Strings(old)
	for len(old) > bs.Keep {
		os.Remove(old[0])
		old = old[1:]
	}
}

// Acceptance report: one document for a suite run, with every run's figures,
// the fabric state and the fleet at the time, ready to print or send on.

type acceptance struct {
	SuiteRun   api.SuiteRun
	Runs       []api.Run
	Fabric     api.FabricCheck
	Hosts      []api.Host
	Controller string
	Generated  time.Time
	Site       string
}

func (s *Server) acceptanceReport(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	sr, err := s.store.SuiteRun(r.Context(), id)
	if err != nil {
		storeError(w, err)
		return
	}
	a := acceptance{SuiteRun: sr, Controller: version.Version, Generated: time.Now(), Site: s.cfg.PublicURL}
	for _, rid := range sr.RunIDs {
		run, err := s.store.Run(r.Context(), rid)
		if err != nil {
			continue
		}
		s.fillBaseline(r.Context(), &run)
		a.Runs = append(a.Runs, run)
	}
	a.Fabric = s.fabricCheck(r.Context())
	if hosts, err := s.store.Hosts(r.Context()); err == nil {
		s.fillLLDP(r.Context(), hosts)
		for _, h := range hosts {
			h.Online = s.hub.online(h.ID)
			a.Hosts = append(a.Hosts, h.Host)
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	WriteAcceptanceHTML(w, a)
}

func WriteAcceptanceHTML(w io.Writer, a acceptance) {
	e := html.EscapeString
	sr := a.SuiteRun
	verdict := nonEmpty(sr.Verdict, sr.Status)
	fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><title>Acceptance report: %s</title>%s</head><body>`, e(sr.SuiteName), reportCSS)
	fmt.Fprintf(w, `<h1>Acceptance report: %s</h1><div class="muted">SR-Net-Speed-Test · suite run %d · %s · generated %s · controller %s</div>`,
		e(sr.SuiteName), sr.ID, e(sr.Trigger), a.Generated.Format("2006-01-02 15:04 MST"), e(a.Controller))
	fmt.Fprintf(w, `<p style="margin-top:16px"><span class="verdict %s">%s</span></p>`, e(verdict), e(strings.ToUpper(verdict)))
	end := "still running"
	if sr.EndedAt != nil {
		end = sr.EndedAt.Local().Format("2006-01-02 15:04:05 MST")
	}
	fmt.Fprintf(w, `<div class="kv"><b>Started</b><span>%s</span><b>Ended</b><span>%s</span><b>Runs</b><span>%d</span></div>`, sr.StartedAt.Local().Format("2006-01-02 15:04:05 MST"), end, len(sr.RunIDs))

	// 1. Summary of every run.
	fmt.Fprint(w, `<h2>1. Runs</h2><table><tr><th>Run</th><th>Test</th><th>Legs</th><th class="n">Aggregate Gbps</th><th class="n">vs baseline</th><th class="n">Pass / warn / fail</th><th>Verdict</th><th>Notes</th></tr>`)
	for _, r := range a.Runs {
		pass, warn, fail := 0, 0, 0
		var notes []string
		for _, l := range r.Legs {
			switch l.Verdict {
			case "pass":
				pass++
			case "warn":
				warn++
			default:
				fail++
			}
			if l.Verdict != "pass" && l.Note != "" && len(notes) < 4 {
				notes = append(notes, fmt.Sprintf("%s > %s: %s", l.ClientHost, l.ServerHost, l.Note))
			}
		}
		agg := fmt.Sprintf("%.1f", r.Summary.ToolGbps)
		if api.IsLatency(r.Spec.Test) {
			agg = "-"
		}
		bl := "-"
		if r.Summary.BaselineRunID != 0 {
			bl = fmt.Sprintf("%+.1f%% (run %d)", r.Summary.DeltaPct, r.Summary.BaselineRunID)
		} else if r.Baseline {
			bl = "is the baseline"
		}
		if r.Error != "" {
			notes = append([]string{r.Error}, notes...)
		}
		fmt.Fprintf(w, `<tr><td>%d %s</td><td>%s%s</td><td>%d</td><td class="n">%s</td><td class="n">%s</td><td class="n">%d / %d / %d</td><td class="%s">%s</td><td class="muted">%s</td></tr>`,
			r.ID, e(r.Name), e(r.Spec.Test), optHTML(r.Spec.Bidir, " bidir"), len(r.Legs), agg, e(bl), pass, warn, fail, e(r.Verdict), e(strings.ToUpper(nonEmpty(r.Verdict, r.Status))), e(strings.Join(notes, "; ")))
	}
	fmt.Fprint(w, `</table>`)

	// 2. Fabric state.
	fmt.Fprintf(w, `<h2>2. Fabric check</h2><div class="muted">as of %s</div>`, a.Fabric.At.Format("2006-01-02 15:04 MST"))
	if a.Fabric.Warning != "" {
		fmt.Fprintf(w, `<p class="warn">%s</p>`, e(a.Fabric.Warning))
	}
	if len(a.Fabric.Profiles) > 0 {
		fmt.Fprint(w, `<table><tr><th>Fabric</th><th>Mode</th><th>Switch MTU</th><th>Host MTU</th><th>PFC priority</th><th>ECN queue</th><th>RoCE DSCP</th><th>CNP DSCP</th><th>Trust</th></tr>`)
		for _, p := range a.Fabric.Profiles {
			fmt.Fprintf(w, `<tr><td>%s</td><td>%s</td><td>%d</td><td>%d</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td></tr>`,
				e(p.Fabric), e(p.Mode), p.SwitchMTU, p.HostMTU, prioOrOff(p.PFCPriority), prioOrOff(p.ECNQueue), prioOrOff(p.RoCEDSCP), prioOrOff(p.CNPDSCP), e(nonEmpty(p.Trust, "-")))
		}
		fmt.Fprint(w, `</table>`)
	} else {
		fmt.Fprint(w, `<p class="muted">No fabric profile set; cables were checked for consistency only.</p>`)
	}
	fmt.Fprint(w, `<table><tr><th>Host</th><th>Port</th><th>IP</th><th>Fabric</th><th>Switch port</th><th>State</th><th>Findings</th></tr>`)
	for _, c := range a.Fabric.Cables {
		var bad []string
		for _, ch := range c.Checks {
			if ch.State == "bad" {
				bad = append(bad, ch.Name+": "+nonEmpty(ch.Note, "mismatch"))
			}
		}
		st, cls := "OK", "ok"
		if !c.OK {
			st, cls = "FAIL", "bad"
		}
		fmt.Fprintf(w, `<tr><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s %s</td><td class="%s">%s</td><td class="muted">%s</td></tr>`,
			e(c.Host), e(nonEmpty(c.Label, c.Netdev)), e(c.IP), e(c.Fabric), e(c.Switch), e(c.SwitchPort), cls, st, e(strings.Join(bad, "; ")))
	}
	if len(a.Fabric.Cables) == 0 {
		fmt.Fprint(w, `<tr><td colspan="7" class="muted">No cable known yet (no LLDP neighbour reported).</td></tr>`)
	}
	fmt.Fprint(w, `</table>`)

	// 3. Fleet.
	fmt.Fprint(w, `<h2>3. Hosts</h2><table><tr><th>Host</th><th>Hostname</th><th>Tags</th><th>Agent</th><th>Arch</th><th>Online</th><th>RDMA ports</th></tr>`)
	for _, h := range a.Hosts {
		var ports []string
		for _, p := range h.Ports {
			if len(p.Addrs) == 0 {
				continue
			}
			ports = append(ports, fmt.Sprintf("%s %s (%s, %d Gbps)", nonEmpty(p.Label, p.Netdev), strings.Split(p.Addrs[0], "/")[0], p.State, p.SpeedMbps/1000))
		}
		on := "yes"
		if !h.Online {
			on = "no"
		}
		fmt.Fprintf(w, `<tr><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td class="muted">%s</td></tr>`,
			e(h.Name), e(h.Hostname), e(strings.Join(h.Tags, ", ")), e(h.AgentVersion), e(h.Arch), on, e(strings.Join(ports, "; ")))
	}
	fmt.Fprint(w, `</table>`)

	// 4. Each run in full.
	for i, r := range a.Runs {
		fmt.Fprintf(w, `<h2 class="pb">%d. Run %d: %s</h2>`, i+4, r.ID, e(r.Name))
		writeRunBody(w, r)
	}
	fmt.Fprint(w, `<div class="sign"><div>Tested by / date</div><div>Accepted by / date</div></div></body></html>`)
}

func prioOrOff(v int) string {
	if v < 0 {
		return "off"
	}
	return strconv.Itoa(v)
}

// Routes.

func (s *Server) routesOps(mux *http.ServeMux) {
	s.routesNotify(mux)
	mux.HandleFunc("GET /api/v1/backup", s.admin(func(w http.ResponseWriter, r *http.Request) {
		b, err := s.makeBackup(r.Context(), r.URL.Query().Get("runs") == "1")
		if err != nil {
			serverError(w, err)
			return
		}
		s.store.Audit(r.Context(), "backup.download", map[string]any{"by": who(r).Name})
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Disposition", `attachment; filename="srnet-backup-`+time.Now().Format("2006-01-02-1504")+`.json"`)
		enc := json.NewEncoder(w)
		enc.SetIndent("", " ")
		enc.Encode(b)
	}))
	mux.HandleFunc("GET /api/v1/backups", s.admin(func(w http.ResponseWriter, r *http.Request) {
		files, _ := filepath.Glob(filepath.Join(s.cfg.DataDir, "backups", "srnet-backup-*.json"))
		sort.Sort(sort.Reverse(sort.StringSlice(files)))
		out := []map[string]any{}
		for _, f := range files {
			if st, err := os.Stat(f); err == nil {
				out = append(out, map[string]any{"name": filepath.Base(f), "size": st.Size(), "at": st.ModTime()})
			}
		}
		writeJSON(w, out)
	}))
	mux.HandleFunc("GET /api/v1/backups/{name}", s.admin(func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if !strings.HasPrefix(name, "srnet-backup-") || !strings.HasSuffix(name, ".json") || strings.ContainsAny(name, "/\\") {
			httpError(w, http.StatusBadRequest, "not a backup file name")
			return
		}
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
		http.ServeFile(w, r, filepath.Join(s.cfg.DataDir, "backups", name))
	}))
	mux.HandleFunc("POST /api/v1/restore", s.admin(func(w http.ResponseWriter, r *http.Request) {
		var b Backup
		if err := json.NewDecoder(io.LimitReader(r.Body, 64<<20)).Decode(&b); err != nil {
			httpError(w, http.StatusBadRequest, "not a backup file: "+err.Error())
			return
		}
		res, err := s.restore(r.Context(), b)
		if err != nil {
			var ue *userError
			if errors.As(err, &ue) {
				httpError(w, http.StatusBadRequest, err.Error())
			} else {
				serverError(w, err)
			}
			return
		}
		s.store.Audit(r.Context(), "backup.restore", map[string]any{"result": res, "from": b.CreatedAt, "by": who(r).Name})
		writeJSON(w, res)
	}))
	mux.HandleFunc("GET /api/v1/suite-runs/{id}/report.html", s.view(s.acceptanceReport))
}
