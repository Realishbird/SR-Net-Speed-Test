package controller

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/Realishbird/SR-Net-Speed-Test/internal/api"
)

// labHosts is a slice of dylan's lab: the 770AP, two Sparks with their
// twin PCIe functions, a Spark whose P2 is on a typo subnet, the new Dell
// before its addresses are set, and a few broken cables.
func labHosts() []hostRec {
	helper := &api.NetplanState{Helper: "1"}
	port := func(id int64, netdev, pci, addr, swPort string, mtu int) api.HostPort {
		p := api.HostPort{ID: id, Netdev: netdev, State: "ACTIVE", MTU: mtu, LLDPSwitch: "z9864f", LLDPPort: swPort, Addrs: []string{}}
		if pci != "" {
			p.PCIe = &api.PCIeLink{Address: pci}
		}
		if addr != "" {
			p.Addrs = []string{addr}
		}
		return p
	}
	host := func(id int64, name string, hp *api.NetplanState, ports ...api.HostPort) hostRec {
		h := hostRec{}
		h.ID, h.Name, h.Approved, h.Netplan, h.Ports = id, name, true, hp, ports
		return h
	}
	mgmt := port(19, "enP7s7", "", "176.16.253.100/16", "Ethernet512", 1500)
	mgmt.Netplan = &api.PortNetplan{Mgmt: true}
	conflict := port(52, "ens3f0np0", "0000:81:00.0", "", "Ethernet20", 9000)
	conflict.Fabric = "100" // label says 100, switch port is in 101
	noVLAN := port(53, "ens4f0np0", "0000:82:00.0", "", "Ethernet30", 9000)
	noVLAN.Fabric = "101"
	asusP1 := port(31, "enp1s0f0np0", "0000:01:00.0", "172.27.101.13/24", "Ethernet7", 9000)
	asusP2 := port(32, "enp1s0f1np1", "0000:01:00.1", "172.27.100.23/24", "Ethernet15", 9000)
	asusP1.Fabric, asusP2.Fabric = "101", "100"
	return []hostRec{
		host(1, "dell770ap", helper,
			port(11, "ens1f0np0", "0000:41:00.0", "172.27.100.1/24", "Ethernet120", 9000),
			port(12, "ens2f0np0", "0000:c1:00.0", "172.27.101.1/24", "Ethernet124", 9000)),
		host(2, "Acer Spark", helper,
			port(21, "enp1s0f0np0", "0000:01:00.0", "172.27.100.13/24", "Ethernet3", 9000),
			port(22, "enP2p1s0f0np0", "0002:01:00.0", "", "Ethernet3", 1500),
			port(23, "enp1s0f1np1", "0000:01:00.1", "", "Ethernet11", 1500),
			port(24, "enP2p1s0f1np1", "0002:01:00.1", "", "Ethernet11", 1500),
			mgmt),
		host(3, "ASUS Spark", helper, asusP1, asusP2),
		host(4, "Dell Spark 1", helper,
			port(41, "enp1s0f0np0", "0000:01:00.0", "172.27.100.10/24", "Ethernet0", 9000),
			port(42, "enp1s0f1np1", "0000:01:00.1", "172.17.101.20/24", "Ethernet8", 9000)),
		host(5, "dellr7615", nil,
			port(51, "eno16595np0", "0000:a1:00.0", "", "Ethernet128", 1500),
			conflict, noVLAN),
	}
}

func labVLANs() map[string]string {
	v := map[string]string{}
	for _, p := range []string{"Ethernet0", "Ethernet3", "Ethernet12", "Ethernet15", "Ethernet120", "Ethernet128"} {
		v["z9864f|"+strings.ToLower(p)] = "100"
	}
	for _, p := range []string{"Ethernet7", "Ethernet8", "Ethernet11", "Ethernet20", "Ethernet124", "Ethernet132"} {
		v["z9864f|"+strings.ToLower(p)] = "101"
	}
	return v
}

