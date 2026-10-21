package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Realishbird/SR-Net-Speed-Test/internal/api"
)

// vmSeries is one row of a VictoriaMetrics instant query.
type vmSeries struct {
	Labels map[string]string
	Value  float64
}

// gnmiMetric selects the gnmic series for one interface counter leaf. gnmic
// names metrics after the whole gNMI path with the configured prefix, e.g.
// srnet_gnmi_interfaces_interface_state_counters_in_octets, and the exact
// middle depends on how the switch answers, so match on the leaf.
func gnmiMetric(leaf string, labels ...string) string {
	sel := `__name__=~"srnet_gnmi_.*` + leaf + `"`
	for _, l := range labels {
		sel += "," + l
	}
	return "{" + sel + "}"
}

// vmQuery runs a PromQL instant query against VictoriaMetrics.
func (m *metrics) vmQuery(ctx context.Context, q string) ([]vmSeries, error) {
	if m.vmURL == "" {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", m.vmURL+"/api/v1/query?query="+url.QueryEscape(q), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var body struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Value  []any             `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	if body.Status != "" && body.Status != "success" {
		return nil, fmt.Errorf("victoriametrics: %s", body.Error)
	}
	out := make([]vmSeries, 0, len(body.Data.Result))
	for _, r := range body.Data.Result {
		if len(r.Value) != 2 {
			continue
		}
		v, err := strconv.ParseFloat(fmt.Sprint(r.Value[1]), 64)
		if err != nil {
			continue
		}
		out = append(out, vmSeries{Labels: r.Metric, Value: v})
	}
	return out, nil
}

