package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Realishbird/SR-Net-Speed-Test/internal/api"
)

// Sampler reads RDMA, NIC, transceiver and host telemetry on a timer.
// It runs on its own goroutine so a hung benchmark never stops metrics.
type Sampler struct {
	FastInterval time.Duration // while any test is active
	IdleInterval time.Duration
	DOMInterval  time.Duration

	mu      sync.Mutex
	ports   []api.Port
	runs    map[int64]int // run id -> active task count
	prev    map[string]portCounters
	prevCPU cpuTimes
	lastDOM time.Time
}

type portCounters struct {
	t      time.Time
	tx, rx float64 // bytes
}

func NewSampler() *Sampler {
	return &Sampler{
		FastInterval: time.Second,
		IdleInterval: 15 * time.Second,
		DOMInterval:  5 * time.Second,
		runs:         map[int64]int{},
		prev:         map[string]portCounters{},
	}
}

func (s *Sampler) SetPorts(p []api.Port) {
	s.mu.Lock()
	s.ports = p
	s.mu.Unlock()
}

// RunStarted and RunEnded tag samples with active run ids and switch to fast sampling.
func (s *Sampler) RunStarted(id int64) {
	s.mu.Lock()
	s.runs[id]++
	s.mu.Unlock()
}

func (s *Sampler) RunEnded(id int64) {
	s.mu.Lock()
	if s.runs[id]--; s.runs[id] <= 0 {
		delete(s.runs, id)
	}
	s.mu.Unlock()
}

func (s *Sampler) state() ([]api.Port, []int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []int64
	for id := range s.runs {
		ids = append(ids, id)
	}
	return append([]api.Port(nil), s.ports...), ids
}

// Loop samples until ctx ends, handing each batch to push.
func (s *Sampler) Loop(ctx context.Context, push func([]api.Sample)) {
	next := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Until(next)):
		}
		_, runs := s.state()
		interval := s.IdleInterval
		if len(runs) > 0 {
			interval = s.FastInterval
		}
		next = time.Now().Add(interval)
		if batch := s.Collect(ctx); len(batch) > 0 {
			push(batch)
		}
	}
}

// Collect takes one reading of every port plus the host.
func (s *Sampler) Collect(ctx context.Context) []api.Sample {
	ports, runs := s.state()
	now := time.Now()
	doDOM := now.Sub(s.lastDOM) >= s.DOMInterval
	if doDOM {
		s.lastDOM = now
	}
	var out []api.Sample
	for _, p := range ports {
		v := s.portValues(ctx, p, now, doDOM)
		out = append(out, api.Sample{Time: now, RdmaDev: p.RdmaDev, PortNum: p.PortNum, Netdev: p.Netdev, RunIDs: runs, Values: v})
	}
	out = append(out, api.Sample{Time: now, RunIDs: runs, Values: s.hostValues()})
	return out
}

func (s *Sampler) portValues(ctx context.Context, p api.Port, now time.Time, doDOM bool) map[string]float64 {
	v := map[string]float64{}
	pdir := filepath.Join(sysRoot, "class/infiniband", p.RdmaDev, "ports", strconv.Itoa(p.PortNum))

	// port_xmit_data / port_rcv_data count 4-octet words.
	tx, txOK := readNum(filepath.Join(pdir, "counters/port_xmit_data"))
	rx, rxOK := readNum(filepath.Join(pdir, "counters/port_rcv_data"))
	if txOK && rxOK {
		tx, rx = tx*4, rx*4
		v["rdma_tx_bytes"], v["rdma_rx_bytes"] = tx, rx
		key := p.RdmaDev + "/" + strconv.Itoa(p.PortNum)
		s.mu.Lock()
		prev, ok := s.prev[key]
		s.prev[key] = portCounters{t: now, tx: tx, rx: rx}
		s.mu.Unlock()
		if ok {
			if dt := now.Sub(prev.t).Seconds(); dt > 0 && tx >= prev.tx && rx >= prev.rx {
				v["rdma_tx_gbps"] = (tx - prev.tx) * 8 / dt / 1e9
				v["rdma_rx_gbps"] = (rx - prev.rx) * 8 / dt / 1e9
			}
		}
	}
	for _, name := range []string{"port_xmit_packets", "port_rcv_packets", "port_rcv_errors", "port_xmit_discards", "symbol_error", "link_downed"} {
		if n, ok := readNum(filepath.Join(pdir, "counters", name)); ok {
			v["rdma_"+name] = n
		}
	}
	for k, n := range HWCounters(p.RdmaDev, p.PortNum) {
		v["rdma_hw_"+k] = n
	}
	if active := stateName(readFile(filepath.Join(pdir, "state"))) == "ACTIVE"; active {
		v["rdma_port_active"] = 1
	} else {
		v["rdma_port_active"] = 0
	}

	if p.Netdev != "" {
		netDir := filepath.Join(sysRoot, "class/net", p.Netdev)
		if n, ok := readNum(filepath.Join(netDir, "speed")); ok && n >= 0 {
			v["link_speed_mbps"] = n
		}
		if n, ok := readNum(filepath.Join(netDir, "carrier_changes")); ok {
			v["link_carrier_changes"] = n
		}
		// Netdev byte counters carry TCP/UDP traffic; RDMA bypasses them.
		if n, ok := readNum(filepath.Join(netDir, "statistics/tx_bytes")); ok {
			v["nic_tx_bytes"] = n
		}
		if n, ok := readNum(filepath.Join(netDir, "statistics/rx_bytes")); ok {
			v["nic_rx_bytes"] = n
		}
		for k, n := range ethtoolStats(ctx, p.Netdev) {
			v["nic_"+k] = n
		}
		if doDOM {
			d := ParseModuleDOM(ethtoolModule(ctx, p.Netdev))
			setPtr(v, "dom_temp_c", d.TempC)
			setPtr(v, "dom_voltage_v", d.VoltageV)
			if d.PowerW != nil {
				if d.PowerRated {
					v["dom_power_rated_max_w"] = *d.PowerW
				} else {
					v["dom_power_w"] = *d.PowerW
				}
			}
			for i, b := range d.TxBiasMa {
				v[fmt.Sprintf("dom_tx_bias_ma.ch%d", i+1)] = b
			}
			if d.VoltageV != nil && len(d.TxBiasMa) > 0 {
				// Same estimate the PowerShell tool reported: voltage x channel-1 bias.
				v["dom_bias_estimate_w"] = *d.VoltageV * d.TxBiasMa[0] / 1000
			}
			for i, p := range d.TxPowerMw {
				v[fmt.Sprintf("dom_tx_power_mw.ch%d", i+1)] = p
			}
			for i, p := range d.RxPowerMw {
				v[fmt.Sprintf("dom_rx_power_mw.ch%d", i+1)] = p
			}
		}
	}
	return v
}

