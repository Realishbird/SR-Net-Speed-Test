package controller

import (
	"strings"
	"testing"

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
