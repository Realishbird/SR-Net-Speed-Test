package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Realishbird/SR-Net-Speed-Test/internal/api"
)

func TestVMQueryParsesSeries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.RawQuery, "query=rate") {
			t.Errorf("query = %s", r.URL.RawQuery)
		}
		w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
			{"metric":{"__name__":"x","source":"z9864f","interface_name":"Ethernet64"},"value":[1700000000,"97.5"]},
			{"metric":{"source":"z9864f","interface_name":"Ethernet8"},"value":[1700000000,"NaN"]}]}}`))
	}))
	defer srv.Close()
	m := newMetrics(srv.URL)
	rows, err := m.vmQuery(context.Background(), "rate(x[2m])")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Value != 97.5 || rows[0].Labels["interface_name"] != "Ethernet64" {
		t.Errorf("rows = %+v", rows)
	}
	if rows, _ := newMetrics("").vmQuery(context.Background(), "x"); rows != nil {
		t.Error("no VictoriaMetrics URL must mean no rows and no error")
	}
}

func TestSwitchPortStatsPrefersGNMIAndFallsBackToSNMP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		row := func(labels, v string) string { return `{"metric":{` + labels + `},"value":[1,"` + v + `"]}` }
		var rows []string
		switch {
		case strings.HasPrefix(q, `rate({__name__=~"srnet_gnmi_.*in_octets"}`):
			rows = []string{row(`"source":"z9864f","interface_name":"Ethernet64"`, "97.9")}
		case strings.HasPrefix(q, `last_over_time({__name__=~"srnet_gnmi_.*oper_status"}`):
			rows = []string{row(`"source":"z9864f","interface_name":"Ethernet64","oper_status":"UP"`, "1")}
		case strings.HasPrefix(q, "last_over_time(srnet_switch_rx_gbps"):
			rows = []string{row(`"switch":"edge","port":"Ethernet0"`, "12.5"), row(`"switch":"z9864F","port":"Ethernet64"`, "50")}
		case strings.HasPrefix(q, "last_over_time(srnet_switch_up"):
			rows = []string{row(`"switch":"edge","port":"Ethernet0"`, "0")}
		}
		w.Write([]byte(`{"status":"success","data":{"result":[` + strings.Join(rows, ",") + `]}}`))
	}))
	defer srv.Close()
	s := &Server{metrics: newMetrics(srv.URL)}
	s.cfg.Switches = []SwitchConfig{
		{Name: "z9864F", Address: "176.16.9.22", GNMIPort: 8080, GNMIUser: "admin", SNMPCommunity: "ro"},
		{Name: "edge", Address: "10.0.0.9", SNMPCommunity: "ro"},
	}
	ports, source, err := s.switchPortStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if source["z9864F"] != "gnmi" || source["edge"] != "snmp" {
		t.Errorf("source = %v", source)
	}
	p := ports["z9864f/ethernet64"]
	if p == nil || p.RxGbps != 97.9 || p.Up == nil || !*p.Up {
		t.Fatalf("gnmi port = %+v", p)
	}
	// The SNMP row for the gNMI switch must not overwrite gNMI's figure.
	if p.RxGbps == 50 {
		t.Error("snmp overwrote gnmi")
	}
	e := ports["edge/ethernet0"]
	if e == nil || e.RxGbps != 12.5 || e.Up == nil || *e.Up {
		t.Errorf("snmp port = %+v", e)
	}
}

func TestBuildTopologyMapsLLDPToSwitchPorts(t *testing.T) {
	hosts := lab()
	hosts[0].Ports[0].LLDPSwitch, hosts[0].Ports[0].LLDPPort = "z9864f", "Ethernet64" // case differs from config
	hosts[1].Ports[0].LLDPSwitch, hosts[1].Ports[0].LLDPPort = "sonic", "Ethernet8"   // unknown chassis, single switch
	switches := []SwitchConfig{{Name: "Z9864F", Address: "176.16.9.22"}}
	up := true
	ports := map[string]*api.TopoSwitchPort{
		"z9864f/ethernet64": {Switch: "Z9864F", Name: "Ethernet64", RxGbps: 97, Up: &up},
		"z9864f/ethernet32": {Switch: "Z9864F", Name: "Ethernet32", RxGbps: 40}, // busy but no host: still shown
		"z9864f/ethernet0":  {Switch: "Z9864F", Name: "Ethernet0"},              // idle, unmapped: hidden
	}
	live := func(id int64) (float64, float64) {
		if id == 10 {
			return 97, 1
		}
		return 0, 0
	}
	stats := func(host string) (api.HostStats, bool) {
		if host == "Dell 770AP" {
			return api.HostStats{CPUPct: 42}, true
		}
		return api.HostStats{}, false
	}
	tp := buildTopology(hosts, switches, ports, map[string]string{"Z9864F": "gnmi"}, live, stats, func(int64) bool { return true })
	if len(tp.Switches) != 1 || tp.Switches[0].Source != "gnmi" {
		t.Fatalf("switches = %+v", tp.Switches)
	}
	names := []string{}
	for _, p := range tp.Switches[0].Ports {
		names = append(names, p.Name)
	}
	if strings.Join(names, ",") != "Ethernet8,Ethernet32,Ethernet64" {
		t.Errorf("ports = %v", names)
	}
	h := tp.Hosts[0]
	if h.Name != "Dell 770AP" || h.Stats == nil || h.Stats.CPUPct != 42 || h.Ports[0].Switch != "Z9864F" || h.Ports[0].SwitchPort != "Ethernet64" || h.Ports[0].TxGbps != 97 || h.Ports[0].IP != "172.27.100.1" {
		t.Errorf("host 0 = %+v", h)
	}
	if tp.Hosts[1].Ports[0].Switch != "Z9864F" || tp.Hosts[1].Stats != nil {
		t.Errorf("host 1 = %+v", tp.Hosts[1])
	}
	if tp.Hosts[2].Ports[0].Switch != "" || tp.Hosts[2].Ports[0].SwitchPort != "" {
		t.Errorf("host without LLDP must stay unmapped: %+v", tp.Hosts[2].Ports[0])
	}
	for _, p := range tp.Switches[0].Ports {
		if p.Name == "Ethernet64" && (len(p.Hosts) != 1 || p.Hosts[0] != "Dell 770AP") {
			t.Errorf("Ethernet64 hosts = %v", p.Hosts)
		}
	}
}

func TestPortLess(t *testing.T) {
	if !portLess("Ethernet8", "Ethernet64") || portLess("Ethernet64", "Ethernet8") || !portLess("Ethernet8", "PortChannel1") {
		t.Error("port order is wrong")
	}
}

func TestHostStatsTracking(t *testing.T) {
	m := newMetrics("")
	now := time.Now()
	for i := 0; i < 5; i++ {
		m.ingest("spark-1", nil, []api.Sample{{Time: now.Add(time.Duration(i-4) * time.Second),
			Values: map[string]float64{"cpu_util_pct": float64(10 * (i + 1)), "load1": 2.5, "mem_total_bytes": 1000, "mem_used_bytes": 250}}})
	}
	st, ok := m.hostLive("spark-1")
	if !ok || st.CPUPct != 50 || st.Load1 != 2.5 || st.MemPct != 25 {
		t.Errorf("live = %+v ok=%v", st, ok)
	}
	if _, ok := m.hostLive("nobody"); ok {
		t.Error("unknown host must not be live")
	}
	avg, peak, ok := m.hostCPU("spark-1", now.Add(-10*time.Second), now)
	if !ok || avg != 30 || peak != 50 {
		t.Errorf("cpu avg=%v peak=%v ok=%v", avg, peak, ok)
	}
	// A port sample (rdma_dev set) must not be mistaken for a host sample.
	m.ingest("spark-1", nil, []api.Sample{{Time: now, RdmaDev: "mlx5_0", PortNum: 1, Values: map[string]float64{"cpu_util_pct": 99}}})
	if st, _ := m.hostLive("spark-1"); st.CPUPct != 50 {
		t.Errorf("port sample changed host stats: %+v", st)
	}
}
