package controller

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	oids, back := fastOIDs([]snmpPort{{"1", "Ethernet0"}, {"5", "Ethernet4"}})
	if len(oids) != 12 || len(back) != 12 {
		t.Fatalf("%d oids", len(oids))
	}
	if c := back["1.3.6.1.2.1.31.1.1.1.6.5"]; c.Index != "5" || c.Col != "in_octets" {
		t.Errorf("back = %+v", c)
	}
	c := newSNMPCollector(nil, newMetrics(""))
	rows := map[string]map[string]any{"1": {"name": "Ethernet0", "in_octets": int64(1000), "out_octets": int64(0), "oper_status": int64(1)}}
	now := time.Now()
	c.render(SwitchConfig{Name: "sw"}, rows, now.Add(-2*time.Second))
	rows["1"]["in_octets"] = int64(1000 + 250_000_000) // 1 Gbps over 2 s
	lines, n := c.render(SwitchConfig{Name: "sw"}, rows, now)
	if n != 1 || !strings.Contains(string(lines), `srnet_switch_rx_gbps{port="Ethernet0",switch="sw"} 1 `) {
		t.Errorf("render:\n%s", lines)
	}
}

func TestHealthParseAndLines(t *testing.T) {
	raw := `[{"source":"z9864f","updates":[{"values":{
	  "openconfig-platform:components":{"component":[
	    {"name":"TEMP 2","state":{"name":"TEMP 2","temperature":{"current":35.5,"high":70,"critical-high":80}}},
	    {"name":"FAN 1","state":{"oper-status":"openconfig-platform-types:ACTIVE"}},
	    {"name":"PSU 2","state":{"oper-status":"INACTIVE"}}]},
	  "openconfig-system:cpus":{"cpu":[{"index":"ALL","state":{"total":{"instant":7}}}]},
	  "openconfig-system:memory":{"state":{"physical":1000,"unused":400}},
	  "openconfig-system-ext:infra":{"state":{"uptime":"3 days, 04:00:10","reboot-cause":"User issued 'reboot'"}}}}]}]`
	h := api.SwitchHealth{At: time.Unix(1700000000, 0)}
	for _, vals := range gnmicUpdates([]byte(raw)) {
		for _, v := range vals {
			parseHealth(v, &h)
		}
	}
	if len(h.Temps) != 1 || h.Temps[0].C != 35.5 || h.Temps[0].HighC != 70 || h.Temps[0].CriticalC != 80 {
		t.Errorf("temps = %+v", h.Temps)
	}
	if len(h.Fans) != 1 || !h.Fans[0].OK || len(h.PSUs) != 1 || h.PSUs[0].OK {
		t.Errorf("fans %+v psus %+v", h.Fans, h.PSUs)
	}
	if h.CPUPct != 7 || h.MemPct != 60 || h.UptimeSec != 3*86400+4*3600+10 || !strings.Contains(h.LastReboot, "reboot") {
		t.Errorf("cpu %v mem %v up %v reboot %q", h.CPUPct, h.MemPct, h.UptimeSec, h.LastReboot)
	}
	lines := string(healthLines("z9864F", h))
	for _, want := range []string{`srnet_switch_temp_c{sensor="TEMP 2",switch="z9864F"} 35.5`, `srnet_switch_fan_ok{fan="FAN 1",switch="z9864F"} 1`, `srnet_switch_psu_ok{psu="PSU 2",switch="z9864F"} 0`, `srnet_switch_cpu_pct{switch="z9864F"} 7`, `srnet_switch_uptime_seconds{switch="z9864F"} 273610`} {
		if !strings.Contains(lines, want) {
			t.Errorf("missing %q in\n%s", want, lines)
		}
	}
	if uptimeSeconds(float64(5e12)) != 5000 || uptimeSeconds("01:02:03") != 3723 {
		t.Error("uptime parsing")
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
	if !strings.Contains(hosts, `host!=""`) || len(switches) != 4 || !strings.Contains(switches[1], "interface_name=~") || !strings.Contains(switches[2], "interface_interface_id=~") || !strings.Contains(switches[0], `port=~"^(?:Ethernet0|Ethernet4)$"`) {
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
	if err := json.Unmarshal(b, &dash); err != nil || dash.UID != "srnet-switch" || len(dash.Templating.List) != 2 {
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
			}
		}
	}
}
