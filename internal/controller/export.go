package controller

import (
	"archive/zip"
	"bufio"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Realishbird/SR-Net-Speed-Test/internal/api"
	"github.com/Realishbird/SR-Net-Speed-Test/internal/version"
)

// Raw export: every value the agents and the switches reported in a window,
// one row per sample, straight from VictoriaMetrics. The run and suite
// reports summarise; this is for people who draw their own charts.
//
// Host samples carry the labels host, rdma_dev, port, netdev, port_label,
// fabric, configured and run_id (the runs active when the sample was
// taken). Switch samples come two ways: srnet_switch_* from the SNMP poller
// (labels switch, port) and srnet_gnmi_* from gnmic (labels source,
// interface_name or name, subscription_name). Counters are exported as
// collected, not turned into rates; the *_gbps series are the only derived
// values and are kept because the agents computed them at sample time.

// rawScope says which series and which window to export.
type rawScope struct {
	From, To    time.Time
	Hosts       []string            // host display names; nil = every host
	SwitchPorts map[string][]string // switch name -> ports; nil with AllSwitch = every port
	AllSwitch   bool
	NoSwitch    bool
	Switches    []SwitchConfig
}

// vmLine is one line of VictoriaMetrics' /api/v1/export.
type vmLine struct {
	Metric     map[string]string `json:"metric"`
	Values     []float64         `json:"values"`
	Timestamps []int64           `json:"timestamps"`
}

var rawHeader = []string{"time", "unix_ms", "source", "name", "port", "rdma_dev", "port_label", "fabric", "metric", "sub", "run_id", "value", "labels"}

// matchers builds the VictoriaMetrics selectors for a scope: one for host
// series, and for switch series one per label the switch data is keyed by.
func (sc rawScope) matchers() (hosts string, switches []string) {
	hosts = `{__name__=~"srnet_.+",host!=""}`
	if sc.Hosts != nil {
		hosts = fmt.Sprintf(`{__name__=~"srnet_.+",host=~%q}`, anyOf(sc.Hosts))
	}
	if sc.NoSwitch {
		return hosts, nil
	}
	if sc.AllSwitch || len(sc.SwitchPorts) == 0 {
		return hosts, []string{`{__name__=~"srnet_switch_.+"}`, `{__name__=~"srnet_gnmi_.+"}`}
	}
	names := make([]string, 0, len(sc.SwitchPorts))
	for n := range sc.SwitchPorts {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		ports := anyOf(sc.SwitchPorts[n])
		switches = append(switches, fmt.Sprintf(`{__name__=~"srnet_switch_.+",switch=%q,port=~%q}`, n, ports))
		src := "(?i)" + regexp.QuoteMeta(n)
		for _, sw := range sc.Switches {
			if sw.Name == n {
				src = gnmiSourceRe(sw)
			}
		}
		// Every regex goes in with %q, which doubles the backslashes of
		// QuoteMeta: a bare "176\.16" is not a valid MetricsQL string.
		for _, key := range []string{"interface_name", "interface_interface_id", "component_name", "name"} {
			switches = append(switches, fmt.Sprintf(`{__name__=~"srnet_gnmi_.+",source=~%q,%s=~%q}`, src, key, ports))
		}
	}
	return hosts, switches
}

func anyOf(list []string) string {
	q := make([]string, 0, len(list))
	for _, v := range list {
		q = append(q, regexp.QuoteMeta(v))
	}
	return "^(?:" + strings.Join(q, "|") + ")$"
}

