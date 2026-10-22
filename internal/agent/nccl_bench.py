#!/usr/bin/env python3
"""NCCL collective benchmark run by the srnet agent, one process per host.

Each host is one rank with one GPU. Rank 0 hosts the rendezvous (a TCP
store on the RDMA fabric address the controller picked), so no MPI and no
SSH between hosts are needed. For every message size the collective runs
for a slice of the duration and the script prints one JSON line with the
average time, algorithm bandwidth and bus bandwidth (the figures nccl-tests
reports), then a final line with the summary.
"""
import argparse, json, os, sys, time

ap = argparse.ArgumentParser()
ap.add_argument("--rank", type=int, required=True)
ap.add_argument("--world", type=int, required=True)
ap.add_argument("--master", required=True, help="ip:port of rank 0")
ap.add_argument("--collective", default="allreduce", choices=["allreduce", "allgather"])
ap.add_argument("--sizes", default="1048576", help="bytes per step, comma separated")
ap.add_argument("--duration", type=float, default=30, help="seconds for the whole sweep")
ap.add_argument("--ifname", default="", help="netdev NCCL uses for its bootstrap sockets")
ap.add_argument("--hca", default="", help="RDMA device NCCL uses (NCCL_IB_HCA)")
a = ap.parse_args()

if a.ifname:
    os.environ.setdefault("NCCL_SOCKET_IFNAME", a.ifname)
    os.environ.setdefault("GLOO_SOCKET_IFNAME", a.ifname)
if a.hca:
    os.environ.setdefault("NCCL_IB_HCA", a.hca)
os.environ.setdefault("NCCL_IB_DISABLE", "0")

def out(obj):
    print(json.dumps(obj), flush=True)

try:
    import torch
    import torch.distributed as dist
except Exception as e:  # noqa: BLE001
    out({"error": "PyTorch is not installed on this host (pip install torch); NCCL tests need it: %s" % e})
    sys.exit(2)
if not torch.cuda.is_available():
    out({"error": "PyTorch sees no CUDA device on this host; NCCL tests need a GPU"})
    sys.exit(2)

host, port = a.master.rsplit(":", 1)
torch.cuda.set_device(0)
import datetime
dist.init_process_group("nccl", init_method="tcp://%s:%s" % (host, port), rank=a.rank, world_size=a.world,
                        timeout=datetime.timedelta(seconds=120))
sizes = [int(s) for s in a.sizes.split(",") if s.strip()]
per = max(2.0, a.duration / max(1, len(sizes)))
n = a.world
results = []
for size in sizes:
    elems = max(1, size // 4)
    if a.collective == "allreduce":
        x = torch.ones(elems, dtype=torch.float32, device="cuda")
        op = lambda: dist.all_reduce(x)
        total = elems * 4
        bus = lambda alg: alg * 2 * (n - 1) / n
    else:
        x = torch.ones(elems, dtype=torch.float32, device="cuda")
        y = [torch.empty(elems, dtype=torch.float32, device="cuda") for _ in range(n)]
        op = lambda: dist.all_gather(y, x)
        total = elems * 4 * n
        bus = lambda alg: alg * (n - 1) / n
    for _ in range(5):
        op()
    torch.cuda.synchronize()
    dist.barrier()
    iters, t0 = 0, time.perf_counter()
    while True:
        op()
        iters += 1
        if iters % 10 == 0:
            torch.cuda.synchronize()
            if time.perf_counter() - t0 >= per:
                break
    torch.cuda.synchronize()
    dt = (time.perf_counter() - t0) / iters
    algbw = total * 8 / dt / 1e9
    r = {"size": size, "iters": iters, "time_us": dt * 1e6, "algbw_gbps": algbw, "busbw_gbps": bus(algbw)}
    results.append(r)
    out(r)
dist.barrier()
out({"done": True, "ranks": n, "collective": a.collective, "max_busbw_gbps": max(r["busbw_gbps"] for r in results),
     "avg_busbw_gbps": sum(r["busbw_gbps"] for r in results) / len(results)})
dist.destroy_process_group()
