package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Realishbird/SR-Net-Speed-Test/internal/api"
)

// errorCounters are the hw_counters whose growth during a test points at a fabric problem.
var errorCounters = []string{
	"out_of_sequence", "packet_seq_err", "local_ack_timeout_err", "rnr_nak_retry_err",
	"implied_nak_seq_err", "duplicate_request", "rx_icrc_encapsulated", "req_cqe_error",
	"resp_cqe_error", "np_cnp_sent", "rp_cnp_handled", "np_ecn_marked_roce_packets",
}

// BuildCommand turns a task into argv. Every field is validated and formatted here;
// nothing from the controller reaches a shell.
func BuildCommand(t api.Task) ([]string, error) {
	if t.TCPPort < 1024 || t.TCPPort > 65535 {
		return nil, fmt.Errorf("invalid tcp port %d", t.TCPPort)
	}
	if t.DurationSec < 1 || t.DurationSec > 24*3600 {
		return nil, fmt.Errorf("invalid duration %d", t.DurationSec)
	}
	if t.MsgSize < 1 || t.MsgSize > 1<<30 {
		return nil, fmt.Errorf("invalid message size %d", t.MsgSize)
	}
	if t.TOS < 0 || t.TOS > 255 {
		return nil, fmt.Errorf("invalid tos %d", t.TOS)
	}
	local := parseIPv4(t.LocalIP)
	peer := parseIPv4(t.PeerIP)
	if t.Kind == api.TaskClient && peer == "" {
		return nil, fmt.Errorf("invalid peer ip %q", t.PeerIP)
	}

	switch t.Tool {
	case api.ToolIperf3:
		if t.Streams < 1 || t.Streams > 128 {
			return nil, fmt.Errorf("invalid stream count %d", t.Streams)
		}
		if local == "" {
			return nil, fmt.Errorf("invalid local ip %q", t.LocalIP)
		}
		args := []string{api.ToolIperf3, "--json", "-p", strconv.Itoa(t.TCPPort), "-B", local}
		if t.Kind == api.TaskServer {
			// -1 exits after one client, so the server reports its own result and never lingers.
			return append(args, "-s", "-1"), nil
		}
		args = append(args, "-c", peer, "-t", strconv.Itoa(t.DurationSec), "-P", strconv.Itoa(t.Streams), "-l", strconv.Itoa(t.MsgSize), "-S", strconv.Itoa(t.TOS))
		switch t.Protocol {
		case "tcp", "":
		case "udp":
			args = append(args, "-u", "-b", "0") // unlimited; loss and jitter are the result
		default:
			return nil, fmt.Errorf("invalid protocol %q", t.Protocol)
		}
		if t.Bidir {
			args = append(args, "--bidir")
		}
		return args, nil

	case api.ToolNCCL:
		return ncclCommand(t, ncclScriptPath)

	case api.ToolWriteBW, api.ToolReadBW, api.ToolSendBW, api.ToolWriteLat, api.ToolReadLat, api.ToolSendLat:
	default:
		return nil, fmt.Errorf("unsupported tool %q", t.Tool)
	}

	if !validName(t.RdmaDev) {
		return nil, fmt.Errorf("invalid rdma device %q", t.RdmaDev)
	}
	switch t.MTU {
	case 256, 512, 1024, 2048, 4096:
	default:
		return nil, fmt.Errorf("invalid rdma mtu %d", t.MTU)
	}
	// Same flags as the PowerShell tool: rdma_cm, TOS 106, MTU 4096, duration mode.
	args := []string{
		t.Tool,
		"-d", t.RdmaDev,
		"-R",
		"-T", strconv.Itoa(t.TOS),
		"-p", strconv.Itoa(t.TCPPort),
		"-m", strconv.Itoa(t.MTU),
		"-s", strconv.Itoa(t.MsgSize),
		"-D", strconv.Itoa(t.DurationSec),
		"-F",
	}
	if strings.HasSuffix(t.Tool, "_bw") {
		if t.QPs < 1 || t.QPs > 1024 {
			return nil, fmt.Errorf("invalid qp count %d", t.QPs)
		}
		args = append(args, "-q", strconv.Itoa(t.QPs), "--report_gbits")
		if t.Bidir {
			args = append(args, "-b")
		}
	}
	if t.Kind == api.TaskClient {
		args = append(args, peer)
	}
	return args, nil
}

func validName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.') {
			return false
		}
	}
	return true
}

// Runner executes tasks and tracks them so a cancel can stop a whole run.
type Runner struct {
	Sampler *Sampler
	Report  func(api.Event)
	Result  func(api.LegResult)

	mu      sync.Mutex
	cancels map[string]context.CancelFunc
	runOf   map[string]int64
}

func NewRunner(s *Sampler, report func(api.Event), result func(api.LegResult)) *Runner {
	return &Runner{Sampler: s, Report: report, Result: result, cancels: map[string]context.CancelFunc{}, runOf: map[string]int64{}}
}

func (r *Runner) Handle(ctx context.Context, t api.Task) {
	switch t.Kind {
	case api.TaskCancel:
		r.cancelRun(t.RunID)
	case api.TaskServer, api.TaskClient:
		go r.run(ctx, t)
	}
}

// Active reports whether any benchmark task is in progress.
func (r *Runner) Active() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.cancels) > 0
}

