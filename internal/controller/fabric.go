package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Realishbird/SR-Net-Speed-Test/internal/api"
)

// Fabric correctness checks (Phase 5). A fabric profile says how each fabric
// is meant to be set up; the controller reads the switch side of every cable
// (port MTU, speed, FEC, VLAN, PFC, QoS map, queue counters) over gNMI once
// a minute next to the LLDP and VLAN poll, the agents report the host side
// (MTU, RoCE mode, PFC, trust, DSCP map, ECN), and the check lists every
// cable with every setting against the profile. A mismatch opens a
// fabric_mismatch alert and is noted on any run that starts while it stands.

const schemaFabric = `
CREATE TABLE IF NOT EXISTS fabric_profiles (
	fabric     TEXT PRIMARY KEY,
	profile    JSONB NOT NULL,
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
`

// Presets the UI offers; the profile for a fabric starts from one of them.
func fabricPreset(mode string) api.FabricProfile {
	p := api.FabricProfile{Mode: mode, SwitchMTU: 9100, HostMTU: 9000, SpeedMbps: 0, PFCPriority: -1, ECNQueue: -1, RoCEDSCP: -1, CNPDSCP: -1, RoCEMode: "v2"}
	if mode == "lossless" {
		p.PFCPriority, p.ECNQueue, p.RoCEDSCP, p.CNPDSCP, p.Trust, p.HostECN = 3, 3, 26, 48, "dscp", true
	}
	return p
}

func (s *Store) FabricProfiles(ctx context.Context) ([]api.FabricProfile, error) {
	rows, err := s.db.Query(ctx, `SELECT profile, updated_at FROM fabric_profiles ORDER BY fabric`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.FabricProfile{}
	for rows.Next() {
		var raw []byte
		var at time.Time
		if err := rows.Scan(&raw, &at); err != nil {
			return nil, err
		}
		var p api.FabricProfile
		json.Unmarshal(raw, &p)
		p.UpdatedAt = &at
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) SaveFabricProfile(ctx context.Context, p api.FabricProfile) error {
	p.UpdatedAt = nil
	b, _ := json.Marshal(p)
	_, err := s.db.Exec(ctx, `INSERT INTO fabric_profiles (fabric, profile) VALUES ($1, $2)
		ON CONFLICT (fabric) DO UPDATE SET profile=EXCLUDED.profile, updated_at=now()`, p.Fabric, b)
	return err
}

func (s *Store) DeleteFabricProfile(ctx context.Context, fabric string) error {
	_, err := s.db.Exec(ctx, `DELETE FROM fabric_profiles WHERE fabric=$1`, fabric)
	return err
}

// switchPortCfg is what the switch says about one of its ports.
type switchPortCfg struct {
	MTU        int
	SpeedMbps  int
	FEC        string
	AdminUp    *bool
	PFC        []int  // priorities with PFC enabled (from openconfig-qos pfc, or the PORT_QOS_MAP pfc_enable list)
	PFCRead    bool   // PFC state was read at all
	DSCPMap    string // name of the DSCP-to-TC map bound to the port ("" = none, traffic lands in queue 0)
	TCQueueMap string // tc_to_queue_map name
	Queues     map[int]queueStat
	PauseRx    float64
	PauseTx    float64
	PauseRead  bool
}

type queueStat struct {
	Dropped, ECNMarked, WREDDropped float64
	WREDProfile                     string
}

// switchQoS reads every gNMI switch's port configuration: sonic-port PORT
// (mtu, speed, fec, admin status), PORT_QOS_MAP (which DSCP map and PFC
// priorities a port has) and the OpenConfig QoS interface tree (PFC enable
// per priority, pause counters, queue drops and ECN marks). Keyed
// switch|port, lower-cased, like the VLAN map. Each read is best effort: a
// path the switch does not serve leaves its fields unknown.
func (s *Server) switchQoS(ctx context.Context, ports map[string][]string) (map[string]*switchPortCfg, error) {
	out := map[string]*switchPortCfg{}
	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()
	bySource := gnmiSources(cfg)
	if len(bySource) == 0 {
		return out, nil
	}
	get := func(source, port string) *switchPortCfg {
		sw, ok := gnmiSource(bySource, source)
		if !ok {
			return nil
		}
		key := strings.ToLower(sw.Name + "|" + port)
		c := out[key]
		if c == nil {
			c = &switchPortCfg{Queues: map[int]queueStat{}}
			out[key] = c
		}
		return c
	}
	var firstErr error
	note := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if raw, err := gnmicGet(ctx, cfg, "/sonic-port:sonic-port/PORT"); err == nil {
		for source, m := range parseSonicPorts(raw) {
			for name, pc := range m {
				if c := get(source, name); c != nil {
					c.MTU, c.SpeedMbps, c.FEC, c.AdminUp = pc.MTU, pc.SpeedMbps, pc.FEC, pc.AdminUp
				}
			}
		}
	} else {
		note(err)
	}
	if raw, err := gnmicGet(ctx, cfg, "/sonic-port-qos-map:sonic-port-qos-map"); err == nil {
		for source, m := range parseSonicQoSMap(raw) {
			for name, qm := range m {
				if c := get(source, name); c != nil {
					c.DSCPMap, c.TCQueueMap = qm.DSCPMap, qm.TCQueueMap
					if !c.PFCRead && qm.PFC != nil {
						c.PFC, c.PFCRead = qm.PFC, true
					}
				}
			}
		}
	} else {
		note(err)
	}
	// The QoS interface tree: the whole container first (one round trip),
	// per port when the switch refuses it.
	raw, err := gnmicGet(ctx, cfg, "/openconfig-qos:qos/interfaces")
	var qos map[string]map[string]*switchPortCfg
	if err == nil {
		qos = parseQoSInterfaces(raw)
	}
	if err != nil || len(qos) == 0 {
		qos = map[string]map[string]*switchPortCfg{}
		for sw, names := range ports {
			for _, p := range gnmicPortList(names) {
				raw, err := gnmicGet(ctx, cfg, fmt.Sprintf("/openconfig-qos:qos/interfaces/interface[interface-id=%s]", p))
				if err != nil {
					note(err)
					continue
				}
				for source, m := range parseQoSInterfaces(raw) {
					if _, ok := gnmiSource(bySource, source); !ok || !strings.EqualFold(gnmiSourceName(bySource, source), sw) {
						continue
					}
					if qos[source] == nil {
						qos[source] = map[string]*switchPortCfg{}
					}
					for k, v := range m {
						qos[source][k] = v
					}
				}
			}
		}
	}
	for source, m := range qos {
		for name, q := range m {
			if c := get(source, name); c != nil {
				if q.PFCRead {
					c.PFC, c.PFCRead = q.PFC, true
				}
				c.PauseRx, c.PauseTx, c.PauseRead = q.PauseRx, q.PauseTx, q.PauseRead
				for i, qs := range q.Queues {
					c.Queues[i] = qs
				}
			}
		}
	}
	if len(out) == 0 {
		return out, firstErr
	}
	return out, nil
}

// gnmiSourceName is the configured switch name a gnmic source label maps to.
func gnmiSourceName(bySource map[string]SwitchConfig, source string) string {
	if sw, ok := gnmiSource(bySource, source); ok {
		return sw.Name
	}
	return ""
}

// gnmicUpdates decodes a gnmic Get response into its per-source update
// values, skipping any log lines gnmic printed before the JSON.
func gnmicUpdates(raw []byte) map[string][]map[string]any {
	var resp []struct {
		Source  string `json:"source"`
		Updates []struct {
			Values map[string]any `json:"values"`
		} `json:"updates"`
	}
	if i := bytes.Index(raw, []byte("\n[")); i >= 0 && !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("[")) {
		raw = raw[i+1:]
	}
	out := map[string][]map[string]any{}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return out
	}
	for _, r := range resp {
		for _, u := range r.Updates {
			out[r.Source] = append(out[r.Source], u.Values)
		}
	}
	return out
}

