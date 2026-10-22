package agent

import (
	"strings"
	"testing"
)

func TestParseDCBPFC(t *testing.T) {
	out := `{"pfc-cap":8,"macsec-bypass":false,"delay":0,"prio-pfc":{"0":false,"1":false,"2":false,"3":true,"4":false,"5":false,"6":false,"7":false},"requests":0,"indications":0}`
	if pfc, ok := ParseDCBPFC(out); !ok || pfc != "3" {
		t.Fatalf("pfc = %q ok=%v", pfc, ok)
	}
	off := `{"pfc-cap":8,"prio-pfc":{"0":false,"1":false,"2":false,"3":false,"4":false,"5":false,"6":false,"7":false}}`
	if pfc, ok := ParseDCBPFC(off); !ok || pfc != "none" {
		t.Fatalf("all off: %q ok=%v", pfc, ok)
	}
	if _, ok := ParseDCBPFC("Error: Unknown device"); ok {
		t.Fatal("garbage parsed")
	}
}

func TestParseDCBApp(t *testing.T) {
	out := `{"ethtype-prio":[],"stream-port-prio":[],"dgram-port-prio":[],"port-prio":[],"pcp-prio":[],"dscp-prio":[[26,3],[48,6]]}`
	if d, trust := ParseDCBApp(out); d != "26:3,48:6" || trust != "dscp" {
		t.Fatalf("dscp map: %q trust %q", d, trust)
	}
	if d, trust := ParseDCBApp(`{"pcp-prio":[[0,0],[3,3]],"dscp-prio":[]}`); d != "" || trust != "pcp" {
		t.Fatalf("pcp only: %q trust %q", d, trust)
	}
	if d, trust := ParseDCBApp(`{"pcp-prio":[],"dscp-prio":[]}`); d != "" || trust != "" {
		t.Fatalf("empty: %q trust %q", d, trust)
	}
}

func TestParseMlnxQos(t *testing.T) {
	out := `DCBX mode: OS controlled
Priority trust state: dscp
dscp2prio mapping:
        prio:0 dscp:07,06,05,04,03,02,01,00,
        prio:3 dscp:26,
        prio:6 dscp:48,
default priority:
Receive buffer size (bytes): 0,156096,0,0,0,0,0,0,
Cable len: 7
PFC configuration:
        priority    0   1   2   3   4   5   6   7
        enabled     0   0   0   1   0   0   0   0
        buffer      0   0   0   1   0   0   0   0
tc: 1 ratelimit: unlimited, tsa: vendor
`
	m := ParseMlnxQos(out)
	if m.Trust != "dscp" || m.PFC != "3" {
		t.Fatalf("%+v", m)
	}
	// Newer mlnx_qos prints one dscp per line.
	m = ParseMlnxQos("Priority trust state: pcp\n        dscp26 prio:3\n        dscp48 prio:6\n        dscp00 prio:0\nPFC configuration:\n\tpriority    0   1   2   3   4   5   6   7\n\tenabled     0   0   0   0   0   0   0   0\n")
	if m.Trust != "pcp" || m.PFC != "none" || m.DSCPPrio != "26:3,48:6" {
		t.Fatalf("%+v", m)
	}
}

func TestSysfsSettings(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"class/infiniband/mlx5_0/ports/1/gid_attrs/types/0": "IB/RoCE v1\n",
		"class/infiniband/mlx5_0/ports/1/gid_attrs/types/1": "RoCE v2\n",
		"class/infiniband/mlx5_0/tc/1/traffic_class":        "106\n",
		"class/infiniband/mlx5_1/ports/1/gid_attrs/types/0": "IB/RoCE v1\n",
		"class/net/enp1s0f0np0/ecn/roce_np/enable/3":        "1\n",
		"class/net/enp1s0f0np0/ecn/roce_np/enable/0":        "0\n",
		"class/net/enp1s0f0np0/ecn/roce_rp/enable/3":        "1\n",
		"class/net/enp1s0f0np0/ecn/roce_rp/enable/4":        "1\n",
		"class/net/enp1s0f0np0/ecn/roce_np/cnp_dscp":        "48\n",
		"class/net/enp1s0f0np0/ecn/roce_np/cnp_802p_prio":   "6\n",
	})
	old := sysRoot
	sysRoot = root
	defer func() { sysRoot = old }()
	if m := roceMode("mlx5_0", 1); m != "v2" {
		t.Fatalf("roce mode %q", m)
	}
	if m := roceMode("mlx5_1", 1); m != "v1" {
		t.Fatalf("v1-only roce mode %q", m)
	}
	if m := roceMode("mlx5_9", 1); m != "" {
		t.Fatalf("missing device roce mode %q", m)
	}
	np, rp, dscp, prio, ok := sysfsECN("enp1s0f0np0")
	if !ok || np != "3" || rp != "3,4" || dscp != 48 || prio != 6 {
		t.Fatalf("ecn: np %q rp %q cnp dscp %d prio %d ok %v", np, rp, dscp, prio, ok)
	}
	if _, _, _, _, ok := sysfsECN("eth9"); ok {
		t.Fatal("a netdev without ecn knobs reported ECN")
	}
	s := readSettings(t.Context(), "mlx5_0", 1, "enp1s0f0np0")
	if s.RoCEMode != "v2" || s.TOS != 106 || s.ECNNp != "3" || s.CNPDSCP != 48 || !strings.Contains(s.Source, "sysfs") {
		t.Fatalf("%+v", s)
	}
}
