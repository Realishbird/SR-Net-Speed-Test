package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Realishbird/SR-Net-Speed-Test/internal/api"
)

// endpoint is one resolved side of a leg.
type endpoint struct {
	AgentID      int64
	Host         string
	PortID       int64
	RdmaDev      string
	PortNum      int
	Netdev       string
	IP           string
	Label        string
	Fabric       string
	ExpectedGbps float64
}

type legPlan struct {
	leg            api.Leg
	client, server endpoint
	tcpPort        int
	serverTask     api.Task
	clientTask     api.Task
	serverRes      *api.LegResult
	clientRes      *api.LegResult
	ready          bool
}

type activeRun struct {
	id      int64
	spec    api.RunSpec
	plans   []*legPlan
	startAt time.Time
	cancel  context.CancelFunc
}

// Orchestrator turns run specs into agent tasks and collects the results.
type Orchestrator struct {
	store   *Store
	hub     *hub
	metrics *metrics

	mu       sync.Mutex
	portLock map[int64]int64        // port id -> run id
	tcpPorts map[int64]map[int]bool // agent id -> tcp ports in use
	active   map[int64]*activeRun
}

func newOrchestrator(s *Store, h *hub, m *metrics) *Orchestrator {
	return &Orchestrator{store: s, hub: h, metrics: m, portLock: map[int64]int64{}, tcpPorts: map[int64]map[int]bool{}, active: map[int64]*activeRun{}}
}

// errorCounterKeys grow only when something is wrong; CNP and ECN counters are
// normal congestion signalling and are shown but do not fail a leg.
var errorCounterKeys = map[string]bool{
	"out_of_sequence": true, "packet_seq_err": true, "local_ack_timeout_err": true, "rnr_nak_retry_err": true,
	"implied_nak_seq_err": true, "duplicate_request": true, "req_cqe_error": true, "resp_cqe_error": true,
}

func applyDefaults(s *api.RunSpec) error {
	switch s.Test {
	case "", "write":
		s.Test = "write"
	case "read":
	default:
		return fmt.Errorf("test must be write or read, not %q", s.Test)
	}
	if s.DurationSec == 0 {
		s.DurationSec = 60
	}
	if s.MsgSize == 0 {
		s.MsgSize = 65536
	}
	if s.QPs == 0 {
		s.QPs = 1
	}
	if s.TOS == 0 {
		s.TOS = 106
	}
	if s.MTU == 0 {
		s.MTU = 4096
	}
	if s.PassPct == 0 {
		s.PassPct = 90
	}
	if s.DurationSec < 5 || s.DurationSec > 24*3600 {
		return errors.New("duration must be between 5 s and 24 h")
	}
	if s.Name == "" {
		s.Name = fmt.Sprintf("%s %s", s.Test, time.Now().Format("2006-01-02 15:04"))
	}
	return nil
}

func (o *Orchestrator) tool(spec api.RunSpec) string {
	if spec.Test == "read" {
		return api.ToolReadBW
	}
	return api.ToolWriteBW
}

// target is a selector match: a host, optionally narrowed to one port.
type target struct {
	host *hostRec
	port *api.HostPort
}

func resolveSelector(hosts []hostRec, sel string) ([]target, error) {
	sel = strings.TrimSpace(sel)
	if tag, ok := strings.CutPrefix(sel, "tag:"); ok {
		var out []target
		for i := range hosts {
			for _, t := range hosts[i].Tags {
				if strings.EqualFold(t, tag) {
					out = append(out, target{host: &hosts[i]})
					break
				}
			}
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("no hosts tagged %q", tag)
		}
		return out, nil
	}
	if ip := net.ParseIP(sel); ip != nil {
		for i := range hosts {
			for j := range hosts[i].Ports {
				for _, a := range hosts[i].Ports[j].Addrs {
					if pip, _, err := net.ParseCIDR(a); err == nil && pip.Equal(ip) {
						return []target{{&hosts[i], &hosts[i].Ports[j]}}, nil
					}
				}
			}
		}
		return nil, fmt.Errorf("no RDMA port has address %s", sel)
	}
	for i := range hosts {
		if strings.EqualFold(hosts[i].Name, sel) || strings.EqualFold(hosts[i].Hostname, sel) {
			return []target{{host: &hosts[i]}}, nil
		}
	}
	for i := range hosts {
		for j := range hosts[i].Ports {
			if hosts[i].Ports[j].Label != "" && strings.EqualFold(hosts[i].Ports[j].Label, sel) {
				return []target{{&hosts[i], &hosts[i].Ports[j]}}, nil
			}
		}
	}
	return nil, fmt.Errorf("no host, tag, IP or port label matches %q", sel)
}

