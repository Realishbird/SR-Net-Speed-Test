package api

import (
	"fmt"
	"net/netip"
	"regexp"
	"time"
)

// RoutableAddrs drops the link-local addresses (169.254.0.0/16) a host puts
// on a port of its own accord, e.g. NetworkManager or avahi on a port left
// without an address after a move. They never carry a test, and counting
// them would pair every such port across hosts on one made-up subnet.
func RoutableAddrs(addrs []string) []string {
	out := addrs[:0:0]
	for _, a := range addrs {
		if pf, err := netip.ParsePrefix(a); err == nil && pf.Addr().IsLinkLocalUnicast() {
			continue
		}
		out = append(out, a)
	}
	return out
}

// NetplanMaxPorts is the most ports one change may cover.
const NetplanMaxPorts = 16

var netdevRe = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,15}$`)

// Validate checks a change on its own: the controller before it sends one,
// the agent and the root helper again before they act on it.
func (ch NetplanChange) Validate() error {
	if len(ch.Ports) == 0 || len(ch.Ports) > NetplanMaxPorts {
		return fmt.Errorf("a change covers 1 to %d ports, not %d", NetplanMaxPorts, len(ch.Ports))
	}
	seenDev, seenIP := map[string]bool{}, map[netip.Addr]string{}
	for _, p := range ch.Ports {
		if !netdevRe.MatchString(p.Netdev) {
			return fmt.Errorf("%q is not an interface name", p.Netdev)
		}
		if seenDev[p.Netdev] {
			return fmt.Errorf("%s is listed twice", p.Netdev)
		}
		seenDev[p.Netdev] = true
		if p.MTU != 0 && (p.MTU < 576 || p.MTU > 9216) {
			return fmt.Errorf("%s: MTU %d is outside 576 to 9216", p.Netdev, p.MTU)
		}
		if len(p.Addresses) > 8 || len(p.Peers) > 8 {
			return fmt.Errorf("%s: too many addresses or peers", p.Netdev)
		}
		for _, a := range p.Addresses {
			pf, err := netip.ParsePrefix(a)
			if err != nil {
				return fmt.Errorf("%s: %q is not an address with a prefix length, like 172.27.101.23/24", p.Netdev, a)
			}
			ip := pf.Addr()
			if ip.IsLoopback() || ip.IsMulticast() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() {
				return fmt.Errorf("%s: %s cannot be a port address", p.Netdev, a)
			}
			if ip.Is4() && pf.Bits() < 31 && (ip == pf.Masked().Addr() || ip == Broadcast(pf)) {
				return fmt.Errorf("%s: %s is the network or broadcast address of its subnet", p.Netdev, a)
			}
			if other, ok := seenIP[ip]; ok {
				return fmt.Errorf("%s is given to both %s and %s", ip, other, p.Netdev)
			}
			seenIP[ip] = p.Netdev
		}
		for _, peer := range p.Peers {
			if _, err := netip.ParseAddr(peer); err != nil {
				return fmt.Errorf("%s: peer %q is not an IP address", p.Netdev, peer)
			}
		}
	}
	return nil
}

// Broadcast is the last address of an IPv4 subnet.
func Broadcast(p netip.Prefix) netip.Addr {
	b := p.Masked().Addr().As4()
	host := uint32(1)<<(32-p.Bits()) - 1
	v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3]) | host
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

// NetplanRequest asks for a change to some of a host's RDMA ports, from the
// Hosts page or the API. The controller adds the fabric peers each port
// must reach.
type NetplanRequest struct {
	Ports      []NetplanPort `json:"ports"`
	TimeoutSec int           `json:"timeout_sec,omitempty"`
}

// NetplanJob is one change the controller sent to a host, and how it ended.
type NetplanJob struct {
	ID      int64         `json:"id"`
	HostID  int64         `json:"host_id"`
	Host    string        `json:"host"`
	Ports   []NetplanPort `json:"ports"`
	Status  string        `json:"status"` // queued | running | applied | rolled_back | refused | failed
	Message string        `json:"message,omitempty"`
	Steps   []string      `json:"steps,omitempty"`
	By      string        `json:"by,omitempty"`
	Batch   int64         `json:"batch,omitempty"` // set when the change is part of an applied network plan
	Created time.Time     `json:"created"`
	Done    *time.Time    `json:"done,omitempty"`
}

// NetplanPreview is the netplan a change would write, for hosts without
// the helper or for an admin who would rather apply it by hand.
type NetplanPreview struct {
	File     string `json:"file"`     // /etc/netplan/90-srnet.yaml
	YAML     string `json:"yaml"`     // its content for the requested ports
	Commands string `json:"commands"` // shell lines to write it and try it with automatic rollback
	Note     string `json:"note,omitempty"`
}

// NetworkPlan proposes fabrics from the switches' VLANs and an address for
// every cabled RDMA port, keeping every address that already fits.
type NetworkPlan struct {
	At       time.Time       `json:"at"`
	Warnings []string        `json:"warnings"`
	Fabrics  []PlanFabric    `json:"fabrics"`
	Ports    []PlanPort      `json:"ports"`
	Switch   []PlanSwitchFix `json:"switch"`   // switch ports whose VLAN or MTU needs changing, for an admin to apply
	Commands []PlanCommands  `json:"commands"` // sonic-cli lines for those fixes, one block per switch
}

// PlanCommands are the lines to paste into one switch's sonic-cli.
type PlanCommands struct {
	Switch  string `json:"switch"`
	Address string `json:"address,omitempty"`
	Lines   string `json:"lines"`
}

// PlanFabric is a fabric the plan uses: from a switch VLAN, a profile, or both.
type PlanFabric struct {
	Fabric     string `json:"fabric"`
	VLAN       string `json:"vlan"`
	Subnet     string `json:"subnet,omitempty"`
	SubnetFrom string `json:"subnet_from,omitempty"` // profile | addresses ("" = unknown: set it on the fabric first)
	HostMTU    int    `json:"host_mtu"`
	Ports      int    `json:"ports"`  // cabled host ports in it
	Exists     bool   `json:"exists"` // a fabric profile exists; false = applying the plan creates it
}

// PlanPort is one cabled RDMA port in the plan.
type PlanPort struct {
	HostID     int64    `json:"host_id"`
	Host       string   `json:"host"`
	Online     bool     `json:"online"`
	Helper     bool     `json:"helper"` // the host has the netplan helper (install.sh re-run)
	PortID     int64    `json:"port_id"`
	Netdev     string   `json:"netdev"`
	Label      string   `json:"label,omitempty"`
	Switch     string   `json:"switch,omitempty"`
	SwitchPort string   `json:"switch_port,omitempty"`
	VLAN       string   `json:"vlan,omitempty"`
	Fabric     string   `json:"fabric"`
	SetFabric  bool     `json:"set_fabric,omitempty"` // the port has no fabric of its own yet; applying stores this one
	Current    []string `json:"current"`
	CurrentMTU int      `json:"current_mtu,omitempty"`
	Address    string   `json:"address,omitempty"` // the address the port should have (CIDR)
	MTU        int      `json:"mtu,omitempty"`
	Action     string   `json:"action"` // ok | assign | renumber | mtu | blocked
	Apply      bool     `json:"apply"`  // ticked by default
	Rule       string   `json:"rule,omitempty"`
	Note       string   `json:"note,omitempty"`
}

// PlanSwitchFix is a switch port the plan wants in another VLAN or at a
// larger MTU. The service only reads switches: an admin pastes Commands.
type PlanSwitchFix struct {
	Switch   string `json:"switch"`
	Port     string `json:"port"`
	Host     string `json:"host"`
	Netdev   string `json:"netdev"`
	VLAN     string `json:"vlan"` // now; "" = in no VLAN
	Want     string `json:"want"`
	MTU      int    `json:"mtu,omitempty"`
	WantMTU  int    `json:"want_mtu,omitempty"`
	Reason   string `json:"reason"`
	Commands string `json:"commands"`
}

// PlanApply is the part of a plan an admin applies: the ticked ports, with
// any address they edited.
type PlanApply struct {
	Fabrics []string        `json:"fabrics"` // new fabrics to create (from the plan's fabrics with exists false)
	Ports   []PlanApplyPort `json:"ports"`
}

type PlanApplyPort struct {
	PortID  int64  `json:"port_id"`
	Address string `json:"address"`
	MTU     int    `json:"mtu,omitempty"`
	Fabric  string `json:"fabric,omitempty"`
}

// PlanApplied answers a plan apply: the fabrics created and the changes queued.
type PlanApplied struct {
	Batch   int64        `json:"batch"`
	Fabrics []string     `json:"fabrics"`
	Labels  int          `json:"labels"` // ports given their fabric
	Jobs    []NetplanJob `json:"jobs"`
}
