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
	LLDPSwitch   string // neighbour chassis the host's lldpd saw on this port
	LLDPPort     string
}

type legPlan struct {
	leg            api.Leg
	client, server endpoint
	stage          int
	startAt        time.Time
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
	plans   []*legPlan // every leg, in order
	stages  [][]*legPlan
	cancel  context.CancelFunc
	summary api.RunSummary // filled by score, extended by applyBaseline
	fabric  []string       // fabric check findings on the run's cables at start
}

// Orchestrator turns run specs into agent tasks and collects the results.
type Orchestrator struct {
	onFinish func(api.Run) // Phase 7: notifications when a run ends
	store    *Store
	hub      *hub
	metrics  *metrics
	// switches lists the configured switches, for per-leg switch counters.
	// Nil when the server has not wired it (tests).
	switches func() []SwitchConfig
	// lldp completes host ports' switch neighbours from the switch side.
	// Nil when the server has not wired it (tests).
	lldp func(ctx context.Context, hosts []hostRec)
	// fabric lists the fabric check's findings on the ports a run uses, for
	// its summary. Nil when the server has not wired it (tests).
	fabric func(ctx context.Context, portIDs map[int64]bool) []string

	mu       sync.Mutex
	portLock map[int64]int64        // port id -> run id
	tcpPorts map[int64]map[int]bool // agent id -> tcp ports in use
	active   map[int64]*activeRun
	netplan  map[int64]bool // agent id -> a network change is being applied: no run starts there
}

func newOrchestrator(s *Store, h *hub, m *metrics) *Orchestrator {
	return &Orchestrator{store: s, hub: h, metrics: m, portLock: map[int64]int64{}, tcpPorts: map[int64]map[int]bool{}, active: map[int64]*activeRun{}, netplan: map[int64]bool{}}
}

// errorCounterKeys grow only when something is wrong; CNP and ECN counters are
// normal congestion signalling and are shown but do not fail a leg.
var errorCounterKeys = map[string]bool{
	"out_of_sequence": true, "packet_seq_err": true, "local_ack_timeout_err": true, "rnr_nak_retry_err": true,
	"implied_nak_seq_err": true, "duplicate_request": true, "req_cqe_error": true, "resp_cqe_error": true,
}

// sendExpectedKeys are counters a two-sided (Send/Receive) test moves by
// design, so they do not warn on a send leg: the receiver's queue of posted
// receives runs dry for an instant now and then and the sender gets an RNR
// NAK and retries (rnr_nak_retry_err), and at teardown the receives still
// posted are flushed with an error completion (resp_cqe_error, one per
// buffer). On a one-sided write or read test both still mean trouble.
var sendExpectedKeys = map[string]bool{"rnr_nak_retry_err": true, "resp_cqe_error": true}

// counterIsError says whether a grown counter is a fault for this test.
func counterIsError(test, key string) bool {
	if !errorCounterKeys[key] {
		return false
	}
	return !(sendExpectedKeys[key] && (test == "send" || test == "send_lat"))
}

func applyDefaults(s *api.RunSpec) error {
	if s.Test == "" {
		s.Test = "write"
	}
	if _, ok := api.Tests[s.Test]; !ok {
		return fmt.Errorf("unknown test %q (write, read, send, write_lat, read_lat, send_lat, tcp, udp, nccl_allreduce, nccl_allgather)", s.Test)
	}
	switch s.Ports {
	case "", api.PortsAll, api.PortsOne:
	default:
		return fmt.Errorf("unknown ports choice %q (all or one)", s.Ports)
	}
	if s.DurationSec == 0 {
		s.DurationSec = 60
	}
	if s.MsgSize == 0 {
		switch {
		case api.IsLatency(s.Test):
			s.MsgSize = 2
		case s.Test == "udp":
			s.MsgSize = 8192
		case s.Test == "tcp":
			s.MsgSize = 131072
		case api.IsCollective(s.Test):
			s.MsgSize = 64 << 20 // 64 MiB: large enough for NCCL's ring to reach line rate
		default:
			s.MsgSize = 65536
		}
	}
	if s.QPs == 0 {
		s.QPs = 1
	}
	if s.Streams == 0 {
		s.Streams = 1
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
	if s.MaxLossPct == 0 {
		s.MaxLossPct = 1
	}
	if s.RegressPct == 0 {
		s.RegressPct = 5
	}
	if s.RegressPct < 0 || s.RegressPct > 100 {
		return errors.New("regression allowance must be between 0 and 100 percent")
	}
	if s.DurationSec < 5 || s.DurationSec > 24*3600 {
		return errors.New("duration must be between 5 s and 24 h")
	}
	if s.Bidir && (api.IsLatency(s.Test) || s.Test == "udp" || api.IsCollective(s.Test)) {
		return errors.New("bidirectional applies to bandwidth tests only")
	}
	if api.IsCollective(s.Test) {
		s.Serial, s.Pairs, s.Mesh, s.Legs = false, false, false, nil // every rank takes part at once
		if len(s.Sizes) > 16 {
			return errors.New("a collective sweep takes at most 16 sizes")
		}
	}
	for _, sz := range s.Sizes {
		if sz < 1 || sz > 1<<30 {
			return fmt.Errorf("invalid sweep size %d", sz)
		}
	}
	if s.Name == "" {
		s.Name = fmt.Sprintf("%s %s", s.Test, time.Now().Format("2006-01-02 15:04"))
	}
	return nil
}

func (o *Orchestrator) tool(spec api.RunSpec) string { return api.Tests[spec.Test] }

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
	// host/port: one port of one host, by label or netdev, for ports that
	// have no label of their own yet.
	if h, port, ok := strings.Cut(sel, "/"); ok {
		for i := range hosts {
			if !strings.EqualFold(hosts[i].Name, strings.TrimSpace(h)) && !strings.EqualFold(hosts[i].Hostname, strings.TrimSpace(h)) {
				continue
			}
			for j := range hosts[i].Ports {
				p := &hosts[i].Ports[j]
				if strings.EqualFold(p.Netdev, strings.TrimSpace(port)) || p.Label != "" && strings.EqualFold(p.Label, strings.TrimSpace(port)) {
					return []target{{&hosts[i], p}}, nil
				}
			}
			return nil, fmt.Errorf("%s has no port %q", hosts[i].Name, strings.TrimSpace(port))
		}
	}
	return nil, fmt.Errorf("no host, tag, IP or port label matches %q", sel)
}