// pairPorts picks the client and server ports that share a subnet.
func pairPorts(c, s target) (endpoint, endpoint, error) {
	ports := func(t target) []*api.HostPort {
		if t.port != nil {
			return []*api.HostPort{t.port}
		}
		var out []*api.HostPort
		for i := range t.host.Ports {
			out = append(out, &t.host.Ports[i])
		}
		return out
	}
	type cand struct {
		cp, sp     *api.HostPort
		cip, sip   string
		bothActive bool
	}
	var best *cand
	for _, cp := range ports(c) {
		for _, ca := range cp.Addrs {
			cip, cnet, err := net.ParseCIDR(ca)
			if err != nil {
				continue
			}
			for _, sp := range ports(s) {
				for _, sa := range sp.Addrs {
					sip, _, err := net.ParseCIDR(sa)
					if err != nil || !cnet.Contains(sip) {
						continue
					}
					cd := cand{cp, sp, cip.String(), sip.String(), cp.State == "ACTIVE" && sp.State == "ACTIVE"}
					if best == nil || (cd.bothActive && !best.bothActive) {
						best = &cd
					}
				}
			}
		}
	}
	if best == nil {
		return endpoint{}, endpoint{}, fmt.Errorf("no port on %s shares a subnet with %s", c.host.Name, s.host.Name)
	}
	if !best.bothActive {
		return endpoint{}, endpoint{}, fmt.Errorf("link %s (%s) to %s (%s) is not ACTIVE", c.host.Name, best.cip, s.host.Name, best.sip)
	}
	mk := func(h *hostRec, p *api.HostPort, ip string) endpoint {
		exp := p.ExpectedGbps
		if exp == 0 {
			exp = float64(p.SpeedMbps) / 1000
		}
		return endpoint{AgentID: h.ID, Host: h.Name, PortID: p.ID, RdmaDev: p.RdmaDev, PortNum: p.PortNum, Netdev: p.Netdev, IP: ip, Label: p.Label, Fabric: p.Fabric, ExpectedGbps: exp}
	}
	return mk(c.host, best.cp, best.cip), mk(s.host, best.sp, best.sip), nil
}

// plan expands a spec into legs: explicit legs, fan-out (one client), fan-in
// (one server), or disjoint pairs.
func plan(hosts []hostRec, spec api.RunSpec) ([]*legPlan, error) {
	type pair struct{ c, s target }
	var pairs []pair
	one := func(sel string) (target, error) {
		ts, err := resolveSelector(hosts, sel)
		if err != nil {
			return target{}, err
		}
		if len(ts) != 1 {
			return target{}, fmt.Errorf("%q matches %d hosts; an explicit leg needs exactly one", sel, len(ts))
		}
		return ts[0], nil
	}
	many := func(sels []string) ([]target, error) {
		var out []target
		for _, s := range sels {
			ts, err := resolveSelector(hosts, s)
			if err != nil {
				return nil, err
			}
			out = append(out, ts...)
		}
		return out, nil
	}
	if len(spec.Legs) > 0 {
		for _, l := range spec.Legs {
			c, err := one(l.Client)
			if err != nil {
				return nil, err
			}
			s, err := one(l.Server)
			if err != nil {
				return nil, err
			}
			pairs = append(pairs, pair{c, s})
		}
	} else {
		cs, err := many(spec.Clients)
		if err != nil {
			return nil, err
		}
		ss, err := many(spec.Servers)
		if err != nil {
			return nil, err
		}
		switch {
		case len(cs) == 0 || len(ss) == 0:
			return nil, errors.New("need at least one client and one server")
		case spec.Pairs:
			if len(cs) != len(ss) {
				return nil, fmt.Errorf("pairs mode needs equal counts: %d clients, %d servers", len(cs), len(ss))
			}
			for i := range cs {
				pairs = append(pairs, pair{cs[i], ss[i]})
			}
		case len(cs) == 1:
			for _, s := range ss {
				pairs = append(pairs, pair{cs[0], s})
			}
		case len(ss) == 1:
			for _, c := range cs {
				pairs = append(pairs, pair{c, ss[0]})
			}
		default:
			return nil, errors.New("several clients and several servers: use --pairs or explicit legs (full mesh comes in Phase 2)")
		}
	}
	var plans []*legPlan
	for i, p := range pairs {
		if p.c.host.ID == p.s.host.ID {
			return nil, fmt.Errorf("leg %d: client and server are the same host (%s)", i+1, p.c.host.Name)
		}
		ce, se, err := pairPorts(p.c, p.s)
		if err != nil {
			return nil, fmt.Errorf("leg %d: %w", i+1, err)
		}
		fabric := se.Fabric
		if fabric == "" {
			fabric = ce.Fabric
		}
		exp := ce.ExpectedGbps
		if se.ExpectedGbps > 0 && (exp == 0 || se.ExpectedGbps < exp) {
			exp = se.ExpectedGbps
		}
		plans = append(plans, &legPlan{
			client: ce, server: se,
			leg: api.Leg{Index: i, ClientHost: ce.Host, ClientIP: ce.IP, ServerHost: se.Host, ServerIP: se.IP, Fabric: fabric, ExpectedGbps: exp},
		})
	}
	return plans, nil
}

