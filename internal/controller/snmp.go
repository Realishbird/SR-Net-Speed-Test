package controller

import (
	"bytes"
	"context"
	"fmt"
	"log"
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

	mu       sync.Mutex
	switches []SwitchConfig
	prev     map[string]snmpPrev   // switch/ifIndex
	status   map[string]snmpStatus // switch name -> last poll
}

// snmpStatus is the outcome of the last poll of one switch.
type snmpStatus struct {
	At    time.Time
	Ports int
	Err   string
}

func newSNMPCollector(sw []SwitchConfig, m *metrics) *snmpCollector {
	c := &snmpCollector{m: m, interval: 30 * time.Second, kick: make(chan struct{}, 1), prev: map[string]snmpPrev{}, status: map[string]snmpStatus{}}
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
			switch v := pdu.Value.(type) {
			case []byte:
				rows[idx][col] = string(v)
			case string:
				rows[idx][col] = v
			default:
				rows[idx][col] = gosnmp.ToBigInt(pdu.Value).Int64()
			}
			return nil
		})
		if err != nil {
			return nil, 0, fmt.Errorf("walk %s: %w", col, err)
		}
	}
	now := time.Now()
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
	return buf.Bytes(), ports, nil
}