// portChoice narrows the ports a run may use: only those on one fabric,
// when the spec names one. A port's fabric is its label, else the fabric of
// the labelled ports on its subnet.
type portChoice struct {
	fabric  string
	subnets map[string]string // network (172.27.100.0/24) -> fabric, from labelled ports
}

func newPortChoice(hosts []hostRec, fabric string) portChoice {
	pc := portChoice{fabric: fabric, subnets: map[string]string{}}
	for _, h := range hosts {
		for _, p := range h.Ports {
			if p.Fabric == "" {
				continue
			}
			for _, a := range p.Addrs {
				if _, n, err := net.ParseCIDR(a); err == nil {
					pc.subnets[n.String()] = p.Fabric
				}
			}
		}
	}
	return pc
}

// fabricOf is the fabric of a port's address.
func (pc portChoice) fabricOf(p *api.HostPort, addr string) string {
	if p.Fabric != "" {
		return p.Fabric
	}
	if _, n, err := net.ParseCIDR(addr); err == nil {
		return pc.subnets[n.String()]
	}
	return ""
}

// portPair is a client port and a server port on one subnet, with the
// addresses (CIDR form) they meet on.
type portPair struct {
	cp, sp     *api.HostPort
	ca, sa     string
	bothActive bool
}

func (pp portPair) cip() string { a, _, _ := strings.Cut(pp.ca, "/"); return a }
func (pp portPair) sip() string { a, _, _ := strings.Cut(pp.sa, "/"); return a }

// portPairs lists every client and server port pair on a shared subnet, in
// port order. A host target offers each port with an address (the twin
// PCIe function of a Spark port has none), never its management port; a
// port target offers that port alone.
func (pc portChoice) portPairs(c, s target) []portPair {
	ports := func(t target) []*api.HostPort {
		if t.port != nil {
			return []*api.HostPort{t.port}
		}
		var out []*api.HostPort
		for i := range t.host.Ports {
			if p := &t.host.Ports[i]; p.Netplan == nil || !p.Netplan.Mgmt {
				out = append(out, p)
			}
		}
		return out
	}
	var out []portPair
	for _, cp := range ports(c) {
		for _, ca := range cp.Addrs {
			_, cnet, err := net.ParseCIDR(ca)
			if err != nil {
				continue
			}
			if pc.fabric != "" && !strings.EqualFold(pc.fabricOf(cp, ca), pc.fabric) {
				continue
			}
			for _, sp := range ports(s) {
				for _, sa := range sp.Addrs {
					sip, _, err := net.ParseCIDR(sa)
					if err != nil || !cnet.Contains(sip) {
						continue
					}
					out = append(out, portPair{cp, sp, ca, sa, cp.State == "ACTIVE" && sp.State == "ACTIVE"})
				}
			}
		}
	}
	return out
}

func (pc portChoice) noPair(c, s target) error {
	if pc.fabric != "" {
		return fmt.Errorf("no port on %s shares a subnet with %s on fabric %s", c.host.Name, s.host.Name, pc.fabric)
	}
	return fmt.Errorf("no port on %s shares a subnet with %s", c.host.Name, s.host.Name)
}

func inactive(c, s target, pp portPair) error {
	return fmt.Errorf("link %s (%s) to %s (%s) is not ACTIVE", c.host.Name, pp.cip(), s.host.Name, pp.sip())
}

// pairPorts picks one client and server port that share a subnet: an
// ACTIVE pair first and, among those, the one whose ports carry the fewest
// legs so far (use, by port id; nil counts nothing), so a fan-out from a
// server with a NIC on each fabric spreads over both.
func (pc portChoice) pairPorts(c, s target, use map[int64]int) (endpoint, endpoint, error) {
	var best *portPair
	bestLoad := 0
	for _, pp := range pc.portPairs(c, s) {
		pp := pp
		load := use[pp.cp.ID] + use[pp.sp.ID]
		if best == nil || pp.bothActive && !best.bothActive || pp.bothActive == best.bothActive && load < bestLoad {
			best, bestLoad = &pp, load
		}
	}
	if best == nil {
		return endpoint{}, endpoint{}, pc.noPair(c, s)
	}
	if !best.bothActive {
		return endpoint{}, endpoint{}, inactive(c, s, *best)
	}
	if use != nil {
		use[best.cp.ID]++
		use[best.sp.ID]++
	}
	return pc.endpoint(c.host, best.cp, best.ca), pc.endpoint(s.host, best.sp, best.sa), nil
}