// Start validates and queues a run, then executes it in the background.
func (o *Orchestrator) Start(ctx context.Context, spec api.RunSpec) (int64, error) {
	if err := applyDefaults(&spec); err != nil {
		return 0, &userError{err}
	}
	hosts, err := o.store.Hosts(ctx)
	if err != nil {
		return 0, err
	}
	plans, err := plan(hosts, spec)
	if err != nil {
		return 0, &userError{err}
	}
	for _, p := range plans {
		for _, e := range []endpoint{p.client, p.server} {
			if !o.hub.online(e.AgentID) {
				return 0, &userError{fmt.Errorf("agent on %s is offline", e.Host)}
			}
		}
	}

	o.mu.Lock()
	for _, p := range plans {
		for _, e := range []endpoint{p.client, p.server} {
			if r, busy := o.portLock[e.PortID]; busy {
				o.mu.Unlock()
				return 0, &userError{fmt.Errorf("%s %s is busy in run %d", e.Host, e.IP, r)}
			}
		}
	}
	legs := make([]api.Leg, len(plans))
	portIDs := make([][2]int64, len(plans))
	for i, p := range plans {
		legs[i] = p.leg
		portIDs[i] = [2]int64{p.client.PortID, p.server.PortID}
	}
	runID, legIDs, err := o.store.CreateRun(ctx, spec, legs, portIDs)
	if err != nil {
		o.mu.Unlock()
		return 0, err
	}
	for i, p := range plans {
		p.leg.ID = legIDs[i]
		o.portLock[p.client.PortID] = runID
		o.portLock[p.server.PortID] = runID
		p.tcpPort = o.allocTCP(p.server.AgentID)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	ar := &activeRun{id: runID, spec: spec, plans: plans, cancel: cancel}
	o.active[runID] = ar
	o.mu.Unlock()

	o.store.Audit(ctx, "run.start", map[string]any{"run_id": runID, "spec": spec})
	go o.execute(runCtx, ar)
	return runID, nil
}

type userError struct{ error }

func (o *Orchestrator) allocTCP(agentID int64) int {
	used := o.tcpPorts[agentID]
	if used == nil {
		used = map[int]bool{}
		o.tcpPorts[agentID] = used
	}
	for p := 18515; p < 19515; p++ {
		if !used[p] {
			used[p] = true
			return p
		}
	}
	return 0
}

func (o *Orchestrator) release(ar *activeRun) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, p := range ar.plans {
		delete(o.portLock, p.client.PortID)
		delete(o.portLock, p.server.PortID)
		delete(o.tcpPorts[p.server.AgentID], p.tcpPort)
	}
	delete(o.active, ar.id)
}

// Cancel stops a running test on every agent involved.
func (o *Orchestrator) Cancel(runID int64) bool {
	o.mu.Lock()
	ar, ok := o.active[runID]
	o.mu.Unlock()
	if ok {
		ar.cancel()
	}
	return ok
}

func (o *Orchestrator) sendCancels(ar *activeRun) {
	sent := map[int64]bool{}
	for _, p := range ar.plans {
		for _, id := range []int64{p.client.AgentID, p.server.AgentID} {
			if !sent[id] {
				sent[id] = true
				o.hub.send(id, api.Task{ID: newID(), Kind: api.TaskCancel, RunID: ar.id}, nil)
			}
		}
	}
}

