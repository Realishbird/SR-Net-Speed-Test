package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/Realishbird/SR-Net-Speed-Test/internal/api"
)

func TestIdleErrorsSkipTestGrowthAndSelfResolve(t *testing.T) {
	a := newAlerter(nil, newHub(), Config{})
	t0 := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }

	if grew, quiet := a.idleErrors("h|p", 10, at(0), false); grew != 0 || quiet {
		t.Fatalf("first sample: grew %v quiet %v", grew, quiet)
	}
	if grew, _ := a.idleErrors("h|p", 10, at(15), false); grew != 0 {
		t.Fatalf("flat counters alerted: %v", grew)
	}
	// A run on the host pushes the counters up; the first idle sample after it
	// must only record the new level.
	a.idleErrors("h|p", 500, at(30), true)
	a.idleErrors("h|p", 900, at(60), true)
	if grew, quiet := a.idleErrors("h|p", 900, at(90), false); grew != 0 || quiet {
		t.Fatalf("growth during the test was reported as idle: grew %v quiet %v", grew, quiet)
	}
	// Growth while idle is the real signal.
	if grew, _ := a.idleErrors("h|p", 903, at(105), false); grew != 3 {
		t.Fatalf("idle growth: %v", grew)
	}
	// Flat for less than idleQuietFor: still open.
	if grew, quiet := a.idleErrors("h|p", 903, at(105+600), false); grew != 0 || quiet {
		t.Fatalf("resolved too early: grew %v quiet %v", grew, quiet)
	}
	// Flat for idleQuietFor: resolves once.
	if _, quiet := a.idleErrors("h|p", 903, at(105+int(idleQuietFor.Seconds())), false); !quiet {
		t.Fatal("did not resolve after a quiet period")
	}
	if _, quiet := a.idleErrors("h|p", 903, at(105+int(idleQuietFor.Seconds())+15), false); quiet {
		t.Fatal("resolved twice")
	}
	// Another port is independent.
	if grew, _ := a.idleErrors("h|q", 1, at(200), false); grew != 0 {
		t.Fatalf("new port alerted on its first sample: %v", grew)
	}
}

func TestAdminLoginCachesOneCredential(t *testing.T) {
	hash, _ := bcrypt.GenerateFromPassword([]byte("hunter22"), bcrypt.MinCost)
	cfg := DefaultConfig()
	cfg.AdminUser, cfg.AdminPasswordHash = "admin", string(hash)
	s := &Server{cfg: cfg}
	if s.adminLogin("admin", "wrong") || s.adminLogin("root", "hunter22") {
		t.Fatal("bad credential accepted")
	}
	if s.adminOKHash != "" {
		t.Fatal("a failed login was cached")
	}
	if !s.adminLogin("admin", "hunter22") {
		t.Fatal("good credential rejected")
	}
	if s.adminOKHash != string(hash) {
		t.Fatal("good credential not cached")
	}
	if !s.adminLogin("admin", "hunter22") || s.adminLogin("admin", "hunter23") {
		t.Fatal("cache answered wrongly")
	}
	// A password change invalidates the cache by itself.
	hash2, _ := bcrypt.GenerateFromPassword([]byte("newpass99"), bcrypt.MinCost)
	s.cfg.AdminPasswordHash = string(hash2)
	if s.adminLogin("admin", "hunter22") {
		t.Fatal("old password still accepted after the hash changed")
	}
	if !s.adminLogin("admin", "newpass99") || s.adminOKHash != string(hash2) {
		t.Fatal("new password rejected or not cached")
	}
}

func TestVersionLessIsNumeric(t *testing.T) {
	ordered := []string{"v0.1.9", "v0.1.18", "v0.1.19-rc1", "v0.1.19", "v0.2.0", "v1.0.0"}
	for i := 0; i+1 < len(ordered); i++ {
		if !versionLess(ordered[i], ordered[i+1]) || versionLess(ordered[i+1], ordered[i]) {
			t.Errorf("%s should sort before %s", ordered[i], ordered[i+1])
		}
	}
}

