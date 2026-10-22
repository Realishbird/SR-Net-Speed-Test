package controller

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Realishbird/SR-Net-Speed-Test/internal/api"
	"github.com/jackc/pgx/v5"
)

// Housekeeping: mirrored releases prune themselves and runs can be deleted.

const defaultReleaseKeep = 3

// InUse lists the versions that must stay on disk: every version an agent
// last reported, and the target of a running update job.
func (r *releases) InUse() map[string]bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]bool{}
	for _, v := range r.versions {
		if v != "" {
			out[v] = true
		}
	}
	if r.job != nil && r.job.Status == "running" {
		out[r.job.Version] = true
	}
	return out
}

// versionCounts counts agents per reported version.
func (r *releases) versionCounts() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]int{}
	for _, v := range r.versions {
		if v != "" {
			out[v]++
		}
	}
	return out
}

// Prune removes mirrored releases beyond the newest keep, except the ones
// in use. It returns the versions removed. keep <= 0 disables pruning, and
// nothing is removed while an update job runs.
func (r *releases) Prune(keep int) []string {
	if keep <= 0 {
		return nil
	}
	if j := r.CurrentJob(); j != nil && j.Status == "running" {
		return nil
	}
	inUse := r.InUse()
	var removed []string
	kept := 0
	for _, rel := range r.Mirrored() { // newest first
		if inUse[rel.Version] {
			continue
		}
		if kept < keep {
			kept++
			continue
		}
		if err := r.Remove(rel.Version, true); err == nil {
			removed = append(removed, rel.Version)
		}
	}
	return removed
}

// Remove deletes one mirrored release. Unless force, a version in use by an
// agent or an update job is refused.
func (r *releases) Remove(version string, force bool) error {
	if !versionRe.MatchString(version) {
		return &userError{fmt.Errorf("version must look like v1.2.3, not %q", version)}
	}
	if !force {
		if j := r.CurrentJob(); j != nil && j.Status == "running" && j.Version == version {
			return &userError{fmt.Errorf("%s is the target of the running update", version)}
		}
		if r.InUse()[version] {
			return &userError{fmt.Errorf("%s is running on an agent; update the fleet first", version)}
		}
	}
	dir := filepath.Join(r.dir, version)
	if _, err := os.Stat(dir); err != nil {
		return &userError{fmt.Errorf("%s is not mirrored", version)}
	}
	return os.RemoveAll(dir)
}

// releaseKeep is the configured number of releases to keep.
func (s *Server) releaseKeep() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg.ReleaseKeep < 0 {
		return 0
	}
	if s.cfg.ReleaseKeep == 0 {
		return defaultReleaseKeep
	}
	return s.cfg.ReleaseKeep
}

// pruneReleases runs after each mirror and once a day.
func (s *Server) pruneReleases(ctx context.Context, by string) {
	removed := s.rel.Prune(s.releaseKeep())
	if len(removed) == 0 {
		return
	}
	log.Printf("releases: pruned %s", strings.Join(removed, ", "))
	s.store.Audit(ctx, "release.prune", map[string]any{"removed": removed, "keep": s.releaseKeep(), "by": by})
}

func (s *Server) pruneLoop(ctx context.Context) {
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return
	case <-time.After(10 * time.Minute):
		s.pruneReleases(ctx, "startup")
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.pruneReleases(ctx, "daily")
		}
	}
}

