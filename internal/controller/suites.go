package controller

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/Realishbird/SR-Net-Speed-Test/internal/api"
)

var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// ParseCron validates a 5-field cron expression (or @daily, @hourly ...).
func ParseCron(expr string) (cron.Schedule, error) {
	return cronParser.Parse(expr)
}

// suiteRunner executes suites (sequences of runs) and fires schedules.
type suiteRunner struct {
	store  *Store
	orch   *Orchestrator
	alerts *alerter

	mu      sync.Mutex
	running map[int64]context.CancelFunc // suite run id -> cancel
}

func newSuiteRunner(s *Store, o *Orchestrator, a *alerter) *suiteRunner {
	return &suiteRunner{store: s, orch: o, alerts: a, running: map[int64]context.CancelFunc{}}
}

// Start begins a suite run in the background and returns its record.
func (sr *suiteRunner) Start(ctx context.Context, suite api.Suite, trigger string) (*api.SuiteRun, error) {
	if len(suite.Specs) == 0 {
		return nil, &userError{errors.New("suite has no runs")}
	}
	rec := &api.SuiteRun{SuiteID: suite.ID, SuiteName: suite.Name, Trigger: trigger, RunIDs: []int64{}}
	if err := sr.store.CreateSuiteRun(ctx, rec); err != nil {
		return nil, err
	}
	rctx, cancel := context.WithCancel(context.Background())
	sr.mu.Lock()
	sr.running[rec.ID] = cancel
	sr.mu.Unlock()
	go sr.execute(rctx, rec, suite)
	return rec, nil
}

func (sr *suiteRunner) Cancel(id int64) bool {
	sr.mu.Lock()
	c, ok := sr.running[id]
	sr.mu.Unlock()
	if ok {
		c()
	}
	return ok
}

func (sr *suiteRunner) execute(ctx context.Context, rec *api.SuiteRun, suite api.Suite) {
	defer func() {
		sr.mu.Lock()
		delete(sr.running, rec.ID)
		sr.mu.Unlock()
	}()
	bg := context.Background()
	rank := map[string]int{"": 0, "pass": 0, "warn": 1, "fail": 2}
	worst := "pass"
	status := "done"
	for i, spec := range suite.Specs {
		if ctx.Err() != nil {
			status = "canceled"
			break
		}
		spec.Name = fmt.Sprintf("%s %d/%d: %s", suite.Name, i+1, len(suite.Specs), nonEmpty(spec.Name, spec.Test))
		runID, err := sr.orch.Start(bg, spec)
		if err != nil {
			// A planning error (host offline, port busy) fails the suite but is reported, not retried.
			log.Printf("suite %s: run %d: %v", suite.Name, i+1, err)
			worst, status = "fail", "failed"
			sr.alerts.open(bg, api.Alert{Kind: "run_failed", Host: suite.Name, Message: fmt.Sprintf("suite %s run %d could not start: %v", suite.Name, i+1, err)})
			break
		}
		rec.RunIDs = append(rec.RunIDs, runID)
		sr.store.SaveSuiteRun(bg, rec)
		run := sr.waitRun(ctx, runID)
		if run.Status == "canceled" || ctx.Err() != nil {
			status = "canceled"
			break
		}
		if run.Status == "failed" || run.Verdict == "fail" {
			sr.alerts.open(bg, api.Alert{Kind: "run_failed", Host: suite.Name, Port: fmt.Sprint(runID),
				Message: fmt.Sprintf("run %d (%s) %s: %s", runID, run.Name, nonEmpty(run.Verdict, run.Status), run.Error)})
		}
		if rank[run.Verdict] > rank[worst] {
			worst = run.Verdict
		}
		if run.Status == "failed" {
			worst = "fail"
		}
	}
	now := time.Now()
	rec.EndedAt, rec.Status, rec.Verdict = &now, status, worst
	sr.store.SaveSuiteRun(bg, rec)
	if status == "done" && worst != "fail" {
		sr.alerts.resolve(bg, "run_failed", suite.Name, "")
	}
	log.Printf("suite %s (%s): %s, %s", suite.Name, rec.Trigger, status, worst)
}

func (sr *suiteRunner) waitRun(ctx context.Context, id int64) api.Run {
	for {
		run, err := sr.store.Run(context.Background(), id)
		if err == nil && run.Status != "queued" && run.Status != "running" {
			return run
		}
		select {
		case <-ctx.Done():
			sr.orch.Cancel(id)
			run.Status = "canceled"
			return run
		case <-time.After(2 * time.Second):
		}
	}
}

// scheduler fires enabled schedules whose cron expression matches the minute.
func (sr *suiteRunner) scheduler(ctx context.Context) {
	last := time.Now().Truncate(time.Minute)
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Until(last.Add(time.Minute).Add(2 * time.Second))):
		}
		now := time.Now().Truncate(time.Minute)
		if !now.After(last) {
			continue
		}
		scheds, err := sr.store.Schedules(ctx)
		if err != nil {
			log.Printf("scheduler: %v", err)
			continue
		}
		for _, sc := range scheds {
			if !sc.Enabled {
				continue
			}
			spec, err := ParseCron(sc.Cron)
			if err != nil {
				continue
			}
			// Fire if a scheduled minute fell in (last, now].
			if next := spec.Next(last); next.After(now) {
				continue
			}
			suite, err := sr.store.Suite(ctx, sc.SuiteID)
			if err != nil {
				continue
			}
			sr.store.TouchSchedule(ctx, sc.ID, now)
			if _, err := sr.Start(ctx, suite, "schedule:"+sc.Name); err != nil {
				log.Printf("schedule %s: %v", sc.Name, err)
			}
		}
		last = now
	}
}

// NextRuns fills NextRunAt for display.
func fillNextRuns(scheds []api.Schedule) {
	now := time.Now()
	for i := range scheds {
		if spec, err := ParseCron(scheds[i].Cron); err == nil && scheds[i].Enabled {
			n := spec.Next(now)
			scheds[i].NextRunAt = &n
		}
	}
}

func nonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
