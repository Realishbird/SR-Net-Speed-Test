#!/usr/bin/env python3
"""Generates switch.json, the "SR-Net: Switch" Grafana dashboard.

    python3 -I internal/controller/grafana/gen_switch.py internal/controller/grafana/switch.json

Edit this file, not switch.json. Every query is plain MetricsQL against the
series the controller and gnmic write; TestSwitchDashboardJSON checks the
syntax rules that bit us before (keep_metric_names goes after the function
call's closing parenthesis, never inside it).

Label names are the ones gnmic writes on Enterprise SONiC 4.5: a list key
becomes <list>_<key>, so interface counters carry interface_name, QoS
series interface_interface_id and queue_name ("Ethernet0:3"), priority
groups priority_group_name, PFC pfc_priority_dot1p and pfc_queue_queue,
transceivers component_name. String leaves (FEC mode, speed) become their
own series with the value as a label of the same name.
"""
import json
import sys

DS = {"type": "prometheus", "uid": "srnet-vm"}
SW = 'source=~"(?i)$switch"'      # gNMI series (gnmic lower-cases the target name)
SN = 'switch=~"(?i)$switch"'      # SNMP and controller series
# Host behind each switch port, from the controller's LLDP and VLAN tables,
# as the controller saw it at the end of the time range: a port relabelled
# half way (eno16595np0 to NIC-1) keeps one line under its current name.
PT = 'tlast_over_time(srnet_switch_port_info{' + SN + '}[15m] @ end())'
PI = ('max by (interface_name, host, host_port, fabric) (label_replace(sgn(%s == on(switch, port) group_left() max by (switch, port) (%s)),'
      ' "interface_name", "$1", "port", "(.*)"))' % (PT, PT))
PM = "suffix: /min"  # drops and errors come a few a minute; per second read as "16.7 mp/s"
y = [0]
panels = []


def grid(w, h):
    g = {"x": grid.x, "y": y[0], "w": w, "h": h}
    grid.x += w
    if grid.x >= 24:
        grid.x = 0
        y[0] += h
    return g


grid.x = 0


def row(title):
    if grid.x:
        grid.x = 0
        y[0] += 8
    panels.append({"type": "row", "title": title, "collapsed": False, "gridPos": {"x": 0, "y": y[0], "w": 24, "h": 1}, "panels": []})
    y[0] += 1


def steps(thresholds, base="green"):
    return {"mode": "absolute", "steps": [{"color": base, "value": None}] + [{"color": c, "value": v} for v, c in thresholds]}


def ts(title, targets, unit="", w=12, h=8, desc="", stack=False, thresholds=None, no_value=None):
    p = {"type": "timeseries", "title": title, "description": desc, "datasource": DS, "gridPos": grid(w, h),
         "fieldConfig": {"defaults": {"unit": unit, "custom": {"lineWidth": 1, "fillOpacity": 8, "showPoints": "never", "spanNulls": 60000}}, "overrides": []},
         "options": {"legend": {"displayMode": "list", "placement": "bottom", "calcs": ["max", "lastNotNull"]}, "tooltip": {"mode": "multi", "sort": "desc"}},
         "targets": [dict(refId=chr(65 + i), expr=e, legendFormat=l) for i, (e, l) in enumerate(targets)]}
    if stack:
        p["fieldConfig"]["defaults"]["custom"]["stacking"] = {"mode": "normal"}
    if thresholds:
        p["fieldConfig"]["defaults"]["thresholds"] = steps(thresholds)
        p["fieldConfig"]["defaults"]["custom"]["thresholdsStyle"] = {"mode": "line"}
    if no_value:
        p["fieldConfig"]["defaults"]["noValue"] = no_value
    panels.append(p)


