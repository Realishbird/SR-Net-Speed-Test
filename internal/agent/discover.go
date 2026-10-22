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
	addLLDP(ctx, inv.Ports, inv.Hostname)
	addSettings(ctx, inv.Ports)
	inv.Tools = toolVersions(ctx)
	addNetplan(&inv)
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
			p.PCIe = ReadPCIe(p)
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
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLinkLocalUnicast() {
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
func addLLDP(ctx context.Context, ports []api.Port, hostname string) {
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
	local := map[string]bool{}
	if ifs, err := net.Interfaces(); err == nil {
		for _, ifc := range ifs {
			local[strings.ToLower(ifc.Name)] = true
		}
	}
	for _, p := range ports {
		local[strings.ToLower(p.Netdev)] = true
	}
	neigh := ParseLLDP(string(out), hostname, local)
	for i := range ports {
		if n, ok := neigh[ports[i].Netdev]; ok {
			ports[i].LLDPSwitch, ports[i].LLDPPort, ports[i].LLDPDescr = n.Chassis, n.Port, n.Descr
		}
	}
}

var lldpWarn, lldpOK sync.Once

// LLDPNeighbour is the switch lldpd heard on one interface.
type LLDPNeighbour struct {
	Chassis string // system name
	Port    string // port id when it is a name (ifname or local subtype), else the description
	Descr   string // port description, when Port is the id
}

// ParseLLDP reads `lldpctl -f keyvalue` into netdev -> the neighbour on it.
// lldpctl prints every neighbour of an interface under the same key
// prefix, each one starting with its "via" line, so the lines are split
// into neighbours before any field is read. A neighbour that is this host
// (a ConnectX-7 on a Spark exposes each cable as two PCIe functions that
// hear each other) or that names one of the host's own interfaces is not
// a switch and is skipped. The port is the id when the switch sends a
// name, else the description: SONiC sends its port alias as a local id and
// whatever description an operator typed, so the controller resolves both
// against the switch's port table.
func ParseLLDP(out, self string, local map[string]bool) map[string]LLDPNeighbour {
	blocks := map[string][]map[string]string{}
	var order []string
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimRight(line, "\r"), "=")
		if !ok || !strings.HasPrefix(k, "lldp.") {
			continue
		}
		parts := strings.Split(k, ".")
		if len(parts) < 3 {
			continue
		}
		dev, field := parts[1], strings.Join(parts[2:], ".")
		bs := blocks[dev]
		if bs == nil {
			order = append(order, dev)
		}
		if _, again := lastBlock(bs)[field]; len(bs) == 0 || field == "via" || field == "rid" && again {
			bs = append(bs, map[string]string{})
		}
		bs[len(bs)-1][field] = v
		blocks[dev] = bs
	}
	res := map[string]LLDPNeighbour{}
	for _, dev := range order {
		for _, b := range blocks[dev] {
			n := LLDPNeighbour{Chassis: b["chassis.name"], Descr: b["port.descr"]}
			n.Port = b["port.ifname"]
			if n.Port == "" {
				n.Port = b["port.local"]
			}
			if n.Port == "" {
				n.Port, n.Descr = n.Descr, ""
			}
			if n.Chassis == "" && n.Port == "" {
				continue
			}
			if self != "" && strings.EqualFold(n.Chassis, self) || local[strings.ToLower(n.Port)] || local[strings.ToLower(n.Descr)] {
				continue
			}
			res[dev] = n
			break
		}
	}
	return res
}

func lastBlock(bs []map[string]string) map[string]string {
	if len(bs) == 0 {
		return nil
	}
	return bs[len(bs)-1]
}
