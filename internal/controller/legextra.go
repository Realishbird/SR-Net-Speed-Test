package controller

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Realishbird/SR-Net-Speed-Test/internal/api"
)

// switchCounters are the per-port counters a run keeps from the switch. gnmic
// and the SNMP poller both publish them under their own prefix.
var switchCounters = []string{"in_octets", "out_octets", "in_discards", "out_discards", "in_errors", "out_errors", "in_fcs_errors"}

// resolveSwitchName maps the chassis name a host's lldpd reported to a
// configured switch: exact name or address first, then a prefix either way
// (SONiC's hostname is often the model or a site name), then the only switch
// configured. Empty when nothing fits.
func resolveSwitchName(switches []SwitchConfig, chassis string) string {
	if chassis == "" {
		return ""
	}
	for _, sw := range switches {
		if strings.EqualFold(sw.Name, chassis) || strings.EqualFold(sw.Address, chassis) {
			return sw.Name
		}
	}
	for _, sw := range switches {
		a, b := strings.ToLower(sw.Name), strings.ToLower(chassis)
		if a != "" && (strings.HasPrefix(a, b) || strings.HasPrefix(b, a)) {
			return sw.Name
		}
	}
	if len(switches) == 1 {
		return switches[0].Name
	}
	return ""
}

// switchSeries returns a function that selects one counter of one switch
// port in VictoriaMetrics: gnmic names series after the whole gNMI path and
// labels them with its lower-cased target name or host:port, the SNMP poller
// uses srnet_switch_<counter> with the configured switch name.
func switchSeries(sw SwitchConfig, gnmi bool, port string) func(counter string) string {
	if gnmi {
		src := fmt.Sprintf("(?i)(%s|%s|%s:%d)", regexp.QuoteMeta(sw.Name), regexp.QuoteMeta(sw.Address), regexp.QuoteMeta(sw.Address), sw.GNMIPort)
		return func(c string) string {
			return gnmiMetric(c, fmt.Sprintf(`source=~"%s"`, src), fmt.Sprintf(`interface_name="%s"`, port))
		}
	}
	return func(c string) string { return fmt.Sprintf(`srnet_switch_%s{switch="%s",port="%s"}`, c, sw.Name, port) }
}

// counterSnap is one switch sample: the counter value and when it was taken.
type counterSnap struct {
	value float64
	at    float64 // unix seconds
}

// switchCountersAt returns the last sample of each switch counter on one port
// at or before t, looking back 90 s (three 30 s samples), keyed by counter
// name. Switch samples are not aligned to the test, so the caller divides by
// the sample spacing rather than the test length.
func (m *metrics) switchCountersAt(ctx context.Context, series func(string) string, t time.Time) (map[string]counterSnap, error) {
	out := map[string]counterSnap{}
	ts, err := m.vmQuery(ctx, fmt.Sprintf(`tlast_over_time(%s[90s] @ %d)`, series("in_octets"), t.Unix()))
	if err != nil {
		return nil, err
	}
	if len(ts) == 0 {
		return out, nil
	}
	at := ts[0].Value
	for _, c := range switchCounters {
		rows, err := m.vmQuery(ctx, fmt.Sprintf(`last_over_time(%s[90s] @ %d)`, series(c), t.Unix()))
		if err != nil {
			return nil, err
		}
		if len(rows) > 0 {
			out[c] = counterSnap{value: rows[0].Value, at: at}
		}
	}
	return out, nil
}

