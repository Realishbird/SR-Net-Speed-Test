package controller

import (
	"encoding/csv"
	"fmt"
	"html"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Realishbird/SR-Net-Speed-Test/internal/api"
)

// WriteRunCSV writes one row per leg.
func WriteRunCSV(w io.Writer, r api.Run) error {
	cw := csv.NewWriter(w)
	head := []string{"run_id", "run_name", "test", "status", "verdict", "leg", "stage", "client", "client_ip", "server", "server_ip", "fabric", "msg_size", "expected_gbps", "tool_gbps", "counter_gbps",
		"lat_min_us", "lat_typical_us", "lat_avg_us", "lat_p99_us", "lat_p999_us", "lat_max_us", "retransmits", "loss_pct", "jitter_ms", "error_deltas", "leg_verdict", "note"}
	if err := cw.Write(head); err != nil {
		return err
	}
	for _, l := range r.Legs {
		// Columns that do not apply to this test stay empty rather than 0.
		latCols := make([]string, 6)
		if lat := l.Latency; lat != nil {
			latCols = []string{f(lat.MinUs, 3), f(lat.TypicalUs, 3), f(lat.AvgUs, 3), f(lat.P99Us, 3), f(lat.P999Us, 3), f(lat.MaxUs, 3)}
		}
		var retrans, loss, jitter string
		switch r.Spec.Test {
		case "tcp":
			retrans = strconv.FormatInt(l.Retransmits, 10)
		case "udp":
			loss, jitter = f(l.LossPct, 3), f(l.JitterMs, 3)
		}
		row := []string{
			strconv.FormatInt(r.ID, 10), r.Name, r.Spec.Test, r.Status, r.Verdict,
			strconv.Itoa(l.Index + 1), strconv.Itoa(l.Stage + 1), l.ClientHost, l.ClientIP, l.ServerHost, l.ServerIP, l.Fabric, strconv.Itoa(l.MsgSize),
			f(l.ExpectedGbps, 2), f(l.ToolGbps, 2), f(l.CounterGbps, 2),
		}
		row = append(row, latCols...)
		row = append(row, retrans, loss, jitter, errorDeltas(l.ErrorDeltas), l.Verdict, l.Note)
		if err := cw.Write(row); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// f rounds to prec decimals and drops trailing zeros: 99.25583 -> 99.26, 100 -> 100.
func f(v float64, prec int) string {
	return strconv.FormatFloat(math.Round(v*math.Pow10(prec))/math.Pow10(prec), 'f', -1, 64)
}

func errorDeltas(m map[string]int64) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s=%d", k, m[k])
	}
	return strings.Join(parts, ";")
}

