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

What it does today: enrollment, RDMA write/read/send bandwidth, RDMA latency and iperf3 TCP/UDP tests; fan-out, fan-in, paired, full mesh and explicit legs; serial link matrix and message size sweeps; synchronised start across legs; per-leg results from both the tool output and the hardware counters; pass/warn/fail verdicts; 1 s telemetry during runs; a web UI and a CLI; Grafana dashboards; SONiC switch port counters over gNMI or SNMP with LLDP mapping of host ports to switch ports. Agent updates pushed from the controller, schedules and alerts come in Phase 3.

## 1. Deploy the controller (Proxmox node shell)

```bash
bash -c "$(curl -fsSL https://raw.githubusercontent.com/Realishbird/SR-Net-Speed-Test/main/proxmox/create-lxc.sh)"
```

Defaults: unprivileged Debian 12 container, 4 cores, 8 GB RAM, 100 GB disk, DHCP on `vmbr0`, no Docker. Every default is a flag (`--ctid`, `--storage`, `--bridge`, `--vlan`, `--ip`, `--cores`, `--memory`, `--disk`, `--password`). The script prints the admin password, the Grafana URL and the CA fingerprint when it finishes. Change the password later with the Password button at the bottom of the UI's sidebar, or from inside the container with `srnet-controller passwd --admin-password-file <file>` followed by a restart of `srnet-controller`. `--upgrade <CTID>` upgrades the stack inside an existing container.

Inside the container the stack is plain systemd services: `srnet-controller` (port 8443, web UI and API), `postgresql`, `victoriametrics` (127.0.0.1:8428, 90 days retention), `grafana-server` (port 3000, three SR-Net dashboards provisioned), and `gnmic` (idle until a switch with gNMI is configured).

The web UI at `https://<lxc ip>:8443` (admin login) covers hosts and naming, launching runs, live per-leg rates and results. Everything it does is also in the `srnet` CLI below.

## 2. Install the agent on each host (Ubuntu 22.04/24.04, Debian 12)

Get an install command from the controller (Hosts page > "Agent install command", or the CLI). The token in it works on up to 10 hosts for one hour, so one command covers a batch of machines:

```bash
pct exec <CTID> -- /usr/local/bin/srnet token
```

It prints a line like this, valid for one hour, to paste on the host:

```bash
curl -fsSL https://raw.githubusercontent.com/Realishbird/SR-Net-Speed-Test/main/install.sh \
  | sudo bash -s -- --controller https://10.0.0.5:8443 --token <token> --ca-sha256 <fingerprint>
```

`install.sh` installs `rdma-core`, `perftest`, `iperf3`, `ethtool`, `chrony` and `lldpd` from apt, downloads the signed-checksum agent binary for the host's architecture (amd64 or arm64), pins the controller's CA by fingerprint, enrolls, and starts `srnet-agent.service` as the unprivileged `srnet` user. It adds no sudo rules: the service's ambient `CAP_NET_ADMIN` covers `ethtool -m` for transceiver readings and its supplementary group `adm` covers `lldpctl` for switch neighbours (the sudo rules older versions wrote never worked under the service's capability bounding set and are removed on upgrade). Re-running upgrades in place; `--uninstall` removes everything.

Hosts without internet, and fleets of hundreds, have two more ways in (see "Air-gapped installs" and "Enrolment without a token" below).

## 3. Name your devices

Hosts enroll under their Linux hostname. Rename and tag them from the controller:

```bash
srnet hosts                                   # ids, ports, link state, live Gbps
srnet host 2 --name "Dell Spark 1" --tags server,spark --site "Lab 2"
srnet port 20 --label "NIC 1" --fabric 100 --expected-gbps 200
srnet import hosts.csv                        # bulk: host,name,tags,site,rack,ip,label,fabric,expected_gbps
```

