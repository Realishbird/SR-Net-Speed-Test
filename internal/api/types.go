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
)

// Test names in a RunSpec and the tool each one runs.
var Tests = map[string]string{
	"write": ToolWriteBW, "read": ToolReadBW, "send": ToolSendBW,
	"write_lat": ToolWriteLat, "read_lat": ToolReadLat, "send_lat": ToolSendLat,
	"tcp": ToolIperf3, "udp": ToolIperf3,
}

// IsLatency reports whether a test measures latency rather than bandwidth.
func IsLatency(test string) bool { return strings.HasSuffix(test, "_lat") }

// IsIP reports whether a test runs over the IP stack (iperf3) rather than RDMA verbs.
func IsIP(test string) bool { return test == "tcp" || test == "udp" }

// Task kinds sent from the controller to an agent.
const (
	TaskServer = "server"
	TaskClient = "client"
	TaskCancel = "cancel"
	TaskUpdate = "update" // replace the agent binary with Version from URLPath
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
	RdmaDev     string   `json:"rdma_dev"`
	PortNum     int      `json:"port_num"`
	Netdev      string   `json:"netdev"`
	Addrs       []string `json:"addrs"` // CIDR form, e.g. 172.27.100.10/24
	LinkLayer   string   `json:"link_layer"`
	State       string   `json:"state"`
	PhysState   string   `json:"phys_state"`
	SpeedMbps   int      `json:"speed_mbps"`
	MTU         int      `json:"mtu"`
	Driver      string   `json:"driver"`
	FWVersion   string   `json:"fw_version"`
	Transceiver string   `json:"transceiver"`
	NUMANode    int      `json:"numa_node"`
	LLDPSwitch  string   `json:"lldp_switch,omitempty"` // neighbour chassis name from lldpd
	LLDPPort    string   `json:"lldp_port,omitempty"`   // neighbour port id
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

	// Update tasks.
	Version string `json:"version,omitempty"`
	URLPath string `json:"url_path,omitempty"` // controller path serving the binary
	SHA256  string `json:"sha256,omitempty"`
}

// Update events sent by agents.
const (
	EventUpdateApplied = "update_applied" // binary staged, agent restarting
	EventUpdateFailed  = "update_failed"
)

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
	TaskID  string `json:"task_id"`
	RunID   int64  `json:"run_id"`
	LegID   int64  `json:"leg_id"`
	Type    string `json:"type"`
	Message string `json:"message,omitempty"`
}

// LegResult is what one side of a leg reports when its benchmark process ends.
type LegResult struct {
	TaskID      string           `json:"task_id"`
	RunID       int64            `json:"run_id"`
	LegID       int64            `json:"leg_id"`
	Kind        string           `json:"kind"`
	Command     []string         `json:"command"`
	ExitCode    int              `json:"exit_code"`
	AvgGbps     float64          `json:"avg_gbps"`
	PeakGbps    float64          `json:"peak_gbps"`
	MsgRateMpps float64          `json:"msg_rate_mpps"`
	Latency     *Latency         `json:"latency,omitempty"`
	Retransmits int64            `json:"retransmits,omitempty"` // iperf3 TCP
	LossPct     float64          `json:"loss_pct,omitempty"`    // iperf3 UDP
	JitterMs    float64          `json:"jitter_ms,omitempty"`   // iperf3 UDP
	Output      string           `json:"output"`
	Error       string           `json:"error,omitempty"`
	ErrorDeltas map[string]int64 `json:"error_deltas,omitempty"`
	StartedAt   time.Time        `json:"started_at"`
	EndedAt     time.Time        `json:"ended_at"`
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
	ID           int64      `json:"id"`
	UID          string     `json:"uid"`
	Name         string     `json:"name"`
	Hostname     string     `json:"hostname"`
	Tags         []string   `json:"tags"`
	Site         string     `json:"site"`
	Rack         string     `json:"rack"`
	Arch         string     `json:"arch"`
	OS           string     `json:"os"`
	AgentVersion string     `json:"agent_version"`
	LastSeen     time.Time  `json:"last_seen"`
	Online       bool       `json:"online"`
	Ports        []HostPort `json:"ports"`
}