// WriteRunHTML writes a self-contained report page (print to PDF from the browser).
func WriteRunHTML(w io.Writer, r api.Run, controllerVersion string) {
	e := html.EscapeString
	lat := api.IsLatency(r.Spec.Test)
	s := r.Spec
	fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><title>Run %d: %s</title>
<style>body{font:13px/1.4 system-ui,sans-serif;margin:32px;color:#111}h1{font-size:20px;margin:0 0 4px}table{border-collapse:collapse;width:100%%;margin-top:16px}
th,td{border:1px solid #ccc;padding:5px 8px;text-align:left}th{background:#f2f2f2;font-size:11px;text-transform:uppercase}td.n,th.n{text-align:right;font-variant-numeric:tabular-nums}
.pass{color:#15803d;font-weight:600}.warn{color:#b45309;font-weight:600}.fail{color:#b91c1c;font-weight:600}.muted{color:#666}.kv{display:grid;grid-template-columns:max-content 1fr;gap:2px 16px;margin-top:12px}
@media print{body{margin:12mm}}</style></head><body>`, r.ID, e(r.Name))
	fmt.Fprintf(w, `<h1>Run %d: %s</h1><div class="muted">SR-Net-Speed-Test report · generated %s · controller %s</div>`, r.ID, e(r.Name), time.Now().Format("2006-01-02 15:04 MST"), e(controllerVersion))
	fmt.Fprintf(w, `<div class="kv"><b>Result</b><span><span class="%s">%s</span> (%s)%s</span>`, e(r.Verdict), e(strings.ToUpper(nonEmpty(r.Verdict, "none"))), e(r.Status), errHTML(r.Error))
	fmt.Fprintf(w, `<b>Test</b><span>%s · %d s · message size %d%s · qps %d%s%s%s%s</span>`, e(s.Test), s.DurationSec, s.MsgSize, sizesHTML(s.Sizes), s.QPs,
		optHTML(s.Streams > 1, fmt.Sprintf(" · %d streams", s.Streams)), optHTML(s.Bidir, " · bidirectional (rates add both directions)"), optHTML(s.Serial, " · serial"), optHTML(s.Mesh, " · full mesh"))
	fmt.Fprintf(w, `<b>Thresholds</b><span>%.0f%% of line rate%s%s</span>`, s.PassPct, optHTML(s.MaxLatP99Us > 0, fmt.Sprintf(" · p99 ≤ %.2f µs", s.MaxLatP99Us)), optHTML(s.Test == "udp", fmt.Sprintf(" · loss ≤ %.2f%%", s.MaxLossPct)))
	if r.StartedAt != nil {
		end := ""
		if r.EndedAt != nil {
			end = " to " + r.EndedAt.Local().Format("15:04:05")
		}
		fmt.Fprintf(w, `<b>When</b><span>%s%s</span>`, r.StartedAt.Local().Format("2006-01-02 15:04:05 MST"), end)
	}
	if !lat {
		fmt.Fprintf(w, `<b>Aggregate</b><span>%.1f Gbps from the tool, %.1f Gbps from hardware counters%s</span>`, r.Summary.ToolGbps, r.Summary.CounterGbps, fabricHTML(r.Summary.ByFabric))
	}
	fmt.Fprint(w, `</div><table><tr><th>#</th><th>Client</th><th>Server</th><th>Fabric</th><th class="n">Size</th>`)
	if lat {
		fmt.Fprint(w, `<th class="n">Min µs</th><th class="n">Typical µs</th><th class="n">Avg µs</th><th class="n">P99 µs</th><th class="n">P99.9 µs</th><th class="n">Max µs</th>`)
	} else {
		fmt.Fprint(w, `<th class="n">Expected Gbps</th><th class="n">Tool Gbps</th><th class="n">Counter Gbps</th>`)
		if s.Test == "tcp" {
			fmt.Fprint(w, `<th class="n">Retransmits</th>`)
		} else if s.Test == "udp" {
			fmt.Fprint(w, `<th class="n">Loss %</th><th class="n">Jitter ms</th>`)
		}
	}
	fmt.Fprint(w, `<th>Error counters</th><th>Verdict</th><th>Note</th></tr>`)
	for _, l := range r.Legs {
		fmt.Fprintf(w, `<tr><td>%d</td><td>%s <span class="muted">%s</span></td><td>%s <span class="muted">%s</span></td><td>%s</td><td class="n">%d</td>`,
			l.Index+1, e(l.ClientHost), e(l.ClientIP), e(l.ServerHost), e(l.ServerIP), e(l.Fabric), l.MsgSize)
		if lat {
			t := l.Latency
			if t == nil {
				t = &api.Latency{}
			}
			fmt.Fprintf(w, `<td class="n">%.2f</td><td class="n">%.2f</td><td class="n">%.2f</td><td class="n">%.2f</td><td class="n">%.2f</td><td class="n">%.2f</td>`, t.MinUs, t.TypicalUs, t.AvgUs, t.P99Us, t.P999Us, t.MaxUs)
		} else {
			fmt.Fprintf(w, `<td class="n">%.0f</td><td class="n">%.1f</td><td class="n">%.1f</td>`, l.ExpectedGbps, l.ToolGbps, l.CounterGbps)
			if s.Test == "tcp" {
				fmt.Fprintf(w, `<td class="n">%d</td>`, l.Retransmits)
			} else if s.Test == "udp" {
				fmt.Fprintf(w, `<td class="n">%.2f</td><td class="n">%.3f</td>`, l.LossPct, l.JitterMs)
			}
		}
		fmt.Fprintf(w, `<td class="muted">%s</td><td class="%s">%s</td><td class="muted">%s</td></tr>`, e(strings.ReplaceAll(errorDeltas(l.ErrorDeltas), ";", ", ")), e(l.Verdict), e(l.Verdict), e(l.Note))
	}
	fmt.Fprint(w, `</table></body></html>`)
}

func errHTML(s string) string {
	if s == "" {
		return ""
	}
	return ` · <span class="fail">` + html.EscapeString(s) + `</span>`
}

func sizesHTML(sizes []int) string {
	if len(sizes) == 0 {
		return ""
	}
	parts := make([]string, len(sizes))
	for i, s := range sizes {
		parts[i] = strconv.Itoa(s)
	}
	return " (sweep " + strings.Join(parts, ", ") + ")"
}

func optHTML(on bool, s string) string {
	if on {
		return html.EscapeString(s)
	}
	return ""
}

func fabricHTML(m map[string]float64) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s %.1f", html.EscapeString(k), m[k])
	}
	return " · by fabric: " + strings.Join(parts, ", ")
}
