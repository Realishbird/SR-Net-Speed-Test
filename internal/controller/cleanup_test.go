package controller

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Realishbird/SR-Net-Speed-Test/internal/api"
)

func mirrorFake(t *testing.T, dir string, versions ...string) {
	t.Helper()
	for i, v := range versions {
		d := filepath.Join(dir, v)
		os.MkdirAll(d, 0o755)
		os.WriteFile(filepath.Join(d, "srnet-agent-linux-amd64"), []byte("bin"), 0o755)
		os.WriteFile(filepath.Join(d, "SHA256SUMS"), []byte("9b4ee2d1d2f33d8e5f9ea1c6b3d5b1a4b7b6d7a2bb1a5d4f3b1c2d3e4f5a6b7c  srnet-agent-linux-amd64\n"), 0o644)
		os.Chtimes(d, time.Now().Add(-time.Duration(len(versions)-i)*time.Hour), time.Now().Add(-time.Duration(len(versions)-i)*time.Hour))
	}
}

func TestReleasePruneAndRemove(t *testing.T) {
	dir := t.TempDir()
	r := newReleases(dir, nil, newHub(), nil)
	mirrorFake(t, dir, "v0.1.9", "v0.1.11", "v0.1.13", "v0.1.14", "v0.1.15", "v0.1.17", "v0.1.18", "v0.1.20", "v0.1.25")
	r.noteVersion(1, "v0.1.20")
	r.noteVersion(2, "v0.1.20")
	if got := r.Prune(0); got != nil {
		t.Errorf("keep 0 must not prune, removed %v", got)
	}
	removed := r.Prune(3)
	want := []string{"v0.1.15", "v0.1.14", "v0.1.13", "v0.1.11", "v0.1.9"}
	if strings.Join(removed, ",") != strings.Join(want, ",") {
		t.Errorf("removed %v, want %v", removed, want)
	}
	var left []string
	for _, m := range r.Mirrored() {
		left = append(left, m.Version)
	}
	if strings.Join(left, ",") != "v0.1.25,v0.1.20,v0.1.18,v0.1.17" {
		t.Errorf("left %v (newest 3 plus the version in use)", left)
	}
	if err := r.Remove("v0.1.20", false); err == nil || !strings.Contains(err.Error(), "running on an agent") {
		t.Errorf("in-use remove: %v", err)
	}
	if err := r.Remove("v0.1.17", false); err != nil {
		t.Errorf("remove: %v", err)
	}
	if err := r.Remove("v0.1.17", false); err == nil {
		t.Error("second remove must say it is not mirrored")
	}
	if err := r.Remove("../etc", false); err == nil {
		t.Error("bad version must be refused")
	}
	r.mu.Lock()
	r.job = &api.UpdateJob{Status: "running", Version: "v0.1.25"}
	r.mu.Unlock()
	if got := r.Prune(1); got != nil {
		t.Errorf("must not prune during a job, removed %v", got)
	}
	if c := r.versionCounts(); c["v0.1.20"] != 2 {
		t.Errorf("counts = %v", c)
	}
}

func TestRemoveReleaseRoute(t *testing.T) {
	s, srv := testServer(t)
	dir := t.TempDir()
	s.rel = newReleases(dir, nil, s.hub, nil)
	mirrorFake(t, dir, "v0.1.1", "v0.1.2")
	s.rel.noteVersion(1, "v0.1.1")
	admin := "srnet_" + randomToken(30)
	s.acc.tokens[hashSecret(admin)] = api.APIToken{ID: 1, Name: "t", Role: "admin"}
	if r := doReq(t, "DELETE", srv.URL+"/api/v1/releases/v0.1.1", admin, ""); r.StatusCode != 409 {
		t.Errorf("in-use delete: %d", r.StatusCode)
	}
	if r := doReq(t, "DELETE", srv.URL+"/api/v1/releases/v0.1.2", admin, ""); r.StatusCode != 204 {
		t.Errorf("delete: %d", r.StatusCode)
	}
	resp := doReq(t, "GET", srv.URL+"/api/v1/releases", admin, "")
	var list api.ReleaseList
	json.NewDecoder(resp.Body).Decode(&list)
	if list.Keep != 3 || len(list.Mirrored) != 1 || list.Mirrored[0].InUse != 1 {
		t.Errorf("list = %+v", list)
	}
}

