package controller

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Realishbird/SR-Net-Speed-Test/internal/api"
)

// Switch health and port info.
//
// Enterprise SONiC keys PSUs, fans and temperature sensors by names it
// invents ("PSU 1", "FAN 3", "TEMP 2") and refuses a wildcard component
// list, so the controller finds the valid names once by asking for each
// candidate, then polls them every minute with one Get. The readings go
// to VictoriaMetrics as srnet_switch_* series (for the Switch dashboard
// and the raw export) and are kept for the Switches page and reports.
//
// pushPortInfo writes srnet_switch_port_info{switch, port, vlan, host,
// host_port, fabric} = 1 every minute from the LLDP and VLAN tables, so a
// dashboard legend can say "Ethernet0 · Dell Spark 1 P1-B1-A".

type switchHealth struct {
	mu      sync.Mutex
	names   map[string][]string // switch -> valid component names
	namedAt map[string]time.Time
	last    map[string]api.SwitchHealth
}

func newSwitchHealth() *switchHealth {
	return &switchHealth{names: map[string][]string{}, namedAt: map[string]time.Time{}, last: map[string]api.SwitchHealth{}}
}

// candidates are the component names probed once per switch (and again
// every 6 hours, in case a PSU or fan was added).
func healthCandidates() []string {
	var out []string
	for i := 1; i <= 4; i++ {
		out = append(out, fmt.Sprintf("PSU %d", i))
	}
	for i := 1; i <= 16; i++ {
		out = append(out, fmt.Sprintf("FAN %d", i))
	}
	for i := 1; i <= 24; i++ {
		out = append(out, fmt.Sprintf("TEMP %d", i))
	}
	return out
}

func componentPath(name string) string {
	return fmt.Sprintf("/openconfig-platform:components/component[name=%s]/state", name)
}

// pollSwitchHealth refreshes every gNMI switch's health, off the request path.
func (s *Server) pollSwitchHealth(ctx context.Context) {
	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()
	bySource := gnmiSources(cfg)
	if len(bySource) == 0 || s.health == nil {
		return
	}
	seen := map[string]bool{}
	for _, sw := range bySource {
		if seen[sw.Name] {
			continue
		}
		seen[sw.Name] = true
		h, err := s.switchHealthOf(ctx, cfg, bySource, sw)
		if err != nil {
			log.Printf("switch %s health: %v", sw.Name, err)
			continue
		}
		s.health.mu.Lock()
		s.health.last[sw.Name] = h
		s.health.mu.Unlock()
		s.metrics.push(healthLines(sw.Name, h))
	}
}

// switchHealthOf finds the switch's component names when needed and reads
// them plus the infra node (uptime, last reboot cause).
func (s *Server) switchHealthOf(ctx context.Context, cfg Config, bySource map[string]SwitchConfig, sw SwitchConfig) (api.SwitchHealth, error) {
	s.health.mu.Lock()
	names, at := s.health.names[sw.Name], s.health.namedAt[sw.Name]
	s.health.mu.Unlock()
	if names == nil || time.Since(at) > 6*time.Hour {
		names = nil
		for _, c := range healthCandidates() {
			raw, err := gnmicGet(ctx, cfg, componentPath(c))
			if err != nil {
				if strings.Contains(err.Error(), "Invalid component") || strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "NotFound") {
					continue
				}
				return api.SwitchHealth{}, err
			}
			found := false
			for source, vals := range gnmicUpdates(raw) {
				if _, ok := gnmiSource(bySource, source); ok && len(vals) > 0 {
					found = true
				}
			}
			if found {
				names = append(names, c)
			}
		}
		s.health.mu.Lock()
		s.health.names[sw.Name], s.health.namedAt[sw.Name] = names, time.Now()
		s.health.mu.Unlock()
		log.Printf("switch %s: %d health components (%s)", sw.Name, len(names), strings.Join(names, ", "))
	}
	h := api.SwitchHealth{At: time.Now(), Temps: []api.SwitchTemp{}, Fans: []api.SwitchPart{}, PSUs: []api.SwitchPart{}}
	paths := []string{"/openconfig-system:system/openconfig-system-ext:infra", "/openconfig-system:system/cpus", "/openconfig-system:system/memory"}
	for _, n := range names {
		paths = append(paths, componentPath(n))
	}
	raw, err := gnmicGet(ctx, cfg, paths[0], paths[1:]...)
	if err != nil {
		return h, err
	}
	for source, vals := range gnmicUpdates(raw) {
		if got, ok := gnmiSource(bySource, source); !ok || !strings.EqualFold(got.Name, sw.Name) {
			continue
		}
		for _, v := range vals {
			parseHealth(v, &h)
		}
	}
	sort.Slice(h.Temps, func(i, j int) bool { return h.Temps[i].Name < h.Temps[j].Name })
	sort.Slice(h.Fans, func(i, j int) bool { return h.Fans[i].Name < h.Fans[j].Name })
	sort.Slice(h.PSUs, func(i, j int) bool { return h.PSUs[i].Name < h.PSUs[j].Name })
	return h, nil
}

