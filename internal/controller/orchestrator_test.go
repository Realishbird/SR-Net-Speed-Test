package controller

import (
	"strings"
	"testing"
	"time"

	"github.com/Realishbird/SR-Net-Speed-Test/internal/api"
)

func lab() []hostRec {
	mk := func(id int64, name string, tags []string, ports ...api.HostPort) hostRec {
		h := hostRec{}
		h.ID, h.Name, h.Hostname, h.Tags, h.Ports = id, name, strings.ToLower(name), tags, ports
		return h
	}
	port := func(id int64, dev, ip, fabric string) api.HostPort {
		return api.HostPort{ID: id, RdmaDev: dev, PortNum: 1, Netdev: "eth" + dev, Addrs: []string{ip}, State: "ACTIVE", SpeedMbps: 200000, Fabric: fabric}
	}
	return []hostRec{
		mk(1, "Dell 770AP", []string{"client"}, port(10, "mlx5_0", "172.27.100.1/24", "100"), port(11, "mlx5_1", "172.27.101.1/24", "101")),
		mk(2, "Dell Spark 1", []string{"server", "spark"}, port(20, "mlx5_0", "172.27.100.10/24", "100")),
		mk(3, "Dell Spark 2", []string{"server", "spark"}, port(30, "mlx5_0", "172.27.101.10/24", "101")),
		mk(4, "Acer Spark", []string{"spark"}, port(40, "mlx5_0", "172.27.100.13/24", "")),
	}
}