// vmCheck asks VictoriaMetrics for one series per selector, so a selector
// it rejects or an unreachable store is reported as an HTTP error before
// any of the download has been sent.
func (m *metrics) vmCheck(ctx context.Context, matchers []string, from, to time.Time) error {
	if m.vmURL == "" {
		return fmt.Errorf("no metrics store is configured")
	}
	q := url.Values{"start": {strconv.FormatInt(from.Unix(), 10)}, "end": {strconv.FormatInt(to.Unix(), 10)}, "limit": {"1"}}
	for _, s := range matchers {
		q.Add("match[]", s)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", m.vmURL+"/api/v1/series?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return fmt.Errorf("victoriametrics: %s %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}

// checkScope runs vmCheck on a scope's selectors and answers 502 when it
// fails. It returns false when the request has been answered.
func (s *Server) checkScope(w http.ResponseWriter, r *http.Request, sc rawScope) bool {
	hosts, switches := sc.matchers()
	if err := s.metrics.vmCheck(r.Context(), append([]string{hosts}, switches...), sc.From, sc.To); err != nil {
		log.Printf("raw export: %v", err)
		httpError(w, http.StatusBadGateway, "the metrics store refused the export: "+err.Error())
		return false
	}
	return true
}

// vmExport streams the series matching the selectors in the window, one
// vmLine at a time; VictoriaMetrics may split a long series over lines.
func (m *metrics) vmExport(ctx context.Context, matchers []string, from, to time.Time, fn func(l vmLine) error) error {
	if m.vmURL == "" || len(matchers) == 0 {
		return nil
	}
	q := url.Values{"start": {strconv.FormatInt(from.Unix(), 10)}, "end": {strconv.FormatInt(to.Unix(), 10)}, "reduce_mem_usage": {"1"}}
	for _, s := range matchers {
		q.Add("match[]", s)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", m.vmURL+"/api/v1/export?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return fmt.Errorf("victoriametrics export: %s %s", resp.Status, strings.TrimSpace(string(b)))
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var l vmLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			return fmt.Errorf("victoriametrics export: %w", err)
		}
		if err := fn(l); err != nil {
			return err
		}
	}
	return sc.Err()
}

// rawRows turns one export line into CSV rows, in time order.
func rawRows(l vmLine, switches []SwitchConfig, w func(row []string) error) error {
	lab := l.Metric
	name := strings.TrimPrefix(lab["__name__"], "srnet_")
	src, who, port := "host", lab["host"], lab["netdev"]
	known := map[string]bool{"__name__": true}
	switch {
	case lab["host"] != "":
		if port == "" && lab["rdma_dev"] != "" {
			port = lab["rdma_dev"] + "/" + lab["port"]
		}
		for _, k := range []string{"host", "netdev", "rdma_dev", "port", "port_label", "fabric", "sub", "run_id"} {
			known[k] = true
		}
	case lab["switch"] != "":
		src, who, port = "switch", lab["switch"], lab["port"]
		known["switch"], known["port"] = true, true
	default:
		src, who = "switch", resolveSwitchName(switches, strings.TrimSuffix(lab["source"], ":"+portOf(lab["source"])))
		if who == "" {
			who = lab["source"]
		}
		// gnmic labels the port by its list key: interface_name for the
		// interface counters, interface_interface_id for QoS, component_name
		// for optics (older builds wrote plain name).
		for _, k := range []string{"interface_name", "interface_interface_id", "component_name", "name"} {
			if port == "" && lab[k] != "" {
				port = lab[k]
			}
			known[k] = true
		}
		known["source"] = true
	}
	var rest []string
	for k, v := range lab {
		if !known[k] && v != "" {
			rest = append(rest, k+"="+v)
		}
	}
	sort.Strings(rest)
	extra := strings.Join(rest, ";")
	idx := make([]int, len(l.Timestamps))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return l.Timestamps[idx[a]] < l.Timestamps[idx[b]] })
	for _, i := range idx {
		if i >= len(l.Values) {
			break
		}
		ts := l.Timestamps[i]
		row := []string{time.UnixMilli(ts).UTC().Format("2006-01-02T15:04:05.000Z"), strconv.FormatInt(ts, 10), src, who, port,
			lab["rdma_dev"], lab["port_label"], lab["fabric"], name, lab["sub"], lab["run_id"], strconv.FormatFloat(l.Values[i], 'g', -1, 64), extra}
		if err := w(row); err != nil {
			return err
		}
	}
	return nil
}

func portOf(addr string) string {
	if _, p, ok := strings.Cut(addr, ":"); ok {
		return p
	}
	return ""
}