// switchPortStats reads the latest per-port figures for every configured
// switch out of VictoriaMetrics: gNMI series when gnmic is streaming, else
// the SNMP poller's. Keys are "<switch name>/<port name>" lower-cased.
func (s *Server) switchPortStats(ctx context.Context) (map[string]*api.TopoSwitchPort, map[string]string, error) {
	ports := map[string]*api.TopoSwitchPort{}
	source := map[string]string{} // switch name -> gnmi | snmp
	switches := s.switchList()
	if len(switches) == 0 || s.metrics.vmURL == "" {
		return ports, source, nil
	}
	// gnmic names the port in a label derived from the path key
	// (interface_name); the SNMP poller uses "port". Try both.
	portOf := func(l map[string]string) string {
		for _, k := range []string{"interface_name", "name", "port"} {
			if v := l[k]; v != "" {
				return v
			}
		}
		return ""
	}
	get := func(sw SwitchConfig, name string) *api.TopoSwitchPort {
		key := strings.ToLower(sw.Name + "/" + name)
		p := ports[key]
		if p == nil {
			p = &api.TopoSwitchPort{Switch: sw.Name, Name: name}
			ports[key] = p
		}
		return p
	}
	// gNMI: match series to a switch by the lower-cased target name (see gnmiSeriesFor).
	bySource := map[string]SwitchConfig{}
	for _, sw := range switches {
		if sw.GNMIUser != "" {
			bySource[strings.ToLower(sw.Name)] = sw
			bySource[strings.ToLower(sw.Address)] = sw
			bySource[strings.ToLower(fmt.Sprintf("%s:%d", sw.Address, sw.GNMIPort))] = sw
		}
	}
	gnmiSwitch := func(l map[string]string) (SwitchConfig, bool) {
		src := strings.ToLower(l["source"])
		if sw, ok := bySource[src]; ok {
			return sw, true
		}
		name, _, _ := strings.Cut(src, ":")
		sw, ok := bySource[name]
		return sw, ok
	}
	type q struct {
		expr string
		set  func(p *api.TopoSwitchPort, v float64, l map[string]string)
	}
	gnmiQueries := []q{
		{`rate(` + gnmiMetric("in_octets") + `[2m])*8/1e9`, func(p *api.TopoSwitchPort, v float64, _ map[string]string) { p.RxGbps = v }},
		{`rate(` + gnmiMetric("out_octets") + `[2m])*8/1e9`, func(p *api.TopoSwitchPort, v float64, _ map[string]string) { p.TxGbps = v }},
		{`last_over_time(` + gnmiMetric("in_discards") + `[2m])`, func(p *api.TopoSwitchPort, v float64, _ map[string]string) { p.InDiscards = v }},
		{`last_over_time(` + gnmiMetric("out_discards") + `[2m])`, func(p *api.TopoSwitchPort, v float64, _ map[string]string) { p.OutDiscards = v }},
		{`last_over_time(` + gnmiMetric("in_errors") + `[2m])`, func(p *api.TopoSwitchPort, v float64, _ map[string]string) { p.InErrors = v }},
		{`last_over_time(` + gnmiMetric("out_errors") + `[2m])`, func(p *api.TopoSwitchPort, v float64, _ map[string]string) { p.OutErrors = v }},
		{`last_over_time(` + gnmiMetric("in_fcs_errors") + `[2m])`, func(p *api.TopoSwitchPort, v float64, _ map[string]string) { p.FCSErrors = v }},
		{`last_over_time(` + gnmiMetric("port_speed") + `[2m])`, func(p *api.TopoSwitchPort, v float64, l map[string]string) {
			// strings-as-labels: "openconfig-if-ethernet:SPEED_100GB".
			if mbps := speedMbps(l["port_speed"]); mbps > 0 {
				p.SpeedMbps = mbps
			}
		}},
		{`last_over_time(` + gnmiMetric("oper_status") + `[2m])`, func(p *api.TopoSwitchPort, v float64, l map[string]string) {
			// strings-as-labels: the state is a label and the value is 1.
			st := strings.ToUpper(l["oper_status"])
			if st == "" {
				st = strings.ToUpper(l["state"])
			}
			up := st == "UP" || (st == "" && v == 1)
			p.Up = &up
		}},
	}
	for _, qq := range gnmiQueries {
		rows, err := s.metrics.vmQuery(ctx, qq.expr)
		if err != nil {
			return ports, source, err
		}
		for _, r := range rows {
			sw, ok := gnmiSwitch(r.Labels)
			name := portOf(r.Labels)
			if !ok || name == "" {
				continue
			}
			source[sw.Name] = "gnmi"
			qq.set(get(sw, name), r.Value, r.Labels)
		}
	}
	// SNMP fills in whatever gNMI did not cover.
	bySwitch := map[string]SwitchConfig{}
	for _, sw := range switches {
		if sw.SNMPCommunity != "" && source[sw.Name] == "" {
			bySwitch[sw.Name] = sw
		}
	}
	if len(bySwitch) > 0 {
		snmpQueries := []q{
			{`last_over_time(srnet_switch_rx_gbps[2m])`, func(p *api.TopoSwitchPort, v float64, _ map[string]string) { p.RxGbps = v }},
			{`last_over_time(srnet_switch_tx_gbps[2m])`, func(p *api.TopoSwitchPort, v float64, _ map[string]string) { p.TxGbps = v }},
			{`last_over_time(srnet_switch_in_discards[2m])`, func(p *api.TopoSwitchPort, v float64, _ map[string]string) { p.InDiscards = v }},
			{`last_over_time(srnet_switch_out_discards[2m])`, func(p *api.TopoSwitchPort, v float64, _ map[string]string) { p.OutDiscards = v }},
			{`last_over_time(srnet_switch_in_errors[2m])`, func(p *api.TopoSwitchPort, v float64, _ map[string]string) { p.InErrors = v }},
			{`last_over_time(srnet_switch_out_errors[2m])`, func(p *api.TopoSwitchPort, v float64, _ map[string]string) { p.OutErrors = v }},
			{`last_over_time(srnet_switch_speed_mbps[2m])`, func(p *api.TopoSwitchPort, v float64, _ map[string]string) { p.SpeedMbps = int(v) }},
			{`last_over_time(srnet_switch_up[2m])`, func(p *api.TopoSwitchPort, v float64, _ map[string]string) { up := v == 1; p.Up = &up }},
		}
		for _, qq := range snmpQueries {
			rows, err := s.metrics.vmQuery(ctx, qq.expr)
			if err != nil {
				return ports, source, err
			}
			for _, r := range rows {
				sw, ok := bySwitch[r.Labels["switch"]]
				name := portOf(r.Labels)
				if !ok || name == "" {
					continue
				}
				source[sw.Name] = "snmp"
				qq.set(get(sw, name), r.Value, r.Labels)
			}
		}
	}
	return ports, source, nil
}

