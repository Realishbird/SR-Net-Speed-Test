package controller

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/Realishbird/SR-Net-Speed-Test/internal/api"
	"github.com/Realishbird/SR-Net-Speed-Test/internal/version"
)

type Server struct {
	cfg     Config
	store   *Store
	hub     *hub
	metrics *metrics
	orch    *Orchestrator
	rel     *releases
	alerts  *alerter
	suites  *suiteRunner
	snmp    *snmpCollector
	health  *switchHealth
	acc     *access   // Phase 7: tokens, SSO sessions, roles
	notify  *notifier // Phase 7: Slack, Teams and webhook channels
	netplan *netplanJobs

	mu         sync.Mutex
	agentCache map[string]int64              // secret hash -> agent id
	portCache  map[int64]map[string]portMeta // agent id -> "dev/port" -> meta
	hostName   map[int64]string
	// adminOK remembers the one credential that last passed bcrypt, keyed by
	// its SHA-256, together with the password hash it was checked against.
	// The UI polls several endpoints every few seconds and a bcrypt check
	// costs tens of milliseconds each, which on a small LXC is CPU spent on
	// nothing; a changed password hash invalidates the entry by itself.
	adminOK     [32]byte
	adminOKHash string
	lldp        map[string]lldpEntry // switch-side LLDP table, polled by switchPollLoop
	vlans       map[string]string    // switch|port (lower-cased) -> VLAN id, from the same poll
	lldpAt      time.Time
	qos         map[string]*switchPortCfg // switch|port (lower-cased) -> port settings and queue counters, from the same poll
	qosErr      string
	qosLogged   string
	enrol       EnrolSettings  // Phase 8: token-less enrolment subnets
	updates     UpdateSettings // Phase 8: fleet update parallelism
}

func NewServer(ctx context.Context, cfg Config) (*Server, error) {
	store, err := OpenStore(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	h := newHub()
	m := newMetrics(cfg.VictoriaMetricsURL)
	go m.writer(ctx)
	snmp := newSNMPCollector(cfg.Switches, m)
	fast := cfg.SwitchFastPollSec
	if fast == 0 {
		fast = 2
	}
	if fast > 0 {
		snmp.fast = time.Duration(fast) * time.Second
	}
	go snmp.loop(ctx)
	orch := newOrchestrator(store, h, m)
	snmp.active = func() bool {
		orch.mu.Lock()
		n := len(orch.active)
		orch.mu.Unlock()
		return n > 0
	}
	go snmp.fastLoop(ctx)
	al := newAlerter(store, h, cfg)
	s := &Server{
		cfg: cfg, store: store, hub: h, metrics: m, orch: orch,
		rel:    newReleases(filepath.Join(cfg.DataDir, "releases"), store, h, al),
		alerts: al, suites: newSuiteRunner(store, orch, al), snmp: snmp, health: newSwitchHealth(),
		agentCache: map[string]int64{}, portCache: map[int64]map[string]portMeta{}, hostName: map[int64]string{},
		acc: newAccess(), netplan: newNetplanJobs(),
	}
	s.notify = newNotifier(store, cfg.PublicURL)
	s.loadAccess(ctx)
	s.loadScale(ctx)
	s.notify.load(ctx)
	al.notifier = s.notify
	s.rel.notify = s.notify
	s.suites.notify = s.notify
	orch.onFinish = s.notify.runFinished
	orch.switches = s.switchList
	orch.lldp = s.fillLLDP
	orch.fabric = s.fabricIssuesFor
	s.refreshPortCache(ctx)
	s.writeGnmic(ctx)
	go al.loop(ctx)
	go s.suites.scheduler(ctx)
	go s.gnmicLoop(ctx)
	go s.switchPollLoop(ctx)
	go s.backupLoop(ctx)
	go s.pruneLoop(ctx)
	return s, nil
}

// gnmicPorts lists, per switch, the ports whose transceivers gnmic should
// subscribe to: every port a host's LLDP maps to that switch, plus every
// port the switch itself reports as up (gNMI oper-status or the SNMP
// poller), since those are the ones with a module in them.
func (s *Server) gnmicPorts(ctx context.Context) map[string][]string {
	out := map[string][]string{}
	switches := s.switchList()
	if hosts, err := s.store.Hosts(ctx); err == nil {
		s.fillLLDP(ctx, hosts)
		for _, h := range hosts {
			for _, p := range h.Ports {
				if name := resolveSwitchName(switches, p.LLDPSwitch); name != "" && p.LLDPPort != "" {
					out[name] = append(out[name], p.LLDPPort)
				}
			}
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if ports, _, err := s.switchPortStats(ctx); err == nil {
		for _, p := range ports {
			if p.Up != nil && *p.Up {
				out[p.Switch] = append(out[p.Switch], p.Name)
			}
		}
	}
	return out
}

// writeGnmic renders the gnmic config for the current switches and ports.
func (s *Server) writeGnmic(ctx context.Context) {
	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()
	if n, err := WriteGnmicConfig(cfg, cfg.GnmicConfigPath, s.gnmicPorts(ctx)); err != nil {
		log.Printf("gnmic config: %v", err)
	} else if n > 0 {
		log.Printf("gnmic config: %d switch(es), %s", n, cfg.GnmicConfigPath)
	}
}

// gnmicLoop re-renders the gnmic config as ports come up or hosts report
// new LLDP neighbours. WriteGnmicConfig leaves an unchanged file alone, so
// gnmic is only restarted when its subscriptions actually change.
func (s *Server) gnmicLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Minute):
		}
		s.writeGnmic(ctx)
	}
}