def stat(title, expr, unit="", w=4, h=4, desc="", thresholds=None, decimals=None, no_value=None, base=None):
    # Without explicit thresholds Grafana colours a stat red above 80, which
    # made "8 weeks" of uptime and 9102 series look like alarms.
    base = base or ("green" if thresholds else "text")
    p = {"type": "stat", "title": title, "description": desc, "datasource": DS, "gridPos": grid(w, h),
         "fieldConfig": {"defaults": {"unit": unit, "thresholds": steps(thresholds or [], base=base)}, "overrides": []},
         "options": {"reduceOptions": {"calcs": ["lastNotNull"]}, "graphMode": "none" if thresholds else "area", "colorMode": "value"},
         "targets": [{"refId": "A", "expr": expr}]}
    if decimals is not None:
        p["fieldConfig"]["defaults"]["decimals"] = decimals
    if no_value:
        p["fieldConfig"]["defaults"]["noValue"] = no_value
    panels.append(p)


def table(title, targets, w=12, h=8, desc="", hide=(), rename=None, merge=False, matrix=None, mappings=None):
    """targets: list of instant expressions. With merge, their rows join on
    the labels they share (one row per port); with matrix=(row, column) one
    label becomes the rows and another the columns."""
    exclude = {k: True for k in ("Time", "__name__", "source", "subscription_name", "job", "instance") + tuple(hide)}
    tr = []
    if merge:
        tr.append({"id": "merge", "options": {}})
        for i in range(len(targets)):
            exclude["Value #" + chr(65 + i)] = True
    if matrix:
        tr.append({"id": "groupingToMatrix", "options": {"rowField": matrix[0], "columnField": matrix[1], "valueField": "Value", "emptyValue": "empty"}})
    tr.append({"id": "organize", "options": {"excludeByName": exclude, "renameByName": rename or {}}})
    defaults = {}
    if mappings:
        defaults["mappings"] = mappings
        defaults["custom"] = {"cellOptions": {"type": "color-text"}}
        defaults["color"] = {"mode": "thresholds"}
        defaults["thresholds"] = steps([], base="text")
    panels.append({"type": "table", "title": title, "description": desc, "datasource": DS, "gridPos": grid(w, h),
                   "fieldConfig": {"defaults": defaults, "overrides": []}, "options": {"showHeader": True},
                   "targets": [{"refId": chr(65 + i), "expr": e, "format": "table", "instant": True} for i, e in enumerate(targets)],
                   "transformations": tr})


def sel(name_re, port_label="interface_name"):
    return '{__name__=~"%s",%s,%s=~"$port"}' % (name_re, SW, port_label)


def rate(name_re, port_label="interface_name", win="[2m]"):
    # keep_metric_names after the call: inside the parentheses it is a
    # syntax error, and without it two counters of one port collide.
    return 'rate(%s%s) keep_metric_names' % (sel(name_re, port_label), win)


def leaf(expr, label, pattern):
    """Copies part of the metric name into a label (before any arithmetic
    drops the name), e.g. the direction or the counter."""
    return 'label_replace(%s, "%s", "$1", "__name__", "%s")' % (expr, label, pattern)


def queue(expr):
    return 'label_replace(%s, "q", "$1", "queue_name", ".*:(.*)")' % expr


def joined(expr):
    # Adds host, host_port and fabric from the port map; ports with no
    # mapped host are kept as they are.
    return '(%s) * on(interface_name) group_left(host, host_port, fabric) %s or (%s) unless on(interface_name) %s' % (expr, PI, expr, PI)


LEG = "{{interface_name}} {{host}} {{host_port}}"
Q = "{{interface_interface_id}} q{{q}}"

