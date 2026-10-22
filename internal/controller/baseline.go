package controller

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/Realishbird/SR-Net-Speed-Test/internal/api"
)

// Baselines (Phase 4). Any finished run can be marked the baseline for its
// shape: the same test and the options that move the numbers (bidirectional,
// QPs, streams, MTU). A later run of that shape is compared leg by leg with
// the newest baseline: a leg matches when its client, server and message
// size are the same, so a fan-out to eight hosts compares against a baseline
// of the same fan-out, and a single-link run against the matching leg of a
// baseline fan-out just as well.

// sameShape says whether two specs produce comparable numbers.
func sameShape(a, b api.RunSpec) bool {
	return a.Test == b.Test && a.Bidir == b.Bidir && a.QPs == b.QPs && a.Streams == b.Streams && a.MTU == b.MTU
}

// legKey identifies a leg across runs.
func legKey(l api.Leg) string {
	return l.ClientHost + "\x00" + l.ServerHost + "\x00" + strconv.Itoa(l.MsgSize)
}

// legMeasure is the number a leg is compared on: tool Gbps for bandwidth
// tests, p99 µs for latency tests (lower is better there).
func legMeasure(test string, l api.Leg) (v float64, lowerIsBetter bool, ok bool) {
	if api.IsLatency(test) {
		if l.Latency == nil || l.Latency.P99Us <= 0 {
			return 0, true, false
		}
		return l.Latency.P99Us, true, true
	}
	if l.ToolGbps <= 0 {
		return 0, false, false
	}
	return l.ToolGbps, false, true
}

// compareBaseline fills each leg's Baseline and the summary's comparison
// from base. regressPct is the allowance below which a leg counts as
// regressed. It returns how many legs regressed.
func compareBaseline(r *api.Run, base api.Run, regressPct float64) int {
	byKey := map[string]api.Leg{}
	for _, l := range base.Legs {
		if l.Verdict == "fail" {
			continue // a failed baseline leg is no reference
		}
		if _, dup := byKey[legKey(l)]; !dup {
			byKey[legKey(l)] = l
		}
	}
	regressed := 0
	var baseSum float64
	var haveSum bool
	lastStage := 0
	for _, l := range r.Legs {
		lastStage = max(lastStage, l.Stage)
	}
	for i := range r.Legs {
		l := &r.Legs[i]
		l.Baseline = nil
		bl, ok := byKey[legKey(*l)]
		if !ok {
			continue
		}
		bv, lower, okB := legMeasure(r.Spec.Test, bl)
		v, _, okV := legMeasure(r.Spec.Test, *l)
		if !okB || !okV {
			continue
		}
		delta := (v - bv) / bv * 100
		if lower {
			delta = -delta // a lower p99 is an improvement
		}
		lb := &api.LegBaseline{RunID: base.ID, Value: bv, DeltaPct: delta, Regressed: delta < -regressPct}
		if lb.Regressed {
			regressed++
		}
		l.Baseline = lb
		if !lower && l.Stage == lastStage {
			baseSum += bv
			haveSum = true
		}
	}
	r.Summary.BaselineRunID = base.ID
	r.Summary.Regressed = regressed
	r.Summary.BaselineGbps, r.Summary.DeltaPct = 0, 0
	if haveSum && baseSum > 0 {
		var mine float64
		for _, l := range r.Legs {
			if l.Baseline != nil && l.Stage == lastStage {
				mine += l.ToolGbps
			}
		}
		r.Summary.BaselineGbps = baseSum
		r.Summary.DeltaPct = (mine - baseSum) / baseSum * 100
	}
	return regressed
}

// regressNote is the note a regressed leg carries.
func regressNote(test string, lb *api.LegBaseline) string {
	unit := "Gbps"
	if api.IsLatency(test) {
		unit = "µs p99"
	}
	return fmt.Sprintf("%.1f%% behind baseline run %d (%.1f %s)", -lb.DeltaPct, lb.RunID, lb.Value, unit)
}

// applyBaseline compares a finished run with its baseline, persists the
// comparison on the legs, and turns a regression into a warning on legs that
// otherwise passed. The run's verdict is returned, possibly lowered to warn.
func (o *Orchestrator) applyBaseline(ctx context.Context, ar *activeRun, verdict string) string {
	base, ok, err := o.store.BaselineFor(ctx, ar.spec, ar.id)
	if err != nil || !ok {
		return verdict
	}
	run := api.Run{ID: ar.id, Spec: ar.spec, Legs: make([]api.Leg, len(ar.plans))}
	for i, p := range ar.plans {
		run.Legs[i] = p.leg
	}
	compareBaseline(&run, base, ar.spec.RegressPct)
	for i, p := range ar.plans {
		p.leg.Baseline = run.Legs[i].Baseline
		if lb := p.leg.Baseline; lb != nil && lb.Regressed && p.leg.Verdict == "pass" {
			p.leg.Verdict = "warn"
			p.leg.Note = regressNote(ar.spec.Test, lb)
			if verdict == "pass" {
				verdict = "warn"
			}
		}
	}
	ar.summary.BaselineRunID, ar.summary.BaselineGbps = run.Summary.BaselineRunID, run.Summary.BaselineGbps
	ar.summary.DeltaPct, ar.summary.Regressed = run.Summary.DeltaPct, run.Summary.Regressed
	return verdict
}

