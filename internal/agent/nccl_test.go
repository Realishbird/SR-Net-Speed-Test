package agent

import (
	"strings"
	"testing"

	"github.com/Realishbird/SR-Net-Speed-Test/internal/api"
)

func TestNCCLCommand(t *testing.T) {
	tk := api.Task{Tool: api.ToolNCCL, Kind: api.TaskClient, Rank: 2, WorldSize: 3, MasterIP: "172.27.100.10", TCPPort: 18515, Collective: "allreduce",
		Sizes: []int{1 << 20, 64 << 20}, DurationSec: 30, MsgSize: 64 << 20, Netdev: "enp1s0f0np0", RdmaDev: "mlx5_0", LocalIP: "172.27.100.12", PeerIP: "172.27.100.10"}
	args, err := BuildCommand(tk)
	if err != nil {
		t.Fatal(err)
	}
	want := "python3 " + ncclScriptPath + " --rank 2 --world 3 --master 172.27.100.10:18515 --collective allreduce --sizes 1048576,67108864 --duration 30 --ifname enp1s0f0np0 --hca mlx5_0"
	if got := strings.Join(args, " "); got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	// Without sizes the message size is the one size.
	tk.Sizes = nil
	args, _ = BuildCommand(tk)
	if !strings.Contains(strings.Join(args, " "), "--sizes 67108864 ") {
		t.Errorf("single size: %v", args)
	}
	for name, bad := range map[string]func(*api.Task){
		"rank":       func(t *api.Task) { t.Rank = 3 },
		"world":      func(t *api.Task) { t.WorldSize = 1 },
		"master":     func(t *api.Task) { t.MasterIP = "rank0; rm -rf /" },
		"collective": func(t *api.Task) { t.Collective = "broadcast" },
		"size":       func(t *api.Task) { t.Sizes = []int{2} },
		"netdev":     func(t *api.Task) { t.Netdev = "eth0 --evil" },
	} {
		x := tk
		bad(&x)
		if _, err := BuildCommand(x); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestParseNCCL(t *testing.T) {
	out := `NCCL INFO Bootstrap : Using enp1s0f0np0:172.27.100.12<0>
{"size": 1048576, "iters": 2040, "time_us": 118.2, "algbw_gbps": 70.9, "busbw_gbps": 47.3}
{"size": 67108864, "iters": 170, "time_us": 5700.0, "algbw_gbps": 94.2, "busbw_gbps": 125.6}
{"done": true, "ranks": 4, "collective": "allreduce", "max_busbw_gbps": 125.6, "avg_busbw_gbps": 86.45}
`
	r := ParseNCCL(out)
	if !r.OK || r.LastGbps != 125.6 || r.PeakGbps != 125.6 || r.Points["1048576"] != 47.3 || len(r.Points) != 2 || r.Error != "" {
		t.Errorf("%+v", r)
	}
	r = ParseNCCL(`{"error": "PyTorch is not installed on this host (pip install torch); NCCL tests need it: No module named 'torch'"}`)
	if r.OK || !strings.Contains(r.Error, "PyTorch is not installed") || len(r.Points) != 0 {
		t.Errorf("%+v", r)
	}
	if r := ParseNCCL("Traceback (most recent call last)\n  boom"); r.OK || r.LastGbps != 0 {
		t.Errorf("garbage: %+v", r)
	}
}

func TestNCCLScriptIsWrittenOnce(t *testing.T) {
	old := ncclScriptPath
	ncclScriptPath = t.TempDir() + "/bench.py"
	defer func() { ncclScriptPath = old }()
	p, err := ensureNCCLScript()
	if err != nil || p != ncclScriptPath {
		t.Fatal(err, p)
	}
	if _, err := ensureNCCLScript(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ncclScript), "init_process_group") {
		t.Error("bundled script lacks the rendezvous")
	}
}
