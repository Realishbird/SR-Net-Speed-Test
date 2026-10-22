// Package api holds the JSON types shared by the agent, the controller and the CLI.
package api

import (
	"strings"
	"time"
)

// Supported benchmark tools.
const (
	ToolWriteBW  = "ib_write_bw"
	ToolReadBW   = "ib_read_bw"
	ToolSendBW   = "ib_send_bw"
	ToolWriteLat = "ib_write_lat"
	ToolReadLat  = "ib_read_lat"
	ToolSendLat  = "ib_send_lat"
	ToolIperf3   = "iperf3"
	ToolNCCL     = "srnet-nccl" // the agent's bundled NCCL collective benchmark (PyTorch)
)

// Test names in a RunSpec and the tool each one runs.
var Tests = map[string]string{
	"write": ToolWriteBW, "read": ToolReadBW, "send": ToolSendBW,
	"write_lat": ToolWriteLat, "read_lat": ToolReadLat, "send_lat": ToolSendLat,
	"tcp": ToolIperf3, "udp": ToolIperf3,
	"nccl_allreduce": ToolNCCL, "nccl_allgather": ToolNCCL,
}

// IsCollective reports whether a test is a GPU collective over every host at
// once (NCCL) rather than a set of point-to-point legs.
func IsCollective(test string) bool { return strings.HasPrefix(test, "nccl_") }

// IsLatency reports whether a test measures latency rather than bandwidth.
func IsLatency(test string) bool { return strings.HasSuffix(test, "_lat") }

// IsIP reports whether a test runs over the IP stack (iperf3) rather than RDMA verbs.
func IsIP(test string) bool { return test == "tcp" || test == "udp" }

// Task kinds sent from the controller to an agent.
const (
	TaskServer  = "server"
	TaskClient  = "client"
	TaskCancel  = "cancel"
	TaskUpdate  = "update"  // replace the agent binary with Version from URLPath
	TaskNetplan = "netplan" // change the network setup of RDMA ports through the root helper
)

// Event types sent from an agent to the controller.
const (
	EventServerReady = "server_ready"
	EventStarted     = "started"
	EventFailed      = "failed"
)

type EnrollRequest struct {
	Token     string    `json:"token"`
	Inventory Inventory `json:"inventory"`
}

type EnrollResponse struct {
	AgentID string `json:"agent_id"`
	Secret  string `json:"secret"`
}

// Port is one RDMA port and the netdev behind it.
type Port struct {
	RdmaDev     string        `json:"rdma_dev"`
	PortNum     int           `json:"port_num"`
	Netdev      string        `json:"netdev"`
	Addrs       []string      `json:"addrs"` // CIDR form, e.g. 172.27.100.10/24
	LinkLayer   string        `json:"link_layer"`
	State       string        `json:"state"`
	PhysState   string        `json:"phys_state"`
	SpeedMbps   int           `json:"speed_mbps"`
	MTU         int           `json:"mtu"`
	Driver      string        `json:"driver"`
	FWVersion   string        `json:"fw_version"`
	Transceiver string        `json:"transceiver"`
	NUMANode    int           `json:"numa_node"`
	LLDPSwitch  string        `json:"lldp_switch,omitempty"` // neighbour chassis name from lldpd
	LLDPPort    string        `json:"lldp_port,omitempty"`   // neighbour port id (its name, else its alias, else its description)
	LLDPDescr   string        `json:"lldp_descr,omitempty"`  // neighbour port description, when the port id was a name or alias
	Settings    *PortSettings `json:"settings,omitempty"`    // host-side RoCE settings, for the fabric check
	PCIe        *PCIeLink     `json:"pcie,omitempty"`        // the NIC function's PCIe link
	Netplan     *PortNetplan  `json:"netplan,omitempty"`     // how netplan configures the port, from the root helper
}

// PCIeLink is how a NIC function is attached: the link it trained to, what
// the card supports and what the slot offers. Gen 1..6, width in lanes.
type PCIeLink struct {
	Address    string  `json:"address"`              // PCI address, e.g. 0000:01:00.0
	Gen        int     `json:"gen"`                  // current generation
	Width      int     `json:"width"`                // current lanes
	MaxGen     int     `json:"max_gen"`              // the card's maximum
	MaxWidth   int     `json:"max_width"`            //
	SlotGen    int     `json:"slot_gen,omitempty"`   // the upstream port's maximum (the slot)
	SlotWidth  int     `json:"slot_width,omitempty"` //
	UsableGbps float64 `json:"usable_gbps"`          // rough data throughput the current link allows
}

// PCIeUsableGbps estimates the payload bandwidth of a link: the raw rate per
// lane (128b/130b encoding from Gen3 on, 8b/10b before) times the lanes,
// less about 15% for TLP headers, flow control and completions, which is
// what a NIC sees in practice (Gen5 x16 about 430 Gbps, Gen5 x4 about 108,
// Gen4 x16 about 215, Gen4 x8 about 108).
func PCIeUsableGbps(gen, width int) float64 {
	perLane := map[int]float64{1: 2.5 * 0.8, 2: 5 * 0.8, 3: 8 * 128 / 130, 4: 16 * 128 / 130, 5: 32 * 128 / 130, 6: 64 * 242 / 256}[gen]
	if perLane == 0 || width <= 0 {
		return 0
	}
	return perLane * float64(width) * 0.85
}

