package controller

import (
	"strings"
	"testing"

	"github.com/Realishbird/SR-Net-Speed-Test/internal/api"
)

// gnmic Get output as this SONiC answers it: a log line, then one entry per
// target with the YANG container under values.
const sonicPortGet = `2026/10/21 22:40:00 some gnmic log line
[
  {
    "source": "z9864f",
    "timestamp": 1,
    "time": "2026-10-21T22:40:00Z",
    "updates": [
      {
        "Path": "sonic-port:sonic-port/PORT",
        "values": {
          "sonic-port:sonic-port/PORT": {
            "PORT_LIST": [
              {"ifname": "Ethernet0", "admin_status": "up", "mtu": 9100, "speed": 100000, "fec": "rs", "lanes": "1,2"},
              {"ifname": "Ethernet120", "admin_status": "up", "mtu": "9100", "speed": "400000", "fec": "rs"},
              {"ifname": "Ethernet4", "admin_status": "down", "mtu": 1500, "speed": 100000}
            ]
          }
        }
      }
    ]
  }
]`

const sonicQoSMapGet = `[{"source":"z9864f","updates":[{"values":{"sonic-port-qos-map:sonic-port-qos-map":{"PORT_QOS_MAP":{"PORT_QOS_MAP_LIST":[
 {"ifname":"CPU","dscp_to_tc_map":"[DSCP_TO_TC_MAP|AZURE]","tc_to_queue_map":"AZURE","pfc_enable":"3,4"},
 {"ifname":"Ethernet0","dscp_to_tc_map":"AZURE","tc_to_queue_map":"AZURE","pfc_enable":"3"}
]}}}}]}]`

const qosInterfacesGet = `[{"source":"z9864f:8080","updates":[{"values":{"openconfig-qos:qos/interfaces":{"interface":[
 {"interface-id":"Ethernet0","config":{"interface-id":"Ethernet0"},
  "pfc":{"pfc-priorities":{"pfc-priority":[
    {"dot1p":0,"state":{"dot1p":0,"enable":false,"pause-frames-rx":"0","pause-frames-tx":"0"}},
    {"dot1p":3,"state":{"dot1p":3,"enable":true,"pause-frames-rx":"12","pause-frames-tx":"7"}}]}},
  "output":{"queues":{"queue":[
    {"name":"Ethernet0:0","state":{"name":"Ethernet0:0","transmit-pkts":"8000000000","dropped-pkts":"416888","openconfig-qos-ext:ecn-marked-pkts":"0","openconfig-qos-ext:wred-dropped-pkts":"0"}},
    {"name":"Ethernet0:3","state":{"name":"Ethernet0:3","transmit-pkts":"10","dropped-pkts":"0","openconfig-qos-ext:ecn-marked-pkts":"55","openconfig-qos-ext:wred-dropped-pkts":"0","openconfig-qos-ext:wred-profile":"AZURE_LOSSLESS"}}]}}},
 {"interface-id":"Ethernet120","pfc":{"pfc-priorities":{"pfc-priority":[{"dot1p":3,"state":{"enable":false}}]}},
  "output":{"queues":{"queue":[{"name":"Ethernet120:0","state":{"dropped-pkts":"0"}}]}}}
]}}}]}]`

func TestParseSwitchSettings(t *testing.T) {
	ports := parseSonicPorts([]byte(sonicPortGet))["z9864f"]
	if len(ports) != 3 {
		t.Fatalf("ports: %+v", ports)
	}
	e0, e120, e4 := ports["Ethernet0"], ports["Ethernet120"], ports["Ethernet4"]
	if e0.MTU != 9100 || e0.SpeedMbps != 100000 || e0.FEC != "rs" || e0.AdminUp == nil || !*e0.AdminUp {
		t.Fatalf("Ethernet0: %+v", e0)
	}
	if e120.MTU != 9100 || e120.SpeedMbps != 400000 {
		t.Fatalf("string numbers: %+v", e120)
	}
	if e4.AdminUp == nil || *e4.AdminUp || e4.MTU != 1500 {
		t.Fatalf("Ethernet4: %+v", e4)
	}

	qm := parseSonicQoSMap([]byte(sonicQoSMapGet))["z9864f"]
	if qm["CPU"].DSCPMap != "AZURE" || qm["CPU"].TCQueueMap != "AZURE" || prioList(qm["CPU"].PFC) != "3,4" {
		t.Fatalf("CPU: %+v", qm["CPU"])
	}
	if qm["Ethernet0"].DSCPMap != "AZURE" || prioList(qm["Ethernet0"].PFC) != "3" {
		t.Fatalf("Ethernet0: %+v", qm["Ethernet0"])
	}
	if _, ok := qm["Ethernet4"]; ok {
		t.Fatal("a port without an entry must stay absent (no QoS map)")
	}

	q := parseQoSInterfaces([]byte(qosInterfacesGet))["z9864f:8080"]
	if len(q) != 2 {
		t.Fatalf("qos interfaces: %d", len(q))
	}
	c := q["Ethernet0"]
	if !c.PFCRead || prioList(c.PFC) != "3" || c.PauseRx != 12 || c.PauseTx != 7 || !c.PauseRead {
		t.Fatalf("Ethernet0 pfc: %+v", c)
	}
	if c.Queues[0].Dropped != 416888 || c.Queues[3].ECNMarked != 55 || c.Queues[3].WREDProfile != "AZURE_LOSSLESS" || c.Queues[0].WREDProfile != "" {
		t.Fatalf("Ethernet0 queues: %+v", c.Queues)
	}
	d := q["Ethernet120"]
	if !d.PFCRead || prioList(d.PFC) != "none" || d.Queues[0].Dropped != 0 {
		t.Fatalf("Ethernet120: %+v", d)
	}
}