// walkObjects calls fn on every JSON object below v, depth first.
func walkObjects(v any, fn func(m map[string]any)) {
	switch t := v.(type) {
	case map[string]any:
		fn(t)
		for _, x := range t {
			walkObjects(x, fn)
		}
	case []any:
		for _, x := range t {
			walkObjects(x, fn)
		}
	}
}

func anyString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	case json.Number:
		return t.String()
	}
	return ""
}

func anyInt(v any) (int, bool) {
	switch t := v.(type) {
	case float64:
		return int(t), true
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(t))
		return n, err == nil
	case json.Number:
		n, err := t.Int64()
		return int(n), err == nil
	}
	return 0, false
}

func anyFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f, err == nil
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	}
	return 0, false
}

func anyBool(v any) (bool, bool) {
	switch t := v.(type) {
	case bool:
		return t, true
	case string:
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "true", "up", "on", "enabled", "1":
			return true, true
		case "false", "down", "off", "disabled", "0":
			return false, true
		}
	}
	return false, false
}

// parseSonicPorts reads the sonic-port PORT table: one entry per ifname with
// mtu, speed (Mbps), fec and admin_status.
func parseSonicPorts(raw []byte) map[string]map[string]switchPortCfg {
	res := map[string]map[string]switchPortCfg{}
	for source, vals := range gnmicUpdates(raw) {
		for _, v := range vals {
			walkObjects(v, func(m map[string]any) {
				name, _ := m["ifname"].(string)
				if name == "" {
					return
				}
				_, hasMTU := m["mtu"]
				_, hasSpeed := m["speed"]
				if !hasMTU && !hasSpeed {
					return
				}
				c := switchPortCfg{}
				c.MTU, _ = anyInt(m["mtu"])
				c.SpeedMbps, _ = anyInt(m["speed"])
				c.FEC, _ = m["fec"].(string)
				if up, ok := anyBool(m["admin_status"]); ok {
					c.AdminUp = &up
				}
				if res[source] == nil {
					res[source] = map[string]switchPortCfg{}
				}
				res[source][name] = c
			})
		}
	}
	return res
}

type qosMapEntry struct {
	DSCPMap, TCQueueMap string
	PFC                 []int // nil when the entry has no pfc_enable
}

// parseSonicQoSMap reads PORT_QOS_MAP: which DSCP-to-TC and TC-to-queue
// maps a port uses and which priorities it has PFC on ("3,4"). A port with
// no entry has no QoS mapping at all: its traffic lands in queue 0 whatever
// DSCP it carries.
func parseSonicQoSMap(raw []byte) map[string]map[string]qosMapEntry {
	res := map[string]map[string]qosMapEntry{}
	for source, vals := range gnmicUpdates(raw) {
		for _, v := range vals {
			walkObjects(v, func(m map[string]any) {
				name, _ := m["ifname"].(string)
				if name == "" {
					return
				}
				e := qosMapEntry{}
				found := false
				for k, x := range m {
					kl := strings.ToLower(k)
					switch {
					case kl == "dscp_to_tc_map":
						e.DSCPMap, found = mapRef(x), true
					case kl == "tc_to_queue_map":
						e.TCQueueMap, found = mapRef(x), true
					case kl == "pfc_enable":
						e.PFC, found = prioInts(anyString(x)), true
					}
				}
				if !found {
					return
				}
				if res[source] == nil {
					res[source] = map[string]qosMapEntry{}
				}
				res[source][name] = e
			})
		}
	}
	return res
}

