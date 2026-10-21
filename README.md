# SR-Net-Speed-Test

RDMA and network speed test service: a controller in a Proxmox LXC plus a small agent on every test host. It replaces the PowerShell fan-out script with something that runs single-link and aggregate (one-to-many, many-to-one, paired) `ib_write_bw` / `ib_read_bw` tests from one place, and records NIC, transceiver and host telemetry while they run.

```
Operator (srnet CLI, Grafana)
        |
        v
Proxmox LXC: srnet-controller + Postgres + VictoriaMetrics + Grafana   (management network only)
        ^                 ^                 ^
        |  HTTPS, agents dial out, no SSH   |
   srnet-agent        srnet-agent       srnet-agent ...
   (host A)           (host B)          (host N)
        \______ RDMA fabrics: test traffic only ______/
```

Phase 1 (this version) covers: enrollment, write and read bandwidth tests, synchronised start across legs, per-leg results from both the perftest output and the RDMA hardware counters, pass/warn/fail verdicts, 1 s telemetry during runs, and a CLI. The web UI, latency and iperf3 profiles, full mesh, switch (SONiC) telemetry and fleet upgrades come in later phases.

## 1. Deploy the controller (Proxmox node shell)

```bash
bash -c "$(curl -fsSL https://raw.githubusercontent.com/Realishbird/SR-Net-Speed-Test/main/proxmox/create-lxc.sh)"
```

Defaults: unprivileged Debian 12 container, 4 cores, 8 GB RAM, 100 GB disk, DHCP on `vmbr0`, no Docker. Every default is a flag (`--ctid`, `--storage`, `--bridge`, `--vlan`, `--ip`, `--cores`, `--memory`, `--disk`, `--password`). The script prints the admin password, the Grafana URL and the CA fingerprint when it finishes. `--upgrade <CTID>` upgrades the stack inside an existing container.

Inside the container the stack is plain systemd services: `srnet-controller` (port 8443), `postgresql`, `victoriametrics` (127.0.0.1:8428, 90 days retention), `grafana-server` (port 3000).

## 2. Install the agent on each host (Ubuntu 22.04/24.04, Debian 12)

Get a one-time install command from the controller:

```bash
pct exec <CTID> -- srnet token
```

It prints a line like this, valid for one hour, to paste on the host:

```bash
curl -fsSL https://raw.githubusercontent.com/Realishbird/SR-Net-Speed-Test/main/install.sh \
  | sudo bash -s -- --controller https://10.0.0.5:8443 --token <token> --ca-sha256 <fingerprint>
```

`install.sh` installs `rdma-core`, `perftest`, `iperf3`, `ethtool`, `chrony` and `lldpd` from apt, downloads the signed-checksum agent binary for the host's architecture (amd64 or arm64), pins the controller's CA by fingerprint, enrolls, and starts `srnet-agent.service` as the unprivileged `srnet` user. The only sudo rule it adds is `ethtool -m` for transceiver readings. Re-running upgrades in place; `--uninstall` removes everything.

## 3. Name your devices

Hosts enroll under their Linux hostname. Rename and tag them from the controller:

```bash
srnet hosts                                   # ids, ports, link state, live Gbps
srnet host 2 --name "Dell Spark 1" --tags server,spark --site "Lab 2"
srnet port 20 --label "NIC 1" --fabric 100 --expected-gbps 200
srnet import hosts.csv                        # bulk: host,name,tags,site,rack,ip,label,fabric,expected_gbps
```

Nothing is hard-coded to any model of host. Test plans refer to hosts by name, `tag:<tag>`, RDMA IP or port label.

## 4. Run tests

```bash
srnet run --test write --clients "Dell 770AP" --servers tag:server --duration 60   # fan-out, aggregate
srnet run --test read  --clients tag:spark --servers "Dell 770AP"                  # fan-in
srnet run --legs "Dell 770AP>Dell Spark 1"                                         # single link
srnet run --clients a,b,c --servers d,e,f --pairs                                  # disjoint pairs
srnet runs; srnet show 12; srnet watch 12; srnet cancel 12
```

For each leg the controller picks the client and server ports that share a subnet, checks both are `ACTIVE`, starts every server, then starts every client at one shared time so aggregate load is simultaneous. Results show the perftest average, the rate derived from `port_rcv_data` / `port_xmit_data` on the unshared side of the leg, growth in RDMA error counters, and a verdict: `fail` below `--pass-pct` (default 90 %) of the expected line rate, `warn` when error counters such as `out_of_sequence` grew.

The API is the same thing the CLI uses (`/api/v1/...`, basic auth with the admin account); the web UI in Phase 2 will sit on top of it.

## Telemetry

Agents sample every second during a run and every 15 s otherwise, and the controller writes everything to VictoriaMetrics as `srnet_*` metrics labelled by host, RDMA device, netdev, port label, fabric and run id:

- RDMA port counters (`port_xmit_data`, `port_rcv_data`, errors, discards) and every `hw_counters` entry (out of sequence, retries, CNP, ECN)
- NIC drop, pause, FEC and error counters from `ethtool -S`, link speed and carrier flaps
- Transceiver DOM from `ethtool -m`: temperature, voltage, per-channel TX bias and optical power; power is reported as `dom_power_w` when the module measures it and `dom_power_rated_max_w` when only a rating is available
- CPU, memory, load and every `hwmon` temperature

Grafana is provisioned with VictoriaMetrics as its data source; dashboards come in Phase 2.

## Development

```bash
go build ./... && go test ./...
go run ./cmd/srnet-agent inventory      # on an RDMA host: what the agent would report
go run ./cmd/srnet-agent sample
```

Tagging `vX.Y.Z` builds amd64 and arm64 binaries for all three commands, writes `SHA256SUMS`, and publishes a GitHub Release that the install scripts download.