// labIfaces is the switch's port table for the ports labHosts uses, with
// made-up aliases in the switch's Eth1/x/y style.
func labIfaces() map[string]map[string]switchIface {
	tab := map[string]switchIface{}
	for i, n := range []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 20, 30, 120, 124, 128, 132} {
		name := "Ethernet" + strconv.Itoa(n)
		tab[strings.ToLower(name)] = switchIface{Name: name, Alias: fmt.Sprintf("Eth1/%d/1", i+1)}
	}
	return map[string]map[string]switchIface{"z9864f": tab}
}

func labSwitches(naming string) planSwitches {
	return planSwitches{
		vlans:  labVLANs(),
		qos:    map[string]*switchPortCfg{"z9864f|ethernet11": {MTU: 1500}},
		known:  labIfaces(),
		naming: map[string]string{"z9864f": naming},
	}
}

func labPlan(t *testing.T) api.NetworkPlan {
	t.Helper()
	profiles := []api.FabricProfile{{Fabric: "101", VLAN: "101", SwitchMTU: 9100, HostMTU: 9000}}
	return buildNetworkPlan(labHosts(), []SwitchConfig{{Name: "z9864f", Address: "176.16.9.22"}}, labSwitches("NATIVE"), profiles, func(int64) bool { return true })
}

func planRow(t *testing.T, plan api.NetworkPlan, portID int64) api.PlanPort {
	t.Helper()
	for _, p := range plan.Ports {
		if p.PortID == portID {
			return p
		}
	}
	t.Fatalf("port %d not in the plan", portID)
	return api.PlanPort{}
}

func TestNetworkPlanLab(t *testing.T) {
	plan := labPlan(t)
	fabrics := map[string]api.PlanFabric{}
	for _, f := range plan.Fabrics {
		fabrics[f.Fabric] = f
	}
	if f := fabrics["100"]; f.Exists || f.Subnet != "172.27.100.0/24" || f.SubnetFrom != "addresses" || f.VLAN != "100" {
		t.Errorf("fabric 100: %+v (want new, subnet learned from the ports)", f)
	}
	if f := fabrics["101"]; !f.Exists || f.Subnet != "172.27.101.0/24" {
		t.Errorf("fabric 101: %+v", f)
	}

	// The Acer's P2: host number 13, PCI function 1, so .23 as in the sheet.
	acer := planRow(t, plan, 23)
	if acer.Action != "assign" || acer.Address != "172.27.101.23/24" || acer.MTU != 9000 || !acer.Apply || acer.Fabric != "101" || !acer.SetFabric {
		t.Errorf("Acer P2: %+v", acer)
	}
	if !strings.Contains(acer.Rule, "host number 13") {
		t.Errorf("Acer P2 rule %q", acer.Rule)
	}
	for _, p := range plan.Ports {
		if p.PortID == 22 || p.PortID == 24 || p.PortID == 19 {
			t.Errorf("twin or management port in the plan: %+v", p)
		}
	}
	// Addresses that fit are kept and not ticked; a port with no fabric of
	// its own is ticked only to store the fabric.
	if p := planRow(t, plan, 32); p.Action != "ok" || p.Apply || p.SetFabric || p.Address != "172.27.100.23/24" {
		t.Errorf("ASUS P2: %+v", p)
	}
	if p := planRow(t, plan, 21); p.Action != "ok" || !p.Apply || !p.SetFabric || p.Fabric != "100" {
		t.Errorf("Acer P1 (no fabric set yet): %+v", p)
	}
	// A typo subnet is proposed for renumbering but never ticked by default.
	if p := planRow(t, plan, 42); p.Action != "renumber" || p.Apply || p.Address != "172.27.101.20/24" || !strings.Contains(p.Note, "172.17.101.20/24") {
		t.Errorf("Dell Spark 1 P2: %+v", p)
	}
	// No addresses yet: the next free one from .10; no helper, so not ticked.
	if p := planRow(t, plan, 51); p.Action != "assign" || p.Address != "172.27.100.11/24" || p.Apply || !strings.Contains(p.Note, "install command") {
		t.Errorf("dellr7615 NIC1: %+v", p)
	}
	if p := planRow(t, plan, 52); p.Action != "blocked" || !strings.Contains(p.Note, "VLAN 101") {
		t.Errorf("label/VLAN conflict: %+v", p)
	}
	if p := planRow(t, plan, 53); p.Action != "blocked" || !strings.Contains(p.Note, "no VLAN") {
		t.Errorf("no VLAN: %+v", p)
	}

	if len(plan.Commands) != 1 {
		t.Fatalf("commands %+v", plan.Commands)
	}
	lines := plan.Commands[0].Lines
	for _, want := range []string{
		"configure terminal\n",
		"interface Ethernet11\n mtu 9100\n exit\n",
		"interface Ethernet20\n switchport access Vlan 100\n exit\n",
		"interface Ethernet30\n switchport access Vlan 101\n exit\n",
		"end\nwrite memory\n",
	} {
		if !strings.Contains(lines, want) {
			t.Errorf("switch lines miss %q:\n%s", want, lines)
		}
	}
	if strings.Index(lines, "Ethernet11") > strings.Index(lines, "Ethernet20") {
		t.Errorf("switch ports out of order:\n%s", lines)
	}
	if strings.Contains(lines, "interface Vlan") {
		t.Errorf("VLANs 100 and 101 exist on the switch; nothing to create:\n%s", lines)
	}
}