// pairAllPorts gives one leg per subnet the client and server share: each
// client port meets a server port on its subnet, every port at most once,
// so a Spark with P1 on fabric 100 and P2 on fabric 101 tests both cables.
// A configured port whose link is down fails the plan rather than being
// left out quietly.
func (pc portChoice) pairAllPorts(c, s target) ([][2]endpoint, error) {
	used := map[*api.HostPort]bool{}
	var out [][2]endpoint
	for _, pp := range pc.portPairs(c, s) {
		if used[pp.cp] || used[pp.sp] {
			continue
		}
		if !pp.bothActive {
			return nil, inactive(c, s, pp)
		}
		used[pp.cp], used[pp.sp] = true, true
		out = append(out, [2]endpoint{pc.endpoint(c.host, pp.cp, pp.ca), pc.endpoint(s.host, pp.sp, pp.sa)})
	}
	if len(out) == 0 {
		return nil, pc.noPair(c, s)
	}
	return out, nil
}

// endpoint is one end of a leg: the port and the address (CIDR form) it
// uses.
func (pc portChoice) endpoint(h *hostRec, p *api.HostPort, addr string) endpoint {
	exp := p.ExpectedGbps
	if exp == 0 {
		exp = float64(p.SpeedMbps) / 1000
	}
	ip, _, _ := strings.Cut(addr, "/")
	return endpoint{AgentID: h.ID, Host: h.Name, PortID: p.ID, RdmaDev: p.RdmaDev, PortNum: p.PortNum, Netdev: p.Netdev, IP: ip, Label: p.Label, Fabric: pc.fabricOf(p, addr), ExpectedGbps: exp, LLDPSwitch: p.LLDPSwitch, LLDPPort: p.LLDPPort}
}

// plan expands a spec into legs: explicit legs, fan-out (one client), fan-in
// (one server), or disjoint pairs.
func plan(hosts []hostRec, spec api.RunSpec) ([]*legPlan, error) {
	if api.IsCollective(spec.Test) {
		return planCollective(hosts, spec)
	}
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
	if spec.Mesh {
		hs, err := many(spec.Hosts)
		if err != nil {
			return nil, err
		}
		if len(hs) < 2 {
			return nil, errors.New("a mesh needs at least two hosts")
		}
		for i := range hs {
			for j := range hs {
				if i != j && hs[i].host.ID != hs[j].host.ID {
					pairs = append(pairs, pair{hs[i], hs[j]})
				}
			}
		}
	} else if len(spec.Legs) > 0 {
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
			return nil, errors.New("several clients and several servers: use --pairs, --mesh, or explicit legs")
		}
	}
	pc := newPortChoice(hosts, spec.Fabric)
	use := map[int64]int{}
	var base []*legPlan
	for i, p := range pairs {
		if p.c.host.ID == p.s.host.ID {
			return nil, fmt.Errorf("leg %d: client and server are the same host (%s)", i+1, p.c.host.Name)
		}
		var eps [][2]endpoint
		if spec.Ports == api.PortsOne {
			ce, se, err := pc.pairPorts(p.c, p.s, use)
			if err != nil {
				return nil, fmt.Errorf("leg %d: %w", i+1, err)
			}
			eps = [][2]endpoint{{ce, se}}
		} else {
			var err error
			if eps, err = pc.pairAllPorts(p.c, p.s); err != nil {
				return nil, fmt.Errorf("leg %d: %w", i+1, err)
			}
		}
		for _, ep := range eps {
			ce, se := ep[0], ep[1]
			fabric := se.Fabric
			if fabric == "" {
				fabric = ce.Fabric
			}
			exp := ce.ExpectedGbps
			if se.ExpectedGbps > 0 && (exp == 0 || se.ExpectedGbps < exp) {
				exp = se.ExpectedGbps
			}
			base = append(base, &legPlan{
				client: ce, server: se,
				leg: api.Leg{ClientHost: ce.Host, ClientIP: ce.IP, ServerHost: se.Host, ServerIP: se.IP, Fabric: fabric, ExpectedGbps: exp},
			})
		}
	}
	// Expand a size sweep: every leg repeats at each size. Serial mode then
	// puts every leg in its own stage; otherwise one stage per size.
	sizes := spec.Sizes
	if len(sizes) == 0 {
		sizes = []int{spec.MsgSize}
	}
	var plans []*legPlan
	for si, sz := range sizes {
		for _, b := range base {
			lp := *b
			lp.leg.MsgSize = sz
			lp.leg.Index = len(plans)
			if spec.Serial {
				lp.stage = len(plans)
			} else {
				lp.stage = si
			}
			lp.leg.Stage = lp.stage
			plans = append(plans, &lp)
		}
	}
	if len(plans) > 512 {
		return nil, fmt.Errorf("%d legs is too many for one run (max 512)", len(plans))
	}
	fairShare(plans, spec.Bidir)
	return plans, nil
}