// writeRawCSV streams the scope as CSV: host series first, then switch series.
func (s *Server) writeRawCSV(ctx context.Context, w io.Writer, sc rawScope, part string) error {
	cw := csv.NewWriter(w)
	if err := cw.Write(rawHeader); err != nil {
		return err
	}
	hosts, switches := sc.matchers()
	var sel []string
	switch part {
	case "hosts":
		sel = []string{hosts}
	case "switch":
		sel = switches
	default:
		sel = append([]string{hosts}, switches...)
	}
	err := s.metrics.vmExport(ctx, sel, sc.From, sc.To, func(l vmLine) error {
		return rawRows(l, sc.Switches, cw.Write)
	})
	if err != nil {
		// The header (and maybe rows) are already sent: end the file with
		// the reason rather than leave it looking complete.
		cw.Write([]string{"# export stopped early: " + err.Error()})
	}
	cw.Flush()
	if err != nil {
		return err
	}
	return cw.Error()
}

// writeRawJSONL streams the export lines as VictoriaMetrics gives them,
// with the scope's source added, so a script can replay them anywhere.
func (s *Server) writeRawJSONL(ctx context.Context, w io.Writer, sc rawScope) error {
	hosts, switches := sc.matchers()
	enc := json.NewEncoder(w)
	err := s.metrics.vmExport(ctx, append([]string{hosts}, switches...), sc.From, sc.To, func(l vmLine) error {
		return enc.Encode(l)
	})
	if err != nil {
		enc.Encode(map[string]string{"error": "export stopped early: " + err.Error()})
	}
	return err
}

// rawManifest is the zip's manifest.json: what was exported and from what.
type rawManifest struct {
	Generated  time.Time           `json:"generated"`
	Controller string              `json:"controller_version"`
	From       time.Time           `json:"from"`
	To         time.Time           `json:"to"`
	Run        *api.Run            `json:"run,omitempty"`
	SuiteRun   *api.SuiteRun       `json:"suite_run,omitempty"`
	Hosts      []api.Host          `json:"hosts"`
	Switches   []rawSwitch         `json:"switches"`
	Files      []string            `json:"files"`
	Columns    []string            `json:"columns"`
	Notes      []string            `json:"notes"`
	Profiles   []api.FabricProfile `json:"fabric_profiles,omitempty"`
}

type rawSwitch struct {
	Name    string   `json:"name"`
	Address string   `json:"address"`
	Ports   []string `json:"ports,omitempty"` // ports in the export; empty = all
}

var rawNotes = []string{
	"One row per collected value. Host rows come from the agents (1 s cadence during a test, 15 s idle; DOM optics every 5 s); switch rows from the switch (SNMP poll every 2 s on ports in use while a test runs, 30 s otherwise; gNMI streaming every 30 s).",
	"Counters (bytes, packets, errors, discards) are cumulative as the device reports them; *_gbps rows are the rate the agent or poller computed between its own two samples.",
	"Rows are grouped by series, each series in time order. Pivot on (time, name, port, metric) for a wide table.",
	"run_id lists the runs active when a host sample was taken; switch samples carry no run id.",
}

// writeRawZip writes manifest.json, host_samples.csv, switch_samples.csv.
// The archive is always finished: if a part fails, ERROR.txt says why, so
// the file still opens instead of looking empty.
func (s *Server) writeRawZip(ctx context.Context, w io.Writer, sc rawScope, man rawManifest) (err error) {
	zw := zip.NewWriter(w)
	defer func() {
		if err != nil {
			if f, e := zw.Create("ERROR.txt"); e == nil {
				fmt.Fprintf(f, "The export stopped early, so the files in this archive are incomplete.\r\n\r\n%v\r\n", err)
			}
		}
		if e := zw.Close(); err == nil {
			err = e
		}
	}()
	man.Generated, man.Controller, man.From, man.To = time.Now(), version.Version, sc.From, sc.To
	man.Columns, man.Notes = rawHeader, rawNotes
	man.Files = []string{"manifest.json", "host_samples.csv", "switch_samples.csv"}
	if man.Hosts == nil {
		man.Hosts = []api.Host{}
	}
	man.Switches = []rawSwitch{}
	for _, sw := range sc.Switches {
		man.Switches = append(man.Switches, rawSwitch{Name: sw.Name, Address: sw.Address, Ports: sc.SwitchPorts[sw.Name]})
	}
	f, err := zw.Create("manifest.json")
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", " ")
	if err := enc.Encode(man); err != nil {
		return err
	}
	for _, part := range []string{"hosts", "switch"} {
		f, err := zw.Create(map[string]string{"hosts": "host_samples.csv", "switch": "switch_samples.csv"}[part])
		if err != nil {
			return err
		}
		if err := s.writeRawCSV(ctx, f, sc, part); err != nil {
			return err
		}
	}
	return nil
}

