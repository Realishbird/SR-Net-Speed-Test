package controller

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/Realishbird/SR-Net-Speed-Test/internal/api"
	"github.com/Realishbird/SR-Net-Speed-Test/internal/version"
)

// Phase 3 routes: releases and fleet updates, suites, schedules, alerts, reports.
func (s *Server) routes3(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/agent/release/{version}/{arch}", s.agent(s.agentRelease))

	mux.HandleFunc("GET /api/v1/releases", s.admin(s.listReleases))
	mux.HandleFunc("POST /api/v1/releases/fetch", s.admin(s.fetchRelease))
	mux.HandleFunc("GET /api/v1/updates", s.admin(s.listUpdates))
	mux.HandleFunc("POST /api/v1/updates", s.admin(s.startUpdate))

	mux.HandleFunc("GET /api/v1/suites", s.admin(s.listSuites))
	mux.HandleFunc("POST /api/v1/suites", s.admin(s.saveSuite))
	mux.HandleFunc("PUT /api/v1/suites/{id}", s.admin(s.saveSuite))
	mux.HandleFunc("DELETE /api/v1/suites/{id}", s.admin(s.deleteSuite))
	mux.HandleFunc("POST /api/v1/suites/{id}/run", s.admin(s.runSuite))
	mux.HandleFunc("GET /api/v1/suite-runs", s.admin(s.listSuiteRuns))
	mux.HandleFunc("POST /api/v1/suite-runs/{id}/cancel", s.admin(s.cancelSuiteRun))

	mux.HandleFunc("GET /api/v1/schedules", s.admin(s.listSchedules))
	mux.HandleFunc("POST /api/v1/schedules", s.admin(s.saveSchedule))
	mux.HandleFunc("PUT /api/v1/schedules/{id}", s.admin(s.saveSchedule))
	mux.HandleFunc("DELETE /api/v1/schedules/{id}", s.admin(s.deleteSchedule))

	mux.HandleFunc("GET /api/v1/alerts", s.admin(s.listAlerts))
	mux.HandleFunc("GET /api/v1/runs/{id}/report.csv", s.admin(s.reportCSV))
	mux.HandleFunc("GET /api/v1/runs/{id}/report.html", s.admin(s.reportHTML))
}

// Releases and updates.

func (s *Server) agentRelease(w http.ResponseWriter, r *http.Request) {
	p, sum, ok := s.rel.BinaryPath(r.PathValue("version"), r.PathValue("arch"))
	if !ok {
		httpError(w, http.StatusNotFound, "release not mirrored")
		return
	}
	w.Header().Set("X-Checksum-SHA256", sum)
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeFile(w, r, p)
}

func (s *Server) listReleases(w http.ResponseWriter, r *http.Request) {
	out := api.ReleaseList{Current: version.Version, Mirrored: s.rel.Mirrored()}
	if r.URL.Query().Get("check") == "1" {
		if v, err := s.rel.LatestOnGitHub(r.Context()); err == nil {
			out.Latest = v
		} else {
			out.Latest = "unknown (" + err.Error() + ")"
		}
	}
	if out.Mirrored == nil {
		out.Mirrored = []api.Release{}
	}
	writeJSON(w, out)
}