func (s *Server) Close() { s.store.Close() }

func (s *Server) Handler() http.Handler {
	if s.acc == nil {
		s.acc = newAccess() // tests build a Server by hand
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })
	ui := uiHandler()
	mux.HandleFunc("GET /{$}", s.view(ui.ServeHTTP))
	mux.HandleFunc("GET /ca.pem", func(w http.ResponseWriter, r *http.Request) { http.ServeFile(w, r, s.cfg.CACert) })

	mux.HandleFunc("POST /api/v1/enroll", s.enroll)
	mux.HandleFunc("GET /api/v1/agent/tasks", s.agent(s.agentTasks))
	mux.HandleFunc("POST /api/v1/agent/inventory", s.agent(s.agentInventory))
	mux.HandleFunc("POST /api/v1/agent/samples", s.agent(s.agentSamples))
	mux.HandleFunc("POST /api/v1/agent/events", s.agent(s.agentEvent))
	mux.HandleFunc("POST /api/v1/agent/results", s.agent(s.agentResult))

	mux.HandleFunc("GET /api/v1/status", s.view(s.status))
	mux.HandleFunc("GET /api/v1/hosts", s.view(s.listHosts))
	mux.HandleFunc("GET /api/v1/topology", s.view(s.topology))
	mux.HandleFunc("PATCH /api/v1/hosts/{id}", s.admin(s.patchHost))
	mux.HandleFunc("DELETE /api/v1/hosts/{id}", s.admin(s.deleteHost))
	mux.HandleFunc("PATCH /api/v1/ports/{id}", s.admin(s.patchPort))
	mux.HandleFunc("POST /api/v1/hosts/import", s.admin(s.importHosts))
	mux.HandleFunc("POST /api/v1/tokens", s.admin(s.createToken))
	mux.HandleFunc("POST /api/v1/admin/password", s.admin(s.changePassword))
	mux.HandleFunc("GET /api/v1/runs", s.view(s.listRuns))
	mux.HandleFunc("POST /api/v1/runs", s.admin(s.createRun))
	mux.HandleFunc("GET /api/v1/runs/{id}", s.view(s.getRun))
	mux.HandleFunc("POST /api/v1/runs/{id}/cancel", s.admin(s.cancelRun))
	mux.HandleFunc("POST /api/v1/runs/{id}/baseline", s.admin(s.setBaseline))
	s.routes3(mux)
	s.routesSwitches(mux)
	s.routesFabric(mux)
	s.routesAccess(mux)
	s.routesOps(mux)
	s.routesScale(mux)
	s.routesExport(mux)
	s.routesNetplan(mux)
	s.routesCleanup(mux)
	return mux
}

// Auth.

