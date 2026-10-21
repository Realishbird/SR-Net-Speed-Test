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

Defaults: unprivileged Debian 12 container, 4 cores, 8 GB RAM, 100 GB disk, DHCP on `vmbr0`, no Docker. Every default is a flag (`--ctid`, `--storage`, `--bridge`, `--vlan`, `--ip`, `--cores`, `--memory`, `--disk`, `--password`). The script prints the admin password, the Grafana URL and the CA fingerprint when it finishes. `--upgrade <CTID>` upgrades the stack inside an existing container.

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
srnet run --mesh tag:spark                                                         # full mesh, every ordered pair
srnet run --clients "Dell 770AP" --servers tag:server --serial                     # link matrix, one leg at a time
srnet run --sizes 4096,65536,1048576 --clients "Dell 770AP" --servers tag:server  # message size sweep
srnet run --test write_lat --legs "Dell 770AP>Dell Spark 1" --max-p99-us 5        # latency, fail above 5 µs p99
srnet run --test tcp --streams 8 --clients "Dell 770AP" --servers "Dell Spark 1"  # iperf3 TCP
srnet run --test udp --max-loss-pct 0.5 --clients "Dell 770AP" --servers "Dell Spark 1"
srnet runs; srnet show 12; srnet watch 12; srnet cancel 12
```

| Test | Tool | Result per leg |
| --- | --- | --- |
| `write`, `read`, `send` | `ib_write_bw`, `ib_read_bw`, `ib_send_bw` (`--bidir` for both directions; every rate and the expected rate then add both directions, so a 100G link expects 200) | Gbps from the tool and from the hardware counters, error counter growth |
| `write_lat`, `read_lat`, `send_lat` | `ib_*_lat` | min, typical, avg, p99, p99.9, max in µs |
| `tcp` | `iperf3` (`--streams N`) | Gbps, retransmits |
| `udp` | `iperf3 -u` | Gbps, loss %, jitter |

For each leg the controller picks the client and server ports that share a subnet, checks both are `ACTIVE`, starts every server, then starts every client at one shared time so aggregate load is simultaneous. Verdicts: `fail` below `--pass-pct` (default 90 %) of the expected line rate, above `--max-p99-us`, or above `--max-loss-pct`; `warn` when error counters such as `out_of_sequence` grew or TCP retransmitted.

The API is the same thing the CLI uses (`/api/v1/...`, basic auth with the admin account); the web UI in Phase 2 will sit on top of it.

## Telemetry

Agents sample every second during a run and every 15 s otherwise, and the controller writes everything to VictoriaMetrics as `srnet_*` metrics labelled by host, RDMA device, netdev, port label, fabric and run id:

- RDMA port counters (`port_xmit_data`, `port_rcv_data`, errors, discards) and every `hw_counters` entry (out of sequence, retries, CNP, ECN)
- NIC drop, pause, FEC and error counters from `ethtool -S`, link speed and carrier flaps
- Transceiver DOM from `ethtool -m`: temperature, voltage, per-channel TX bias and optical power; power is reported as `dom_power_w` when the module measures it and `dom_power_rated_max_w` when only a rating is available
- CPU, memory, load and every `hwmon` temperature

Grafana is provisioned with VictoriaMetrics as its data source and three dashboards: **Live run** (pick a run id), **Link health** (error counters, optics and flaps per host over time) and **Fleet health**.

## Switch telemetry (Enterprise SONiC)

Open **Switches** in the web UI, click **Add switch**, and enter the management IP plus a read-only SNMP community, a gNMI user and password, or both. Saving tests SNMP straight away; gNMI data shows within a minute. The same thing from the shell:

```bash
srnet-controller switch --name z9864f --address 10.0.0.2 --snmp-community public --gnmi-user admin --gnmi-password secret
systemctl restart srnet-controller
```

With gNMI credentials the controller writes `/etc/srnet/gnmic.yaml` (a path unit restarts `gnmic` when it changes) and `gnmic` streams OpenConfig interface counters every 30 s and transceiver state every 60 s as `srnet_gnmi_*` metrics (the fastest rates Enterprise SONiC 4.5 accepts; the switch logs `Invalid Sample Interval` for anything shorter). With only an SNMP community the controller polls IF-MIB every 30 s as `srnet_switch_*` metrics (octets, errors, discards, oper status, derived Gbps). Both can be set; gNMI is preferred. The switch is only ever read.

On Enterprise SONiC the telemetry server is on by default (port 8080, TLS with a self-signed certificate, password authentication with the switch's user accounts, mgmt VRF); `show ip telemetry` on the switch confirms it. For SNMP, create a read-only community, for example `snmp-server community srnetro group ro` in `configure terminal`. To test gNMI from the controller: `gnmic -a <switch>:8080 -u admin -p '<password>' --skip-verify capabilities`.

Hosts run `lldpd`, so each port's switch neighbour and port id show in `srnet hosts` and the UI without a cable map. The Topology page draws the result: the switch with the ports in use, every host with its ports, CPU, load and memory, lines coloured by fabric and sized by the live rate, and the legs of a run in progress highlighted. Below it, the switch ports in use with their in/out rate, discards and errors (gNMI, or SNMP when that is all there is), and the hosts with their switch ports. `GET /api/v1/topology` returns the same data. Hosts installed before v0.1.18 need `install.sh` run once more: `lldpctl` is executable by group adm only, and the agent service now runs with that supplementary group (the earlier sudo rule never worked, because the service's capability bounding set leaves sudo unable to change group).

Each finished leg also records the switch's view of it and the hosts' CPU. From LLDP the leg knows which switch ports it crossed ("Switch path" on the run page, `switch_client_port` and `switch_server_port` in the CSV). From the switch's own octet counters, sampled every 30 s by gNMI or SNMP, it gets the rate on the server-side port during the test (`switch_gbps`), a third measurement next to the tool and the NIC counters, plus any discards, errors or FCS errors that grew on either port (`switch_deltas`, shown with the error counters). `cpu_*` columns give the average and peak CPU use of the client and the server during the leg, so a host that ran out of CPU is visible next to its rate. The Grafana button on a run page opens the Live run dashboard on that run's time range.

## Agent updates from the controller

Releases are mirrored on the controller and pushed to agents one host at a time, with rollback.

```bash
srnet releases --check           # what is mirrored, and the newest tag on GitHub
srnet fetch-release              # mirror the latest release (or: srnet fetch-release v0.4.0)
srnet update --version v0.4.0    # every host; or --host spark-01,spark-02 / --tag client
srnet updates                    # job history
```

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

## Development

```bash
go build ./... && go test ./...
go run ./cmd/srnet-agent inventory      # on an RDMA host: what the agent would report
go run ./cmd/srnet-agent sample
```

Every merge to `main` publishes the next patch release automatically (`v0.1.0` becomes `v0.1.1`): amd64 and arm64 binaries for all three commands plus `SHA256SUMS`, which the install scripts and the controller's update mirror download. Push a `vX.Y.0` tag by hand for a minor or major bump. After a release, `create-lxc.sh --upgrade <CTID>` updates the controller, and `srnet fetch-release` then `srnet update` roll the agents.
