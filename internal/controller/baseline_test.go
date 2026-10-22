package controller

import (
	"strings"
	"testing"
	"time"

	"github.com/Realishbird/SR-Net-Speed-Test/internal/api"
)

func TestCompareBaselineMatchesLegsAndFlagsRegressions(t *testing.T) {
	base := api.Run{ID: 6, Spec: api.RunSpec{Test: "write"}, Legs: []api.Leg{
		{ClientHost: "dell770ap", ServerHost: "spark-424f", MsgSize: 65536, ToolGbps: 98.0, Verdict: "pass"},
		{ClientHost: "dell770ap", ServerHost: "spark-4256", MsgSize: 65536, ToolGbps: 98.0, Verdict: "pass"},
		{ClientHost: "dell770ap", ServerHost: "spark-193f", MsgSize: 65536, ToolGbps: 40.0, Verdict: "fail"}, // a failed baseline leg is no reference
	}}
	r := api.Run{ID: 9, Spec: api.RunSpec{Test: "write", RegressPct: 5}, Legs: []api.Leg{
		{ClientHost: "dell770ap", ServerHost: "spark-424f", MsgSize: 65536, ToolGbps: 97.5, Verdict: "pass"},
		{ClientHost: "dell770ap", ServerHost: "spark-4256", MsgSize: 65536, ToolGbps: 90.0, Verdict: "pass"},
		{ClientHost: "dell770ap", ServerHost: "spark-193f", MsgSize: 65536, ToolGbps: 98.0, Verdict: "pass"},
		{ClientHost: "dell770ap", ServerHost: "spark-c2e1", MsgSize: 65536, ToolGbps: 98.0, Verdict: "pass"}, // not in the baseline
	}}
	if n := compareBaseline(&r, base, 5); n != 1 {
		t.Fatalf("regressed legs = %d, want 1", n)
	}
	l0, l1 := r.Legs[0].Baseline, r.Legs[1].Baseline
	if l0 == nil || l0.RunID != 6 || l0.Value != 98 || l0.Regressed || l0.DeltaPct > -0.5 || l0.DeltaPct < -0.52 {
		t.Fatalf("leg 1 baseline: %+v", l0)
	}
	if l1 == nil || !l1.Regressed || l1.DeltaPct > -8.1 || l1.DeltaPct < -8.2 {
		t.Fatalf("leg 2 baseline: %+v", l1)
	}
	if r.Legs[2].Baseline != nil || r.Legs[3].Baseline != nil {
		t.Fatal("legs without a usable baseline leg were compared")
	}
	if r.Summary.BaselineRunID != 6 || r.Summary.Regressed != 1 || r.Summary.BaselineGbps != 196 {
		t.Fatalf("summary: %+v", r.Summary)
	}
	// (97.5 + 90) against 196: -4.34%.
	if d := r.Summary.DeltaPct; d > -4.3 || d < -4.4 {
		t.Fatalf("aggregate delta %.2f", d)
	}
	if note := regressNote("write", l1); !strings.Contains(note, "8.2% behind baseline run 6 (98.0 Gbps)") {
		t.Fatalf("note: %s", note)
	}
}

func TestCompareBaselineLatencyLowerIsBetter(t *testing.T) {
	base := api.Run{ID: 3, Spec: api.RunSpec{Test: "write_lat"}, Legs: []api.Leg{
		{ClientHost: "a", ServerHost: "b", MsgSize: 2, Latency: &api.Latency{P99Us: 4.0}, Verdict: "pass"},
	}}
	better := api.Run{ID: 4, Spec: api.RunSpec{Test: "write_lat"}, Legs: []api.Leg{{ClientHost: "a", ServerHost: "b", MsgSize: 2, Latency: &api.Latency{P99Us: 3.8}, Verdict: "pass"}}}
	worse := api.Run{ID: 5, Spec: api.RunSpec{Test: "write_lat"}, Legs: []api.Leg{{ClientHost: "a", ServerHost: "b", MsgSize: 2, Latency: &api.Latency{P99Us: 4.4}, Verdict: "pass"}}}
	if n := compareBaseline(&better, base, 5); n != 0 || better.Legs[0].Baseline.DeltaPct < 4.9 {
		t.Fatalf("lower p99 must count as an improvement: n=%d %+v", n, better.Legs[0].Baseline)
	}
	if n := compareBaseline(&worse, base, 5); n != 1 || !worse.Legs[0].Baseline.Regressed {
		t.Fatalf("higher p99 must regress: n=%d %+v", n, worse.Legs[0].Baseline)
	}
	if worse.Summary.BaselineGbps != 0 || worse.Summary.DeltaPct != 0 {
		t.Fatalf("latency runs have no Gbps aggregate: %+v", worse.Summary)
	}
	if !strings.Contains(regressNote("write_lat", worse.Legs[0].Baseline), "µs p99") {
		t.Fatal("latency note names the unit")
	}
}

func TestSameShape(t *testing.T) {
	a := api.RunSpec{Test: "write", QPs: 1, MTU: 4096, Streams: 1, DurationSec: 60, Name: "x", Clients: []string{"a"}}
	b := a
	b.Name, b.DurationSec, b.Clients = "y", 300, []string{"b"} // name, duration and hosts do not change what a leg measures
	if !sameShape(a, b) {
		t.Fatal("same shape rejected")
	}
	for _, mod := range []func(*api.RunSpec){
		func(s *api.RunSpec) { s.Bidir = true }, func(s *api.RunSpec) { s.QPs = 4 }, func(s *api.RunSpec) { s.Test = "read" }, func(s *api.RunSpec) { s.MTU = 1024 },
	} {
		c := a
		mod(&c)
		if sameShape(a, c) {
			t.Fatalf("different shape accepted: %+v", c)
		}
	}
}