// mapRef reads a SONiC table reference: a plain name, or "[DSCP_TO_TC_MAP|AZURE]".
func mapRef(v any) string {
	s := anyString(v)
	s = strings.Trim(s, "[]")
	if i := strings.LastIndex(s, "|"); i >= 0 {
		s = s[i+1:]
	}
	return s
}

func prioInts(s string) []int {
	out := []int{}
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == ';' }) {
		if n, err := strconv.Atoi(f); err == nil {
			out = append(out, n)
		}
	}
	sort.Ints(out)
	return out
}

// parseQoSInterfaces reads the OpenConfig QoS interface tree. Below each
// interface (an object with "interface-id") it takes PFC priorities (objects
// with "dot1p" and an enable flag, plus pause-frame counters) and output
// queues (objects with a "name" like "Ethernet0:3" and counters named
// dropped-pkts, ecn-marked-pkts, wred-dropped-pkts). Field names vary
// between SONiC builds, so matching is by suffix, lower-cased.
func parseQoSInterfaces(raw []byte) map[string]map[string]*switchPortCfg {
	res := map[string]map[string]*switchPortCfg{}
	for source, vals := range gnmicUpdates(raw) {
		for _, v := range vals {
			walkObjects(v, func(m map[string]any) {
				id, _ := m["interface-id"].(string)
				if id == "" || !hasSubtree(m) {
					return // the interface's own config leaf repeats interface-id; only the node with children is the interface
				}
				c := &switchPortCfg{Queues: map[int]queueStat{}}
				seenPrio, seenQueue := map[int]bool{}, map[string]bool{}
				walkObjects(m, func(o map[string]any) {
					if o["interface-id"] != nil && !sameMap(o, m) {
						return // another interface nested alongside; its own visit handles it
					}
					// A priority or queue node repeats its key inside its
					// "state" child; the parent is visited first and
					// flatten() already read the child's leaves, so the
					// child's own visit is skipped.
					if p, ok := anyInt(o["dot1p"]); ok && !seenPrio[p] {
						seenPrio[p] = true
						en, found := nestedBool(o, "enable")
						if found {
							c.PFCRead = true
							if en {
								c.PFC = append(c.PFC, p)
							}
						}
						for k, x := range flatten(o) {
							kl := strings.ToLower(k)
							if f, ok := anyFloat(x); ok {
								switch {
								case strings.HasSuffix(kl, "pause-frames-rx"), strings.HasSuffix(kl, "pause_frames_rx"):
									c.PauseRx += f
									c.PauseRead = true
								case strings.HasSuffix(kl, "pause-frames-tx"), strings.HasSuffix(kl, "pause_frames_tx"):
									c.PauseTx += f
									c.PauseRead = true
								}
							}
						}
					}
					if name, _ := o["name"].(string); strings.Contains(name, ":") && !seenQueue[name] {
						seenQueue[name] = true
						if q, err := strconv.Atoi(name[strings.LastIndex(name, ":")+1:]); err == nil {
							qs := c.Queues[q]
							for k, x := range flatten(o) {
								kl := strings.ToLower(k)
								f, isNum := anyFloat(x)
								switch {
								case isNum && strings.HasSuffix(kl, "ecn-marked-pkts"):
									qs.ECNMarked = f
								case isNum && strings.HasSuffix(kl, "wred-dropped-pkts"):
									qs.WREDDropped = f
								case isNum && strings.HasSuffix(kl, "dropped-pkts"):
									qs.Dropped = f
								case !isNum && strings.Contains(kl, "wred") && anyString(x) != "":
									qs.WREDProfile = anyString(x)
								}
							}
							c.Queues[q] = qs
						}
					}
				})
				sort.Ints(c.PFC)
				if c.PFC == nil && c.PFCRead {
					c.PFC = []int{}
				}
				if res[source] == nil {
					res[source] = map[string]*switchPortCfg{}
				}
				res[source][id] = c
			})
		}
	}
	return res
}

// hasSubtree reports whether an object has any nested object or list.
func hasSubtree(m map[string]any) bool {
	for _, v := range m {
		switch v.(type) {
		case map[string]any, []any:
			return true
		}
	}
	return false
}

func sameMap(a, b map[string]any) bool {
	return fmt.Sprintf("%p", a) == fmt.Sprintf("%p", b)
}

// nestedBool finds a boolean leaf named key in o, its "state" or its "config".
func nestedBool(o map[string]any, key string) (bool, bool) {
	for _, m := range []any{o, o["state"], o["config"]} {
		if mm, ok := m.(map[string]any); ok {
			if b, ok := anyBool(mm[key]); ok {
				return b, true
			}
		}
	}
	return false, false
}

// flatten returns the scalar leaves of o and its direct "state"/"config"
// children, keyed by leaf name (module prefixes stripped).
func flatten(o map[string]any) map[string]any {
	out := map[string]any{}
	add := func(m map[string]any) {
		for k, v := range m {
			switch v.(type) {
			case map[string]any, []any:
			default:
				out[stripModule(k)] = v
			}
		}
	}
	add(o)
	for _, k := range []string{"state", "config"} {
		if m, ok := o[k].(map[string]any); ok {
			add(m)
		}
	}
	return out
}

// hostPortView is the host side of one cable as the check needs it.
type hostPortView struct {
	host     string
	port     api.HostPort
	settings *api.PortSettings
	mtu      int
}

