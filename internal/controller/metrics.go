package controller

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Realishbird/SR-Net-Speed-Test/internal/api"
)

// portMeta is what the controller attaches to each port's samples.
type portMeta struct {
	PortID  int64
	Host    string
	RdmaDev string
	PortNum int
	Netdev  string
	Label   string
	Fabric  string
}

type bytesPoint struct {
	t            time.Time
	tx, rx       float64 // RDMA port counters, bytes
	nicTx, nicRx float64 // netdev counters, bytes (IP traffic)
}

type liveRate struct {
	t            time.Time
	tx, rx       float64 // Gbps from RDMA counters
	nicTx, nicRx float64 // Gbps from netdev counters
}

// metrics keeps recent per-port byte counters in memory (for live views and
// counter-derived results) and forwards every sample to VictoriaMetrics.
type metrics struct {
	vmURL string
	out   chan []byte

	mu     sync.Mutex
	ring   map[int64][]bytesPoint
	latest map[int64]liveRate
}

const ringAge = 2 * time.Hour

func newMetrics(vmURL string) *metrics {
	return &metrics{vmURL: strings.TrimRight(vmURL, "/"), out: make(chan []byte, 256), ring: map[int64][]bytesPoint{}, latest: map[int64]liveRate{}}
}

// ingest records a batch from one agent. ports maps "dev/port" to its metadata;
// host is the host's display name for host-level samples.
func (m *metrics) ingest(host string, ports map[string]portMeta, batch []api.Sample) {
	var buf bytes.Buffer
	for _, s := range batch {
		labels := map[string]string{"host": host}
		var meta portMeta
		var isPort bool
		if s.RdmaDev != "" {
			meta, isPort = ports[s.RdmaDev+"/"+strconv.Itoa(s.PortNum)]
			labels["rdma_dev"] = s.RdmaDev
			labels["port"] = strconv.Itoa(s.PortNum)
			labels["netdev"] = s.Netdev
			if isPort {
				labels["port_label"] = meta.Label
				labels["fabric"] = meta.Fabric
			}
		}
		if len(s.RunIDs) > 0 {
			ids := make([]string, len(s.RunIDs))
			for i, id := range s.RunIDs {
				ids[i] = strconv.FormatInt(id, 10)
			}
			sort.Strings(ids)
			labels["run_id"] = strings.Join(ids, ",")
		}
		ts := s.Time.UnixMilli()
		keys := make([]string, 0, len(s.Values))
		for k := range s.Values {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			name, sub, _ := strings.Cut(k, ".")
			l := labels
			if sub != "" {
				l = copyWith(labels, "sub", sub)
			}
			fmt.Fprintf(&buf, "srnet_%s%s %g %d\n", metricName(name), formatLabels(l), s.Values[k], ts)
		}
		if isPort {
			m.track(meta.PortID, s)
		}
	}
	if m.vmURL == "" || buf.Len() == 0 {
		return
	}
	select {
	case m.out <- buf.Bytes():
	default:
		log.Printf("metrics: VictoriaMetrics writer is behind; dropped %d bytes", buf.Len())
	}
}

func (m *metrics) track(portID int64, s api.Sample) {
	tx, okT := s.Values["rdma_tx_bytes"]
	rx, okR := s.Values["rdma_rx_bytes"]
	m.mu.Lock()
	defer m.mu.Unlock()
	if okT && okR {
		pt := bytesPoint{t: s.Time, tx: tx, rx: rx, nicTx: s.Values["nic_tx_bytes"], nicRx: s.Values["nic_rx_bytes"]}
		r := m.ring[portID]
		lr := liveRate{t: s.Time, tx: s.Values["rdma_tx_gbps"], rx: s.Values["rdma_rx_gbps"]}
		if n := len(r); n > 0 {
			if dt := s.Time.Sub(r[n-1].t).Seconds(); dt > 0 && pt.nicTx >= r[n-1].nicTx && pt.nicRx >= r[n-1].nicRx {
				lr.nicTx = (pt.nicTx - r[n-1].nicTx) * 8 / dt / 1e9
				lr.nicRx = (pt.nicRx - r[n-1].nicRx) * 8 / dt / 1e9
			}
		}
		r = append(r, pt)
		cut := 0
		for cut < len(r) && time.Since(r[cut].t) > ringAge {
			cut++
		}
		m.ring[portID] = r[cut:]
		m.latest[portID] = lr
	}
}

// live returns the most recent RDMA rate on a port, or 0 if it is stale.
func (m *metrics) live(portID int64) (tx, rx float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.latest[portID]
	if !ok || time.Since(r.t) > 20*time.Second {
		return 0, 0
	}
	return r.tx, r.rx
}

// liveNIC is live for the netdev (IP) counters.
func (m *metrics) liveNIC(portID int64) (tx, rx float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.latest[portID]
	if !ok || time.Since(r.t) > 20*time.Second {
		return 0, 0
	}
	return r.nicTx, r.nicRx
}

// counterGbps is the average rate on a port between from and to, from the
// byte counters. tx selects the transmit direction; nic selects the netdev
// counters (IP traffic) instead of the RDMA port counters.
func (m *metrics) counterGbps(portID int64, from, to time.Time, tx, nic bool) float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	var a, b *bytesPoint
	for i := range m.ring[portID] {
		p := &m.ring[portID][i]
		if a == nil && !p.t.Before(from) {
			a = p
		}
		if !p.t.After(to) {
			b = p
		}
	}
	if a == nil || b == nil {
		return 0
	}
	dt := b.t.Sub(a.t).Seconds()
	if dt < 1 {
		return 0
	}
	var d float64
	switch {
	case nic && tx:
		d = b.nicTx - a.nicTx
	case nic:
		d = b.nicRx - a.nicRx
	case tx:
		d = b.tx - a.tx
	default:
		d = b.rx - a.rx
	}
	if d < 0 {
		return 0
	}
	return d * 8 / dt / 1e9
}

// push queues already-formatted Prometheus lines (switch collectors use it).
func (m *metrics) push(lines []byte) {
	if m.vmURL == "" || len(lines) == 0 {
		return
	}
	select {
	case m.out <- lines:
	default:
		log.Printf("metrics: VictoriaMetrics writer is behind; dropped %d bytes", len(lines))
	}
}

// writer posts batches to VictoriaMetrics' Prometheus import endpoint.
func (m *metrics) writer(ctx context.Context) {
	if m.vmURL == "" {
		return
	}
	client := &http.Client{Timeout: 10 * time.Second}
	url := m.vmURL + "/api/v1/import/prometheus"
	for {
		select {
		case <-ctx.Done():
			return
		case b := <-m.out:
			resp, err := client.Post(url, "text/plain", bytes.NewReader(b))
			if err != nil {
				log.Printf("metrics: %v", err)
				continue
			}
			resp.Body.Close()
			if resp.StatusCode >= 300 {
				log.Printf("metrics: VictoriaMetrics returned %s", resp.Status)
			}
		}
	}
}

func metricName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

func copyWith(m map[string]string, k, v string) map[string]string {
	c := make(map[string]string, len(m)+1)
	for a, b := range m {
		c[a] = b
	}
	c[k] = v
	return c
}

func formatLabels(l map[string]string) string {
	keys := make([]string, 0, len(l))
	for k, v := range l {
		if v != "" {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return ""
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + strconv.Quote(l[k])
	}
	return "{" + strings.Join(parts, ",") + "}"
}