// Downtrained says whether the link runs below the card's maximum.
func (l *PCIeLink) Downtrained() bool {
	if l == nil {
		return false
	}
	return (l.MaxGen > 0 && l.Gen < l.MaxGen) || (l.MaxWidth > 0 && l.Width < l.MaxWidth)
}

// String renders "Gen5 x4".
func (l *PCIeLink) String() string {
	if l == nil || l.Gen == 0 {
		return ""
	}
	return "Gen" + strconvItoa(l.Gen) + " x" + strconvItoa(l.Width)
}

func strconvItoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

// PortSettings is how the host has the port set up for RoCE: what the
// fabric check compares with the fabric profile and the switch port.
// Empty strings mean the agent could not read that setting.
type PortSettings struct {
	RoCEMode string `json:"roce_mode,omitempty"` // "v2" when a RoCE v2 GID exists, "v1" when only v1
	Trust    string `json:"trust,omitempty"`     // dscp or pcp (what the NIC maps traffic to priorities by)
	PFC      string `json:"pfc,omitempty"`       // priorities with PFC on, e.g. "3" or "3,4"; "none" when read and all off
	DSCPPrio string `json:"dscp_prio,omitempty"` // DSCP to priority map in use, e.g. "26:3,48:6"
	ECNNp    string `json:"ecn_np,omitempty"`    // priorities with ECN notification point (CNP sender) on
	ECNRp    string `json:"ecn_rp,omitempty"`    // priorities with ECN reaction point (rate limiter) on
	CNPDSCP  int    `json:"cnp_dscp,omitempty"`  // DSCP the NIC marks CNPs with (-1 unknown)
	CNPPrio  int    `json:"cnp_prio,omitempty"`  // 802.1p priority of CNPs (-1 unknown)
	TOS      int    `json:"tos,omitempty"`       // default RoCE traffic class (TOS) of the RDMA device, -1 unknown
	Source   string `json:"source,omitempty"`    // which tools answered: dcb, mlnx_qos, sysfs
}

type Inventory struct {
	Hostname     string            `json:"hostname"`
	Arch         string            `json:"arch"`
	OS           string            `json:"os"`
	Kernel       string            `json:"kernel"`
	AgentVersion string            `json:"agent_version"`
	CPUModel     string            `json:"cpu_model"`
	CPUs         int               `json:"cpus"`
	MemBytes     uint64            `json:"mem_bytes"`
	Ports        []Port            `json:"ports"`
	Tools        map[string]string `json:"tools"`
	Netplan      *NetplanState     `json:"netplan,omitempty"`
}

// Task is one unit of work for an agent: one side of one test leg, or a cancel.
type Task struct {
	ID          string    `json:"id"`
	Kind        string    `json:"kind"`
	RunID       int64     `json:"run_id"`
	LegID       int64     `json:"leg_id"`
	Tool        string    `json:"tool,omitempty"`
	RdmaDev     string    `json:"rdma_dev,omitempty"`
	PortNum     int       `json:"port_num,omitempty"`
	Netdev      string    `json:"netdev,omitempty"`
	LocalIP     string    `json:"local_ip,omitempty"`
	PeerIP      string    `json:"peer_ip,omitempty"`
	TCPPort     int       `json:"tcp_port,omitempty"`
	DurationSec int       `json:"duration_sec,omitempty"`
	MsgSize     int       `json:"msg_size,omitempty"`
	QPs         int       `json:"qps,omitempty"`
	TOS         int       `json:"tos,omitempty"`
	MTU         int       `json:"mtu,omitempty"`
	Bidir       bool      `json:"bidir,omitempty"`    // perftest -b
	Streams     int       `json:"streams,omitempty"`  // iperf3 -P
	Protocol    string    `json:"protocol,omitempty"` // iperf3: tcp or udp
	StartAt     time.Time `json:"start_at,omitempty"`

	// Collective (NCCL) tasks: every host runs one rank; rank 0 is the
	// server task and hosts the rendezvous on TCPPort.
	Rank       int    `json:"rank,omitempty"`
	WorldSize  int    `json:"world_size,omitempty"`
	MasterIP   string `json:"master_ip,omitempty"`
	Collective string `json:"collective,omitempty"` // allreduce | allgather
	Sizes      []int  `json:"sizes,omitempty"`      // bytes per step, each measured in turn

	// Update tasks.
	Version string `json:"version,omitempty"`
	URLPath string `json:"url_path,omitempty"` // controller path serving the binary
	SHA256  string `json:"sha256,omitempty"`

	// Netplan tasks.
	Netplan *NetplanChange `json:"netplan,omitempty"`
}

// Update events sent by agents.
const (
	EventUpdateApplied = "update_applied" // binary staged, agent restarting
	EventUpdateFailed  = "update_failed"
	EventNetplanResult = "netplan_result" // Event.Netplan holds the outcome
)

