package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Realishbird/SR-Net-Speed-Test/internal/api"
)

// Switch health and port info.
//
// Enterprise SONiC keys PSUs, fans and temperature sensors by names it
// invents ("PSU 1", "FAN 3", "TEMP 2") and refuses a wildcard component
// list, so the controller finds the valid keys once by asking for each
// candidate, then polls them every minute with one Get. The answer names
// the hardware rather than the key ("TEMP 1" answers as "CPU Temp", "FAN 1"
// as "FanTray1-Fan1"), so each reading is classified by the key in its
// update path and labelled with the hardware name. Fan speed lives under
// component[name=FAN n]/fan/state and takes a second Get. The readings go
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

// healthFamilies are the component keys probed once per switch (and again
// every 6 hours, or after a poll fails, in case a PSU or fan changed). Keys
// are numbered from 1; a family stops after two missing in a row.
var healthFamilies = []struct {
	prefix string
	max    int
}{{"PSU", 4}, {"FAN", 32}, {"TEMP", 48}}

func componentPath(name string) string {
	return fmt.Sprintf("/openconfig-platform:components/component[name=%s]/state", name)
}

func fanSpeedPath(name string) string {
	return fmt.Sprintf("/openconfig-platform:components/component[name=%s]/fan/state", name)
}

var componentKeyRe = regexp.MustCompile(`component\[name=([^\]]+)\]`)

// componentKey is the list key in a Get update's path: "TEMP 1" from
// openconfig-platform:components/component[name=TEMP 1]/state.
func componentKey(path string) string {
	if m := componentKeyRe.FindStringSubmatch(path); m != nil {
		return m[1]
	}
	return ""
}

// gnmicUpdate is one update of a gnmic Get answer.
type gnmicUpdate struct {
	Source string
	Path   string
	Values map[string]any
}

// gnmicGetUpdates decodes a gnmic Get response keeping each update's path,
// skipping any log lines gnmic printed before the JSON.
func gnmicGetUpdates(raw []byte) []gnmicUpdate {
	var resp []struct {
		Source  string `json:"source"`
		Updates []struct {
			Path   string         `json:"path"` // gnmic writes "Path"; the match ignores case
			Values map[string]any `json:"values"`
		} `json:"updates"`
	}
	if i := bytes.Index(raw, []byte("\n[")); i >= 0 && !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("[")) {
		raw = raw[i+1:]
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil
	}
	var out []gnmicUpdate
	for _, r := range resp {
		for _, u := range r.Updates {
			out = append(out, gnmicUpdate{r.Source, u.Path, u.Values})
		}
	}
	return out
}

