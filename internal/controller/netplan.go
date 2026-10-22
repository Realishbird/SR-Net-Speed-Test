package controller

// Network setup of RDMA ports: the per-port editor on the Hosts page, and
// the network plan that creates fabrics from the switches' VLANs and gives
// every cabled port an address.
//
// The controller never touches a host's files itself. It sends a netplan
// task; the agent hands it to the root helper install.sh adds, which
// applies it and puts everything back unless the port comes up with its
// address and MTU, a fabric peer answers a ping from it, and the agent
// still reaches the controller (see internal/agent/netplan.go). Switches
// are only read: a VLAN or MTU the plan wants on a switch port comes back
// as sonic-cli lines for an admin to paste.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/netip"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Realishbird/SR-Net-Speed-Test/internal/api"
)

func (s *Server) routesNetplan(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/hosts/{id}/netplan", s.view(s.getHostNetplan))
	mux.HandleFunc("POST /api/v1/hosts/{id}/netplan", s.admin(s.postHostNetplan))
	mux.HandleFunc("POST /api/v1/hosts/{id}/netplan/preview", s.view(s.previewHostNetplan))
	mux.HandleFunc("POST /api/v1/hosts/{id}/netplan/move", s.admin(s.postHostNetplanMove))
	mux.HandleFunc("GET /api/v1/netplan/jobs", s.view(func(w http.ResponseWriter, r *http.Request) { writeJSON(w, s.netplan.list(0)) }))
	mux.HandleFunc("GET /api/v1/network/plan", s.view(s.getNetworkPlan))
	mux.HandleFunc("POST /api/v1/network/plan/apply", s.admin(s.applyNetworkPlan))
}

// netplanJobs keeps the latest network changes, newest last. They live in
// memory: the audit log has the lasting record.
type netplanJobs struct {
	mu   sync.Mutex
	jobs []*api.NetplanJob
	last int64
}

func newNetplanJobs() *netplanJobs { return &netplanJobs{} }

// add gives a job its id. Ids come from the clock so a restarted controller
// never reuses one: the helper on a host skips a change id it has seen.
func (n *netplanJobs) add(j *api.NetplanJob) {
	n.mu.Lock()
	defer n.mu.Unlock()
	id := time.Now().UnixMilli()
	if id <= n.last {
		id = n.last + 1
	}
	n.last, j.ID = id, id
	n.jobs = append(n.jobs, j)
	if len(n.jobs) > 100 {
		n.jobs = n.jobs[len(n.jobs)-100:]
	}
}

func (n *netplanJobs) update(j *api.NetplanJob, f func(j *api.NetplanJob)) {
	n.mu.Lock()
	f(j)
	n.mu.Unlock()
}

// list copies the jobs of one host (0: all), newest first.
func (n *netplanJobs) list(hostID int64) []api.NetplanJob {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := []api.NetplanJob{}
	for i := len(n.jobs) - 1; i >= 0; i-- {
		if hostID == 0 || n.jobs[i].HostID == hostID {
			out = append(out, *n.jobs[i])
		}
	}
	return out
}

// pending says whether a host has a change queued or running.
func (n *netplanJobs) pending(hostID int64) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, j := range n.jobs {
		if j.HostID == hostID && (j.Status == "queued" || j.Status == "running") {
			return true
		}
	}
	return false
}

func (s *Server) hostByID(ctx context.Context, r *http.Request) (hostRec, []hostRec, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return hostRec{}, nil, &userError{errors.New("bad host id")}
	}
	hosts, err := s.store.Hosts(ctx)
	if err != nil {
		return hostRec{}, nil, err
	}
	for _, h := range hosts {
		if h.ID == id {
			return h, hosts, nil
		}
	}
	return hostRec{}, nil, &userError{errors.New("no such host")}
}

func netplanHTTPError(w http.ResponseWriter, err error) {
	var ue *userError
	if errors.As(err, &ue) {
		httpError(w, http.StatusBadRequest, ue.Error())
		return
	}
	serverError(w, err)
}

// hostNetplan is what the Network editor shows for a host.
type hostNetplan struct {
	State *api.NetplanState `json:"state"`
	Ports []hostNetplanPort `json:"ports"`
	Jobs  []api.NetplanJob  `json:"jobs"`
}

type hostNetplanPort struct {
	PortID  int64            `json:"port_id"`
	Netdev  string           `json:"netdev"`
	Addrs   []string         `json:"addrs"`
	MTU     int              `json:"mtu"`
	State   string           `json:"state"`
	Netplan *api.PortNetplan `json:"netplan,omitempty"`
}

func (s *Server) getHostNetplan(w http.ResponseWriter, r *http.Request) {
	h, _, err := s.hostByID(r.Context(), r)
	if err != nil {
		netplanHTTPError(w, err)
		return
	}
	out := hostNetplan{State: h.Netplan, Ports: []hostNetplanPort{}, Jobs: s.netplan.list(h.ID)}
	for _, p := range h.Ports {
		out.Ports = append(out.Ports, hostNetplanPort{PortID: p.ID, Netdev: p.Netdev, Addrs: p.Addrs, MTU: p.MTU, State: p.State, Netplan: p.Netplan})
	}
	writeJSON(w, out)
}

