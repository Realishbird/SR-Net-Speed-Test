package controller

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
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
// (configured by WriteGnmicConfig) is the primary path.
type snmpCollector struct {
	switches []SwitchConfig
	m        *metrics
	interval time.Duration
	prev     map[string]snmpPrev // switch/ifIndex
}

func newSNMPCollector(sw []SwitchConfig, m *metrics) *snmpCollector {
	var withSNMP []SwitchConfig
	for _, s := range sw {
		if s.SNMPCommunity != "" && s.Address != "" {
			withSNMP = append(withSNMP, s)
		}
	}
	return &snmpCollector{switches: withSNMP, m: m, interval: 30 * time.Second, prev: map[string]snmpPrev{}}
}

func (c *snmpCollector) loop(ctx context.Context) {
	if len(c.switches) == 0 {
		return
	}
	log.Printf("snmp: polling %d switch(es) every %s", len(c.switches), c.interval)
	for {
		for _, sw := range c.switches {
			if lines, err := c.poll(sw); err != nil {
				log.Printf("snmp %s: %v", sw.Name, err)
			} else {
				c.m.push(lines)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(c.interval):
		}
	}
}

func (c *snmpCollector) poll(sw SwitchConfig) ([]byte, error) {
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
			return nil, fmt.Errorf("walk %s: %w", col, err)
		}
	}
	now := time.Now()
	var buf bytes.Buffer
	ts := now.UnixMilli()
	for idx, r := range rows {
		name, _ := r["name"].(string)
		if name == "" {
			continue
		}
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
	return buf.Bytes(), nil
}
