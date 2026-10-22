// Command srnet is the operator CLI for the controller.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/Realishbird/SR-Net-Speed-Test/internal/api"
)

type cli struct {
	url, user, pass string
	http            *http.Client
}

func main() {
	url := envOr("SRNET_URL", "https://127.0.0.1:8443")
	user := envOr("SRNET_USER", "admin")
	pass := os.Getenv("SRNET_PASSWORD")
	ca := envOr("SRNET_CA", "/etc/srnet/tls/ca.pem")
	if pass == "" {
		if b, err := os.ReadFile(envOr("SRNET_PASSWORD_FILE", "/etc/srnet/admin-password")); err == nil {
			pass = strings.TrimSpace(string(b))
		}
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	if pem, err := os.ReadFile(ca); err == nil {
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(pem)
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	c := &cli{url: strings.TrimRight(url, "/"), user: user, pass: pass, http: &http.Client{Transport: tr, Timeout: 60 * time.Second}}

	if len(os.Args) < 2 {
		usage()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	args := os.Args[2:]
	var err error
	switch os.Args[1] {
	case "hosts":
		err = c.hosts(ctx)
	case "host":
		err = c.host(ctx, args)
	case "port":
		err = c.port(ctx, args)
	case "import":
		err = c.importCSV(ctx, args)
	case "token":
		err = c.token(ctx)
	case "run":
		err = c.run(ctx, args)
	case "runs":
		err = c.runs(ctx)
	case "show":
		err = c.show(ctx, args)
	case "watch":
		err = c.watch(ctx, args)
	case "cancel":
		err = c.cancel(ctx, args)
	case "baseline":
		err = c.baseline(ctx, args)
	case "releases":
		err = c.releases(ctx, args)
	case "fetch-release":
		err = c.fetchRelease(ctx, args)
	case "update":
		err = c.update(ctx, args)
	case "updates":
		err = c.updates(ctx)
	case "suite":
		err = c.suite(ctx, args)
	case "suite-runs":
		err = c.suiteRuns(ctx)
	case "schedule":
		err = c.schedule(ctx, args)
	case "alerts":
		err = c.alerts(ctx, args)
	case "report":
		err = c.report(ctx, args)
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: srnet <command> [flags]

  hosts                         list enrolled hosts and their RDMA ports
  host <id> [--name N] [--tags a,b] [--site S] [--rack R]
  port <id> [--label L] [--fabric F] [--expected-gbps G]
  import <file.csv>             bulk-apply names, tags and port labels
  token                         print an agent install command (good for 10 hosts for an hour)
  run [flags]                   start a test (see run --help): fan-out, fan-in, --pairs, --mesh, --legs,
                                --serial (link matrix), --sizes (sweep); tests: write read send
                                write_lat read_lat send_lat tcp udp nccl_allreduce nccl_allgather
                                (collectives take --hosts: every host is one rank, the first is rank 0)
  runs                          list recent runs
  show <run id>                 show a run's result
  watch <run id>                follow a run live until it ends
  cancel <run id>
  baseline <run id> [--off]     mark a finished run as the baseline its shape is compared against
  report <run id> [--format csv|html] [-o file]

  releases [--check]            agent builds mirrored on the controller (and the newest on GitHub)
  fetch-release [version]       mirror a release from GitHub (default: latest)
  update --version vX [--host a,b | --tag t]
                                push an agent update one host at a time, with rollback
  updates                       update job history
  suite list|show|create|update|delete|run
                                suites are ordered lists of runs (JSON array of run specs)
  suite-runs                    suite run history
  schedule list|add|enable|disable|delete
                                run a suite on a cron expression
  alerts [--all|--clear ID|--clear-all]  open alerts (agent offline, link down, hot optics, idle errors, failed runs, failed updates); clear by hand

Environment: SRNET_URL, SRNET_USER, SRNET_PASSWORD or SRNET_PASSWORD_FILE, SRNET_CA`)
	os.Exit(2)
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func (c *cli) call(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	ct := "application/json"
	switch v := in.(type) {
	case nil:
	case []byte:
		body, ct = bytes.NewReader(v), "text/csv"
	default:
		b, _ := json.Marshal(v)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.url+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", ct)
	req.SetBasicAuth(c.user, c.pass)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var e struct{ Error string }
		json.NewDecoder(resp.Body).Decode(&e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return fmt.Errorf("%s", e.Error)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (c *cli) hosts(ctx context.Context) error {
	var hosts []api.Host
	if err := c.call(ctx, "GET", "/api/v1/hosts", nil, &hosts); err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "HOST\tNAME\tONLINE\tTAGS\tPORT\tDEV\tNETDEV\tIP\tSTATE\tSPEED\tLABEL\tFABRIC\tEXPECTED\tSWITCH PORT\tLIVE TX/RX")
	for _, h := range hosts {
		on := "no"
		if h.Online {
			on = "yes"
		}
		if len(h.Ports) == 0 {
			fmt.Fprintf(w, "%d\t%s\t%s\t%s\t-\t-\t-\t-\t-\t-\t-\t-\t-\t-\t-\n", h.ID, h.Name, on, strings.Join(h.Tags, ","))
		}
		for _, p := range h.Ports {
			exp := "-"
			if p.ExpectedGbps > 0 {
				exp = fmt.Sprintf("%.0f", p.ExpectedGbps)
			}
			sw := "-"
			if p.LLDPSwitch != "" || p.LLDPPort != "" {
				sw = p.LLDPSwitch + "/" + p.LLDPPort
			}
			fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%d\t%s/%d\t%s\t%s\t%s\t%dG\t%s\t%s\t%s\t%s\t%.1f/%.1f\n",
				h.ID, h.Name, on, strings.Join(h.Tags, ","), p.ID, p.RdmaDev, p.PortNum, p.Netdev, strings.Join(p.Addrs, ","),
				p.State, p.SpeedMbps/1000, p.Label, p.Fabric, exp, sw, p.TxGbps, p.RxGbps)
		}
	}
	return w.Flush()
}

func (c *cli) host(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("host", flag.ExitOnError)
	name := fs.String("name", "", "display name")
	tags := fs.String("tags", "", "comma-separated tags (replaces existing)")
	site := fs.String("site", "", "site")
	rack := fs.String("rack", "", "rack")
	id, rest := leadingID(args)
	fs.Parse(rest)
	if id == "" {
		return fmt.Errorf("usage: srnet host <id> [flags]")
	}
	var u api.HostUpdate
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "name":
			u.Name = name
		case "tags":
			t := splitList(*tags)
			u.Tags = &t
		case "site":
			u.Site = site
		case "rack":
			u.Rack = rack
		}
	})
	return c.call(ctx, "PATCH", "/api/v1/hosts/"+id, u, nil)
}

func (c *cli) port(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("port", flag.ExitOnError)
	label := fs.String("label", "", "port label, e.g. \"NIC 1\"")
	fabric := fs.String("fabric", "", "fabric name, e.g. 100")
	exp := fs.Float64("expected-gbps", 0, "expected line rate; 0 uses the negotiated speed")
	id, rest := leadingID(args)
	fs.Parse(rest)
	if id == "" {
		return fmt.Errorf("usage: srnet port <id> [flags]")
	}
	var u api.PortUpdate
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "label":
			u.Label = label
		case "fabric":
			u.Fabric = fabric
		case "expected-gbps":
			u.ExpectedGbps = exp
		}
	})
	return c.call(ctx, "PATCH", "/api/v1/ports/"+id, u, nil)
}

func (c *cli) importCSV(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: srnet import <file.csv>")
	}
	b, err := os.ReadFile(args[0])
	if err != nil {
		return err
	}
	var res struct {
		Applied  int
		Problems []string
	}
	if err := c.call(ctx, "POST", "/api/v1/hosts/import", b, &res); err != nil {
		return err
	}
	fmt.Printf("applied %d row(s)\n", res.Applied)
	for _, p := range res.Problems {
		fmt.Println("  ", p)
	}
	return nil
}

func (c *cli) token(ctx context.Context) error {
	var t api.TokenResponse
	if err := c.call(ctx, "POST", "/api/v1/tokens", nil, &t); err != nil {
		return err
	}
	fmt.Printf("Run this on up to %d hosts (token expires %s):\n\n%s\n", t.Hosts, t.ExpiresAt.Local().Format(time.Kitchen), t.InstallCommand)
	return nil
}

func (c *cli) run(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	var spec api.RunSpec
	var clients, servers, legs, mesh, hosts, sizes string
	fs.StringVar(&spec.Name, "name", "", "run name")
	fs.StringVar(&spec.Test, "test", "write", "write, read, send, write_lat, read_lat, send_lat, tcp, udp, nccl_allreduce, nccl_allgather")
	fs.StringVar(&clients, "clients", "", "client selectors, comma-separated: host name, tag:<tag>, RDMA IP or port label")
	fs.StringVar(&servers, "servers", "", "server selectors, comma-separated")
	fs.StringVar(&legs, "legs", "", "explicit legs client>server, comma-separated")
	fs.StringVar(&mesh, "mesh", "", "full mesh among these selectors (every ordered pair)")
	fs.StringVar(&hosts, "hosts", "", "collective (nccl_*): the hosts taking part, one GPU rank each; the first hosts the rendezvous")
	fs.BoolVar(&spec.Pairs, "pairs", false, "pair clients[i] with servers[i]")
	fs.BoolVar(&spec.Serial, "serial", false, "run legs one at a time (link matrix)")
	fs.StringVar(&sizes, "sizes", "", "sweep: repeat every leg at each message size, comma-separated")
	fs.IntVar(&spec.DurationSec, "duration", 60, "seconds")
	fs.IntVar(&spec.MsgSize, "size", 0, "message size in bytes (default per test: 65536 bw, 2 latency, 128K tcp, 8K udp)")
	fs.IntVar(&spec.QPs, "qps", 1, "queue pairs per leg (RDMA bandwidth)")
	fs.IntVar(&spec.Streams, "streams", 1, "parallel streams (iperf3)")
	fs.BoolVar(&spec.Bidir, "bidir", false, "bidirectional (bandwidth tests)")
	fs.IntVar(&spec.TOS, "tos", 106, "IP TOS")
	fs.IntVar(&spec.MTU, "mtu", 4096, "RDMA MTU")
	fs.Float64Var(&spec.PassPct, "pass-pct", 90, "pass threshold as percent of expected line rate")
	fs.Float64Var(&spec.MaxLatP99Us, "max-p99-us", 0, "latency tests: fail when p99 is above this (0 = report only)")
	fs.Float64Var(&spec.MaxLossPct, "max-loss-pct", 1, "udp: fail above this loss percentage")
	fs.Float64Var(&spec.RegressPct, "regress-pct", 5, "warn when a leg is this far behind its baseline, in percent")
	follow := fs.Bool("watch", true, "follow the run until it ends")
	fs.Parse(args)
	spec.Clients, spec.Servers = splitList(clients), splitList(servers)
	if mesh != "" {
		spec.Mesh, spec.Hosts = true, splitList(mesh)
	}
	if hosts != "" {
		spec.Hosts = splitList(hosts)
	}
	for _, sz := range splitList(sizes) {
		n, err := strconv.Atoi(sz)
		if err != nil {
			return fmt.Errorf("size %q is not a number", sz)
		}
		spec.Sizes = append(spec.Sizes, n)
	}
	for _, l := range splitList(legs) {
		cl, sv, ok := strings.Cut(l, ">")
		if !ok {
			return fmt.Errorf("leg %q must look like client>server", l)
		}
		spec.Legs = append(spec.Legs, api.LegSpec{Client: strings.TrimSpace(cl), Server: strings.TrimSpace(sv)})
	}
	var run api.Run
	if err := c.call(ctx, "POST", "/api/v1/runs", spec, &run); err != nil {
		return err
	}
	fmt.Printf("run %d started: %d leg(s)\n", run.ID, len(run.Legs))
	if !*follow {
		return nil
	}
	return c.follow(ctx, run.ID)
}

func (c *cli) runs(ctx context.Context) error {
	var runs []api.Run
	if err := c.call(ctx, "GET", "/api/v1/runs", nil, &runs); err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tNAME\tTEST\tSTATUS\tVERDICT\tTOOL Gbps\tCOUNTER Gbps\tVS BASELINE\tSTARTED")
	for _, r := range runs {
		started := "-"
		if r.StartedAt != nil {
			started = r.StartedAt.Local().Format("2006-01-02 15:04:05")
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%.1f\t%.1f\t%s\t%s\n", r.ID, r.Name, r.Spec.Test, r.Status, r.Verdict, r.Summary.ToolGbps, r.Summary.CounterGbps, baselineCol(r), started)
	}
	return w.Flush()
}

func (c *cli) show(ctx context.Context, args []string) error {
	id, _ := leadingID(args)
	if id == "" {
		return fmt.Errorf("usage: srnet show <run id>")
	}
	var r api.Run
	if err := c.call(ctx, "GET", "/api/v1/runs/"+id, nil, &r); err != nil {
		return err
	}
	printRun(r)
	return nil
}

func (c *cli) watch(ctx context.Context, args []string) error {
	id, _ := leadingID(args)
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return fmt.Errorf("usage: srnet watch <run id>")
	}
	return c.follow(ctx, n)
}

func (c *cli) follow(ctx context.Context, id int64) error {
	path := "/api/v1/runs/" + strconv.FormatInt(id, 10)
	for {
		var r api.Run
		if err := c.call(ctx, "GET", path, nil, &r); err != nil {
			return err
		}
		if r.Status != "queued" && r.Status != "running" {
			fmt.Println()
			printRun(r)
			if r.Verdict == "fail" {
				os.Exit(3)
			}
			return nil
		}
		parts := make([]string, 0, len(r.Legs))
		for _, l := range r.Legs {
			parts = append(parts, fmt.Sprintf("%s>%s %.1f", l.ClientHost, l.ServerHost, l.LiveGbps))
		}
		fmt.Printf("\r\033[K%s  aggregate %.1f Gbps  |  %s", time.Now().Format("15:04:05"), r.Summary.LiveGbps, strings.Join(parts, "  "))
		select {
		case <-ctx.Done():
			fmt.Println()
			return nil
		case <-time.After(2 * time.Second):
		}
	}
}

func printRun(r api.Run) {
	fmt.Printf("run %d  %s  test=%s  status=%s  verdict=%s\n", r.ID, r.Name, r.Spec.Test, r.Status, strings.ToUpper(r.Verdict))
	if r.Error != "" {
		fmt.Println("error:", r.Error)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	if api.IsLatency(r.Spec.Test) {
		fmt.Fprintln(w, "LEG\tCLIENT\tSERVER\tFABRIC\tSIZE\tMIN µs\tTYP µs\tAVG µs\tP99 µs\tP99.9 µs\tMAX µs\tBASELINE\tVERDICT\tNOTE")
		for _, l := range r.Legs {
			lt := l.Latency
			if lt == nil {
				lt = &api.Latency{}
			}
			fmt.Fprintf(w, "%d\t%s (%s)\t%s (%s)\t%s\t%d\t%.2f\t%.2f\t%.2f\t%.2f\t%.2f\t%.2f\t%s\t%s\t%s\n", l.Index+1, l.ClientHost, l.ClientIP, l.ServerHost, l.ServerIP, l.Fabric, l.MsgSize,
				lt.MinUs, lt.TypicalUs, lt.AvgUs, lt.P99Us, lt.P999Us, lt.MaxUs, legBaseline(l), l.Verdict, l.Note)
		}
		w.Flush()
		return
	}
	extra := ""
	if r.Spec.Test == "tcp" {
		extra = "\tRETRANS"
	} else if r.Spec.Test == "udp" {
		extra = "\tLOSS %\tJITTER ms"
	}
	fmt.Fprintln(w, "LEG\tCLIENT\tSERVER\tFABRIC\tSIZE\tEXPECTED\tTOOL Gbps\tCOUNTER Gbps"+extra+"\tBASELINE\tVERDICT\tNOTE")
	for _, l := range r.Legs {
		x := ""
		if r.Spec.Test == "tcp" {
			x = fmt.Sprintf("\t%d", l.Retransmits)
		} else if r.Spec.Test == "udp" {
			x = fmt.Sprintf("\t%.2f\t%.3f", l.LossPct, l.JitterMs)
		}
		fmt.Fprintf(w, "%d\t%s (%s)\t%s (%s)\t%s\t%d\t%.0f\t%.1f\t%.1f%s\t%s\t%s\t%s\n", l.Index+1, l.ClientHost, l.ClientIP, l.ServerHost, l.ServerIP, l.Fabric, l.MsgSize, l.ExpectedGbps, l.ToolGbps, l.CounterGbps, x, legBaseline(l), l.Verdict, l.Note)
	}
	w.Flush()
	fmt.Printf("aggregate: tool %.1f Gbps, counters %.1f Gbps", r.Summary.ToolGbps, r.Summary.CounterGbps)
	if r.Baseline {
		fmt.Print("  [baseline]")
	} else if r.Summary.BaselineRunID != 0 {
		fmt.Printf("  (%+.1f%% vs baseline run %d)", r.Summary.DeltaPct, r.Summary.BaselineRunID)
	}
	if len(r.Summary.ByFabric) > 0 {
		keys := make([]string, 0, len(r.Summary.ByFabric))
		for k := range r.Summary.ByFabric {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var parts []string
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s %.1f", k, r.Summary.ByFabric[k]))
		}
		fmt.Printf("  (by fabric: %s)", strings.Join(parts, ", "))
	}
	fmt.Println()
}

// baselineCol is the runs list's baseline column: the run's own mark, or
// its aggregate against the baseline it was compared with.
func baselineCol(r api.Run) string {
	switch {
	case r.Baseline:
		return "baseline"
	case r.Summary.BaselineRunID != 0:
		return fmt.Sprintf("%+.1f%% vs run %d", r.Summary.DeltaPct, r.Summary.BaselineRunID)
	}
	return "-"
}

func (c *cli) baseline(ctx context.Context, args []string) error {
	id, rest := leadingID(args)
	fs := flag.NewFlagSet("baseline", flag.ExitOnError)
	off := fs.Bool("off", false, "clear the baseline mark")
	fs.Parse(rest)
	if id == "" {
		return fmt.Errorf("usage: srnet baseline <run id> [--off]")
	}
	if err := c.call(ctx, "POST", "/api/v1/runs/"+id+"/baseline", api.BaselineRequest{Baseline: !*off}, nil); err != nil {
		return err
	}
	if *off {
		fmt.Printf("run %s is no longer a baseline\n", id)
	} else {
		fmt.Printf("run %s is now the baseline for runs of its shape\n", id)
	}
	return nil
}

// legBaseline is a leg's baseline column: the reference value and the change.
func legBaseline(l api.Leg) string {
	if l.Baseline == nil {
		return "-"
	}
	return fmt.Sprintf("%.1f (%+.1f%%)", l.Baseline.Value, l.Baseline.DeltaPct)
}

func (c *cli) cancel(ctx context.Context, args []string) error {
	id, _ := leadingID(args)
	if id == "" {
		return fmt.Errorf("usage: srnet cancel <run id>")
	}
	return c.call(ctx, "POST", "/api/v1/runs/"+id+"/cancel", nil, nil)
}

func leadingID(args []string) (string, []string) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return "", args
	}
	return args[0], args[1:]
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