// runScope is the window and the hosts and switch ports a run touched.
func (s *Server) runScope(ctx context.Context, run api.Run, swMode string) (rawScope, []api.Host) {
	sc := rawScope{Switches: s.switchList(), SwitchPorts: map[string][]string{}}
	sc.From = run.CreatedAt
	if run.StartedAt != nil {
		sc.From = *run.StartedAt
	}
	sc.From = sc.From.Add(-30 * time.Second)
	sc.To = time.Now()
	if run.EndedAt != nil {
		sc.To = run.EndedAt.Add(30 * time.Second)
	}
	names := map[string]bool{}
	for _, l := range run.Legs {
		names[l.ClientHost], names[l.ServerHost] = true, true
		if l.Switch != nil {
			for _, p := range []string{l.Switch.ClientPort, l.Switch.ServerPort} {
				if sw, port, ok := strings.Cut(p, " "); ok {
					sc.SwitchPorts[sw] = appendUnique(sc.SwitchPorts[sw], port)
				}
			}
		}
	}
	var hostsOut []api.Host
	if hosts, err := s.store.Hosts(ctx); err == nil {
		s.fillLLDP(ctx, hosts)
		for _, h := range hosts {
			if !names[h.Name] {
				continue
			}
			sc.Hosts = append(sc.Hosts, h.Name)
			hostsOut = append(hostsOut, h.Host)
			for _, p := range h.Ports {
				if sw := resolveSwitchName(sc.Switches, p.LLDPSwitch); sw != "" && p.LLDPPort != "" && len(p.Addrs) > 0 {
					sc.SwitchPorts[sw] = appendUnique(sc.SwitchPorts[sw], p.LLDPPort)
				}
			}
		}
	}
	if sc.Hosts == nil { // hosts renamed or gone since: fall back to the leg names
		for n := range names {
			sc.Hosts = append(sc.Hosts, n)
		}
	}
	sort.Strings(sc.Hosts)
	for _, ports := range sc.SwitchPorts {
		sort.Strings(ports)
	}
	switch swMode {
	case "all":
		sc.AllSwitch = true
	case "none":
		sc.NoSwitch = true
	default:
		if len(sc.SwitchPorts) == 0 {
			sc.AllSwitch = true // nothing mapped yet: better every port than none
		}
	}
	return sc, hostsOut
}

func appendUnique(list []string, v string) []string {
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}

// Routes.

func (s *Server) routesExport(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/runs/{id}/raw", s.view(s.rawRun))
	mux.HandleFunc("GET /api/v1/suite-runs/{id}/raw.zip", s.view(s.rawSuiteRun))
	mux.HandleFunc("GET /api/v1/export", s.view(s.rawWindow))
}

func rawFormat(r *http.Request) string {
	f := r.URL.Query().Get("format")
	if f == "" {
		f = "csv"
	}
	return f
}

func (s *Server) rawRun(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	run, err := s.store.Run(r.Context(), id)
	if err != nil {
		storeError(w, err)
		return
	}
	if run.StartedAt == nil {
		httpError(w, http.StatusConflict, "the run has not started; nothing was collected for it yet")
		return
	}
	sc, hosts := s.runScope(r.Context(), run, r.URL.Query().Get("switch"))
	if f := rawFormat(r); (f == "csv" || f == "jsonl" || f == "zip") && !s.checkScope(w, r, sc) {
		return
	}
	name := fmt.Sprintf("srnet-run-%d-raw", id)
	switch rawFormat(r) {
	case "csv":
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.csv"`, name))
		err = s.writeRawCSV(r.Context(), w, sc, "")
	case "jsonl":
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.jsonl"`, name))
		err = s.writeRawJSONL(r.Context(), w, sc)
	case "zip":
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.zip"`, name))
		s.fillBaseline(r.Context(), &run)
		profiles, _ := s.store.FabricProfiles(r.Context())
		err = s.writeRawZip(r.Context(), w, sc, rawManifest{Run: &run, Hosts: hosts, Profiles: profiles})
	default:
		httpError(w, http.StatusBadRequest, "format must be csv, jsonl or zip")
		return
	}
	if err != nil {
		log.Printf("raw export run %d: %v", id, err)
	}
}