# ---------------- Overview
row("Overview")
stat("Ports up", 'count(srnet_switch_up{%s} == 1)' % SN, desc="Ports with link, from the SNMP walk every 30 s.")
stat("Total in", 'sum(%s) * 8 / 1e9' % rate("srnet_gnmi_.*interface_state_counters_in_octets"), "Gbps", desc="Sum of the selected ports' ingress, 2 minute rate.", decimals=1)
stat("Total out", 'sum(%s) * 8 / 1e9' % rate("srnet_gnmi_.*interface_state_counters_out_octets"), "Gbps", decimals=1)
stat("Queue drops /min", 'sum(%s) * 60 or vector(0)' % rate("srnet_gnmi_.*queues.*_dropped_pkts", "interface_interface_id"), PM, desc="Packets dropped per minute in egress queues of the selected ports, over the last 2 minutes. Any value above 0 under load means the fabric is lossy for that traffic.", thresholds=[(0.01, "red")], decimals=1)
stat("PFC pause /min", 'sum(%s) * 60 or vector(0)' % rate("srnet_gnmi_.*pfc.*pause_frames_(rx|tx)", "interface_interface_id"), PM, desc="Priority flow control pause frames received plus sent, per minute. Pauses received while PFC is off on the switch come from hosts set up for lossless RoCE; the switch ignores them.", thresholds=[(0.01, "orange")], decimals=1)
stat("CRC errors /min", 'sum(%s) * 60 or vector(0)' % rate("srnet_gnmi_.*ethernet.*in_crc_errors"), PM, desc="Frames failing the CRC per minute: a bad cable or optic.", thresholds=[(0.01, "red")], decimals=1)
stat("CPU", 'max(srnet_switch_cpu_pct{%s})' % SN, "percent", thresholds=[(70, "orange"), (90, "red")], decimals=0)
stat("Memory", 'max(srnet_switch_mem_used_pct{%s})' % SN, "percent", thresholds=[(80, "orange"), (95, "red")], decimals=0)
stat("Hottest sensor", 'max(srnet_switch_temp_c{%s})' % SN, "celsius", desc="Hottest of the switch's temperature sensors (CPU, ASIC, inlet, outlet and so on).", thresholds=[(60, "orange"), (75, "red")], decimals=0)
stat("Hottest optic", 'max(%s)' % sel("srnet_gnmi_.*component_state_temperature_instant", "component_name"), "celsius", desc="Hottest transceiver among the selected ports (module case temperature).", thresholds=[(65, "orange"), (70, "red")], decimals=0)
stat("Fans / PSUs failed", '(count(srnet_switch_fan_ok{%s} == 0) or vector(0)) + (count(srnet_switch_psu_ok{%s} == 0) or vector(0))' % (SN, SN), "", thresholds=[(1, "red")], decimals=0)
stat("Uptime", 'max(srnet_switch_uptime_seconds{%s})' % SN, "s", decimals=0, desc="Time since the switch last booted. Red only in the first hour after a reboot.", thresholds=[(3600, "text")], base="red")

