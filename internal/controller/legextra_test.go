package controller

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Realishbird/SR-Net-Speed-Test/internal/api"
)

func TestResolveSwitchName(t *testing.T) {
	sws := []SwitchConfig{{Name: "Z9864F", Address: "176.16.9.22"}, {Name: "edge", Address: "10.0.0.9"}}
	for chassis, want := range map[string]string{"z9864f": "Z9864F", "176.16.9.22": "Z9864F", "edge-1": "edge", "Z9864F-ON": "Z9864F", "sonic": "", "": ""} {
		if got := resolveSwitchName(sws, chassis); got != want {
			t.Errorf("resolveSwitchName(%q) = %q, want %q", chassis, got, want)
		}
	}
	if got := resolveSwitchName(sws[:1], "sonic"); got != "Z9864F" {
		t.Errorf("single switch must catch unknown chassis names, got %q", got)
	}
}

// TestLegSwitchFromVictoriaMetrics serves the switch counter samples a 60 s
// write test would leave in VictoriaMetrics and checks the leg gets the
// switch's rate on the server port plus the discards that grew.
func TestLegSwitchFromVictoriaMetrics(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	end := start.Add(60 * time.Second)
	// Samples every 30 s: one 15 s before the start, one 15 s before the end.
	t0, t1 := float64(start.Unix()-15), float64(end.Unix()-15)
	counters := map[string][2]float64{ // value at t0, value at t1
		"in_octets":    {1e9, 1e9 + 1e9},           // client -> switch on the client port, idle here
		"out_octets":   {5e9, 5e9 + 735e9/1e9*1e9}, // 735 GB over 60 s = 98 Gbps
		"in_discards":  {3, 10},
		"out_discards": {0, 0},
		"in_errors":    {0, 0},
		"out_errors":   {0, 0},
	}
	re := regexp.MustCompile(`^(tlast_over_time|last_over_time)\(\{__name__=~"srnet_gnmi_\.\*(\w+)",([^}]*)\}\[90s\] @ (\d+)\)$`)
	qosRe := regexp.MustCompile(`^last_over_time\(\{__name__=~"srnet_gnmi_\.\*qos[^"]*",source=~"[^"]*",interface_interface_id="(Ethernet\d+)"\}\[90s\] @ (\d+)\) or last_over_time\(\{[^}]*interface_id="Ethernet\d+"\}\[90s\] @ \d+\)$`)
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		queries = append(queries, q)
		if qm := qosRe.FindStringSubmatch(q); qm != nil {
			// The QoS subscription (Phase 5): queue 0 on the server port dropped
			// 40 packets during the test and its PFC priority 3 saw 5 pauses.
			if qm[1] != "Ethernet64" {
				w.Write([]byte(`{"status":"success","data":{"result":[]}}`))
				return
			}
			drops, pauses := 100, 10
			if qm[2] == fmt.Sprint(end.Unix()) {
				drops, pauses = 140, 15
			}
			fmt.Fprintf(w, `{"status":"success","data":{"result":[
				{"metric":{"__name__":"srnet_gnmi_openconfig_qos_qos_interfaces_interface_output_queues_queue_state_dropped_pkts","source":"z9864f","interface_id":"Ethernet64","name":"Ethernet64:0"},"value":[%d,"%d"]},
				{"metric":{"__name__":"srnet_gnmi_openconfig_qos_qos_interfaces_interface_output_queues_queue_state_dropped_pkts","source":"z9864f","interface_id":"Ethernet64","name":"Ethernet64:3"},"value":[%d,"0"]},
				{"metric":{"__name__":"srnet_gnmi_openconfig_qos_qos_interfaces_interface_pfc_pfc_priorities_pfc_priority_state_pause_frames_rx","source":"z9864f","interface_id":"Ethernet64","dot1p":"3"},"value":[%d,"%d"]}]}}`,
				end.Unix(), drops, end.Unix(), end.Unix(), pauses)
			return
		}
		m := re.FindStringSubmatch(q)
		if m == nil {
			t.Errorf("unexpected query %q", q)
			w.Write([]byte(`{"status":"success","data":{"result":[]}}`))
			return
		}
		if !strings.Contains(m[3], `interface_name="Ethernet8"`) && !strings.Contains(m[3], `interface_name="Ethernet64"`) {
			t.Errorf("selector %q lacks the port", m[3])
		}
		at, ts := m[4], t0
		if at == fmt.Sprint(end.Unix()) {
			ts = t1
		}
		var v float64
		if m[1] == "tlast_over_time" {
			v = ts
		} else {
			c, ok := counters[m[2]]
			if !ok {
				w.Write([]byte(`{"status":"success","data":{"result":[]}}`))
				return
			}
			v = c[0]
			if ts == t1 {
				v = c[1]
			}
			if strings.Contains(m[3], "Ethernet8") { // client port: swap directions, no discards
				if m[2] == "in_octets" {
					v = counters["out_octets"][0]
					if ts == t1 {
						v = counters["out_octets"][1]
					}
				} else if m[2] != "out_octets" {
					v = 0
				}
			}
		}
		fmt.Fprintf(w, `{"status":"success","data":{"result":[{"metric":{"__name__":"x","source":"z9864f","interface_name":"Ethernet64"},"value":[%d,"%g"]}]}}`, int64(ts), v)
	}))
	defer srv.Close()

	o := newOrchestrator(nil, nil, newMetrics(srv.URL))
	o.switches = func() []SwitchConfig {
		return []SwitchConfig{{Name: "Z9864F", Address: "176.16.9.22", GNMIPort: 8080, GNMIUser: "admin"}}
	}
	p := &legPlan{
		client: endpoint{Host: "Dell 770AP", LLDPSwitch: "z9864f", LLDPPort: "Ethernet8"},
		server: endpoint{Host: "spark-424f", LLDPSwitch: "sonic", LLDPPort: "Ethernet64"},
	}
	ar := &activeRun{spec: api.RunSpec{Test: "write", DurationSec: 60}}
	ls := o.legSwitch(ar, p, start, end)
	if ls == nil {
		t.Fatal("no switch figures")
	}
	if ls.ClientPort != "Z9864F Ethernet8" || ls.ServerPort != "Z9864F Ethernet64" {
		t.Errorf("ports = %q, %q", ls.ClientPort, ls.ServerPort)
	}
	if ls.Gbps < 97.9 || ls.Gbps > 98.1 {
		t.Errorf("Gbps = %.3f, want 98 (735 GB over the 60 s between samples)", ls.Gbps)
	}
	if ls.Deltas["Ethernet64 in_discards"] != 7 || ls.Deltas["Ethernet64 queue 0 dropped_pkts"] != 40 || ls.Deltas["Ethernet64 prio 3 pause_frames_rx"] != 5 || len(ls.Deltas) != 3 {
		t.Errorf("deltas = %v", ls.Deltas)
	}
	if ls.Note != "" {
		t.Errorf("note = %q", ls.Note)
	}

	// A read test flows server -> client, so the server port's "in" counts.
	ar.spec.Test = "read"
	if ls := o.legSwitch(ar, p, start, end); ls.Gbps < 0.13 || ls.Gbps > 0.14 { // 1 GB over 60 s
		t.Errorf("read Gbps = %.3f, want 0.133", ls.Gbps)
	}
	// Bidirectional adds both directions.
	ar.spec.Test, ar.spec.Bidir = "write", true
	if ls := o.legSwitch(ar, p, start, end); ls.Gbps < 98.0 || ls.Gbps > 98.2 {
		t.Errorf("bidir Gbps = %.3f, want 98.13", ls.Gbps)
	}
	// Without LLDP there is nothing to say, and without VictoriaMetrics only the ports.
	if ls := o.legSwitch(ar, &legPlan{}, start, end); ls != nil {
		t.Errorf("unmapped leg got %+v", ls)
	}
	o.metrics = newMetrics("")
	if ls := o.legSwitch(ar, p, start, end); ls == nil || ls.Gbps != 0 || ls.ServerPort == "" {
		t.Errorf("no VM: %+v", ls)
	}
}

