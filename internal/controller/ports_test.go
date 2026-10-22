package controller

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Realishbird/SR-Net-Speed-Test/internal/api"
)

// twoPortLab is the lab since the Sparks' P2 cables went in: the 770AP with
// a NIC on each fabric, and Sparks with P1 on one fabric and P2 on the
// other, each P2 still unlabelled, plus the second PCIe half of P1 (no
// address).
func twoPortLab() []hostRec {
	mk := func(id int64, name string, tags []string, ports ...api.HostPort) hostRec {
		h := hostRec{}
		h.ID, h.Name, h.Hostname, h.Tags, h.Ports = id, name, strings.ToLower(strings.ReplaceAll(name, " ", "-")), tags, ports
		return h
	}
	port := func(id int64, netdev, ip, label, fabric string) api.HostPort {
		p := api.HostPort{ID: id, RdmaDev: "roce" + netdev, PortNum: 1, Netdev: netdev, State: "ACTIVE", SpeedMbps: 100000, Label: label, Fabric: fabric, Addrs: []string{}}
		if ip != "" {
			p.Addrs = []string{ip}
		}
		return p
	}
	return []hostRec{
		mk(1, "Dell 770AP", nil, port(10, "ens1f0np0", "172.27.100.1/24", "NIC-1", "100"), port(11, "ens2f0np0", "172.27.101.1/24", "NIC-2", "101")),
		mk(2, "Dell Spark 1", []string{"spark"},
			port(20, "enp1s0f0np0", "172.27.100.10/24", "P1-B1-A", "100"),
			port(21, "enp1s0f1np1", "172.27.101.20/24", "", ""),
			port(22, "enP2p1s0f0np0", "", "", "")),
		mk(3, "Dell Spark 2", []string{"spark"},
			port(30, "enp1s0f0np0", "172.27.101.10/24", "P1-B2-A", "101"),
			port(31, "enp1s0f1np1", "172.27.100.20/24", "", "")),
	}
}

func legNames(plans []*legPlan) []string {
	var out []string
	for _, p := range plans {
		out = append(out, p.client.Host+" "+p.client.IP+" > "+p.server.Host+" "+p.server.IP+" ["+p.leg.Fabric+"]")
	}
	return out
}

func TestPlanEveryPortOfAHost(t *testing.T) {
	plans, err := plan(twoPortLab(), api.RunSpec{Clients: []string{"tag:spark"}, Servers: []string{"Dell 770AP"}})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(legNames(plans), "\n")
	want := strings.Join([]string{
		"Dell Spark 1 172.27.100.10 > Dell 770AP 172.27.100.1 [100]",
		"Dell Spark 1 172.27.101.20 > Dell 770AP 172.27.101.1 [101]",
		"Dell Spark 2 172.27.101.10 > Dell 770AP 172.27.101.1 [101]",
		"Dell Spark 2 172.27.100.20 > Dell 770AP 172.27.100.1 [100]",
	}, "\n")
	if got != want {
		t.Errorf("legs:\n%s\nwant:\n%s", got, want)
	}
	// Spark to Spark: one leg per shared fabric.
	plans, err = plan(twoPortLab(), api.RunSpec{Clients: []string{"Dell Spark 1"}, Servers: []string{"Dell Spark 2"}})
	if err != nil || len(plans) != 2 {
		t.Fatalf("spark pair: %v %v", legNames(plans), err)
	}
}

func TestPlanOneLinkPerHostSpreadsPorts(t *testing.T) {
	plans, err := plan(twoPortLab(), api.RunSpec{Clients: []string{"Dell 770AP"}, Servers: []string{"tag:spark"}, Ports: api.PortsOne})
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 2 || plans[0].client.IP == plans[1].client.IP {
		t.Errorf("one link per host should use both 770AP NICs: %v", legNames(plans))
	}
}