// healthKeys finds which component keys the switch answers for.
func (s *Server) healthKeys(ctx context.Context, cfg Config, mine func(string) bool) ([]string, error) {
	var keys []string
	var lastErr error
	for _, fam := range healthFamilies {
		misses := 0
		for i := 1; i <= fam.max && misses < 2; i++ {
			key := fmt.Sprintf("%s %d", fam.prefix, i)
			found := false
			raw, err := gnmicGet(ctx, cfg, componentPath(key))
			if err != nil {
				lastErr = err // usually "does not exist"; a dead switch fails them all
			}
			for _, u := range gnmicGetUpdates(raw) {
				if mine(u.Source) && len(u.Values) > 0 {
					found = true
				}
			}
			if found {
				keys = append(keys, key)
				misses = 0
			} else {
				misses++
			}
		}
	}
	if len(keys) == 0 && lastErr != nil {
		return nil, lastErr
	}
	return keys, nil
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

// switchHealthOf finds the switch's component keys when needed and reads
// them plus the system nodes (CPU, memory, uptime, last reboot cause). Each
// part is read with its own Get, so one failing does not blank the rest.
func (s *Server) switchHealthOf(ctx context.Context, cfg Config, bySource map[string]SwitchConfig, sw SwitchConfig) (api.SwitchHealth, error) {
	mine := func(source string) bool {
		got, ok := gnmiSource(bySource, source)
		return ok && strings.EqualFold(got.Name, sw.Name)
	}
	s.health.mu.Lock()
	keys, at := s.health.names[sw.Name], s.health.namedAt[sw.Name]
	s.health.mu.Unlock()
	if keys == nil || time.Since(at) > 6*time.Hour {
		found, err := s.healthKeys(ctx, cfg, mine)
		if err != nil {
			return api.SwitchHealth{}, err
		}
		keys = found
		s.health.mu.Lock()
		s.health.names[sw.Name], s.health.namedAt[sw.Name] = keys, time.Now()
		s.health.mu.Unlock()
		log.Printf("switch %s: %d health components (%s)", sw.Name, len(keys), strings.Join(keys, ", "))
	}
	h := api.SwitchHealth{At: time.Now(), Temps: []api.SwitchTemp{}, Fans: []api.SwitchPart{}, PSUs: []api.SwitchPart{}}
	var errs []string
	raw, err := gnmicGet(ctx, cfg, "/openconfig-system:system/openconfig-system-ext:infra", "/openconfig-system:system/cpus", "/openconfig-system:system/memory")
	if err != nil {
		errs = append(errs, "system: "+err.Error())
	}
	for _, u := range gnmicGetUpdates(raw) {
		if mine(u.Source) {
			parseHealth(u.Values, &h)
		}
	}
	var paths, fans []string
	for _, k := range keys {
		paths = append(paths, componentPath(k))
		if strings.HasPrefix(k, "FAN") {
			fans = append(fans, fanSpeedPath(k))
		}
	}
	if len(paths) > 0 {
		raw, err := gnmicGet(ctx, cfg, paths[0], paths[1:]...)
		if err != nil {
			errs = append(errs, "components: "+err.Error())
			s.health.mu.Lock()
			s.health.namedAt[sw.Name] = time.Time{} // a part may have gone; find them again next time
			s.health.mu.Unlock()
		}
		for _, u := range gnmicGetUpdates(raw) {
			if mine(u.Source) {
				parseComponent(componentKey(u.Path), u.Values, &h)
			}
		}
	}
	if len(fans) > 0 {
		raw, err := gnmicGet(ctx, cfg, fans[0], fans[1:]...)
		if err != nil {
			errs = append(errs, "fan speed: "+err.Error())
		}
		for _, u := range gnmicGetUpdates(raw) {
			if mine(u.Source) {
				parseFanSpeed(componentKey(u.Path), u.Values, &h)
			}
		}
	}
	if len(errs) > 0 {
		if h.CPUPct == 0 && h.MemPct == 0 && len(h.Temps)+len(h.Fans)+len(h.PSUs) == 0 {
			return h, fmt.Errorf("%s", strings.Join(errs, "; "))
		}
		log.Printf("switch %s health: %s", sw.Name, strings.Join(errs, "; "))
	}
	sort.Slice(h.Temps, func(i, j int) bool { return keyLess(h.Temps[i].Component, h.Temps[j].Component) })
	sort.Slice(h.Fans, func(i, j int) bool { return keyLess(h.Fans[i].Component, h.Fans[j].Component) })
	sort.Slice(h.PSUs, func(i, j int) bool { return keyLess(h.PSUs[i].Component, h.PSUs[j].Component) })
	return h, nil
}

// keyLess orders component keys by family, then number ("FAN 2" < "FAN 10").
func keyLess(a, b string) bool {
	pa, na, _ := strings.Cut(a, " ")
	pb, nb, _ := strings.Cut(b, " ")
	if pa != pb {
		return pa < pb
	}
	ia, _ := strconv.Atoi(na)
	ib, _ := strconv.Atoi(nb)
	if ia != ib {
		return ia < ib
	}
	return a < b
}

// leaves collects every scalar leaf below v by its name without the module
// prefix; the first one found at the shallowest depth wins.
func leaves(v any) map[string]any {
	out := map[string]any{}
	level := []any{v}
	for len(level) > 0 {
		var next []any
		for _, x := range level {
			switch t := x.(type) {
			case map[string]any:
				names := make([]string, 0, len(t))
				for k := range t {
					names = append(names, k)
				}
				sort.Strings(names)
				for _, k := range names {
					switch c := t[k].(type) {
					case map[string]any, []any:
						next = append(next, c)
					default:
						if _, ok := out[stripModule(k)]; !ok {
							out[stripModule(k)] = c
						}
					}
				}
			case []any:
				next = append(next, t...)
			}
		}
		level = next
	}
	return out
}

// parseComponent reads one component's state. key is what the controller
// asked for ("TEMP 1"), which says what the part is; the answer's own name
// ("CPU Temp") is the label. Without a key the answer's name decides.
func parseComponent(key string, v map[string]any, h *api.SwitchHealth) {
	f := leaves(v)
	name := stringOf(f, "name")
	if name == "" {
		name = key
	}
	kind := key
	if kind == "" {
		kind = name
	}
	kind = strings.ToUpper(kind)
	switch {
	case strings.HasPrefix(kind, "TEMP"):
		if c, ok := firstFloat(f, "current", "instant"); ok && !hasTemp(h.Temps, key, name) {
			t := api.SwitchTemp{Name: name, Component: key, C: c}
			t.HighC, _ = firstFloat(f, "high-threshold", "high")
			t.CriticalC, _ = firstFloat(f, "critical-high-threshold", "critical-high")
			h.Temps = append(h.Temps, t)
		}
	case strings.HasPrefix(kind, "FAN"):
		if hasAny(f, "oper-status") && !hasPart(h.Fans, key, name) {
			h.Fans = append(h.Fans, api.SwitchPart{Name: name, Component: key, OK: operOK(f), Status: stringOf(f, "oper-status")})
		}
	case strings.HasPrefix(kind, "PSU"):
		if hasAny(f, "oper-status") && !hasPart(h.PSUs, key, name) {
			h.PSUs = append(h.PSUs, api.SwitchPart{Name: name, Component: key, OK: operOK(f), Status: stringOf(f, "oper-status")})
		}
	}
}

// parseFanSpeed adds the RPM from component[name=FAN n]/fan/state to the
// fan read under that key.
func parseFanSpeed(key string, v map[string]any, h *api.SwitchHealth) {
	rpm, ok := firstFloat(leaves(v), "speed")
	if !ok || key == "" {
		return
	}
	for i := range h.Fans {
		if h.Fans[i].Component == key {
			h.Fans[i].RPM = rpm
		}
	}
}

// parseHealth picks the system leaves out of one gnmic update value: CPUs
// like {"index":"ALL","state":{"total":{"instant":4}}}, memory like
// {"state":{"physical":N,"used":N}}, infra like {"state":{"uptime":...}}.
func parseHealth(v map[string]any, h *api.SwitchHealth) {
	walkObjects(v, func(m map[string]any) {
		st, _ := m["state"].(map[string]any)
		if st == nil {
			st = m
		}
		f := flatten(m)
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

func hasPart(parts []api.SwitchPart, key, name string) bool {
	for _, p := range parts {
		if p.Component == key && p.Name == name {
			return true
		}
	}
	return false
}

func hasTemp(temps []api.SwitchTemp, key, name string) bool {
	for _, t := range temps {
		if t.Component == key && t.Name == name {
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
	part := func(kind, name, key string) string {
		l := copyWith(base, kind, name)
		if key != "" {
			l["component"] = key
		}
		return formatLabels(l)
	}
	for _, t := range h.Temps {
		l := part("sensor", t.Name, t.Component)
		fmt.Fprintf(&b, "srnet_switch_temp_c%s %g %d\n", l, t.C, ts)
		if t.HighC > 0 {
			fmt.Fprintf(&b, "srnet_switch_temp_high_c%s %g %d\n", l, t.HighC, ts)
		}
		if t.CriticalC > 0 {
			fmt.Fprintf(&b, "srnet_switch_temp_crit_c%s %g %d\n", l, t.CriticalC, ts)
		}
	}
	for _, p := range h.Fans {
		l := part("fan", p.Name, p.Component)
		fmt.Fprintf(&b, "srnet_switch_fan_ok%s %d %d\n", l, b2i(p.OK), ts)
		if p.RPM > 0 {
			fmt.Fprintf(&b, "srnet_switch_fan_rpm%s %g %d\n", l, p.RPM, ts)
		}
	}
	for _, p := range h.PSUs {
		fmt.Fprintf(&b, "srnet_switch_psu_ok%s %d %d\n", part("psu", p.Name, p.Component), b2i(p.OK), ts)
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