// NetplanPort is the wanted network setup of one RDMA port.
type NetplanPort struct {
	Netdev    string   `json:"netdev"`
	Addresses []string `json:"addresses"`       // CIDR form, e.g. 172.27.101.23/24
	MTU       int      `json:"mtu,omitempty"`   // 0 leaves the MTU as netplan defaults it
	DHCP4     bool     `json:"dhcp4,omitempty"` //
	Peers     []string `json:"peers,omitempty"` // fabric peers one of which must answer a ping from the port; the controller fills it
}

// NetplanChange is one requested change to some of a host's RDMA ports.
type NetplanChange struct {
	ID         int64         `json:"id"`
	Ports      []NetplanPort `json:"ports"`
	TimeoutSec int           `json:"timeout_sec,omitempty"` // how long the helper waits for the ports to come up before rolling back
}

// NetplanResult is the outcome of a change: applied, rolled_back (the
// change was made, failed its checks and was undone), refused (nothing was
// touched) or failed.
type NetplanResult struct {
	ID      int64     `json:"id"`
	Status  string    `json:"status"`
	Message string    `json:"message,omitempty"`
	Steps   []string  `json:"steps,omitempty"` // what the helper did, in order
	At      time.Time `json:"at"`
}

// NetplanState is what a host reports about its netplan helper.
type NetplanState struct {
	Helper string         `json:"helper"`          // helper version; "" when install.sh has not added it
	At     time.Time      `json:"at"`              // when the helper last looked
	File   string         `json:"file,omitempty"`  // the controller-managed netplan file
	Error  string         `json:"error,omitempty"` // why the helper could not read the setup
	Last   *NetplanResult `json:"last,omitempty"`  // the last change it processed
}

// PortNetplan is how netplan configures one port today.
type PortNetplan struct {
	Managed bool     `json:"managed"`         // defined in the controller-managed file
	Files   []string `json:"files,omitempty"` // every netplan file that defines the port
	YAML    string   `json:"yaml,omitempty"`  // the port's stanzas as found (secrets left out)
	Mgmt    bool     `json:"mgmt,omitempty"`  // carries the default route or the controller connection: never editable
}

// Latency is the perftest latency summary in microseconds.
type Latency struct {
	MinUs     float64 `json:"min_us"`
	MaxUs     float64 `json:"max_us"`
	TypicalUs float64 `json:"typical_us"`
	AvgUs     float64 `json:"avg_us"`
	StdevUs   float64 `json:"stdev_us"`
	P99Us     float64 `json:"p99_us"`
	P999Us    float64 `json:"p999_us"`
}

type Event struct {
	TaskID  string         `json:"task_id"`
	RunID   int64          `json:"run_id"`
	LegID   int64          `json:"leg_id"`
	Type    string         `json:"type"`
	Message string         `json:"message,omitempty"`
	Netplan *NetplanResult `json:"netplan,omitempty"`
}

// LegResult is what one side of a leg reports when its benchmark process ends.
type LegResult struct {
	TaskID      string             `json:"task_id"`
	RunID       int64              `json:"run_id"`
	LegID       int64              `json:"leg_id"`
	Kind        string             `json:"kind"`
	Command     []string           `json:"command"`
	ExitCode    int                `json:"exit_code"`
	AvgGbps     float64            `json:"avg_gbps"`
	PeakGbps    float64            `json:"peak_gbps"`
	MsgRateMpps float64            `json:"msg_rate_mpps"`
	Latency     *Latency           `json:"latency,omitempty"`
	Retransmits int64              `json:"retransmits,omitempty"` // iperf3 TCP
	LossPct     float64            `json:"loss_pct,omitempty"`    // iperf3 UDP
	JitterMs    float64            `json:"jitter_ms,omitempty"`   // iperf3 UDP
	Points      map[string]float64 `json:"points,omitempty"`      // collective: bus bandwidth (Gbps) per message size in bytes
	Output      string             `json:"output"`
	Error       string             `json:"error,omitempty"`
	ErrorDeltas map[string]int64   `json:"error_deltas,omitempty"`
	StartedAt   time.Time          `json:"started_at"`
	EndedAt     time.Time          `json:"ended_at"`
}

// Sample is one telemetry reading. RdmaDev is empty for host-level values.
// A value key may carry one label after a dot, e.g. "hwmon_temp_c.coretemp_core0".
type Sample struct {
	Time    time.Time          `json:"t"`
	RdmaDev string             `json:"rdma_dev,omitempty"`
	PortNum int                `json:"port_num,omitempty"`
	Netdev  string             `json:"netdev,omitempty"`
	RunIDs  []int64            `json:"run_ids,omitempty"`
	Values  map[string]float64 `json:"values"`
}

type SampleBatch struct {
	Samples []Sample `json:"samples"`
}

// Admin API types.