func TestResultLinesForTrends(t *testing.T) {
	at := time.Date(2026, 10, 21, 20, 0, 0, 0, time.UTC)
	r := api.Run{ID: 9, Name: "nightly 1/3: write", Verdict: "warn", Spec: api.RunSpec{Test: "write"}, Summary: api.RunSummary{ToolGbps: 187.5}, Legs: []api.Leg{
		{ClientHost: "dell770ap", ServerHost: "spark-424f", ClientIP: "172.27.100.1", ServerIP: "172.27.100.10", Fabric: "100", MsgSize: 65536, ToolGbps: 97.5, CounterGbps: 99.1, ExpectedGbps: 100, Verdict: "pass",
			Baseline: &api.LegBaseline{RunID: 6, Value: 98, DeltaPct: -0.51}, ErrorDeltas: map[string]int64{"out_of_sequence": 2, "rnr_nak_retry_err": 3}},
		{ClientHost: "dell770ap", ServerHost: "spark-4256", ClientIP: "172.27.101.1", ServerIP: "172.27.101.10", Fabric: "101", MsgSize: 65536, ToolGbps: 90, Verdict: "warn", Switch: &api.LegSwitch{Gbps: 91.2}},
	}}
	out := string(resultLines(r, at))
	for _, want := range []string{
		`srnet_result_gbps{bidir="no",client="dell770ap",client_ip="172.27.100.1",fabric="100",msg_size="65536",run_id="9",server="spark-424f",server_ip="172.27.100.10",test="write",verdict="pass"} 97.5 1792612800000`,
		`srnet_result_counter_gbps{bidir="no",client="dell770ap",client_ip="172.27.100.1",fabric="100",msg_size="65536",run_id="9",server="spark-424f",server_ip="172.27.100.10",test="write",verdict="pass"} 99.1 1792612800000`,
		`srnet_result_baseline_value{bidir="no",client="dell770ap",client_ip="172.27.100.1",fabric="100",msg_size="65536",run_id="9",server="spark-424f",server_ip="172.27.100.10",test="write",verdict="pass"} 98 1792612800000`,
		`srnet_result_delta_pct{bidir="no",client="dell770ap",client_ip="172.27.100.1",fabric="100",msg_size="65536",run_id="9",server="spark-424f",server_ip="172.27.100.10",test="write",verdict="pass"} -0.51 1792612800000`,
		`srnet_result_error_counters{bidir="no",client="dell770ap",client_ip="172.27.100.1",fabric="100",msg_size="65536",run_id="9",server="spark-424f",server_ip="172.27.100.10",test="write",verdict="pass"} 5 1792612800000`,
		`srnet_result_switch_gbps{bidir="no",client="dell770ap",client_ip="172.27.101.1",fabric="101",msg_size="65536",run_id="9",server="spark-4256",server_ip="172.27.101.10",test="write",verdict="warn"} 91.2 1792612800000`,
		`srnet_run_gbps{name="nightly 1/3: write",run_id="9",test="write",verdict="warn"} 187.5 1792612800000`,
		`srnet_run_legs{name="nightly 1/3: write",run_id="9",test="write",verdict="warn"} 2 1792612800000`,
	} {
		if !strings.Contains(out, want+"\n") {
			t.Fatalf("missing line:\n%s\nin:\n%s", want, out)
		}
	}
	if strings.Contains(out, "srnet_result_counter_gbps{bidir=\"no\",client=\"dell770ap\",client_ip=\"172.27.101.1\"") {
		t.Fatal("a leg without a counter rate wrote one")
	}
	if strings.Contains(out, "srnet_result_baseline_value{bidir=\"no\",client=\"dell770ap\",client_ip=\"172.27.101.1\"") {
		t.Fatal("a leg without a baseline wrote one")
	}
}

func TestReportsCarryBaseline(t *testing.T) {
	r := api.Run{ID: 9, Name: "w", Status: "done", Verdict: "warn", Spec: api.RunSpec{Test: "write", PassPct: 90, RegressPct: 5},
		Summary: api.RunSummary{BaselineRunID: 6, BaselineGbps: 98, DeltaPct: -8.2, Regressed: 1},
		Legs:    []api.Leg{{ClientHost: "a", ServerHost: "b", ToolGbps: 90, Verdict: "warn", Baseline: &api.LegBaseline{RunID: 6, Value: 98, DeltaPct: -8.16, Regressed: true}}}}
	var csvOut, htmlOut strings.Builder
	if err := WriteRunCSV(&csvOut, r); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(csvOut.String()), "\n")
	if !strings.HasSuffix(lines[0], ",baseline_run,baseline_value,baseline_delta_pct") || !strings.HasSuffix(lines[1], ",6,98,-8.2") {
		t.Fatalf("csv:\n%s", csvOut.String())
	}
	WriteRunHTML(&htmlOut, r, "test")
	h := htmlOut.String()
	for _, want := range []string{"compared with run 6: 8.2% slower", "1 leg(s) regressed", `98.0 <span class="warn">-8.2%</span>`} {
		if !strings.Contains(h, want) {
			t.Fatalf("html report lacks %q", want)
		}
	}
}