// planCollective lays out an NCCL collective: every selected host is one
// rank, the first is rank 0 and hosts the rendezvous, and each other rank is
// a leg "rank i > rank 0" so the run keeps the usual per-host rows. All ranks
// must share one fabric (one subnet), because NCCL's ring runs over every
// host at once. The expected figure is the bus bandwidth NCCL's own tests
// report, which a ring over equal links tops out at the slowest link's rate.
// A size sweep runs inside the one process, so there is a single stage.
func planCollective(hosts []hostRec, spec api.RunSpec) ([]*legPlan, error) {
	sels := spec.Hosts
	if len(sels) == 0 {
		sels = append(append([]string{}, spec.Clients...), spec.Servers...)
	}
	var ranks []target
	seen := map[int64]bool{}
	for _, sel := range sels {
		ts, err := resolveSelector(hosts, sel)
		if err != nil {
			return nil, err
		}
		for _, t := range ts {
			if !seen[t.host.ID] {
				seen[t.host.ID] = true
				ranks = append(ranks, t)
			}
		}
	}
	if len(ranks) < 2 {
		return nil, errors.New("a collective needs at least two hosts (one GPU each)")
	}
	if len(ranks) > 512 {
		return nil, fmt.Errorf("%d hosts is too many for one collective (max 512)", len(ranks))
	}
	var plans []*legPlan
	var master endpoint
	exp := 0.0
	for i, t := range ranks[1:] {
		ce, se, err := newPortChoice(hosts, spec.Fabric).pairPorts(t, ranks[0], nil)
		if err != nil {
			return nil, fmt.Errorf("rank %d: %w", i+1, err)
		}
		if i == 0 {
			master = se
		} else if se.PortID != master.PortID {
			return nil, fmt.Errorf("rank %d (%s) reaches %s on %s, other ranks on %s: every host of a collective must sit on one fabric", i+1, ce.Host, se.Host, se.IP, master.IP)
		}
		fabric := se.Fabric
		if fabric == "" {
			fabric = ce.Fabric
		}
		for _, e := range []endpoint{ce, se} {
			if e.ExpectedGbps > 0 && (exp == 0 || e.ExpectedGbps < exp) {
				exp = e.ExpectedGbps
			}
		}
		plans = append(plans, &legPlan{client: ce, server: se, leg: api.Leg{
			Index: i, ClientHost: ce.Host, ClientIP: ce.IP, ServerHost: se.Host, ServerIP: se.IP, Fabric: fabric, MsgSize: spec.MsgSize}})
	}
	for _, p := range plans {
		p.leg.ExpectedGbps = exp
	}
	return plans, nil
}

// fairShare caps each leg's expected rate by the ports it shares. A port
// used by n legs of the same stage can give each of them at most 1/n of
// its line rate: an incast of eight 100G hosts into one 100G port expects
// 12.5 Gbps per leg, a fan-out from a 400G port to four 100G hosts still
// expects 100 each. Bidirectional rates add both directions, so line rate
// counts twice; that is applied last.
func fairShare(plans []*legPlan, bidir bool) {
	type use struct {
		n    int
		rate float64
		name string
	}
	byStage := map[int]map[int64]*use{}
	for _, p := range plans {
		m := byStage[p.stage]
		if m == nil {
			m = map[int64]*use{}
			byStage[p.stage] = m
		}
		for _, e := range []endpoint{p.client, p.server} {
			u := m[e.PortID]
			if u == nil {
				u = &use{rate: e.ExpectedGbps, name: e.Host + " " + e.IP}
				m[e.PortID] = u
			}
			u.n++
		}
	}
	for _, p := range plans {
		for _, e := range []endpoint{p.client, p.server} {
			u := byStage[p.stage][e.PortID]
			if u == nil || u.n < 2 || u.rate <= 0 {
				continue
			}
			share := u.rate / float64(u.n)
			if p.leg.ExpectedGbps == 0 || share < p.leg.ExpectedGbps {
				p.leg.ExpectedGbps = share
				p.leg.SharedPort = fmt.Sprintf("%s x%d", u.name, u.n)
			}
		}
		if bidir {
			p.leg.ExpectedGbps *= 2
		}
	}
}

func stagesOf(plans []*legPlan) [][]*legPlan {
	var stages [][]*legPlan
	for _, p := range plans {
		for len(stages) <= p.stage {
			stages = append(stages, nil)
		}
		stages[p.stage] = append(stages[p.stage], p)
	}
	return stages
}

// Start validates and queues a run, then executes it in the background.
// Preview plans a spec as Start would and returns its legs.
func (o *Orchestrator) Preview(ctx context.Context, spec api.RunSpec) ([]api.LegPreview, error) {
	if err := applyDefaults(&spec); err != nil {
		return nil, &userError{err}
	}
	hosts, err := o.store.Hosts(ctx)
	if err != nil {
		return nil, err
	}
	hosts = approvedHosts(hosts)
	if o.lldp != nil {
		o.lldp(ctx, hosts)
	}
	plans, err := plan(hosts, spec)
	if err != nil {
		return nil, &userError{err}
	}
	name := func(e endpoint) string {
		if e.Label != "" {
			return e.Label
		}
		return e.Netdev
	}
	out := make([]api.LegPreview, len(plans))
	for i, p := range plans {
		out[i] = api.LegPreview{Stage: p.stage, Client: p.client.Host, ClientPort: name(p.client), ClientIP: p.client.IP,
			Server: p.server.Host, ServerPort: name(p.server), ServerIP: p.server.IP, Fabric: p.leg.Fabric, MsgSize: p.leg.MsgSize, ExpectedGbps: p.leg.ExpectedGbps}
	}
	return out, nil
}

