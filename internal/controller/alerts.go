package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/Realishbird/SR-Net-Speed-Test/internal/api"
)

// alerter opens and resolves alerts in the store and posts each change to the
// configured webhook (any JSON receiver: Slack, Teams, Alertmanager, a script).
type alerter struct {
	store      *Store
	hub        *hub
	webhook    string
	hotC       float64
	offlineFor time.Duration

	mu        sync.Mutex
	wasOnline map[int64]bool
	idleErr   map[string]float64 // port key -> last idle error counter total
}

// idleErrorKeys are hw_counters whose growth while no test runs points at a bad link.
var idleErrorKeys = []string{"rdma_hw_out_of_sequence", "rdma_hw_packet_seq_err", "rdma_hw_local_ack_timeout_err", "rdma_port_rcv_errors", "rdma_symbol_error", "nic_rx_crc_errors_phy", "nic_rx_symbol_err_phy"}

func newAlerter(s *Store, h *hub, cfg Config) *alerter {
	hot := cfg.TransceiverMaxC
	if hot == 0 {
		hot = 70
	}
	return &alerter{store: s, hub: h, webhook: cfg.AlertWebhookURL, hotC: hot, offlineFor: 2 * time.Minute, wasOnline: map[int64]bool{}, idleErr: map[string]float64{}}
}

func (a *alerter) open(ctx context.Context, al api.Alert) {
	opened, err := a.store.OpenAlert(ctx, al)
	if err != nil {
		log.Printf("alert: %v", err)
		return
	}
	if opened {
		log.Printf("ALERT %s %s %s: %s", al.Kind, al.Host, al.Port, al.Message)
		a.notify("firing", al)
	}
}

func (a *alerter) resolve(ctx context.Context, kind, host, port string) {
	if ok, _ := a.store.ResolveAlert(ctx, kind, host, port); ok {
		a.notify("resolved", api.Alert{Kind: kind, Host: host, Port: port})
	}
}

func (a *alerter) notify(state string, al api.Alert) {
	if a.webhook == "" {
		return
	}
	body, _ := json.Marshal(map[string]any{
		"state": state, "kind": al.Kind, "host": al.Host, "port": al.Port, "message": al.Message, "time": time.Now(),
		"text": fmt.Sprintf("[srnet %s] %s %s %s: %s", state, al.Kind, al.Host, al.Port, al.Message),
	})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, "POST", a.webhook, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			log.Printf("alert webhook: %v", err)
			return
		}
		resp.Body.Close()
	}()
}

// onSamples checks each telemetry batch for link down, hot optics and error
// counters growing while no test runs on that port.
func (a *alerter) onSamples(ctx context.Context, host string, ports map[string]portMeta, batch []api.Sample) {
	for _, s := range batch {
		if s.RdmaDev == "" {
			continue
		}
		meta, ok := ports[s.RdmaDev+"/"+strconv.Itoa(s.PortNum)]
		if !ok {
			continue
		}
		port := meta.RdmaDev + "/" + strconv.Itoa(meta.PortNum)
		if meta.Label != "" {
			port = meta.Label
		}
		if active, ok := s.Values["rdma_port_active"]; ok {
			if active == 0 {
				a.open(ctx, api.Alert{Kind: "link_down", Host: host, Port: port, Message: fmt.Sprintf("%s %s is not ACTIVE", host, port)})
			} else {
				a.resolve(ctx, "link_down", host, port)
			}
		}
		if t, ok := s.Values["dom_temp_c"]; ok {
			if t >= a.hotC {
				a.open(ctx, api.Alert{Kind: "transceiver_hot", Host: host, Port: port, Message: fmt.Sprintf("%s %s transceiver at %.1f C (limit %.0f C)", host, port, t, a.hotC)})
			} else if t < a.hotC-5 {
				a.resolve(ctx, "transceiver_hot", host, port)
			}
		}
		if len(s.RunIDs) == 0 {
			total := 0.0
			for _, k := range idleErrorKeys {
				total += s.Values[k]
			}
			key := host + "|" + port
			a.mu.Lock()
			prev, seen := a.idleErr[key]
			a.idleErr[key] = total
			a.mu.Unlock()
			if seen && total > prev {
				a.open(ctx, api.Alert{Kind: "errors_idle", Host: host, Port: port, Message: fmt.Sprintf("%s %s: error counters grew by %.0f while idle", host, port, total-prev)})
			}
		}
	}
}

// loop checks agent liveness every 30 s.
func (a *alerter) loop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Second):
		}
		hosts, err := a.store.Hosts(ctx)
		if err != nil {
			continue
		}
		for _, h := range hosts {
			online := a.hub.online(h.ID)
			a.mu.Lock()
			was := a.wasOnline[h.ID]
			a.wasOnline[h.ID] = online
			a.mu.Unlock()
			switch {
			case online:
				a.resolve(ctx, "agent_offline", h.Name, "")
			case was || time.Since(h.LastSeen) > a.offlineFor && !h.LastSeen.IsZero():
				if time.Since(h.LastSeen) > a.offlineFor {
					a.open(ctx, api.Alert{Kind: "agent_offline", Host: h.Name, Message: fmt.Sprintf("%s has not reported since %s", h.Name, h.LastSeen.Local().Format(time.RFC822))})
				}
			}
		}
	}
}
