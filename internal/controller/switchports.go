package controller

import (
	"context"
	"strings"
)

// switchIface is one interface from the switch's own port table: the name
// the switch uses internally (Ethernet8), its alias (Eth1/2/1, the CLI name
// when the switch runs in standard naming mode) and the description an
// operator typed on it (SPARK-01-RDMA-P2), which is what the switch sends
// as the LLDP port description.
type switchIface struct {
	Name, Alias, Descr string
}

// switchIfaces reads each switch's port table (SONiC's PORT_LIST, one Get
// for the whole switch) and keys it switch name -> lower-cased ifname.
func (s *Server) switchIfaces(ctx context.Context) (map[string]map[string]switchIface, error) {
	out := map[string]map[string]switchIface{}
	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()
	bySource := gnmiSources(cfg)
	if len(bySource) == 0 {
		return out, nil
	}
	raw, err := gnmicGet(ctx, cfg, "/sonic-port:sonic-port/PORT/PORT_LIST")
	if err != nil {
		return out, err
	}
	for source, ifaces := range parsePortList(raw) {
		sw, ok := gnmiSource(bySource, source)
		if !ok {
			continue
		}
		key := strings.ToLower(sw.Name)
		if out[key] == nil {
			out[key] = map[string]switchIface{}
		}
		for _, ifc := range ifaces {
			out[key][strings.ToLower(ifc.Name)] = ifc
		}
	}
	return out, nil
}