func (o *Orchestrator) Start(ctx context.Context, spec api.RunSpec) (int64, error) {
	if err := applyDefaults(&spec); err != nil {
		return 0, &userError{err}
	}
	hosts, err := o.store.Hosts(ctx)
	if err != nil {
		return 0, err
	}
	hosts = approvedHosts(hosts)
	if o.lldp != nil {
		o.lldp(ctx, hosts)
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
			if o.netplan[e.AgentID] {
				o.mu.Unlock()
				return 0, &userError{fmt.Errorf("the network setup of %s is being changed; try again in a few minutes", e.Host)}
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
		if api.IsCollective(spec.Test) && i > 0 {
			o.tcpPorts[p.server.AgentID][p.tcpPort] = false
			p.tcpPort = plans[0].tcpPort // every rank meets at rank 0's one port
		}
	}
	runCtx, cancel := context.WithCancel(context.Background())
	ar := &activeRun{id: runID, spec: spec, plans: plans, stages: stagesOf(plans), cancel: cancel}
	o.active[runID] = ar
	o.mu.Unlock()
	if o.fabric != nil {
		ids := map[int64]bool{}
		for _, p := range plans {
			ids[p.client.PortID], ids[p.server.PortID] = true, true
		}
		ar.fabric = o.fabric(ctx, ids)
	}

	o.store.Audit(ctx, "run.start", map[string]any{"run_id": runID, "spec": spec})
	go o.execute(runCtx, ar)
	return runID, nil
}

type userError struct{ error }

// approvedHosts drops hosts still waiting for approval (Phase 8): they are
// visible, but no test runs on them until an admin lets them in.
func approvedHosts(hosts []hostRec) []hostRec {
	out := hosts[:0:0]
	for _, h := range hosts {
		if h.Approved {
			out = append(out, h)
		}
	}
	return out
}

// hostBusy reports whether any active run uses a port on the agent.
func (o *Orchestrator) hostBusy(agentID int64) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, ar := range o.active {
		for _, p := range ar.plans {
			if p.client.AgentID == agentID || p.server.AgentID == agentID {
				return true
			}
		}
	}
	return false
}

// claimNetplan marks a host as having a network change under way, so no run
// starts on it; it fails while a run uses the host or another change runs.
func (o *Orchestrator) claimNetplan(agentID int64) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.netplan[agentID] {
		return errors.New("another network change on this host is still running")
	}
	for _, ar := range o.active {
		for _, p := range ar.plans {
			if p.client.AgentID == agentID || p.server.AgentID == agentID {
				return fmt.Errorf("run %d is using this host; change the network when it ends", ar.id)
			}
		}
	}
	o.netplan[agentID] = true
	return nil
}

func (o *Orchestrator) releaseNetplan(agentID int64) {
	o.mu.Lock()
	delete(o.netplan, agentID)
	o.mu.Unlock()
}

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
	o.store.SetRunStatus(bg, ar.id, "running")
	status, errMsg := "done", ""
	for i, stage := range ar.stages {
		if err := o.runStage(ctx, ar, stage); err != nil {
			if ctx.Err() != nil {
				status, errMsg = "canceled", "canceled by operator"
			} else {
				status, errMsg = "failed", fmt.Sprintf("stage %d of %d: %v", i+1, len(ar.stages), err)
			}
			break
		}
		if len(ar.stages) > 1 && i < len(ar.stages)-1 {
			select {
			case <-ctx.Done():
				status, errMsg = "canceled", "canceled by operator"
			case <-time.After(2 * time.Second): // let servers release their ports
			}
			if status != "done" {
				break
			}
		}
	}
	o.sendCancels(ar) // stops any stragglers; a no-op on agents with nothing running
	sum, verdict := o.score(ar)
	ar.summary = sum
	ar.summary.FabricIssues = ar.fabric
	if status != "done" {
		verdict = "fail"
	} else {
		verdict = o.applyBaseline(bg, ar, verdict)
	}
	for _, p := range ar.plans {
		o.store.SaveLeg(bg, p.leg, p.clientRes, p.serverRes)
	}
	o.store.FinishRun(bg, ar.id, status, verdict, errMsg, ar.summary)
	if status == "done" {
		o.publishResult(bg, ar.id)
	}
	log.Printf("run %d %s: %s, %.1f Gbps aggregate", ar.id, status, verdict, sum.ToolGbps)
	if o.onFinish != nil {
		if r, err := o.store.Run(bg, ar.id); err == nil {
			o.onFinish(r)
		}
	}
}

