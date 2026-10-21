package controller

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	// VictoriaMetrics answers the two LLDP leaf queries with one neighbour
	// on Ethernet0 and the 770AP on Ethernet120; the Spark's lldpd said nothing.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		row := func(leaf, iface, id, val string) string {
			return fmt.Sprintf(`{"metric":{"__name__":"srnet_gnmi_openconfig_lldp_lldp_interfaces_interface_neighbors_neighbor_state_%s","source":"z9864f","name":"%s","id":"%s","%s":"%s"},"value":[1,"1"]}`, leaf, iface, id, leaf, val)
		}
		var rows []string
		switch {
		case strings.Contains(q, "system_name"):
			rows = []string{row("system_name", "Ethernet0", "1", "spark-424f"), row("system_name", "Ethernet120", "7", "dell770ap")}
		case strings.Contains(q, "port_description"):
			rows = []string{row("port_description", "Ethernet0", "1", "enp1s0f0np0"), row("port_description", "Ethernet120", "7", "eno16695np0")}
		}
		fmt.Fprintf(w, `{"status":"success","data":{"result":[%s]}}`, strings.Join(rows, ","))
	}))
	defer srv.Close()
	cfg := DefaultConfig()
	cfg.Switches = []SwitchConfig{{Name: "Z9864F", Address: "176.16.9.22", GNMIUser: "admin", GNMIPort: 8080}}
	s := &Server{cfg: cfg, metrics: newMetrics(srv.URL)}
	hosts := []hostRec{
		{Host: api.Host{Name: "Dell Spark 1", Hostname: "spark-424f", Ports: []api.HostPort{{Netdev: "enp1s0f0np0"}, {Netdev: "enp1s0f1np1"}}}},
		{Host: api.Host{Name: "Dell 770AP", Hostname: "DELL770AP", Ports: []api.HostPort{{Netdev: "eno16695np0", LLDPSwitch: "other", LLDPPort: "Ethernet9"}}}},
	}
	s.fillLLDP(context.Background(), hosts)
	if p := hosts[0].Ports[0]; p.LLDPSwitch != "Z9864F" || p.LLDPPort != "Ethernet0" {
		t.Errorf("spark port 0: %+v", p)
	}
	if p := hosts[0].Ports[1]; p.LLDPPort != "" {
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