func TestScoreAddsSwitchAndCPU(t *testing.T) {
	m := newMetrics("")
	o := newOrchestrator(nil, nil, m)
	o.switches = func() []SwitchConfig { return []SwitchConfig{{Name: "sw1", Address: "10.0.0.2"}} }
	start := time.Now().Add(-70 * time.Second)
	for sec := 0; sec <= 60; sec += 5 {
		m.ingest("spark-1", nil, []api.Sample{{Time: start.Add(time.Duration(sec) * time.Second), Values: map[string]float64{"cpu_util_pct": 20 + float64(sec)}}})
	}
	p := &legPlan{startAt: start, leg: api.Leg{ExpectedGbps: 100},
		client:    endpoint{Host: "Dell 770AP", LLDPSwitch: "sw1", LLDPPort: "Ethernet1"},
		server:    endpoint{Host: "spark-1"},
		clientRes: &api.LegResult{AvgGbps: 98}, serverRes: &api.LegResult{}}
	ar := &activeRun{spec: api.RunSpec{Test: "write", DurationSec: 60, PassPct: 90}, plans: []*legPlan{p}, stages: [][]*legPlan{{p}}}
	o.score(ar)
	l := p.leg
	if l.Switch == nil || l.Switch.ClientPort != "sw1 Ethernet1" || l.Switch.ServerPort != "" {
		t.Errorf("switch = %+v", l.Switch)
	}
	if l.CPU == nil || l.CPU.ServerAvg != 50 || l.CPU.ServerPeak != 80 || l.CPU.ClientAvg != 0 {
		t.Errorf("cpu = %+v", l.CPU)
	}
}