// parseHealth picks the health leaves out of one gnmic update value.
// Components look like {"name":"TEMP 1","state":{"temperature":{"current":45,
// "high":70}}} or with the readings directly under state; CPUs like
// {"index":"ALL","state":{"total":{"instant":4}}}; memory like
// {"state":{"physical":N,"used":N}}; infra like {"state":{"uptime":...}}.
func parseHealth(v map[string]any, h *api.SwitchHealth) {
	walkObjects(v, func(m map[string]any) {
		st, _ := m["state"].(map[string]any)
		if st == nil {
			st = m
		}
		f := flatten(m)
		name, _ := m["name"].(string)
		if name == "" {
			name, _ = st["name"].(string)
		}
		switch {
		// A component's name sits on the object and again on its state
		// object, which walkObjects also visits; count each name once.
		case strings.HasPrefix(name, "TEMP") && !hasTemp(h.Temps, name):
			tf := f
			if tm, ok := st["temperature"].(map[string]any); ok {
				tf = flatten(tm)
			}
			if c, ok := firstFloat(tf, "current", "instant"); ok {
				t := api.SwitchTemp{Name: name, C: c}
				t.HighC, _ = firstFloat(tf, "high", "high-threshold", "max")
				t.CriticalC, _ = firstFloat(tf, "critical-high")
				h.Temps = append(h.Temps, t)
			}
		case strings.HasPrefix(name, "FAN") && hasAny(f, "oper-status") && !hasPart(h.Fans, name):
			h.Fans = append(h.Fans, api.SwitchPart{Name: name, OK: operOK(f), Status: stringOf(f, "oper-status")})
		case strings.HasPrefix(name, "PSU") && hasAny(f, "oper-status") && !hasPart(h.PSUs, name):
			h.PSUs = append(h.PSUs, api.SwitchPart{Name: name, OK: operOK(f), Status: stringOf(f, "oper-status")})
		}
		if _, isCPU := m["index"]; isCPU {
			if tot, ok := st["total"].(map[string]any); ok {
				if c, ok := firstFloat(flatten(tot), "instant", "avg"); ok && (m["index"] == "ALL" || h.CPUPct == 0) {
					h.CPUPct = c
				}
			}
		}
		if phys, ok := firstFloat(f, "physical"); ok && phys > 0 {
			used := 0.0
			if u, ok := firstFloat(f, "used"); ok {
				used = u
			} else if fr, ok := firstFloat(f, "free", "unused"); ok {
				used = phys - fr
			}
			if used > 0 {
				h.MemPct = used / phys * 100
			}
		}
		if h.UptimeSec == 0 {
			if u, ok := f["uptime"]; ok {
				h.UptimeSec = uptimeSeconds(u)
			}
		}
		if r := stringOf(f, "reboot-cause", "last-reboot-reason", "reboot-reason"); r != "" && h.LastReboot == "" {
			h.LastReboot = r
		}
	})
}

// uptimeSeconds reads an uptime leaf: seconds, nanoseconds (large), or a
// text like "3 days, 04:05:06".
func uptimeSeconds(v any) float64 {
	if n, ok := anyFloat(v); ok {
		if n > 1e12 { // nanoseconds
			return n / 1e9
		}
		return n
	}
	s, _ := v.(string)
	days, hh, mm, ss := 0, 0, 0, 0
	if i := strings.Index(s, "day"); i >= 0 {
		fmt.Sscanf(strings.TrimSpace(s[:i]), "%d", &days)
		if j := strings.Index(s, ","); j >= 0 {
			s = s[j+1:]
		}
	}
	fmt.Sscanf(strings.TrimSpace(s), "%d:%d:%d", &hh, &mm, &ss)
	return float64(days*86400 + hh*3600 + mm*60 + ss)
}