// prepareChange checks a request against the host and the fleet and turns
// it into the change the helper gets, with the fabric peers each port must
// reach after it.
func prepareChange(h hostRec, hosts []hostRec, req api.NetplanRequest) (api.NetplanChange, error) {
	ch := api.NetplanChange{Ports: req.Ports, TimeoutSec: req.TimeoutSec}
	if ch.TimeoutSec == 0 {
		ch.TimeoutSec = 90
	}
	if ch.TimeoutSec < 30 || ch.TimeoutSec > 300 {
		return ch, &userError{errors.New("timeout_sec is 30 to 300")}
	}
	byNetdev := map[string]api.HostPort{}
	for _, p := range h.Ports {
		byNetdev[p.Netdev] = p
	}
	// The change rewrites every port it lists, so an address may move
	// between them (Validate refuses one given to two ports).
	inChange := map[int64]bool{}
	for _, p := range ch.Ports {
		if hp, ok := byNetdev[p.Netdev]; ok {
			inChange[hp.ID] = true
		}
	}
	for i := range ch.Ports {
		p := &ch.Ports[i]
		p.Peers = nil // the controller picks them
		hp, ok := byNetdev[p.Netdev]
		if !ok {
			return ch, &userError{fmt.Errorf("%s has no RDMA port %s", h.Name, p.Netdev)}
		}
		if hp.Netplan != nil && hp.Netplan.Mgmt {
			return ch, &userError{fmt.Errorf("%s %s carries the management or controller connection and is never changed from here", h.Name, p.Netdev)}
		}
		for j, a := range p.Addresses {
			a = strings.TrimSpace(a)
			p.Addresses[j] = a
			pf, err := netip.ParsePrefix(a)
			if err != nil {
				continue // Validate words the error
			}
			if other := addressOwner(hosts, pf.Addr(), inChange); other != "" {
				return ch, &userError{fmt.Errorf("%s is already on %s", pf.Addr(), other)}
			}
			p.Peers = appendPeers(p.Peers, hosts, h.ID, pf)
		}
	}
	if err := ch.Validate(); err != nil {
		return ch, &userError{err}
	}
	return ch, nil
}

// addressOwner names the port that holds addr, other than the ports in skip.
func addressOwner(hosts []hostRec, addr netip.Addr, skip map[int64]bool) string {
	for _, h := range hosts {
		for _, p := range h.Ports {
			if skip[p.ID] {
				continue
			}
			for _, a := range p.Addrs {
				if pf, err := netip.ParsePrefix(a); err == nil && pf.Addr() == addr {
					return h.Name + " " + p.Netdev
				}
			}
		}
	}
	return ""
}

// appendPeers adds up to three addresses of other hosts in the subnet,
// lowest first: in a fabric numbered like this lab the servers sit at the
// bottom of the range and are the ones most likely to be up.
func appendPeers(peers []string, hosts []hostRec, self int64, subnet netip.Prefix) []string {
	subnet = subnet.Masked()
	var cands []netip.Addr
	for _, h := range hosts {
		if h.ID == self {
			continue
		}
		for _, p := range h.Ports {
			for _, a := range p.Addrs {
				if pf, err := netip.ParsePrefix(a); err == nil && subnet.Contains(pf.Addr()) {
					cands = append(cands, pf.Addr())
				}
			}
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].Less(cands[j]) })
	for _, c := range cands {
		if len(peers) >= 3 {
			break
		}
		dup := false
		for _, p := range peers {
			dup = dup || p == c.String()
		}
		if !dup {
			peers = append(peers, c.String())
		}
	}
	return peers
}

func (s *Server) postHostNetplan(w http.ResponseWriter, r *http.Request) {
	var req api.NetplanRequest
	if !readJSON(w, r, &req) {
		return
	}
	h, hosts, err := s.hostByID(r.Context(), r)
	if err != nil {
		netplanHTTPError(w, err)
		return
	}
	ch, err := prepareChange(h, hosts, req)
	if err != nil {
		netplanHTTPError(w, err)
		return
	}
	if err := s.netplanReady(h); err != nil {
		httpError(w, http.StatusConflict, err.Error())
		return
	}
	if err := s.orch.claimNetplan(h.ID); err != nil {
		httpError(w, http.StatusConflict, err.Error())
		return
	}
	job := &api.NetplanJob{HostID: h.ID, Host: h.Name, Ports: ch.Ports, Status: "running", By: who(r).Name, Created: time.Now()}
	s.netplan.add(job)
	go func() {
		defer s.orch.releaseNetplan(h.ID)
		s.runNetplanJob(context.Background(), job, ch)
	}()
	writeJSONStatus(w, http.StatusAccepted, s.netplan.list(h.ID)[0])
}

// netplanMove asks to move a port's addresses to another port of the host.
type netplanMove struct {
	From       string `json:"from"`
	To         string `json:"to"`
	TimeoutSec int    `json:"timeout_sec,omitempty"`
	Preview    bool   `json:"preview,omitempty"` // only render the netplan, for hosts without the helper
}

// moveRequest is the change that moves from's addresses and MTU to to in
// one netplan try: on a DGX Spark, from the first PCIe half of a cable to
// the second, so two cables stop sharing one PCIe link. The old port keeps
// no address, so its stanza stays (MTU, link up) with none listed.
func moveRequest(h hostRec, m netplanMove) (api.NetplanRequest, api.HostPort, api.HostPort, error) {
	var from, to api.HostPort
	var okF, okT bool
	for _, p := range h.Ports {
		switch p.Netdev {
		case m.From:
			from, okF = p, true
		case m.To:
			to, okT = p, true
		}
	}
	var addrs []string
	for _, a := range from.Addrs {
		if pf, err := netip.ParsePrefix(a); err == nil && !pf.Addr().IsLinkLocalUnicast() {
			addrs = append(addrs, a)
		}
	}
	switch {
	case !okF || !okT:
		return api.NetplanRequest{}, from, to, &userError{fmt.Errorf("%s has no RDMA ports %s and %s", h.Name, m.From, m.To)}
	case m.From == m.To:
		return api.NetplanRequest{}, from, to, &userError{errors.New("pick two different ports")}
	case len(addrs) == 0:
		return api.NetplanRequest{}, from, to, &userError{fmt.Errorf("%s has no address to move", m.From)}
	case len(to.Addrs) > 0:
		return api.NetplanRequest{}, from, to, &userError{fmt.Errorf("%s already has an address; edit it instead", m.To)}
	}
	mtu := from.MTU
	if mtu < 576 || mtu > 9216 {
		mtu = 0
	}
	return api.NetplanRequest{TimeoutSec: m.TimeoutSec, Ports: []api.NetplanPort{
		{Netdev: from.Netdev, Addresses: []string{}, MTU: mtu},
		{Netdev: to.Netdev, Addresses: addrs, MTU: mtu},
	}}, from, to, nil
}