func TestPlanFabricFilterAndPortSelectors(t *testing.T) {
	plans, err := plan(twoPortLab(), api.RunSpec{Clients: []string{"tag:spark"}, Servers: []string{"Dell 770AP"}, Fabric: "101"})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range plans {
		if p.leg.Fabric != "101" {
			t.Errorf("fabric 101 only, got %v", legNames(plans))
		}
	}
	if len(plans) != 2 {
		t.Errorf("an unlabelled P2 on fabric 101's subnet counts as fabric 101: %v", legNames(plans))
	}
	plans, err = plan(twoPortLab(), api.RunSpec{Clients: []string{"Dell Spark 1/enp1s0f1np1"}, Servers: []string{"Dell 770AP"}})
	if err != nil || len(plans) != 1 || plans[0].client.IP != "172.27.101.20" {
		t.Errorf("host/netdev selector: %v %v", legNames(plans), err)
	}
	if _, err := plan(twoPortLab(), api.RunSpec{Clients: []string{"Dell Spark 1/eth9"}, Servers: []string{"Dell 770AP"}}); err == nil || !strings.Contains(err.Error(), "no port") {
		t.Errorf("unknown port: %v", err)
	}
	if _, err := plan(twoPortLab(), api.RunSpec{Clients: []string{"P1-B1-A"}, Servers: []string{"Dell 770AP"}, Fabric: "101"}); err == nil {
		t.Error("a port on fabric 100 must not pass a fabric 101 filter")
	}
	if err := applyDefaults(&api.RunSpec{Ports: "both"}); err == nil {
		t.Error("unknown ports choice accepted")
	}
}

func TestPlanEveryPortRefusesDownLink(t *testing.T) {
	hosts := twoPortLab()
	hosts[1].Ports[1].State = "DOWN"
	if _, err := plan(hosts, api.RunSpec{Clients: []string{"Dell Spark 1"}, Servers: []string{"Dell 770AP"}}); err == nil || !strings.Contains(err.Error(), "not ACTIVE") {
		t.Errorf("a configured port that is down must fail the plan, got %v", err)
	}
}

func TestMarkTwins(t *testing.T) {
	ports := []api.HostPort{
		{Netdev: "enp1s0f0np0", Addrs: []string{"172.27.100.13/24"}, LLDPSwitch: "z9864f", LLDPPort: "Ethernet3"},
		{Netdev: "enP2p1s0f0np0", LLDPSwitch: "z9864f", LLDPPort: "Ethernet3", PCIe: &api.PCIeLink{Address: "0002:01:00.0"}},
		{Netdev: "enP2p1s0f1np1", LLDPSwitch: "z9864f", LLDPPort: "Ethernet11", PCIe: &api.PCIeLink{Address: "0002:01:00.1"}},
		{Netdev: "enp1s0f1np1", LLDPSwitch: "z9864f", LLDPPort: "Ethernet11", PCIe: &api.PCIeLink{Address: "0000:01:00.1"}},
	}
	markTwins(ports)
	if ports[0].TwinOf != "" || ports[1].TwinOf != "enp1s0f0np0" {
		t.Errorf("P1: %q %q", ports[0].TwinOf, ports[1].TwinOf)
	}
	// Neither half has an address yet: the lower PCI address is the cable.
	if ports[3].TwinOf != "" || ports[2].TwinOf != "enp1s0f1np1" {
		t.Errorf("P2: %q %q", ports[2].TwinOf, ports[3].TwinOf)
	}
}

func TestLegKeyKeepsPortsApart(t *testing.T) {
	a := api.Leg{ClientHost: "Dell Spark 1", ClientIP: "172.27.100.10", ServerHost: "Dell 770AP", ServerIP: "172.27.100.1"}
	b := api.Leg{ClientHost: "Dell Spark 1", ClientIP: "172.27.101.20", ServerHost: "Dell 770AP", ServerIP: "172.27.101.1"}
	if legKey(a) == legKey(b) {
		t.Error("the P1 and P2 legs of one host pair share a baseline key")
	}
}