func (s *Server) fetchRelease(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Version string `json:"version"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Version == "" || req.Version == "latest" {
		v, err := s.rel.LatestOnGitHub(r.Context())
		if err != nil {
			httpError(w, http.StatusBadGateway, "could not ask GitHub for the latest release: "+err.Error())
			return
		}
		req.Version = v
	}
	rel, err := s.rel.Fetch(r.Context(), req.Version)
	if userErr(w, err) {
		return
	}
	s.store.Audit(r.Context(), "release.fetch", rel)
	writeJSON(w, rel)
}

func (s *Server) listUpdates(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.store.UpdateJobs(r.Context(), 20)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, jobs)
}

func (s *Server) startUpdate(w http.ResponseWriter, r *http.Request) {
	var req api.UpdateRequest
	if !readJSON(w, r, &req) {
		return
	}
	hosts, err := s.store.Hosts(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	targets := hosts
	if len(req.Hosts) > 0 {
		targets = nil
		seen := map[int64]bool{}
		for _, sel := range req.Hosts {
			matched := false
			for _, h := range hosts {
				if hostMatches(h, sel) && !seen[h.ID] {
					seen[h.ID], matched = true, true
					targets = append(targets, h)
				}
			}
			if !matched {
				httpError(w, http.StatusConflict, fmt.Sprintf("no host matches %q", sel))
				return
			}
		}
	}
	job, err := s.rel.StartJob(r.Context(), req.Version, targets, s.orch)
	if userErr(w, err) {
		return
	}
	s.store.Audit(r.Context(), "update.start", map[string]any{"job_id": job.ID, "version": req.Version, "hosts": req.Hosts})
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, job)
}

func hostMatches(h hostRec, sel string) bool {
	if t, ok := strings.CutPrefix(sel, "tag:"); ok {
		for _, tag := range h.Tags {
			if strings.EqualFold(tag, t) {
				return true
			}
		}
		return false
	}
	return strings.EqualFold(h.Name, sel) || strings.EqualFold(h.Hostname, sel)
}

// Suites.

func (s *Server) listSuites(w http.ResponseWriter, r *http.Request) {
	out, err := s.store.Suites(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, out)
}

func (s *Server) saveSuite(w http.ResponseWriter, r *http.Request) {
	var su api.Suite
	if !readJSON(w, r, &su) {
		return
	}
	if r.PathValue("id") != "" {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		su.ID = id
	} else {
		su.ID = 0
	}
	su.Name = strings.TrimSpace(su.Name)
	if su.Name == "" {
		httpError(w, http.StatusBadRequest, "suite needs a name")
		return
	}
	if len(su.Specs) == 0 {
		httpError(w, http.StatusBadRequest, "suite needs at least one run")
		return
	}
	for i := range su.Specs {
		if err := applyDefaults(&su.Specs[i]); err != nil {
			httpError(w, http.StatusBadRequest, fmt.Sprintf("run %d: %v", i+1, err))
			return
		}
	}
	if err := s.store.SaveSuite(r.Context(), &su); err != nil {
		if strings.Contains(err.Error(), "suites_name_key") {
			httpError(w, http.StatusConflict, "a suite with that name exists")
			return
		}
		storeError(w, err)
		return
	}
	s.store.Audit(r.Context(), "suite.save", map[string]any{"id": su.ID, "name": su.Name})
	writeJSON(w, su)
}

func (s *Server) deleteSuite(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteSuite(r.Context(), id); err != nil {
		serverError(w, err)
		return
	}
	s.store.Audit(r.Context(), "suite.delete", map[string]any{"id": id})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) runSuite(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	su, err := s.store.Suite(r.Context(), id)
	if err != nil {
		storeError(w, err)
		return
	}
	rec, err := s.suites.Start(r.Context(), su, "manual")
	if userErr(w, err) {
		return
	}
	s.store.Audit(r.Context(), "suite.run", map[string]any{"suite_run_id": rec.ID, "suite": su.Name})
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, rec)
}

func (s *Server) listSuiteRuns(w http.ResponseWriter, r *http.Request) {
	out, err := s.store.SuiteRuns(r.Context(), 50)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, out)
}

func (s *Server) cancelSuiteRun(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if !s.suites.Cancel(id) {
		httpError(w, http.StatusConflict, "suite run is not active")
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// Schedules.

func (s *Server) listSchedules(w http.ResponseWriter, r *http.Request) {
	out, err := s.store.Schedules(r.Context())
	if err != nil {
		serverError(w, err)
		return
	}
	fillNextRuns(out)
	writeJSON(w, out)
}

func (s *Server) saveSchedule(w http.ResponseWriter, r *http.Request) {
	var sc api.Schedule
	if !readJSON(w, r, &sc) {
		return
	}
	if r.PathValue("id") != "" {
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		sc.ID = id
	} else {
		sc.ID = 0
	}
	sc.Name = strings.TrimSpace(sc.Name)
	sc.Cron = strings.TrimSpace(sc.Cron)
	if sc.Name == "" {
		httpError(w, http.StatusBadRequest, "schedule needs a name")
		return
	}
	if _, err := ParseCron(sc.Cron); err != nil {
		httpError(w, http.StatusBadRequest, fmt.Sprintf("cron %q: %v", sc.Cron, err))
		return
	}
	if _, err := s.store.Suite(r.Context(), sc.SuiteID); err != nil {
		httpError(w, http.StatusBadRequest, fmt.Sprintf("no suite with id %d", sc.SuiteID))
		return
	}
	if err := s.store.SaveSchedule(r.Context(), &sc); err != nil {
		storeError(w, err)
		return
	}
	s.store.Audit(r.Context(), "schedule.save", map[string]any{"id": sc.ID, "name": sc.Name, "cron": sc.Cron, "enabled": sc.Enabled})
	fill := []api.Schedule{sc}
	fillNextRuns(fill)
	writeJSON(w, fill[0])
}

func (s *Server) deleteSchedule(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteSchedule(r.Context(), id); err != nil {
		serverError(w, err)
		return
	}
	s.store.Audit(r.Context(), "schedule.delete", map[string]any{"id": id})
	w.WriteHeader(http.StatusNoContent)
}

// Alerts and reports.

func (s *Server) listAlerts(w http.ResponseWriter, r *http.Request) {
	out, err := s.store.Alerts(r.Context(), r.URL.Query().Get("all") != "1", 200)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, out)
}

func (s *Server) reportCSV(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	run, err := s.store.Run(r.Context(), id)
	if err != nil {
		storeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="srnet-run-%d.csv"`, id))
	WriteRunCSV(w, run)
}

func (s *Server) reportHTML(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	run, err := s.store.Run(r.Context(), id)
	if err != nil {
		storeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	WriteRunHTML(w, run, version.Version)
}

// userErr writes a 409 for user errors and a 500 for the rest; false means no error.
func userErr(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	var ue *userError
	if errors.As(err, &ue) {
		httpError(w, http.StatusConflict, ue.Error())
		return true
	}
	serverError(w, err)
	return true
}