// postHostNetplanMove moves a port's addresses to another port, and once
// the host confirms the change, its label, fabric and expected rate too,
// so runs and selectors find the port where it now is.
func (s *Server) postHostNetplanMove(w http.ResponseWriter, r *http.Request) {
	var m netplanMove
	if !readJSON(w, r, &m) {
		return
	}
	h, hosts, err := s.hostByID(r.Context(), r)
	if err != nil {
		netplanHTTPError(w, err)
		return
	}
	req, from, to, err := moveRequest(h, m)
	if err != nil {
		netplanHTTPError(w, err)
		return
	}
	ch, err := prepareChange(h, hosts, req)
	if err != nil {
		netplanHTTPError(w, err)
		return
	}
	if m.Preview {
		current := ""
		if h.Netplan != nil {
			current = h.Netplan.File
		}
		pv := renderNetplanPreview(current, ch, h.Ports)
		// By hand there is no helper to take the address off the old port,
		// and another file may still set it there.
		pv.Commands = fmt.Sprintf("grep -ln '%s' /etc/netplan/*.yaml   # a file listed besides 90-srnet.yaml still sets %s: remove its addresses there first\n", from.Netdev, from.Netdev) + pv.Commands
		for _, a := range req.Ports[1].Addresses {
			pv.Commands += fmt.Sprintf("sudo ip addr del %s dev %s   # netplan does not take a moved address off the old port\n", a, from.Netdev)
		}
		writeJSON(w, pv)
		return
	}
	if err := s.netplanReady(h); err != nil {
		httpError(w, http.StatusConflict, err.Error())
		return
	}
	if err := s.orch.claimNetplan(h.ID); err != nil {
		httpError(w, http.StatusConflict, err.Error())
		return
	}
	by := who(r).Name
	job := &api.NetplanJob{HostID: h.ID, Host: h.Name, Ports: ch.Ports, Status: "running", By: by, Created: time.Now()}
	s.netplan.add(job)
	go func() {
		defer s.orch.releaseNetplan(h.ID)
		ctx := context.Background()
		if s.runNetplanJob(ctx, job, ch) == "applied" {
			s.movePortSettings(ctx, from, to, by)
		}
	}()
	writeJSONStatus(w, http.StatusAccepted, s.netplan.list(h.ID)[0])
}

// movePortSettings carries a port's label, fabric and expected rate over
// to the port its address moved to.
func (s *Server) movePortSettings(ctx context.Context, from, to api.HostPort, by string) {
	if from.Label == "" && from.Fabric == "" && from.ExpectedGbps == 0 {
		return
	}
	label, fabric, exp := from.Label, from.Fabric, from.ExpectedGbps
	if err := s.store.UpdatePort(ctx, to.ID, api.PortUpdate{Label: &label, Fabric: &fabric, ExpectedGbps: &exp}); err != nil {
		log.Printf("netplan move: %s settings to %s: %v", from.Netdev, to.Netdev, err)
		return
	}
	empty, zero := "", 0.0
	if err := s.store.UpdatePort(ctx, from.ID, api.PortUpdate{Label: &empty, Fabric: &empty, ExpectedGbps: &zero}); err != nil {
		log.Printf("netplan move: clearing %s: %v", from.Netdev, err)
	}
	s.store.Audit(ctx, "port.move", map[string]any{"from": from.Netdev, "to": to.Netdev, "label": label, "fabric": fabric, "by": by})
}

// netplanReady says why a host cannot take a change now.
func (s *Server) netplanReady(h hostRec) error {
	switch {
	case !s.hub.online(h.ID):
		return fmt.Errorf("%s is offline", h.Name)
	case h.Netplan == nil || h.Netplan.Helper == "":
		return fmt.Errorf("%s has no netplan helper yet: re-run the install command on it once (Hosts page, Agent install command), or use Copy netplan", h.Name)
	case s.netplan.pending(h.ID):
		return fmt.Errorf("%s already has a network change under way", h.Name)
	}
	return nil
}

// runNetplanJob sends one change and waits for the helper's verdict, which
// it returns (applied, rolled_back, refused or failed).
func (s *Server) runNetplanJob(ctx context.Context, job *api.NetplanJob, ch api.NetplanChange) string {
	ch.ID = job.ID
	s.netplan.update(job, func(j *api.NetplanJob) { j.Status = "running" })
	s.store.Audit(ctx, "host.netplan.start", map[string]any{"job": job.ID, "host": job.Host, "ports": ch.Ports, "by": job.By, "batch": job.Batch})
	inbox := make(chan msg, 4)
	t := api.Task{ID: newID(), Kind: api.TaskNetplan, Netplan: &ch}
	defer s.hub.forget(t.ID)
	s.hub.send(job.HostID, t, inbox)
	status, message, steps := "failed", "", []string(nil)
	// The helper waits up to the change's timeout, plus netplan apply twice.
	select {
	case <-ctx.Done():
		message = "the controller stopped while waiting"
	case <-time.After(time.Duration(ch.TimeoutSec)*time.Second + 4*time.Minute):
		message = "no answer from the agent; the helper rolls back on its own if the change did not check out"
	case m := <-inbox:
		switch {
		case m.event != nil && m.event.Netplan != nil:
			status, message, steps = m.event.Netplan.Status, m.event.Netplan.Message, m.event.Netplan.Steps
		case m.event != nil:
			message = "agent answered: " + m.event.Message
		default:
			message = "agent answered without a result"
		}
	}
	now := time.Now()
	s.netplan.update(job, func(j *api.NetplanJob) { j.Status, j.Message, j.Steps, j.Done = status, message, steps, &now })
	s.store.Audit(ctx, "host.netplan.done", map[string]any{"job": job.ID, "host": job.Host, "ports": ch.Ports, "by": job.By, "status": status, "message": message, "steps": steps})
	return status
}

// previewHostNetplan renders the netplan file a request would leave, merged
// with what the helper reported of the host's managed file, and the lines
// to apply it by hand with netplan try (which rolls back unless confirmed).
func (s *Server) previewHostNetplan(w http.ResponseWriter, r *http.Request) {
	var req api.NetplanRequest
	if !readJSON(w, r, &req) {
		return
	}
	h, hosts, err := s.hostByID(r.Context(), r)
	if err != nil {
		netplanHTTPError(w, err)
		return
	}
	ch, err := prepareChange(h, hosts, req)
	if err != nil {
		netplanHTTPError(w, err)
		return
	}
	current := ""
	if h.Netplan != nil {
		current = h.Netplan.File
	}
	writeJSON(w, renderNetplanPreview(current, ch, h.Ports))
}