// pcieLab is twoPortLab with PCIe links as the agents report them: the
// 770AP's NICs are 400G on their own Gen5 x16 links, and each Spark's P1 and
// P2 sit on the first of its ConnectX-7's two Gen5 x4 links (0000:01:00),
// with the second (0002:01:00) holding only the unaddressed twin halves.
func pcieLab() []hostRec {
	hosts := twoPortLab()
	link := func(addr string, width int) *api.PCIeLink {
		return &api.PCIeLink{Address: addr, Gen: 5, Width: width, MaxGen: 5, MaxWidth: width, UsableGbps: api.PCIeUsableGbps(5, width)}
	}
	hosts[0].Ports[0].SpeedMbps, hosts[0].Ports[0].PCIe = 400000, link("0000:41:00.0", 16)
	hosts[0].Ports[1].SpeedMbps, hosts[0].Ports[1].PCIe = 400000, link("0000:c1:00.0", 16)
	for _, h := range hosts[1:] {
		for i := range h.Ports {
			p := &h.Ports[i]
			fn := p.Netdev[len(p.Netdev)-1:]
			if strings.HasPrefix(p.Netdev, "enP2") {
				p.PCIe = link("0002:01:00."+fn, 4)
			} else {
				p.PCIe = link("0000:01:00."+fn, 4)
			}
		}
	}
	return hosts
}

func TestFairSharePCIeLink(t *testing.T) {
	half := api.PCIeUsableGbps(5, 4) / 2
	plans, err := plan(pcieLab(), api.RunSpec{Test: "write", Clients: []string{"Dell 770AP"}, Servers: []string{"tag:spark"}})
	if err != nil || len(plans) != 4 {
		t.Fatalf("%v %v", legNames(plans), err)
	}
	for _, p := range plans {
		if p.leg.ExpectedGbps != half || !strings.Contains(p.leg.PCIeShare, p.server.Host+" PCIe 0000:01:00 Gen5 x4") ||
			!strings.HasSuffix(p.leg.PCIeShare, "shared by 2 legs") || p.leg.SharedPort != "" {
			t.Errorf("both Spark ports on one x4 link should split it: %s expected %.1f %q", legNames([]*legPlan{p}), p.leg.ExpectedGbps, p.leg.PCIeShare)
		}
	}
	if n := shareNote(plans[0].leg); !strings.HasPrefix(n, "limited by Dell Spark") {
		t.Errorf("preview note %q", n)
	}

	// Bidirectional legs use both directions of the link, so the cap holds
	// one way and the expected figure doubles.
	plans, _ = plan(pcieLab(), api.RunSpec{Test: "write", Clients: []string{"Dell 770AP"}, Servers: []string{"tag:spark"}, Bidir: true})
	if plans[0].leg.ExpectedGbps != 2*half {
		t.Errorf("bidirectional: %.1f, want %.1f", plans[0].leg.ExpectedGbps, 2*half)
	}

	// A read moves data from the server to the client: still both on the
	// Spark's link, in the other direction.
	plans, _ = plan(pcieLab(), api.RunSpec{Test: "read", Clients: []string{"Dell 770AP"}, Servers: []string{"tag:spark"}})
	if plans[0].leg.ExpectedGbps != half {
		t.Errorf("read: %.1f, want %.1f", plans[0].leg.ExpectedGbps, half)
	}

	// One port sending and the other receiving use opposite directions.
	plans, err = plan(pcieLab(), api.RunSpec{Test: "write", Legs: []api.LegSpec{
		{Client: "Dell Spark 1/enp1s0f0np0", Server: "Dell 770AP"},
		{Client: "Dell 770AP", Server: "Dell Spark 1/enp1s0f1np1"},
	}})
	if err != nil || len(plans) != 2 || plans[0].leg.ExpectedGbps != 100 || plans[1].leg.ExpectedGbps != 100 || plans[0].leg.PCIeShare != "" {
		t.Errorf("opposite directions should not share: %v %v", legNames(plans), err)
	}

	// P2 moved to the second PCIe half: each port has its own link.
	hosts := pcieLab()
	sp := hosts[1].Ports
	sp[1].Addrs = nil
	sp = append(sp, api.HostPort{ID: 23, RdmaDev: "roceP2p1s0f1", PortNum: 1, Netdev: "enP2p1s0f1np1", State: "ACTIVE", SpeedMbps: 100000,
		Addrs: []string{"172.27.101.20/24"}, PCIe: &api.PCIeLink{Address: "0002:01:00.1", Gen: 5, Width: 4, UsableGbps: api.PCIeUsableGbps(5, 4)}})
	hosts[1].Ports = sp
	plans, err = plan(hosts, api.RunSpec{Test: "write", Clients: []string{"Dell 770AP"}, Servers: []string{"Dell Spark 1"}})
	if err != nil || len(plans) != 2 {
		t.Fatalf("%v %v", legNames(plans), err)
	}
	for _, p := range plans {
		if p.leg.ExpectedGbps != 100 || p.leg.PCIeShare != "" {
			t.Errorf("P2 on its own link: %s expected %.1f %q", legNames([]*legPlan{p}), p.leg.ExpectedGbps, p.leg.PCIeShare)
		}
	}

	// A port faster than its link is capped even alone.
	hosts = pcieLab()
	hosts[1].Ports[0].SpeedMbps = 200000
	plans, _ = plan(hosts, api.RunSpec{Test: "write", Legs: []api.LegSpec{{Client: "Dell 770AP", Server: "Dell Spark 1/enp1s0f0np0"}}})
	if len(plans) != 1 || plans[0].leg.ExpectedGbps != 2*half || strings.Contains(plans[0].leg.PCIeShare, "shared") {
		t.Errorf("200G port on Gen5 x4: %.1f %q", plans[0].leg.ExpectedGbps, plans[0].leg.PCIeShare)
	}
}