func TestSwitchLLDPFillsHostPorts(t *testing.T) {
	// gnmic's rendering of a Get on /openconfig-lldp:lldp/interfaces from
	// the lab's Z9864F: the Spark's cabled port is seen twice (both PCIe
	// functions advertise), the 770AP once; the interface's own state
	// object carries counters, not a neighbour.
	raw := []byte(`version=0.38.2
[{"source":"z9864f","timestamp":1,"time":"2026-10-21T20:16:12Z","updates":[{"Path":"openconfig-lldp:lldp/interfaces","values":{"openconfig-lldp:lldp/interfaces":{"openconfig-lldp:interfaces":{"interface":[
	{"config":{"name":"Ethernet0"},"name":"Ethernet0","neighbors":{"neighbor":[
		{"id":"fc:4c:ea:f9:42:4f","state":{"chassis-id":"fc:4c:ea:f9:42:4f","id":"135","management-address":"176.16.252.133","port-description":"enp1s0f0np0","port-id":"fc:4c:ea:f9:42:4f","system-name":"spark-424f","ttl":120}},
		{"id":"fc:4c:ea:f9:42:53","state":{"chassis-id":"fc:4c:ea:f9:42:4f","id":"136","port-description":"enP2p1s0f0np0","port-id":"fc:4c:ea:f9:42:53","system-name":"spark-424f"}}]},
	 "state":{"counters":{"frame-in":"240118"},"name":"Ethernet0"}},
	{"config":{"name":"Ethernet120"},"name":"Ethernet120","neighbors":{"neighbor":[
		{"id":"7","state":{"port-description":"eno16695np0","port-id":"aa:bb","system-name":"dell770ap"}}]},"state":{"name":"Ethernet120"}},
	{"config":{"name":"Ethernet8"},"name":"Ethernet8","state":{"name":"Ethernet8"}}
]}}}}]}]`)
	got := parseLLDPGet(raw)
	nb := got["z9864f"]
	if len(nb) != 3 {
		t.Fatalf("neighbours: %+v", nb)
	}
	if nb[0] != (lldpNeighbour{Iface: "Ethernet0", SystemName: "spark-424f", PortDesc: "enp1s0f0np0", PortID: "fc:4c:ea:f9:42:4f"}) {
		t.Errorf("first neighbour: %+v", nb[0])
	}
	if nb[2].Iface != "Ethernet120" || nb[2].SystemName != "dell770ap" {
		t.Errorf("770AP: %+v", nb[2])
	}

	cfg := DefaultConfig()
	cfg.Switches = []SwitchConfig{{Name: "Z9864F", Address: "176.16.9.22", GNMIUser: "admin", GNMIPort: 8080}}
	s := &Server{cfg: cfg, lldp: map[string]lldpEntry{}}
	for _, n := range nb {
		s.lldp[strings.ToLower(n.SystemName)+"|"+strings.ToLower(n.PortDesc)] = lldpEntry{Switch: "Z9864F", Port: n.Iface}
	}
	hosts := []hostRec{
		{Host: api.Host{Name: "Dell Spark 1", Hostname: "spark-424f", Ports: []api.HostPort{{Netdev: "enp1s0f0np0"}, {Netdev: "enP2p1s0f0np0"}, {Netdev: "enp1s0f1np1"}}}},
		{Host: api.Host{Name: "Dell 770AP", Hostname: "DELL770AP", Ports: []api.HostPort{{Netdev: "eno16695np0", LLDPSwitch: "other", LLDPPort: "Ethernet9"}}}},
	}
	s.fillLLDP(context.Background(), hosts)
	if p := hosts[0].Ports[0]; p.LLDPSwitch != "Z9864F" || p.LLDPPort != "Ethernet0" {
		t.Errorf("spark port 0: %+v", p)
	}
	if p := hosts[0].Ports[1]; p.LLDPPort != "Ethernet0" {
		t.Errorf("spark twin function: %+v", p)
	}
	if p := hosts[0].Ports[2]; p.LLDPPort != "" {
		t.Errorf("uncabled port got a neighbour: %+v", p)
	}
	if p := hosts[1].Ports[0]; p.LLDPPort != "Ethernet9" {
		t.Errorf("host's own LLDP report was overwritten: %+v", p)
	}
	if got := speedMbps("openconfig-if-ethernet:SPEED_100GB"); got != 100000 {
		t.Errorf("speed: %d", got)
	}
	if got := speedMbps("SPEED_2500MB"); got != 2500 {
		t.Errorf("speed: %d", got)
	}
	if got := speedMbps("SPEED_UNKNOWN"); got != 0 {
		t.Errorf("speed: %d", got)
	}
}