func renderNetplanPreview(current string, ch api.NetplanChange, ports []api.HostPort) api.NetplanPreview {
	var m struct {
		Network struct {
			Version   int                       `yaml:"version"`
			Ethernets map[string]map[string]any `yaml:"ethernets"`
		} `yaml:"network"`
	}
	yaml.Unmarshal([]byte(current), &m)
	if m.Network.Ethernets == nil {
		m.Network.Ethernets = map[string]map[string]any{}
	}
	m.Network.Version = 2
	var others []string
	for _, p := range ch.Ports {
		st := m.Network.Ethernets[p.Netdev]
		if st == nil {
			st = map[string]any{}
		}
		delete(st, "addresses")
		delete(st, "mtu")
		st["dhcp4"] = p.DHCP4
		if len(p.Addresses) > 0 {
			st["addresses"] = p.Addresses
		}
		if p.MTU > 0 {
			st["mtu"] = p.MTU
		}
		m.Network.Ethernets[p.Netdev] = st
		for _, hp := range ports {
			if hp.Netdev == p.Netdev && hp.Netplan != nil {
				for _, f := range hp.Netplan.Files {
					if f != "90-srnet.yaml" {
						others = append(others, fmt.Sprintf("%s (in %s)", p.Netdev, f))
					}
				}
			}
		}
	}
	var b strings.Builder
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	enc.Encode(m)
	enc.Close()
	body := "# Managed by SR-Net-Speed-Test. Change these ports from the controller's Hosts page;\n# edits made here are kept for keys the controller does not set (routes, for example).\n" + b.String()
	pv := api.NetplanPreview{File: "/etc/netplan/90-srnet.yaml", YAML: body}
	pv.Commands = "sudo cp -a /etc/netplan /root/netplan-backup-$(date +%Y%m%d-%H%M%S)\n" +
		"sudo tee /etc/netplan/90-srnet.yaml >/dev/null <<'SRNET'\n" + body + "SRNET\n" +
		"sudo chmod 600 /etc/netplan/90-srnet.yaml\n" +
		"sudo netplan try --timeout 120   # press Enter within 120 s to keep it; otherwise it rolls back\n"
	if len(others) > 0 {
		pv.Note = "Remove these ports from their other netplan files first, or their old addresses stay as well: " + strings.Join(others, ", ")
	}
	return pv
}

// Network plan.

func (s *Server) getNetworkPlan(w http.ResponseWriter, r *http.Request) {
	plan, _, err := s.networkPlan(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, plan)
}

func (s *Server) networkPlan(ctx context.Context) (api.NetworkPlan, []hostRec, error) {
	hosts, err := s.store.Hosts(ctx)
	if err != nil {
		return api.NetworkPlan{}, nil, err
	}
	s.fillLLDP(ctx, hosts)
	profiles, err := s.store.FabricProfiles(ctx)
	if err != nil {
		return api.NetworkPlan{}, nil, err
	}
	s.mu.Lock()
	vlans, qos, lldpAt, lldp, ifaces, naming := s.vlans, s.qos, s.lldpAt, s.lldp, s.ifaces, s.naming
	s.mu.Unlock()
	sws := planSwitches{vlans: vlans, qos: qos, known: knownIfaces(ifaces, lldp, vlans), naming: naming}
	plan := buildNetworkPlan(hosts, s.switchList(), sws, profiles, s.hub.online)
	if len(s.switchList()) == 0 {
		plan.Warnings = append(plan.Warnings, "No switch is configured: fabrics come from the switch's VLANs, so add the switch on the Switches page first.")
	} else if len(vlans) == 0 {
		plan.Warnings = append(plan.Warnings, "The switch's VLAN table has not been read yet (it is polled once a minute).")
	} else if !lldpAt.IsZero() && time.Since(lldpAt) > 5*time.Minute {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("The switch's LLDP and VLAN tables were last read %s ago.", time.Since(lldpAt).Round(time.Minute)))
	}
	return plan, hosts, nil
}

var netdevFunc = regexp.MustCompile(`f(\d+)(np\d+)?$`)

// pciFunction is the PCI function of a port: on a dual-port card the
// second port is function 1. From the PCI address, else the netdev name.
func pciFunction(p api.HostPort) int {
	if p.PCIe != nil {
		if i := strings.LastIndexByte(p.PCIe.Address, '.'); i >= 0 {
			if n, err := strconv.Atoi(p.PCIe.Address[i+1:]); err == nil {
				return n
			}
		}
	}
	if m := netdevFunc.FindStringSubmatch(p.Netdev); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	return 0
}

func pciAddress(p api.HostPort) string {
	if p.PCIe != nil {
		return p.PCIe.Address
	}
	return ""
}

// v4 arithmetic for the address picker.
func v4Offset(subnet netip.Prefix, a netip.Addr) int {
	if !a.Is4() || !subnet.Contains(a) {
		return -1
	}
	b, n := a.As4(), subnet.Masked().Addr().As4()
	return int(uint32(b[0])<<24|uint32(b[1])<<16|uint32(b[2])<<8|uint32(b[3])) - int(uint32(n[0])<<24|uint32(n[1])<<16|uint32(n[2])<<8|uint32(n[3]))
}

func v4At(subnet netip.Prefix, off int) (netip.Addr, bool) {
	if !subnet.Addr().Is4() || subnet.Bits() > 30 {
		return netip.Addr{}, false
	}
	size := 1 << (32 - subnet.Bits())
	if off < 1 || off > size-2 {
		return netip.Addr{}, false
	}
	n := subnet.Masked().Addr().As4()
	v := uint32(n[0])<<24 | uint32(n[1])<<16 | uint32(n[2])<<8 | uint32(n[3]) + uint32(off)
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}), true
}

// planCand is one cabled port while the plan is built.
type planCand struct {
	h       hostRec
	p       api.HostPort
	sw, vln string
}

