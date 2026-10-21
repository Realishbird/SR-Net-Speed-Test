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
	ports, source, err := s.switchPortStats(r.Context())
	t := buildTopology(hosts, s.switchList(), ports, source, s.metrics.live, s.metrics.hostLive, s.hub.online)
	if err != nil {
		t.Warning = "switch counters unavailable: " + err.Error()
	}
	t.Runs = s.orch.ActiveLegs()
	writeJSON(w, t)
}