type Host struct {
	ID           int64         `json:"id"`
	UID          string        `json:"uid"`
	Name         string        `json:"name"`
	Hostname     string        `json:"hostname"`
	Tags         []string      `json:"tags"`
	Site         string        `json:"site"`
	Rack         string        `json:"rack"`
	Arch         string        `json:"arch"`
	OS           string        `json:"os"`
	AgentVersion string        `json:"agent_version"`
	LastSeen     time.Time     `json:"last_seen"`
	Online       bool          `json:"online"`
	Approved     bool          `json:"approved"` // false: enrolled from a subnet without a token, waiting on the Hosts page
	Ports        []HostPort    `json:"ports"`
	Netplan      *NetplanState `json:"netplan,omitempty"`
}

type HostPort struct {
	ID           int64        `json:"id"`
	RdmaDev      string       `json:"rdma_dev"`
	PortNum      int          `json:"port_num"`
	Netdev       string       `json:"netdev"`
	Addrs        []string     `json:"addrs"`
	State        string       `json:"state"`
	SpeedMbps    int          `json:"speed_mbps"`
	FWVersion    string       `json:"fw_version"`
	Transceiver  string       `json:"transceiver"`
	Label        string       `json:"label"`
	Fabric       string       `json:"fabric"`
	ExpectedGbps float64      `json:"expected_gbps"`
	LLDPSwitch   string       `json:"lldp_switch,omitempty"`
	LLDPPort     string       `json:"lldp_port,omitempty"`
	LLDPDescr    string       `json:"lldp_descr,omitempty"`
	TxGbps       float64      `json:"tx_gbps"`
	RxGbps       float64      `json:"rx_gbps"`
	MTU          int          `json:"mtu,omitempty"`
	Driver       string       `json:"driver,omitempty"`
	PCIe         *PCIeLink    `json:"pcie,omitempty"`
	Netplan      *PortNetplan `json:"netplan,omitempty"`
}

type HostUpdate struct {
	Name *string   `json:"name,omitempty"`
	Tags *[]string `json:"tags,omitempty"`
	Site *string   `json:"site,omitempty"`
	Rack *string   `json:"rack,omitempty"`
}

type PortUpdate struct {
	Label        *string  `json:"label,omitempty"`
	Fabric       *string  `json:"fabric,omitempty"`
	ExpectedGbps *float64 `json:"expected_gbps,omitempty"`
}

type TokenResponse struct {
	Token          string    `json:"token"`
	ExpiresAt      time.Time `json:"expires_at"`
	Hosts          int       `json:"hosts"` // enrollments the token is good for
	InstallCommand string    `json:"install_command"`
}

// RunSpec describes a test. Endpoints are selectors: a host name, "tag:<tag>",
// an RDMA IP, or a port label.
//
// Topology comes from which fields are set: Legs (explicit), Mesh (every
// ordered pair among Hosts), Pairs (clients[i] to servers[i]), or clients and
// servers with one side single (fan-out / fan-in). Serial runs legs one at a
// time (a link matrix); Sizes repeats every leg at each message size (a sweep).
type RunSpec struct {
	Name        string    `json:"name"`
	Test        string    `json:"test"` // write, read, send, write_lat, read_lat, send_lat, tcp, udp
	Clients     []string  `json:"clients,omitempty"`
	Servers     []string  `json:"servers,omitempty"`
	Hosts       []string  `json:"hosts,omitempty"`
	Mesh        bool      `json:"mesh,omitempty"`
	Pairs       bool      `json:"pairs,omitempty"`
	Legs        []LegSpec `json:"legs,omitempty"`
	Serial      bool      `json:"serial,omitempty"`
	Sizes       []int     `json:"sizes,omitempty"`
	DurationSec int       `json:"duration_sec"`
	MsgSize     int       `json:"msg_size"`
	QPs         int       `json:"qps"`
	TOS         int       `json:"tos"`
	MTU         int       `json:"mtu"`
	Bidir       bool      `json:"bidir,omitempty"`
	Streams     int       `json:"streams,omitempty"` // iperf3 parallel streams
	PassPct     float64   `json:"pass_pct"`
	MaxLatP99Us float64   `json:"max_lat_p99_us,omitempty"` // latency tests fail above this when set
	MaxLossPct  float64   `json:"max_loss_pct,omitempty"`   // udp tests fail above this (default 1)
	RegressPct  float64   `json:"regress_pct,omitempty"`    // warn when a leg is this much worse than its baseline (default 5)
}

type LegSpec struct {
	Client string `json:"client"`
	Server string `json:"server"`
}