// buildNetworkPlan works out the fabrics and the address of every cabled
// RDMA port:
//
//   - a fabric per switch VLAN with host ports in it (named after the VLAN
//     unless a profile already claims that VLAN); its subnet from the
//     profile, else the one most of its ports' addresses are in;
//   - a port is in the fabric of its switch port's VLAN; one whose own
//     fabric disagrees, or whose switch port is in no VLAN, is held back
//     and the switch lines to fix it are listed;
//   - an address that fits is kept; a port with none gets the host's
//     number plus 10 per PCI function (the lab numbering: servers .1 and
//     .2 on single-port cards, Spark port 1 at .10-.19 and port 2 at
//     .20-.29), or the next free address from .10 when that is taken;
//   - the MTU is raised to the fabric's host MTU (9000).
//
// The twin PCIe function of a Spark port (enP2p1s0f0np0, on the same
// switch port as enp1s0f0np0) and management ports are left out. Switch
// lines are only written for interfaces the switch itself lists, under the
// name its CLI takes.
func buildNetworkPlan(hosts []hostRec, switches []SwitchConfig, sws planSwitches, profiles []api.FabricProfile, online func(int64) bool) api.NetworkPlan {
	vlans, qos := sws.vlans, sws.qos
	plan := api.NetworkPlan{At: time.Now(), Warnings: []string{}, Fabrics: []api.PlanFabric{}, Ports: []api.PlanPort{}, Switch: []api.PlanSwitchFix{}, Commands: []api.PlanCommands{}}
	byFabric := map[string]api.FabricProfile{}
	vlanFabric := map[string]string{} // VLAN -> fabric whose profile names it
	for _, p := range profiles {
		byFabric[p.Fabric] = p
		if p.VLAN != "" {
			vlanFabric[p.VLAN] = p.Fabric
		}
	}
	fabricOfVLAN := func(v string) string {
		if f, ok := vlanFabric[v]; ok {
			return f
		}
		return v
	}
	vlanOfFabric := func(f string) string {
		if p, ok := byFabric[f]; ok && p.VLAN != "" {
			return p.VLAN
		}
		return f
	}

	// Cabled ports, one per host and switch port (the twin PCIe function
	// with no address gives way).
	var cands []planCand
	seen := map[string]int{}
	for _, h := range hosts {
		if !h.Approved {
			continue
		}
		for _, p := range h.Ports {
			if p.Netplan != nil && p.Netplan.Mgmt {
				continue
			}
			sw := resolveSwitchName(switches, p.LLDPSwitch)
			if sw == "" || p.LLDPPort == "" {
				continue
			}
			if tab, ok := sws.known[strings.ToLower(sw)]; ok {
				if _, real := tab[strings.ToLower(p.LLDPPort)]; !real {
					continue // not an interface of that switch: a stale or misread LLDP report
				}
			}
			c := planCand{h: h, p: p, sw: sw, vln: vlans[strings.ToLower(sw+"|"+p.LLDPPort)]}
			key := fmt.Sprintf("%d|%s|%s", h.ID, strings.ToLower(sw), strings.ToLower(p.LLDPPort))
			if i, ok := seen[key]; ok {
				o := cands[i].p
				better := len(p.Addrs) > 0 && len(o.Addrs) == 0 ||
					len(p.Addrs) == 0 && len(o.Addrs) == 0 && (pciAddress(p) < pciAddress(o) || pciAddress(p) == pciAddress(o) && strings.ToLower(p.Netdev) < strings.ToLower(o.Netdev))
				if better {
					cands[i] = c
				}
				continue
			}
			seen[key] = len(cands)
			cands = append(cands, c)
		}
	}

	// Fabrics and their subnets.
	type fab struct {
		api.PlanFabric
		counts map[netip.Prefix]int
	}
	fabs := map[string]*fab{}
	getFab := func(name, vlan string) *fab {
		f := fabs[name]
		if f == nil {
			f = &fab{PlanFabric: api.PlanFabric{Fabric: name, VLAN: vlan, HostMTU: 9000}, counts: map[netip.Prefix]int{}}
			if p, ok := byFabric[name]; ok {
				f.Exists = true
				if p.VLAN != "" {
					f.VLAN = p.VLAN
				}
				if p.HostMTU > 0 {
					f.HostMTU = p.HostMTU
				}
				if pf, err := netip.ParsePrefix(p.Subnet); err == nil {
					f.Subnet, f.SubnetFrom = pf.Masked().String(), "profile"
				}
			}
			fabs[name] = f
		}
		return f
	}
	for _, p := range profiles {
		getFab(p.Fabric, p.VLAN)
	}
	fabricOf := func(c planCand) string {
		if c.vln != "" {
			return fabricOfVLAN(c.vln)
		}
		return c.p.Fabric
	}
	for _, c := range cands {
		name := fabricOf(c)
		if name == "" {
			continue
		}
		f := getFab(name, vlanOfFabric(name))
		f.Ports++
		if c.p.Fabric != "" && c.vln != "" && c.p.Fabric != name {
			continue // held back below; its addresses say nothing about this fabric
		}
		for _, a := range c.p.Addrs {
			if pf, err := netip.ParsePrefix(a); err == nil && pf.Addr().Is4() {
				f.counts[pf.Masked()]++
			}
		}
	}
	subnets := map[string]netip.Prefix{}
	for name, f := range fabs {
		if f.Subnet == "" {
			best, n := netip.Prefix{}, 0
			for pf, k := range f.counts {
				if k > n || k == n && pf.String() < best.String() {
					best, n = pf, k
				}
			}
			if n > 0 {
				f.Subnet, f.SubnetFrom = best.String(), "addresses"
			}
		}
		if pf, err := netip.ParsePrefix(f.Subnet); err == nil {
			subnets[name] = pf.Masked()
		}
		plan.Fabrics = append(plan.Fabrics, f.PlanFabric)
	}
	sort.Slice(plan.Fabrics, func(i, j int) bool { return plan.Fabrics[i].Fabric < plan.Fabrics[j].Fabric })

	// Every address in use, on any port of any host.
	used := map[netip.Addr]bool{}
	for _, h := range hosts {
		for _, p := range h.Ports {
			for _, a := range p.Addrs {
				if pf, err := netip.ParsePrefix(a); err == nil {
					used[pf.Addr()] = true
				}
			}
		}
	}
	// A host's number: the offset of its addresses less 10 per PCI function.
	hostNum := map[int64]int{}
	{
		votes := map[int64]map[int]int{}
		for _, h := range hosts {
			for _, p := range h.Ports {
				for _, a := range p.Addrs {
					pf, err := netip.ParsePrefix(a)
					if err != nil {
						continue
					}
					for _, sn := range subnets {
						if off := v4Offset(sn, pf.Addr()); off > 0 {
							if n := off - 10*pciFunction(p); n >= 1 {
								if votes[h.ID] == nil {
									votes[h.ID] = map[int]int{}
								}
								votes[h.ID][n]++
							}
						}
					}
				}
			}
		}
		for id, v := range votes {
			best, k := 0, 0
			for n, c := range v {
				if c > k || c == k && n < best {
					best, k = n, c
				}
			}
			hostNum[id] = best
		}
	}
	pick := func(c planCand, sn netip.Prefix) (netip.Addr, string) {
		fn := pciFunction(c.p)
		if n, ok := hostNum[c.h.ID]; ok {
			if a, ok := v4At(sn, n+10*fn); ok && !used[a] {
				return a, fmt.Sprintf("host number %d + 10 x PCI function %d", n, fn)
			}
		}
		start := 10
		if sn.Bits() > 27 {
			start = 1
		}
		for off := start; ; off++ {
			a, ok := v4At(sn, off)
			if !ok {
				return netip.Addr{}, ""
			}
			if !used[a] {
				return a, "next free address"
			}
		}
	}

	// Switch fixes, by switch port.
	fixes := map[string]*api.PlanSwitchFix{}
	addFix := func(c planCand, want, reason string) {
		key := strings.ToLower(c.sw + "|" + c.p.LLDPPort)
		f := fixes[key]
		if f == nil {
			f = &api.PlanSwitchFix{Switch: c.sw, Port: c.p.LLDPPort, Host: c.h.Name, Netdev: c.p.Netdev, VLAN: c.vln}
			fixes[key] = f
		}
		if want != "" {
			f.Want = want
		}
		if f.Reason == "" {
			f.Reason = reason
		} else if !strings.Contains(f.Reason, reason) {
			f.Reason += "; " + reason
		}
	}

	for _, c := range cands {
		pp := api.PlanPort{HostID: c.h.ID, Host: c.h.Name, Online: online(c.h.ID), Helper: c.h.Netplan != nil && c.h.Netplan.Helper != "",
			PortID: c.p.ID, Netdev: c.p.Netdev, Label: c.p.Label, Switch: c.sw, SwitchPort: c.p.LLDPPort, VLAN: c.vln,
			Current: c.p.Addrs, CurrentMTU: c.p.MTU}
		if pp.Current == nil {
			pp.Current = []string{}
		}
		name := fabricOf(c)
		pp.Fabric = name
		pp.SetFabric = c.p.Fabric == "" && name != ""
		prof, hasProf := byFabric[name]
		if hasProf && prof.SwitchMTU > 0 {
			if spc := qos[strings.ToLower(c.sw+"|"+c.p.LLDPPort)]; spc != nil && spc.MTU > 0 && spc.MTU < prof.SwitchMTU {
				addFix(c, "", fmt.Sprintf("switch port MTU %d is below the fabric's %d", spc.MTU, prof.SwitchMTU))
				f := fixes[strings.ToLower(c.sw+"|"+c.p.LLDPPort)]
				f.MTU, f.WantMTU = spc.MTU, prof.SwitchMTU
			}
		}
		switch {
		case name == "":
			pp.Action, pp.Note = "blocked", "the switch port is in no VLAN and the port has no fabric: put the switch port in the fabric's VLAN, or set the port's fabric"
		case c.vln == "":
			want := vlanOfFabric(c.p.Fabric)
			addFix(c, want, fmt.Sprintf("the port's fabric is %s but the switch port is in no VLAN", c.p.Fabric))
			pp.Action, pp.Note = "blocked", fmt.Sprintf("the switch port is in no VLAN: paste the switch lines below to put it in VLAN %s first", want)
		case c.p.Fabric != "" && c.p.Fabric != name:
			addFix(c, vlanOfFabric(c.p.Fabric), fmt.Sprintf("the port's fabric is %s but the switch port is in VLAN %s", c.p.Fabric, c.vln))
			pp.Action, pp.Note = "blocked", fmt.Sprintf("the port says fabric %s but its switch port is in VLAN %s: move the switch port (lines below) or change the port's fabric to %s", c.p.Fabric, c.vln, name)
		case c.p.State != "" && c.p.State != "ACTIVE":
			pp.Action, pp.Note = "blocked", "the link is down ("+c.p.State+")"
		}
		if pp.Action == "blocked" {
			plan.Ports = append(plan.Ports, pp)
			continue
		}
		sn, ok := subnets[name]
		if !ok {
			pp.Action, pp.Note = "blocked", fmt.Sprintf("fabric %s has no subnet yet: no port in it has an address; set the subnet on the fabric", name)
			plan.Ports = append(plan.Ports, pp)
			continue
		}
		mtu := 9000
		for _, f := range plan.Fabrics {
			if f.Fabric == name {
				mtu = f.HostMTU
			}
		}
		var fits, outside []string
		for _, a := range c.p.Addrs {
			if pf, err := netip.ParsePrefix(a); err == nil && sn.Contains(pf.Addr()) && pf.Bits() == sn.Bits() {
				fits = append(fits, a)
			} else if err == nil && pf.Addr().Is4() {
				outside = append(outside, a)
			}
		}
		switch {
		case len(fits) > 0:
			pp.Address, pp.Action = fits[0], "ok"
			if c.p.MTU > 0 && c.p.MTU < mtu {
				pp.Action, pp.MTU, pp.Apply = "mtu", mtu, true
				pp.Note = fmt.Sprintf("MTU %d is below the fabric's %d", c.p.MTU, mtu)
			}
		default:
			a, rule := pick(c, sn)
			if !a.IsValid() {
				pp.Action, pp.Note = "blocked", fmt.Sprintf("no free address left in %s", sn)
				break
			}
			used[a] = true
			pp.Address, pp.MTU, pp.Rule = netip.PrefixFrom(a, sn.Bits()).String(), mtu, rule
			if len(outside) == 0 {
				pp.Action, pp.Apply = "assign", true
			} else {
				pp.Action = "renumber"
				pp.Note = fmt.Sprintf("has %s, outside fabric %s's subnet %s; tick it to replace that address", strings.Join(outside, ", "), name, sn)
			}
		}
		if pp.Action == "ok" {
			pp.Apply = pp.SetFabric // only the fabric is stored; the host is not touched
		} else if pp.Apply && (!pp.Online || !pp.Helper) {
			pp.Apply = false
			if !pp.Online {
				pp.Note = strings.TrimPrefix(pp.Note+"; host is offline", "; ")
			} else {
				pp.Note = strings.TrimPrefix(pp.Note+"; re-run the install command on this host to add the netplan helper, or use Copy netplan", "; ")
			}
		}
		plan.Ports = append(plan.Ports, pp)
	}
	sort.SliceStable(plan.Ports, func(i, j int) bool {
		a, b := plan.Ports[i], plan.Ports[j]
		if a.Fabric != b.Fabric {
			return a.Fabric < b.Fabric
		}
		if a.Host != b.Host {
			return strings.ToLower(a.Host) < strings.ToLower(b.Host)
		}
		return a.Netdev < b.Netdev
	})

	// Switch lines, one block per switch.
	known := map[string]map[string]bool{} // switch -> VLANs it has members in
	for k, v := range vlans {
		sw := k
		if i := strings.IndexByte(k, '|'); i >= 0 {
			sw = k[:i]
		}
		if known[sw] == nil {
			known[sw] = map[string]bool{}
		}
		known[sw][v] = true
	}
	bySwitch := map[string][]*api.PlanSwitchFix{}
	var unchecked []string
	for _, f := range fixes {
		ifc, ok := sws.known[strings.ToLower(f.Switch)][strings.ToLower(f.Port)]
		if !ok {
			// Fail closed: a name the switch does not list never reaches a
			// line someone pastes into its CLI.
			unchecked = append(unchecked, f.Switch+" "+f.Port)
			plan.Switch = append(plan.Switch, *f)
			continue
		}
		var b strings.Builder
		fmt.Fprintf(&b, "interface %s\n", cliName(sws.naming[strings.ToLower(f.Switch)], ifc))
		if f.Want != "" {
			fmt.Fprintf(&b, " switchport access Vlan %s\n", f.Want)
		}
		if f.WantMTU > 0 {
			fmt.Fprintf(&b, " mtu %d\n", f.WantMTU)
		}
		b.WriteString(" exit\n")
		f.Commands = b.String()
		plan.Switch = append(plan.Switch, *f)
		bySwitch[f.Switch] = append(bySwitch[f.Switch], f)
	}
	if len(unchecked) > 0 {
		sort.Strings(unchecked)
		plan.Warnings = append(plan.Warnings, "No switch lines for "+strings.Join(unchecked, ", ")+": the switch's port table does not list that interface (or has not been read yet), so the name cannot be trusted.")
	}
	sort.Slice(plan.Switch, func(i, j int) bool {
		if plan.Switch[i].Switch != plan.Switch[j].Switch {
			return plan.Switch[i].Switch < plan.Switch[j].Switch
		}
		return portLess(plan.Switch[i].Port, plan.Switch[j].Port)
	})
	for _, sw := range switches {
		fs := bySwitch[sw.Name]
		if len(fs) == 0 {
			continue
		}
		sort.Slice(fs, func(i, j int) bool { return portLess(fs[i].Port, fs[j].Port) })
		var b strings.Builder
		b.WriteString("configure terminal\n")
		made := map[string]bool{}
		for _, f := range fs {
			if f.Want != "" && !known[strings.ToLower(sw.Name)][f.Want] && !made[f.Want] {
				fmt.Fprintf(&b, "interface Vlan %s\n exit\n", f.Want)
				made[f.Want] = true
			}
		}
		for _, f := range fs {
			b.WriteString(f.Commands)
		}
		b.WriteString("end\nwrite memory\n")
		plan.Commands = append(plan.Commands, api.PlanCommands{Switch: sw.Name, Address: sw.Address, Lines: b.String()})
	}
	return plan
}