# ---------------- Throughput
row("Throughput")
ts("Ingress Gbps per port (gNMI, 30 s)", [(joined(rate("srnet_gnmi_.*interface_state_counters_in_octets") + " * 8 / 1e9"), LEG)], "Gbps", desc="Bytes received by the switch on each port. gNMI samples every 30 s (the fastest Enterprise SONiC accepts), so short bursts are averaged.")
ts("Egress Gbps per port (gNMI, 30 s)", [(joined(rate("srnet_gnmi_.*interface_state_counters_out_octets") + " * 8 / 1e9"), LEG)], "Gbps")
ts("Port Gbps during runs (SNMP every 2 s, 20 s average)", [('srnet_switch_rx_gbps{%s,port=~"$port"} > 0.05' % SN, "{{port}} in"), ('srnet_switch_tx_gbps{%s,port=~"$port"} > 0.05' % SN, "{{port}} out")], "Gbps", desc="While a run is active the controller polls the ports that are up every 2 s over SNMP; between runs every 30 s. The switch refreshes its SNMP counters only every 3 to 6 s and in uneven steps, so each point is a line fitted through the last 20 s of counter changes; the first point of a run comes about 12 s in. Idle ports are left out.", no_value="idle")
ts("Utilisation (% of line rate)", [('clamp_max(%s * 8 / 1e6 / on(interface_name) group_left() max by (interface_name) (label_replace(srnet_switch_speed_mbps{%s}, "interface_name", "$1", "port", "(.*)")) * 100, 100) > 0.1' % (leaf(rate("srnet_gnmi_.*interface_state_counters_(in|out)_octets"), "dir", ".*_counters_(in|out)_octets"), SN), "{{interface_name}} {{dir}}")], "percent", desc="Ingress and egress rate against the port's configured speed (ifHighSpeed, from SNMP). A 100G port carrying 98 Gbps is at 98%. Capped at 100: a port at line rate can read a point or two over it because the 30 s samples do not land exactly 30 s apart. Idle ports are left out.", thresholds=[(95, "orange")], no_value="idle")
ts("Packets per second", [(leaf(rate("srnet_gnmi_.*interface_state_counters_in_(unicast|multicast|broadcast)_pkts"), "kind", ".*_counters_in_(.*)_pkts"), "{{interface_name}} {{kind}}")], "pps")
ts("Average frame size (bytes)", [('sum by (interface_name) (%s) / sum by (interface_name) (%s) > 0' % (rate("srnet_gnmi_.*interface_state_counters_in_octets"), rate("srnet_gnmi_.*interface_state_counters_in_pkts")), "{{interface_name}}")], "bytes", desc="Octets divided by packets received. RDMA at 4096 MTU shows about 4 kB; small values mean control traffic or fragmentation.", no_value="idle")

# ---------------- Congestion
row("Congestion and buffers")
ts("Queue drops per minute", [(queue(leaf(rate("srnet_gnmi_.*queues.*_dropped_pkts", "interface_interface_id"), "kind", ".*_state_(.*)")) + " * 60 > 0", Q + " {{kind}}")], PM, desc="Packets dropped per egress queue per minute (tail drops and WRED drops); queues that dropped nothing are left out. The 400G to 100G step from the 770AP into the Sparks shows here as queue 0 drops on the Spark ports.", thresholds=[(0.01, "red")], no_value="no drops")
ts("ECN marked per second", [(queue(rate("srnet_gnmi_.*queues.*ecn_marked_pkts", "interface_interface_id")) + " > 0", Q)], "pps", desc="Packets ECN-marked by WRED; unmarked queues are left out. Nothing here under congestion means ECN is not configured on the queue, so RoCE congestion control never kicks in and the NIC retransmits after drops instead.", no_value="no ECN marks")
ts("Transmit packets per queue", [(queue(rate("srnet_gnmi_.*queues.*_transmit_pkts", "interface_interface_id")) + " > 1", Q)], "pps", desc="Which queue carries the traffic (idle queues left out). RoCE should sit in the lossless queue (3 on a classic profile); everything in queue 0 means no DSCP-to-queue map is applied.", stack=True, no_value="idle")
ts("Queue watermark (% of buffer)", [('max by (interface_interface_id, q) (%s) > 0' % queue(sel("srnet_gnmi_.*queues.*periodic_watermark_percent", "interface_interface_id")), Q)], "percent", desc="Peak egress queue occupancy in the last sample window, as a share of the shared buffer; queues at 0 are left out. Sustained values near 100 precede drops.", thresholds=[(80, "orange"), (95, "red")], no_value="0%")
ts("Queue watermark (bytes)", [('max by (interface_interface_id, q) (%s) > 0' % queue(sel("srnet_gnmi_.*queues.*state_watermark$", "interface_interface_id")), Q)], "bytes", desc="Peak egress queue occupancy in bytes since the counter was last read (queues at 0 left out).", no_value="0")
ts("Ingress priority-group watermark (%)", [('max by (interface_interface_id, priority_group_name, kind) (%s) > 0' % leaf(sel("srnet_gnmi_.*priority_groups.*periodic_watermark_percent", "interface_interface_id"), "kind", ".*_state_(.*)_periodic_watermark_percent"), "{{priority_group_name}} {{kind}}")], "percent", desc="Shared and headroom buffer use per ingress priority group (groups at 0 left out). Headroom use above 0 means PFC was asserted and the buffer absorbed in-flight data.", no_value="0%")