// runStage starts the servers of one stage, then its clients at a shared time,
// and waits for their results. Legs that never report keep nil results.
func (o *Orchestrator) runStage(ctx context.Context, ar *activeRun, plans []*legPlan) error {
	spec := ar.spec
	if api.IsCollective(spec.Test) {
		return o.runCollective(ctx, ar, plans)
	}
	inbox := make(chan msg, len(plans)*8)
	byTask := map[string]*legPlan{}
	var taskIDs []string
	defer func() { o.hub.forget(taskIDs...) }()

	base := api.Task{RunID: ar.id, Tool: o.tool(spec), DurationSec: spec.DurationSec, QPs: spec.QPs, TOS: spec.TOS, MTU: spec.MTU, Bidir: spec.Bidir, Streams: spec.Streams}
	if api.IsIP(spec.Test) {
		base.Protocol = spec.Test
	}

	// 1. Start every server and wait until all are listening.
	for _, p := range plans {
		t := base
		t.ID, t.Kind, t.LegID, t.MsgSize = newID(), api.TaskServer, p.leg.ID, p.leg.MsgSize
		t.RdmaDev, t.PortNum, t.Netdev, t.LocalIP, t.PeerIP, t.TCPPort = p.server.RdmaDev, p.server.PortNum, p.server.Netdev, p.server.IP, p.client.IP, p.tcpPort
		p.serverTask = t
		byTask[t.ID] = p
		taskIDs = append(taskIDs, t.ID)
		o.hub.send(p.server.AgentID, t, inbox)
	}
	readyBy := time.After(30 * time.Second)
	for waiting := len(plans); waiting > 0; {
		select {
		case <-ctx.Done():
			o.sendCancels(ar)
			return ctx.Err()
		case <-readyBy:
			o.sendCancels(ar)
			return fmt.Errorf("%d server(s) did not report ready within 30 s", waiting)
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
				o.sendCancels(ar)
				return fmt.Errorf("server %s %s: %s", p.server.Host, p.server.IP, msg)
			}
		}
	}

	// 2. Start every client at one shared time so aggregate load is simultaneous.
	startAt := time.Now().Add(3 * time.Second)
	for _, p := range plans {
		p.startAt = startAt
		t := base
		t.ID, t.Kind, t.LegID, t.MsgSize, t.StartAt = newID(), api.TaskClient, p.leg.ID, p.leg.MsgSize, startAt
		t.RdmaDev, t.PortNum, t.Netdev, t.LocalIP, t.PeerIP, t.TCPPort = p.client.RdmaDev, p.client.PortNum, p.client.Netdev, p.client.IP, p.server.IP, p.tcpPort
		p.clientTask = t
		byTask[t.ID] = p
		taskIDs = append(taskIDs, t.ID)
		o.hub.send(p.client.AgentID, t, inbox)
	}

	// 3. Collect results. Servers normally exit with their client; any still
	// running 15 s after the last client are canceled.
	deadline := time.After(time.Until(startAt) + time.Duration(spec.DurationSec)*time.Second + 90*time.Second)
	var grace <-chan time.Time
	var err error
	for !allDone(plans) {
		select {
		case <-ctx.Done():
			if err == nil {
				err = ctx.Err()
				o.sendCancels(ar)
				grace = time.After(15 * time.Second)
			}
			ctx = context.Background()
		case <-deadline:
			o.sendCancels(ar)
			grace = time.After(0)
		case <-grace:
			return err
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
			if grace == nil && allClientsDone(plans) {
				grace = time.After(15 * time.Second)
			}
		}
	}
	return err
}