func TestLiveAvgSmoothsSwings(t *testing.T) {
	m := newMetrics("")
	now := time.Now().Add(-10 * time.Second)
	// A leg swinging between 20 and 80 Gbps each second, 50 on average.
	var tx float64
	for sec := 0; sec <= 10; sec++ {
		g := 20.0
		if sec%2 == 1 {
			g = 80
		}
		if sec > 0 {
			tx += g * 1e9 / 8
		}
		m.track(7, api.Sample{Time: now.Add(time.Duration(sec) * time.Second), Values: map[string]float64{
			"rdma_tx_bytes": tx, "rdma_rx_bytes": 0, "rdma_tx_gbps": g, "rdma_rx_gbps": 0}})
	}
	if got, _ := m.live(7); got != 20 {
		t.Fatalf("one-second rate %v", got)
	}
	got, _ := m.liveAvg(7, liveWindow, false)
	if got < 44 || got > 56 {
		t.Errorf("five-second average %.1f, want about 50", got)
	}
	// Idle: one sample in the window falls back to the latest rate.
	m2 := newMetrics("")
	m2.track(8, api.Sample{Time: time.Now(), Values: map[string]float64{"rdma_tx_bytes": 1, "rdma_rx_bytes": 1, "rdma_tx_gbps": 3, "rdma_rx_gbps": 4}})
	if tx, rx := m2.liveAvg(8, liveWindow, false); tx != 3 || rx != 4 {
		t.Errorf("fallback %v %v", tx, rx)
	}
}

// sparkPorts is one Spark as its agent reports it: two cables, each seen as
// two PCIe functions, with both addresses on the first PCIe link.
func sparkPorts() []api.HostPort {
	p := func(id int64, netdev, pci, swPort, addr string) api.HostPort {
		hp := api.HostPort{ID: id, Netdev: netdev, State: "ACTIVE", SpeedMbps: 100000, MTU: 9000, LLDPSwitch: "z9864f", LLDPPort: swPort,
			PCIe: &api.PCIeLink{Address: pci, Gen: 5, Width: 4, UsableGbps: api.PCIeUsableGbps(5, 4)}}
		if addr != "" {
			hp.Addrs = []string{addr, "fe80::1/64"}
		}
		return hp
	}
	return []api.HostPort{
		p(20, "enp1s0f0np0", "0000:01:00.0", "Ethernet0", "172.27.100.10/24"),
		p(21, "enp1s0f1np1", "0000:01:00.1", "Ethernet8", "172.27.101.20/24"),
		p(22, "enP2p1s0f0np0", "0002:01:00.0", "Ethernet0", ""),
		p(23, "enP2p1s0f1np1", "0002:01:00.1", "Ethernet8", ""),
	}
}

