package agent

import (
	"strings"
	"testing"

	"github.com/Realishbird/SR-Net-Speed-Test/internal/api"
)

const perftestOut = `---------------------------------------------------------------------------------------
                    RDMA_Write BW Test
 Dual-port       : OFF		Device         : mlx5_0
 Number of qps   : 1		Transport type : IB
 Connection type : RC		Using SRQ      : OFF
 TX depth        : 128
 CQ Moderation   : 1
 Mtu             : 4096[B]
 Link type       : Ethernet
 GID index       : 3
 Max inline data : 0[B]
 rdma_cm QPs	 : ON
 Data ex. method : rdma_cm
---------------------------------------------------------------------------------------
 local address: LID 0000 QPN 0x00e8 PSN 0x1a2b3c RKey 0x1fff00 VAddr 0x007f0000000000
 remote address: LID 0000 QPN 0x00e9 PSN 0x4d5e6f RKey 0x1fff01 VAddr 0x007f0000000000
---------------------------------------------------------------------------------------
 #bytes     #iterations    BW peak[Gb/sec]    BW average[Gb/sec]   MsgRate[Mpps]
 65536      1119280          0.00               195.63             0.373128
---------------------------------------------------------------------------------------
`

func TestParsePerftest(t *testing.T) {
	r := ParsePerftest(perftestOut)
	if !r.OK {
		t.Fatal("expected a result row")
	}
	if r.Bytes != 65536 || r.Iterations != 1119280 {
		t.Errorf("bytes/iterations = %d/%d", r.Bytes, r.Iterations)
	}
	if r.AvgGbps != 195.63 || r.MsgRateMpps != 0.373128 {
		t.Errorf("avg=%v mpps=%v", r.AvgGbps, r.MsgRateMpps)
	}
	if ParsePerftest("Couldn't connect to 172.27.100.10:18515\n").OK {
		t.Error("error output must not parse as a result")
	}
}

const domMeasured = `	Identifier                                : 0x11 (QSFP28)
	Vendor name                               : Mellanox
	Vendor PN                                 : MCP1600-C003
	Module temperature                        : 41.25 degrees C / 106.25 degrees F
	Module voltage                            : 3.2812 V
	Power consumption                         : 2.85 W
	Laser tx bias current (Channel 1)         : 7.512 mA
	Laser tx bias current (Channel 2)         : 7.400 mA
	Transmit avg optical power (Channel 1)    : 1.0234 mW / 0.10 dBm
	Rcvr signal avg optical power(Channel 1)  : 0.9876 mW / -0.05 dBm
`

const domRated = `	Identifier                                : 0x11 (QSFP28)
	Extended identifier description           : 3.5W max. Power consumption
	Module temperature                        : 38.00 degrees C / 100.40 degrees F
	Module voltage                            : 3.3000 V
`

func TestParseModuleDOM(t *testing.T) {
	d := ParseModuleDOM(domMeasured)
	if d.TempC == nil || *d.TempC != 41.25 {
		t.Errorf("temp = %v", d.TempC)
	}
	if d.PowerW == nil || *d.PowerW != 2.85 || d.PowerRated {
		t.Errorf("power = %v rated=%v", d.PowerW, d.PowerRated)
	}
	if len(d.TxBiasMa) != 2 || d.TxBiasMa[0] != 7.512 {
		t.Errorf("bias = %v", d.TxBiasMa)
	}
	if len(d.TxPowerMw) != 1 || len(d.RxPowerMw) != 1 {
		t.Errorf("optical power tx=%v rx=%v", d.TxPowerMw, d.RxPowerMw)
	}

	d = ParseModuleDOM(domRated)
	if d.PowerW == nil || *d.PowerW != 3.5 || !d.PowerRated {
		t.Errorf("rated power = %v rated=%v", d.PowerW, d.PowerRated)
	}
	if d.TempC == nil || *d.TempC != 38 {
		t.Errorf("temp = %v", d.TempC)
	}
}

func TestParseEthtoolStats(t *testing.T) {
	s := ParseEthtoolStats("NIC statistics:\n     rx_packets: 100\n     rx_discards_phy: 7\n     tx_pause_ctrl_phy: 3\n     rx_prio3_buf_discard: 0\n     bad line\n")
	if s["rx_discards_phy"] != 7 || s["tx_pause_ctrl_phy"] != 3 {
		t.Errorf("stats = %v", s)
	}
	if _, ok := s["rx_packets"]; ok {
		t.Error("rx_packets is not an error counter and should be skipped")
	}
}