func TestCheckCableLossyLabWithoutProfile(t *testing.T) {
	up := true
	spc := &switchPortCfg{MTU: 9100, SpeedMbps: 100000, FEC: "rs", AdminUp: &up, PFC: []int{}, PFCRead: true, Queues: map[int]queueStat{0: {Dropped: 416888}}, PauseRead: true}
	v := hostPortView{host: "spark-424f", mtu: 9000, port: api.HostPort{Netdev: "enp1s0f0np0", Addrs: []string{"172.27.100.10/24"}, State: "ACTIVE", SpeedMbps: 100000, Fabric: "100", Label: "P1-B1-A"},
		settings: &api.PortSettings{RoCEMode: "v2", PFC: "none", Trust: "pcp", ECNNp: "none", ECNRp: "none", CNPDSCP: 48, TOS: 106}}
	cc := checkCable(v, "100", "z9864f", "Ethernet0", spc, "100", api.FabricProfile{}, false)
	if !cc.OK {
		t.Fatalf("a consistent lossy cable with no profile must pass: %+v", cc.Checks)
	}
	if cc.IP != "172.27.100.10" || cc.Counters["queue 0 dropped"] != 416888 {
		t.Fatalf("%+v %+v", cc, cc.Counters)
	}
	byName := map[string]api.SettingCheck{}
	for _, c := range cc.Checks {
		byName[c.Name] = c
	}
	if byName["PFC"].Switch != "none" || byName["PFC"].Host != "none" || byName["PFC"].State != "ok" {
		t.Fatalf("pfc: %+v", byName["PFC"])
	}
	if byName["Switch QoS map"].Switch != "none (queue 0)" || byName["Switch QoS map"].State != "ok" {
		t.Fatalf("qos map: %+v", byName["Switch QoS map"])
	}

	// The same cable against the lossless preset fails on exactly the right things.
	prof := fabricPreset("lossless")
	prof.Fabric = "100"
	cc = checkCable(v, "100", "z9864f", "Ethernet0", spc, "100", prof, true)
	if cc.OK {
		t.Fatal("lossless profile on a lossy cable passed")
	}
	bad := map[string]string{}
	for _, c := range cc.Checks {
		if c.State == "bad" {
			bad[c.Name] = c.Note
		}
	}
	for _, want := range []string{"PFC", "Switch QoS map", "Host trust", "ECN", "Drops"} {
		if _, ok := bad[want]; !ok {
			t.Errorf("lossless profile did not flag %s; flagged: %v", want, bad)
		}
	}
	if len(bad) != 5 {
		t.Errorf("unexpected failures: %v", bad)
	}
	if !strings.Contains(bad["PFC"], "switch has PFC none, profile wants priority 3") {
		t.Errorf("pfc note: %s", bad["PFC"])
	}
}

func TestCheckCableConsistencyFaults(t *testing.T) {
	up := true
	spc := &switchPortCfg{MTU: 1500, SpeedMbps: 200000, AdminUp: &up, PFC: []int{3}, PFCRead: true, Queues: map[int]queueStat{}}
	v := hostPortView{host: "h", mtu: 9000, port: api.HostPort{Netdev: "eth0", Addrs: []string{"10.0.0.1/24"}, State: "ACTIVE", SpeedMbps: 100000, Fabric: "100"},
		settings: &api.PortSettings{RoCEMode: "v1", PFC: "none"}}
	cc := checkCable(v, "100", "sw", "Ethernet4", spc, "101", api.FabricProfile{}, false)
	bad := map[string]string{}
	for _, c := range cc.Checks {
		if c.State == "bad" {
			bad[c.Name] = c.Note
		}
	}
	for name, note := range map[string]string{
		"Speed": "different speeds", "MTU": "switch port MTU 1500 is below 4200", "VLAN": "switch port is in VLAN 101, fabric 100 expects 100",
		"RoCE mode": "RoCE v1 only", "PFC": "switch has PFC on 3, host on none",
	} {
		if !strings.Contains(bad[name], note) {
			t.Errorf("%s: got %q, want %q", name, bad[name], note)
		}
	}
	// No switch data at all: nothing on the switch side is judged, the host side still is.
	cc = checkCable(hostPortView{host: "h", port: api.HostPort{Netdev: "eth0", State: "DOWN"}}, "", "", "", nil, "", api.FabricProfile{}, false)
	if cc.OK || cc.Checks[0].Name != "Link" || cc.Checks[0].State != "bad" {
		t.Fatalf("down, uncabled port: %+v", cc.Checks[0])
	}
	unknown := 0
	for _, c := range cc.Checks[1:] {
		if c.State == "unknown" {
			unknown++
		}
	}
	if unknown < 4 {
		t.Fatalf("settings with nothing to read must be unknown, not bad: %+v", cc.Checks)
	}
}