func TestMarkPCIeShare(t *testing.T) {
	ports := sparkPorts()
	markTwins(ports)
	markPCIeShare(ports)
	if !strings.Contains(ports[0].PCIeShared, "0000:01:00") || !strings.Contains(ports[0].PCIeShared, "enp1s0f1np1") || !strings.Contains(ports[1].PCIeShared, "enp1s0f0np0") {
		t.Errorf("shared: %q / %q", ports[0].PCIeShared, ports[1].PCIeShared)
	}
	if ports[0].PCIeMoveTo != "" || ports[1].PCIeMoveTo != "enP2p1s0f1np1" {
		t.Errorf("only the second cable moves, to its own twin: %q %q", ports[0].PCIeMoveTo, ports[1].PCIeMoveTo)
	}
	// After the move each link carries one cable: nothing to flag.
	ports = sparkPorts()
	ports[3].Addrs, ports[1].Addrs = ports[1].Addrs, nil
	markTwins(ports)
	markPCIeShare(ports)
	for _, p := range ports {
		if p.PCIeShared != "" || p.PCIeMoveTo != "" {
			t.Errorf("%s still flagged: %q %q", p.Netdev, p.PCIeShared, p.PCIeMoveTo)
		}
	}
	// A dual-port 100G card on a Gen5 x16 link has room for both.
	ports = sparkPorts()[:2]
	for i := range ports {
		ports[i].PCIe.Width, ports[i].PCIe.UsableGbps = 16, api.PCIeUsableGbps(5, 16)
	}
	markPCIeShare(ports)
	if ports[0].PCIeShared != "" {
		t.Errorf("x16 flagged: %q", ports[0].PCIeShared)
	}
}