type Run struct {
	ID        int64      `json:"id"`
	Name      string     `json:"name"`
	Spec      RunSpec    `json:"spec"`
	Status    string     `json:"status"`   // queued | running | done | failed | canceled
	Verdict   string     `json:"verdict"`  // pass | warn | fail | ""
	Baseline  bool       `json:"baseline"` // marked as the reference for runs of the same shape
	Error     string     `json:"error,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	Summary   RunSummary `json:"summary"`
	Legs      []Leg      `json:"legs,omitempty"`
}

type RunSummary struct {
	ToolGbps    float64            `json:"tool_gbps"`
	CounterGbps float64            `json:"counter_gbps"`
	ByFabric    map[string]float64 `json:"by_fabric,omitempty"`
	LiveGbps    float64            `json:"live_gbps,omitempty"`
	// Comparison with the newest baseline run of the same shape, when one exists.
	BaselineRunID int64    `json:"baseline_run_id,omitempty"`
	BaselineGbps  float64  `json:"baseline_gbps,omitempty"` // the baseline's aggregate over the legs matched here
	DeltaPct      float64  `json:"delta_pct,omitempty"`     // aggregate change against the baseline, in percent (+ is faster)
	Regressed     int      `json:"regressed,omitempty"`     // legs that fell behind their baseline by more than the allowance
	FabricIssues  []string `json:"fabric_issues,omitempty"` // fabric check findings on the run's cables when it started
	// Workload-shaped figures (Phase 6). Fairness is the slowest leg's rate
	// over the fastest among the legs of the last stage, 1 = perfectly fair;
	// Congestion sums the counters that show the fabric pushing back:
	// ECN-marked packets, CNPs sent and handled, switch queue drops and PFC
	// pauses on the legs' ports.
	Fairness   float64          `json:"fairness,omitempty"`
	Congestion map[string]int64 `json:"congestion,omitempty"`
}

type Leg struct {
	ID           int64              `json:"id"`
	Index        int                `json:"index"`
	Stage        int                `json:"stage"`              // sequential stage this leg ran in
	MsgSize      int                `json:"msg_size,omitempty"` // set per leg in a size sweep
	ClientHost   string             `json:"client_host"`
	ClientIP     string             `json:"client_ip"`
	ServerHost   string             `json:"server_host"`
	ServerIP     string             `json:"server_ip"`
	Fabric       string             `json:"fabric"`
	ExpectedGbps float64            `json:"expected_gbps"`
	SharedPort   string             `json:"shared_port,omitempty"` // "<host> <ip> x<n>": a port this leg shares with n-1 other legs of its stage, which caps its expected rate to a fair share
	ToolGbps     float64            `json:"tool_gbps"`
	CounterGbps  float64            `json:"counter_gbps"`
	LiveGbps     float64            `json:"live_gbps"`
	Latency      *Latency           `json:"latency,omitempty"`
	Points       map[string]float64 `json:"points,omitempty"` // collective: bus bandwidth per size; ToolGbps is the largest size's
	Retransmits  int64              `json:"retransmits,omitempty"`
	LossPct      float64            `json:"loss_pct,omitempty"`
	JitterMs     float64            `json:"jitter_ms,omitempty"`
	ErrorDeltas  map[string]int64   `json:"error_deltas,omitempty"`
	Switch       *LegSwitch         `json:"switch,omitempty"`   // switch ports and counters, when LLDP and switch telemetry exist
	CPU          *LegCPU            `json:"cpu,omitempty"`      // host CPU during the test, when the agents reported it
	Baseline     *LegBaseline       `json:"baseline,omitempty"` // the same leg in the baseline run, when one matched
	Verdict      string             `json:"verdict"`
	Note         string             `json:"note,omitempty"`
}

// Release is an agent build mirrored on the controller.
type Release struct {
	Version  string            `json:"version"`
	Arches   []string          `json:"arches"`
	SHA256   map[string]string `json:"sha256"` // arch -> checksum
	Mirrored time.Time         `json:"mirrored_at"`
	InUse    int               `json:"in_use,omitempty"` // agents running this version
}

type ReleaseList struct {
	Current  string    `json:"controller_version"`
	Latest   string    `json:"latest_on_github,omitempty"`
	Mirrored []Release `json:"mirrored"`
	Keep     int       `json:"keep"` // newest versions kept by the daily prune, besides those in use
}

// DeleteResult is the outcome of a run deletion.
type DeleteResult struct {
	Deleted      []int64      `json:"deleted"`
	Skipped      []DeleteSkip `json:"skipped"`
	SampleErrors int          `json:"sample_errors,omitempty"`
}

type DeleteSkip struct {
	ID     int64  `json:"id"`
	Reason string `json:"reason"`
}

// UpdateJob tracks one fleet update: hosts are updated one at a time.
type UpdateJob struct {
	ID        int64              `json:"id"`
	Version   string             `json:"version"`
	Status    string             `json:"status"` // running | done | failed
	Hosts     []UpdateHostStatus `json:"hosts"`
	CreatedAt time.Time          `json:"created_at"`
	EndedAt   *time.Time         `json:"ended_at,omitempty"`
}

type UpdateHostStatus struct {
	HostID int64  `json:"host_id"`
	Name   string `json:"name"`
	From   string `json:"from"`
	Status string `json:"status"` // pending | updating | done | failed | skipped
	Note   string `json:"note,omitempty"`
}

type UpdateRequest struct {
	Version  string   `json:"version"`
	Hosts    []string `json:"hosts,omitempty"`    // selectors; empty = every host
	Parallel int      `json:"parallel,omitempty"` // hosts updated at once; 0 = the Settings page value
}

// Suite is an ordered list of runs executed back to back.
type Suite struct {
	ID    int64     `json:"id"`
	Name  string    `json:"name"`
	Specs []RunSpec `json:"specs"`
}

type SuiteRun struct {
	ID        int64      `json:"id"`
	SuiteID   int64      `json:"suite_id"`
	SuiteName string     `json:"suite_name"`
	Trigger   string     `json:"trigger"` // manual | schedule:<name>
	Status    string     `json:"status"`  // running | done | failed | canceled
	Verdict   string     `json:"verdict"`
	RunIDs    []int64    `json:"run_ids"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
}

