package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Realishbird/SR-Net-Speed-Test/internal/api"
)

// incastLab: eight 100G Sparks on fabric 100 and a 400G 770AP port.
func incastLab() []hostRec {
	mk := func(id int64, name string, speed int, ip string, tags ...string) hostRec {
		h := hostRec{}
		h.ID, h.Name, h.Hostname, h.Tags = id, name, strings.ToLower(name), tags
		h.Ports = []api.HostPort{{ID: id * 10, RdmaDev: "mlx5_0", PortNum: 1, Netdev: "eth0", Addrs: []string{ip}, State: "ACTIVE", SpeedMbps: speed, Fabric: "100"}}
		return h
	}
	hosts := []hostRec{mk(1, "dell770ap", 400000, "172.27.100.1/24", "client")}
	for i := 0; i < 8; i++ {
		hosts = append(hosts, mk(int64(i+2), "spark-"+string(rune('a'+i)), 100000, "172.27.100."+string(rune('1'))+string(rune('0'+i))+"/24", "spark"))
	}
	return hosts
}

func TestFairShareCapsSharedPorts(t *testing.T) {
	// Fan-out from a 400G port to eight 100G hosts: 400/8 = 50 per leg.
	plans, err := plan(incastLab(), api.RunSpec{Clients: []string{"dell770ap"}, Servers: []string{"tag:spark"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 8 || plans[0].leg.ExpectedGbps != 50 || !strings.HasPrefix(plans[0].leg.SharedPort, "dell770ap 172.27.100.1 x8") {
		t.Fatalf("fan-out: %d legs, leg 0 %+v", len(plans), plans[0].leg)
	}
	// Four hosts on the same 400G port get a full 100 each: no cap.
	plans, _ = plan(incastLab(), api.RunSpec{Clients: []string{"dell770ap"}, Servers: []string{"spark-a", "spark-b", "spark-c", "spark-d"}})
	if plans[0].leg.ExpectedGbps != 100 || plans[0].leg.SharedPort != "" {
		t.Fatalf("four on a 400G port: %+v", plans[0].leg)
	}
	// Incast of eight into one 100G Spark: 12.5 per leg, doubled when bidirectional.
	servers := []string{"spark-a"}
	var clients []string
	for _, h := range incastLab()[2:] {
		clients = append(clients, h.Name)
	}
	plans, err = plan(incastLab(), api.RunSpec{Clients: clients, Servers: servers})
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 7 || plans[0].leg.ExpectedGbps != 100.0/7 || !strings.Contains(plans[0].leg.SharedPort, "spark-a 172.27.100.10 x7") {
		t.Fatalf("incast: %d legs, leg 0 %+v", len(plans), plans[0].leg)
	}
	plans, _ = plan(incastLab(), api.RunSpec{Clients: clients, Servers: servers, Bidir: true})
	if plans[0].leg.ExpectedGbps != 2*100.0/7 {
		t.Fatalf("bidir incast: %+v", plans[0].leg)
	}
	// Serial legs never share a port within a stage.
	plans, _ = plan(incastLab(), api.RunSpec{Clients: clients, Servers: servers, Serial: true})
	if plans[0].leg.ExpectedGbps != 100 || plans[0].leg.SharedPort != "" {
		t.Fatalf("serial: %+v", plans[0].leg)
	}
}

func TestScoreFairnessAndCongestion(t *testing.T) {
	o := &Orchestrator{metrics: newMetrics("")}
	servers := []string{"spark-a"}
	var clients []string
	for _, h := range incastLab()[2:] {
		clients = append(clients, h.Name)
	}
	plans, err := plan(incastLab(), api.RunSpec{Clients: clients, Servers: servers})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now().Add(-70 * time.Second)
	for i, p := range plans {
		p.startAt = start
		p.clientRes = &api.LegResult{AvgGbps: 14.2 - float64(i)*0.15, ErrorDeltas: map[string]int64{"np_ecn_marked_roce_packets": 100, "rp_cnp_handled": 7}}
		p.serverRes = &api.LegResult{ErrorDeltas: map[string]int64{"np_cnp_sent": 9}}
	}
	plans[0].leg.Switch = nil
	ar := &activeRun{id: 1, spec: api.RunSpec{Test: "write", DurationSec: 60, PassPct: 90}, plans: plans, stages: stagesOf(plans)}
	sum, verdict := o.score(ar)
	if verdict != "pass" {
		t.Fatalf("verdict %s: %+v", verdict, plans[6].leg)
	}
	// Slowest 13.3 over fastest 14.2.
	if sum.Fairness < 0.936 || sum.Fairness > 0.937 {
		t.Fatalf("fairness %.3f", sum.Fairness)
	}
	if sum.Congestion["ecn marked"] != 700 || sum.Congestion["cnp sent"] != 63 || sum.Congestion["cnp handled"] != 49 {
		t.Fatalf("congestion %+v", sum.Congestion)
	}
	// Congestion counters are reported, not faults.
	if plans[0].leg.Verdict != "pass" || plans[0].leg.Note != "" {
		t.Fatalf("leg 0: %+v", plans[0].leg)
	}
	// A single leg has no fairness figure.
	one, _ := plan(incastLab(), api.RunSpec{Clients: []string{"spark-b"}, Servers: servers})
	one[0].startAt, one[0].clientRes, one[0].serverRes = start, &api.LegResult{AvgGbps: 98}, &api.LegResult{}
	sum, _ = o.score(&activeRun{id: 2, spec: ar.spec, plans: one, stages: stagesOf(one)})
	if sum.Fairness != 0 || sum.Congestion != nil {
		t.Fatalf("single leg: %+v", sum)
	}
}

func TestPlanCollectiveRanksOnOneFabric(t *testing.T) {
	lab := incastLab()
	plans, err := plan(lab, api.RunSpec{Test: "nccl_allreduce", Hosts: []string{"spark-a", "spark-b", "spark-c"}, MsgSize: 64 << 20, Sizes: []int{1 << 20, 64 << 20}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 2 || plans[0].server.Host != "spark-a" || plans[0].client.Host != "spark-b" || plans[1].client.Host != "spark-c" {
		t.Fatalf("ranks: %+v", plans)
	}
	for _, p := range plans {
		if p.leg.ExpectedGbps != 100 || p.leg.SharedPort != "" || p.stage != 0 || p.leg.MsgSize != 64<<20 {
			t.Errorf("leg %+v", p.leg)
		}
	}
	// Clients and servers together serve as the rank list when hosts are not given.
	plans, err = plan(lab, api.RunSpec{Test: "nccl_allgather", Clients: []string{"dell770ap"}, Servers: []string{"tag:spark"}})
	if err != nil || len(plans) != 8 || plans[0].server.Host != "dell770ap" || plans[0].leg.ExpectedGbps != 100 {
		t.Fatalf("clients+servers: %v, %d plans", err, len(plans))
	}
	if _, err := plan(lab, api.RunSpec{Test: "nccl_allreduce", Hosts: []string{"spark-a"}}); err == nil || !strings.Contains(err.Error(), "two hosts") {
		t.Errorf("one host: %v", err)
	}
	// A host on another subnet cannot join the ring.
	other := lab[0]
	other.ID, other.Name, other.Hostname = 99, "far", "far"
	other.Ports = []api.HostPort{{ID: 990, RdmaDev: "mlx5_0", PortNum: 1, Netdev: "eth0", Addrs: []string{"172.27.101.5/24"}, State: "ACTIVE", SpeedMbps: 100000, Fabric: "101"}}
	if _, err := plan(append(lab, other), api.RunSpec{Test: "nccl_allreduce", Hosts: []string{"spark-a", "spark-b", "far"}}); err == nil || !strings.Contains(err.Error(), "rank 2") {
		t.Errorf("other subnet: %v", err)
	}
	spec := api.RunSpec{Test: "nccl_allreduce", Bidir: true}
	if err := applyDefaults(&spec); err == nil {
		t.Error("bidir collective accepted")
	}
	spec = api.RunSpec{Test: "nccl_allgather", Serial: true, Mesh: true}
	if err := applyDefaults(&spec); err != nil || spec.MsgSize != 64<<20 || spec.Serial || spec.Mesh {
		t.Errorf("defaults: %v %+v", err, spec)
	}
}

// TestRunCollectiveRanksAndScore drives a three-rank all-reduce through the
// hub with a fake agent per host: rank 0 is a server task that reports ready,
// the others start as clients once it has, every rank gets the same
// rendezvous port and sizes, and the collective-wide figures end up on
// every leg with the sweep points.
func TestRunCollectiveRanksAndScore(t *testing.T) {
	lab := incastLab()
	spec := api.RunSpec{Test: "nccl_allreduce", Hosts: []string{"spark-a", "spark-b", "spark-c"}, DurationSec: 10, PassPct: 90}
	if err := applyDefaults(&spec); err != nil {
		t.Fatal(err)
	}
	spec.Sizes = []int{1 << 20, 64 << 20}
	plans, err := plan(lab, spec)
	if err != nil {
		t.Fatal(err)
	}
	h := newHub()
	o := newOrchestrator(nil, h, newMetrics(""))
	for i, p := range plans {
		p.leg.ID = int64(i + 1)
		p.tcpPort = 18515
	}
	ar := &activeRun{id: 7, spec: spec, plans: plans, stages: stagesOf(plans)}
	var tasks []api.Task
	done := make(chan error, 1)
	go func() { done <- o.runCollective(context.Background(), ar, plans) }()
	// Rank 0's agent (spark-a, id 2) gets the server task first.
	got := h.poll(context.Background(), 2, 5*time.Second)
	if len(got) != 1 || got[0].Kind != api.TaskServer || got[0].Rank != 0 || got[0].WorldSize != 3 || got[0].MasterIP != "172.27.100.10" || got[0].Collective != "allreduce" || len(got[0].Sizes) != 2 {
		t.Fatalf("rank 0 task: %+v", got)
	}
	t0 := got[0]
	tasks = append(tasks, t0)
	// Nobody else has a task until rank 0 is ready.
	if early := h.poll(context.Background(), 3, 300*time.Millisecond); len(early) != 0 {
		t.Fatalf("rank 1 started before rank 0 was ready: %+v", early)
	}
	h.deliver(2, msg{taskID: t0.ID, event: &api.Event{Type: api.EventServerReady}})
	for _, id := range []int64{3, 4} {
		got := h.poll(context.Background(), id, 5*time.Second)
		if len(got) != 1 || got[0].Kind != api.TaskClient || got[0].TCPPort != 18515 || got[0].MasterIP != "172.27.100.10" || got[0].Rank != int(id-2) || got[0].StartAt.IsZero() {
			t.Fatalf("agent %d task: %+v", id, got)
		}
		tasks = append(tasks, got[0])
	}
	points := map[string]float64{"1048576": 61.5, "67108864": 94.2}
	for i, tk := range tasks {
		h.deliver(int64(i+2), msg{taskID: tk.ID, result: &api.LegResult{TaskID: tk.ID, AvgGbps: 94.2, PeakGbps: 94.2, Points: points}})
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runCollective did not finish")
	}
	for _, p := range plans {
		if p.serverRes == nil || p.clientRes == nil || p.serverRes.TaskID != t0.ID {
			t.Fatalf("leg results: %+v %+v", p.serverRes, p.clientRes)
		}
	}
	sum, verdict := o.score(ar)
	if verdict != "pass" || sum.ToolGbps != 94.2 || sum.Fairness != 0 || sum.ByFabric["100"] != 94.2 {
		t.Errorf("verdict %s, summary %+v", verdict, sum)
	}
	for _, p := range plans {
		if p.leg.ToolGbps != 94.2 || p.leg.Points["1048576"] != 61.5 || p.leg.Verdict != "pass" {
			t.Errorf("leg %+v", p.leg)
		}
	}
	// Half the line rate fails like any other leg.
	for _, p := range plans {
		p.clientRes.AvgGbps = 48
	}
	if _, verdict := o.score(ar); verdict != "fail" {
		t.Errorf("48 of 100 Gbps passed")
	}
}