// speedMbps reads an OpenConfig port speed such as SPEED_100GB or
// openconfig-if-ethernet:SPEED_2500MB; 0 when it is not one.
func speedMbps(s string) int {
	_, s, _ = strings.Cut(s, "SPEED_")
	n, unit := splitUnit(s)
	switch unit {
	case "GB":
		return n * 1000
	case "MB":
		return n
	}
	return 0
}

func splitUnit(s string) (int, string) {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	n, _ := strconv.Atoi(s[:i])
	return n, s[i:]
}

// lldpEntry is one neighbour the switch itself reported: which of its ports
// a host's netdev is cabled to.
type lldpEntry struct {
	Switch, Port string
}

// switchLLDP reads every configured switch's own LLDP table out of
// VictoriaMetrics (gnmic subscribes to it) and keys it by the neighbour's
// system name and port description, lower-cased, which on a Linux host are
// its hostname and netdev. A host whose agent cannot run lldpctl is still
// placed, because the switch saw it.
func (s *Server) switchLLDP(ctx context.Context) map[string]lldpEntry {
	out := map[string]lldpEntry{}
	switches := s.switchList()
	if len(switches) == 0 || s.metrics.vmURL == "" {
		return out
	}
	bySource := map[string]SwitchConfig{}
	for _, sw := range switches {
		if sw.GNMIUser != "" {
			bySource[strings.ToLower(sw.Name)] = sw
			bySource[strings.ToLower(sw.Address)] = sw
			bySource[strings.ToLower(fmt.Sprintf("%s:%d", sw.Address, sw.GNMIPort))] = sw
		}
	}
	// Each leaf is its own series; join system-name and port-description on
	// the interface and neighbour keys.
	type key struct{ source, iface, id string }
	names := map[key]string{}
	descs := map[key]string{}
	for leaf, dst := range map[string]map[key]string{"system_name": names, "port_description": descs} {
		rows, err := s.metrics.vmQuery(ctx, `last_over_time(`+gnmiMetric("lldp.*neighbor_state_"+leaf)+`[5m])`)
		if err != nil {
			return out
		}
		for _, r := range rows {
			k := key{strings.ToLower(r.Labels["source"]), r.Labels["name"], r.Labels["id"]}
			if v := r.Labels[leaf]; v != "" && k.iface != "" {
				dst[k] = v
			}
		}
	}
	for k, sys := range names {
		desc := descs[k]
		if desc == "" {
			continue
		}
		sw, ok := bySource[k.source]
		if !ok {
			name, _, _ := strings.Cut(k.source, ":")
			if sw, ok = bySource[name]; !ok {
				continue
			}
		}
		out[strings.ToLower(sys)+"|"+strings.ToLower(desc)] = lldpEntry{Switch: sw.Name, Port: k.iface}
	}
	return out
}

// fillLLDP completes each host port's switch neighbour from the switches'
// own LLDP tables where the host's lldpd gave nothing, so the Hosts page,
// the Topology and per-leg switch figures work even before every host runs
// lldpctl. The host's own report wins when it has one.
func (s *Server) fillLLDP(ctx context.Context, hosts []hostRec) {
	lldp := s.cachedLLDP(ctx)
	if len(lldp) == 0 {
		return
	}
	for i := range hosts {
		h := &hosts[i]
		for j := range h.Ports {
			p := &h.Ports[j]
			if p.LLDPPort != "" {
				continue
			}
			if e, ok := lldp[strings.ToLower(h.Hostname)+"|"+strings.ToLower(p.Netdev)]; ok {
				p.LLDPSwitch, p.LLDPPort = e.Switch, e.Port
			}
		}
	}
}