// Schedule runs a suite on a cron expression (5 fields, controller local time).
type Schedule struct {
	ID        int64      `json:"id"`
	Name      string     `json:"name"`
	SuiteID   int64      `json:"suite_id"`
	SuiteName string     `json:"suite_name,omitempty"`
	Cron      string     `json:"cron"`
	Enabled   bool       `json:"enabled"`
	LastRunAt *time.Time `json:"last_run_at,omitempty"`
	NextRunAt *time.Time `json:"next_run_at,omitempty"`
}

// Alert is a condition the controller noticed; it closes when the condition clears.
type Alert struct {
	ID         int64      `json:"id"`
	Kind       string     `json:"kind"` // agent_offline | link_down | errors_idle | transceiver_hot | run_failed | update_failed
	Host       string     `json:"host,omitempty"`
	Port       string     `json:"port,omitempty"`
	Message    string     `json:"message"`
	OpenedAt   time.Time  `json:"opened_at"`
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
}

// Switch is a SONiC switch the controller reads port counters from, as shown
// in the UI and CLI. Secrets are never returned; Has* says whether one is set.
type Switch struct {
	Name            string        `json:"name"`
	Address         string        `json:"address"`
	SNMPVersion     string        `json:"snmp_version,omitempty"`
	HasSNMP         bool          `json:"has_snmp"`
	GNMIPort        int           `json:"gnmi_port,omitempty"`
	GNMIUser        string        `json:"gnmi_user,omitempty"`
	HasGNMIPassword bool          `json:"has_gnmi_password"`
	GNMITLS         bool          `json:"gnmi_tls"`
	SNMP            *SwitchProbe  `json:"snmp,omitempty"`
	GNMI            *SwitchProbe  `json:"gnmi,omitempty"`
	Health          *SwitchHealth `json:"health,omitempty"`
}

// SwitchHealth is the switch's own state: CPU, memory, temperature
// sensors, fans and PSUs, polled over gNMI once a minute.
type SwitchHealth struct {
	At         time.Time    `json:"at"`
	CPUPct     float64      `json:"cpu_pct,omitempty"`
	MemPct     float64      `json:"mem_pct,omitempty"`
	UptimeSec  float64      `json:"uptime_sec,omitempty"`
	LastReboot string       `json:"last_reboot,omitempty"`
	Temps      []SwitchTemp `json:"temps"`
	Fans       []SwitchPart `json:"fans"`
	PSUs       []SwitchPart `json:"psus"`
}

type SwitchTemp struct {
	Name      string  `json:"name"`                // what the switch calls it, e.g. "CPU Temp"
	Component string  `json:"component,omitempty"` // the gNMI key it was read under, e.g. "TEMP 1"
	C         float64 `json:"c"`
	HighC     float64 `json:"high_c,omitempty"`
	CriticalC float64 `json:"critical_c,omitempty"`
}

type SwitchPart struct {
	Name      string  `json:"name"`
	Component string  `json:"component,omitempty"`
	OK        bool    `json:"ok"`
	Status    string  `json:"status,omitempty"`
	RPM       float64 `json:"rpm,omitempty"` // fans only
}

// SwitchProbe is the latest evidence that a collection path works.
type SwitchProbe struct {
	OK      bool       `json:"ok"`
	At      *time.Time `json:"at,omitempty"`
	Ports   int        `json:"ports,omitempty"`  // SNMP: interfaces seen on the last poll
	Series  int        `json:"series,omitempty"` // gNMI: metric series received in the last 2 minutes
	Message string     `json:"message,omitempty"`
}

// SwitchRequest adds or replaces a switch. An empty SNMPCommunity or
// GNMIPassword keeps the stored secret; SNMPEnabled/GNMIEnabled false drops
// that collection path entirely.
type SwitchRequest struct {
	Name          string `json:"name"`
	Address       string `json:"address"`
	SNMPEnabled   bool   `json:"snmp_enabled"`
	SNMPVersion   string `json:"snmp_version,omitempty"`
	SNMPCommunity string `json:"snmp_community,omitempty"`
	GNMIEnabled   bool   `json:"gnmi_enabled"`
	GNMIPort      int    `json:"gnmi_port,omitempty"`
	GNMIUser      string `json:"gnmi_user,omitempty"`
	GNMIPassword  string `json:"gnmi_password,omitempty"`
	GNMITLS       bool   `json:"gnmi_tls"`
}

// HostStats is the latest host-level telemetry an agent reported.
type HostStats struct {
	At      time.Time `json:"at"`
	CPUPct  float64   `json:"cpu_pct"`
	Load1   float64   `json:"load1"`
	MemPct  float64   `json:"mem_pct"`
	MemUsed float64   `json:"mem_used_bytes"`
}