// fabricCheck builds the check for every cabled host port.
func (s *Server) fabricCheck(ctx context.Context) api.FabricCheck {
	fc := api.FabricCheck{At: time.Now(), Profiles: []api.FabricProfile{}, Cables: []api.CableCheck{}}
	profiles, err := s.store.FabricProfiles(ctx)
	if err != nil {
		fc.Warning = err.Error()
	}
	fc.Profiles = profiles
	hosts, err := s.store.Hosts(ctx)
	if err != nil {
		fc.Warning = err.Error()
		return fc
	}
	s.fillLLDP(ctx, hosts)
	s.mu.Lock()
	qos, vlans, qosErr := s.qos, s.vlans, s.qosErr
	s.mu.Unlock()
	if qosErr != "" {
		fc.Warning = "switch settings: " + qosErr
	}
	switches := s.switchList()
	byFabric := map[string]api.FabricProfile{}
	for _, p := range profiles {
		byFabric[p.Fabric] = p
	}
	for _, h := range hosts {
		for _, hp := range h.Ports {
			if !cableInUse(h, hp) {
				continue
			}
			var inv *api.Port
			for i := range h.inv.Ports {
				if h.inv.Ports[i].RdmaDev == hp.RdmaDev && h.inv.Ports[i].PortNum == hp.PortNum {
					inv = &h.inv.Ports[i]
				}
			}
			view := hostPortView{host: h.Name, port: hp}
			if inv != nil {
				view.settings, view.mtu = inv.Settings, inv.MTU
			}
			swName := resolveSwitchName(switches, hp.LLDPSwitch)
			var spc *switchPortCfg
			vlan := ""
			if swName != "" && hp.LLDPPort != "" {
				key := strings.ToLower(swName + "|" + hp.LLDPPort)
				spc, vlan = qos[key], vlans[key]
			}
			fabric := hp.Fabric
			if fabric == "" {
				fabric = vlan
			}
			cc := checkCable(view, fabric, swName, hp.LLDPPort, spc, vlan, byFabric[fabric], fabric != "" && hasProfile(byFabric, fabric))
			if !cc.OK {
				fc.Issues++
			}
			fc.Cables = append(fc.Cables, cc)
		}
	}
	sort.Slice(fc.Cables, func(i, j int) bool {
		a, b := fc.Cables[i], fc.Cables[j]
		if a.Fabric != b.Fabric {
			return a.Fabric < b.Fabric
		}
		if a.Host != b.Host {
			return strings.ToLower(a.Host) < strings.ToLower(b.Host)
		}
		return a.Netdev < b.Netdev
	})
	return fc
}

func hasProfile(m map[string]api.FabricProfile, fabric string) bool {
	_, ok := m[fabric]
	return ok
}

