package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Realishbird/SR-Net-Speed-Test/internal/api"
)

// Releases and fleet updates.

func (c *cli) releases(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("releases", flag.ExitOnError)
	check := fs.Bool("check", false, "also ask GitHub for the newest release")
	fs.Parse(args)
	q := ""
	if *check {
		q = "?check=1"
	}
	var list api.ReleaseList
	if err := c.call(ctx, "GET", "/api/v1/releases"+q, nil, &list); err != nil {
		return err
	}
	fmt.Printf("controller %s\n", list.Current)
	if list.Latest != "" {
		fmt.Printf("latest on GitHub: %s\n", list.Latest)
	}
	if len(list.Mirrored) == 0 {
		fmt.Println("no agent releases mirrored yet: srnet fetch-release [version]")
		return nil
	}
	fmt.Println("mirrored:")
	for _, r := range list.Mirrored {
		fmt.Printf("  %-12s %-14s %s\n", r.Version, strings.Join(r.Arches, ","), r.Mirrored.Local().Format("2006-01-02 15:04"))
	}
	return nil
}

func (c *cli) fetchRelease(ctx context.Context, args []string) error {
	v := "latest"
	if len(args) > 0 {
		v = args[0]
	}
	var rel api.Release
	if err := c.call(ctx, "POST", "/api/v1/releases/fetch", map[string]string{"version": v}, &rel); err != nil {
		return err
	}
	fmt.Printf("mirrored %s (%s)\n", rel.Version, strings.Join(rel.Arches, ", "))
	return nil
}

