package agent

import (
	"context"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/Realishbird/SR-Net-Speed-Test/internal/api"
)

// TestRunnerServerClientIperf3 drives a real server and client task over
// loopback the way the controller does: server first, wait for ready, then a
// client with a shared start time. Skipped where iperf3 is not installed.
func TestRunnerServerClientIperf3(t *testing.T) {
	if _, err := exec.LookPath("iperf3"); err != nil {
		t.Skip("iperf3 not installed")
	}
	var mu sync.Mutex
	var events []api.Event
	results := make(chan api.LegResult, 2)
	r := NewRunner(NewSampler(),
		func(e api.Event) { mu.Lock(); events = append(events, e); mu.Unlock() },
		func(res api.LegResult) { results <- res })
	ctx := context.Background()
	base := api.Task{RunID: 1, LegID: 1, Tool: api.ToolIperf3, Protocol: "tcp", DurationSec: 1, Streams: 1, MsgSize: 131072, TCPPort: 25201, LocalIP: "127.0.0.1", PeerIP: "127.0.0.1"}

	srv := base
	srv.ID, srv.Kind = "srv", api.TaskServer
	r.Handle(ctx, srv)
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		ready := false
		for _, e := range events {
			if e.TaskID == "srv" && e.Type == api.EventServerReady {
				ready = true
			}
			if e.TaskID == "srv" && e.Type == api.EventFailed {
				mu.Unlock()
				t.Fatalf("server failed: %s", e.Message)
			}
		}
		mu.Unlock()
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server never reported ready")
		}
		time.Sleep(50 * time.Millisecond)
	}

	cli := base
	cli.ID, cli.Kind, cli.StartAt = "cli", api.TaskClient, time.Now().Add(500*time.Millisecond)
	r.Handle(ctx, cli)
	got := map[string]api.LegResult{}
	for len(got) < 2 {
		select {
		case res := <-results:
			got[res.TaskID] = res
		case <-time.After(20 * time.Second):
			t.Fatalf("results so far: %+v", got)
		}
	}
	for id, res := range got {
		if res.Error != "" {
			t.Errorf("%s: %s\n%s", id, res.Error, res.Output)
		}
	}
	if got["cli"].AvgGbps <= 0 {
		t.Errorf("client reported %.2f Gbps", got["cli"].AvgGbps)
	}
}
