package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
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

// switchLLDP asks every switch with gNMI settings for its own LLDP table
// (a gNMI Get of /openconfig-lldp:lldp/interfaces, run through gnmic with
// the same config file its subscriptions use) and keys the neighbours by
// system name and port description, lower-cased, which on a Linux host are
// its hostname and netdev. A host whose agent cannot run lldpctl is still
// placed, because the switch saw it. This is a Get rather than a
// subscription because Enterprise SONiC refuses the wildcard interface
// list in a Subscribe ("wildcard path not supported") and LLDP changes
// when cables move, not more often.
func (s *Server) switchLLDP(ctx context.Context) (map[string]lldpEntry, error) {
	out := map[string]lldpEntry{}
	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()
	bySource := gnmiSources(cfg)
	if len(bySource) == 0 {
		return out, nil
	}
	raw, err := gnmicGet(ctx, cfg, "/openconfig-lldp:lldp/interfaces")
	if err != nil {
		return out, err
	}
	for source, neighbours := range parseLLDPGet(raw) {
		sw, ok := gnmiSource(bySource, source)
		if !ok {
			continue
		}
		for _, n := range neighbours {
			if n.SystemName == "" || n.PortDesc == "" {
				continue
			}
			out[strings.ToLower(n.SystemName)+"|"+strings.ToLower(n.PortDesc)] = lldpEntry{Switch: sw.Name, Port: n.Iface}
		}
	}
	return out, nil
}

// gnmiSources indexes the switches with gNMI settings by every spelling
// gnmic may use as a response source: the target name, the address and
// address:port, lower-cased.
func gnmiSources(cfg Config) map[string]SwitchConfig {
	bySource := map[string]SwitchConfig{}
	for _, sw := range cfg.Switches {
		if sw.GNMIUser == "" || sw.Address == "" {
			continue
		}
		port := sw.GNMIPort
		if port == 0 {
			port = 8080
		}
		host, _, _ := strings.Cut(sw.Address, ":")
		bySource[strings.ToLower(sw.Name)] = sw
		bySource[strings.ToLower(host)] = sw
		bySource[strings.ToLower(fmt.Sprintf("%s:%d", host, port))] = sw
	}
	return bySource
}

func gnmiSource(bySource map[string]SwitchConfig, source string) (SwitchConfig, bool) {
	src := strings.ToLower(source)
	if sw, ok := bySource[src]; ok {
		return sw, true
	}
	name, _, _ := strings.Cut(src, ":")
	sw, ok := bySource[name]
	return sw, ok
}

// gnmicGet runs one gNMI Get against every switch in the gnmic config and
// returns gnmic's JSON rendering of the answers. The config file is the
// one gnmic's own subscriptions use, so the credentials live in one place.
func gnmicGet(ctx context.Context, cfg Config, path string, more ...string) ([]byte, error) {
	if _, err := os.Stat(cfg.GnmicConfigPath); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	args := []string{"--config", cfg.GnmicConfigPath, "--log=false", "--encoding", "json_ietf", "--format", "json", "get", "--path", path}
	for _, p := range more {
		args = append(args, "--path", p)
	}
	cmd := exec.CommandContext(ctx, cfg.GnmicPath, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	raw, err := cmd.Output()
	if err != nil && len(bytes.TrimSpace(raw)) == 0 {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return nil, fmt.Errorf("gnmic get %s: %v: %s", path, err, msg)
	}
	return raw, nil
}

// switchVLANs reads each switch's VLAN membership (SONiC's VLAN_MEMBER
// table, one Get for the whole switch; the OpenConfig vlans tree is not
// served by Enterprise SONiC) and keys it switch|port, lower-cased, with
// the VLAN number as the value: "Vlan100" on Ethernet0 is fabric 100, the
// fabric as the switch sees it.
func (s *Server) switchVLANs(ctx context.Context) (map[string]string, error) {
	out := map[string]string{}
	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()
	bySource := gnmiSources(cfg)
	if len(bySource) == 0 {
		return out, nil
	}
	raw, err := gnmicGet(ctx, cfg, "/sonic-vlan:sonic-vlan/VLAN_MEMBER")
	if err != nil {
		return out, err
	}
	for source, members := range parseVLANMembers(raw) {
		sw, ok := gnmiSource(bySource, source)
		if !ok {
			continue
		}
		for port, vlan := range members {
			out[strings.ToLower(sw.Name+"|"+port)] = vlan
		}
	}
	return out, nil
}

// parseVLANMembers reads gnmic's rendering of a Get on SONiC's VLAN_MEMBER
// table ({"ifname":"Ethernet0","name":"Vlan100","tagging_mode":"untagged"}
// rows under VLAN_MEMBER_LIST) and returns, per source, port -> VLAN
// number. A port in several VLANs (a trunk) keeps the lowest-numbered
// untagged one, else the first seen.
func parseVLANMembers(raw []byte) map[string]map[string]string {
	var resp []struct {
		Source  string `json:"source"`
		Updates []struct {
			Values map[string]any `json:"values"`
		} `json:"updates"`
	}
	res := map[string]map[string]string{}
	if i := bytes.Index(raw, []byte("\n[")); i >= 0 && !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("[")) {
		raw = raw[i+1:]
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return res
	}
	for _, r := range resp {
		var walk func(v any)
		walk = func(v any) {
			switch t := v.(type) {
			case map[string]any:
				ifname, _ := t["ifname"].(string)
				name, _ := t["name"].(string)
				mode, _ := t["tagging_mode"].(string)
				if ifname != "" && strings.HasPrefix(name, "Vlan") {
					id := strings.TrimPrefix(name, "Vlan")
					if res[r.Source] == nil {
						res[r.Source] = map[string]string{}
					}
					if cur, seen := res[r.Source][ifname]; !seen || mode == "untagged" && cur != id {
						res[r.Source][ifname] = id
					}
				}
				for _, x := range t {
					walk(x)
				}
			case []any:
				for _, x := range t {
					walk(x)
				}
			}
		}
		for _, u := range r.Updates {
			walk(u.Values)
		}
	}
	return res
}