func TestFastOIDs(t *testing.T) {
	oids, back := fastOIDs([]snmpPort{{"1", "Ethernet0", 100000}, {"5", "Ethernet4", 100000}})
	if len(oids) != 12 || len(back) != 12 {
		t.Fatalf("%d oids", len(oids))
	}
	if c := back["1.3.6.1.2.1.31.1.1.1.6.5"]; c.Index != "5" || c.Col != "in_octets" {
		t.Errorf("back = %+v", c)
	}
	c := newSNMPCollector(nil, newMetrics(""))
	rows := map[string]map[string]any{"1": {"name": "Ethernet0", "in_octets": int64(1000), "out_octets": int64(0), "oper_status": int64(1)}}
	now := time.Now()
	c.render(SwitchConfig{Name: "sw"}, rows, now.Add(-2*time.Second), "walk")
	rows["1"]["in_octets"] = int64(1000 + 250_000_000) // 1 Gbps over 2 s
	lines, n := c.render(SwitchConfig{Name: "sw"}, rows, now, "walk")
	if n != 1 || !strings.Contains(string(lines), `srnet_switch_rx_gbps{port="Ethernet0",switch="sw"} 1 `) {
		t.Errorf("render:\n%s", lines)
	}
}

// rxRate pulls the rx_gbps value out of rendered lines, or -1.
func rxRate(lines []byte) float64 {
	for _, l := range strings.Split(string(lines), "\n") {
		if strings.HasPrefix(l, "srnet_switch_rx_gbps") {
			f := strings.Fields(l)
			v, _ := strconv.ParseFloat(f[1], 64)
			return v
		}
	}
	return -1
}

// TestSNMPFastRate models the Z9864F: the SNMP agent refreshes its counters
// every 5 s, the fast poll reads every 2.1 s and a full walk lands in
// between. The 2 s chart must show the real 80 Gbps, never thousands.
func TestSNMPFastRate(t *testing.T) {
	c := newSNMPCollector(nil, newMetrics(""))
	sw := SwitchConfig{Name: "sw"}
	base := time.Unix(1700000000, 0)
	const bps = 80e9 / 8                // bytes per second while traffic runs
	counter := func(at float64) int64 { // value the agent serves at time at
		refreshed := math.Floor((at-0.7)/5)*5 + 0.7
		if refreshed > 150 {
			refreshed = 150 // traffic stops at 150 s
		}
		return int64(1e9 + refreshed*bps)
	}
	row := func(at float64, fast bool) map[string]map[string]any {
		r := map[string]any{"name": "Ethernet120", "in_octets": counter(at), "out_octets": counter(at) / 2, "_at": base.Add(time.Duration(at * float64(time.Second)))}
		if fast {
			r["_mbps"] = int64(100000)
		} else {
			r["speed_mbps"] = int64(100000)
		}
		return map[string]map[string]any{"121": r}
	}
	var last float64
	for at := 0.0; at < 200; at += 2.1 {
		lines, _ := c.render(sw, row(at, true), base, "fast")
		if v := rxRate(lines); v >= 0 {
			if v > 100 {
				t.Fatalf("t=%.1f: %g Gbps on a 100G port", at, v)
			}
			if at > 90 && at < 150 && math.Abs(v-80) > 80*0.04 {
				t.Errorf("t=%.1f: %g Gbps, want 80", at, v)
			}
			last = v
		}
		if int(at*10)%30 == 0 { // a full walk every few polls, interleaved
			lines, _ := c.render(sw, row(at+0.05, false), base, "walk")
			if v := rxRate(lines); v > 100 {
				t.Fatalf("walk t=%.1f: %g Gbps", at, v)
			}
		}
	}
	if last != 0 {
		t.Errorf("idle port ends at %g Gbps, want 0", last)
	}
}