func (s *Server) removeRelease(w http.ResponseWriter, r *http.Request) {
	v := r.PathValue("version")
	err := s.rel.Remove(v, r.URL.Query().Get("force") == "1")
	if userErr(w, err) {
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	s.store.Audit(r.Context(), "release.remove", map[string]any{"version": v, "by": who(r).Name})
	w.WriteHeader(http.StatusNoContent)
}

// Run deletion.

// RunFilter picks runs for a bulk delete when no ids are given.
type runFilter struct {
	IDs           []int64 `json:"ids,omitempty"`
	Failed        bool    `json:"failed,omitempty"`          // status failed or canceled, or verdict fail
	OlderThanDays int     `json:"older_than_days,omitempty"` // created before now - N days
	Force         bool    `json:"force,omitempty"`           // allow deleting baselines
	Samples       bool    `json:"samples,omitempty"`         // also drop the run's series from VictoriaMetrics
}

// SelectRuns resolves a filter to run ids (finished runs only).
func (s *Store) SelectRuns(ctx context.Context, f runFilter) ([]int64, error) {
	if len(f.IDs) > 0 {
		return f.IDs, nil
	}
	if !f.Failed && f.OlderThanDays <= 0 {
		return nil, &userError{errors.New("give run ids, or a filter (failed, older_than_days)")}
	}
	q := `SELECT id FROM runs WHERE status NOT IN ('queued','running')`
	var args []any
	if f.Failed {
		q += ` AND (status IN ('failed','canceled') OR verdict='fail')`
	}
	if f.OlderThanDays > 0 {
		args = append(args, time.Now().Add(-time.Duration(f.OlderThanDays)*24*time.Hour))
		q += fmt.Sprintf(` AND created_at < $%d`, len(args))
	}
	rows, err := s.db.Query(ctx, q+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// DeleteRun removes a run and its legs (cascade) and drops it from suite
// runs. A queued or running run is refused; a baseline needs force.
func (s *Store) DeleteRun(ctx context.Context, id int64, force bool) (api.Run, error) {
	run, err := s.Run(ctx, id)
	if err != nil {
		return run, err
	}
	if run.Status == "queued" || run.Status == "running" {
		return run, &userError{fmt.Errorf("run %d is %s; cancel it first", id, run.Status)}
	}
	if run.Baseline && !force {
		return run, &userError{fmt.Errorf("run %d is a baseline; unmark it or delete with force", id)}
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return run, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `UPDATE suite_runs SET run_ids = array_remove(run_ids, $1) WHERE $1 = ANY(run_ids)`, id); err != nil {
		return run, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM runs WHERE id=$1`, id); err != nil {
		return run, err
	}
	return run, tx.Commit(ctx)
}

// deleteRunSeries drops every series stamped with the run's id from
// VictoriaMetrics. Only series written during the run carry run_id, so
// idle telemetry of the same ports is untouched.
func (m *metrics) deleteRunSeries(ctx context.Context, runID int64) error {
	if m.vmURL == "" {
		return nil
	}
	q := url.Values{"match[]": {fmt.Sprintf(`{run_id="%d"}`, runID)}}
	req, err := http.NewRequestWithContext(ctx, "POST", m.vmURL+"/api/v1/admin/tsdb/delete_series?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("victoriametrics delete_series: %s", resp.Status)
	}
	return nil
}

// deleteRuns deletes each id, collecting the outcome per run.
func (s *Server) deleteRuns(ctx context.Context, ids []int64, force, samples bool, by string) api.DeleteResult {
	res := api.DeleteResult{Deleted: []int64{}, Skipped: []api.DeleteSkip{}}
	for _, id := range ids {
		run, err := s.store.DeleteRun(ctx, id, force)
		if err != nil {
			msg := err.Error()
			if errors.Is(err, pgx.ErrNoRows) {
				msg = "not found"
			}
			res.Skipped = append(res.Skipped, api.DeleteSkip{ID: id, Reason: msg})
			continue
		}
		res.Deleted = append(res.Deleted, id)
		if samples {
			if err := s.metrics.deleteRunSeries(ctx, id); err != nil {
				log.Printf("run %d: delete samples: %v", id, err)
				res.SampleErrors++
			}
		}
		s.store.Audit(ctx, "run.delete", map[string]any{"run_id": id, "name": run.Name, "status": run.Status, "verdict": run.Verdict, "samples": samples, "by": by})
	}
	return res
}

func (s *Server) deleteRun(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	res := s.deleteRuns(r.Context(), []int64{id}, q.Get("force") == "1", q.Get("samples") == "1", who(r).Name)
	if len(res.Deleted) == 0 {
		reason := res.Skipped[0].Reason
		if reason == "not found" {
			httpError(w, http.StatusNotFound, "run not found")
			return
		}
		httpError(w, http.StatusConflict, reason)
		return
	}
	writeJSON(w, res)
}

func (s *Server) deleteRunsBulk(w http.ResponseWriter, r *http.Request) {
	var f runFilter
	if !readJSON(w, r, &f) {
		return
	}
	ids, err := s.store.SelectRuns(r.Context(), f)
	if userErr(w, err) {
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	writeJSON(w, s.deleteRuns(r.Context(), ids, f.Force, f.Samples, who(r).Name))
}

// previewDelete tells the UI what a filter would remove.
func (s *Server) previewDelete(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	days, _ := strconv.Atoi(q.Get("older_than_days"))
	ids, err := s.store.SelectRuns(r.Context(), runFilter{Failed: q.Get("failed") == "1", OlderThanDays: days})
	if userErr(w, err) {
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	if ids == nil {
		ids = []int64{}
	}
	writeJSON(w, map[string]any{"ids": ids, "count": len(ids)})
}

func (s *Server) routesCleanup(mux *http.ServeMux) {
	mux.HandleFunc("DELETE /api/v1/releases/{version}", s.admin(s.removeRelease))
	mux.HandleFunc("DELETE /api/v1/runs/{id}", s.admin(s.deleteRun))
	mux.HandleFunc("POST /api/v1/runs/delete", s.admin(s.deleteRunsBulk))
	mux.HandleFunc("GET /api/v1/runs/delete-preview", s.view(s.previewDelete))
}
