package agent

import (
	"bufio"
	"context"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Realishbird/SR-Net-Speed-Test/internal/api"
	"github.com/Realishbird/SR-Net-Speed-Test/internal/version"
)

// sysRoot lets tests point discovery at a fake sysfs tree.
var sysRoot = "/sys"

// Inventory collects host facts and every RDMA port on the host.
func Inventory(ctx context.Context) api.Inventory {
	inv := api.Inventory{
		Arch:         runtime.GOARCH,
		AgentVersion: version.Version,
		CPUs:         runtime.NumCPU(),
		Tools:        map[string]string{},
	}
	inv.Hostname, _ = os.Hostname()
	inv.OS = osPrettyName()
	inv.Kernel = strings.TrimSpace(readFile("/proc/sys/kernel/osrelease"))
	inv.CPUModel = cpuModel()
	inv.MemBytes = memTotal()
	inv.Ports = DiscoverPorts(ctx)
	addLLDP(ctx, inv.Ports)
	addSettings(ctx, inv.Ports)
	inv.Tools = toolVersions(ctx)
	return inv
}

// DiscoverPorts walks /sys/class/infiniband and maps each port to its netdev.
func DiscoverPorts(ctx context.Context) []api.Port {
	devs, _ := os.ReadDir(filepath.Join(sysRoot, "class/infiniband"))
	var ports []api.Port
	for _, d := range devs {
		dev := d.Name()
		devDir := filepath.Join(sysRoot, "class/infiniband", dev)
		fw := strings.TrimSpace(readFile(filepath.Join(devDir, "fw_ver")))
		numa := -1
		if n, err := strconv.Atoi(strings.TrimSpace(readFile(filepath.Join(devDir, "device/numa_node")))); err == nil {
			numa = n
		}
		pdirs, _ := os.ReadDir(filepath.Join(devDir, "ports"))
		for _, pd := range pdirs {
			num, err := strconv.Atoi(pd.Name())
			if err != nil {
				continue
			}
			pdir := filepath.Join(devDir, "ports", pd.Name())
			p := api.Port{
				RdmaDev:   dev,
				PortNum:   num,
				FWVersion: fw,
				NUMANode:  numa,
				LinkLayer: strings.TrimSpace(readFile(filepath.Join(pdir, "link_layer"))),
				State:     stateName(readFile(filepath.Join(pdir, "state"))),
				PhysState: stateName(readFile(filepath.Join(pdir, "phys_state"))),
			}
			p.Netdev = portNetdev(devDir, pdir)
			if p.Netdev != "" {
				fillNetdev(ctx, &p)
			}
			ports = append(ports, p)
		}
	}
	sort.Slice(ports, func(i, j int) bool {
		if ports[i].RdmaDev != ports[j].RdmaDev {
			return ports[i].RdmaDev < ports[j].RdmaDev
		}
		return ports[i].PortNum < ports[j].PortNum
	})
	return ports
}

// portNetdev finds the Ethernet netdev behind a RoCE port.
func portNetdev(devDir, pdir string) string {
	if nd := strings.TrimSpace(readFile(filepath.Join(pdir, "gid_attrs/ndevs/0"))); nd != "" {
		return nd
	}
	nets, _ := os.ReadDir(filepath.Join(devDir, "device/net"))
	if len(nets) == 1 {
		return nets[0].Name()
	}
	return ""
}

func fillNetdev(ctx context.Context, p *api.Port) {
	netDir := filepath.Join(sysRoot, "class/net", p.Netdev)
	p.SpeedMbps, _ = strconv.Atoi(strings.TrimSpace(readFile(filepath.Join(netDir, "speed"))))
	if p.SpeedMbps < 0 {
		p.SpeedMbps = 0
	}
	p.MTU, _ = strconv.Atoi(strings.TrimSpace(readFile(filepath.Join(netDir, "mtu"))))
	if link, err := os.Readlink(filepath.Join(netDir, "device/driver")); err == nil {
		p.Driver = filepath.Base(link)
	}
	if ifc, err := net.InterfaceByName(p.Netdev); err == nil {
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
				p.Addrs = append(p.Addrs, ipn.String())
			}
		}
	}
	p.Transceiver = transceiverPN(ctx, p.Netdev)
}

var pnRe = regexp.MustCompile(`(?m)^\s*Vendor (name|PN)\s*:\s*(.+?)\s*$`)

func transceiverPN(ctx context.Context, netdev string) string {
	out := ethtoolModule(ctx, netdev)
	var name, pn string
	for _, m := range pnRe.FindAllStringSubmatch(out, -1) {
		if m[1] == "name" {
			name = m[2]
		} else {
			pn = m[2]
		}
	}
	return strings.TrimSpace(name + " " + pn)
}