func hasAny(f map[string]any, keys ...string) bool {
	for _, k := range keys {
		if _, ok := f[k]; ok {
			return true
		}
	}
	return false
}

func firstFloat(f map[string]any, keys ...string) (float64, bool) {
	for _, k := range keys {
		if v, ok := f[k]; ok {
			if n, ok := anyFloat(v); ok {
				return n, true
			}
		}
	}
	return 0, false
}

func stringOf(f map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := f[k].(string); ok {
			return v
		}
	}
	return ""
}

func operOK(f map[string]any) bool {
	st := strings.ToUpper(stringOf(f, "oper-status"))
	if strings.Contains(st, "INACTIVE") || strings.Contains(st, "DISABLED") || strings.Contains(st, "FAIL") {
		return false
	}
	return strings.Contains(st, "ACTIVE") || st == "UP" || strings.HasSuffix(st, "_OK")
}

func hasPart(parts []api.SwitchPart, name string) bool {
	for _, p := range parts {
		if p.Name == name {
			return true
		}
	}
	return false
}

func hasTemp(temps []api.SwitchTemp, name string) bool {
	for _, t := range temps {
		if t.Name == name {
			return true
		}
	}
	return false
}

// healthLines renders a health reading as Prometheus lines.
func healthLines(sw string, h api.SwitchHealth) []byte {
	var b bytes.Buffer
	ts := h.At.UnixMilli()
	base := map[string]string{"switch": sw}
	if h.CPUPct > 0 {
		fmt.Fprintf(&b, "srnet_switch_cpu_pct%s %g %d\n", formatLabels(base), h.CPUPct, ts)
	}
	if h.MemPct > 0 {
		fmt.Fprintf(&b, "srnet_switch_mem_used_pct%s %g %d\n", formatLabels(base), h.MemPct, ts)
	}
	if h.UptimeSec > 0 {
		fmt.Fprintf(&b, "srnet_switch_uptime_seconds%s %g %d\n", formatLabels(base), h.UptimeSec, ts)
	}
	for _, t := range h.Temps {
		l := formatLabels(copyWith(base, "sensor", t.Name))
		fmt.Fprintf(&b, "srnet_switch_temp_c%s %g %d\n", l, t.C, ts)
		if t.HighC > 0 {
			fmt.Fprintf(&b, "srnet_switch_temp_high_c%s %g %d\n", l, t.HighC, ts)
		}
	}
	for _, p := range h.Fans {
		fmt.Fprintf(&b, "srnet_switch_fan_ok%s %d %d\n", formatLabels(copyWith(base, "fan", p.Name)), b2i(p.OK), ts)
	}
	for _, p := range h.PSUs {
		fmt.Fprintf(&b, "srnet_switch_psu_ok%s %d %d\n", formatLabels(copyWith(base, "psu", p.Name)), b2i(p.OK), ts)
	}
	return b.Bytes()
}

func b2i(v bool) int {
	if v {
		return 1
	}
	return 0
}

// pushPortInfo writes the port-to-host map as an info series.
func (s *Server) pushPortInfo() {
	s.mu.Lock()
	lldp, vlans := s.lldp, s.vlans
	s.mu.Unlock()
	if len(lldp) == 0 && len(vlans) == 0 {
		return
	}
	switches := s.switchList()
	hosts, err := s.store.Hosts(context.Background())
	if err != nil {
		return
	}
	var b bytes.Buffer
	ts := time.Now().UnixMilli()
	seen := map[string]bool{}
	for _, h := range hosts {
		for _, p := range h.Ports {
			e, ok := lldp[strings.ToLower(h.Hostname)+"|"+strings.ToLower(p.Netdev)]
			if !ok || len(p.Addrs) == 0 {
				continue
			}
			sw := resolveSwitchName(switches, e.Switch)
			if sw == "" {
				sw = e.Switch
			}
			key := strings.ToLower(sw + "|" + e.Port)
			if seen[key] {
				continue
			}
			seen[key] = true
			label := p.Label
			if label == "" {
				label = p.Netdev
			}
			fmt.Fprintf(&b, "srnet_switch_port_info%s 1 %d\n", formatLabels(map[string]string{"switch": sw, "port": e.Port, "vlan": vlans[key], "host": h.Name, "host_port": label, "fabric": p.Fabric}), ts)
		}
	}
	s.metrics.push(b.Bytes())
}