func (c *cli) update(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	var req api.UpdateRequest
	var hosts, tag string
	fs.StringVar(&req.Version, "version", "", "mirrored release to install (required)")
	fs.StringVar(&hosts, "host", "", "host names, comma-separated (default: every host)")
	fs.StringVar(&tag, "tag", "", "only hosts with this tag")
	watch := fs.Bool("watch", true, "follow the job until it ends")
	fs.Parse(args)
	if req.Version == "" {
		return fmt.Errorf("--version is required (see srnet releases)")
	}
	req.Hosts = splitList(hosts)
	if tag != "" {
		req.Hosts = append(req.Hosts, "tag:"+tag)
	}
	var job api.UpdateJob
	if err := c.call(ctx, "POST", "/api/v1/updates", req, &job); err != nil {
		return err
	}
	fmt.Printf("update job %d: %d host(s) to %s\n", job.ID, len(job.Hosts), job.Version)
	if !*watch {
		printJob(job)
		return nil
	}
	shown := map[int64]string{}
	for {
		var jobs []api.UpdateJob
		if err := c.call(ctx, "GET", "/api/v1/updates", nil, &jobs); err != nil {
			return err
		}
		for _, j := range jobs {
			if j.ID != job.ID {
				continue
			}
			for _, h := range j.Hosts {
				if shown[h.HostID] != h.Status {
					shown[h.HostID] = h.Status
					line := fmt.Sprintf("  %-24s %s", h.Name, h.Status)
					if h.Note != "" {
						line += ": " + h.Note
					}
					fmt.Println(line)
				}
			}
			if j.Status != "running" {
				fmt.Printf("job %d %s\n", j.ID, j.Status)
				if j.Status == "failed" {
					os.Exit(1)
				}
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(3 * time.Second):
		}
	}
}

func (c *cli) updates(ctx context.Context) error {
	var jobs []api.UpdateJob
	if err := c.call(ctx, "GET", "/api/v1/updates", nil, &jobs); err != nil {
		return err
	}
	if len(jobs) == 0 {
		fmt.Println("no update jobs yet")
	}
	for _, j := range jobs {
		fmt.Printf("job %d  %s  %s  %s\n", j.ID, j.Version, j.Status, j.CreatedAt.Local().Format("2006-01-02 15:04"))
		printJob(j)
	}
	return nil
}

func printJob(j api.UpdateJob) {
	for _, h := range j.Hosts {
		line := fmt.Sprintf("  %-24s %-10s %s", h.Name, h.From, h.Status)
		if h.Note != "" {
			line += ": " + h.Note
		}
		fmt.Println(line)
	}
}

// Suites: suite list | show <id> | create <name> <specs.json> | update <id> <specs.json> | delete <id> | run <id|name>
// A specs file is a JSON array of run specs (the same fields as POST /api/v1/runs).

func (c *cli) suite(ctx context.Context, args []string) error {
	if len(args) == 0 {
		args = []string{"list"}
	}
	switch args[0] {
	case "list":
		suites, err := c.suites(ctx)
		if err != nil {
			return err
		}
		if len(suites) == 0 {
			fmt.Println("no suites: srnet suite create <name> <specs.json>")
		}
		for _, s := range suites {
			fmt.Printf("%-4d %-28s %d run(s)\n", s.ID, s.Name, len(s.Specs))
		}
		return nil
	case "show":
		s, err := c.findSuite(ctx, args[1:])
		if err != nil {
			return err
		}
		b, _ := json.MarshalIndent(s, "", "  ")
		fmt.Println(string(b))
		return nil
	case "create", "update":
		if len(args) < 3 {
			return fmt.Errorf("usage: srnet suite %s <name|id> <specs.json>", args[0])
		}
		specs, err := readSpecs(args[2])
		if err != nil {
			return err
		}
		var s api.Suite
		method, path := "POST", "/api/v1/suites"
		if args[0] == "update" {
			cur, err := c.findSuite(ctx, args[1:2])
			if err != nil {
				return err
			}
			s.Name = cur.Name
			method, path = "PUT", fmt.Sprintf("/api/v1/suites/%d", cur.ID)
		} else {
			s.Name = args[1]
		}
		s.Specs = specs
		if err := c.call(ctx, method, path, s, &s); err != nil {
			return err
		}
		fmt.Printf("suite %d %q: %d run(s)\n", s.ID, s.Name, len(s.Specs))
		return nil
	case "delete":
		s, err := c.findSuite(ctx, args[1:])
		if err != nil {
			return err
		}
		return c.call(ctx, "DELETE", fmt.Sprintf("/api/v1/suites/%d", s.ID), nil, nil)
	case "run":
		s, err := c.findSuite(ctx, args[1:])
		if err != nil {
			return err
		}
		var rec api.SuiteRun
		if err := c.call(ctx, "POST", fmt.Sprintf("/api/v1/suites/%d/run", s.ID), nil, &rec); err != nil {
			return err
		}
		fmt.Printf("suite run %d started (%s, %d run(s)); srnet suite-runs to follow\n", rec.ID, s.Name, len(s.Specs))
		return nil
	}
	return fmt.Errorf("unknown suite subcommand %q", args[0])
}

func readSpecs(path string) ([]api.RunSpec, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var specs []api.RunSpec
	if err := json.Unmarshal(b, &specs); err != nil {
		var one api.RunSpec
		if err2 := json.Unmarshal(b, &one); err2 != nil {
			return nil, fmt.Errorf("%s: expected a JSON array of run specs: %v", path, err)
		}
		specs = []api.RunSpec{one}
	}
	return specs, nil
}

func (c *cli) suites(ctx context.Context) ([]api.Suite, error) {
	var out []api.Suite
	return out, c.call(ctx, "GET", "/api/v1/suites", nil, &out)
}

func (c *cli) findSuite(ctx context.Context, args []string) (api.Suite, error) {
	if len(args) == 0 {
		return api.Suite{}, fmt.Errorf("suite id or name required")
	}
	suites, err := c.suites(ctx)
	if err != nil {
		return api.Suite{}, err
	}
	id, _ := strconv.ParseInt(args[0], 10, 64)
	for _, s := range suites {
		if s.ID == id || strings.EqualFold(s.Name, args[0]) {
			return s, nil
		}
	}
	return api.Suite{}, fmt.Errorf("no suite %q", args[0])
}

func (c *cli) suiteRuns(ctx context.Context) error {
	var runs []api.SuiteRun
	if err := c.call(ctx, "GET", "/api/v1/suite-runs", nil, &runs); err != nil {
		return err
	}
	if len(runs) == 0 {
		fmt.Println("no suite runs yet")
	}
	for _, r := range runs {
		ids := make([]string, len(r.RunIDs))
		for i, id := range r.RunIDs {
			ids[i] = strconv.FormatInt(id, 10)
		}
		fmt.Printf("%-4d %-24s %-18s %-9s %-5s %s  runs %s\n", r.ID, r.SuiteName, r.Trigger, r.Status, r.Verdict, r.StartedAt.Local().Format("01-02 15:04"), strings.Join(ids, ","))
	}
	return nil
}

// Schedules: schedule list | add <name> --suite <id|name> --cron "0 2 * * *" | enable <id> | disable <id> | delete <id>

func (c *cli) schedule(ctx context.Context, args []string) error {
	if len(args) == 0 {
		args = []string{"list"}
	}
	list := func() ([]api.Schedule, error) {
		var out []api.Schedule
		return out, c.call(ctx, "GET", "/api/v1/schedules", nil, &out)
	}
	switch args[0] {
	case "list":
		scheds, err := list()
		if err != nil {
			return err
		}
		if len(scheds) == 0 {
			fmt.Println(`no schedules: srnet schedule add nightly --suite <name> --cron "0 2 * * *"`)
		}
		for _, s := range scheds {
			state := "enabled"
			if !s.Enabled {
				state = "disabled"
			}
			next := ""
			if s.NextRunAt != nil {
				next = "next " + s.NextRunAt.Local().Format("2006-01-02 15:04")
			}
			fmt.Printf("%-4d %-20s %-20s %-14q %-9s %s\n", s.ID, s.Name, s.SuiteName, s.Cron, state, next)
		}
		return nil
	case "add":
		fs := flag.NewFlagSet("schedule add", flag.ExitOnError)
		suite := fs.String("suite", "", "suite id or name")
		cron := fs.String("cron", "", `5-field cron in controller local time, e.g. "0 2 * * *"`)
		if len(args) < 2 {
			return fmt.Errorf("usage: srnet schedule add <name> --suite <id|name> --cron <expr>")
		}
		fs.Parse(args[2:])
		s, err := c.findSuite(ctx, []string{*suite})
		if err != nil {
			return err
		}
		sc := api.Schedule{Name: args[1], SuiteID: s.ID, Cron: *cron, Enabled: true}
		if err := c.call(ctx, "POST", "/api/v1/schedules", sc, &sc); err != nil {
			return err
		}
		fmt.Printf("schedule %d %q: %s at %q", sc.ID, sc.Name, s.Name, sc.Cron)
		if sc.NextRunAt != nil {
			fmt.Printf(", next %s", sc.NextRunAt.Local().Format("2006-01-02 15:04"))
		}
		fmt.Println()
		return nil
	case "enable", "disable", "delete":
		if len(args) < 2 {
			return fmt.Errorf("usage: srnet schedule %s <id>", args[0])
		}
		id, _ := strconv.ParseInt(args[1], 10, 64)
		if args[0] == "delete" {
			return c.call(ctx, "DELETE", fmt.Sprintf("/api/v1/schedules/%d", id), nil, nil)
		}
		scheds, err := list()
		if err != nil {
			return err
		}
		for _, sc := range scheds {
			if sc.ID == id {
				sc.Enabled = args[0] == "enable"
				return c.call(ctx, "PUT", fmt.Sprintf("/api/v1/schedules/%d", id), sc, nil)
			}
		}
		return fmt.Errorf("no schedule %d", id)
	}
	return fmt.Errorf("unknown schedule subcommand %q", args[0])
}

// Alerts and reports.

func (c *cli) alerts(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("alerts", flag.ExitOnError)
	all := fs.Bool("all", false, "include resolved alerts")
	fs.Parse(args)
	q := ""
	if *all {
		q = "?all=1"
	}
	var alerts []api.Alert
	if err := c.call(ctx, "GET", "/api/v1/alerts"+q, nil, &alerts); err != nil {
		return err
	}
	if len(alerts) == 0 {
		fmt.Println("no open alerts")
	}
	for _, a := range alerts {
		state := "OPEN    "
		if a.ResolvedAt != nil {
			state = "resolved"
		}
		fmt.Printf("%s %-16s %s  %s\n", state, a.Kind, a.OpenedAt.Local().Format("01-02 15:04"), a.Message)
	}
	return nil
}

func (c *cli) report(ctx context.Context, args []string) error {
	id, rest := leadingID(args)
	if id == "" {
		return fmt.Errorf("usage: srnet report <run id> [--format csv|html] [-o file]")
	}
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	format := fs.String("format", "csv", "csv or html")
	out := fs.String("o", "", "write to this file instead of stdout")
	fs.Parse(rest)
	if *format != "csv" && *format != "html" {
		return fmt.Errorf("format must be csv or html")
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/api/v1/runs/%s/report.%s", c.url, id, *format), nil)
	req.SetBasicAuth(c.user, c.pass)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var e struct{ Error string }
		json.NewDecoder(resp.Body).Decode(&e)
		return fmt.Errorf("%s", nonEmpty(e.Error, resp.Status))
	}
	w := io.Writer(os.Stdout)
	if *out != "" {
		f, err := os.Create(*out)
		if err != nil {
			return err
		}
		defer f.Close()
		w = f
	}
	_, err = io.Copy(w, resp.Body)
	return err
}

func nonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
