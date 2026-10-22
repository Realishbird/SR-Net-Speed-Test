package agent

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Realishbird/SR-Net-Speed-Test/internal/api"
)

// PCIe link of a NIC function, from sysfs (readable without root):
// current_link_speed and current_link_width are what the link trained to,
// max_link_speed and max_link_width what the card supports, and the
// upstream port's maximum what the slot offers. A link below the card's
// maximum is a downtrain (reseat, a bent pin, a bad riser) or a slot that
// is narrower than the card; the controller tells the two apart.

// pcieGen maps a sysfs link speed ("32.0 GT/s PCIe") to a generation.
func pcieGen(s string) int {
	f := strings.Fields(s)
	if len(f) == 0 {
		return 0
	}
	gt, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return 0
	}
	switch {
	case gt >= 64:
		return 6
	case gt >= 32:
		return 5
	case gt >= 16:
		return 4
	case gt >= 8:
		return 3
	case gt >= 5:
		return 2
	case gt >= 2.5:
		return 1
	}
	return 0
}

// pcieDevice returns the sysfs directory of the PCI function behind a port.
func pcieDevice(p api.Port) string {
	cands := []string{filepath.Join(sysRoot, "class/infiniband", p.RdmaDev, "device")}
	if p.Netdev != "" {
		cands = append(cands, filepath.Join(sysRoot, "class/net", p.Netdev, "device"))
	}
	for _, c := range cands {
		if real, err := filepath.EvalSymlinks(c); err == nil {
			if _, err := os.Stat(filepath.Join(real, "current_link_speed")); err == nil {
				return real
			}
		}
	}
	return ""
}

// ReadPCIe fills a port's PCIe link details; nil when sysfs has none (a
// virtual function, or a platform without PCIe link attributes).
func ReadPCIe(p api.Port) *api.PCIeLink {
	dev := pcieDevice(p)
	if dev == "" {
		return nil
	}
	num := func(path string) int {
		n, _ := strconv.Atoi(strings.TrimSpace(readFile(path)))
		return n
	}
	l := &api.PCIeLink{
		Address:  filepath.Base(dev),
		Gen:      pcieGen(readFile(filepath.Join(dev, "current_link_speed"))),
		Width:    num(filepath.Join(dev, "current_link_width")),
		MaxGen:   pcieGen(readFile(filepath.Join(dev, "max_link_speed"))),
		MaxWidth: num(filepath.Join(dev, "max_link_width")),
	}
	if l.Gen == 0 && l.Width == 0 {
		return nil
	}
	// The upstream port (root port or switch downstream port) is the slot.
	if up, err := filepath.EvalSymlinks(filepath.Join(dev, "..")); err == nil {
		if _, err := os.Stat(filepath.Join(up, "max_link_speed")); err == nil {
			l.SlotGen = pcieGen(readFile(filepath.Join(up, "max_link_speed")))
			l.SlotWidth = num(filepath.Join(up, "max_link_width"))
		}
	}
	l.UsableGbps = api.PCIeUsableGbps(l.Gen, l.Width)
	return l
}

// pcieValues samples the link and the AER error counters, so a link that
// trains down or starts logging errors during a run is seen.
func pcieValues(p api.Port, v map[string]float64) {
	dev := pcieDevice(p)
	if dev == "" {
		return
	}
	if g := pcieGen(readFile(filepath.Join(dev, "current_link_speed"))); g > 0 {
		v["pcie_link_gen"] = float64(g)
	}
	if n, ok := readNum(filepath.Join(dev, "current_link_width")); ok {
		v["pcie_link_width"] = n
	}
	for _, kind := range []string{"correctable", "nonfatal", "fatal"} {
		if n, ok := aerTotal(filepath.Join(dev, "aer_dev_"+kind)); ok {
			v["pcie_aer_"+kind] = n
		}
	}
}

// aerTotal reads the TOTAL_ERR_* line of an aer_dev_* file.
func aerTotal(path string) (float64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "TOTAL_ERR_") {
			f := strings.Fields(line)
			if len(f) == 2 {
				n, err := strconv.ParseFloat(f[1], 64)
				return n, err == nil
			}
		}
	}
	return 0, false
}
