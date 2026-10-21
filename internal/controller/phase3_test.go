package controller

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Realishbird/SR-Net-Speed-Test/internal/api"
)

func TestReleaseMirrorListsOnlyVerifiableBuilds(t *testing.T) {
	dir := t.TempDir()
	bin := []byte("agent binary")
	sum := sha256.Sum256(bin)
	write := func(ver string, files map[string][]byte) {
		os.MkdirAll(filepath.Join(dir, ver), 0o755)
		for name, b := range files {
			os.WriteFile(filepath.Join(dir, ver, name), b, 0o755)
		}
	}
	sums := []byte(hex.EncodeToString(sum[:]) + "  srnet-agent-linux-amd64\n" + hex.EncodeToString(sum[:]) + " *srnet-agent-linux-arm64\n")
	write("v0.3.0", map[string][]byte{"SHA256SUMS": sums, "srnet-agent-linux-amd64": bin, "srnet-agent-linux-arm64": bin})
	write("v0.2.0", map[string][]byte{"SHA256SUMS": sums, "srnet-agent-linux-amd64": bin}) // arm64 missing
	write("v0.1.0", map[string][]byte{"srnet-agent-linux-amd64": bin})                     // no checksums
	write("garbage", map[string][]byte{"SHA256SUMS": sums, "srnet-agent-linux-amd64": bin})

	r := newReleases(dir, nil, newHub(), nil)
	got := r.Mirrored()
	if len(got) != 2 || got[0].Version != "v0.3.0" || got[1].Version != "v0.2.0" {
		t.Fatalf("mirrored = %+v", got)
	}
	if strings.Join(got[0].Arches, ",") != "amd64,arm64" || strings.Join(got[1].Arches, ",") != "amd64" {
		t.Errorf("arches = %v %v", got[0].Arches, got[1].Arches)
	}
	if got[0].SHA256["arm64"] != hex.EncodeToString(sum[:]) {
		t.Errorf("sha = %v", got[0].SHA256)
	}
	if _, _, ok := r.BinaryPath("v0.2.0", "arm64"); ok {
		t.Error("served an arm64 binary that is not mirrored")
	}
	if _, _, ok := r.BinaryPath("../../etc/passwd", "amd64"); ok {
		t.Error("path traversal accepted")
	}
	if p, s, ok := r.BinaryPath("v0.3.0", "amd64"); !ok || s != hex.EncodeToString(sum[:]) || !strings.HasSuffix(p, "v0.3.0/srnet-agent-linux-amd64") {
		t.Errorf("BinaryPath = %q %q %v", p, s, ok)
	}
}

func TestParseCron(t *testing.T) {
	loc := time.FixedZone("test", 0)
	from := time.Date(2026, 10, 21, 1, 30, 0, 0, loc)
	cases := map[string]string{
		"0 2 * * *":    "2026-10-21 02:00",
		"*/15 * * * *": "2026-10-21 01:45",
		"0 6 * * 1":    "2026-10-26 06:00",
		"@daily":       "2026-10-22 00:00",
	}
	for expr, want := range cases {
		s, err := ParseCron(expr)
		if err != nil {
			t.Fatalf("%q: %v", expr, err)
		}
		if got := s.Next(from).In(loc).Format("2006-01-02 15:04"); got != want {
			t.Errorf("%q next = %s, want %s", expr, got, want)
		}
	}
	for _, bad := range []string{"", "0 2 * *", "61 * * * *", "0 2 * * * *"} {
		if _, err := ParseCron(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

func TestHostMatches(t *testing.T) {
	h := hostRec{Host: api.Host{Name: "Spark-01", Hostname: "spark01.lab", Tags: []string{"client", "Rack3"}}}
	for _, sel := range []string{"spark-01", "SPARK01.lab", "tag:client", "tag:rack3"} {
		if !hostMatches(h, sel) {
			t.Errorf("%q should match", sel)
		}
	}
	for _, sel := range []string{"spark-02", "tag:server", "client"} {
		if hostMatches(h, sel) {
			t.Errorf("%q should not match", sel)
		}
	}
}

func TestRunReports(t *testing.T) {
	now := time.Now()
	run := api.Run{ID: 7, Name: "fanout", Status: "done", Verdict: "warn", StartedAt: &now, EndedAt: &now,
		Spec:    api.RunSpec{Test: "write", DurationSec: 60, MsgSize: 65536, QPs: 1, PassPct: 90},
		Summary: api.RunSummary{ToolGbps: 1580.2, CounterGbps: 1575.9, ByFabric: map[string]float64{"A": 790, "B": 790}},
		Legs: []api.Leg{
			{Index: 0, ClientHost: "770AP", ClientIP: "172.27.100.1", ServerHost: "Spark-01", ServerIP: "172.27.100.11", Fabric: "A", MsgSize: 65536, ExpectedGbps: 200, ToolGbps: 197.5, CounterGbps: 197.1234, Verdict: "pass"},
			{Index: 1, ClientHost: "770AP", ClientIP: "172.27.100.2", ServerHost: "Spark-02 \"b\"", ServerIP: "172.27.100.12", Fabric: "B", MsgSize: 65536, ExpectedGbps: 200, ToolGbps: 150, CounterGbps: 149.8,
				ErrorDeltas: map[string]int64{"out_of_sequence": 3, "local_ack_timeout_err": 1}, Verdict: "warn", Note: "75% of line rate <script>"},
		}}
	var csvOut bytes.Buffer
	if err := WriteRunCSV(&csvOut, run); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(csvOut.String()), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "run_id,run_name,test,status,verdict,leg,stage,client,") {
		t.Fatalf("csv:\n%s", csvOut.String())
	}
	// Gbps rounded to 2 places; latency, retransmit, loss and jitter columns empty for a write test.
	if !strings.Contains(lines[1], ",200,197.5,197.12,,,,,,,,,,,pass,") {
		t.Errorf("csv row 1: %s", lines[1])
	}
	if !strings.Contains(lines[2], `"Spark-02 ""b"""`) || !strings.Contains(lines[2], "local_ack_timeout_err=1;out_of_sequence=3") {
		t.Errorf("csv row 2: %s", lines[2])
	}
	var htmlOut bytes.Buffer
	WriteRunHTML(&htmlOut, run, "v0.3.0")
	h := htmlOut.String()
	for _, want := range []string{"<title>Run 7: fanout</title>", "1580.2 Gbps from the tool", "by fabric: A 790.0, B 790.0", `class="warn">WARN`, "&lt;script&gt;", "Spark-02 &#34;b&#34;"} {
		if !strings.Contains(h, want) {
			t.Errorf("html is missing %q", want)
		}
	}
	if strings.Contains(h, "<script>") {
		t.Error("html did not escape a note")
	}
}