// runCollective runs one NCCL collective over every rank. Rank 0 is started
// as the server task (its process opens the rendezvous at once and waits up
// to two minutes for the others), the other ranks start together as clients
// once rank 0 reports ready. All ranks report the same collective-wide
// figures, so rank 0's result becomes the server result of every leg.
func (o *Orchestrator) runCollective(ctx context.Context, ar *activeRun, plans []*legPlan) error {
	spec := ar.spec
	inbox := make(chan msg, (len(plans)+1)*8)
	byTask := map[string]*legPlan{}
	var taskIDs []string
	defer func() { o.hub.forget(taskIDs...) }()

	master := plans[0].server
	sizes := spec.Sizes
	if len(sizes) == 0 {
		sizes = []int{spec.MsgSize}
	}
	base := api.Task{RunID: ar.id, Tool: api.ToolNCCL, DurationSec: spec.DurationSec, MsgSize: spec.MsgSize, TOS: spec.TOS, MTU: spec.MTU,
		TCPPort: plans[0].tcpPort, MasterIP: master.IP, WorldSize: len(plans) + 1, Collective: strings.TrimPrefix(spec.Test, "nccl_"), Sizes: sizes}

	t0 := base
	t0.ID, t0.Kind, t0.LegID, t0.Rank = newID(), api.TaskServer, plans[0].leg.ID, 0
	t0.RdmaDev, t0.PortNum, t0.Netdev, t0.LocalIP, t0.PeerIP = master.RdmaDev, master.PortNum, master.Netdev, master.IP, master.IP
	for _, p := range plans {
		p.serverTask = t0
	}
	byTask[t0.ID] = plans[0]
	taskIDs = append(taskIDs, t0.ID)
	o.hub.send(master.AgentID, t0, inbox)
	readyBy := time.After(30 * time.Second)
	for ready := false; !ready; {
		select {
		case <-ctx.Done():
			o.sendCancels(ar)
			return ctx.Err()
		case <-readyBy:
			o.sendCancels(ar)
			return errors.New("rank 0 did not report ready within 30 s")
		case m := <-inbox:
			if m.event != nil && m.event.Type == api.EventServerReady {
				ready = true
			}
			if (m.event != nil && m.event.Type == api.EventFailed) || m.result != nil {
				msg := "rank 0 exited early"
				if m.event != nil {
					msg = m.event.Message
				} else if m.result.Error != "" {
					msg = m.result.Error
				}
				o.sendCancels(ar)
				return fmt.Errorf("rank 0 on %s: %s", master.Host, msg)
			}
		}
	}

	startAt := time.Now().Add(3 * time.Second)
	for i, p := range plans {
		p.startAt = startAt
		t := base
		t.ID, t.Kind, t.LegID, t.StartAt, t.Rank = newID(), api.TaskClient, p.leg.ID, startAt, i+1
		t.RdmaDev, t.PortNum, t.Netdev, t.LocalIP, t.PeerIP = p.client.RdmaDev, p.client.PortNum, p.client.Netdev, p.client.IP, master.IP
		p.clientTask = t
		byTask[t.ID] = p
		taskIDs = append(taskIDs, t.ID)
		o.hub.send(p.client.AgentID, t, inbox)
	}

	deadline := time.After(time.Until(startAt) + time.Duration(spec.DurationSec)*time.Second + 150*time.Second)
	var grace <-chan time.Time
	var err error
	for !allDone(plans) {
		select {
		case <-ctx.Done():
			if err == nil {
				err = ctx.Err()
				o.sendCancels(ar)
				grace = time.After(15 * time.Second)
			}
			ctx = context.Background()
		case <-deadline:
			o.sendCancels(ar)
			grace = time.After(0)
		case <-grace:
			return err
		case m := <-inbox:
			if m.result == nil {
				continue
			}
			if m.taskID == t0.ID {
				for _, p := range plans {
					p.serverRes = m.result
				}
			} else if p := byTask[m.taskID]; p != nil {
				p.clientRes = m.result
			}
			if grace == nil && allClientsDone(plans) {
				grace = time.After(15 * time.Second)
			}
		}
	}
	return err
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

// counterDir is which byte counter on a port measures a leg.
type counterDir int

const (
	dirRx counterDir = iota
	dirTx
	dirBoth // bidirectional: tx plus rx, to match the tool's combined figure
)

// counterSide picks the port whose byte counter measures one leg alone within
// its stage: the server port in a fan-out, the client port in a fan-in. dir is
// the direction data flows on that port; nic selects netdev counters for IP tests.
func counterSide(ar *activeRun, p *legPlan) (portID int64, dir counterDir, nic, ok bool) {
	uses := map[int64]int{}
	for _, q := range ar.stages[p.stage] {
		uses[q.client.PortID]++
		uses[q.server.PortID]++
	}
	nic = api.IsIP(ar.spec.Test)
	// write/send/tcp/udp: data flows client -> server. read: server -> client.
	serverDir, clientDir := dirRx, dirTx
	if ar.spec.Test == "read" || ar.spec.Test == "read_lat" {
		serverDir, clientDir = dirTx, dirRx
	}
	if ar.spec.Bidir {
		serverDir, clientDir = dirBoth, dirBoth
	}
	if api.IsCollective(ar.spec.Test) {
		// A ring sends each rank's share onward: what a rank transmits over
		// the collective is its bus bandwidth, so the client port's tx rate
		// is the figure to hold against NCCL's own.
		return p.client.PortID, dirTx, nic, true
	}
	if uses[p.server.PortID] == 1 {
		return p.server.PortID, serverDir, nic, true
	}
	if uses[p.client.PortID] == 1 {
		return p.client.PortID, clientDir, nic, true
	}
	return 0, dirRx, nic, false
}

// congestionKeys are the host counters that show the fabric pushing back:
// ECN marks received, CNPs sent (notification point) and handled (reaction
// point). They are reported, never a fault; a lossless fabric is expected to
// produce them under load.
var congestionKeys = map[string]string{"np_ecn_marked_roce_packets": "ecn marked", "np_cnp_sent": "cnp sent", "rp_cnp_handled": "cnp handled"}

func (o *Orchestrator) score(ar *activeRun) (api.RunSummary, string) {
	sum := api.RunSummary{ByFabric: map[string]float64{}, Congestion: map[string]int64{}}
	rank := map[string]int{"pass": 0, "warn": 1, "fail": 2}
	worst := "pass"
	lat := api.IsLatency(ar.spec.Test)
	collective := api.IsCollective(ar.spec.Test)
	minRate, maxRate := -1.0, 0.0
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
		if c := p.clientRes; c != nil {
			l.ToolGbps, l.Latency, l.Retransmits, l.LossPct, l.JitterMs, l.Points = c.AvgGbps, c.Latency, c.Retransmits, c.LossPct, c.JitterMs, c.Points
		}
		if l.ToolGbps == 0 && p.serverRes != nil {
			l.ToolGbps, l.Points = p.serverRes.AvgGbps, p.serverRes.Points
		}
		if !lat && !p.startAt.IsZero() && p.clientRes != nil && p.clientRes.Error == "" {
			from := p.startAt.Add(2 * time.Second)
			to := p.startAt.Add(time.Duration(ar.spec.DurationSec-1) * time.Second)
			if n := len(ar.spec.Sizes); collective && n > 1 {
				// A collective sweep runs every size in one process; the tool
				// figure is the largest size's, so count that slice alone.
				from = p.startAt.Add(time.Duration(float64(ar.spec.DurationSec)*float64(n-1)/float64(n))*time.Second + 2*time.Second)
			}
			if id, dir, nic, ok := counterSide(ar, p); ok {
				l.CounterGbps = o.metrics.counterGbps(id, from, to, dir, nic)
			} else {
				l.Note = "both ports shared with other legs; no per-leg counter rate"
			}
		}
		if !p.startAt.IsZero() {
			// The switch's view of the leg and the hosts' CPU over the whole test.
			from, to := p.startAt, p.startAt.Add(time.Duration(ar.spec.DurationSec)*time.Second)
			l.Switch = o.legSwitch(ar, p, from, to)
			l.CPU = o.legCPU(p, from, to)
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
		case lat && l.Latency == nil:
			l.Verdict, l.Note = "fail", "no latency result"
		case lat && ar.spec.MaxLatP99Us > 0 && l.Latency.P99Us > ar.spec.MaxLatP99Us:
			l.Verdict, l.Note = "fail", fmt.Sprintf("p99 %.2f µs is above %.2f µs", l.Latency.P99Us, ar.spec.MaxLatP99Us)
		case ar.spec.Test == "udp" && l.LossPct > ar.spec.MaxLossPct:
			l.Verdict, l.Note = "fail", fmt.Sprintf("%.2f%% loss is above %.2f%%", l.LossPct, ar.spec.MaxLossPct)
		case !lat && ar.spec.Test != "udp" && l.ExpectedGbps > 0 && measured < ar.spec.PassPct/100*l.ExpectedGbps:
			l.Verdict = "fail"
			l.Note = fmt.Sprintf("%.1f Gbps is below %.0f%% of %.0f Gbps", measured, ar.spec.PassPct, l.ExpectedGbps)
		}
		if l.Verdict == "pass" {
			var bad, expected []string
			for k, v := range l.ErrorDeltas {
				switch {
				case v <= 0 || !errorCounterKeys[k]:
				case counterIsError(ar.spec.Test, k):
					bad = append(bad, fmt.Sprintf("%s +%d", k, v))
				default:
					expected = append(expected, fmt.Sprintf("%s +%d", k, v))
				}
			}
			if len(bad) > 0 {
				sort.Strings(bad)
				l.Verdict, l.Note = "warn", "error counters grew: "+strings.Join(bad, ", ")
			} else if len(expected) > 0 {
				sort.Strings(expected)
				l.Note = "normal for a send test: " + strings.Join(expected, ", ")
			} else if ar.spec.Test == "tcp" && l.Retransmits > 0 {
				l.Verdict, l.Note = "warn", fmt.Sprintf("%d TCP retransmits", l.Retransmits)
			}
		}
		if rank[l.Verdict] > rank[worst] {
			worst = l.Verdict
		}
		// Aggregates sum the legs of the last stage: in a sweep that is the
		// largest size, and in a serial matrix it is the last link alone.
		if p.stage == len(ar.stages)-1 {
			sum.ToolGbps += l.ToolGbps
			sum.CounterGbps += l.CounterGbps
			f := l.Fabric
			if f == "" {
				f = "unassigned"
			}
			sum.ByFabric[f] += l.ToolGbps
			if !lat && l.ToolGbps > 0 {
				if minRate < 0 || l.ToolGbps < minRate {
					minRate = l.ToolGbps
				}
				maxRate = max(maxRate, l.ToolGbps)
			}
		}
		for k, name := range congestionKeys {
			if v := l.ErrorDeltas[k]; v > 0 {
				sum.Congestion[name] += v
			}
		}
		if l.Switch != nil {
			for k, v := range l.Switch.Deltas {
				switch {
				case strings.Contains(k, "dropped_pkts") || strings.Contains(k, "discards"):
					sum.Congestion["switch drops"] += int64(v)
				case strings.Contains(k, "pause_frames"):
					sum.Congestion["switch pauses"] += int64(v)
				case strings.Contains(k, "ecn_marked"):
					sum.Congestion["switch ecn marked"] += int64(v)
				}
			}
		}
	}
	if maxRate > 0 && minRate >= 0 && len(ar.stages[len(ar.stages)-1]) > 1 && !collective {
		sum.Fairness = minRate / maxRate
	}
	if collective {
		// Every rank reports the same collective-wide bus bandwidth; the
		// run's figure is that, not a sum over ranks.
		sum.ToolGbps, sum.CounterGbps = 0, 0
		for f := range sum.ByFabric {
			delete(sum.ByFabric, f)
		}
		for _, p := range ar.plans {
			if p.leg.ToolGbps > sum.ToolGbps {
				sum.ToolGbps = p.leg.ToolGbps
			}
			sum.CounterGbps += p.leg.CounterGbps
		}
		if len(ar.plans) > 0 {
			sum.CounterGbps /= float64(len(ar.plans))
			f := ar.plans[0].leg.Fabric
			if f == "" {
				f = "unassigned"
			}
			sum.ByFabric[f] = sum.ToolGbps
		}
	}
	if len(sum.Congestion) == 0 {
		sum.Congestion = nil
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
		if p.startAt.IsZero() || (p.clientRes != nil && p.serverRes != nil) {
			continue // not in the running stage
		}
		if id, dir, nic, ok := counterSide(ar, p); ok {
			tx, rx := o.metrics.live(id)
			if nic {
				tx, rx = o.metrics.liveNIC(id)
			}
			switch dir {
			case dirTx:
				r.Legs[i].LiveGbps = tx
			case dirRx:
				r.Legs[i].LiveGbps = rx
			default:
				r.Legs[i].LiveGbps = tx + rx
			}
			r.Summary.LiveGbps += r.Legs[i].LiveGbps
		}
	}
}