// Topology is the live picture the Topology page draws: switches with the
// ports that matter, hosts with their ports and LLDP neighbours, and the
// legs of runs in progress.
type Topology struct {
	At       time.Time    `json:"at"`
	Warning  string       `json:"warning,omitempty"`
	OpticMax float64      `json:"optic_max_c"` // transceiver alert limit (controller.json transceiver_max_c)
	NicMax   float64      `json:"nic_max_c"`   // NIC chip alert limit (controller.json nic_max_c)
	Switches []TopoSwitch `json:"switches"`
	Links    []TopoLink   `json:"links"` // switch to switch
	Hosts    []TopoHost   `json:"hosts"`
	Runs     []TopoRun    `json:"runs"`
}

// TopoLink is a cable between two configured switches, from their LLDP tables.
type TopoLink struct {
	A     string `json:"a"`
	APort string `json:"a_port"`
	B     string `json:"b"`
	BPort string `json:"b_port"`
}

type TopoSwitch struct {
	Name    string           `json:"name"`
	Address string           `json:"address"`
	Source  string           `json:"source,omitempty"` // gnmi | snmp | "" (no data)
	Ports   []TopoSwitchPort `json:"ports"`
}

type TopoSwitchPort struct {
	Switch      string   `json:"switch"`
	Name        string   `json:"name"`
	Up          *bool    `json:"up,omitempty"`
	SpeedMbps   int      `json:"speed_mbps,omitempty"`
	RxGbps      float64  `json:"rx_gbps"`
	TxGbps      float64  `json:"tx_gbps"`
	InDiscards  float64  `json:"in_discards"`
	OutDiscards float64  `json:"out_discards"`
	InErrors    float64  `json:"in_errors"`
	OutErrors   float64  `json:"out_errors"`
	FCSErrors   float64  `json:"fcs_errors"`
	VLAN        string   `json:"vlan,omitempty"`  // the switch's own VLAN for this port ("100"), the fabric as the switch sees it
	Descr       string   `json:"descr,omitempty"` // the port's description on the switch
	Hosts       []string `json:"hosts"`           // host names whose LLDP points here
}

type TopoHost struct {
	ID       int64          `json:"id"`
	Name     string         `json:"name"`
	Hostname string         `json:"hostname"`
	Tags     []string       `json:"tags"`
	Online   bool           `json:"online"`
	Stats    *HostStats     `json:"stats,omitempty"`
	Ports    []TopoHostPort `json:"ports"`
}

type TopoHostPort struct {
	ID         int64   `json:"id"`
	RdmaDev    string  `json:"rdma_dev"`
	PortNum    int     `json:"port_num"`
	Netdev     string  `json:"netdev"`
	IP         string  `json:"ip,omitempty"`
	Label      string  `json:"label"`
	Fabric     string  `json:"fabric"`
	State      string  `json:"state"`
	SpeedMbps  int     `json:"speed_mbps"`
	TxGbps     float64 `json:"tx_gbps"`
	RxGbps     float64 `json:"rx_gbps"`
	LLDPSwitch string  `json:"lldp_switch,omitempty"`
	LLDPPort   string  `json:"lldp_port,omitempty"`
	Switch     string  `json:"switch,omitempty"`      // configured switch the LLDP neighbour resolved to
	SwitchPort string  `json:"switch_port,omitempty"` // its port
	TwinOf     string  `json:"twin_of,omitempty"`     // netdev of the port this one shares a cable with (a second PCIe function of the same physical port); drawn once, under that port
	OpticC     float64 `json:"optic_c,omitempty"`     // transceiver temperature from DOM, latest reading
	NicC       float64 `json:"nic_c,omitempty"`       // NIC chip temperature from the port's hwmon sensor, latest reading
}

type TopoRun struct {
	ID   int64     `json:"id"`
	Name string    `json:"name"`
	Legs []TopoLeg `json:"legs"`
}

type TopoLeg struct {
	ClientPortID int64   `json:"client_port_id"`
	ServerPortID int64   `json:"server_port_id"`
	ClientHost   string  `json:"client_host"`
	ServerHost   string  `json:"server_host"`
	LiveGbps     float64 `json:"live_gbps"`
}

// PasswordChange is the body of POST /api/v1/admin/password.
type PasswordChange struct {
	Current string `json:"current"`
	New     string `json:"new"`
}

// LegSwitch is what the switch saw on a leg's two ports during the test.
type LegSwitch struct {
	ClientPort string             `json:"client_port,omitempty"` // "<switch> <port>"
	ServerPort string             `json:"server_port,omitempty"`
	Gbps       float64            `json:"gbps"`             // average rate along the data flow, from the switch's octet counters
	Deltas     map[string]float64 `json:"deltas,omitempty"` // "<port> <counter>": growth during the test, only when > 0
	Note       string             `json:"note,omitempty"`
}

// LegBaseline compares a leg with the same leg (same client, server and
// message size) of the newest baseline run of the run's shape. Value is the
// baseline's figure: Gbps for bandwidth tests, p99 µs for latency tests.
type LegBaseline struct {
	RunID     int64   `json:"run_id"`
	Value     float64 `json:"value"`
	DeltaPct  float64 `json:"delta_pct"` // this leg against the baseline, in percent of the baseline (+ is faster, or lower latency)
	Regressed bool    `json:"regressed"` // worse than the baseline by more than the run's allowance
}