func TestFabricPresets(t *testing.T) {
	l := fabricPreset("lossless")
	if l.PFCPriority != 3 || l.ECNQueue != 3 || l.RoCEDSCP != 26 || l.CNPDSCP != 48 || l.Trust != "dscp" || !l.HostECN || l.SwitchMTU != 9100 {
		t.Fatalf("%+v", l)
	}
	y := fabricPreset("lossy")
	if y.PFCPriority != -1 || y.ECNQueue != -1 || y.RoCEDSCP != -1 || y.HostECN || y.RoCEMode != "v2" {
		t.Fatalf("%+v", y)
	}
}

// A Spark's ConnectX-7 shows its one cabled port as two PCIe functions; the
// switch lists both on the same switch port. Only the configured half is a
// cable the fabric check holds to a profile.
func TestCableInUseSkipsTwinsAndUnusedPorts(t *testing.T) {
	h := hostRec{Host: api.Host{Name: "Dell Spark 1", Ports: []api.HostPort{
		{Netdev: "enp1s0f0np0", Addrs: []string{"172.27.100.10/24"}, Label: "P1-B1-A", Fabric: "100", State: "ACTIVE", LLDPSwitch: "z9864f", LLDPPort: "Ethernet0"},
		{Netdev: "enP2p1s0f0np0", State: "ACTIVE", LLDPSwitch: "z9864f", LLDPPort: "Ethernet0"},
		{Netdev: "enp2s0f1np1", State: "DOWN"},
		{Netdev: "eth9", Label: "spare", State: "DOWN", LLDPSwitch: "z9864f", LLDPPort: "Ethernet9"},
	}}}
	want := []bool{true, false, false, false}
	for i, p := range h.Ports {
		if got := cableInUse(h, p); got != want[i] {
			t.Errorf("cableInUse(%s) = %v, want %v", p.Netdev, got, want[i])
		}
	}
	// A labelled port without an address on its own cable is still checked.
	if !cableInUse(hostRec{}, api.HostPort{Netdev: "x", Label: "P2", State: "ACTIVE", LLDPSwitch: "sw", LLDPPort: "Ethernet4"}) {
		t.Error("labelled port on its own cable should be checked")
	}
}

// TestCheckCablePausingHost: the new Dell's Broadcom NICs, whose PFC setting
// the agent cannot read, sent pause frames to switch ports with PFC off.
func TestCheckCablePausingHost(t *testing.T) {
	up := true
	spc := &switchPortCfg{MTU: 9100, SpeedMbps: 400000, AdminUp: &up, PFC: []int{}, PFCRead: true, PauseRx: 111, PauseRead: true, Queues: map[int]queueStat{}}
	v := hostPortView{host: "dellr7615", mtu: 9000, port: api.HostPort{Netdev: "eno16595np0", Addrs: []string{"172.27.100.2/24"}, State: "ACTIVE", SpeedMbps: 400000, Fabric: "100"}}
	cc := checkCable(v, "100", "z9864f", "Ethernet128", spc, "100", api.FabricProfile{}, false)
	for _, c := range cc.Checks {
		if c.Name == "PFC" {
			if c.State != "bad" || c.Host != "sends pause frames" || !strings.Contains(c.Note, "received 111 PFC pause frames") {
				t.Fatalf("pfc check: %+v", c)
			}
			return
		}
	}
	t.Fatal("no PFC check")
}

func TestPFCIgnored(t *testing.T) {
	deltas := map[string]float64{"Ethernet132 prio 3 pause_frames_rx": 46, "Ethernet132 prio 4 pause_frames_rx": 2, "Ethernet132 queue 0 dropped_pkts": 2, "Ethernet1320 prio 3 pause_frames_rx": 9}
	got := pfcIgnored("Ethernet132", deltas, map[string]bool{"3": true, "4": false})
	if len(got) != 1 || got[0] != "Ethernet132 priority 3 (46)" {
		t.Fatalf("got %v", got)
	}
	if pfcIgnored("Ethernet132", deltas, nil) != nil {
		t.Fatal("unknown PFC state must not flag")
	}
}