func TestReportsCarrySwitchAndCPU(t *testing.T) {
	r := api.Run{ID: 9, Name: "x", Spec: api.RunSpec{Test: "write", DurationSec: 60}, Status: "done", Verdict: "pass", Legs: []api.Leg{{
		ClientHost: "a", ServerHost: "b", ToolGbps: 98, Verdict: "pass",
		Switch: &api.LegSwitch{ClientPort: "sw Ethernet8", ServerPort: "sw Ethernet64", Gbps: 97.86, Deltas: map[string]float64{"Ethernet64 in_discards": 7}},
		CPU:    &api.LegCPU{ClientAvg: 12.34, ClientPeak: 40, ServerAvg: 5, ServerPeak: 9},
	}}}
	var csvOut, htmlOut strings.Builder
	if err := WriteRunCSV(&csvOut, r); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(csvOut.String()), "\n")
	if !strings.HasSuffix(lines[0], ",switch_client_port,switch_server_port,switch_gbps,switch_deltas,cpu_client_avg_pct,cpu_client_peak_pct,cpu_server_avg_pct,cpu_server_peak_pct,baseline_run,baseline_value,baseline_delta_pct") {
		t.Errorf("csv header = %s", lines[0])
	}
	if !strings.HasSuffix(lines[1], ",sw Ethernet8,sw Ethernet64,97.86,Ethernet64 in_discards=7,12.3,40,5,9,,,") {
		t.Errorf("csv row = %s", lines[1])
	}
	WriteRunHTML(&htmlOut, r, "test")
	for _, want := range []string{"Switch path", "sw Ethernet8 &gt; sw Ethernet64", "97.9", "12 (peak 40) / 5 (peak 9)", "switch Ethernet64 in_discards=7"} {
		if !strings.Contains(htmlOut.String(), want) {
			t.Errorf("html lacks %q", want)
		}
	}
}