// legSwitch fills in which switch ports a leg crossed and what the switch saw
// there during the test: the rate along the data flow, from the switch's own
// octet counters, and any discards or errors that grew. It is best effort; a
// leg with no LLDP mapping or no switch telemetry simply has no switch figures.
func (o *Orchestrator) legSwitch(ar *activeRun, p *legPlan, from, to time.Time) *api.LegSwitch {
	if o.switches == nil {
		return nil
	}
	switches := o.switches()
	cName := resolveSwitchName(switches, p.client.LLDPSwitch)
	sName := resolveSwitchName(switches, p.server.LLDPSwitch)
	ls := &api.LegSwitch{}
	if cName != "" && p.client.LLDPPort != "" {
		ls.ClientPort = cName + " " + p.client.LLDPPort
	}
	if sName != "" && p.server.LLDPPort != "" {
		ls.ServerPort = sName + " " + p.server.LLDPPort
	}
	if ls.ClientPort == "" && ls.ServerPort == "" {
		return nil
	}
	if o.metrics == nil || o.metrics.vmURL == "" {
		return ls
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	find := func(name string) (SwitchConfig, bool) {
		for _, sw := range switches {
			if sw.Name == name {
				return sw, true
			}
		}
		return SwitchConfig{}, false
	}
	// Data flows client -> server for write, send, tcp and udp, the other way
	// for read. On the server's switch port that is "out", on the client's
	// "in". Bidirectional adds both. The rate is taken on the server port when
	// it is mapped, else on the client port, never both.
	toServer := !strings.HasPrefix(ar.spec.Test, "read")
	deltas := map[string]float64{}
	rated := false
	for _, side := range []struct {
		name, port string
		server     bool
	}{{sName, p.server.LLDPPort, true}, {cName, p.client.LLDPPort, false}} {
		sw, ok := find(side.name)
		if !ok || side.port == "" {
			continue
		}
		series := switchSeries(sw, sw.GNMIUser != "", side.port)
		s0, err := o.metrics.switchCountersAt(ctx, series, from)
		var s1 map[string]counterSnap
		if err == nil {
			s1, err = o.metrics.switchCountersAt(ctx, series, to)
		}
		if err != nil {
			ls.Note = "switch counters unavailable: " + err.Error()
			break
		}
		if len(s0) == 0 || len(s1) == 0 {
			if ls.Note == "" {
				ls.Note = "no switch samples for " + side.name + " " + side.port
			}
			continue
		}
		delta := func(c string) (float64, float64) { // growth, span in seconds
			a, b := s0[c], s1[c]
			if b.at <= a.at || b.value < a.value {
				return 0, 0
			}
			return b.value - a.value, b.at - a.at
		}
		if !rated {
			in, span := delta("in_octets")
			out, _ := delta("out_octets")
			var bytes float64
			switch {
			case ar.spec.Bidir:
				bytes = in + out
			case side.server == toServer:
				bytes = out
			default:
				bytes = in
			}
			if span > 0 {
				ls.Gbps = bytes * 8 / span / 1e9
				rated = true
			} else if ls.Note == "" {
				ls.Note = "switch took no new sample during the test"
			}
		}
		for _, c := range switchCounters[2:] {
			if v, _ := delta(c); v >= 0.5 {
				deltas[side.port+" "+c] = float64(int64(v + 0.5))
			}
		}
	}
	if len(deltas) > 0 {
		ls.Deltas = deltas
	}
	return ls
}

// legCPU is the client's and server's CPU use during the leg.
func (o *Orchestrator) legCPU(p *legPlan, from, to time.Time) *api.LegCPU {
	if o.metrics == nil {
		return nil
	}
	ca, cp, okC := o.metrics.hostCPU(p.client.Host, from, to)
	sa, sp, okS := o.metrics.hostCPU(p.server.Host, from, to)
	if !okC && !okS {
		return nil
	}
	return &api.LegCPU{ClientAvg: ca, ClientPeak: cp, ServerAvg: sa, ServerPeak: sp}
}

// switchDeltas renders a leg's switch counter growth as "port counter=n" pairs.
func switchDeltas(m map[string]float64) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s=%.0f", k, m[k])
	}
	return strings.Join(parts, ";")
}

// switchPath renders a leg's switch ports as "client port > server port".
func switchPath(ls *api.LegSwitch) string {
	if ls == nil {
		return ""
	}
	c, s := ls.ClientPort, ls.ServerPort
	if c == "" {
		c = "?"
	}
	if s == "" {
		s = "?"
	}
	return c + " > " + s
}