func hashSecret(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func randomToken(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// adminLogin checks a Basic auth credential against the configured admin,
// remembering the last good one so repeated requests skip bcrypt.
func (s *Server) adminLogin(user, pass string) bool {
	key := sha256.Sum256([]byte(user + "\x00" + pass))
	s.mu.Lock()
	adminUser, hash := s.cfg.AdminUser, s.cfg.AdminPasswordHash
	cached := s.adminOKHash == hash && hash != "" && subtle.ConstantTimeCompare(s.adminOK[:], key[:]) == 1
	s.mu.Unlock()
	if cached {
		return true
	}
	if subtle.ConstantTimeCompare([]byte(user), []byte(adminUser)) != 1 ||
		bcrypt.CompareHashAndPassword([]byte(hash), []byte(pass)) != nil {
		return false
	}
	s.mu.Lock()
	s.adminOK, s.adminOKHash = key, hash
	s.mu.Unlock()
	return true
}

type agentKey struct{}

func (s *Server) agent(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || tok == "" {
			httpError(w, http.StatusUnauthorized, "agent token required")
			return
		}
		hash := hashSecret(tok)
		s.mu.Lock()
		id, cached := s.agentCache[hash]
		s.mu.Unlock()
		if !cached {
			var err error
			id, err = s.store.AgentBySecret(r.Context(), hash)
			if err != nil {
				httpError(w, http.StatusUnauthorized, "unknown agent")
				return
			}
			s.mu.Lock()
			s.agentCache[hash] = id
			s.mu.Unlock()
		}
		h(w, r.WithContext(context.WithValue(r.Context(), agentKey{}, id)))
	}
}

func agentID(r *http.Request) int64 { return r.Context().Value(agentKey{}).(int64) }

// Agent endpoints.

func (s *Server) enroll(w http.ResponseWriter, r *http.Request) {
	var req api.EnrollRequest
	if !readJSON(w, r, &req) {
		return
	}
	if req.Inventory.Hostname == "" {
		httpError(w, http.StatusBadRequest, "hostname missing from inventory")
		return
	}
	// An install token admits any host; without one the host must come from
	// an enrolment subnet, and is then approved at once or parked for an
	// admin (Phase 8).
	approved, via := true, "token"
	if err := s.store.ConsumeToken(r.Context(), hashSecret(req.Token)); err != nil {
		e := s.enrolSettings()
		if !enrolAllowed(e, clientIP(r)) {
			if req.Token == "" {
				err = errors.New("no install token given and " + clientIP(r) + " is not in an enrolment subnet")
			}
			httpError(w, http.StatusForbidden, err.Error())
			return
		}
		approved, via = e.AutoApprove, "subnet"
	}
	secret := randomToken(32)
	uid, err := s.store.EnrollAgent(r.Context(), "agt_"+randomToken(9), hashSecret(secret), req.Inventory, approved)
	if err != nil {
		serverError(w, err)
		return
	}
	if !approved {
		s.notify.hostPending(req.Inventory.Hostname, clientIP(r))
	}
	s.mu.Lock()
	s.agentCache = map[string]int64{} // a re-enrolled host's old secret must stop working
	s.mu.Unlock()
	s.store.Audit(r.Context(), "agent.enroll", map[string]any{"uid": uid, "hostname": req.Inventory.Hostname, "ip": clientIP(r), "via": via, "approved": approved})
	writeJSON(w, api.EnrollResponse{AgentID: uid, Secret: secret})
}

func (s *Server) agentTasks(w http.ResponseWriter, r *http.Request) {
	wait, _ := strconv.Atoi(r.URL.Query().Get("wait"))
	if wait <= 0 || wait > 30 {
		wait = 25
	}
	id := agentID(r)
	s.store.TouchAgent(r.Context(), id)
	writeJSON(w, s.hub.poll(r.Context(), id, time.Duration(wait)*time.Second))
}

func (s *Server) agentInventory(w http.ResponseWriter, r *http.Request) {
	var inv api.Inventory
	if !readJSON(w, r, &inv) {
		return
	}
	if err := s.store.SaveInventory(r.Context(), agentID(r), inv); err != nil {
		serverError(w, err)
		return
	}
	s.rel.noteVersion(agentID(r), inv.AgentVersion)
	s.refreshPortCache(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) refreshPortCache(ctx context.Context) {
	hosts, err := s.store.Hosts(ctx)
	if err != nil {
		log.Printf("port cache: %v", err)
		return
	}
	pc := map[int64]map[string]portMeta{}
	names := map[int64]string{}
	for _, h := range hosts {
		names[h.ID] = h.Name
		m := map[string]portMeta{}
		for _, p := range h.Ports {
			m[p.RdmaDev+"/"+strconv.Itoa(p.PortNum)] = portMeta{PortID: p.ID, Host: h.Name, RdmaDev: p.RdmaDev, PortNum: p.PortNum, Netdev: p.Netdev, Label: p.Label, Fabric: p.Fabric, HasIP: len(p.Addrs) > 0, PCIe: p.PCIe}
		}
		pc[h.ID] = m
	}
	s.mu.Lock()
	s.portCache, s.hostName = pc, names
	s.mu.Unlock()
}

func (s *Server) agentSamples(w http.ResponseWriter, r *http.Request) {
	var b api.SampleBatch
	if !readJSON(w, r, &b) {
		return
	}
	id := agentID(r)
	s.mu.Lock()
	ports, host := s.portCache[id], s.hostName[id]
	s.mu.Unlock()
	s.metrics.ingest(host, ports, b.Samples)
	s.alerts.onSamples(r.Context(), host, ports, b.Samples)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) agentEvent(w http.ResponseWriter, r *http.Request) {
	var e api.Event
	if !readJSON(w, r, &e) {
		return
	}
	if !s.hub.deliver(agentID(r), msg{taskID: e.TaskID, event: &e}) {
		httpError(w, http.StatusGone, "no run is waiting on this task")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) agentResult(w http.ResponseWriter, r *http.Request) {
	var res api.LegResult
	if !readJSON(w, r, &res) {
		return
	}
	if !s.hub.deliver(agentID(r), msg{taskID: res.TaskID, result: &res}) {
		httpError(w, http.StatusGone, "no run is waiting on this task")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Admin endpoints.

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	p := who(r)
	writeJSON(w, map[string]any{"version": version.Version, "public_url": s.cfg.PublicURL, "grafana_url": s.cfg.GrafanaURL, "time": time.Now(),
		"switches": len(s.switchList()), "whoami": api.Whoami{Name: p.Name, Role: p.Role, Via: p.Via}, "sso": s.oidcSettings().Ready()})
}

func (s *Server) listHosts(w http.ResponseWriter, r *http.Request) {
	hosts, err := s.store.Hosts(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	s.fillLLDP(r.Context(), hosts)
	out := make([]api.Host, len(hosts))
	for i, h := range hosts {
		h.Online = s.hub.online(h.ID)
		for j := range h.Ports {
			h.Ports[j].TxGbps, h.Ports[j].RxGbps = s.metrics.live(h.Ports[j].ID)
		}
		out[i] = h.Host
	}
	writeJSON(w, out)
}

func (s *Server) patchHost(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var u api.HostUpdate
	if !readJSON(w, r, &u) {
		return
	}
	if u.Name != nil && strings.TrimSpace(*u.Name) == "" {
		httpError(w, http.StatusBadRequest, "name cannot be empty")
		return
	}
	if err := s.renameHost(r.Context(), id, u); err != nil {
		storeError(w, err)
		return
	}
	s.store.Audit(r.Context(), "host.update", map[string]any{"id": id, "update": u})
	s.refreshPortCache(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteHost(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteHost(r.Context(), id); err != nil {
		serverError(w, err)
		return
	}
	s.mu.Lock()
	s.agentCache = map[string]int64{}
	s.mu.Unlock()
	s.store.Audit(r.Context(), "host.delete", map[string]any{"id": id})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) patchPort(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var u api.PortUpdate
	if !readJSON(w, r, &u) {
		return
	}
	if err := s.store.UpdatePort(r.Context(), id, u); err != nil {
		storeError(w, err)
		return
	}
	s.store.Audit(r.Context(), "port.update", map[string]any{"id": id, "update": u})
	s.refreshPortCache(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

// renameHost applies a host update and, when the display name changes,
// carries the host's open alerts over to the new name.
func (s *Server) renameHost(ctx context.Context, id int64, u api.HostUpdate) error {
	old := ""
	if u.Name != nil {
		s.mu.Lock()
		old = s.hostName[id]
		s.mu.Unlock()
	}
	if err := s.store.UpdateHost(ctx, id, u); err != nil {
		return err
	}
	if u.Name != nil && old != "" && old != *u.Name {
		if err := s.store.RenameAlertHost(ctx, old, *u.Name); err != nil {
			log.Printf("rename alerts of %s: %v", old, err)
		}
	}
	return nil
}

// importHosts applies a CSV of names, tags and port labels. Columns are found by
// header: host (required: current name or hostname), name, tags (separated by ;),
// site, rack, ip, label, fabric, expected_gbps. Port columns apply to the port with that ip.
func (s *Server) importHosts(w http.ResponseWriter, r *http.Request) {
	rd := csv.NewReader(io.LimitReader(r.Body, 1<<20))
	rd.TrimLeadingSpace = true
	rows, err := rd.ReadAll()
	if err != nil || len(rows) < 2 {
		httpError(w, http.StatusBadRequest, "expected a CSV with a header row and at least one data row")
		return
	}
	col := map[string]int{}
	for i, h := range rows[0] {
		col[strings.ToLower(strings.TrimSpace(h))] = i
	}
	if _, ok := col["host"]; !ok {
		httpError(w, http.StatusBadRequest, `CSV needs a "host" column`)
		return
	}
	get := func(row []string, k string) (string, bool) {
		i, ok := col[k]
		if !ok || i >= len(row) || strings.TrimSpace(row[i]) == "" {
			return "", false
		}
		return strings.TrimSpace(row[i]), true
	}
	hosts, err := s.store.Hosts(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	var problems []string
	applied := 0
	for n, row := range rows[1:] {
		line := n + 2
		sel, _ := get(row, "host")
		var h *hostRec
		for i := range hosts {
			if strings.EqualFold(hosts[i].Name, sel) || strings.EqualFold(hosts[i].Hostname, sel) {
				h = &hosts[i]
				break
			}
		}
		if h == nil {
			problems = append(problems, fmt.Sprintf("line %d: no enrolled host %q", line, sel))
			continue
		}
		var hu api.HostUpdate
		if v, ok := get(row, "name"); ok {
			hu.Name = &v
		}
		if v, ok := get(row, "tags"); ok {
			var tags []string
			for _, t := range strings.Split(v, ";") {
				if t = strings.TrimSpace(t); t != "" {
					tags = append(tags, t)
				}
			}
			hu.Tags = &tags
		}
		if v, ok := get(row, "site"); ok {
			hu.Site = &v
		}
		if v, ok := get(row, "rack"); ok {
			hu.Rack = &v
		}
		if err := s.renameHost(r.Context(), h.ID, hu); err != nil {
			problems = append(problems, fmt.Sprintf("line %d: %v", line, err))
			continue
		}
		if ip, ok := get(row, "ip"); ok {
			var portID int64
			for _, p := range h.Ports {
				for _, a := range p.Addrs {
					if pip, _, err := net.ParseCIDR(a); err == nil && pip.String() == ip {
						portID = p.ID
					}
				}
			}
			if portID == 0 {
				problems = append(problems, fmt.Sprintf("line %d: %s has no port with ip %s", line, sel, ip))
				continue
			}
			var pu api.PortUpdate
			if v, ok := get(row, "label"); ok {
				pu.Label = &v
			}
			if v, ok := get(row, "fabric"); ok {
				pu.Fabric = &v
			}
			if v, ok := get(row, "expected_gbps"); ok {
				f, err := strconv.ParseFloat(v, 64)
				if err != nil {
					problems = append(problems, fmt.Sprintf("line %d: expected_gbps %q is not a number", line, v))
					continue
				}
				pu.ExpectedGbps = &f
			}
			if err := s.store.UpdatePort(r.Context(), portID, pu); err != nil {
				problems = append(problems, fmt.Sprintf("line %d: %v", line, err))
				continue
			}
		}
		applied++
	}
	s.store.Audit(r.Context(), "host.import", map[string]any{"rows": applied, "problems": problems})
	s.refreshPortCache(r.Context())
	writeJSON(w, map[string]any{"applied": applied, "problems": problems})
}

// createToken mints an install token. It enrolls up to `hosts` machines
// (default 10, at most 100) within the hour, so one command covers a batch.
func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
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
	cmd := fmt.Sprintf("curl -fsSL %s | sudo bash -s -- --controller %s --token %s --ca-sha256 %s",
		s.cfg.InstallScriptURL, s.cfg.PublicURL, tok, fp)
	s.store.Audit(r.Context(), "token.create", map[string]any{"expires_at": exp, "hosts": hosts})
	writeJSON(w, api.TokenResponse{Token: tok, ExpiresAt: exp, Hosts: hosts, InstallCommand: cmd})
}

func tokenHosts(q string) int {
	n, err := strconv.Atoi(q)
	if err != nil || n < 1 {
		return 10
	}
	return min(n, 100)
}

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request) {
	runs, err := s.store.Runs(r.Context(), 50)
	if err != nil {
		serverError(w, err)
		return
	}
	if runs == nil {
		runs = []api.Run{}
	}
	writeJSON(w, runs)
}

func (s *Server) createRun(w http.ResponseWriter, r *http.Request) {
	var spec api.RunSpec
	if !readJSON(w, r, &spec) {
		return
	}
	id, err := s.orch.Start(r.Context(), spec)
	var ue *userError
	if errors.As(err, &ue) {
		httpError(w, http.StatusConflict, ue.Error())
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	run, err := s.store.Run(r.Context(), id)
	if err != nil {
		serverError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(run)
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	run, err := s.store.Run(r.Context(), id)
	if err != nil {
		storeError(w, err)
		return
	}
	s.orch.Live(&run)
	s.fillBaseline(r.Context(), &run)
	writeJSON(w, run)
}

// setBaseline marks a finished run as the reference for later runs of the
// same shape, or clears the mark. The run's result series are written again
// so the Trends dashboard reflects the change.
func (s *Server) setBaseline(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req api.BaselineRequest
	if !readJSON(w, r, &req) {
		return
	}
	err := s.store.SetBaseline(r.Context(), id, req.Baseline)
	var ue *userError
	if errors.As(err, &ue) {
		httpError(w, http.StatusConflict, ue.Error())
		return
	}
	if err != nil {
		storeError(w, err)
		return
	}
	s.store.Audit(r.Context(), "run.baseline", map[string]any{"run_id": id, "baseline": req.Baseline})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) cancelRun(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if !s.orch.Cancel(id) {
		httpError(w, http.StatusConflict, "run is not active")
		return
	}
	s.store.Audit(r.Context(), "run.cancel", map[string]any{"run_id": id})
	w.WriteHeader(http.StatusAccepted)
}

// Helpers.

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, 8<<20))
	if err := dec.Decode(v); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	}
	return true
}

// changePassword sets a new admin password from the UI. The current password
// is asked for again even though Basic auth just checked it, so a page left
// open cannot be used to lock the operator out.
func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	var req api.PasswordChange
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "bad JSON: "+err.Error())
		return
	}
	s.mu.Lock()
	hash := s.cfg.AdminPasswordHash
	s.mu.Unlock()
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Current)) != nil {
		httpError(w, http.StatusForbidden, "current password is wrong")
		return
	}
	if len(req.New) < 8 {
		httpError(w, http.StatusBadRequest, "new password must be at least 8 characters")
		return
	}
	if len(req.New) > 256 {
		httpError(w, http.StatusBadRequest, "new password is too long")
		return
	}
	h, err := bcrypt.GenerateFromPassword([]byte(req.New), bcrypt.DefaultCost)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.mu.Lock()
	s.cfg.AdminPasswordHash = string(h)
	cfg := s.cfg
	s.mu.Unlock()
	if err := cfg.SaveBack(); err != nil {
		log.Printf("admin password: saved in memory only, config not written: %v", err)
	}
	// The srnet CLI reads its password from a file next to the config; a
	// stale copy would lock it out after the change.
	if pwFile := cfg.CLIPasswordFile(); pwFile != "" {
		if st, err := os.Stat(pwFile); err == nil && st.Mode().IsRegular() {
			if err := os.WriteFile(pwFile, []byte(req.New+"\n"), 0o600); err != nil {
				log.Printf("admin password: could not update %s for the srnet CLI: %v", pwFile, err)
			}
		}
	}
	if s.store != nil {
		s.store.Audit(r.Context(), "admin.password", map[string]any{"user": s.cfg.AdminUser})
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func writeJSONStatus(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func serverError(w http.ResponseWriter, err error) {
	log.Printf("error: %v", err)
	httpError(w, http.StatusInternalServerError, "internal error")
}

func storeError(w http.ResponseWriter, err error) {
	if errors.Is(err, pgx.ErrNoRows) {
		httpError(w, http.StatusNotFound, "not found")
		return
	}
	serverError(w, err)
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid id")
		return 0, false
	}
	return id, true
}

func clientIP(r *http.Request) string {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}