func (r *Runner) cancelRun(runID int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, c := range r.cancels {
		if r.runOf[id] == runID {
			c()
		}
	}
}

func (r *Runner) run(parent context.Context, t api.Task) {
	res := api.LegResult{TaskID: t.ID, RunID: t.RunID, LegID: t.LegID, Kind: t.Kind, ExitCode: -1}
	fail := func(err error) {
		res.Error = err.Error()
		res.EndedAt = time.Now()
		r.Report(api.Event{TaskID: t.ID, RunID: t.RunID, LegID: t.LegID, Type: api.EventFailed, Message: err.Error()})
		r.Result(res)
	}
	if t.Tool == api.ToolNCCL {
		if _, err := ensureNCCLScript(); err != nil {
			fail(err)
			return
		}
	}
	args, err := BuildCommand(t)
	if err != nil {
		fail(err)
		return
	}
	res.Command = args

	ctx, cancel := context.WithTimeout(parent, taskTimeout(t, time.Now()))
	defer cancel()
	r.mu.Lock()
	r.cancels[t.ID], r.runOf[t.ID] = cancel, t.RunID
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.cancels, t.ID)
		delete(r.runOf, t.ID)
		r.mu.Unlock()
	}()

	r.Sampler.RunStarted(t.RunID)
	defer r.Sampler.RunEnded(t.RunID)

	if t.Kind == api.TaskClient && !t.StartAt.IsZero() {
		select {
		case <-ctx.Done():
			fail(errors.New("canceled before start"))
			return
		case <-time.After(time.Until(t.StartAt)):
		}
	}

	var before map[string]float64
	if t.RdmaDev != "" {
		before = HWCounters(t.RdmaDev, t.PortNum)
	}
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	res.StartedAt = time.Now()
	if err := cmd.Start(); err != nil {
		fail(err)
		return
	}

	if t.Kind == api.TaskServer {
		// perftest has no ready signal; a server still alive after a second is listening.
		exited := make(chan struct{})
		go func() {
			select {
			case <-time.After(time.Second):
				r.Report(api.Event{TaskID: t.ID, RunID: t.RunID, LegID: t.LegID, Type: api.EventServerReady})
			case <-exited:
			}
		}()
		err = cmd.Wait()
		close(exited)
	} else {
		r.Report(api.Event{TaskID: t.ID, RunID: t.RunID, LegID: t.LegID, Type: api.EventStarted})
		err = cmd.Wait()
	}
	res.EndedAt = time.Now()
	res.Output = tail(out.String(), 16*1024)
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
	}
	parsedOK := true
	switch {
	case t.Tool == api.ToolNCCL:
		nr := ParseNCCL(out.String())
		res.AvgGbps, res.PeakGbps, res.Points = nr.LastGbps, nr.PeakGbps, nr.Points
		parsedOK = nr.OK
		if nr.Error != "" && err == nil {
			err = errors.New(nr.Error)
		}
	case t.Tool == api.ToolIperf3:
		ir, ok := ParseIperf3(out.String())
		res.AvgGbps, res.Retransmits, res.LossPct, res.JitterMs = ir.Gbps, ir.Retransmits, ir.LossPct, ir.JitterMs
		parsedOK = ok
	case strings.HasSuffix(t.Tool, "_lat"):
		lat, ok := ParsePerftestLat(out.String())
		if ok {
			res.Latency = &lat
		}
		parsedOK = ok
	default:
		pr := ParsePerftest(out.String())
		res.AvgGbps, res.PeakGbps, res.MsgRateMpps = pr.AvgGbps, pr.PeakGbps, pr.MsgRateMpps
		parsedOK = pr.OK
	}
	res.ErrorDeltas = map[string]int64{}
	if t.RdmaDev != "" {
		after := HWCounters(t.RdmaDev, t.PortNum)
		for _, k := range errorCounters {
			if a, ok := after[k]; ok {
				if d := int64(a - before[k]); d > 0 {
					res.ErrorDeltas[k] = d
				}
			}
		}
	}
	if ctx.Err() != nil {
		res.Error = "timed out or canceled"
	} else if err != nil {
		res.Error = err.Error()
	} else if !parsedOK && (t.Kind == api.TaskClient || t.Tool == api.ToolIperf3 || t.Tool == api.ToolNCCL) {
		res.Error = "no result in tool output"
	}
	if res.Error != "" {
		r.Report(api.Event{TaskID: t.ID, RunID: t.RunID, LegID: t.LegID, Type: api.EventFailed, Message: res.Error})
	}
	r.Result(res)
}

// taskTimeout bounds a task: its duration plus two minutes of slack, plus the
// wait for the shared start time when a client has one. Servers carry no start
// time; time.Until on the zero time is hugely negative and would expire the
// context before the tool even started ("context deadline exceeded").
func taskTimeout(t api.Task, now time.Time) time.Duration {
	limit := time.Duration(t.DurationSec)*time.Second + 2*time.Minute
	if !t.StartAt.IsZero() {
		if wait := t.StartAt.Sub(now); wait > 0 {
			limit += wait
		}
	}
	return limit
}

func parseIPv4(s string) string {
	ip := net.ParseIP(s).To4()
	if ip == nil {
		return ""
	}
	return ip.String()
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