// planSwitches is what the switch poll knows, as the network plan uses it.
type planSwitches struct {
	vlans  map[string]string                 // switch|port -> VLAN
	qos    map[string]*switchPortCfg         // switch|port -> port settings
	known  map[string]map[string]switchIface // switch -> ifname -> interface, see knownIfaces
	naming map[string]string                 // switch -> NATIVE | STANDARD
}

// applyNetworkPlan creates the ticked fabrics, gives ports their fabric,
// and queues one network change per host, run one host at a time.
func (s *Server) applyNetworkPlan(w http.ResponseWriter, r *http.Request) {
	var req api.PlanApply
	if !readJSON(w, r, &req) {
		return
	}
	plan, hosts, err := s.networkPlan(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	changes, err := planChanges(plan, hosts, req)
	if err != nil {
		netplanHTTPError(w, err)
		return
	}
	for _, ch := range changes {
		if len(ch.change.Ports) == 0 {
			continue
		}
		if err := s.netplanReady(ch.host); err != nil {
			httpError(w, http.StatusConflict, err.Error())
			return
		}
	}
	out := api.PlanApplied{Batch: time.Now().UnixMilli(), Fabrics: []string{}, Jobs: []api.NetplanJob{}}
	ctx := r.Context()
	for _, name := range req.Fabrics {
		for _, f := range plan.Fabrics {
			if f.Fabric != name || f.Exists {
				continue
			}
			if f.Subnet == "" {
				httpError(w, http.StatusBadRequest, fmt.Sprintf("fabric %s has no subnet yet", name))
				return
			}
			p := fabricPreset("")
			p.Fabric, p.VLAN, p.Subnet, p.HostMTU = f.Fabric, f.VLAN, f.Subnet, f.HostMTU
			if err := s.store.SaveFabricProfile(ctx, p); err != nil {
				serverError(w, err)
				return
			}
			out.Fabrics = append(out.Fabrics, name)
		}
	}
	for _, ch := range changes {
		for _, id := range ch.setFabric {
			f := ch.fabric[id]
			if err := s.store.UpdatePort(ctx, id, api.PortUpdate{Fabric: &f}); err == nil {
				out.Labels++
			}
		}
	}
	var jobs []*api.NetplanJob
	var jobChanges []api.NetplanChange
	for _, ch := range changes {
		if len(ch.change.Ports) == 0 {
			continue
		}
		j := &api.NetplanJob{HostID: ch.host.ID, Host: ch.host.Name, Ports: ch.change.Ports, Status: "queued", By: who(r).Name, Batch: out.Batch, Created: time.Now()}
		s.netplan.add(j)
		jobs = append(jobs, j)
		jobChanges = append(jobChanges, ch.change)
		out.Jobs = append(out.Jobs, *j)
	}
	s.store.Audit(ctx, "network.plan.apply", map[string]any{"batch": out.Batch, "by": who(r).Name, "fabrics": out.Fabrics, "labels": out.Labels, "ports": req.Ports})
	go func() {
		for i, j := range jobs {
			if err := s.orch.claimNetplan(j.HostID); err != nil {
				now := time.Now()
				s.netplan.update(j, func(j *api.NetplanJob) { j.Status, j.Message, j.Done = "refused", err.Error(), &now })
				continue
			}
			s.runNetplanJob(context.Background(), j, jobChanges[i])
			s.orch.releaseNetplan(j.HostID)
		}
		go s.pollFabricNow()
	}()
	if len(out.Fabrics) > 0 {
		go s.pollFabricNow()
	}
	writeJSON(w, out)
}

// hostChange is the part of an applied plan that falls on one host.
type hostChange struct {
	host      hostRec
	change    api.NetplanChange
	setFabric []int64
	fabric    map[int64]string
}

// planChanges checks an apply against a freshly built plan and groups the
// ports by host. An edited address must still be free and in the fabric.
func planChanges(plan api.NetworkPlan, hosts []hostRec, req api.PlanApply) ([]hostChange, error) {
	rows := map[int64]api.PlanPort{}
	for _, p := range plan.Ports {
		rows[p.PortID] = p
	}
	subnets := map[string]netip.Prefix{}
	for _, f := range plan.Fabrics {
		if pf, err := netip.ParsePrefix(f.Subnet); err == nil {
			subnets[f.Fabric] = pf
		}
	}
	byHost := map[int64]*hostChange{}
	var order []int64
	taken := map[netip.Addr]int64{}
	for _, ap := range req.Ports {
		row, ok := rows[ap.PortID]
		if !ok {
			return nil, &userError{fmt.Errorf("port %d is not in the plan any more; reload the plan", ap.PortID)}
		}
		if row.Action == "blocked" {
			return nil, &userError{fmt.Errorf("%s %s cannot be changed: %s", row.Host, row.Netdev, row.Note)}
		}
		sn, ok := subnets[row.Fabric]
		if !ok {
			return nil, &userError{fmt.Errorf("fabric %s has no subnet", row.Fabric)}
		}
		addr := strings.TrimSpace(ap.Address)
		if addr == "" {
			addr = row.Address
		}
		if !strings.Contains(addr, "/") {
			addr += "/" + strconv.Itoa(sn.Bits())
		}
		pf, err := netip.ParsePrefix(addr)
		if err != nil || !sn.Contains(pf.Addr()) || pf.Bits() != sn.Bits() {
			return nil, &userError{fmt.Errorf("%s %s: %s is not an address in fabric %s's subnet %s", row.Host, row.Netdev, ap.Address, row.Fabric, sn)}
		}
		if other, dup := taken[pf.Addr()]; dup && other != ap.PortID {
			return nil, &userError{fmt.Errorf("%s is given to two ports", pf.Addr())}
		}
		taken[pf.Addr()] = ap.PortID
		if owner := addressOwner(hosts, pf.Addr(), map[int64]bool{row.PortID: true}); owner != "" {
			return nil, &userError{fmt.Errorf("%s is already on %s", pf.Addr(), owner)}
		}
		hc := byHost[row.HostID]
		if hc == nil {
			for _, h := range hosts {
				if h.ID == row.HostID {
					hc = &hostChange{host: h, change: api.NetplanChange{TimeoutSec: 90}, fabric: map[int64]string{}}
				}
			}
			if hc == nil {
				return nil, &userError{fmt.Errorf("host %s is gone", row.Host)}
			}
			byHost[row.HostID] = hc
			order = append(order, row.HostID)
		}
		if row.SetFabric {
			hc.setFabric = append(hc.setFabric, row.PortID)
			hc.fabric[row.PortID] = row.Fabric
		}
		mtu := ap.MTU
		if mtu == 0 {
			mtu = row.MTU
		}
		if mtu == 0 {
			mtu = row.CurrentMTU
		}
		// The port keeps any other address it has in the fabric (an alias);
		// a renumbered address outside the fabric is dropped.
		addrs := []string{pf.String()}
		for _, a := range row.Current {
			if c, err := netip.ParsePrefix(a); err == nil && sn.Contains(c.Addr()) && c.Addr() != pf.Addr() {
				addrs = append(addrs, a)
			}
		}
		if row.Action == "ok" && row.Address == pf.String() && mtu == row.CurrentMTU {
			continue // nothing to change on the host; the fabric label is set above
		}
		np := api.NetplanPort{Netdev: row.Netdev, Addresses: addrs, MTU: mtu}
		np.Peers = appendPeers(nil, hosts, row.HostID, pf)
		hc.change.Ports = append(hc.change.Ports, np)
	}
	out := make([]hostChange, 0, len(order))
	for _, id := range order {
		hc := byHost[id]
		if len(hc.change.Ports) > 0 {
			if err := hc.change.Validate(); err != nil {
				return nil, &userError{fmt.Errorf("%s: %v", hc.host.Name, err)}
			}
		}
		out = append(out, *hc)
	}
	return out, nil
}
