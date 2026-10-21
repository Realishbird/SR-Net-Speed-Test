package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for p, v := range files {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDiscoverPortsAndCounters(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"class/infiniband/mlx5_0/fw_ver":                              "28.39.1002\n",
		"class/infiniband/mlx5_0/device/numa_node":                    "1\n",
		"class/infiniband/mlx5_0/ports/1/state":                       "4: ACTIVE\n",
		"class/infiniband/mlx5_0/ports/1/phys_state":                  "5: LinkUp\n",
		"class/infiniband/mlx5_0/ports/1/link_layer":                  "Ethernet\n",
		"class/infiniband/mlx5_0/ports/1/gid_attrs/ndevs/0":           "enp1s0f0np0\n",
		"class/infiniband/mlx5_0/ports/1/counters/port_xmit_data":     "1000\n",
		"class/infiniband/mlx5_0/ports/1/counters/port_rcv_data":      "2000\n",
		"class/infiniband/mlx5_0/ports/1/hw_counters/out_of_sequence": "3\n",
		"class/infiniband/mlx5_0/ports/1/hw_counters/np_cnp_sent":     "9\n",
		"class/infiniband/mlx5_1/ports/1/state":                       "1: DOWN\n",
		"class/infiniband/mlx5_1/ports/1/phys_state":                  "3: Disabled\n",
		"class/infiniband/mlx5_1/ports/1/link_layer":                  "Ethernet\n",
		"class/net/enp1s0f0np0/speed":                                 "200000\n",
		"class/net/enp1s0f0np0/mtu":                                   "9000\n",
	})
	old := sysRoot
	sysRoot = root
	t.Cleanup(func() { sysRoot = old })

	ports := DiscoverPorts(context.Background())
	if len(ports) != 2 {
		t.Fatalf("got %d ports: %+v", len(ports), ports)
	}
	p := ports[0]
	if p.RdmaDev != "mlx5_0" || p.PortNum != 1 || p.Netdev != "enp1s0f0np0" || p.State != "ACTIVE" || p.PhysState != "LinkUp" {
		t.Errorf("port 0 = %+v", p)
	}
	if p.SpeedMbps != 200000 || p.MTU != 9000 || p.FWVersion != "28.39.1002" || p.NUMANode != 1 {
		t.Errorf("port 0 details = %+v", p)
	}
	if ports[1].State != "DOWN" || ports[1].Netdev != "" {
		t.Errorf("port 1 = %+v", ports[1])
	}

	hw := HWCounters("mlx5_0", 1)
	if hw["out_of_sequence"] != 3 || hw["np_cnp_sent"] != 9 {
		t.Errorf("hw counters = %v", hw)
	}

	s := NewSampler()
	s.SetPorts(ports)
	first := s.Collect(context.Background())
	if len(first) != 3 { // two ports plus the host
		t.Fatalf("got %d samples", len(first))
	}
	if first[0].Values["rdma_tx_bytes"] != 4000 || first[0].Values["rdma_rx_bytes"] != 8000 {
		t.Errorf("4-octet counters not scaled: %v", first[0].Values)
	}
	if _, ok := first[0].Values["rdma_tx_gbps"]; ok {
		t.Error("first sample must not carry a rate")
	}
	writeTree(t, root, map[string]string{"class/infiniband/mlx5_0/ports/1/counters/port_xmit_data": "251000\n"})
	second := s.Collect(context.Background())
	if g := second[0].Values["rdma_tx_gbps"]; g <= 0 {
		t.Errorf("expected a tx rate, got %v", g)
	}
	if second[0].Values["rdma_port_active"] != 1 || second[1].Values["rdma_port_active"] != 0 {
		t.Error("port active flags wrong")
	}
}