// fillBaseline compares a stored run with its baseline on the fly, for runs
// that finished before a baseline existed (nothing is persisted).
func (s *Server) fillBaseline(ctx context.Context, r *api.Run) {
	if r.Status != "done" || r.Summary.BaselineRunID != 0 || len(r.Legs) == 0 {
		return
	}
	base, ok, err := s.store.BaselineFor(ctx, r.Spec, r.ID)
	if err != nil || !ok {
		return
	}
	pct := r.Spec.RegressPct
	if pct == 0 {
		pct = 5
	}
	compareBaseline(r, base, pct)
}

// resultLines renders a finished run's legs as Prometheus lines stamped at
// the run's end, so Grafana can draw each link's history across runs (the
// Trends dashboard). One sample per leg per run; the value a leg was compared
// against travels with it, so the dashboard can draw the reference line.
func resultLines(r api.Run, at time.Time) []byte {
	var buf bytes.Buffer
	ts := at.UnixMilli()
	yes := map[bool]string{true: "yes", false: "no"}
	for _, l := range r.Legs {
		labels := map[string]string{
			"run_id": strconv.FormatInt(r.ID, 10), "test": r.Spec.Test, "client": l.ClientHost, "server": l.ServerHost,
			"client_ip": l.ClientIP, "server_ip": l.ServerIP, "fabric": l.Fabric, "msg_size": strconv.Itoa(l.MsgSize),
			"verdict": l.Verdict, "bidir": yes[r.Spec.Bidir],
		}
		ls := formatLabels(labels)
		if l.Latency != nil {
			fmt.Fprintf(&buf, "srnet_result_lat_p99_us%s %g %d\n", ls, l.Latency.P99Us, ts)
			fmt.Fprintf(&buf, "srnet_result_lat_avg_us%s %g %d\n", ls, l.Latency.AvgUs, ts)
		} else {
			fmt.Fprintf(&buf, "srnet_result_gbps%s %g %d\n", ls, l.ToolGbps, ts)
			if l.CounterGbps > 0 {
				fmt.Fprintf(&buf, "srnet_result_counter_gbps%s %g %d\n", ls, l.CounterGbps, ts)
			}
			if l.Switch != nil && l.Switch.Gbps > 0 {
				fmt.Fprintf(&buf, "srnet_result_switch_gbps%s %g %d\n", ls, l.Switch.Gbps, ts)
			}
			if l.ExpectedGbps > 0 {
				fmt.Fprintf(&buf, "srnet_result_expected_gbps%s %g %d\n", ls, l.ExpectedGbps, ts)
			}
		}
		if l.Baseline != nil {
			fmt.Fprintf(&buf, "srnet_result_baseline_value%s %g %d\n", ls, l.Baseline.Value, ts)
			fmt.Fprintf(&buf, "srnet_result_delta_pct%s %g %d\n", ls, l.Baseline.DeltaPct, ts)
		}
		var errs int64
		for _, v := range l.ErrorDeltas {
			errs += v
		}
		fmt.Fprintf(&buf, "srnet_result_error_counters%s %d %d\n", ls, errs, ts)
	}
	rl := formatLabels(map[string]string{"run_id": strconv.FormatInt(r.ID, 10), "test": r.Spec.Test, "name": r.Name, "verdict": r.Verdict})
	if !api.IsLatency(r.Spec.Test) {
		fmt.Fprintf(&buf, "srnet_run_gbps%s %g %d\n", rl, r.Summary.ToolGbps, ts)
	}
	fmt.Fprintf(&buf, "srnet_run_legs%s %d %d\n", rl, len(r.Legs), ts)
	return buf.Bytes()
}

// publishResult writes a finished run's result series to VictoriaMetrics.
func (o *Orchestrator) publishResult(ctx context.Context, id int64) {
	r, err := o.store.Run(ctx, id)
	if err != nil || o.metrics == nil {
		return
	}
	at := time.Now()
	if r.EndedAt != nil {
		at = *r.EndedAt
	}
	o.metrics.push(resultLines(r, at))
}
