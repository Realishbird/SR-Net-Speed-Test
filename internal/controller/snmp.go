package controller

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gosnmp/gosnmp"
)

// IF-MIB columns the collector walks on each switch.
var ifColumns = map[string]string{
	"1.3.6.1.2.1.31.1.1.1.1":  "name",        // ifName
	"1.3.6.1.2.1.31.1.1.1.15": "speed_mbps",  // ifHighSpeed
	"1.3.6.1.2.1.2.2.1.8":     "oper_status", // ifOperStatus (1 = up)
	"1.3.6.1.2.1.31.1.1.1.6":  "in_octets",   // ifHCInOctets
	"1.3.6.1.2.1.31.1.1.1.10": "out_octets",  // ifHCOutOctets
	"1.3.6.1.2.1.2.2.1.14":    "in_errors",
	"1.3.6.1.2.1.2.2.1.20":    "out_errors",
	"1.3.6.1.2.1.2.2.1.13":    "in_discards",
	"1.3.6.1.2.1.2.2.1.19":    "out_discards",
}

// ifOrder is the walk order: the octet counters first, back to back, so
// their read time is known to within a fraction of a second.
var ifOrder = []string{"in_octets", "out_octets", "name", "speed_mbps", "oper_status", "in_errors", "out_errors", "in_discards", "out_discards"}

// snmpPrev is the last octet reading of one port: the values and when they
// were first seen. For the fast poll it also keeps when the port was last
// read, the recent gaps between counter changes, which give the switch's own
// counter refresh period, and the recent changes themselves.
type snmpPrev struct {
	t       time.Time
	in, out float64
	polled  time.Time
	gaps    []float64   // seconds between recent changes, newest last
	hist    []snmpPoint // recent changes seen within a poll of happening, oldest first
}

// snmpPoint is one change of a port's octet counters: the values and when
// the fast poll first saw them.
type snmpPoint struct {
	t       time.Time
	in, out float64
}

// The fast poll's rate is a straight-line fit over the counter changes of the
// last snmpRateWindow, once they span snmpRateMinSpan.
const (
	snmpRateWindow  = 20 * time.Second
	snmpRateMinSpan = 12 * time.Second
)

// fastRate fits a line through the recent changes and returns its slope in
// bytes per second, in and out. The Z9864F's SNMP agent hands out counters it
// refreshed some seconds before, so one step between two polls holds one to
// three of the switch's own refreshes: in run 17 a rate per step swung from
// 198 to 588 Gbps on a port gNMI read at 399. A line fitted through 12 to 20 s
// of the same changes stays within 10% of it.
func (p snmpPrev) fastRate() (in, out float64, ok bool) {
	n := len(p.hist)
	if n < 2 {
		return 0, 0, false
	}
	last := p.hist[n-1].t
	from := 0
	for k := n - 2; k >= 0; k-- {
		if last.Sub(p.hist[k].t) >= snmpRateWindow {
			from = k
			break
		}
	}
	pts := p.hist[from:]
	if last.Sub(pts[0].t) < snmpRateMinSpan {
		return 0, 0, false
	}
	var mt, mi, mo float64
	for _, q := range pts {
		mt += q.t.Sub(last).Seconds()
		mi += q.in - pts[0].in
		mo += q.out - pts[0].out
	}
	k := float64(len(pts))
	mt, mi, mo = mt/k, mi/k, mo/k
	var den, ni, no float64
	for _, q := range pts {
		dt := q.t.Sub(last).Seconds() - mt
		den += dt * dt
		ni += dt * (q.in - pts[0].in - mi)
		no += dt * (q.out - pts[0].out - mo)
	}
	if den == 0 {
		return 0, 0, false
	}
	return ni / den, no / den, true
}

// refreshPeriod is the mean of the recent gaps between counter changes, or
// 0 until three are known.
func (p snmpPrev) refreshPeriod() float64 {
	if len(p.gaps) < 3 {
		return 0
	}
	sum := 0.0
	for _, g := range p.gaps {
		sum += g
	}
	return sum / float64(len(p.gaps))
}

// snmpCollector polls every switch that has an SNMP community and writes
// srnet_switch_* metrics. It is the fallback where gNMI is not enabled; gnmic
// (configured by WriteGnmicConfig) is the primary path. The switch list can be
// replaced at runtime with set(), which also triggers an immediate poll.
type snmpCollector struct {
	m        *metrics
	interval time.Duration
	kick     chan struct{}

	// fast is the interval of the quick poll of the ports that are up while
	// a run is active (0 = off); active says whether a run is on.
	fast   time.Duration
	active func() bool

	mu       sync.Mutex
	switches []SwitchConfig
	prev     map[string]snmpPrev   // walk|fast/switch/ifIndex
	status   map[string]snmpStatus // switch name -> last poll
	upIdx    map[string][]snmpPort // switch name -> ports that were up at the last full walk
}

