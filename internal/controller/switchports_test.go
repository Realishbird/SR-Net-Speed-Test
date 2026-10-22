package controller

import (
	"strings"
	"testing"
	"time"

	"github.com/Realishbird/SR-Net-Speed-Test/internal/api"
)

// sparkLLDP is what the Sparks' agents reported on 2026-10-22, when the
// switch ports had descriptions and each port also heard its twin PCIe
// function: the switch's description, or the twin's netdev, as the "port".
func sparkLLDP() []hostRec {
	port := func(netdev, sw, swPort string) api.HostPort {
		return api.HostPort{Netdev: netdev, LLDPSwitch: sw, LLDPPort: swPort}
	}
	h1 := hostRec{}
	h1.Name, h1.Hostname = "Dell Spark 1", "spark-424f"
	h1.Ports = []api.HostPort{
		port("enp1s0f0np0", "sonic", "enP2p1s0f0np0"),
		port("enP2p1s0f0np0", "sonic", "enp1s0f0np0"),
		port("enp1s0f1np1", "spark-424f", "SPARK-01-RDMA-P2"),
		port("enP2p1s0f1np1", "spark-424f", "SPARK-01-RDMA-P2"),
	}
	h2 := hostRec{}
	h2.Name, h2.Hostname = "Acer Spark", "spark-bde8"
	h2.Ports = []api.HostPort{
		port("enp1s0f0np0", "sonic", "SPARK-07-RDMA"), // the description alone
		port("enp1s0f1np1", "sonic", "Eth1/2/4"),      // the alias
		port("enP2p1s0f1np1", "spark-4256", "enp1s0f1np1"),
		port("enP2p1s0f0np0", "sonic", "Ethernet999"),
	}
	h3 := hostRec{}
	h3.Name, h3.Hostname = "Dell Spark 2", "spark-4256"
	return []hostRec{h1, h2, h3}
}

func sparkIfaces() map[string]map[string]switchIface {
	tab := map[string]switchIface{}
	for _, ifc := range []switchIface{
		{"Ethernet0", "Eth1/1/1", "SPARK-01-RDMA"},
		{"Ethernet3", "Eth1/1/4", "SPARK-07-RDMA"},
		{"Ethernet8", "Eth1/2/1", "SPARK-01-RDMA-P2"},
		{"Ethernet11", "Eth1/2/4", "SPARK-07-RDMA-P2"},
		{"Ethernet20", "Eth1/3/5", "SPARE"},
		{"Ethernet21", "Eth1/3/6", "SPARE"},
	} {
		tab[strings.ToLower(ifc.Name)] = ifc
	}
	return map[string]map[string]switchIface{"z9864f": tab}
}

func TestFillLLDPSwitchTableWins(t *testing.T) {
	switches := []SwitchConfig{{Name: "Z9864F", Address: "176.16.9.22"}}
	lldp := map[string]lldpEntry{
		"spark-424f|enp1s0f0np0":   {Switch: "Z9864F", Port: "Ethernet0"},
		"spark-424f|enp2p1s0f0np0": {Switch: "Z9864F", Port: "Ethernet0"},
		"spark-424f|enp1s0f1np1":   {Switch: "Z9864F", Port: "Ethernet8"},
		"spark-424f|enp2p1s0f1np1": {Switch: "Z9864F", Port: "Ethernet8"},
	}
	hosts := sparkLLDP()
	fillLLDPFrom(hosts, lldp, sparkIfaces(), switches)
	for _, p := range hosts[0].Ports {
		want := "Ethernet0"
		if strings.Contains(p.Netdev, "f1") {
			want = "Ethernet8"
		}
		if p.LLDPSwitch != "Z9864F" || p.LLDPPort != want {
			t.Errorf("Dell Spark 1 %s: %s/%s, want Z9864F/%s from the switch's table", p.Netdev, p.LLDPSwitch, p.LLDPPort, want)
		}
	}
	got := map[string]string{}
	for _, p := range hosts[1].Ports {
		got[p.Netdev] = p.LLDPSwitch + "/" + p.LLDPPort
	}
	for netdev, want := range map[string]string{
		"enp1s0f0np0":   "Z9864F/Ethernet3",  // matched by description
		"enp1s0f1np1":   "Z9864F/Ethernet11", // matched by alias
		"enP2p1s0f1np1": "/",                 // another enrolled host's hostname: a twin, not a switch
		"enP2p1s0f0np0": "/",                 // not an interface of the switch
	} {
		if got[netdev] != want {
			t.Errorf("Acer %s: %q, want %q", netdev, got[netdev], want)
		}
	}
}

func TestFillLLDPWithoutSwitchTables(t *testing.T) {
	// No switch-side tables at all (an SNMP-only switch, or before the
	// first poll): the host's report stands unless it is the host itself.
	switches := []SwitchConfig{{Name: "Z9864F", Address: "176.16.9.22"}}
	hosts := sparkLLDP()
	hosts[0].Ports[0].LLDPPort = "Ethernet0"
	fillLLDPFrom(hosts, nil, nil, switches)
	if p := hosts[0].Ports[0]; p.LLDPPort != "Ethernet0" {
		t.Errorf("plain report dropped: %+v", p)
	}
	if p := hosts[0].Ports[1]; p.LLDPPort != "" {
		t.Errorf("own twin netdev kept as a switch port: %+v", p)
	}
	if p := hosts[0].Ports[2]; p.LLDPPort != "" {
		t.Errorf("own hostname kept as a switch: %+v", p)
	}
}