# ---------------- PFC
row("Priority flow control")
ts("PFC pause frames received per minute", [(rate("srnet_gnmi_.*pfc.*pause_frames_rx", "interface_interface_id") + " * 60 > 0", "{{interface_interface_id}} prio {{pfc_priority_dot1p}}")], PM, desc="Pause frames the switch received from hosts (the host asked the switch to stop), per priority. If PFC is off for that priority on the switch (table below), the switch ignores them: the host is set up for lossless RoCE and the switch is not.", no_value="no pause frames")
ts("PFC pause frames sent per minute", [(rate("srnet_gnmi_.*pfc.*pause_frames_tx", "interface_interface_id") + " * 60 > 0", "{{interface_interface_id}} prio {{pfc_priority_dot1p}}")], PM, desc="Pause frames the switch sent to hosts (the switch asked the host to stop), per priority.", no_value="no pause frames")
ONOFF = [{"type": "value", "options": {"1": {"text": "on", "color": "green"}, "0": {"text": "off", "color": "text"}}}]
table("PFC enabled per priority", ['max by (interface_interface_id, pfc_priority_dot1p) (last_over_time(%s[5m]))' % sel("srnet_gnmi_.*pfc.*priority.*_enable", "interface_interface_id")],
      desc="One row per port, one column per priority: on or off. RoCE needs PFC on for its priority (3 on a classic profile) on every port it crosses.",
      matrix=("interface_interface_id", "pfc_priority_dot1p"), mappings=ONOFF, rename={"interface_interface_id\\pfc_priority_dot1p": "port \\ priority"})
table("PFC watchdog", ['max by (where, counter) (label_join(last_over_time(%s[5m]), "where", " queue ", "interface_interface_id", "pfc_queue_queue"))' % leaf(sel("srnet_gnmi_.*pfc.*queue.*(storm|watchdog|_ok|_drop).*", "interface_interface_id"), "counter", ".*_state_(?:statistics_)?(.*)")],
      desc="PFC watchdog counters, one row per port and queue. storm_detected above 0 means a port was stuck sending pause and the watchdog cut it off.",
      matrix=("where", "counter"), rename={"where\\counter": "port and queue"})

# ---------------- Errors
row("Errors and link quality")
ts("CRC errors per minute", [(joined(rate("srnet_gnmi_.*ethernet.*in_crc_errors") + " * 60"), LEG)], PM, desc="Any CRC error at steady rate points at the cable or the optic on that port. Compare with the host's symbol errors on the same leg.", thresholds=[(0.01, "red")])
ts("Malformed frames per minute", [(leaf(rate("srnet_gnmi_.*ethernet.*_(in|out)_(fragment|jabber|oversize|undersize)_frames"), "kind", ".*_((?:in|out)_[a-z]+_frames)") + " * 60 > 0", "{{interface_name}} {{kind}}")], PM, desc="Fragments and undersize frames: a bad link. Oversize and jabber: an MTU mismatch (a 9000 byte frame into a 1500 byte port). Ports without any are left out.", no_value="none")
ts("Discards and errors per minute (SNMP)", [('rate(srnet_switch_in_discards{%s,port=~"$port"}[2m]) * 60 > 0' % SN, "{{port}} in discards"), ('rate(srnet_switch_out_discards{%s,port=~"$port"}[2m]) * 60 > 0' % SN, "{{port}} out discards"), ('rate(srnet_switch_in_errors{%s,port=~"$port"}[2m]) * 60 > 0' % SN, "{{port}} in errors"), ('rate(srnet_switch_out_errors{%s,port=~"$port"}[2m]) * 60 > 0' % SN, "{{port}} out errors")], PM, desc="Interface-level discards (buffer full) and errors from IF-MIB, per minute; ports without any are left out. Out discards on a 100G port fed from a 400G port are the speed-mismatch drops.", no_value="none")
table("FEC and link state", ['max by (interface_name, %s) (last_over_time(%s[5m]))' % (l, sel("srnet_gnmi_.*ethernet.*state.*_" + l + "$")) for l in ("oper_fec", "port_fec", "port_speed")], merge=True,
      desc="Per port: the FEC in use (RS-FEC is required at 100G and above over copper and most optics) and the configured speed. This SONiC build does not expose FEC corrected or uncorrected codeword counters, so pre-FEC bit error rate is not available; CRC errors are the post-FEC signal.",
      rename={"interface_name": "port", "oper_fec": "FEC in use", "port_fec": "FEC configured", "port_speed": "speed"})
