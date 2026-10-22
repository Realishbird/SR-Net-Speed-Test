package agent

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Realishbird/SR-Net-Speed-Test/internal/api"
)

// NCCL collectives (Phase 6). The agent carries a small PyTorch benchmark
// (nccl_bench.py); each host runs one rank, rank 0 hosts the rendezvous on
// its RDMA fabric address, so no MPI and no SSH between hosts are needed.
// The hosts need PyTorch with CUDA (DGX OS ships it; elsewhere pip install
// torch); the script says so clearly when it is missing.

//go:embed nccl_bench.py
var ncclScript []byte

// ncclScriptPath is where the script is written before a run; tests may
// point it elsewhere.
var ncclScriptPath = func() string {
	sum := sha256.Sum256(ncclScript)
	return filepath.Join(os.TempDir(), "srnet-nccl-"+hex.EncodeToString(sum[:6])+".py")
}()

// ensureNCCLScript writes the bundled benchmark where python3 can run it.
// The name carries a hash of the content, so an agent update never runs a
// stale copy.
func ensureNCCLScript() (string, error) {
	if cur, err := os.ReadFile(ncclScriptPath); err == nil && string(cur) == string(ncclScript) {
		return ncclScriptPath, nil
	}
	if err := os.WriteFile(ncclScriptPath, ncclScript, 0o600); err != nil {
		return "", fmt.Errorf("write nccl benchmark: %w", err)
	}
	return ncclScriptPath, nil
}

// ncclCommand builds the argv of one rank.
func ncclCommand(t api.Task, script string) ([]string, error) {
	if t.WorldSize < 2 || t.WorldSize > 4096 || t.Rank < 0 || t.Rank >= t.WorldSize {
		return nil, fmt.Errorf("invalid rank %d of %d", t.Rank, t.WorldSize)
	}
	master := parseIPv4(t.MasterIP)
	if master == "" {
		return nil, fmt.Errorf("invalid master ip %q", t.MasterIP)
	}
	switch t.Collective {
	case "allreduce", "allgather":
	default:
		return nil, fmt.Errorf("invalid collective %q", t.Collective)
	}
	sizes := t.Sizes
	if len(sizes) == 0 {
		sizes = []int{t.MsgSize}
	}
	parts := make([]string, len(sizes))
	for i, sz := range sizes {
		if sz < 4 || sz > 1<<32 {
			return nil, fmt.Errorf("invalid collective size %d", sz)
		}
		parts[i] = strconv.Itoa(sz)
	}
	if !validName(t.Netdev) && t.Netdev != "" {
		return nil, fmt.Errorf("invalid netdev %q", t.Netdev)
	}
	if t.RdmaDev != "" && !validName(t.RdmaDev) {
		return nil, fmt.Errorf("invalid rdma device %q", t.RdmaDev)
	}
	args := []string{"python3", script, "--rank", strconv.Itoa(t.Rank), "--world", strconv.Itoa(t.WorldSize),
		"--master", master + ":" + strconv.Itoa(t.TCPPort), "--collective", t.Collective, "--sizes", strings.Join(parts, ","),
		"--duration", strconv.Itoa(t.DurationSec)}
	if t.Netdev != "" {
		args = append(args, "--ifname", t.Netdev)
	}
	if t.RdmaDev != "" {
		args = append(args, "--hca", t.RdmaDev)
	}
	return args, nil
}

// NCCLResult is what ParseNCCL reads from the benchmark's JSON lines.
type NCCLResult struct {
	Points   map[string]float64 // size in bytes -> bus bandwidth, Gbps
	LastGbps float64            // bus bandwidth at the largest size
	PeakGbps float64
	Error    string
	OK       bool
}

// ParseNCCL reads the benchmark output: one JSON object per line.
func ParseNCCL(out string) NCCLResult {
	r := NCCLResult{Points: map[string]float64{}}
	largest := 0
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) != nil {
			continue
		}
		if e, ok := m["error"].(string); ok && e != "" {
			r.Error = e
			continue
		}
		if done, _ := m["done"].(bool); done {
			r.OK = true
			continue
		}
		size, okS := m["size"].(float64)
		bus, okB := m["busbw_gbps"].(float64)
		if !okS || !okB {
			continue
		}
		r.Points[strconv.Itoa(int(size))] = bus
		r.PeakGbps = max(r.PeakGbps, bus)
		if int(size) >= largest {
			largest, r.LastGbps = int(size), bus
		}
	}
	if len(r.Points) == 0 {
		r.OK = false
	}
	return r
}