// lldpNeighbour is one row of a switch's LLDP table.
type lldpNeighbour struct {
	Iface, SystemName, PortDesc, PortID string
}

// parseLLDPGet reads gnmic's JSON rendering of a Get on
// /openconfig-lldp:lldp/interfaces: a list of responses, one per target,
// each with "source" and "updates" whose "values" hold the OpenConfig tree.
// The tree is walked generically because the exact nesting and module
// prefixes ("openconfig-lldp:interface") vary between switches: an object
// with a "name" and a "neighbors" child is an interface, and a "state"
// object below it that carries a system-name or port-description is a
// neighbour.
func parseLLDPGet(raw []byte) map[string][]lldpNeighbour {
	var resp []struct {
		Source  string `json:"source"`
		Updates []struct {
			Values map[string]any `json:"values"`
		} `json:"updates"`
	}
	res := map[string][]lldpNeighbour{}
	// With log: true gnmic may print a few log lines first; the answer
	// starts at the first line that opens the JSON list.
	if i := bytes.Index(raw, []byte("\n[")); i >= 0 && !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("[")) {
		raw = raw[i+1:]
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return res
	}
	for _, r := range resp {
		var walk func(v any, iface string)
		walk = func(v any, iface string) {
			switch t := v.(type) {
			case map[string]any:
				m := make(map[string]any, len(t))
				for k, x := range t {
					m[stripModule(k)] = x
				}
				if n, ok := m["name"].(string); ok {
					if _, has := m["neighbors"]; has {
						iface = n
					}
				}
				if st, ok := m["state"].(map[string]any); ok && iface != "" {
					nb := lldpNeighbour{Iface: iface}
					for k, x := range st {
						sv, _ := x.(string)
						switch stripModule(k) {
						case "system-name":
							nb.SystemName = sv
						case "port-description":
							nb.PortDesc = sv
						case "port-id":
							nb.PortID = sv
						}
					}
					if nb.SystemName != "" || nb.PortDesc != "" {
						res[r.Source] = append(res[r.Source], nb)
					}
				}
				for _, x := range t {
					walk(x, iface)
				}
			case []any:
				for _, x := range t {
					walk(x, iface)
				}
			}
		}
		for _, u := range r.Updates {
			walk(u.Values, "")
		}
	}
	return res
}

// stripModule drops a YANG module prefix: "openconfig-lldp:interface" is
// "interface".
func stripModule(k string) string {
	if i := strings.LastIndex(k, ":"); i >= 0 {
		return k[i+1:]
	}
	return k
}

// fillLLDP completes each host port's switch neighbour from the switches'
// own LLDP tables where the host's lldpd gave nothing, so the Hosts page,
// the Topology and per-leg switch figures work even before every host runs
// lldpctl. The host's own report wins when it has one.
func (s *Server) fillLLDP(ctx context.Context, hosts []hostRec) {
	s.mu.Lock()
	lldp := s.lldp
	s.mu.Unlock()
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

// switchPollLoop refreshes the switch LLDP and VLAN tables once a minute,
// off the request path: a Get takes a second or two and both tables change
// when cables move or VLANs are reassigned, not between Hosts page polls.
// A failed poll keeps the last table and logs once per change of error text.
func (s *Server) switchPollLoop(ctx context.Context) {
	lastErr := map[string]string{}
	report := func(what string, err error, n int) {
		if err != nil {
			if msg := err.Error(); msg != lastErr[what] {
				log.Printf("switch %s: %v", what, err)
				lastErr[what] = msg
			}
			return
		}
		if lastErr[what] != "" {
			log.Printf("switch %s: %d entries", what, n)
		}
		lastErr[what] = ""
	}
	for {
		if m, err := s.switchLLDP(ctx); err == nil {
			s.mu.Lock()
			s.lldp, s.lldpAt = m, time.Now()
			s.mu.Unlock()
			report("lldp", nil, len(m))
		} else {
			report("lldp", err, 0)
		}
		if m, err := s.switchVLANs(ctx); err == nil {
			s.mu.Lock()
			s.vlans = m
			s.mu.Unlock()
			report("vlans", nil, len(m))
		} else {
			report("vlans", err, 0)
		}
		s.refreshQoS(ctx)
		s.pushPortInfo()
		s.pollSwitchHealth(ctx)
		if s.alerts != nil {
			s.fabricAlerts(ctx, s.fabricCheck(ctx))
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Minute):
		}
	}
}