func TestHealthParseAndLines(t *testing.T) {
	// System nodes, as one Get of infra, cpus and memory returns them.
	sys := `[{"source":"z9864f","updates":[{"Path":"openconfig-system:system","values":{
	  "openconfig-system:cpus":{"cpu":[{"index":"ALL","state":{"total":{"instant":7}}}]},
	  "openconfig-system:memory":{"state":{"physical":1000,"unused":400}},
	  "openconfig-system-ext:infra":{"state":{"uptime":"3 days, 04:00:10","reboot-cause":"User issued 'reboot'"}}}}]}]`
	// Components as the Z9864F answers them: the answer names the hardware,
	// not the key that was asked for, and numbers come back as strings.
	comps := `[{"source":"z9864f","updates":[
	  {"Path":"openconfig-platform:components/component[name=TEMP 1]/state","values":{"openconfig-platform:state":{"name":"CPU Temp","temperature":{"critical-high-threshold":"97","current":"32","high-threshold":"95"}}}},
	  {"Path":"openconfig-platform:components/component[name=FAN 1]/state","values":{"openconfig-platform:state":{"name":"FanTray1-Fan1","oper-status":"openconfig-platform-types:ACTIVE"}}},
	  {"Path":"openconfig-platform:components/component[name=FAN 2]/state","values":{"openconfig-platform:state":{"name":"FanTray1-Fan2","oper-status":"openconfig-platform-types:INACTIVE"}}},
	  {"Path":"openconfig-platform:components/component[name=PSU 2]/state","values":{"openconfig-platform:state":{"name":"PSU 2","oper-status":"openconfig-platform-types:ACTIVE"}}}]},
	  {"source":"other","updates":[{"Path":"openconfig-platform:components/component[name=TEMP 9]/state","values":{"state":{"name":"Elsewhere","temperature":{"current":"50"}}}}]}]`
	speed := `[{"source":"z9864f","updates":[{"Path":"openconfig-platform:components/component[name=FAN 1]/fan/state","values":{"openconfig-platform-fan:state":{"openconfig-platform-fan:speed":8442}}}]}]`
	h := api.SwitchHealth{At: time.Unix(1700000000, 0)}
	for _, u := range gnmicGetUpdates([]byte(sys)) {
		parseHealth(u.Values, &h)
	}
	for _, u := range gnmicGetUpdates([]byte(comps)) {
		if u.Source == "z9864f" {
			parseComponent(componentKey(u.Path), u.Values, &h)
		}
	}
	for _, u := range gnmicGetUpdates([]byte(speed)) {
		parseFanSpeed(componentKey(u.Path), u.Values, &h)
	}
	if len(h.Temps) != 1 || h.Temps[0].Name != "CPU Temp" || h.Temps[0].Component != "TEMP 1" || h.Temps[0].C != 32 || h.Temps[0].HighC != 95 || h.Temps[0].CriticalC != 97 {
		t.Errorf("temps = %+v", h.Temps)
	}
	if len(h.Fans) != 2 || h.Fans[0].Name != "FanTray1-Fan1" || !h.Fans[0].OK || h.Fans[0].RPM != 8442 || h.Fans[1].OK {
		t.Errorf("fans = %+v", h.Fans)
	}
	if len(h.PSUs) != 1 || !h.PSUs[0].OK || h.PSUs[0].Component != "PSU 2" {
		t.Errorf("psus = %+v", h.PSUs)
	}
	if h.CPUPct != 7 || h.MemPct != 60 || h.UptimeSec != 3*86400+4*3600+10 || !strings.Contains(h.LastReboot, "reboot") {
		t.Errorf("cpu %v mem %v up %v reboot %q", h.CPUPct, h.MemPct, h.UptimeSec, h.LastReboot)
	}
	lines := string(healthLines("z9864F", h))
	for _, want := range []string{
		`srnet_switch_temp_c{component="TEMP 1",sensor="CPU Temp",switch="z9864F"} 32`,
		`srnet_switch_temp_high_c{component="TEMP 1",sensor="CPU Temp",switch="z9864F"} 95`,
		`srnet_switch_temp_crit_c{component="TEMP 1",sensor="CPU Temp",switch="z9864F"} 97`,
		`srnet_switch_fan_ok{component="FAN 1",fan="FanTray1-Fan1",switch="z9864F"} 1`,
		`srnet_switch_fan_ok{component="FAN 2",fan="FanTray1-Fan2",switch="z9864F"} 0`,
		`srnet_switch_fan_rpm{component="FAN 1",fan="FanTray1-Fan1",switch="z9864F"} 8442`,
		`srnet_switch_psu_ok{component="PSU 2",psu="PSU 2",switch="z9864F"} 1`,
		`srnet_switch_cpu_pct{switch="z9864F"} 7`,
		`srnet_switch_uptime_seconds{switch="z9864F"} 273610`,
	} {
		if !strings.Contains(lines, want) {
			t.Errorf("missing %q in\n%s", want, lines)
		}
	}
	if uptimeSeconds(float64(5e12)) != 5000 || uptimeSeconds("01:02:03") != 3723 {
		t.Error("uptime parsing")
	}
	if !keyLess("FAN 2", "FAN 10") || keyLess("TEMP 1", "FAN 3") {
		t.Error("keyLess")
	}
}