func TestLookupIfaceAmbiguousDescription(t *testing.T) {
	tab := sparkIfaces()["z9864f"]
	if _, ok := lookupIface(tab, "SPARE"); ok {
		t.Error("a description two ports share matched one of them")
	}
	if ifc, ok := lookupIface(tab, "eth1/1/4"); !ok || ifc.Name != "Ethernet3" {
		t.Errorf("alias lookup: %+v %v", ifc, ok)
	}
}

func TestKnownIfacesFallback(t *testing.T) {
	lldp := map[string]lldpEntry{"spark-424f|enp1s0f0np0": {Switch: "Z9864F", Port: "Ethernet0"}}
	vlans := map[string]string{"z9864f|ethernet8": "101"}
	k := knownIfaces(nil, lldp, vlans)
	if _, ok := k["z9864f"]["ethernet0"]; !ok {
		t.Errorf("LLDP port missing: %+v", k)
	}
	if _, ok := k["z9864f"]["ethernet8"]; !ok {
		t.Errorf("VLAN port missing: %+v", k)
	}
	// A port table, once read, is the whole truth.
	k = knownIfaces(sparkIfaces(), lldp, map[string]string{"z9864f|ethernet99": "100"})
	if _, ok := k["z9864f"]["ethernet99"]; ok {
		t.Error("VLAN table added a port the port table does not list")
	}
}

func TestParsePortListAndNaming(t *testing.T) {
	raw := []byte(`[{"source":"z9864f","timestamp":1,"time":"x","updates":[{"Path":"sonic-port:sonic-port/PORT/PORT_LIST",
	 "values":{"sonic-port:sonic-port/PORT/PORT_LIST":{"sonic-port:PORT_LIST":[
	  {"access_vlan":"101","admin_status":"up","alias":"Eth1/2/1","description":"SPARK-01-RDMA-P2","ifname":"Ethernet8","index":2,"mtu":9100},
	  {"alias":"Eth1/1/1","ifname":"Ethernet0"}]}}}]}]`)
	got := parsePortList(raw)["z9864f"]
	if len(got) != 2 || got[0] != (switchIface{"Ethernet8", "Eth1/2/1", "SPARK-01-RDMA-P2"}) || got[1].Name != "Ethernet0" {
		t.Errorf("port list: %+v", got)
	}
	raw = []byte(`gnmic log line
[{"source":"z9864f","updates":[{"Path":"openconfig-system:system/state","values":{"openconfig-system:system/state":{"openconfig-system:state":{"hostname":"sonic","openconfig-system-deviation:intf-naming-mode":"NATIVE"}}}}]}]`)
	if m := parseNamingMode(raw); m["z9864f"] != "NATIVE" {
		t.Errorf("naming mode: %+v", m)
	}
}

func TestNetworkPlanSwitchLinesFailClosed(t *testing.T) {
	profiles := []api.FabricProfile{{Fabric: "101", VLAN: "101", SwitchMTU: 9100, HostMTU: 9000}}
	switches := []SwitchConfig{{Name: "z9864f", Address: "176.16.9.22"}}
	online := func(int64) bool { return true }

	// Standard naming: the CLI takes the alias.
	plan := buildNetworkPlan(labHosts(), switches, labSwitches("STANDARD"), profiles, online)
	if len(plan.Commands) != 1 || strings.Contains(plan.Commands[0].Lines, "interface Ethernet") ||
		!strings.Contains(plan.Commands[0].Lines, "interface Eth1/") {
		t.Errorf("standard naming lines: %+v", plan.Commands)
	}

	// A port the switch's table does not list: no candidate, no line.
	hosts := labHosts()
	hosts[3].Ports[1].LLDPPort = "SPARK-01-RDMA-P2"
	plan = buildNetworkPlan(hosts, switches, labSwitches("NATIVE"), profiles, online)
	for _, p := range plan.Ports {
		if p.SwitchPort == "SPARK-01-RDMA-P2" {
			t.Errorf("a switch port the switch does not list is in the plan: %+v", p)
		}
	}

	// No port table at all: fixes are listed but no lines are written.
	sws := labSwitches("NATIVE")
	sws.known = nil
	plan = buildNetworkPlan(labHosts(), switches, sws, profiles, online)
	if len(plan.Commands) != 0 {
		t.Errorf("lines written without a port table: %+v", plan.Commands)
	}
	if len(plan.Switch) == 0 || !strings.Contains(strings.Join(plan.Warnings, " "), "No switch lines for") {
		t.Errorf("fixes %+v, warnings %q", plan.Switch, plan.Warnings)
	}
}

func TestParseSwitchClock(t *testing.T) {
	raw := []byte(`[{"source":"z9864f","updates":[{"Path":"openconfig-system:system/state","values":{"openconfig-system:system/state":{"openconfig-system:state":{"current-datetime":"2026-10-22T22:42:49Z","hostname":"sonic","openconfig-system-deviation:intf-naming-mode":"NATIVE"}}}}]}]`)
	got := parseSwitchClock(raw)["z9864f"]
	if want := time.Date(2026, 10, 22, 22, 42, 49, 0, time.UTC); !got.Equal(want) {
		t.Errorf("clock %v, want %v", got, want)
	}
	if m := parseNamingMode(raw); m["z9864f"] != "NATIVE" {
		t.Errorf("naming from the same answer: %v", m)
	}
}