ts("Ingress frame sizes", [(leaf(rate("srnet_gnmi_.*ethernet.*counters_in_distribution_in_frames_.*_octets"), "size", ".*_in_frames_(.*)_octets") + " > 0", "{{interface_name}} {{size}}")], "pps", desc="Histogram of received frame sizes as rates (empty buckets left out). RDMA traffic at MTU 4096 lands in the 2048 to 4095 bucket; 9000 byte MTU in 8192 and up.", stack=True, no_value="idle")
ts("Egress frame sizes", [(leaf(rate("srnet_gnmi_.*ethernet.*counters_out_distribution_out_frames_.*_octets"), "size", ".*_out_frames_(.*)_octets") + " > 0", "{{interface_name}} {{size}}")], "pps", stack=True, no_value="idle")

# ---------------- Optics
row("Optics")
ts("Transceiver temperature", [(sel("srnet_gnmi_.*component_state_temperature_instant", "component_name"), "{{component_name}}")], "celsius", desc="Module case temperature from the switch side DOM. Most QSFP and OSFP modules alarm at 70 to 75 C.", thresholds=[(65, "orange"), (70, "red")])
# The Z9864F reports module totals under leaves OpenConfig defines per
# channel in dBm: 17 to 25 for an 8 lane module only makes sense as mW
# summed over the lanes, so the panels show the per-lane average in dBm.
LANES = 'on(component_name) group_left() max by (component_name) (%s)' % sel("srnet_gnmi_.*transceiver_state_media_lane_count", "component_name")
ts("Receive power per lane (dBm)", [('10 * log10(%s / %s)' % (sel("srnet_gnmi_.*transceiver_state_input_power_instant", "component_name"), LANES), "{{component_name}}")], "dBm", desc="Average optical receive power per lane, from the module total the switch reports (mW over all media lanes) divided by the lane count. Healthy DR and FR optics sit around -4 to +4 dBm per lane. A drop on one port means a dirty connector or a failing far-end laser.", thresholds=[(-8, "green")])
ts("Transmit power per lane (dBm)", [('10 * log10(%s / %s)' % (sel("srnet_gnmi_.*transceiver_state_output_power_instant", "component_name"), LANES), "{{component_name}}")], "dBm", desc="Average optical transmit power per lane, derived the same way as receive power.")
ts("Laser bias current (module total, as reported)", [(sel("srnet_gnmi_.*transceiver_state_laser_bias_current_instant", "component_name"), "{{component_name}}")], "none", desc="The switch reports one bias figure per module, apparently summed over the lanes, so read it as a trend: rising bias at constant output power is a laser ageing out.")