func TestRawExportCSV(t *testing.T) {
	// A fake VictoriaMetrics export endpoint returning one host and one switch series.
	vm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/export" {
			http.NotFound(w, r)
			return
		}
		m := r.URL.Query()["match[]"]
		if len(m) == 0 {
			t.Errorf("no match[]")
		}
		w.Write([]byte(`{"metric":{"__name__":"srnet_rdma_rx_bytes","host":"Dell Spark 1","rdma_dev":"rocep1s0f0","port":"1","netdev":"enp1s0f0np0","port_label":"P1-B1-A","fabric":"100","configured":"yes","run_id":"9"},"values":[100,200],"timestamps":[1700000000000,1700000001000]}
{"metric":{"__name__":"srnet_gnmi_openconfig_interfaces_interfaces_interface_state_counters_in_octets","interface_name":"Ethernet0","source":"z9864f"},"values":[5],"timestamps":[1700000000000]}
{"metric":{"__name__":"srnet_switch_in_discards","switch":"z9864F","port":"Ethernet4"},"values":[7],"timestamps":[1700000000000]}
`))
	}))
	defer vm.Close()
	s, _ := testServer(t)
	s.metrics = newMetrics(vm.URL)
	sc := rawScope{From: time.Unix(1700000000, 0), To: time.Unix(1700000100, 0), Switches: []SwitchConfig{{Name: "z9864F", Address: "10.0.0.2", GNMIPort: 8080}}, SwitchPorts: map[string][]string{"z9864F": {"Ethernet0", "Ethernet4"}}}
	var buf bytes.Buffer
	if err := s.writeRawCSV(context.Background(), &buf, sc, ""); err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 5 || strings.Join(rows[0], ",") != strings.Join(rawHeader, ",") {
		t.Fatalf("rows: %v", rows)
	}
	h := rows[1]
	if h[2] != "host" || h[3] != "Dell Spark 1" || h[4] != "enp1s0f0np0" || h[5] != "rocep1s0f0" || h[6] != "P1-B1-A" || h[7] != "100" || h[8] != "rdma_rx_bytes" || h[10] != "9" || h[11] != "100" || h[1] != "1700000000000" {
		t.Errorf("host row = %v", h)
	}
	if rows[2][11] != "200" || rows[2][1] != "1700000001000" {
		t.Errorf("second sample = %v", rows[2])
	}
	sw := rows[3]
	if sw[2] != "switch" || sw[3] != "z9864F" || sw[4] != "Ethernet0" || !strings.HasSuffix(sw[8], "counters_in_octets") || sw[11] != "5" {
		t.Errorf("gnmi row = %v", sw)
	}
	if rows[4][2] != "switch" || rows[4][4] != "Ethernet4" || rows[4][8] != "switch_in_discards" {
		t.Errorf("snmp row = %v", rows[4])
	}
	hosts, switches := sc.matchers()
	if !strings.Contains(hosts, `host!=""`) || len(switches) != 5 || !strings.Contains(switches[1], "interface_name=~") || !strings.Contains(switches[2], "interface_interface_id=~") || !strings.Contains(switches[3], "component_name=~") || !strings.Contains(switches[0], `port=~"^(?:Ethernet0|Ethernet4)$"`) {
		t.Errorf("matchers: %s %v", hosts, switches)
	}
	var jl bytes.Buffer
	if err := s.writeRawJSONL(context.Background(), &jl, sc); err != nil || strings.Count(jl.String(), "\n") != 3 {
		t.Errorf("jsonl: %v %q", err, jl.String())
	}
	var zb bytes.Buffer
	if err := s.writeRawZip(context.Background(), &zb, sc, rawManifest{}); err != nil || !bytes.HasPrefix(zb.Bytes(), []byte("PK")) {
		t.Errorf("zip: %v", err)
	}
}