// checkCable compares one cable with its fabric profile. Without a profile
// only consistency is checked: the two ends must agree on speed and on PFC,
// the VLAN must match the fabric label, the link must be up, and the MTUs
// must carry a 4096-byte RDMA MTU.
func checkCable(v hostPortView, fabric, swName, swPort string, spc *switchPortCfg, vlan string, prof api.FabricProfile, hasProf bool) api.CableCheck {
	ip := ""
	for _, a := range v.port.Addrs {
		if pip, _, err := net.ParseCIDR(a); err == nil {
			ip = pip.String()
			break
		}
	}
	cc := api.CableCheck{Host: v.host, Netdev: v.port.Netdev, Label: v.port.Label, IP: ip, Fabric: fabric, Switch: swName, SwitchPort: swPort, OK: true, Checks: []api.SettingCheck{}}
	add := func(c api.SettingCheck) {
		if c.State == "bad" {
			cc.OK = false
		}
		cc.Checks = append(cc.Checks, c)
	}
	num := func(n int) string {
		if n <= 0 {
			return ""
		}
		return strconv.Itoa(n)
	}
	// Link.
	{
		c := api.SettingCheck{Name: "Link", Want: "ACTIVE, cabled to a known switch port", Host: v.port.State, State: "ok"}
		switch {
		case v.port.State != "ACTIVE":
			c.State, c.Note = "bad", "host port is not ACTIVE"
		case swPort == "":
			c.State, c.Note = "bad", "no switch port: LLDP has not matched this port to a configured switch"
		case spc != nil && spc.AdminUp != nil && !*spc.AdminUp:
			c.Switch, c.State, c.Note = "admin down", "bad", "switch port is administratively down"
		case spc != nil && spc.AdminUp != nil:
			c.Switch = "up"
		}
		add(c)
	}
	// Speed: both ends agree, and match the profile when it says.
	{
		c := api.SettingCheck{Name: "Speed", Host: num(v.port.SpeedMbps), State: "unknown"}
		if spc != nil && spc.SpeedMbps > 0 {
			c.Switch = num(spc.SpeedMbps)
		}
		want := 0
		if hasProf {
			want = prof.SpeedMbps
		}
		c.Want = num(want)
		switch {
		case c.Host != "" && c.Switch != "" && c.Host != c.Switch:
			c.State, c.Note = "bad", "the two ends negotiated different speeds"
		case want > 0 && c.Host != "" && v.port.SpeedMbps != want:
			c.State, c.Note = "bad", fmt.Sprintf("host port runs at %d Mbps, profile wants %d", v.port.SpeedMbps, want)
		case want > 0 && c.Switch != "" && spc.SpeedMbps != want:
			c.State, c.Note = "bad", fmt.Sprintf("switch port runs at %d Mbps, profile wants %d", spc.SpeedMbps, want)
		case c.Host != "" && (c.Switch != "" || want == 0):
			c.State = "ok"
		}
		add(c)
	}
	// MTU on both ends: at least the profile value, and enough for RDMA MTU 4096 regardless.
	{
		wantHost, wantSwitch := 4200, 4200
		if hasProf {
			wantHost, wantSwitch = max(prof.HostMTU, 4200), max(prof.SwitchMTU, 4200)
		}
		c := api.SettingCheck{Name: "MTU", Want: fmt.Sprintf("host >= %d, switch >= %d", wantHost, wantSwitch), Host: num(v.mtu), State: "unknown"}
		defer func() {
			// PCIe: the NIC's link must be what the card supports and wide
			// enough for the port speed; a downtrained link (reseat the card,
			// a bad riser) or a narrow slot caps every test on this port.
			pc := api.SettingCheck{Name: "PCIe", Want: "link at the card's maximum, usable bandwidth >= port speed", State: "unknown", Note: "agent has not reported the PCIe link (older agent)"}
			if l := v.port.PCIe; l != nil {
				pc.Host, pc.State, pc.Note = fmt.Sprintf("%s (%.0f Gbps usable)", l.String(), l.UsableGbps), "ok", ""
				switch {
				case l.Downtrained() && l.SlotGen > 0 && (l.SlotGen < l.MaxGen || l.SlotWidth < l.MaxWidth) && l.Gen >= min(l.SlotGen, l.MaxGen) && l.Width >= min(l.SlotWidth, l.MaxWidth):
					pc.State, pc.Note = "bad", fmt.Sprintf("the slot offers Gen%d x%d but the card supports Gen%d x%d: move the card to a faster slot", l.SlotGen, l.SlotWidth, l.MaxGen, l.MaxWidth)
				case l.Downtrained():
					pc.State, pc.Note = "bad", fmt.Sprintf("link trained to %s but the card supports Gen%d x%d: reseat the card or check the riser", l.String(), l.MaxGen, l.MaxWidth)
				case v.port.SpeedMbps > 0 && l.UsableGbps < float64(v.port.SpeedMbps)/1000*0.98:
					pc.State, pc.Note = "bad", fmt.Sprintf("%s carries about %.0f Gbps, below the %dG port: PCIe caps this port", l.String(), l.UsableGbps, v.port.SpeedMbps/1000)
				}
			}
			add(pc)
		}()
		if spc != nil && spc.MTU > 0 {
			c.Switch = num(spc.MTU)
		}
		switch {
		case v.mtu > 0 && v.mtu < wantHost:
			c.State, c.Note = "bad", fmt.Sprintf("host MTU %d is below %d", v.mtu, wantHost)
		case spc != nil && spc.MTU > 0 && spc.MTU < wantSwitch:
			c.State, c.Note = "bad", fmt.Sprintf("switch port MTU %d is below %d", spc.MTU, wantSwitch)
		case v.mtu > 0 && (spc == nil || spc.MTU > 0):
			c.State = "ok"
		}
		add(c)
	}
	// VLAN: the switch port's VLAN is the fabric.
	{
		want := fabric
		if hasProf && prof.VLAN != "" {
			want = prof.VLAN
		}
		c := api.SettingCheck{Name: "VLAN", Want: want, Switch: vlan, Host: v.port.Fabric, State: "unknown"}
		switch {
		case vlan == "":
		case want == "":
			c.State = "ok"
		case vlan != want:
			c.State, c.Note = "bad", fmt.Sprintf("switch port is in VLAN %s, fabric %s expects %s", vlan, fabric, want)
		default:
			c.State = "ok"
		}
		add(c)
	}
	// FEC, informational unless the profile is extended later.
	if spc != nil && spc.FEC != "" {
		add(api.SettingCheck{Name: "FEC", Switch: spc.FEC, State: "ok"})
	}
	// RoCE mode.
	{
		want := "v2"
		if hasProf && prof.RoCEMode != "" {
			want = prof.RoCEMode
		}
		c := api.SettingCheck{Name: "RoCE mode", Want: want, State: "unknown"}
		if v.settings != nil && v.settings.RoCEMode != "" {
			c.Host = v.settings.RoCEMode
			c.State = "ok"
			if c.Host != want {
				c.State, c.Note = "bad", fmt.Sprintf("host offers RoCE %s only", c.Host)
			}
		}
		add(c)
	}
	// PFC: the two ends must agree; the profile says which priority.
	{
		c := api.SettingCheck{Name: "PFC", State: "unknown"}
		if hasProf {
			if prof.PFCPriority >= 0 {
				c.Want = "priority " + strconv.Itoa(prof.PFCPriority)
			} else if prof.Mode == "lossy" {
				c.Want = "off"
			}
		}
		if spc != nil && spc.PFCRead {
			c.Switch = prioList(spc.PFC)
		}
		if v.settings != nil && v.settings.PFC != "" {
			c.Host = v.settings.PFC
		}
		switch {
		case c.Switch != "" && c.Host != "" && c.Switch != c.Host:
			c.State, c.Note = "bad", fmt.Sprintf("switch has PFC on %s, host on %s", nz(c.Switch), nz(c.Host))
		case c.Want == "priority "+strconv.Itoa(prof.PFCPriority) && hasProf && prof.PFCPriority >= 0:
			want := strconv.Itoa(prof.PFCPriority)
			switch {
			case c.Switch != "" && c.Switch != want:
				c.State, c.Note = "bad", fmt.Sprintf("switch has PFC %s, profile wants priority %s", nz(c.Switch), want)
			case c.Host != "" && c.Host != want:
				c.State, c.Note = "bad", fmt.Sprintf("host has PFC %s, profile wants priority %s", nz(c.Host), want)
			case c.Switch != "" || c.Host != "":
				c.State = "ok"
			}
		case c.Want == "off":
			switch {
			case c.Switch != "" && c.Switch != "none":
				c.State, c.Note = "bad", "switch has PFC on "+c.Switch+", profile is lossy"
			case c.Host != "" && c.Host != "none":
				c.State, c.Note = "bad", "host has PFC on "+c.Host+", profile is lossy"
			case c.Switch != "" || c.Host != "":
				c.State = "ok"
			}
		case c.Switch != "" || c.Host != "":
			c.State = "ok"
		}
		add(c)
	}
	// QoS map on the switch port: without one every DSCP lands in queue 0.
	{
		c := api.SettingCheck{Name: "Switch QoS map", State: "unknown"}
		if hasProf && prof.RoCEDSCP >= 0 {
			c.Want = fmt.Sprintf("DSCP %d mapped to the RoCE queue", prof.RoCEDSCP)
		}
		if spc != nil {
			if spc.DSCPMap != "" {
				c.Switch = "dscp map " + spc.DSCPMap
				c.State = "ok"
			} else if spc.PFCRead || spc.MTU > 0 {
				c.Switch = "none (queue 0)"
				c.State = "ok"
				if hasProf && prof.RoCEDSCP >= 0 {
					c.State, c.Note = "bad", "no DSCP-to-queue map on the switch port: RoCE traffic lands in queue 0"
				}
			}
		}
		add(c)
	}
	// Host trust and DSCP map.
	if hasProf && prof.Trust != "" {
		c := api.SettingCheck{Name: "Host trust", Want: prof.Trust, State: "unknown"}
		if v.settings != nil && v.settings.Trust != "" {
			c.Host = v.settings.Trust
			if v.settings.DSCPPrio != "" {
				c.Host += " (" + v.settings.DSCPPrio + ")"
			}
			c.State = "ok"
			if v.settings.Trust != prof.Trust {
				c.State, c.Note = "bad", fmt.Sprintf("host trusts %s, profile wants %s", v.settings.Trust, prof.Trust)
			} else if prof.Trust == "dscp" && prof.RoCEDSCP >= 0 && prof.PFCPriority >= 0 && v.settings.DSCPPrio != "" &&
				!strings.Contains(","+v.settings.DSCPPrio+",", fmt.Sprintf(",%d:%d,", prof.RoCEDSCP, prof.PFCPriority)) {
				c.State, c.Note = "bad", fmt.Sprintf("host does not map DSCP %d to priority %d", prof.RoCEDSCP, prof.PFCPriority)
			}
		}
		add(c)
	}
	// ECN: switch queue marks, host NIC reacts.
	{
		c := api.SettingCheck{Name: "ECN", State: "unknown"}
		if hasProf && prof.ECNQueue >= 0 {
			c.Want = fmt.Sprintf("switch marks on queue %d", prof.ECNQueue)
			if prof.HostECN {
				c.Want += fmt.Sprintf(", host np and rp on priority %d", prof.PFCPriority)
			}
		}
		if spc != nil {
			if q, ok := spc.Queues[max(prof.ECNQueue, 0)]; ok && hasProf && prof.ECNQueue >= 0 {
				switch {
				case q.WREDProfile != "":
					c.Switch = "wred " + q.WREDProfile
				case q.ECNMarked > 0:
					c.Switch = fmt.Sprintf("marking (%.0f marked)", q.ECNMarked)
				default:
					c.Switch = "no marks seen"
				}
			}
		}
		if v.settings != nil && (v.settings.ECNNp != "" || v.settings.ECNRp != "") {
			c.Host = fmt.Sprintf("np %s, rp %s", nz(v.settings.ECNNp), nz(v.settings.ECNRp))
			if v.settings.CNPDSCP >= 0 {
				c.Host += fmt.Sprintf(", cnp dscp %d", v.settings.CNPDSCP)
			}
		}
		if hasProf && prof.HostECN && prof.PFCPriority >= 0 && v.settings != nil && v.settings.ECNNp != "" {
			p := strconv.Itoa(prof.PFCPriority)
			if !hasPrio(v.settings.ECNNp, p) || !hasPrio(v.settings.ECNRp, p) {
				c.State, c.Note = "bad", fmt.Sprintf("host ECN is not on for priority %s (np %s, rp %s)", p, nz(v.settings.ECNNp), nz(v.settings.ECNRp))
			} else if prof.CNPDSCP >= 0 && v.settings.CNPDSCP >= 0 && v.settings.CNPDSCP != prof.CNPDSCP {
				c.State, c.Note = "bad", fmt.Sprintf("host sends CNPs with DSCP %d, profile wants %d", v.settings.CNPDSCP, prof.CNPDSCP)
			} else {
				c.State = "ok"
			}
		} else if c.Host != "" || c.Switch != "" {
			c.State = "ok"
		}
		add(c)
	}
	// Queue drops and pauses: lifetime counters the switch keeps; a lossless
	// profile treats drops on the RoCE queue as a fault.
	if spc != nil {
		cc.Counters = map[string]float64{}
		var drops float64
		for q, qs := range spc.Queues {
			if qs.Dropped > 0 {
				cc.Counters[fmt.Sprintf("queue %d dropped", q)] = qs.Dropped
				drops += qs.Dropped
			}
			if qs.ECNMarked > 0 {
				cc.Counters[fmt.Sprintf("queue %d ecn marked", q)] = qs.ECNMarked
			}
			if qs.WREDDropped > 0 {
				cc.Counters[fmt.Sprintf("queue %d wred dropped", q)] = qs.WREDDropped
			}
		}
		if spc.PauseRead {
			cc.Counters["pause rx"] = spc.PauseRx
			cc.Counters["pause tx"] = spc.PauseTx
		}
		if hasProf && prof.Mode == "lossless" && drops > 0 {
			add(api.SettingCheck{Name: "Drops", Want: "none on a lossless fabric", Switch: fmt.Sprintf("%.0f packets dropped", drops), State: "bad", Note: "the switch dropped packets on this port; a lossless fabric pauses instead"})
		}
	}
	return cc
}