// stateName turns "4: ACTIVE" into "ACTIVE" and "5: LinkUp" into "LinkUp".
func stateName(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, ":"); i >= 0 {
		return strings.TrimSpace(s[i+1:])
	}
	return s
}

func readFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

func osPrettyName() string {
	f, err := os.Open("/etc/os-release")
	if err != nil {
		return runtime.GOOS
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "PRETTY_NAME="); ok {
			return strings.Trim(v, `"`)
		}
	}
	return runtime.GOOS
}

func cpuModel() string {
	for _, line := range strings.Split(readFile("/proc/cpuinfo"), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "model name", "Model":
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func memTotal() uint64 {
	for _, line := range strings.Split(readFile("/proc/meminfo"), "\n") {
		if v, ok := strings.CutPrefix(line, "MemTotal:"); ok {
			f := strings.Fields(v)
			if len(f) > 0 {
				kb, _ := strconv.ParseUint(f[0], 10, 64)
				return kb * 1024
			}
		}
	}
	return 0
}

func toolVersions(ctx context.Context) map[string]string {
	tools := map[string]string{}
	for _, t := range []string{api.ToolWriteBW, api.ToolReadBW, api.ToolSendBW, api.ToolWriteLat, api.ToolIperf3, "ethtool", "lldpctl", "python3", "nvidia-smi", "dcb", "mlnx_qos"} {
		if _, err := exec.LookPath(t); err == nil {
			tools[t] = "installed"
		}
	}
	// Package versions are the useful number for perftest; dpkg is always present on supported OSes.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "dpkg-query", "-W", "-f", "${Package} ${Version}\n", "perftest", "iperf3", "ethtool", "rdma-core", "lldpd").Output()
	if err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			if f := strings.Fields(line); len(f) == 2 {
				tools[f[0]] = f[1]
			}
		}
	}
	// PyTorch with CUDA is what the NCCL collective tests need.
	if _, ok := tools["python3"]; ok {
		out, err := exec.CommandContext(ctx, "python3", "-c", "import torch; print(torch.__version__, torch.cuda.is_available(), torch.cuda.nccl.version() if torch.cuda.is_available() else '')").Output()
		if err == nil {
			if f := strings.Fields(string(out)); len(f) >= 2 {
				tools["torch"] = f[0]
				tools["cuda"] = f[1]
				if len(f) > 2 {
					tools["nccl"] = strings.Trim(f[2], "()")
				}
			}
		}
	}
	return tools
}

// addLLDP fills each port's switch neighbour from lldpd, so the controller can
// match host ports to switch ports without anyone typing cable maps.
func addLLDP(ctx context.Context, ports []api.Port) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// lldpcli is setuid _lldpd and executable by group adm only, so install.sh
	// puts the service in that group. Older installs relied on a sudo rule,
	// which the unit's capability bounding set silently broke (sudo cannot
	// change its group without CAP_SETGID); keep it as a second try.
	out, err := exec.CommandContext(ctx, "lldpctl", "-f", "keyvalue").Output()
	if err != nil {
		var err2 error
		if out, err2 = exec.CommandContext(ctx, "sudo", "-n", "/usr/sbin/lldpctl", "-f", "keyvalue").Output(); err2 != nil {
			lldpWarn.Do(func() {
				log.Printf("lldp: cannot run lldpctl (%v; sudo: %v); re-run install.sh to put the agent in group adm", err, err2)
			})
			return
		}
	}
	lldpOK.Do(func() { log.Printf("lldp: neighbours read from lldpctl") })
	neigh := ParseLLDP(string(out))
	for i := range ports {
		if n, ok := neigh[ports[i].Netdev]; ok {
			ports[i].LLDPSwitch, ports[i].LLDPPort = n[0], n[1]
		}
	}
}

var lldpWarn, lldpOK sync.Once

// ParseLLDP reads `lldpctl -f keyvalue` into netdev -> {chassis name, port id}.
func ParseLLDP(out string) map[string][2]string {
	res := map[string][2]string{}
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok || !strings.HasPrefix(k, "lldp.") {
			continue
		}
		parts := strings.Split(k, ".")
		if len(parts) < 4 {
			continue
		}
		dev := parts[1]
		n := res[dev]
		switch strings.Join(parts[2:], ".") {
		case "chassis.name":
			n[0] = v
		case "port.ifname":
			n[1] = v
		case "port.descr":
			if n[1] == "" {
				n[1] = v
			}
		}
		res[dev] = n
	}
	for dev, n := range res {
		if n[0] == "" && n[1] == "" {
			delete(res, dev)
		}
	}
	return res
}