func TestDeleteRunRoutes(t *testing.T) {
	s, srv := testServer(t)
	admin := "srnet_" + randomToken(30)
	s.acc.tokens[hashSecret(admin)] = api.APIToken{ID: 1, Name: "t", Role: "admin"}
	if r := doReq(t, "DELETE", srv.URL+"/api/v1/runs/abc", admin, ""); r.StatusCode != 400 {
		t.Errorf("bad id: %d", r.StatusCode)
	}
	if r := doReq(t, "POST", srv.URL+"/api/v1/runs/delete", admin, `{}`); r.StatusCode != 409 {
		t.Errorf("empty filter: %d", r.StatusCode)
	}
	if r := doReq(t, "GET", srv.URL+"/api/v1/runs/delete-preview", admin, ""); r.StatusCode != 409 {
		t.Errorf("empty preview: %d", r.StatusCode)
	}
	if r := doReq(t, "DELETE", srv.URL+"/api/v1/runs/1", "", ""); r.StatusCode != 401 {
		t.Errorf("unauthenticated delete: %d", r.StatusCode)
	}
}

func TestTemperatures(t *testing.T) {
	m := newMetrics("")
	m.mu.Lock()
	m.temps[7] = portTemps{opticAt: time.Now(), nicAt: time.Now().Add(-time.Hour), optic: 49.5, nic: 44}
	m.mu.Unlock()
	o, n := m.temperatures(7)
	if o != 49.5 || n != 0 {
		t.Errorf("optic %v nic %v (stale NIC reading must be dropped)", o, n)
	}
}

func TestSwitchDashboardJSON(t *testing.T) {
	d, err := GrafanaDashboards()
	if err != nil {
		t.Fatal(err)
	}
	b, ok := d["switch.json"]
	if !ok {
		t.Fatal("switch.json missing")
	}
	var dash struct {
		UID        string `json:"uid"`
		Templating struct {
			List []struct{ Name string } `json:"list"`
		} `json:"templating"`
		Panels []struct {
			Type    string `json:"type"`
			Targets []struct{ Expr string }
		} `json:"panels"`
	}
	if err := json.Unmarshal(b, &dash); err != nil || dash.UID != "srnet-switch" || len(dash.Templating.List) != 3 {
		t.Fatalf("dashboard: %v uid=%s vars=%d", err, dash.UID, len(dash.Templating.List))
	}
	for name, raw := range d {
		var any struct {
			Panels []struct {
				Targets []struct{ Expr string }
			} `json:"panels"`
		}
		json.Unmarshal(raw, &any)
		for _, p := range any.Panels {
			for _, tg := range p.Targets {
				if strings.Contains(tg.Expr, `__name__=~`) && (strings.Contains(tg.Expr, "rate(") || strings.Contains(tg.Expr, "increase(")) && !strings.Contains(tg.Expr, "keep_metric_names") {
					t.Errorf("%s: multi-metric rate without keep_metric_names: %s", name, tg.Expr)
				}
				// keep_metric_names goes after the call's closing
				// parenthesis; "x[2m] keep_metric_names)" is a syntax
				// error that blanked every rate panel in v0.1.26.
				for rest := tg.Expr; ; {
					i := strings.Index(rest, "keep_metric_names")
					if i < 0 {
						break
					}
					if strings.TrimRight(rest[:i], " ")[len(strings.TrimRight(rest[:i], " "))-1] != ')' {
						t.Errorf("%s: keep_metric_names not after a closing parenthesis: %s", name, tg.Expr)
					}
					rest = rest[i+1:]
				}
				// Optic series carry component_name, not name.
				if strings.Contains(tg.Expr, "transceiver") && strings.Contains(tg.Expr, `,name=~`) {
					t.Errorf("%s: transceiver query filters on name: %s", name, tg.Expr)
				}
			}
		}
	}
}