func nz(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func hasPrio(list, p string) bool {
	for _, x := range strings.Split(list, ",") {
		if strings.TrimSpace(x) == p {
			return true
		}
	}
	return false
}

func prioList(on []int) string {
	if len(on) == 0 {
		return "none"
	}
	parts := make([]string, len(on))
	for i, n := range on {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ",")
}

// cableInUse says whether a host port is one a test can use and so one the
// fabric check should hold to a profile: it has an address, a label or a
// fabric set, its link is up, and it is not the twin PCIe function of a
// configured port on the same cable (the Sparks' ConnectX-7 shows its one
// port twice; the switch sees both on the same switch port, and the half
// with no address sits at the default MTU and must not raise an alert).
func cableInUse(h hostRec, hp api.HostPort) bool {
	if len(hp.Addrs) == 0 && hp.Label == "" && hp.Fabric == "" {
		return false
	}
	if hp.State != "" && hp.State != "ACTIVE" {
		return false
	}
	if hp.LLDPSwitch != "" && hp.LLDPPort != "" && len(hp.Addrs) == 0 {
		for _, o := range h.Ports {
			if o.Netdev != hp.Netdev && len(o.Addrs) > 0 && strings.EqualFold(o.LLDPSwitch, hp.LLDPSwitch) && strings.EqualFold(o.LLDPPort, hp.LLDPPort) {
				return false
			}
		}
	}
	return true
}

// fabricAlerts opens a fabric_mismatch alert per failing cable, resolves
// the ones that pass again, and closes alerts on cables the check no
// longer lists (a port that lost its address, or an unused twin half).
func (s *Server) fabricAlerts(ctx context.Context, fc api.FabricCheck) {
	listed := map[string]bool{}
	for _, c := range fc.Cables {
		port := c.Netdev
		if c.Label != "" {
			port = c.Label
		}
		listed[c.Host+"|"+port] = true
	}
	if open, err := s.store.Alerts(ctx, true, 1000); err == nil {
		for _, a := range open {
			if a.Kind == "fabric_mismatch" && !listed[a.Host+"|"+a.Port] {
				s.alerts.resolve(ctx, "fabric_mismatch", a.Host, a.Port)
			}
		}
	}
	for _, c := range fc.Cables {
		port := c.Netdev
		if c.Label != "" {
			port = c.Label
		}
		if c.OK {
			s.alerts.resolve(ctx, "fabric_mismatch", c.Host, port)
			continue
		}
		var bad []string
		for _, ch := range c.Checks {
			if ch.State == "bad" {
				bad = append(bad, ch.Name+": "+ch.Note)
			}
		}
		s.alerts.open(ctx, api.Alert{Kind: "fabric_mismatch", Host: c.Host, Port: port, Message: fmt.Sprintf("%s %s (%s %s): %s", c.Host, port, c.Switch, c.SwitchPort, strings.Join(bad, "; "))})
	}
}

// fabricIssuesFor lists the failing checks on the cables a run uses, for
// the run's summary.
func (s *Server) fabricIssuesFor(ctx context.Context, portIDs map[int64]bool) []string {
	fc := s.fabricCheck(ctx)
	hosts, err := s.store.Hosts(ctx)
	if err != nil {
		return nil
	}
	want := map[string]bool{} // host|netdev
	for _, h := range hosts {
		for _, p := range h.Ports {
			if portIDs[p.ID] {
				want[h.Name+"|"+p.Netdev] = true
			}
		}
	}
	var out []string
	for _, c := range fc.Cables {
		if !want[c.Host+"|"+c.Netdev] || c.OK {
			continue
		}
		for _, ch := range c.Checks {
			if ch.State == "bad" {
				out = append(out, fmt.Sprintf("%s %s: %s", c.Host, c.Netdev, ch.Note))
			}
		}
	}
	if len(out) > 12 {
		out = append(out[:12], fmt.Sprintf("and %d more", len(out)-12))
	}
	return out
}

// Routes.

func (s *Server) routesFabric(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/fabric-check", s.view(s.getFabricCheck))
	mux.HandleFunc("GET /api/v1/fabrics", s.view(s.listFabrics))
	mux.HandleFunc("PUT /api/v1/fabrics/{name}", s.admin(s.putFabric))
	mux.HandleFunc("DELETE /api/v1/fabrics/{name}", s.admin(s.deleteFabric))
	mux.HandleFunc("GET /api/v1/fabrics/presets", s.view(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]api.FabricProfile{"lossless": fabricPreset("lossless"), "lossy": fabricPreset("lossy")})
	}))
}