// snmpPort is a port the fast poll reads: its ifIndex, name and speed.
type snmpPort struct {
	Index string
	Name  string
	Mbps  int64
}

// snmpStatus is the outcome of the last poll of one switch.
type snmpStatus struct {
	At    time.Time
	Ports int
	Err   string
}

func newSNMPCollector(sw []SwitchConfig, m *metrics) *snmpCollector {
	c := &snmpCollector{m: m, interval: 30 * time.Second, kick: make(chan struct{}, 1), prev: map[string]snmpPrev{}, status: map[string]snmpStatus{}, upIdx: map[string][]snmpPort{}}
	c.set(sw)
	return c
}

// set replaces the polled switches (only those with an address and a
// community are kept) and wakes the loop so the change shows within seconds.
func (c *snmpCollector) set(sw []SwitchConfig) {
	var withSNMP []SwitchConfig
	keep := map[string]bool{}
	for _, s := range sw {
		if s.SNMPCommunity != "" && s.Address != "" {
			withSNMP = append(withSNMP, s)
			keep[s.Name] = true
		}
	}
	c.mu.Lock()
	c.switches = withSNMP
	for name := range c.status {
		if !keep[name] {
			delete(c.status, name)
		}
	}
	c.mu.Unlock()
	select {
	case c.kick <- struct{}{}:
	default:
	}
}

func (c *snmpCollector) current() []SwitchConfig {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]SwitchConfig(nil), c.switches...)
}

// statuses returns the last poll outcome per switch name.
func (c *snmpCollector) statuses() map[string]snmpStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]snmpStatus, len(c.status))
	for k, v := range c.status {
		out[k] = v
	}
	return out
}

// pollNow polls one switch immediately, records the outcome and pushes its
// samples. It is what the "test" action in the UI runs.
func (c *snmpCollector) pollNow(sw SwitchConfig) snmpStatus {
	st := snmpStatus{At: time.Now()}
	lines, ports, err := c.poll(sw)
	st.Ports = ports
	if err != nil {
		st.Err = err.Error()
	} else {
		c.m.push(lines)
	}
	c.mu.Lock()
	c.status[sw.Name] = st
	c.mu.Unlock()
	return st
}