func TestBuildCommand(t *testing.T) {
	base := api.Task{Kind: api.TaskClient, Tool: api.ToolWriteBW, RdmaDev: "mlx5_0", PeerIP: "172.27.100.10", TCPPort: 18515, DurationSec: 60, MsgSize: 65536, QPs: 1, TOS: 106, MTU: 4096}
	args, err := BuildCommand(base)
	if err != nil {
		t.Fatal(err)
	}
	want := "ib_write_bw -d mlx5_0 -R -T 106 -p 18515 -m 4096 -s 65536 -D 60 -F -q 1 --report_gbits 172.27.100.10"
	if got := join(args); got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	srv := base
	srv.Kind = api.TaskServer
	args, _ = BuildCommand(srv)
	if args[len(args)-1] != "--report_gbits" {
		t.Errorf("server must not get a peer address: %v", args)
	}
	for name, bad := range map[string]func(*api.Task){
		"tool": func(t *api.Task) { t.Tool = "bash" },
		"dev":  func(t *api.Task) { t.RdmaDev = "mlx5_0; rm -rf /" },
		"ip":   func(t *api.Task) { t.PeerIP = "example.com" },
		"port": func(t *api.Task) { t.TCPPort = 80 },
		"mtu":  func(t *api.Task) { t.MTU = 9000 },
		"dur":  func(t *api.Task) { t.DurationSec = 0 },
		"size": func(t *api.Task) { t.MsgSize = 0 },
		"qps":  func(t *api.Task) { t.QPs = 0 },
		"tos":  func(t *api.Task) { t.TOS = 300 },
	} {
		tk := base
		bad(&tk)
		if _, err := BuildCommand(tk); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func join(a []string) string {
	s := ""
	for i, x := range a {
		if i > 0 {
			s += " "
		}
		s += x
	}
	return s
}

func TestBuildCommandOtherTools(t *testing.T) {
	base := api.Task{Kind: api.TaskClient, RdmaDev: "mlx5_0", LocalIP: "172.27.100.1", PeerIP: "172.27.100.10", TCPPort: 18516, DurationSec: 30, MsgSize: 2, QPs: 1, TOS: 106, MTU: 4096}
	lat := base
	lat.Tool = api.ToolWriteLat
	args, err := BuildCommand(lat)
	if err != nil {
		t.Fatal(err)
	}
	if got := join(args); got != "ib_write_lat -d mlx5_0 -R -T 106 -p 18516 -m 4096 -s 2 -D 30 -F 172.27.100.10" {
		t.Errorf("lat: %s", got)
	}
	bidir := base
	bidir.Tool, bidir.Bidir = api.ToolSendBW, true
	args, _ = BuildCommand(bidir)
	if got := join(args); !strings.Contains(got, " -b ") {
		t.Errorf("bidir flag missing: %s", got)
	}
	ip := base
	ip.Tool, ip.Streams, ip.Protocol, ip.MsgSize = api.ToolIperf3, 8, "udp", 65507
	args, err = BuildCommand(ip)
	if err != nil {
		t.Fatal(err)
	}
	if got := join(args); got != "iperf3 --json -p 18516 -B 172.27.100.1 -c 172.27.100.10 -t 30 -P 8 -l 65507 -S 106 -u -b 0" {
		t.Errorf("iperf3 client: %s", got)
	}
	ip.Kind = api.TaskServer
	args, _ = BuildCommand(ip)
	if got := join(args); got != "iperf3 --json -p 18516 -B 172.27.100.1 -s -1" {
		t.Errorf("iperf3 server: %s", got)
	}
}

const latOut = `---------------------------------------------------------------------------------------
                    RDMA_Write Latency Test
---------------------------------------------------------------------------------------
 #bytes #iterations    t_min[usec]    t_max[usec]  t_typical[usec]    t_avg[usec]    t_stdev[usec]   99% percentile[usec]   99.9% percentile[usec]
 2       1000000          1.63           27.11          1.72              1.74            0.09            2.05                    4.31
---------------------------------------------------------------------------------------
`

func TestParsePerftestLat(t *testing.T) {
	lat, ok := ParsePerftestLat(latOut)
	if !ok {
		t.Fatal("expected a row")
	}
	if lat.MinUs != 1.63 || lat.TypicalUs != 1.72 || lat.P99Us != 2.05 || lat.P999Us != 4.31 {
		t.Errorf("lat = %+v", lat)
	}
	if _, ok := ParsePerftestLat("Failed to connect\n"); ok {
		t.Error("error output must not parse")
	}
}

func TestParseIperf3(t *testing.T) {
	tcp := `{"end":{"sum_sent":{"bits_per_second":9.4e10,"retransmits":17},"sum_received":{"bits_per_second":9.38e10}}}`
	r, ok := ParseIperf3("some banner\n" + tcp)
	if !ok || r.Gbps < 93.7 || r.Gbps > 93.9 || r.Retransmits != 17 {
		t.Errorf("tcp = %+v ok=%v", r, ok)
	}
	bidir := `{"end":{"sum_sent":{"bits_per_second":9.4e10,"retransmits":3},"sum_received":{"bits_per_second":9.7e10},"sum_sent_bidir_reverse":{"bits_per_second":9.6e10,"retransmits":4},"sum_received_bidir_reverse":{"bits_per_second":9.6e10}}}`
	r, ok = ParseIperf3(bidir)
	if !ok || r.Gbps < 192.9 || r.Gbps > 193.1 || r.Retransmits != 7 {
		t.Errorf("bidir = %+v ok=%v", r, ok)
	}
	udp := `{"end":{"sum":{"bits_per_second":2.5e10,"jitter_ms":0.012,"lost_percent":0.4}}}`
	r, ok = ParseIperf3(udp)
	if !ok || r.Gbps != 25 || r.LossPct != 0.4 || r.JitterMs != 0.012 {
		t.Errorf("udp = %+v ok=%v", r, ok)
	}
	if _, ok := ParseIperf3(`{"error":"unable to connect to server"}`); ok {
		t.Error("iperf3 error must not parse as a result")
	}
}

func TestParseLLDP(t *testing.T) {
	out := "lldp.enp1s0f0np0.via=LLDP\nlldp.enp1s0f0np0.chassis.name=sonic-z9864\nlldp.enp1s0f0np0.port.ifname=Ethernet64\nlldp.enp1s0f0np0.port.descr=Ethernet64\nlldp.eno1.chassis.name=mgmt-sw\nlldp.eno1.port.ifname=ge-0/0/3\n"
	n := ParseLLDP(out)
	if n["enp1s0f0np0"] != [2]string{"sonic-z9864", "Ethernet64"} || n["eno1"][1] != "ge-0/0/3" {
		t.Errorf("lldp = %v", n)
	}
}