// BaselineRequest is the body of POST /api/v1/runs/{id}/baseline.
type BaselineRequest struct {
	Baseline bool `json:"baseline"`
}

// FabricProfile says how a fabric is meant to be set up (Phase 5). The
// fabric check compares every cable of the fabric (host port, switch port)
// with it. -1 in a number means "not checked"; an empty string the same.
type FabricProfile struct {
	Fabric      string     `json:"fabric"`
	Mode        string     `json:"mode"`         // lossless (PFC and ECN expected, drops are a fault) | lossy (PFC off expected, drops allowed) | "" (consistency only)
	SwitchMTU   int        `json:"switch_mtu"`   // switch port MTU at least this (9100)
	HostMTU     int        `json:"host_mtu"`     // host netdev MTU at least this (9000; RDMA MTU 4096 needs more than 4200)
	SpeedMbps   int        `json:"speed_mbps"`   // link speed on both ends (100000)
	VLAN        string     `json:"vlan"`         // switch VLAN the ports must be in ("100"); empty = the fabric name
	Subnet      string     `json:"subnet"`       // the fabric's IP subnet ("172.27.100.0/24"), for auto addressing; empty = learned from the ports
	PFCPriority int        `json:"pfc_priority"` // priority with PFC on, on the switch and the host (3); -1 = PFC off expected
	ECNQueue    int        `json:"ecn_queue"`    // switch queue with ECN marking (3); -1 = not checked
	RoCEDSCP    int        `json:"roce_dscp"`    // DSCP the switch maps to the RoCE queue (26); -1 = not checked
	CNPDSCP     int        `json:"cnp_dscp"`     // DSCP CNPs are sent with (48); -1 = not checked
	RoCEMode    string     `json:"roce_mode"`    // v2
	Trust       string     `json:"trust"`        // host NIC trust mode: dscp or pcp
	HostECN     bool       `json:"host_ecn"`     // host NIC congestion control (ECN np and rp) on the PFC priority
	UpdatedAt   *time.Time `json:"updated_at,omitempty"`
}

// FabricCheck is the fabric check page: every cable the controller knows,
// each setting on both ends against the fabric's profile.
type FabricCheck struct {
	At       time.Time       `json:"at"`
	Warning  string          `json:"warning,omitempty"`
	Profiles []FabricProfile `json:"profiles"`
	Cables   []CableCheck    `json:"cables"`
	Issues   int             `json:"issues"` // cables with at least one failed check
}

type CableCheck struct {
	Host       string             `json:"host"`
	Netdev     string             `json:"netdev"`
	Label      string             `json:"label,omitempty"`
	IP         string             `json:"ip,omitempty"`
	Fabric     string             `json:"fabric"`
	Switch     string             `json:"switch,omitempty"`
	SwitchPort string             `json:"switch_port,omitempty"`
	OK         bool               `json:"ok"`
	Checks     []SettingCheck     `json:"checks"`
	Counters   map[string]float64 `json:"counters,omitempty"` // switch queue drops, ECN marks, pause frames (lifetime)
}

// SettingCheck is one setting compared across the profile, the switch port
// and the host port. State is ok, bad, unknown (a side could not be read)
// or skip (the profile does not check it).
type SettingCheck struct {
	Name   string `json:"name"`
	Want   string `json:"want,omitempty"`
	Switch string `json:"switch,omitempty"`
	Host   string `json:"host,omitempty"`
	State  string `json:"state"`
	Note   string `json:"note,omitempty"`
}

// LegCPU is the CPU use of both hosts during the leg, in percent.
type LegCPU struct {
	ClientAvg  float64 `json:"client_avg"`
	ClientPeak float64 `json:"client_peak"`
	ServerAvg  float64 `json:"server_avg"`
	ServerPeak float64 `json:"server_peak"`
}

// Phase 7: access.

// APIToken is a long-lived credential for scripts and CI (Bearer auth).
// The token itself is shown once, when created; only its hash is kept.
type APIToken struct {
	ID        int64      `json:"id"`
	Name      string     `json:"name"`
	Role      string     `json:"role"` // admin | viewer
	Prefix    string     `json:"prefix"`
	CreatedAt time.Time  `json:"created_at"`
	LastUsed  *time.Time `json:"last_used,omitempty"`
	Token     string     `json:"token,omitempty"` // only in the create response
}

// Session is a signed-in SSO user.
type Session struct {
	ID        string    `json:"id"` // hash of the cookie, never the cookie
	Subject   string    `json:"subject"`
	Name      string    `json:"name"`
	Email     string    `json:"email"`
	Role      string    `json:"role"`
	IP        string    `json:"ip"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	LastSeen  time.Time `json:"last_seen"`
	Current   bool      `json:"current,omitempty"`
}

// Whoami says who the caller is, on the status response.
type Whoami struct {
	Name string `json:"name"`
	Role string `json:"role"` // admin | viewer
	Via  string `json:"via"`  // password | token | sso
}