func setPtr(v map[string]float64, k string, p *float64) {
	if p != nil {
		v[k] = *p
	}
}

// HWCounters reads every numeric file under hw_counters (congestion, retransmit, sequence errors).
func HWCounters(dev string, port int) map[string]float64 {
	dir := filepath.Join(sysRoot, "class/infiniband", dev, "ports", strconv.Itoa(port), "hw_counters")
	ents, _ := os.ReadDir(dir)
	out := map[string]float64{}
	for _, e := range ents {
		if n, ok := readNum(filepath.Join(dir, e.Name())); ok {
			out[e.Name()] = n
		}
	}
	return out
}

func ethtoolStats(ctx context.Context, netdev string) map[string]float64 {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ethtool", "-S", netdev).Output()
	if err != nil {
		return nil
	}
	return ParseEthtoolStats(string(out))
}

type cpuTimes struct{ idle, total float64 }

func (s *Sampler) hostValues() map[string]float64 {
	v := map[string]float64{}
	if f := strings.Fields(firstLine(readFile("/proc/stat"))); len(f) > 5 && f[0] == "cpu" {
		var cur cpuTimes
		for i, x := range f[1:] {
			n, _ := strconv.ParseFloat(x, 64)
			cur.total += n
			if i == 3 || i == 4 { // idle, iowait
				cur.idle += n
			}
		}
		if s.prevCPU.total > 0 && cur.total > s.prevCPU.total {
			v["cpu_util_pct"] = 100 * (1 - (cur.idle-s.prevCPU.idle)/(cur.total-s.prevCPU.total))
		}
		s.prevCPU = cur
	}
	mem := map[string]float64{}
	for _, line := range strings.Split(readFile("/proc/meminfo"), "\n") {
		k, rest, ok := strings.Cut(line, ":")
		if f := strings.Fields(rest); ok && len(f) > 0 {
			n, _ := strconv.ParseFloat(f[0], 64)
			mem[k] = n * 1024
		}
	}
	if mem["MemTotal"] > 0 {
		v["mem_total_bytes"] = mem["MemTotal"]
		v["mem_used_bytes"] = mem["MemTotal"] - mem["MemAvailable"]
	}
	if f := strings.Fields(readFile("/proc/loadavg")); len(f) > 0 {
		v["load1"], _ = strconv.ParseFloat(f[0], 64)
	}
	// hwmon covers CPU package, NIC ASIC (mlx5 exposes one on recent kernels) and board sensors.
	chips, _ := filepath.Glob(filepath.Join(sysRoot, "class/hwmon/hwmon*"))
	for _, chip := range chips {
		name := strings.TrimSpace(readFile(filepath.Join(chip, "name")))
		inputs, _ := filepath.Glob(filepath.Join(chip, "temp*_input"))
		for _, in := range inputs {
			n, ok := readNum(in)
			if !ok {
				continue
			}
			base := strings.TrimSuffix(filepath.Base(in), "_input")
			label := strings.TrimSpace(readFile(filepath.Join(chip, base+"_label")))
			if label == "" {
				label = base
			}
			v["hwmon_temp_c."+sanitize(filepath.Base(chip)+"_"+name+"_"+label)] = n / 1000
		}
	}
	return v
}

func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			return r
		}
		return '_'
	}, s)
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

func readNum(path string) (float64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
	return n, err == nil
}
