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
	ar := &activeRun{id: 1, spec: api.RunSpec{Test: "write", DurationSec: 60, PassPct: 90}, plans: plans, startAt: start}
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
	// Only a counter error, not a bandwidth miss: warn.
	plans[1].clientRes.AvgGbps = 195
	fill(30, 195)
	_, verdict = o.score(ar)
	if verdict != "warn" || !strings.Contains(plans[1].leg.Note, "out_of_sequence +2") {
		t.Errorf("verdict = %s, leg = %+v", verdict, plans[1].leg)
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