type HostPort struct {
	ID           int64    `json:"id"`
	RdmaDev      string   `json:"rdma_dev"`
	PortNum      int      `json:"port_num"`
	Netdev       string   `json:"netdev"`
	Addrs        []string `json:"addrs"`
	State        string   `json:"state"`
	SpeedMbps    int      `json:"speed_mbps"`
	FWVersion    string   `json:"fw_version"`
	Transceiver  string   `json:"transceiver"`
	Label        string   `json:"label"`
	Fabric       string   `json:"fabric"`
	ExpectedGbps float64  `json:"expected_gbps"`
	LLDPSwitch   string   `json:"lldp_switch,omitempty"`
	LLDPPort     string   `json:"lldp_port,omitempty"`
	TxGbps       float64  `json:"tx_gbps"`
	RxGbps       float64  `json:"rx_gbps"`
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
}

type LegSpec struct {
	Client string `json:"client"`
	Server string `json:"server"`
}

type Run struct {
	ID        int64      `json:"id"`
	Name      string     `json:"name"`
	Spec      RunSpec    `json:"spec"`
	Status    string     `json:"status"`  // queued | running | done | failed | canceled
	Verdict   string     `json:"verdict"` // pass | warn | fail | ""
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
}

type Leg struct {
	ID           int64            `json:"id"`
	Index        int              `json:"index"`
	Stage        int              `json:"stage"`              // sequential stage this leg ran in
	MsgSize      int              `json:"msg_size,omitempty"` // set per leg in a size sweep
	ClientHost   string           `json:"client_host"`
	ClientIP     string           `json:"client_ip"`
	ServerHost   string           `json:"server_host"`
	ServerIP     string           `json:"server_ip"`
	Fabric       string           `json:"fabric"`
	ExpectedGbps float64          `json:"expected_gbps"`
	ToolGbps     float64          `json:"tool_gbps"`
	CounterGbps  float64          `json:"counter_gbps"`
	LiveGbps     float64          `json:"live_gbps"`
	Latency      *Latency         `json:"latency,omitempty"`
	Retransmits  int64            `json:"retransmits,omitempty"`
	LossPct      float64          `json:"loss_pct,omitempty"`
	JitterMs     float64          `json:"jitter_ms,omitempty"`
	ErrorDeltas  map[string]int64 `json:"error_deltas,omitempty"`
	Verdict      string           `json:"verdict"`
	Note         string           `json:"note,omitempty"`
}

// Release is an agent build mirrored on the controller.
type Release struct {
	Version  string            `json:"version"`
	Arches   []string          `json:"arches"`
	SHA256   map[string]string `json:"sha256"` // arch -> checksum
	Mirrored time.Time         `json:"mirrored_at"`
}

type ReleaseList struct {
	Current  string    `json:"controller_version"`
	Latest   string    `json:"latest_on_github,omitempty"`
	Mirrored []Release `json:"mirrored"`
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
	Version string   `json:"version"`
	Hosts   []string `json:"hosts,omitempty"` // selectors; empty = every host
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
	Name            string       `json:"name"`
	Address         string       `json:"address"`
	SNMPVersion     string       `json:"snmp_version,omitempty"`
	HasSNMP         bool         `json:"has_snmp"`
	GNMIPort        int          `json:"gnmi_port,omitempty"`
	GNMIUser        string       `json:"gnmi_user,omitempty"`
	HasGNMIPassword bool         `json:"has_gnmi_password"`
	GNMITLS         bool         `json:"gnmi_tls"`
	SNMP            *SwitchProbe `json:"snmp,omitempty"`
	GNMI            *SwitchProbe `json:"gnmi,omitempty"`
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