func (o *Orchestrator) execute(ctx context.Context, ar *activeRun) {
	defer o.release(ar)
	bg := context.Background()
	fail := func(status, msg string) {
		o.sendCancels(ar)
		log.Printf("run %d %s: %s", ar.id, status, msg)
		o.store.FinishRun(bg, ar.id, status, "fail", msg, api.RunSummary{})
	}
	o.store.SetRunStatus(bg, ar.id, "running")
	spec := ar.spec
	tool := o.tool(spec)
	inbox := make(chan msg, len(ar.plans)*8)
	byTask := map[string]*legPlan{}
	var taskIDs []string
	defer func() { o.hub.forget(taskIDs...) }()

	base := api.Task{RunID: ar.id, Tool: tool, DurationSec: spec.DurationSec, MsgSize: spec.MsgSize, QPs: spec.QPs, TOS: spec.TOS, MTU: spec.MTU}

	// 1. Start every server and wait until all are listening.
	for _, p := range ar.plans {
		t := base
		t.ID, t.Kind, t.LegID = newID(), api.TaskServer, p.leg.ID
		t.RdmaDev, t.PortNum, t.Netdev, t.LocalIP, t.PeerIP, t.TCPPort = p.server.RdmaDev, p.server.PortNum, p.server.Netdev, p.server.IP, p.client.IP, p.tcpPort
		p.serverTask = t
		byTask[t.ID] = p
		taskIDs = append(taskIDs, t.ID)
		o.hub.send(p.server.AgentID, t, inbox)
	}
	readyBy := time.After(30 * time.Second)
	for waiting := len(ar.plans); waiting > 0; {
		select {
		case <-ctx.Done():
			fail("canceled", "canceled by operator")
			return
		case <-readyBy:
			fail("failed", fmt.Sprintf("%d server(s) did not report ready within 30 s", waiting))
			return
		case m := <-inbox:
			p := byTask[m.taskID]
			if m.event != nil && m.event.Type == api.EventServerReady && !p.ready {
				p.ready = true
				waiting--
			}
			if (m.event != nil && m.event.Type == api.EventFailed) || m.result != nil {
				msg := "server exited early"
				if m.event != nil {
					msg = m.event.Message
				} else if m.result.Error != "" {
					msg = m.result.Error
				}
				fail("failed", fmt.Sprintf("server %s %s: %s", p.server.Host, p.server.IP, msg))
				return
			}
		}
	}

	// 2. Start every client at one shared time so aggregate load is simultaneous.
	ar.startAt = time.Now().Add(3 * time.Second)
	for _, p := range ar.plans {
		t := base
		t.ID, t.Kind, t.LegID, t.StartAt = newID(), api.TaskClient, p.leg.ID, ar.startAt
		t.RdmaDev, t.PortNum, t.Netdev, t.LocalIP, t.PeerIP, t.TCPPort = p.client.RdmaDev, p.client.PortNum, p.client.Netdev, p.client.IP, p.server.IP, p.tcpPort
		p.clientTask = t
		byTask[t.ID] = p
		taskIDs = append(taskIDs, t.ID)
		o.hub.send(p.client.AgentID, t, inbox)
	}

	// 3. Collect results. Servers normally exit with their client; any still
	// running 15 s after the last client are canceled.
	deadline := time.After(time.Until(ar.startAt) + time.Duration(spec.DurationSec)*time.Second + 90*time.Second)
	var grace <-chan time.Time
	canceled := false
	for !allDone(ar.plans) {
		select {
		case <-ctx.Done():
			if !canceled {
				canceled = true
				o.sendCancels(ar)
				grace = time.After(15 * time.Second)
			}
			ctx = context.Background()
		case <-deadline:
			o.sendCancels(ar)
			grace = time.After(0)
		case <-grace:
			goto done
		case m := <-inbox:
			if m.result == nil {
				continue
			}
			p := byTask[m.taskID]
			if m.taskID == p.clientTask.ID {
				p.clientRes = m.result
			} else {
				p.serverRes = m.result
			}
			if grace == nil && allClientsDone(ar.plans) {
				grace = time.After(15 * time.Second)
			}
		}
	}
done:
	o.sendCancels(ar) // stops any stragglers; a no-op on agents with nothing running
	status := "done"
	if canceled {
		status = "canceled"
	}
	sum, verdict := o.score(ar)
	for _, p := range ar.plans {
		o.store.SaveLeg(bg, p.leg, p.clientRes, p.serverRes)
	}
	errMsg := ""
	if canceled {
		errMsg = "canceled by operator"
	}
	o.store.FinishRun(bg, ar.id, status, verdict, errMsg, sum)
	log.Printf("run %d %s: %s, %.1f Gbps aggregate", ar.id, status, verdict, sum.ToolGbps)
}