func (c *snmpCollector) loop(ctx context.Context) {
	logged := -1
	for {
		list := c.current()
		if len(list) != logged {
			log.Printf("snmp: polling %d switch(es) every %s", len(list), c.interval)
			logged = len(list)
		}
		for _, sw := range list {
			if st := c.pollNow(sw); st.Err != "" {
				log.Printf("snmp %s: %s", sw.Name, st.Err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(c.interval):
		case <-c.kick:
		}
	}
}

func (c *snmpCollector) poll(sw SwitchConfig) ([]byte, int, error) {
	g, err := c.connect(sw)
	if err != nil {
		return nil, 0, err
	}
	defer g.Conn.Close()

	colOID := map[string]string{}
	for oid, col := range ifColumns {
		colOID[col] = oid
	}
	rows := map[string]map[string]any{} // ifIndex -> column -> value
	start := time.Now()
	var octetsAt time.Time
	for _, col := range ifOrder {
		oid := colOID[col]
		walk := g.BulkWalk
		if g.Version == gosnmp.Version1 {
			walk = g.Walk
		}
		err := walk(oid, func(pdu gosnmp.SnmpPDU) error {
			idx := pdu.Name[strings.LastIndex(pdu.Name, ".")+1:]
			if rows[idx] == nil {
				rows[idx] = map[string]any{}
			}
			rows[idx][col] = snmpValue(pdu.Value)
			return nil
		})
		if err != nil {
			return nil, 0, fmt.Errorf("walk %s: %w", col, err)
		}
		if col == "out_octets" {
			// The octet columns were read between start and now.
			octetsAt = start.Add(time.Since(start) / 2)
		}
	}
	var up []snmpPort
	for idx, r := range rows {
		r["_at"] = octetsAt
		if name, _ := r["name"].(string); name != "" {
			if st, _ := r["oper_status"].(int64); st == 1 {
				mbps, _ := r["speed_mbps"].(int64)
				up = append(up, snmpPort{idx, name, mbps})
			}
		}
	}
	sort.Slice(up, func(i, j int) bool { return up[i].Name < up[j].Name })
	lines, ports := c.render(sw, rows, time.Now(), "walk")
	c.mu.Lock()
	c.upIdx[sw.Name] = up
	c.mu.Unlock()
	return lines, ports, nil
}

func snmpValue(v any) any {
	switch x := v.(type) {
	case []byte:
		return string(x)
	case string:
		return x
	default:
		return gosnmp.ToBigInt(v).Int64()
	}
}

// fastColumns are the counters the quick poll reads per port.
var fastColumns = []string{"in_octets", "out_octets", "in_errors", "out_errors", "in_discards", "out_discards"}

// fastLoop polls only the ports that were up at the last full walk, every
// c.fast, while a run is active. Enterprise SONiC refuses gNMI sample
// intervals under 30 s, so this is how a run page gets 2 s switch counters.
func (c *snmpCollector) fastLoop(ctx context.Context) {
	if c.fast <= 0 {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(c.fast):
		}
		if c.active == nil || !c.active() {
			continue
		}
		for _, sw := range c.current() {
			lines, err := c.pollFast(sw)
			if err != nil {
				log.Printf("snmp %s fast poll: %v", sw.Name, err)
				continue
			}
			c.m.push(lines)
		}
	}
}

// pollFast GETs the fast columns for every up port of one switch in chunks.
func (c *snmpCollector) pollFast(sw SwitchConfig) ([]byte, error) {
	c.mu.Lock()
	ports := append([]snmpPort(nil), c.upIdx[sw.Name]...)
	c.mu.Unlock()
	if len(ports) == 0 {
		return nil, nil
	}
	g, err := c.connect(sw)
	if err != nil {
		return nil, err
	}
	defer g.Conn.Close()
	oids, byOID := fastOIDs(ports)
	rows := map[string]map[string]any{}
	for _, p := range ports {
		rows[p.Index] = map[string]any{"name": p.Name, "_mbps": p.Mbps}
	}
	// A chunk is 30 OIDs, five whole ports; each row is stamped with the
	// time its chunk came back.
	const chunk = 30
	for i := 0; i < len(oids); i += chunk {
		end := i + chunk
		if end > len(oids) {
			end = len(oids)
		}
		res, err := g.Get(oids[i:end])
		if err != nil {
			return nil, err
		}
		at := time.Now()
		for _, pdu := range res.Variables {
			if pdu.Type == gosnmp.NoSuchObject || pdu.Type == gosnmp.NoSuchInstance {
				continue
			}
			k, ok := byOID[strings.TrimPrefix(pdu.Name, ".")]
			if !ok {
				continue
			}
			rows[k.Index][k.Col] = snmpValue(pdu.Value)
			rows[k.Index]["_at"] = at
		}
	}
	lines, _ := c.render(sw, rows, time.Now(), "fast")
	return lines, nil
}

type snmpCell struct{ Index, Col string }

// fastOIDs builds the instance OIDs for the fast poll and a map back to
// (ifIndex, column).
func fastOIDs(ports []snmpPort) ([]string, map[string]snmpCell) {
	colOID := map[string]string{}
	for oid, col := range ifColumns {
		colOID[col] = oid
	}
	var oids []string
	back := map[string]snmpCell{}
	for _, p := range ports {
		for _, col := range fastColumns {
			oid := colOID[col] + "." + p.Index
			oids = append(oids, oid)
			back[oid] = snmpCell{p.Index, col}
		}
	}
	return oids, back
}

func (c *snmpCollector) connect(sw SwitchConfig) (*gosnmp.GoSNMP, error) {
	host, port := sw.Address, uint16(161)
	if h, p, ok := strings.Cut(sw.Address, ":"); ok {
		if n, err := strconv.Atoi(p); err == nil {
			host, port = h, uint16(n)
		}
	}
	g := &gosnmp.GoSNMP{Target: host, Port: port, Community: sw.SNMPCommunity, Version: gosnmp.Version2c, Timeout: 5 * time.Second, Retries: 1, MaxRepetitions: 50}
	if sw.SNMPVersion == "1" {
		g.Version = gosnmp.Version1
	}
	if err := g.Connect(); err != nil {
		return nil, err
	}
	return g, nil
}

// render turns walked rows into Prometheus lines, computing rates against
// the previous reading of each port. kind is "walk" for the full walk and
// "fast" for the quick poll during runs, and each keeps its own previous
// readings: sharing them gave rates over a fraction of a second between a
// fast read and a walk, which showed as 7 Tbps on a 100G port. A row may
// carry "_at", the time its octet counters were read, used instead of now.
//
// The switch's SNMP agent refreshes its counters every few seconds, so a
// poll every 2 s sees the counters jump in uneven steps. The fast poll
// therefore takes a rate only when the counters change, from a line fitted
// through the changes of the last 20 s (see fastRate). Rates never exceed
// the port's speed.
func (c *snmpCollector) render(sw SwitchConfig, rows map[string]map[string]any, now time.Time, kind string) ([]byte, int) {
	var buf bytes.Buffer
	ports := 0
	c.mu.Lock()
	defer c.mu.Unlock()
	for idx, r := range rows {
		name, _ := r["name"].(string)
		if name == "" {
			continue
		}
		ports++
		at := now
		if t, ok := r["_at"].(time.Time); ok && !t.IsZero() {
			at = t
		}
		ts := at.UnixMilli()
		labels := formatLabels(map[string]string{"switch": sw.Name, "port": name})
		num := func(col string) (float64, bool) {
			n, ok := r[col].(int64)
			return float64(n), ok
		}
		for _, col := range []string{"speed_mbps", "in_octets", "out_octets", "in_errors", "out_errors", "in_discards", "out_discards"} {
			if n, ok := num(col); ok {
				fmt.Fprintf(&buf, "srnet_switch_%s%s %g %d\n", col, labels, n, ts)
			}
		}
		if n, ok := num("oper_status"); ok {
			up := 0.0
			if n == 1 {
				up = 1
			}
			fmt.Fprintf(&buf, "srnet_switch_up%s %g %d\n", labels, up, ts)
		}
		in, okI := num("in_octets")
		out, okO := num("out_octets")
		if !okI || !okO {
			continue
		}
		limit := math.Inf(1)
		if n, ok := num("speed_mbps"); ok && n > 0 {
			limit = n / 1000
		} else if n, ok := num("_mbps"); ok && n > 0 {
			limit = n / 1000
		}
		emit := func(bytesIn, bytesOut, secs float64) {
			fmt.Fprintf(&buf, "srnet_switch_rx_gbps%s %g %d\n", labels, math.Min(bytesIn*8/secs/1e9, limit), ts)
			fmt.Fprintf(&buf, "srnet_switch_tx_gbps%s %g %d\n", labels, math.Min(bytesOut*8/secs/1e9, limit), ts)
		}
		key := kind + "/" + sw.Name + "/" + idx
		p, ok := c.prev[key]
		since := at.Sub(p.t).Seconds()
		switch {
		case !ok || in < p.in || out < p.out:
			// First reading, or the counters were reset.
			c.prev[key] = snmpPrev{t: at, in: in, out: out, polled: at}
		case since < 0.5:
			// Too close to the last reading to say anything.
		case kind != "fast":
			emit(in-p.in, out-p.out, since)
			c.prev[key] = snmpPrev{t: at, in: in, out: out}
		case in == p.in && out == p.out:
			// No change yet. Once a whole refresh period has gone by
			// without one, the port really is idle.
			p.polled = at
			c.prev[key] = p
			per := p.refreshPeriod()
			if per == 0 {
				per = 6
			}
			if since > 1.5*per {
				emit(0, 0, 1)
			}
		default:
			// The counters moved. The change is a point on the line only
			// if the port was read just before; after a pause between
			// runs nobody knows when it happened, so the line starts over.
			if since <= 15 {
				p.gaps = append(p.gaps, since)
				if len(p.gaps) > 16 {
					p.gaps = p.gaps[len(p.gaps)-16:]
				}
			}
			fresh := at.Sub(p.polled) <= 5*time.Second
			p.t, p.in, p.out, p.polled = at, in, out, at
			keep := p.hist[:0]
			for _, q := range p.hist {
				if fresh && at.Sub(q.t) <= snmpRateWindow+25*time.Second {
					keep = append(keep, q)
				}
			}
			p.hist = keep
			if fresh {
				p.hist = append(p.hist, snmpPoint{t: at, in: in, out: out})
			}
			if ri, ro, ok := p.fastRate(); ok {
				emit(math.Max(ri, 0), math.Max(ro, 0), 1)
			}
			c.prev[key] = p
		}
	}
	return buf.Bytes(), ports
}