# ---------------- Health
row("Switch health")
ts("CPU and memory", [('srnet_switch_cpu_pct{%s}' % SN, "CPU %"), ('srnet_switch_mem_used_pct{%s}' % SN, "memory used %")], "percent", desc="Control-plane CPU and memory. Telemetry streaming itself costs a little CPU; a busy CPU does not slow the data plane but delays counters.", thresholds=[(85, "red")])
ts("Temperature sensors", [('srnet_switch_temp_c{%s}' % SN, "{{sensor}}"), ('min(srnet_switch_temp_high_c{%s})' % SN, "high limit")], "celsius", desc="Every TEMP sensor the switch reports (ASIC, inlet, outlet, CPU) against the lowest high-threshold.")
ts("Fan speed", [('srnet_switch_fan_rpm{%s}' % SN, "{{fan}}")], "rotrpm", desc="Every fan the switch reports, in RPM. One fan well below its tray mates is failing; all of them climbing means the switch is running hot.")
ts("Fans and PSUs OK", [('srnet_switch_fan_ok{%s}' % SN, "{{fan}}"), ('srnet_switch_psu_ok{%s}' % SN, "{{psu}}")], "", desc="1 = present and operating, 0 = failed or missing.", h=6)
stat("Uptime", 'max(srnet_switch_uptime_seconds{%s})' % SN, "s", w=6, h=6, decimals=0, thresholds=[(3600, "text")], base="red")
stat("gNMI series per switch", 'count(last_over_time({__name__=~"srnet_gnmi_.+",%s}[2m]))' % SW, "", w=6, h=6, desc="How many distinct series gnmic delivered in the last two minutes. A drop to 0 means the stream died (see the Switches page).", thresholds=[], no_value="0: stream down")

dash = {
    "uid": "srnet-switch",
    "title": "SR-Net: Switch",
    "tags": ["srnet", "switch"],
    "timezone": "browser",
    "schemaVersion": 39,
    "version": 3,
    "refresh": "30s",
    "time": {"from": "now-3h", "to": "now"},
    "editable": True,
    "description": "Everything the controller reads from the switch itself, over gNMI (gnmic streams every 30 s, the fastest Enterprise SONiC accepts) and SNMP (every 30 s, every 2 s on the ports in use while a run is active): throughput, queue drops, ECN, PFC, buffers, errors, FEC, optics and switch health. Open it from a run page with \"Switch view\" to land on the run's time range and ports. Generated by gen_switch.py.",
    "annotations": {"list": [
        {"name": "Runs", "datasource": DS, "enable": True, "iconColor": "rgba(0, 150, 255, 0.8)", "expr": "max by (run_id) (srnet_rdma_rx_gbps{run_id!=\"\"})", "step": "15s", "titleFormat": "Run {{run_id}}", "tagKeys": "run_id", "textFormat": "Test traffic from the controller's run {{run_id}}", "useValueForTime": False}
    ]},
    "templating": {"list": [
        {"name": "switch", "label": "Switch", "type": "query", "datasource": DS, "query": {"query": 'label_values({__name__=~"srnet_gnmi_.*interface_state_counters_in_octets"}, source)', "refId": "v"}, "refresh": 2, "sort": 1, "includeAll": False, "current": {}, "description": "gNMI target name (the switch's name in the controller, lower-cased by gnmic)."},
        {"name": "ports", "label": "Show", "type": "custom", "query": "Ports with a host : srnet_switch_port_info,All ports : srnet_switch_up", "options": [
            {"text": "Ports with a host", "value": "srnet_switch_port_info", "selected": True}, {"text": "All ports", "value": "srnet_switch_up", "selected": False}],
         "current": {"text": "Ports with a host", "value": "srnet_switch_port_info"}, "description": "Ports with a host: the ports the controller has mapped to a host through LLDP. All ports: every port on the switch."},
        {"name": "port", "label": "Ports", "type": "query", "datasource": DS, "query": {"query": 'label_values({__name__="$ports",switch=~"(?i)$switch"}, port)', "refId": "v"}, "refresh": 2, "sort": 7, "multi": True, "includeAll": True, "current": {"text": "All", "value": "$__all"}, "description": "Pick the switch ports to look at; All means every port in the list (by default the ports with a host). The run page's Switch view button pre-selects the run's ports."}
    ]},
    "panels": panels,
}
json.dump(dash, open(sys.argv[1], "w"), indent=1)
print(len(panels), "panels")