func TestNetplanMoveToTwin(t *testing.T) {
	h := hostRec{}
	h.ID, h.Name, h.Ports = 2, "Dell Spark 1", sparkPorts()
	peer := hostRec{}
	peer.ID, peer.Name = 1, "Dell 770AP"
	peer.Ports = []api.HostPort{{ID: 11, Netdev: "ens2f0np0", Addrs: []string{"172.27.101.1/24"}}}
	hosts := []hostRec{peer, h}
	req, from, to, err := moveRequest(h, netplanMove{From: "enp1s0f1np1", To: "enP2p1s0f1np1"})
	if err != nil || from.ID != 21 || to.ID != 23 {
		t.Fatalf("%v %+v", err, req)
	}
	// The address moves within one change, which the fleet-wide duplicate
	// check must allow, and the new port must reach a fabric peer.
	ch, err := prepareChange(h, hosts, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(ch.Ports) != 2 || len(ch.Ports[0].Addresses) != 0 || ch.Ports[0].MTU != 9000 ||
		len(ch.Ports[1].Addresses) != 1 || ch.Ports[1].Addresses[0] != "172.27.101.20/24" || ch.Ports[1].MTU != 9000 {
		t.Errorf("change %+v", ch.Ports)
	}
	if p := ch.Ports[1].Peers; len(p) != 1 || p[0] != "172.27.101.1" {
		t.Errorf("peers %v", p)
	}
	for name, m := range map[string]netplanMove{
		"same port":        {From: "enp1s0f1np1", To: "enp1s0f1np1"},
		"nothing to move":  {From: "enP2p1s0f0np0", To: "enP2p1s0f1np1"},
		"target addressed": {From: "enp1s0f1np1", To: "enp1s0f0np0"},
		"unknown":          {From: "enp1s0f1np1", To: "eth9"},
	} {
		if _, _, _, err := moveRequest(h, m); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Outside a move, an address another port of the host holds is refused.
	if _, err := prepareChange(h, hosts, api.NetplanRequest{Ports: []api.NetplanPort{{Netdev: "enP2p1s0f1np1", Addresses: []string{"172.27.101.20/24"}}}}); err == nil {
		t.Error("a duplicate of the host's own other port was accepted")
	}
}

func TestLabelSelectors(t *testing.T) {
	hosts := twoPortLab()
	dell := hostRec{}
	dell.ID, dell.Name, dell.Hostname = 4, "dellr7615", "dellr7615"
	dell.Ports = []api.HostPort{{ID: 40, Netdev: "eno16595np0", State: "ACTIVE", SpeedMbps: 400000, Label: "NIC-1", Addrs: []string{"172.27.100.2/24"}}}
	hosts = append(hosts, dell)
	if _, err := resolveSelector(hosts, "nic-1"); err == nil || !strings.Contains(err.Error(), "Dell 770AP and dellr7615") || !strings.Contains(err.Error(), `"Dell 770AP/nic-1"`) {
		t.Errorf("a label on two hosts: %v", err)
	}
	ts, err := resolveSelector(hosts, "dellr7615/NIC-1")
	if err != nil || len(ts) != 1 || ts[0].port.ID != 40 {
		t.Errorf("host/label: %v %v", ts, err)
	}
	ts, err = resolveSelector(hosts, "P1-*")
	if err != nil || len(ts) != 2 || ts[0].port.Label != "P1-B1-A" || ts[1].port.Label != "P1-B2-A" {
		t.Errorf("pattern: %v %v", ts, err)
	}
	if _, err := resolveSelector(hosts, "Q*"); err == nil {
		t.Error("a pattern nothing fits")
	}
	// The pattern drives a plan: one leg per matching port.
	plans, err := plan(hosts, api.RunSpec{Clients: []string{"Dell 770AP"}, Servers: []string{"P1-*"}})
	if err != nil || len(plans) != 2 {
		t.Errorf("plan from a pattern: %v %v", legNames(plans), err)
	}
}

func TestDropHint(t *testing.T) {
	l := &api.Leg{ErrorDeltas: map[string]int64{"out_of_sequence": 5802, "np_cnp_sent": 5802}, Switch: &api.LegSwitch{Gbps: 52}}
	if h := dropHint(l); !strings.Contains(h, "receiving host") {
		t.Errorf("hint %q", h)
	}
	l.Switch.Deltas = map[string]float64{"Ethernet0 queue 0 dropped_pkts": 40}
	if h := dropHint(l); h != "" {
		t.Errorf("the switch dropped too, so no hint: %q", h)
	}
	l.Switch = &api.LegSwitch{} // no switch reading: say nothing
	if h := dropHint(l); h != "" {
		t.Errorf("no switch figures: %q", h)
	}
}

// TestPortSpreadRun18 replays run 18's tool rates: the 770AP's NIC-2 legs
// trailed its NIC-1 legs by 3.4% while every Spark port and both of the new
// Dell's NICs ran level. Only the 770AP is named.
func TestPortSpreadRun18(t *testing.T) {
	var plans []*legPlan
	ep := func(host string, id int64, label string) endpoint {
		return endpoint{Host: host, PortID: id, Label: label}
	}
	legs := []struct {
		client endpoint
		gbps   []float64
	}{
		{ep("Dell 770AP", 1, "NIC-1"), []float64{192.72, 192.75, 192.81, 192.75}},
		{ep("Dell 770AP", 2, "NIC-2"), []float64{186.16, 186.2, 186.21, 186.31}},
		{ep("dellr7615", 3, "NIC-1"), []float64{192.98, 192.95, 193, 193.06}},
		{ep("dellr7615", 4, "NIC-2"), []float64{194.04, 194.05, 194.03, 194.03}},
	}
	spark := int64(100)
	for i, c := range legs {
		for j, g := range c.gbps {
			spark++
			plans = append(plans, &legPlan{client: c.client, server: ep(fmt.Sprintf("Spark %d", j), spark, fmt.Sprintf("P%d", i/2+1)),
				leg: api.Leg{ToolGbps: g, ExpectedGbps: 200}})
		}
	}
	got := portSpread(plans)
	if want := []string{"Dell 770AP NIC-2: its 4 legs averaged 186.2 Gbps, 3.4% below NIC-1's 192.8"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	// One stray slow leg among fast ones is not a pattern.
	plans[4].leg.ToolGbps = 193
	if got := portSpread(plans); len(got) != 0 {
		t.Fatalf("mixed rates flagged: %q", got)
	}
}