// ActiveLegs lists the legs of every run in progress with their live rate,
// for the Topology page.
func (o *Orchestrator) ActiveLegs() []api.TopoRun {
	o.mu.Lock()
	ids := make([]int64, 0, len(o.active))
	for id := range o.active {
		ids = append(ids, id)
	}
	o.mu.Unlock()
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := []api.TopoRun{}
	for _, id := range ids {
		run := api.Run{ID: id, Legs: nil}
		o.mu.Lock()
		ar, ok := o.active[id]
		o.mu.Unlock()
		if !ok {
			continue
		}
		tr := api.TopoRun{ID: id, Name: ar.spec.Name, Legs: []api.TopoLeg{}}
		for _, p := range ar.plans {
			run.Legs = append(run.Legs, p.leg)
		}
		o.Live(&run)
		for i, p := range ar.plans {
			if p.startAt.IsZero() || (p.clientRes != nil && p.serverRes != nil) {
				continue
			}
			tr.Legs = append(tr.Legs, api.TopoLeg{ClientPortID: p.client.PortID, ServerPortID: p.server.PortID, ClientHost: p.client.Host, ServerHost: p.server.Host, LiveGbps: run.Legs[i].LiveGbps})
		}
		out = append(out, tr)
	}
	return out
}

func newID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}