// parsePortList reads gnmic's rendering of a Get on SONiC's PORT_LIST
// ({"ifname":"Ethernet8","alias":"Eth1/2/1","description":"..."} rows) and
// returns the interfaces per source.
func parsePortList(raw []byte) map[string][]switchIface {
	res := map[string][]switchIface{}
	for source, values := range gnmicUpdates(raw) {
		var walk func(v any)
		walk = func(v any) {
			switch t := v.(type) {
			case map[string]any:
				if name, _ := t["ifname"].(string); name != "" {
					alias, _ := t["alias"].(string)
					descr, _ := t["description"].(string)
					res[source] = append(res[source], switchIface{Name: name, Alias: alias, Descr: descr})
					return
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
		for _, v := range values {
			walk(v)
		}
	}
	return res
}

// switchNaming reads each switch's interface naming mode (Enterprise SONiC
// reports NATIVE or STANDARD under system/state). In standard mode the CLI
// only takes the alias (Eth1/2/1), in native mode the name (Ethernet8).
func (s *Server) switchNaming(ctx context.Context) (map[string]string, error) {
	out := map[string]string{}
	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()
	bySource := gnmiSources(cfg)
	if len(bySource) == 0 {
		return out, nil
	}
	raw, err := gnmicGet(ctx, cfg, "/openconfig-system:system/state")
	if err != nil {
		return out, err
	}
	for source, mode := range parseNamingMode(raw) {
		if sw, ok := gnmiSource(bySource, source); ok {
			out[strings.ToLower(sw.Name)] = mode
		}
	}
	return out, nil
}

func parseNamingMode(raw []byte) map[string]string {
	res := map[string]string{}
	for source, values := range gnmicUpdates(raw) {
		var walk func(v any)
		walk = func(v any) {
			if t, ok := v.(map[string]any); ok {
				for k, x := range t {
					if m, ok := x.(string); ok && strings.HasSuffix(k, "intf-naming-mode") {
						res[source] = strings.ToUpper(m)
					}
					walk(x)
				}
			}
		}
		for _, v := range values {
			walk(v)
		}
	}
	return res
}

// knownIfaces is every interface the controller can vouch for on each
// switch: the port table where it was read, else the ports the switch's
// LLDP and VLAN tables name. A switch with none of these is absent, and
// then nothing can be checked against it.
func knownIfaces(ifaces map[string]map[string]switchIface, lldp map[string]lldpEntry, vlans map[string]string) map[string]map[string]switchIface {
	out := map[string]map[string]switchIface{}
	for sw, tab := range ifaces {
		if len(tab) > 0 {
			out[sw] = tab
		}
	}
	add := func(sw, port string) {
		sw = strings.ToLower(sw)
		if port == "" || len(ifaces[sw]) > 0 {
			return
		}
		if out[sw] == nil {
			out[sw] = map[string]switchIface{}
		}
		if _, ok := out[sw][strings.ToLower(port)]; !ok {
			out[sw][strings.ToLower(port)] = switchIface{Name: port}
		}
	}
	for _, e := range lldp {
		add(e.Switch, e.Port)
	}
	for k := range vlans {
		if sw, port, ok := strings.Cut(k, "|"); ok {
			add(sw, port)
		}
	}
	return out
}

// lookupIface finds the switch interface a host's lldpd named: by its
// name, its alias, or (when only one interface carries it) its
// description, which is all some switches send.
func lookupIface(tab map[string]switchIface, name string) (switchIface, bool) {
	if name == "" {
		return switchIface{}, false
	}
	if ifc, ok := tab[strings.ToLower(name)]; ok {
		return ifc, true
	}
	var hit switchIface
	n := 0
	for _, ifc := range tab {
		if ifc.Alias != "" && strings.EqualFold(ifc.Alias, name) {
			return ifc, true
		}
		if ifc.Descr != "" && strings.EqualFold(ifc.Descr, name) {
			hit = ifc
			n++
		}
	}
	return hit, n == 1
}

// cliName is the name sonic-cli takes for a switch interface.
func cliName(naming string, ifc switchIface) string {
	if naming == "STANDARD" && ifc.Alias != "" {
		return ifc.Alias
	}
	return ifc.Name
}

// fillLLDPFrom sets each host port's switch neighbour. The switch's own
// LLDP table wins: it is keyed by the host's hostname and netdev, which is
// what a Linux lldpd advertises, and it names the switch port as the
// switch knows it. Without an entry there the host's lldpd report is used,
// but only when it names a real interface of a configured switch (looked
// up by name, alias or description), and never when it is the host itself
// or another enrolled host: a ConnectX-7 on a Spark exposes each cable as
// two PCIe functions that hear each other's LLDP, and some switches send
// a description (SPARK-01-RDMA-P2) rather than the interface name.
func fillLLDPFrom(hosts []hostRec, lldp map[string]lldpEntry, known map[string]map[string]switchIface, switches []SwitchConfig) {
	hostNames := map[string]bool{}
	for _, h := range hosts {
		if h.Hostname != "" {
			hostNames[strings.ToLower(h.Hostname)] = true
		}
	}
	for i := range hosts {
		h := &hosts[i]
		own := map[string]bool{}
		for _, p := range h.Ports {
			own[strings.ToLower(p.Netdev)] = true
		}
		for j := range h.Ports {
			p := &h.Ports[j]
			if e, ok := lldp[strings.ToLower(h.Hostname)+"|"+strings.ToLower(p.Netdev)]; ok {
				p.LLDPSwitch, p.LLDPPort = e.Switch, e.Port
				continue
			}
			if p.LLDPSwitch == "" && p.LLDPPort == "" {
				continue
			}
			sw := resolveSwitchName(switches, p.LLDPSwitch)
			drop := hostNames[strings.ToLower(p.LLDPSwitch)] || own[strings.ToLower(p.LLDPPort)] || own[strings.ToLower(p.LLDPDescr)]
			if !drop && sw != "" {
				if tab, ok := known[strings.ToLower(sw)]; ok {
					ifc, found := lookupIface(tab, p.LLDPPort)
					if !found {
						ifc, found = lookupIface(tab, p.LLDPDescr)
					}
					if found {
						p.LLDPSwitch, p.LLDPPort = sw, ifc.Name
					} else {
						drop = true
					}
				}
			}
			if drop {
				p.LLDPSwitch, p.LLDPPort = "", ""
			}
		}
	}
}
