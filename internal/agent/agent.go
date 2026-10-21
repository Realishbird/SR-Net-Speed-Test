// Package agent runs on each test host: it enrolls with the controller, runs
// benchmark tasks, and streams NIC, transceiver and host telemetry.
package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Realishbird/SR-Net-Speed-Test/internal/api"
	"github.com/Realishbird/SR-Net-Speed-Test/internal/version"
)

const DefaultConfigPath = "/etc/srnet/agent.json"

type Config struct {
	Controller string `json:"controller"` // https://host:8443
	CAFile     string `json:"ca_file"`
	AgentID    string `json:"agent_id"`
	Secret     string `json:"secret"`
}

func LoadConfig(path string) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	return c, json.Unmarshal(b, &c)
}

func (c Config) Save(path string) error {
	b, _ := json.MarshalIndent(c, "", "  ")
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// Client talks to the controller over HTTPS, trusting only the controller's CA.
type Client struct {
	base   string
	secret string
	http   *http.Client
}

func NewClient(cfg Config) (*Client, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.CAFile != "" {
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("no certificates in %s", cfg.CAFile)
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	tr.Proxy = nil // the controller is on the management network; never route it through a proxy
	return &Client{base: cfg.Controller, secret: cfg.Secret, http: &http.Client{Transport: tr}}, nil
}

func (c *Client) do(ctx context.Context, method, path string, in, out any, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.secret != "" {
		req.Header.Set("Authorization", "Bearer "+c.secret)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &httpError{status: resp.StatusCode, msg: fmt.Sprintf("%s %s: %s: %s", method, path, resp.Status, bytes.TrimSpace(msg))}
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

type httpError struct {
	status int
	msg    string
}

func (e *httpError) Error() string { return e.msg }

// Enroll trades a one-time token for this agent's identity and writes the config.
func Enroll(ctx context.Context, cfg Config, token, path string) error {
	c, err := NewClient(cfg)
	if err != nil {
		return err
	}
	var resp api.EnrollResponse
	if err := c.do(ctx, "POST", "/api/v1/enroll", api.EnrollRequest{Token: token, Inventory: Inventory(ctx)}, &resp, 30*time.Second); err != nil {
		return err
	}
	cfg.AgentID, cfg.Secret = resp.AgentID, resp.Secret
	return cfg.Save(path)
}

// Run is the agent's main loop.
func Run(ctx context.Context, cfg Config) error {
	if cfg.Secret == "" {
		return errors.New("agent is not enrolled; run `srnet-agent enroll` first")
	}
	c, err := NewClient(cfg)
	if err != nil {
		return err
	}
	// The background loops run until this context ends. A staged update has
	// to end it explicitly: the parent context only ends on SIGTERM, and the
	// agent must exit on its own for the launcher to swap the binary in.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sampler := NewSampler()
	spool := newSpool(c)
	runner := NewRunner(sampler,
		func(e api.Event) { spool.send(ctx, "/api/v1/agent/events", e) },
		func(r api.LegResult) { spool.send(ctx, "/api/v1/agent/results", r) },
	)

	var wg sync.WaitGroup
	wg.Add(3)
	go func() { // inventory: at start and every minute
		defer wg.Done()
		for {
			inv := Inventory(ctx)
			sampler.SetPorts(inv.Ports)
			if err := c.do(ctx, "POST", "/api/v1/agent/inventory", inv, nil, 30*time.Second); err != nil {
				log.Printf("inventory: %v", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Minute):
			}
		}
	}()
	go func() {
		defer wg.Done()
		sampler.Loop(ctx, func(s []api.Sample) {
			if err := c.do(ctx, "POST", "/api/v1/agent/samples", api.SampleBatch{Samples: s}, nil, 10*time.Second); err != nil {
				log.Printf("samples: %v", err)
			}
		})
	}()
	go func() {
		defer wg.Done()
		spool.loop(ctx)
	}()

	log.Printf("agent %s (%s) polling %s", cfg.AgentID, version.Version, cfg.Controller)
	backoff := time.Second
	healthy := false
	for ctx.Err() == nil {
		var tasks []api.Task
		err := c.do(ctx, "GET", "/api/v1/agent/tasks?wait=25", nil, &tasks, 40*time.Second)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			log.Printf("poll: %v (retry in %s)", err, backoff)
			select {
			case <-ctx.Done():
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff = time.Second
		if !healthy {
			healthy = true
			MarkHealthy()
		}
		for _, t := range tasks {
			log.Printf("task %s: %s %s run=%d leg=%d", t.ID, t.Kind, t.Tool, t.RunID, t.LegID)
			if t.Kind == api.TaskUpdate {
				if restart := handleUpdate(ctx, c, runner, spool, t); restart {
					cancel()
					wg.Wait()
					return errRestart
				}
				continue
			}
			runner.Handle(ctx, t)
		}
	}
	wg.Wait()
	return ctx.Err()
}

// ErrRestart is returned by Run when a staged update needs a process restart;
// main exits 0 and systemd starts the launcher, which swaps the binary in.
var errRestart = errors.New("restart to apply update")

func IsRestart(err error) bool { return errors.Is(err, errRestart) }

func handleUpdate(ctx context.Context, c *Client, runner *Runner, sp *spool, t api.Task) bool {
	ev := api.Event{TaskID: t.ID, Type: api.EventUpdateApplied, Message: t.Version}
	if runner.Active() {
		ev.Type, ev.Message = api.EventUpdateFailed, "a test is running on this host"
	} else if err := c.ApplyUpdate(ctx, t); err != nil {
		ev.Type, ev.Message = api.EventUpdateFailed, err.Error()
	}
	// Deliver synchronously: the process is about to exit.
	if err := c.do(ctx, "POST", "/api/v1/agent/events", ev, nil, 15*time.Second); err != nil {
		sp.send(ctx, "/api/v1/agent/events", ev)
	}
	if ev.Type == api.EventUpdateFailed {
		log.Printf("update to %s failed: %s", t.Version, ev.Message)
		return false
	}
	log.Printf("update to %s staged; restarting", t.Version)
	return true
}

// spool delivers events and results in order, retrying while the controller is
// unreachable so a result is not lost to a brief outage.
type spool struct {
	c     *Client
	mu    sync.Mutex
	items []spoolItem
	wake  chan struct{}
}

type spoolItem struct {
	path string
	body any
}

func newSpool(c *Client) *spool { return &spool{c: c, wake: make(chan struct{}, 1)} }

func (s *spool) send(ctx context.Context, path string, body any) {
	s.mu.Lock()
	s.items = append(s.items, spoolItem{path, body})
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *spool) loop(ctx context.Context) {
	for {
		s.mu.Lock()
		var it *spoolItem
		if len(s.items) > 0 {
			it = &s.items[0]
		}
		s.mu.Unlock()
		if it == nil {
			select {
			case <-ctx.Done():
				return
			case <-s.wake:
			}
			continue
		}
		err := s.c.do(ctx, "POST", it.path, it.body, nil, 15*time.Second)
		var he *httpError
		if errors.As(err, &he) && he.status < 500 {
			log.Printf("deliver %s: dropped: %v", it.path, err) // the controller rejected it; retrying will not help
		} else if err != nil {
			log.Printf("deliver %s: %v", it.path, err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
			continue
		}
		s.mu.Lock()
		s.items = s.items[1:]
		s.mu.Unlock()
	}
}