Nothing is hard-coded to any model of host. Test plans refer to hosts by name, `tag:<tag>`, RDMA IP, port label or `host/netdev`. A host (or tag) selector tests every port it shares a fabric with the other side, so a Spark with P1 on fabric 100 and P2 on fabric 101 gets a leg on each; `--ports one` keeps one link per host pair, spread over the ports, and `--fabric 101` keeps only ports on that fabric (a port's fabric label, else the fabric of its subnet). The New run page lists each host's ports next to it and previews the legs before the run starts. On the Hosts page the second PCIe function of a Spark port (enP2p1s0f0np0, the ConnectX-7's second x4 link on the same cable) sits under the port it shares the cable with.

### Port addresses and MTU (netplan)

Each RDMA port on the Hosts page has an **Edit** button for its addresses, MTU and DHCP. The agent never runs as root, so `install.sh` adds a small root helper (`srnet-netplan.path`, `.service` and `.timer`, running the root-owned `/usr/local/bin/srnet-agent netplan-helper`; hosts installed before this feature need the install command run once more). The helper:

- refuses the management port (the one with the default route or the route to the controller) and anything that is not an RDMA netdev, and refuses while a test runs on the host (no test starts on it until the change is done);
- moves the port's stanza out of the other files in `/etc/netplan` (keeping its other settings, such as `optional` or `routes`) into `/etc/netplan/90-srnet.yaml`, because netplan adds up the addresses of every file that defines a port;
- runs `netplan generate`, then `netplan apply`, and keeps the change only if the port shows the address and MTU with link up, a fabric peer answers a ping from it, and the agent still reaches the controller. Otherwise every file goes back as it was and is applied again. Backups of the last 10 changes stay in `/var/lib/srnet-netplan/backups`, and every change is in the audit log.

**Copy netplan** shows the file and the `netplan try` lines to apply it by hand, for hosts without the helper.

**Auto IP** (Hosts page) turns each switch VLAN with hosts on it into a fabric (its subnet from the fabric profile, else from its ports' addresses) and places every cabled RDMA port in the fabric of its switch port's VLAN. Addresses that fit are kept. A port with none gets the host's number plus 10 per PCI function (servers .1, .2 on single-port cards; a Spark's port 1 at .1x and port 2 at .2x), or the next free address from .10. Proposed addresses can be edited before **Apply ticked**, which creates the new fabrics, stores each port's fabric and applies the changes one host at a time through the helper. The twin PCIe function of a Spark port and the management port are left out. A switch port in the wrong VLAN, or below the fabric's switch MTU, comes back as `sonic-cli` lines to paste: the service only ever reads the switch. Lines are only written for interfaces in the switch's own port table, under the name its CLI takes (the alias in standard naming mode, the name in native mode).

## 4. Run tests

```bash
srnet run --test write --clients "Dell 770AP" --servers tag:server --duration 60   # fan-out, aggregate
srnet run --test read  --clients tag:spark --servers "Dell 770AP"                  # fan-in, every Spark port
srnet run --clients tag:spark --servers "Dell 770AP" --fabric 101                  # fan-in, fabric 101 ports only
srnet run --clients "Dell 770AP" --servers tag:spark --ports one                   # one link per Spark, spread over both 770AP NICs
srnet run --legs "Dell 770AP>Dell Spark 1"                                         # single link
srnet run --clients a,b,c --servers d,e,f --pairs                                  # disjoint pairs
srnet run --mesh tag:spark                                                         # full mesh, every ordered pair
srnet run --clients "Dell 770AP" --servers tag:server --serial                     # link matrix, one leg at a time
srnet run --sizes 4096,65536,1048576 --clients "Dell 770AP" --servers tag:server  # message size sweep
srnet run --test write_lat --legs "Dell 770AP>Dell Spark 1" --max-p99-us 5        # latency, fail above 5 µs p99
srnet run --test tcp --streams 8 --clients "Dell 770AP" --servers "Dell Spark 1"  # iperf3 TCP
srnet run --test udp --max-loss-pct 0.5 --clients "Dell 770AP" --servers "Dell Spark 1"
srnet run --test nccl_allreduce --hosts tag:spark --sizes 1048576,67108864,268435456 # GPU collective over every Spark
srnet runs; srnet show 12; srnet watch 12; srnet cancel 12
```

| Test | Tool | Result per leg |
| --- | --- | --- |
| `write`, `read`, `send` | `ib_write_bw`, `ib_read_bw`, `ib_send_bw` (`--bidir` for both directions; every rate and the expected rate then add both directions, so a 100G link expects 200) | Gbps from the tool and from the hardware counters, error counter growth |
| `write_lat`, `read_lat`, `send_lat` | `ib_*_lat` | min, typical, avg, p99, p99.9, max in µs |
| `tcp` | `iperf3` (`--streams N`) | Gbps, retransmits |
| `udp` | `iperf3 -u` | Gbps, loss %, jitter |
| `nccl_allreduce`, `nccl_allgather` | the agent's bundled PyTorch benchmark, one GPU rank per host | bus bandwidth (as nccl-tests reports it) at the largest size, per size with `--sizes` |

For each leg the controller picks the client and server ports that share a subnet, checks both are `ACTIVE`, starts every server, then starts every client at one shared time so aggregate load is simultaneous. Verdicts: `fail` below `--pass-pct` (default 90 %) of the expected line rate, above `--max-p99-us`, or above `--max-loss-pct`; `warn` when error counters such as `out_of_sequence` grew or TCP retransmitted.

The API is the same thing the CLI uses (`/api/v1/...`, basic auth with the admin account); the web UI in Phase 2 will sit on top of it.

## Telemetry

Agents sample every second during a run and every 15 s otherwise, and the controller writes everything to VictoriaMetrics as `srnet_*` metrics labelled by host, RDMA device, netdev, port label, fabric and run id:

- RDMA port counters (`port_xmit_data`, `port_rcv_data`, errors, discards) and every `hw_counters` entry (out of sequence, retries, CNP, ECN)
- NIC drop, pause, FEC and error counters from `ethtool -S`, link speed and carrier flaps
- Transceiver DOM from `ethtool -m`: temperature, voltage, per-channel TX bias and optical power; power is reported as `dom_power_w` when the module measures it and `dom_power_rated_max_w` when only a rating is available
- CPU, memory, load and every `hwmon` temperature
- The NIC chip temperature per port (`nic_temp_c`, the hwmon sensor behind the port's PCI function, mlx5 and bnxt alike) next to the optic's DOM temperature; the Topology page shows both per port, amber near the limit and red above it (`nic_max_c` in `controller.json`, default 100; `transceiver_max_c`, default 70), and a `nic_hot` alert opens above the limit
- The PCIe link behind each port: generation and width negotiated against what the card and the slot support (`pcie_link_gen`, `pcie_link_width`) plus the AER error counters (`pcie_aer_correctable`, `pcie_aer_nonfatal`, `pcie_aer_fatal`)

Grafana is provisioned with VictoriaMetrics as its data source and five dashboards: **Live run** (pick a run id), **Link health** (error counters, optics and flaps per host over time), **Fleet health**, **Trends** (every finished run's result per link, with the baseline it was compared against) and **Switch** (see below).

### Raw export

Every sample, as collected, is downloadable: the run page has **Raw data (CSV)** and **Raw (zip)** buttons, suite runs have a **Raw data** link, and the CLI has `srnet raw`. One row per reading, counters as counted (not pre-converted to rates), covering the run plus 30 s either side, from every host in the run and from the switch ports on its path:

```
time,unix_ms,source,name,port,rdma_dev,port_label,fabric,metric,sub,run_id,value,labels
2026-10-22T02:44:33.770Z,1792637073770,host,Dell Spark 1,enp1s0f0np0,rocep1s0f0,P1-B1-A,100,rdma_rx_bytes,,9,1899102384,configured=yes
2026-10-22T02:44:30.000Z,1792637070000,switch,z9864F,Ethernet0,,,,gnmi_openconfig_..._counters_in_octets,,,884213312,source=z9864f
```

```bash
srnet raw 42 -o run42.csv                       # host and mapped switch samples
srnet raw 42 --format zip -o run42.zip          # manifest.json (run, legs, hosts, switches, fabric profiles) + host_samples.csv + switch_samples.csv
srnet raw 42 --format jsonl --switch all        # VictoriaMetrics export lines, every switch port
srnet raw --from -1h --hosts "Dell Spark 1"     # a time window instead of a run (up to 7 days), idle telemetry included
```

The same through the API: `GET /api/v1/runs/{id}/raw?format=csv|jsonl|zip&switch=mapped|all|none`, `GET /api/v1/suite-runs/{id}/raw.zip` (one `run-<id>/raw.zip` per run inside) and `GET /api/v1/export?from=&to=&hosts=&switch=&format=` (RFC 3339, unix seconds, or relative like `-30m`). Exports stream, so a long run with many hosts does not need memory on the controller.

### PCIe check

The Hosts page shows each port's PCIe link ("Gen5 x4") with the card's maximum on hover, red when the link trained below what the card can do. The Fabric check page has a **PCIe** row per cable: fail when the link is downtrained, warn when the slot is narrower than the card or when the usable PCIe bandwidth (per-lane rate × lanes × 128/130 × 0.85, about 108 Gbps for Gen5 x4, 215 for Gen4 x16, 430 for Gen5 x16) is below the port's line rate. A `pcie_degraded` alert opens when a sampled link drops below the card's maximum while running. On a DGX Spark each half of the ConnectX-7 sits behind its own Gen5 x4 link, which caps one half at roughly 100 Gbps: that is why both halves are needed for 200G.

## Baselines and regression

Open a finished run and click **Set as baseline** (or `srnet baseline <id>`). Every later run of the same shape (test, direction, QPs, MTU) is compared leg by leg with the newest baseline: the run page and the CSV and HTML reports show the baseline figure and the change in percent, and a leg that falls more than the run's allowance behind (default 5%, `regress_pct` in the spec, "Warn % behind baseline" on the New run form) warns with a note naming the baseline run. Latency tests compare p99, lower is better. A leg matches when its client, server and message size match, so a single-link run is compared with the matching leg of a baseline fan-out. Each finished leg is also written to VictoriaMetrics as `srnet_result_*` series for the Trends dashboard. The **Nightly acceptance** button on the Suites page creates a write, send and read suite between chosen hosts and schedules it, which is the acceptance suite the plan's Phase 3 gate asks for.

## Workload-shaped tests

The Phase 6 tests put the fabric under the load real jobs produce rather than one stream per link:

- **Incast and fan-out.** A port used by several legs of the same stage can give each only its fair share, so the expected rate is capped by the shared port (eight 100G Sparks into one 100G port expect 12.5 Gbps each, a 400G port feeding four 100G hosts still expects 100) and the leg note names the shared port. The run summary carries **fairness**, the slowest leg over the fastest in the last stage (1 means every leg got the same share), and **congestion**, the ECN marks and CNPs the hosts counted plus the queue drops and PFC pauses that grew on the legs' switch ports.
- **Size sweep.** The "Size sweep" button on the New run form fills 256 B to 1 MiB (one stage per size), and a run with several sizes draws a chart of Gbps over message size with the expected rate as a reference, so the size at which a link reaches line rate is visible at a glance.
- **NCCL collectives.** `nccl_allreduce` and `nccl_allgather` run every selected host as one GPU rank through a small PyTorch benchmark the agent carries (`internal/agent/nccl_bench.py`); the first host runs rank 0 and hosts the rendezvous on its RDMA address, the others join 3 s later, so no MPI, SSH or shared filesystem is needed. The run reports bus bandwidth the way nccl-tests does, for each size of a sweep in one process, and passes when the largest size reaches `--pass-pct` of the slowest link's rate. Hosts need PyTorch with CUDA (DGX OS ships it; the Hosts page shows the `torch`, `cuda` and `nccl` versions once the agent finds them), and a host without it fails the run with a note saying so.

## Fabric check

The **Fabric** page lists every cable the controller knows (host port to switch port, from LLDP) with each setting on both ends against the fabric's profile. The switch side is read over gNMI once a minute next to the LLDP and VLAN poll: port MTU, speed, FEC and admin state (`sonic-port`), the DSCP-to-queue map and PFC priorities bound to the port (`sonic-port-qos-map`), and the OpenConfig QoS interface tree (PFC enable and pause counters per priority, queue drops, ECN marks, WRED profile). The host side comes with the agent's inventory: netdev MTU, RoCE mode (v2 GID present), PFC, trust mode and DSCP map (`dcb pfc/app show`, or `mlnx_qos`), and the mlx5 ECN knobs (`/sys/class/net/<dev>/ecn`: notification and reaction point per priority, CNP DSCP).

A **fabric profile** per fabric (Fabric page, "Set profile") says what is expected; it starts from a preset, **lossless RoCE** (DSCP 26 to queue 3, PFC on priority 3, ECN on queue 3, CNPs on DSCP 48, host trust dscp, host ECN on) or **lossy** (PFC off, no QoS map, drops allowed), and every field can be edited. Without a profile only consistency is checked: both ends agree on speed and PFC, the switch VLAN matches the fabric, the link is ACTIVE and admin up, and the MTUs carry a 4096-byte RDMA MTU. A failing cable opens a `fabric_mismatch` alert (closed when it passes again), a run that starts on it carries the findings in its summary (run page, "Fabric" line), and the per-port QoS counters gnmic now streams give each leg the queue drops, ECN marks and PFC pauses that grew on its switch ports during the test (the "Error counters" column, `switch Ethernet0 queue 0 dropped_pkts +n`).

## Switch telemetry (Enterprise SONiC)

Open **Switches** in the web UI, click **Add switch**, and enter the management IP plus a read-only SNMP community, a gNMI user and password, or both. Saving tests SNMP straight away; gNMI data shows within a minute. The same thing from the shell:

```bash
srnet-controller switch --name z9864f --address 10.0.0.2 --snmp-community public --gnmi-user admin --gnmi-password secret
systemctl restart srnet-controller
```

With gNMI credentials the controller writes `/etc/srnet/gnmic.yaml` (a path unit restarts `gnmic` when it changes) and `gnmic` streams OpenConfig interface counters every 30 s and transceiver state every 60 s as `srnet_gnmi_*` metrics (the fastest rates Enterprise SONiC 4.5 accepts; the switch logs `Invalid Sample Interval` for anything shorter). With only an SNMP community the controller polls IF-MIB every 30 s as `srnet_switch_*` metrics (octets, errors, discards, oper status, derived Gbps). Both can be set; gNMI is preferred. The switch is only ever read.

Because the switch refuses gNMI samples under 30 s, the controller also polls the ports that are up over SNMP every 2 s while a run is active (`switch_fast_poll_sec` in `controller.json`, `-1` to turn it off): in/out octets, errors and discards, so the switch's rate lines up with the hosts' one-second counters on the Live run dashboard and in the raw export. Set both an SNMP community and gNMI credentials to get this.

For each cabled port gnmic also streams, in the one per-port subscription (several streams at once make this SONiC drop one with "queue disposed"): egress queue counters and watermarks (transmit, dropped, ECN-marked and WRED-dropped packets, watermark and periodic watermark in bytes and percent), ingress priority-group watermarks (shared and headroom), PFC counters and the PFC watchdog, the Ethernet error counters (CRC, fragments, jabbers, oversize, undersize) with the in/out frame-size histogram, and the Ethernet state (FEC mode, advertised speed, link-down reason). Switch CPU and memory stream every 30 s. PSUs, fans, temperature sensors, uptime and the last reboot cause are keyed by names the switch invents, so the controller finds the valid component names once (probing `PSU n`, `FAN n`, `TEMP n`) and polls them every minute, written as `srnet_switch_temp_c{sensor}`, `srnet_switch_fan_ok{fan}`, `srnet_switch_psu_ok{psu}`, `srnet_switch_cpu_pct`, `srnet_switch_mem_used_pct` and `srnet_switch_uptime_seconds`; the Switches page shows them as a health strip. This SONiC build does not expose FEC corrected/uncorrected counters or pre-FEC BER, per-lane optic power, or buffer-pool totals; CRC errors plus optic power and bias trends are the signals for a bad cable or optic. Every minute the controller also writes `srnet_switch_port_info{switch, port, vlan, host, host_port, fabric}` so dashboard legends can say which host hangs off each switch port.

### Switch dashboard

**SR-Net: Switch** in Grafana is built only from what the switch itself reports: pick the switch and ports at the top (the **Switch view** button on a run page opens it on that run's time range and ports, and runs show as annotations). Rows: overview stats (ports up, total in/out, queue drops, PFC pauses, CRC errors, CPU, memory, hottest sensor and optic, failed fans or PSUs, uptime), throughput per port (gNMI 30 s, SNMP 2 s during runs, utilisation against line rate, packets, average frame size), congestion and buffers (drops, ECN marks and transmit packets per queue, queue and priority-group watermarks), PFC (pause frames per priority, enable and watchdog table), errors and link quality (CRC, malformed frames, SNMP discards and errors, FEC and link state table, in/out frame-size histograms), optics (temperature with 65/70 thresholds, receive and transmit power, bias current) and switch health (CPU, memory, every temperature sensor against its limit, fans and PSUs). Together with the per-leg switch figures on a run page this is the switch review: throughput against line rate, drops by queue, buffer peaks, PFC, errors and optics per port.

On Enterprise SONiC the telemetry server is on by default (port 8080, TLS with a self-signed certificate, password authentication with the switch's user accounts, mgmt VRF); `show ip telemetry` on the switch confirms it. For SNMP, create a read-only community, for example `snmp-server community srnetro group ro` in `configure terminal`. To test gNMI from the controller: `gnmic -a <switch>:8080 -u admin -p '<password>' --skip-verify capabilities`.

Hosts run `lldpd`, so each port's switch neighbour and port id show in `srnet hosts` and the UI without a cable map. The switch's own LLDP table (polled over gNMI once a minute, keyed by host name and netdev) wins over what a host reports; a host's report is only used when it names a real interface of a configured switch, matched against the switch's port table by name, alias or description (SONiC sends the alias as the port id and the description an operator typed), and never when the neighbour is the host itself, as with the twin PCIe function of a Spark port. Switch port descriptions show next to each port on the Topology page. The Topology page draws the result: the switch with the ports in use, every host with its ports, CPU, load and memory, lines coloured by fabric and sized by the live rate, and the legs of a run in progress highlighted. Below it, the switch ports in use with their in/out rate, discards and errors (gNMI, or SNMP when that is all there is), and the hosts with their switch ports. `GET /api/v1/topology` returns the same data. Hosts installed before v0.1.18 need `install.sh` run once more: `lldpctl` is executable by group adm only, and the agent service now runs with that supplementary group (the earlier sudo rule never worked, because the service's capability bounding set leaves sudo unable to change group).

Each finished leg also records the switch's view of it and the hosts' CPU. From LLDP the leg knows which switch ports it crossed ("Switch path" on the run page, `switch_client_port` and `switch_server_port` in the CSV). From the switch's own octet counters, sampled every 30 s by gNMI or SNMP, it gets the rate on the server-side port during the test (`switch_gbps`), a third measurement next to the tool and the NIC counters, plus any discards, errors or FCS errors that grew on either port (`switch_deltas`, shown with the error counters). `cpu_*` columns give the average and peak CPU use of the client and the server during the leg, so a host that ran out of CPU is visible next to its rate. The Grafana button on a run page opens the Live run dashboard on that run's time range.

## Agent updates from the controller

Releases are mirrored on the controller and pushed to agents one host at a time, with rollback.

```bash
srnet releases --check           # what is mirrored, and the newest tag on GitHub
srnet fetch-release              # mirror the latest release (or: srnet fetch-release v0.4.0)
srnet update --version v0.4.0    # every host; or --host spark-01,spark-02 / --tag client
srnet updates                    # job history
```

Mirrored releases clean themselves up: the newest 3 stay (`release_keep` in `controller.json`; `0` keeps everything), plus any version an agent still runs and the target of a running update job. The prune runs after each mirror or upload and once a day, never during an update job, and each removal is audited (`release.prune`). The Fleet page has a **Remove** button per release (greyed out while an agent runs that version), `srnet remove-release v0.1.9` and `DELETE /api/v1/releases/{version}` do the same.

The Fleet page in the UI does the same. For each host the controller waits until no test uses it, sends an update task, and the agent downloads the binary over its existing TLS session, checks the SHA-256 from the release's `SHA256SUMS`, stages it and exits. The launcher (`/usr/local/bin/srnet-agent-launch`) swaps the new binary in and keeps the old one. The controller waits up to 90 s for the host to report the new version; if it does not come back, the job marks the host failed and the launcher restores the previous binary on the next restart (a new binary that never completes a poll is rolled back). A failed host stops nothing else: the job carries on with the next host and an `update_failed` alert opens.

## Suites and schedules

A suite is an ordered list of runs (a JSON array of run specs, the same fields `srnet run` sends). Schedules run a suite on a 5-field cron expression in the controller's local time.

```bash
cat > nightly.json <<'EOF'
[
  {"name": "fan-out write", "test": "write", "clients": ["770AP"], "servers": ["tag:spark"], "duration_sec": 60},
  {"name": "link matrix", "test": "write", "clients": ["770AP"], "servers": ["tag:spark"], "serial": true, "duration_sec": 20},
  {"name": "latency", "test": "write_lat", "clients": ["770AP"], "servers": ["tag:spark"], "serial": true, "duration_sec": 10, "max_lat_p99_us": 5}
]
EOF
srnet suite create nightly nightly.json
srnet suite run nightly
srnet schedule add nightly --suite nightly --cron "0 2 * * *"
srnet suite-runs
```

In the UI, open any finished run and click "Add to a suite" to copy its spec into the suite editor.

## Alerts and reports

The controller opens an alert when an agent stops reporting for 2 minutes, a port with an address leaves ACTIVE (a port with no IP is taken to be unused), error counters grow while no test runs on that port (growth during a run belongs to that run's result and does not alert; an idle-error alert closes by itself after 15 minutes without further growth), a transceiver reports 70 C or more (`transceiver_max_c` in `controller.json`), a run or suite fails, or an update fails. Alerts close themselves when the condition clears; the Alerts page has "Clear" per alert and "Clear all" for the ones that do not (a failed update, a failed run), and `srnet alerts --clear ID` / `--clear-all` do the same. `srnet alerts` and the Alerts page list them; set `alert_webhook_url` in `/etc/srnet/controller.json` to POST a JSON body (`state`, `kind`, `host`, `port`, `message`, `text`) to Slack, Teams, Alertmanager or a script on every open and resolve.

Every run has a CSV and an HTML report (print to PDF from the browser): `srnet report 42 --format csv -o run42.csv`, `srnet report 42 --format html -o run42.html`, or the links on the run page.

### Deleting runs

Runs can be deleted to clean up: tick rows on the Runs page and **Delete selected**, or **Clean up** to pick failed and canceled runs (and runs with a fail verdict) or runs older than N days, with a preview of how many match; the run page has a **Delete** button. Each confirms with the run names and offers to also delete the run's per-second samples from VictoriaMetrics (only the series stamped with that run id; idle telemetry of the same ports stays). Queued and running runs are skipped, a baseline needs the "even if a baseline" tick, legs go with the run, suite runs drop the id from their list, and every deletion is audited with the run's name, status and verdict. From the shell: `srnet delete 1 4`, `srnet delete --failed`, `srnet delete --older-than 30d --samples --yes`; the API is `DELETE /api/v1/runs/{id}?force=1&samples=1` and `POST /api/v1/runs/delete {ids | failed, older_than_days, force, samples}`.

The Fabric check only grades cables in use: a port with an IP, a label or a fabric, that is ACTIVE if it reports a state, and not the twin PCIe function that shares a switch port with a configured port on the same host (a DGX Spark's second half). Open `fabric_mismatch` alerts for ports that leave that set close on the next check.

## Sign-in, roles and API tokens

The admin password always works (HTTP Basic, or the browser prompt). Two more ways in come with Phase 7, both on the Settings page:

- **Single sign-on** over OpenID Connect (Okta, Entra ID, Keycloak, Google, Authentik, any provider with a discovery document). Fill in the issuer URL, client ID and secret, register the redirect URL the page shows at the provider, and list who is an **admin** (changes anything) and who is a **viewer** (reads everything) by email address or group name from the provider's claims, or let anyone the provider signs in be a viewer. Sign-in uses the authorization code flow with PKCE; the ID token's signature (RS256/ES256 families), issuer, audience, expiry and nonce are checked against the provider's keys. Sessions last 12 hours by default and are listed on the Settings page, where an admin can sign anyone out. With single sign-on on, `/` goes to the sign-in page; `/?basic` still asks for the password. "Test provider" checks discovery before you save.
- **API tokens** for scripts, CI and the CLI: `Settings > New token` (or `srnet api-token create ci`) prints a `srnet_...` token once. Send it as `Authorization: Bearer <token>`; a viewer token can only GET. The CLI takes it from `SRNET_TOKEN` instead of the password.

Every endpoint under `/api/v1` is plain JSON. The ones a script usually needs:

| Method and path | What it does |
|---|---|
| `GET /api/v1/status` | version, counts, who you are |
| `GET /api/v1/hosts`, `PATCH /api/v1/hosts/{id}`, `POST /api/v1/hosts/{id}/approve` | fleet, naming, approval |
| `POST /api/v1/runs` (a run spec), `GET /api/v1/runs`, `GET /api/v1/runs/{id}`, `POST /api/v1/runs/{id}/cancel` | tests |
| `GET /api/v1/runs/{id}/report.html`, `.csv` | run reports |
| `GET /api/v1/suites`, `POST /api/v1/suites/{id}/run`, `GET /api/v1/suite-runs`, `GET /api/v1/suite-runs/{id}/report.html` | suites and the acceptance report |
| `GET /api/v1/fabric-check`, `GET /api/v1/alerts` | fabric state, open alerts |
| `GET /api/v1/backup`, `POST /api/v1/restore` | configuration backup |
| `GET /api/v1/access/tokens`, `POST`, `DELETE .../{id}` | API tokens |

```bash
export SRNET_URL=https://10.0.0.5:8443 SRNET_CA=ca.pem SRNET_TOKEN=srnet_...
curl -sS --cacert ca.pem -H "Authorization: Bearer $SRNET_TOKEN" "$SRNET_URL/api/v1/runs?limit=5"
curl -sS --cacert ca.pem -H "Authorization: Bearer $SRNET_TOKEN" -X POST "$SRNET_URL/api/v1/runs" \
  -d '{"name":"ci write","test":"write","clients":["770AP"],"servers":["tag:spark"],"duration_sec":30}'
```

## Notifications (Slack, Teams, webhooks)

`Settings > Notifications` sends alerts and results where people look. A channel is a Slack incoming webhook, a Microsoft Teams Workflows webhook ("post to a channel when a webhook request is received", delivered as an Adaptive Card) or a plain JSON webhook (`event`, `level`, `title`, `text`, `link`, `fields`, `data`, `time`; signed with HMAC-SHA256 in `X-SRNet-Signature` when the channel has a secret). Each channel picks its events: alert opened or resolved, run passed, warned or failed, suite run finished, fleet update finished, host waiting for approval. Delivery is tried three times; the page shows each channel's last result, and "Send test" posts a sample. The older `alert_webhook_url` in `controller.json` keeps working for alerts.

## Backup and restore

`Settings > Download backup`, `GET /api/v1/backup` or `srnet backup` writes one JSON file with everything an operator typed in: host names, tags, sites, port labels and fabrics, fabric profiles, suites, schedules, switch settings, sign-in, notification, enrolment and update settings, and the hashes of API tokens (the tokens keep working after a restore). Results and telemetry are not in it: `pg_dump srnet` and the VictoriaMetrics snapshot API cover those; `?runs=1` adds the last 1000 runs for reference only. The controller writes a backup into `/var/lib/srnet/backups` every night and keeps 14. `Restore...` on the page, `POST /api/v1/restore` or `srnet restore file.json` merges a backup on top of what exists, matching hosts by agent UID and ports by device and port number, and deletes nothing. The same file moves a configuration to a new controller: deploy it, let the hosts enrol, restore.

## Acceptance report

Every suite run has a printable report (`Suites > Report`, `GET /api/v1/suite-runs/{id}/report.html`, `srnet suite-report 4 -o report.html`): the suite's runs with their verdicts and aggregates against baselines, each run's leg table, the fabric check at the time, the hosts under test with agent versions, and sign-off lines. It is the document to attach to a hand-over or send to a vendor.

## Enrolment without a token

For hundreds of hosts one fixed command fits cloud-init, Ansible or a PXE post-script better than tokens. `Settings > Enrolment` lists subnets whose hosts may enrol without a token; they are approved at once or wait on the Hosts page (`pending` badge, Approve or Reject; `srnet approve <id>`; `host_pending` notification) and no test runs on an unapproved host. The install command then has no `--token`:

```bash
curl -fsSL https://raw.githubusercontent.com/Realishbird/SR-Net-Speed-Test/main/install.sh \
  | sudo bash -s -- --controller https://10.0.0.5:8443 --ca-sha256 <fingerprint>
```

The Hosts page shows every form of the command under "Agent install command", and a filter box narrows a long fleet by name, tag, site or hostname. Updates run several hosts at a time (`Settings > Fleet updates`, or "At a time" when starting one, up to 20) with the same per-host wait, verification and rollback.

## Air-gapped installs

A lab without internet needs nothing from GitHub at install time:

1. Put a release on the controller. With internet on the controller, `Fleet > Mirror a release`; without, download `SHA256SUMS`, `srnet-agent-linux-amd64` and `srnet-agent-linux-arm64` from the GitHub release elsewhere and use `Fleet > Upload release` (each binary is checked against `SHA256SUMS`).
2. The controller serves `install.sh` itself and the mirrored binaries at `/dl/<version>/<file>` (an install token, an admin login or an address in an enrolment subnet opens them). The "Air-gapped" tab of the Hosts page's "Agent install command" gives it:

```bash
curl -fsSLk https://10.0.0.5:8443/install.sh \
  | sudo bash -s -- --controller https://10.0.0.5:8443 --token <token> --ca-sha256 <fingerprint> --source controller
```

`install.sh` pins the controller's CA by fingerprint before it downloads anything, so the `-k` on the first fetch of the script is the only unverified step and the fingerprint catches a wrong controller. `--source auto` (the default) tries the controller first and falls back to GitHub; `--source github` is the old behaviour. The packages (`rdma-core`, `perftest`, `iperf3`, `ethtool`, `lldpd`, `chrony`) still come from apt: point the hosts at a local mirror, or pass `--skip-apt` on hosts that already have them (DGX OS and most RDMA images do). Agent updates from the Fleet page never needed internet on the hosts.

## Several switches

Each switch on the Switches page is polled on its own; a port's switch comes from the host's LLDP neighbour, so a host cabled to any configured switch is placed. Cables between configured switches (leaf and spine, or two leaves joined) are read from the switches' own LLDP tables and drawn as arcs above the switch row on the Topology page; the "Switch ports in use" table names the peer switch for such ports. Topology draws every host up to 40 and above that only hosts in a test or carrying traffic, with a selector for all of them.

## Development

```bash
go build ./... && go test ./...
go run ./cmd/srnet-agent inventory      # on an RDMA host: what the agent would report
go run ./cmd/srnet-agent sample
```

Every merge to `main` publishes the next patch release automatically (`v0.1.0` becomes `v0.1.1`): amd64 and arm64 binaries for all three commands plus `SHA256SUMS`, which the install scripts and the controller's update mirror download. Push a `vX.Y.0` tag by hand for a minor or major bump. After a release, `create-lxc.sh --upgrade <CTID>` updates the controller, and `srnet fetch-release` then `srnet update` roll the agents.
