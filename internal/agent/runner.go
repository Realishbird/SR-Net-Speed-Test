package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strconv"
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
	switch t.Tool {
	case api.ToolWriteBW, api.ToolReadBW:
	default:
		return nil, fmt.Errorf("unsupported tool %q", t.Tool)
	}
	if !validName(t.RdmaDev) {
		return nil, fmt.Errorf("invalid rdma device %q", t.RdmaDev)
	}
	if t.TCPPort < 1024 || t.TCPPort > 65535 {
		return nil, fmt.Errorf("invalid tcp port %d", t.TCPPort)
	}
	if t.DurationSec < 1 || t.DurationSec > 24*3600 {
		return nil, fmt.Errorf("invalid duration %d", t.DurationSec)
	}
	if t.MsgSize < 1 || t.MsgSize > 1<<30 {
		return nil, fmt.Errorf("invalid message size %d", t.MsgSize)
	}
	if t.QPs < 1 || t.QPs > 1024 {
		return nil, fmt.Errorf("invalid qp count %d", t.QPs)
	}
	switch t.MTU {
	case 256, 512, 1024, 2048, 4096:
	default:
		return nil, fmt.Errorf("invalid rdma mtu %d", t.MTU)
	}
	if t.TOS < 0 || t.TOS > 255 {
		return nil, fmt.Errorf("invalid tos %d", t.TOS)
	}
	// Same flags as the PowerShell tool: rdma_cm, TOS 106, MTU 4096, duration mode, Gbit/s output.
	args := []string{
		t.Tool,
		"-d", t.RdmaDev,
		"-R",
		"-T", strconv.Itoa(t.TOS),
		"-p", strconv.Itoa(t.TCPPort),
		"-m", strconv.Itoa(t.MTU),
		"-s", strconv.Itoa(t.MsgSize),
		"-q", strconv.Itoa(t.QPs),
		"-D", strconv.Itoa(t.DurationSec),
		"-F", "--report_gbits",
	}
	if t.Kind == api.TaskClient {
		ip := parseIPv4(t.PeerIP)
		if ip == "" {
			return nil, fmt.Errorf("invalid peer ip %q", t.PeerIP)
		}
		args = append(args, ip)
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
	args, err := BuildCommand(t)
	if err != nil {
		fail(err)
		return
	}
	res.Command = args

	// A server waits for its client; a client waits for the shared start time.
	limit := time.Duration(t.DurationSec)*time.Second + 2*time.Minute
	ctx, cancel := context.WithTimeout(parent, limit+time.Until(t.StartAt))
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

	before := HWCounters(t.RdmaDev, t.PortNum)
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
	pr := ParsePerftest(out.String())
	res.AvgGbps, res.PeakGbps, res.MsgRateMpps = pr.AvgGbps, pr.PeakGbps, pr.MsgRateMpps
	after := HWCounters(t.RdmaDev, t.PortNum)
	res.ErrorDeltas = map[string]int64{}
	for _, k := range errorCounters {
		if a, ok := after[k]; ok {
			if d := int64(a - before[k]); d > 0 {
				res.ErrorDeltas[k] = d
			}
		}
	}
	if ctx.Err() != nil {
		res.Error = "timed out or canceled"
	} else if err != nil {
		res.Error = err.Error()
	} else if !pr.OK {
		res.Error = "no result row in perftest output"
	}
	if res.Error != "" {
		r.Report(api.Event{TaskID: t.ID, RunID: t.RunID, LegID: t.LegID, Type: api.EventFailed, Message: res.Error})
	}
	r.Result(res)
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