// cachedLLDP keeps the switch LLDP map for a minute: the Hosts page polls
// every few seconds and the table changes when cables move, not before.
func (s *Server) cachedLLDP(ctx context.Context) map[string]lldpEntry {
	s.mu.Lock()
	fresh := s.lldpAt.Add(time.Minute).After(time.Now())
	m := s.lldp
	s.mu.Unlock()
	if fresh {
		return m
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	m = s.switchLLDP(ctx)
	s.mu.Lock()
	s.lldp, s.lldpAt = m, time.Now()
	s.mu.Unlock()
	return m
}

// buildTopology joins hosts, their LLDP neighbours and the switch port
// figures into one picture. A host port names its switch by the chassis
// name lldpd saw, which may differ in case from the configured switch name,
// or be missing when LLDP has not run yet; with a single switch configured
// an unknown chassis name is taken to be that switch.
func buildTopology(hosts []hostRec, switches []SwitchConfig, ports map[string]*api.TopoSwitchPort, source map[string]string, live func(portID int64) (tx, rx float64), hostLive func(host string) (api.HostStats, bool), online func(agentID int64) bool) api.Topology {
	t := api.Topology{At: time.Now(), Switches: []api.TopoSwitch{}, Hosts: []api.TopoHost{}, Runs: []api.TopoRun{}}
	resolveSwitch := func(chassis string) string { return resolveSwitchName(switches, chassis) }
	used := map[string]bool{}
	for _, h := range hosts {
		th := api.TopoHost{ID: h.ID, Name: h.Name, Hostname: h.Hostname, Tags: h.Tags, Online: online(h.ID), Ports: []api.TopoHostPort{}}
		if th.Tags == nil {
			th.Tags = []string{}
		}
		if st, ok := hostLive(h.Name); ok {
			th.Stats = &st
		}
		for _, p := range h.Ports {
			tp := api.TopoHostPort{ID: p.ID, RdmaDev: p.RdmaDev, PortNum: p.PortNum, Netdev: p.Netdev, Label: p.Label, Fabric: p.Fabric, State: p.State, SpeedMbps: p.SpeedMbps, LLDPSwitch: p.LLDPSwitch, LLDPPort: p.LLDPPort}
			for _, a := range p.Addrs {
				if ip, _, err := net.ParseCIDR(a); err == nil {
					tp.IP = ip.String()
					break
				}
			}
			tp.TxGbps, tp.RxGbps = live(p.ID)
			if p.LLDPPort != "" {
				tp.Switch = resolveSwitch(p.LLDPSwitch)
				if tp.Switch != "" {
					tp.SwitchPort = p.LLDPPort
					key := strings.ToLower(tp.Switch + "/" + p.LLDPPort)
					used[key] = true
					if sp := ports[key]; sp != nil {
						sp.Hosts = append(sp.Hosts, h.Name)
					} else {
						ports[key] = &api.TopoSwitchPort{Switch: tp.Switch, Name: p.LLDPPort, Hosts: []string{h.Name}}
					}
				}
			}
			th.Ports = append(th.Ports, tp)
		}
		t.Hosts = append(t.Hosts, th)
	}
	for _, sw := range switches {
		ts := api.TopoSwitch{Name: sw.Name, Address: sw.Address, Source: source[sw.Name], Ports: []api.TopoSwitchPort{}}
		for key, p := range ports {
			if !strings.HasPrefix(key, strings.ToLower(sw.Name)+"/") {
				continue
			}
			// Only ports that lead to a known host or carry traffic make the
			// picture; a 64-port switch of idle ports is noise.
			if used[key] || p.RxGbps >= 0.1 || p.TxGbps >= 0.1 {
				if p.Hosts == nil {
					p.Hosts = []string{}
				}
				ts.Ports = append(ts.Ports, *p)
			}
		}
		sort.Slice(ts.Ports, func(i, j int) bool { return portLess(ts.Ports[i].Name, ts.Ports[j].Name) })
		t.Switches = append(t.Switches, ts)
	}
	return t
}

// portLess orders "Ethernet8" before "Ethernet64".
func portLess(a, b string) bool {
	pa, na := splitNum(a)
	pb, nb := splitNum(b)
	if pa != pb {
		return pa < pb
	}
	return na < nb
}

func splitNum(s string) (string, int) {
	i := len(s)
	for i > 0 && s[i-1] >= '0' && s[i-1] <= '9' {
		i--
	}
	n, _ := strconv.Atoi(s[i:])
	return s[:i], n
}

func (s *Server) topology(w http.ResponseWriter, r *http.Request) {
	hosts, err := s.store.Hosts(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	s.fillLLDP(r.Context(), hosts)
	ports, source, err := s.switchPortStats(r.Context())
	t := buildTopology(hosts, s.switchList(), ports, source, s.metrics.live, s.metrics.hostLive, s.hub.online)
	if err != nil {
		t.Warning = "switch counters unavailable: " + err.Error()
	}
	t.Runs = s.orch.ActiveLegs()
	writeJSON(w, t)
}