func TestSendTestExpectsRNRAndFlushCounters(t *testing.T) {
	for _, c := range []struct {
		test, key string
		isError   bool
	}{
		{"send", "rnr_nak_retry_err", false}, {"send", "resp_cqe_error", false}, {"send_lat", "resp_cqe_error", false},
		{"send", "out_of_sequence", true}, {"write", "rnr_nak_retry_err", true}, {"read", "resp_cqe_error", true},
		{"send", "np_cnp_sent", false}, // congestion signalling is never an error
	} {
		if got := counterIsError(c.test, c.key); got != c.isError {
			t.Errorf("%s %s: isError %v, want %v", c.test, c.key, got, c.isError)
		}
	}
}

func TestSwitchVLANsFromSonicTable(t *testing.T) {
	raw := []byte(`[{"source":"z9864f","timestamp":1,"time":"2026-10-21T20:19:47Z","updates":[{"Path":"sonic-vlan:sonic-vlan/VLAN_MEMBER","values":{"sonic-vlan:sonic-vlan/VLAN_MEMBER":{"sonic-vlan:VLAN_MEMBER":{"VLAN_MEMBER_LIST":[
		{"ifname":"Ethernet0","name":"Vlan100","tagging_mode":"untagged"},
		{"ifname":"Ethernet4","name":"Vlan101","tagging_mode":"untagged"},
		{"ifname":"Ethernet120","name":"Vlan100","tagging_mode":"untagged"},
		{"ifname":"Ethernet128","name":"Vlan200","tagging_mode":"tagged"},
		{"ifname":"Ethernet128","name":"Vlan100","tagging_mode":"untagged"}]}}}}]}]`)
	got := parseVLANMembers(raw)["z9864f"]
	want := map[string]string{"Ethernet0": "100", "Ethernet4": "101", "Ethernet120": "100", "Ethernet128": "100"}
	if len(got) != len(want) {
		t.Fatalf("members: %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: got %q want %q", k, got[k], v)
		}
	}
	topo := api.Topology{
		Switches: []api.TopoSwitch{{Name: "Z9864F", Ports: []api.TopoSwitchPort{{Name: "Ethernet0"}, {Name: "Ethernet9"}}}},
		Hosts: []api.TopoHost{{Name: "Dell Spark 1", Ports: []api.TopoHostPort{
			{Netdev: "a", Switch: "Z9864F", SwitchPort: "Ethernet0"},
			{Netdev: "b", Switch: "Z9864F", SwitchPort: "Ethernet4", Fabric: "100"}}}},
	}
	applyVLANs(&topo, map[string]string{"z9864f|ethernet0": "100", "z9864f|ethernet4": "101"})
	if p := topo.Switches[0].Ports; p[0].VLAN != "100" || p[1].VLAN != "" {
		t.Errorf("switch port vlans: %+v", p)
	}
	if p := topo.Hosts[0].Ports; p[0].Fabric != "100" || p[1].Fabric != "100" {
		t.Errorf("host port fabrics: %+v (unset takes the VLAN, a set one is kept for the mismatch)", p)
	}
}