func (s *Server) getFabricCheck(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.fabricCheck(r.Context()))
}

func (s *Server) listFabrics(w http.ResponseWriter, r *http.Request) {
	p, err := s.store.FabricProfiles(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, p)
}

func (s *Server) putFabric(w http.ResponseWriter, r *http.Request) {
	var p api.FabricProfile
	if !readJSON(w, r, &p) {
		return
	}
	p.Fabric = strings.TrimSpace(r.PathValue("name"))
	if p.Fabric == "" || len(p.Fabric) > 64 {
		httpError(w, http.StatusBadRequest, "fabric name is required")
		return
	}
	switch p.Mode {
	case "", "lossless", "lossy":
	default:
		httpError(w, http.StatusBadRequest, "mode must be lossless, lossy or empty")
		return
	}
	if p.PFCPriority < -1 || p.PFCPriority > 7 || p.ECNQueue < -1 || p.ECNQueue > 7 || p.RoCEDSCP < -1 || p.RoCEDSCP > 63 || p.CNPDSCP < -1 || p.CNPDSCP > 63 {
		httpError(w, http.StatusBadRequest, "priorities are 0-7 (or -1 for off), DSCPs 0-63 (or -1 for unchecked)")
		return
	}
	if err := s.store.SaveFabricProfile(r.Context(), p); err != nil {
		serverError(w, err)
		return
	}
	s.store.Audit(r.Context(), "fabric.save", p)
	go s.pollFabricNow()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteFabric(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteFabricProfile(r.Context(), r.PathValue("name")); err != nil {
		serverError(w, err)
		return
	}
	s.store.Audit(r.Context(), "fabric.delete", map[string]string{"fabric": r.PathValue("name")})
	w.WriteHeader(http.StatusNoContent)
}

// pollFabricNow re-evaluates the alerts after a profile change.
func (s *Server) pollFabricNow() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s.fabricAlerts(ctx, s.fabricCheck(ctx))
}