func TestPlanChanges(t *testing.T) {
	plan, hosts := labPlan(t), labHosts()
	changes, err := planChanges(plan, hosts, api.PlanApply{Ports: []api.PlanApplyPort{{PortID: 23}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].host.ID != 2 || len(changes[0].change.Ports) != 1 {
		t.Fatalf("changes %+v", changes)
	}
	np := changes[0].change.Ports[0]
	if np.Netdev != "enp1s0f1np1" || len(np.Addresses) != 1 || np.Addresses[0] != "172.27.101.23/24" || np.MTU != 9000 {
		t.Errorf("change %+v", np)
	}
	if len(np.Peers) == 0 || np.Peers[0] != "172.27.101.1" {
		t.Errorf("peers %v: the 770AP (.1) should be asked first", np.Peers)
	}
	if len(changes[0].setFabric) != 1 || changes[0].fabric[23] != "101" {
		t.Errorf("fabric label not set: %+v", changes[0])
	}
	// A port whose address fits only gets its fabric stored.
	changes, err = planChanges(plan, hosts, api.PlanApply{Ports: []api.PlanApplyPort{{PortID: 21}}})
	if err != nil || len(changes) != 1 || len(changes[0].change.Ports) != 0 || changes[0].fabric[21] != "100" {
		t.Errorf("fabric-only apply: %v %+v", err, changes)
	}
	// An edited address without a prefix length takes the fabric's.
	changes, err = planChanges(plan, hosts, api.PlanApply{Ports: []api.PlanApplyPort{{PortID: 23, Address: "172.27.101.33"}}})
	if err != nil || changes[0].change.Ports[0].Addresses[0] != "172.27.101.33/24" {
		t.Errorf("edited address: %v %+v", err, changes)
	}
	for name, ap := range map[string]api.PlanApplyPort{
		"outside the fabric": {PortID: 23, Address: "172.27.100.33/24"},
		"taken":              {PortID: 23, Address: "172.27.101.13/24"},
		"blocked":            {PortID: 52},
		"unknown port":       {PortID: 999},
	} {
		if _, err := planChanges(plan, hosts, api.PlanApply{Ports: []api.PlanApplyPort{ap}}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := planChanges(plan, hosts, api.PlanApply{Ports: []api.PlanApplyPort{{PortID: 23, Address: "172.27.101.40"}, {PortID: 42, Address: "172.27.101.40"}}}); err == nil {
		t.Error("one address given to two ports")
	}
}

func TestPrepareChange(t *testing.T) {
	hosts := labHosts()
	acer := hosts[1]
	req := func(dev string, addrs ...string) api.NetplanRequest {
		return api.NetplanRequest{Ports: []api.NetplanPort{{Netdev: dev, Addresses: addrs, MTU: 9000, Peers: []string{"8.8.8.8"}}}}
	}
	ch, err := prepareChange(acer, hosts, req("enp1s0f1np1", " 172.27.101.23/24 "))
	if err != nil {
		t.Fatal(err)
	}
	if ch.TimeoutSec != 90 || ch.Ports[0].Addresses[0] != "172.27.101.23/24" {
		t.Errorf("%+v", ch)
	}
	if p := ch.Ports[0].Peers; len(p) != 2 || p[0] != "172.27.101.1" || p[1] != "172.27.101.13" {
		t.Errorf("peers %v: the controller picks them, from other hosts in the subnet", p)
	}
	for name, r := range map[string]api.NetplanRequest{
		"management port": req("enP7s7", "176.16.253.101/16"),
		"not on the host": req("eth9", "172.27.101.23/24"),
		"duplicate":       req("enp1s0f1np1", "172.27.101.13/24"),
		"bad address":     req("enp1s0f1np1", "172.27.101.300/24"),
		"timeout":         {Ports: req("enp1s0f1np1", "172.27.101.23/24").Ports, TimeoutSec: 5},
	} {
		if _, err := prepareChange(acer, hosts, r); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestNetplanPreviewMergesManagedFile(t *testing.T) {
	current := "network:\n  version: 2\n  ethernets:\n    enp1s0f0np0:\n      addresses: [172.27.100.13/24]\n      mtu: 9000\n      routes:\n      - to: 10.9.0.0/16\n        via: 172.27.100.254\n"
	ports := []api.HostPort{{Netdev: "enp1s0f1np1", Netplan: &api.PortNetplan{Files: []string{"50-cloud-init.yaml"}}}}
	pv := renderNetplanPreview(current, api.NetplanChange{Ports: []api.NetplanPort{{Netdev: "enp1s0f1np1", Addresses: []string{"172.27.101.23/24"}, MTU: 9000}}}, ports)
	for _, want := range []string{"enp1s0f0np0:", "172.27.100.13/24", "via: 172.27.100.254", "enp1s0f1np1:", "- 172.27.101.23/24", "mtu: 9000", "dhcp4: false"} {
		if !strings.Contains(pv.YAML, want) {
			t.Errorf("preview misses %q:\n%s", want, pv.YAML)
		}
	}
	if !strings.Contains(pv.Commands, "netplan try") || !strings.Contains(pv.Commands, "chmod 600") {
		t.Errorf("commands:\n%s", pv.Commands)
	}
	if !strings.Contains(pv.Note, "50-cloud-init.yaml") {
		t.Errorf("note %q should say where else the port is defined", pv.Note)
	}
}

func TestNetplanClaimBlocksRuns(t *testing.T) {
	o := newOrchestrator(nil, newHub(), nil)
	if err := o.claimNetplan(5); err != nil {
		t.Fatal(err)
	}
	if err := o.claimNetplan(5); err == nil {
		t.Error("two changes at once on one host")
	}
	o.releaseNetplan(5)
	o.active[7] = &activeRun{id: 7, plans: []*legPlan{{client: endpoint{AgentID: 5}, server: endpoint{AgentID: 1}}}}
	if err := o.claimNetplan(5); err == nil || !strings.Contains(err.Error(), "run 7") {
		t.Errorf("claimed a host a run is using: %v", err)
	}
	if err := o.claimNetplan(2); err != nil {
		t.Errorf("host 2 is free: %v", err)
	}
}

func TestNetplanJobIDsIncrease(t *testing.T) {
	n := newNetplanJobs()
	a, b := &api.NetplanJob{HostID: 1, Status: "running"}, &api.NetplanJob{HostID: 2, Status: "applied"}
	n.add(a)
	n.add(b)
	if b.ID <= a.ID {
		t.Errorf("ids %d then %d", a.ID, b.ID)
	}
	if !n.pending(1) || n.pending(2) {
		t.Error("pending")
	}
	if l := n.list(0); len(l) != 2 || l[0].ID != b.ID {
		t.Errorf("list newest first: %+v", l)
	}
}
