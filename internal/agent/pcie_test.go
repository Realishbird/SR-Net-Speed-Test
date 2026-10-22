package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Realishbird/SR-Net-Speed-Test/internal/api"
)

// fakePCI builds a sysfs tree where mlx5_0 sits on 0000:01:00.0 behind a
// root port 0000:00:01.0, with a NIC temperature sensor.
func fakePCI(t *testing.T) string {
	root := t.TempDir()
	dev := "devices/pci0000:00/0000:00:01.0/0000:01:00.0"
	writeTree(t, root, map[string]string{
		dev + "/current_link_speed":                      "16.0 GT/s PCIe\n",
		dev + "/current_link_width":                      "4\n",
		dev + "/max_link_speed":                          "32.0 GT/s PCIe\n",
		dev + "/max_link_width":                          "16\n",
		dev + "/aer_dev_correctable":                     "RxErr 0\nBadTLP 2\nTOTAL_ERR_COR 2\n",
		dev + "/aer_dev_fatal":                           "TOTAL_ERR_FATAL 0\n",
		dev + "/hwmon/hwmon3/temp1_input":                "44000\n",
		dev + "/hwmon/hwmon3/temp1_label":                "asic\n",
		dev + "/hwmon/hwmon3/temp2_input":                "49000\n",
		dev + "/hwmon/hwmon3/temp2_label":                "module0\n",
		"devices/pci0000:00/0000:00:01.0/max_link_speed": "32.0 GT/s PCIe\n",
		"devices/pci0000:00/0000:00:01.0/max_link_width": "8\n",
		"class/infiniband/mlx5_0/ports/1/state":          "4: ACTIVE\n",
	})
	if err := os.Symlink(filepath.Join(root, dev), filepath.Join(root, "class/infiniband/mlx5_0/device")); err != nil {
		t.Fatal(err)
	}
	old := sysRoot
	sysRoot = root
	t.Cleanup(func() { sysRoot = old })
	return root
}

func TestReadPCIeAndNicTemp(t *testing.T) {
	fakePCI(t)
	p := api.Port{RdmaDev: "mlx5_0", PortNum: 1}
	l := ReadPCIe(p)
	if l == nil {
		t.Fatal("no PCIe link read")
	}
	if l.Gen != 4 || l.Width != 4 || l.MaxGen != 5 || l.MaxWidth != 16 || l.SlotGen != 5 || l.SlotWidth != 8 || l.Address != "0000:01:00.0" {
		t.Errorf("link = %+v", *l)
	}
	if !l.Downtrained() || l.String() != "Gen4 x4" {
		t.Errorf("downtrained=%v string=%q", l.Downtrained(), l.String())
	}
	if l.UsableGbps < 50 || l.UsableGbps > 60 { // Gen4 x4: 16 GT/s * 4 * 128/130 * 0.85 = 53.6
		t.Errorf("usable = %v", l.UsableGbps)
	}
	v := map[string]float64{}
	pcieValues(p, v)
	if v["pcie_link_gen"] != 4 || v["pcie_link_width"] != 4 || v["pcie_aer_correctable"] != 2 || v["pcie_aer_fatal"] != 0 {
		t.Errorf("values = %v", v)
	}
	if _, ok := v["pcie_aer_nonfatal"]; ok {
		t.Error("missing aer file must not produce a value")
	}
	c, ok := nicTemp(p)
	if !ok || c != 44 {
		t.Errorf("nic temp = %v %v (module sensor must be skipped)", c, ok)
	}
	if ReadPCIe(api.Port{RdmaDev: "none"}) != nil {
		t.Error("unknown device must give nil")
	}
	ports := DiscoverPorts(context.Background())
	if len(ports) != 1 || ports[0].PCIe == nil || ports[0].PCIe.Gen != 4 {
		t.Errorf("discovery did not attach PCIe: %+v", ports)
	}
}

func TestPCIeGen(t *testing.T) {
	for in, want := range map[string]int{"2.5 GT/s PCIe": 1, "5.0 GT/s PCIe": 2, "8.0 GT/s PCIe": 3, "16.0 GT/s PCIe": 4, "32.0 GT/s PCIe": 5, "64.0 GT/s PCIe": 6, "Unknown": 0, "": 0} {
		if got := pcieGen(in); got != want {
			t.Errorf("pcieGen(%q) = %d, want %d", in, got, want)
		}
	}
}