// refreshQoS reads the switch side and keeps it for the check.
func (s *Server) refreshQoS(ctx context.Context) {
	ports := map[string][]string{}
	if hosts, err := s.store.Hosts(ctx); err == nil {
		s.fillLLDP(ctx, hosts)
		switches := s.switchList()
		for _, h := range hosts {
			for _, p := range h.Ports {
				if name := resolveSwitchName(switches, p.LLDPSwitch); name != "" && p.LLDPPort != "" {
					ports[name] = append(ports[name], p.LLDPPort)
				}
			}
		}
	}
	qos, err := s.switchQoS(ctx, ports)
	s.mu.Lock()
	if err != nil {
		s.qosErr = err.Error()
	} else {
		s.qosErr = ""
	}
	if len(qos) > 0 || err == nil {
		s.qos = qos
	}
	s.mu.Unlock()
	if err != nil {
		s.mu.Lock()
		logged := s.qosLogged == err.Error()
		s.qosLogged = err.Error()
		s.mu.Unlock()
		if !logged {
			log.Printf("switch qos: %v", err)
		}
	}
}

// dot1p is the PFC priority of a QoS series: gnmic labels it
// pfc_priority_dot1p on this SONiC, plain dot1p on older builds.
func dot1p(labels map[string]string) string {
	for _, k := range []string{"pfc_priority_dot1p", "priority_dot1p", "dot1p"} {
		if v := labels[k]; v != "" {
			return v
		}
	}
	return ""
}

// qosCounterRe picks the gnmic series of the per-port QoS subscription that
// matter for a leg: queue drops and ECN marks, PFC pause frames.
const qosCounterRe = `srnet_gnmi_.*qos.*(dropped_pkts|ecn_marked_pkts|pause_frames_rx|pause_frames_tx)$`

// qosDeltas is the growth of one switch port's queue drops, ECN marks and
// pause frames between from and to, keyed "<port> queue 3 dropped_pkts" or
// "<port> prio 3 pause_frames_rx". Best effort: an empty map when the
// switch streams no QoS counters.
func (m *metrics) qosDeltas(ctx context.Context, sw SwitchConfig, port string, from, to time.Time) map[string]float64 {
	out := map[string]float64{}
	if m == nil || m.vmURL == "" {
		return out
	}
	src := fmt.Sprintf("(?i)(%s|%s|%s:%d)", regexp.QuoteMeta(sw.Name), regexp.QuoteMeta(sw.Address), regexp.QuoteMeta(sw.Address), sw.GNMIPort)
	// gnmic names the list key interface_interface_id on this SONiC (older
	// builds wrote interface_id); ask for both.
	sel1 := fmt.Sprintf(`{__name__=~"%s",source=~"%s",interface_interface_id="%s"}`, qosCounterRe, src, port)
	sel2 := fmt.Sprintf(`{__name__=~"%s",source=~"%s",interface_id="%s"}`, qosCounterRe, src, port)
	snap := func(t time.Time) map[string]float64 {
		rows, err := m.vmQuery(ctx, fmt.Sprintf(`last_over_time(%s[90s] @ %d) or last_over_time(%s[90s] @ %d)`, sel1, t.Unix(), sel2, t.Unix()))
		if err != nil {
			return nil
		}
		r := map[string]float64{}
		for _, row := range rows {
			name := row.Labels["__name__"]
			leaf := name[strings.LastIndex(name, "_")+1:]
			for _, l := range []string{"dropped_pkts", "ecn_marked_pkts", "pause_frames_rx", "pause_frames_tx"} {
				if strings.HasSuffix(name, l) {
					leaf = l
				}
			}
			where := ""
			q := row.Labels["queue_name"]
			if q == "" {
				q = row.Labels["name"]
			}
			if strings.Contains(q, ":") {
				where = "queue " + q[strings.LastIndex(q, ":")+1:] + " "
			} else if p := dot1p(row.Labels); p != "" {
				where = "prio " + p + " "
			}
			r[port+" "+where+leaf] += row.Value
		}
		return r
	}
	a, b := snap(from), snap(to)
	for k, v1 := range b {
		if v0, ok := a[k]; ok && v1 > v0+0.5 {
			out[k] = float64(int64(v1 - v0 + 0.5))
		}
	}
	return out
}