func (s *Server) rawSuiteRun(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	sr, err := s.store.SuiteRun(r.Context(), id)
	if err != nil {
		storeError(w, err)
		return
	}
	for _, rid := range sr.RunIDs { // the selectors are built the same way for every run; check the first
		if run, err := s.store.Run(r.Context(), rid); err == nil && run.StartedAt != nil {
			if sc, _ := s.runScope(r.Context(), run, r.URL.Query().Get("switch")); !s.checkScope(w, r, sc) {
				return
			}
			break
		}
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="srnet-suite-run-%d-raw.zip"`, id))
	zw := zip.NewWriter(w)
	for _, rid := range sr.RunIDs {
		run, err := s.store.Run(r.Context(), rid)
		if err != nil || run.StartedAt == nil {
			continue
		}
		sc, hosts := s.runScope(r.Context(), run, r.URL.Query().Get("switch"))
		f, err := zw.Create(fmt.Sprintf("run-%d/raw.zip", rid))
		if err != nil {
			break
		}
		if err := s.writeRawZip(r.Context(), f, sc, rawManifest{Run: &run, SuiteRun: &sr, Hosts: hosts}); err != nil {
			log.Printf("raw export suite run %d, run %d: %v", id, rid, err)
			break
		}
	}
	zw.Close()
}

// rawWindow exports a time range outside any run: idle telemetry, a manual
// test, or a longer look at one host.
func (s *Server) rawWindow(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	parse := func(v string, def time.Time) (time.Time, error) {
		if v == "" {
			return def, nil
		}
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			if n > 1e12 {
				return time.UnixMilli(n), nil
			}
			return time.Unix(n, 0), nil
		}
		if d, err := time.ParseDuration(v); err == nil && strings.HasPrefix(v, "-") {
			return time.Now().Add(d), nil
		}
		return time.Parse(time.RFC3339, v)
	}
	to, err := parse(q.Get("to"), time.Now())
	if err != nil {
		httpError(w, http.StatusBadRequest, "to: RFC3339, unix seconds or a relative duration like -1h")
		return
	}
	from, err := parse(q.Get("from"), to.Add(-time.Hour))
	if err != nil {
		httpError(w, http.StatusBadRequest, "from: RFC3339, unix seconds or a relative duration like -1h")
		return
	}
	if !to.After(from) || to.Sub(from) > 7*24*time.Hour {
		httpError(w, http.StatusBadRequest, "the window must be between a second and seven days")
		return
	}
	sc := rawScope{From: from, To: to, Switches: s.switchList(), AllSwitch: true}
	if hs := q.Get("hosts"); hs != "" {
		for _, h := range strings.Split(hs, ",") {
			if h = strings.TrimSpace(h); h != "" {
				sc.Hosts = append(sc.Hosts, h)
			}
		}
	}
	if q.Get("switch") == "none" {
		sc.NoSwitch, sc.AllSwitch = true, false
	}
	if f := rawFormat(r); (f == "csv" || f == "jsonl" || f == "zip") && !s.checkScope(w, r, sc) {
		return
	}
	name := "srnet-raw-" + from.UTC().Format("20060102T150405Z") + "-" + to.UTC().Format("20060102T150405Z")
	switch rawFormat(r) {
	case "csv":
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.csv"`, name))
		err = s.writeRawCSV(r.Context(), w, sc, "")
	case "jsonl":
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.jsonl"`, name))
		err = s.writeRawJSONL(r.Context(), w, sc)
	case "zip":
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.zip"`, name))
		var hosts []api.Host
		if hs, e := s.store.Hosts(r.Context()); e == nil {
			for _, h := range hs {
				if sc.Hosts == nil || contains(sc.Hosts, h.Name) {
					hosts = append(hosts, h.Host)
				}
			}
		}
		err = s.writeRawZip(r.Context(), w, sc, rawManifest{Hosts: hosts})
	default:
		httpError(w, http.StatusBadRequest, "format must be csv, jsonl or zip")
		return
	}
	if err != nil {
		log.Printf("raw export window: %v", err)
	}
}