// applyVLANs stamps each switch port with the VLAN the switch has it in,
// and gives a host port with no fabric of its own the VLAN of the switch
// port it is cabled to: the switch's view of the fabric. A host port whose
// hand-set fabric differs from that VLAN keeps its own value so the
// Topology can show the mismatch.
func applyVLANs(t *api.Topology, vlans map[string]string) {
	if len(vlans) == 0 {
		return
	}
	for i := range t.Switches {
		sw := &t.Switches[i]
		for j := range sw.Ports {
			p := &sw.Ports[j]
			p.VLAN = vlans[strings.ToLower(sw.Name+"|"+p.Name)]
		}
	}
	for i := range t.Hosts {
		for j := range t.Hosts[i].Ports {
			p := &t.Hosts[i].Ports[j]
			if p.Fabric == "" && p.SwitchPort != "" {
				p.Fabric = vlans[strings.ToLower(p.Switch+"|"+p.SwitchPort)]
			}
		}
	}
}

// collapseTwins folds two host ports that sit on the same switch port into
// one cable. A ConnectX-7 on the Sparks presents each physical port as two
// PCIe functions (enp1s0f0np0 and enP2p1s0f0np0), both run lldpd, and the
// switch lists both as neighbours on the one port. The port with an
// address keeps the switch port and is drawn; its twin points at it and
// loses the switch port, so the picture, the switch port's host list and
// the per-host switch port column count the cable once.
func collapseTwins(ports []api.TopoHostPort) {
	first := map[string]int{} // switch/port -> index of the port that keeps it
	for i := range ports {
		p := &ports[i]
		if p.SwitchPort == "" {
			continue
		}
		key := strings.ToLower(p.Switch + "/" + p.SwitchPort)
		j, seen := first[key]
		if !seen {
			first[key] = i
			continue
		}
		keep, twin := &ports[j], p
		if p.IP != "" && keep.IP == "" {
			keep, twin = p, keep
			first[key] = i
		}
		twin.TwinOf = keep.Netdev
		twin.Switch, twin.SwitchPort = "", ""
	}
}

// buildTopology joins hosts, their LLDP neighbours and the switch port
// figures into one picture. A host port names its switch by the chassis
// name lldpd saw, which may differ in case from the configured switch name,
// or be missing when LLDP has not run yet; with a single switch configured
// an unknown chassis name is taken to be that switch.
func buildTopology(hosts []hostRec, switches []SwitchConfig, ports map[string]*api.TopoSwitchPort, source map[string]string, live func(portID int64) (tx, rx float64), temps func(portID int64) (optic, nic float64), hostLive func(host string) (api.HostStats, bool), online func(agentID int64) bool) api.Topology {
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
			if temps != nil {
				tp.OpticC, tp.NicC = temps(p.ID)
			}
			if p.LLDPPort != "" {
				if tp.Switch = resolveSwitch(p.LLDPSwitch); tp.Switch != "" {
					tp.SwitchPort = p.LLDPPort
				}
			}
			th.Ports = append(th.Ports, tp)
		}
		collapseTwins(th.Ports)
		for _, tp := range th.Ports {
			if tp.SwitchPort == "" {
				continue
			}
			key := strings.ToLower(tp.Switch + "/" + tp.SwitchPort)
			used[key] = true
			if sp := ports[key]; sp != nil {
				sp.Hosts = append(sp.Hosts, h.Name)
			} else {
				ports[key] = &api.TopoSwitchPort{Switch: tp.Switch, Name: tp.SwitchPort, Hosts: []string{h.Name}}
			}
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
	t := buildTopology(hosts, s.switchList(), ports, source, s.metrics.live, s.metrics.temperatures, s.metrics.hostLive, s.hub.online)
	t.OpticMax, t.NicMax = s.alerts.hotC, s.alerts.nicHotC
	s.mu.Lock()
	vlans, lldp := s.vlans, s.lldp
	s.mu.Unlock()
	applyVLANs(&t, vlans)
	t.Links = switchLinks(lldp, s.switchList())
	if t.Links == nil {
		t.Links = []api.TopoLink{}
	}
	if err != nil {
		t.Warning = "switch counters unavailable: " + err.Error()
	}
	t.Runs = s.orch.ActiveLegs()
	writeJSON(w, t)
}