func TestPlanFanOutPicksSubnetMatchedPorts(t *testing.T) {
	spec := api.RunSpec{Clients: []string{"Dell 770AP"}, Servers: []string{"tag:server"}}
	plans, err := plan(lab(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 2 {
		t.Fatalf("got %d legs", len(plans))
	}
	if plans[0].client.IP != "172.27.100.1" || plans[0].server.IP != "172.27.100.10" || plans[0].leg.Fabric != "100" {
		t.Errorf("leg 0 = %+v", plans[0].leg)
	}
	if plans[1].client.IP != "172.27.101.1" || plans[1].server.IP != "172.27.101.10" || plans[1].leg.Fabric != "101" {
		t.Errorf("leg 1 = %+v", plans[1].leg)
	}
	if plans[0].leg.ExpectedGbps != 200 {
		t.Errorf("expected gbps should default to link speed, got %v", plans[0].leg.ExpectedGbps)
	}
}

func TestPlanSelectorsAndErrors(t *testing.T) {
	hosts := lab()
	if _, err := plan(hosts, api.RunSpec{Clients: []string{"172.27.100.1"}, Servers: []string{"172.27.101.10"}}); err == nil {
		t.Error("ports on different subnets must not pair")
	}
	if _, err := plan(hosts, api.RunSpec{Clients: []string{"Dell 770AP"}, Servers: []string{"Dell 770AP"}}); err == nil {
		t.Error("same host both sides must fail")
	}
	if _, err := plan(hosts, api.RunSpec{Clients: []string{"nope"}, Servers: []string{"Dell Spark 1"}}); err == nil {
		t.Error("unknown selector must fail")
	}
	p, err := plan(hosts, api.RunSpec{Legs: []api.LegSpec{{Client: "172.27.100.13", Server: "dell spark 1"}}})
	if err != nil {
		t.Fatal(err)
	}
	if p[0].client.Host != "Acer Spark" || p[0].leg.Fabric != "100" {
		t.Errorf("leg = %+v", p[0].leg)
	}
	hosts[1].Ports[0].State = "DOWN"
	if _, err := plan(hosts, api.RunSpec{Clients: []string{"Dell 770AP"}, Servers: []string{"Dell Spark 1"}}); err == nil || !strings.Contains(err.Error(), "not ACTIVE") {
		t.Errorf("down link should be refused, got %v", err)
	}
}

func TestScoreUsesCountersAndErrorDeltas(t *testing.T) {
	m := newMetrics("")
	o := &Orchestrator{metrics: m}
	plans, err := plan(lab(), api.RunSpec{Clients: []string{"Dell 770AP"}, Servers: []string{"tag:server"}})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now().Add(-70 * time.Second)
	for _, p := range plans {
		p.startAt = start
	}
	ar := &activeRun{id: 1, spec: api.RunSpec{Test: "write", DurationSec: 60, PassPct: 90}, plans: plans, stages: stagesOf(plans)}
	// One sample per second, as agents send during a run.
	fill := func(portID int64, gbps float64) {
		m.ring[portID] = nil
		for sec := 0; sec <= 60; sec++ {
			m.track(portID, api.Sample{Time: start.Add(time.Duration(sec) * time.Second),
				Values: map[string]float64{"rdma_tx_bytes": 0, "rdma_rx_bytes": gbps * 1e9 / 8 * float64(sec)}})
		}
	}
	fill(20, 190) // server port of leg 0
	fill(30, 100) // server port of leg 1
	plans[0].clientRes = &api.LegResult{AvgGbps: 191}
	plans[0].serverRes = &api.LegResult{ErrorDeltas: map[string]int64{"np_cnp_sent": 50}}
	plans[1].clientRes = &api.LegResult{AvgGbps: 101, ErrorDeltas: map[string]int64{"out_of_sequence": 2}}
	plans[1].serverRes = &api.LegResult{}

	sum, verdict := o.score(ar)
	if verdict != "fail" {
		t.Errorf("verdict = %s", verdict)
	}
	l0, l1 := plans[0].leg, plans[1].leg
	if l0.Verdict != "pass" || l0.CounterGbps < 185 || l0.CounterGbps > 195 {
		t.Errorf("leg 0 = %+v", l0)
	}
	if l1.Verdict != "fail" || !strings.Contains(l1.Note, "below 90%") {
		t.Errorf("leg 1 = %+v", l1)
	}
	if sum.ToolGbps != 292 || sum.ByFabric["100"] != 191 || sum.ByFabric["101"] != 101 {
		t.Errorf("summary = %+v", sum)
	}
	// Two packets resent out of some 357 million: a pass that says so.
	plans[1].clientRes.AvgGbps = 195
	fill(30, 195)
	_, verdict = o.score(ar)
	if verdict != "pass" || !strings.Contains(plans[1].leg.Note, "out_of_sequence +2, about 1 in 179 million") {
		t.Errorf("verdict = %s, leg = %+v", verdict, plans[1].leg)
	}
	// Above 1 in a million: warn.
	plans[1].clientRes.ErrorDeltas = map[string]int64{"out_of_sequence": 2000, "packet_seq_err": 9}
	_, verdict = o.score(ar)
	if verdict != "warn" || plans[1].leg.Note != "error counters grew: out_of_sequence +2000, packet_seq_err +9" {
		t.Errorf("verdict = %s, leg = %+v", verdict, plans[1].leg)
	}
	// Any other error counter warns at once; on a send test the teardown
	// counters are named as normal next to it.
	ar.spec.Test = "send"
	plans[1].clientRes.ErrorDeltas = map[string]int64{"out_of_sequence": 3, "local_ack_timeout_err": 1, "resp_cqe_error": 270}
	_, verdict = o.score(ar)
	if want := "error counters grew: local_ack_timeout_err +1; a few packets lost and resent (out_of_sequence +3, about 1 in 119 million, under 1 in a million); normal for a send test: resp_cqe_error +270"; verdict != "warn" || plans[1].leg.Note != want {
		t.Errorf("verdict = %s, note = %q\nwant %q", verdict, plans[1].leg.Note, want)
	}
}

func TestScoreBidirCountsBothDirections(t *testing.T) {
	m := newMetrics("")
	o := &Orchestrator{metrics: m}
	hosts := lab()
	hosts[0].Ports[0].SpeedMbps, hosts[1].Ports[0].SpeedMbps = 100000, 100000
	spec := api.RunSpec{Test: "write", Bidir: true, DurationSec: 60, PassPct: 90, Clients: []string{"Dell Spark 1"}, Servers: []string{"Dell 770AP"}}
	plans, err := plan(hosts, spec)
	if err != nil {
		t.Fatal(err)
	}
	// Both directions add up, so a 100G link is expected to reach 200.
	if plans[0].leg.ExpectedGbps != 200 {
		t.Fatalf("expected = %v", plans[0].leg.ExpectedGbps)
	}
	start := time.Now().Add(-70 * time.Second)
	plans[0].startAt = start
	for sec := 0; sec <= 60; sec++ {
		b := 1e9 / 8 * float64(sec)
		m.track(10, api.Sample{Time: start.Add(time.Duration(sec) * time.Second),
			Values: map[string]float64{"rdma_tx_bytes": 98 * b, "rdma_rx_bytes": 99 * b}})
	}
	plans[0].clientRes = &api.LegResult{AvgGbps: 195.3}
	plans[0].serverRes = &api.LegResult{}
	ar := &activeRun{id: 1, spec: spec, plans: plans, stages: stagesOf(plans)}
	sum, verdict := o.score(ar)
	l := plans[0].leg
	if verdict != "pass" || l.CounterGbps < 196 || l.CounterGbps > 198 || sum.CounterGbps != l.CounterGbps {
		t.Errorf("verdict = %s, leg = %+v", verdict, l)
	}
	// A link that only reaches line rate one way is a fail in bidirectional mode.
	plans[0].clientRes.AvgGbps = 100
	m.ring[10] = nil
	for sec := 0; sec <= 60; sec++ {
		m.track(10, api.Sample{Time: start.Add(time.Duration(sec) * time.Second),
			Values: map[string]float64{"rdma_tx_bytes": 0, "rdma_rx_bytes": 99 * 1e9 / 8 * float64(sec)}})
	}
	if _, verdict = o.score(ar); verdict != "fail" {
		t.Errorf("one-way traffic should fail a bidirectional run, got %s (%+v)", verdict, plans[0].leg)
	}
}

func TestFormatLabelsAndMetricNames(t *testing.T) {
	got := formatLabels(map[string]string{"host": "Dell Spark 1", "fabric": "", "netdev": "enp1s0f0np0"})
	if got != `{host="Dell Spark 1",netdev="enp1s0f0np0"}` {
		t.Errorf("labels = %s", got)
	}
	if metricName("dom_tx_bias_ma") != "dom_tx_bias_ma" || metricName("bad-name.x") != "bad_name_x" {
		t.Error("metric names not sanitized")
	}
}

func TestPlanMeshSweepAndSerial(t *testing.T) {
	hosts := lab()
	spec := api.RunSpec{Test: "write", Mesh: true, Hosts: []string{"Dell 770AP", "Dell Spark 1", "Acer Spark"}, MsgSize: 65536}
	if err := applyDefaults(&spec); err != nil {
		t.Fatal(err)
	}
	plans, err := plan(hosts, spec)
	if err != nil {
		t.Fatal(err)
	}
	// 770AP<->Spark1 and 770AP<->Acer share subnet 100; Spark1<->Acer too: 6 ordered pairs.
	if len(plans) != 6 || len(stagesOf(plans)) != 1 {
		t.Fatalf("mesh: %d legs, %d stages", len(plans), len(stagesOf(plans)))
	}

	sweep := api.RunSpec{Test: "write", Clients: []string{"Dell 770AP"}, Servers: []string{"tag:server"}, Sizes: []int{4096, 65536, 1048576}}
	applyDefaults(&sweep)
	plans, err = plan(hosts, sweep)
	if err != nil {
		t.Fatal(err)
	}
	st := stagesOf(plans)
	if len(plans) != 6 || len(st) != 3 || st[1][0].leg.MsgSize != 65536 || st[2][1].leg.MsgSize != 1048576 {
		t.Errorf("sweep: %d legs, %d stages", len(plans), len(st))
	}

	serial := sweep
	serial.Sizes, serial.Serial = nil, true
	applyDefaults(&serial)
	plans, _ = plan(hosts, serial)
	if st := stagesOf(plans); len(st) != 2 || len(st[0]) != 1 || len(st[1]) != 1 {
		t.Errorf("serial: %d stages", len(st))
	}
	// In a serial matrix each leg has its own stage, so even a shared client port is unshared per stage.
	ar := &activeRun{spec: serial, plans: plans, stages: stagesOf(plans)}
	if id, dir, nic, ok := counterSide(ar, plans[0]); !ok || dir != dirRx || nic || id != 20 {
		t.Errorf("counterSide = %d %v %v %v", id, dir, nic, ok)
	}
}

func TestApplyDefaultsPerTest(t *testing.T) {
	cases := map[string]struct {
		size int
		tool string
	}{"write": {65536, api.ToolWriteBW}, "read_lat": {2, api.ToolReadLat}, "tcp": {131072, api.ToolIperf3}, "udp": {8192, api.ToolIperf3}}
	for test, want := range cases {
		s := api.RunSpec{Test: test}
		if err := applyDefaults(&s); err != nil {
			t.Fatal(err)
		}
		if s.MsgSize != want.size || api.Tests[s.Test] != want.tool {
			t.Errorf("%s: size %d tool %s", test, s.MsgSize, api.Tests[s.Test])
		}
	}
	bad := api.RunSpec{Test: "ping"}
	if err := applyDefaults(&bad); err == nil {
		t.Error("unknown test must be rejected")
	}
	bidirLat := api.RunSpec{Test: "write_lat", Bidir: true}
	if err := applyDefaults(&bidirLat); err == nil {
		t.Error("bidir latency must be rejected")
	}
}

func TestScoreLatencyAndUDP(t *testing.T) {
	o := &Orchestrator{metrics: newMetrics("")}
	plans, _ := plan(lab(), api.RunSpec{Clients: []string{"Dell 770AP"}, Servers: []string{"Dell Spark 1"}})
	for _, p := range plans {
		p.startAt = time.Now().Add(-30 * time.Second)
	}
	ar := &activeRun{spec: api.RunSpec{Test: "write_lat", DurationSec: 20, MaxLatP99Us: 5}, plans: plans, stages: stagesOf(plans)}
	plans[0].clientRes = &api.LegResult{Latency: &api.Latency{P99Us: 7.5}}
	plans[0].serverRes = &api.LegResult{}
	if _, v := o.score(ar); v != "fail" || !strings.Contains(plans[0].leg.Note, "p99 7.50") {
		t.Errorf("latency verdict %s note %q", v, plans[0].leg.Note)
	}
	ar.spec = api.RunSpec{Test: "udp", DurationSec: 20, MaxLossPct: 1, PassPct: 90}
	plans[0].clientRes = &api.LegResult{AvgGbps: 20, LossPct: 2.5}
	if _, v := o.score(ar); v != "fail" || !strings.Contains(plans[0].leg.Note, "loss") {
		t.Errorf("udp verdict %s note %q", v, plans[0].leg.Note)
	}
	ar.spec = api.RunSpec{Test: "tcp", DurationSec: 20, PassPct: 50}
	plans[0].clientRes = &api.LegResult{AvgGbps: 150, Retransmits: 3}
	if _, v := o.score(ar); v != "warn" {
		t.Errorf("tcp retransmit verdict %s note %q", v, plans[0].leg.Note)
	}
}