func allDone(ps []*legPlan) bool {
	for _, p := range ps {
		if p.clientRes == nil || p.serverRes == nil {
			return false
		}
	}
	return true
}

func allClientsDone(ps []*legPlan) bool {
	for _, p := range ps {
		if p.clientRes == nil {
			return false
		}
	}
	return true
}

// counterSide picks the port whose hardware counter measures one leg alone:
// the server port in a fan-out, the client port in a fan-in.
func counterSide(ps []*legPlan, p *legPlan, test string) (portID int64, tx bool, ok bool) {
	uses := map[int64]int{}
	for _, q := range ps {
		uses[q.client.PortID]++
		uses[q.server.PortID]++
	}
	// write: data flows client -> server. read: server -> client.
	if uses[p.server.PortID] == 1 {
		return p.server.PortID, test == "read", true
	}
	if uses[p.client.PortID] == 1 {
		return p.client.PortID, test == "write", true
	}
	return 0, false, false
}

func (o *Orchestrator) score(ar *activeRun) (api.RunSummary, string) {
	sum := api.RunSummary{ByFabric: map[string]float64{}}
	from := ar.startAt.Add(2 * time.Second)
	to := ar.startAt.Add(time.Duration(ar.spec.DurationSec-1) * time.Second)
	rank := map[string]int{"pass": 0, "warn": 1, "fail": 2}
	worst := "pass"
	for _, p := range ar.plans {
		l := &p.leg
		l.ErrorDeltas = map[string]int64{}
		for _, r := range []*api.LegResult{p.clientRes, p.serverRes} {
			if r == nil {
				continue
			}
			for k, v := range r.ErrorDeltas {
				l.ErrorDeltas[k] += v
			}
		}
		if p.clientRes != nil {
			l.ToolGbps = p.clientRes.AvgGbps
		}
		if l.ToolGbps == 0 && p.serverRes != nil {
			l.ToolGbps = p.serverRes.AvgGbps
		}
		if id, tx, ok := counterSide(ar.plans, p, ar.spec.Test); ok {
			l.CounterGbps = o.metrics.counterGbps(id, from, to, tx)
		} else {
			l.Note = "both ports shared with other legs; no per-leg counter rate"
		}
		l.Verdict = "pass"
		measured := l.CounterGbps
		if measured == 0 {
			measured = l.ToolGbps
		}
		switch {
		case p.clientRes == nil:
			l.Verdict, l.Note = "fail", "no result from client"
		case p.clientRes.Error != "":
			l.Verdict, l.Note = "fail", p.clientRes.Error
		case l.ExpectedGbps > 0 && measured < ar.spec.PassPct/100*l.ExpectedGbps:
			l.Verdict = "fail"
			l.Note = fmt.Sprintf("%.1f Gbps is below %.0f%% of %.0f Gbps", measured, ar.spec.PassPct, l.ExpectedGbps)
		}
		if l.Verdict == "pass" {
			var bad []string
			for k, v := range l.ErrorDeltas {
				if errorCounterKeys[k] && v > 0 {
					bad = append(bad, fmt.Sprintf("%s +%d", k, v))
				}
			}
			if len(bad) > 0 {
				sort.Strings(bad)
				l.Verdict, l.Note = "warn", "error counters grew: "+strings.Join(bad, ", ")
			}
		}
		if rank[l.Verdict] > rank[worst] {
			worst = l.Verdict
		}
		sum.ToolGbps += l.ToolGbps
		sum.CounterGbps += l.CounterGbps
		f := l.Fabric
		if f == "" {
			f = "unassigned"
		}
		sum.ByFabric[f] += l.ToolGbps
	}
	return sum, worst
}

// Live fills in current per-leg rates for a running test.
func (o *Orchestrator) Live(r *api.Run) {
	o.mu.Lock()
	ar, ok := o.active[r.ID]
	o.mu.Unlock()
	if !ok {
		return
	}
	byID := map[int64]*legPlan{}
	for _, p := range ar.plans {
		byID[p.leg.ID] = p
	}
	r.Summary.LiveGbps = 0
	for i := range r.Legs {
		p := byID[r.Legs[i].ID]
		if p == nil {
			continue
		}
		if id, tx, ok := counterSide(ar.plans, p, ar.spec.Test); ok {
			t, rx := o.metrics.live(id)
			if tx {
				r.Legs[i].LiveGbps = t
			} else {
				r.Legs[i].LiveGbps = rx
			}
			r.Summary.LiveGbps += r.Legs[i].LiveGbps
		}
	}
}

func newID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}