// TestRawMatchersQuoted checks every selector the export and the per-leg
// switch figures build is valid MetricsQL: each string literal unquotes and
// each regex compiles. v0.1.26 wrote the switch's IP address as "176\.16..."
// inside a plain string, VictoriaMetrics rejected it and run exports came
// back empty.
func TestRawMatchersQuoted(t *testing.T) {
	sw := SwitchConfig{Name: "z9864F", Address: "176.16.9.22", GNMIPort: 8080}
	sc := rawScope{Switches: []SwitchConfig{sw}, Hosts: []string{"Dell Spark 1", "lab.host (2)"}, SwitchPorts: map[string][]string{"z9864F": {"Ethernet0", "Ethernet1.5"}}}
	hosts, switches := sc.matchers()
	all := append([]string{hosts}, switches...)
	all = append(all, switchSeries(sw, true, "Ethernet0")("in_octets"), switchSeries(sw, false, "Ethernet0")("in_octets"))
	lit := regexp.MustCompile(`(=~|=|!=)("(?:[^"\\]|\\.)*")`)
	for _, m := range all {
		found := lit.FindAllStringSubmatch(m, -1)
		if len(found) == 0 {
			t.Errorf("no labels in %s", m)
		}
		for _, f := range found {
			v, err := strconv.Unquote(f[2])
			if err != nil {
				t.Errorf("%s: %s does not unquote: %v", m, f[2], err)
				continue
			}
			if f[1] == "=~" {
				re, err := regexp.Compile("^(?:" + v + ")$")
				if err != nil {
					t.Errorf("%s: bad regex %q: %v", m, v, err)
				} else if strings.Contains(m, "source=~") && !re.MatchString("176.16.9.22:8080") && strings.Contains(f[0], "176") {
					t.Errorf("source regex %q misses the address", v)
				}
			}
		}
	}
	if !strings.Contains(hosts, `lab\\.host \\(2\\)`) {
		t.Errorf("host regex not escaped: %s", hosts)
	}
}

// TestRawZipFinishesOnError: when the store fails part way, the archive is
// still a valid zip and says why in ERROR.txt.
func TestRawZipFinishesOnError(t *testing.T) {
	vm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(strings.Join(r.URL.Query()["match[]"], " "), "srnet_switch_") {
			http.Error(w, "cannot parse string literal", http.StatusBadRequest)
			return
		}
		w.Write([]byte(`{"metric":{"__name__":"srnet_rdma_rx_bytes","host":"h"},"values":[1],"timestamps":[1700000000000]}` + "\n"))
	}))
	defer vm.Close()
	s, _ := testServer(t)
	s.metrics = newMetrics(vm.URL)
	sc := rawScope{From: time.Unix(1700000000, 0), To: time.Unix(1700000100, 0), Switches: []SwitchConfig{{Name: "sw", Address: "10.0.0.2"}}, AllSwitch: true}
	var zb bytes.Buffer
	if err := s.writeRawZip(context.Background(), &zb, sc, rawManifest{}); err == nil {
		t.Fatal("want the store's error")
	}
	zr, err := zip.NewReader(bytes.NewReader(zb.Bytes()), int64(zb.Len()))
	if err != nil {
		t.Fatalf("archive does not open: %v", err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	if got := strings.Join(names, ","); got != "manifest.json,host_samples.csv,switch_samples.csv,ERROR.txt" {
		t.Errorf("entries = %s", got)
	}
	if err := s.metrics.vmCheck(context.Background(), []string{`{__name__=~"srnet_switch_.+"}`}, sc.From, sc.To); err == nil || !strings.Contains(err.Error(), "400") {
		t.Errorf("vmCheck = %v", err)
	}
}
