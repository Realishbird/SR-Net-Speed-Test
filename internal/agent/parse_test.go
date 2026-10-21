package agent

import (
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
	want := "ib_write_bw -d mlx5_0 -R -T 106 -p 18515 -m 4096 -s 65536 -q 1 -D 60 -F --report_gbits 172.27.100.10"
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
