package controller

import (
	"bytes"
	"context"
	"fmt"
	"log"
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

type snmpPrev struct {
	t       time.Time
	in, out float64
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
	prev     map[string]snmpPrev   // switch/ifIndex
	status   map[string]snmpStatus // switch name -> last poll
	upIdx    map[string][]snmpPort // switch name -> ports that were up at the last full walk
}

// snmpPort is a port the fast poll reads: its ifIndex and name.
type snmpPort struct {
	Index string
	Name  string
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

	rows := map[string]map[string]any{} // ifIndex -> column -> value
	for oid, col := range ifColumns {
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
	}
	var up []snmpPort
	for idx, r := range rows {
		if name, _ := r["name"].(string); name != "" {
			if st, _ := r["oper_status"].(int64); st == 1 {
				up = append(up, snmpPort{idx, name})
			}
		}
	}
	sort.Slice(up, func(i, j int) bool { return up[i].Name < up[j].Name })
	lines, ports := c.render(sw, rows, time.Now())
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
		rows[p.Index] = map[string]any{"name": p.Name}
	}
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
		for _, pdu := range res.Variables {
			if pdu.Type == gosnmp.NoSuchObject || pdu.Type == gosnmp.NoSuchInstance {
				continue
			}
			k, ok := byOID[strings.TrimPrefix(pdu.Name, ".")]
			if !ok {
				continue
			}
			rows[k.Index][k.Col] = snmpValue(pdu.Value)
		}
	}
	lines, _ := c.render(sw, rows, time.Now())
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
// the previous reading of each port.
func (c *snmpCollector) render(sw SwitchConfig, rows map[string]map[string]any, now time.Time) ([]byte, int) {
	var buf bytes.Buffer
	ports := 0
	c.mu.Lock()
	defer c.mu.Unlock()
	ts := now.UnixMilli()
	for idx, r := range rows {
		name, _ := r["name"].(string)
		if name == "" {
			continue
		}
		ports++
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
		if okI && okO {
			key := sw.Name + "/" + idx
			if p, ok := c.prev[key]; ok {
				if dt := now.Sub(p.t).Seconds(); dt > 0 && in >= p.in && out >= p.out {
					fmt.Fprintf(&buf, "srnet_switch_rx_gbps%s %g %d\n", labels, (in-p.in)*8/dt/1e9, ts)
					fmt.Fprintf(&buf, "srnet_switch_tx_gbps%s %g %d\n", labels, (out-p.out)*8/dt/1e9, ts)
				}
			}
			c.prev[key] = snmpPrev{now, in, out}
		}
	}
	return buf.Bytes(), ports
}
