package controller

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Realishbird/SR-Net-Speed-Test/internal/api"
)

const schema3 = `
CREATE TABLE IF NOT EXISTS update_jobs (
	id         BIGSERIAL PRIMARY KEY,
	version    TEXT NOT NULL,
	status     TEXT NOT NULL,
	hosts      JSONB NOT NULL DEFAULT '[]',
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	ended_at   TIMESTAMPTZ
);
CREATE TABLE IF NOT EXISTS suites (
	id    BIGSERIAL PRIMARY KEY,
	name  TEXT UNIQUE NOT NULL,
	specs JSONB NOT NULL DEFAULT '[]'
);
CREATE TABLE IF NOT EXISTS suite_runs (
	id         BIGSERIAL PRIMARY KEY,
	suite_id   BIGINT REFERENCES suites(id) ON DELETE SET NULL,
	suite_name TEXT NOT NULL,
	trigger    TEXT NOT NULL,
	status     TEXT NOT NULL,
	verdict    TEXT NOT NULL DEFAULT '',
	run_ids    BIGINT[] NOT NULL DEFAULT '{}',
	started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	ended_at   TIMESTAMPTZ
);
CREATE TABLE IF NOT EXISTS schedules (
	id          BIGSERIAL PRIMARY KEY,
	name        TEXT NOT NULL,
	suite_id    BIGINT NOT NULL REFERENCES suites(id) ON DELETE CASCADE,
	cron        TEXT NOT NULL,
	enabled     BOOLEAN NOT NULL DEFAULT true,
	last_run_at TIMESTAMPTZ
);
CREATE TABLE IF NOT EXISTS alerts (
	id          BIGSERIAL PRIMARY KEY,
	kind        TEXT NOT NULL,
	host        TEXT NOT NULL DEFAULT '',
	port        TEXT NOT NULL DEFAULT '',
	message     TEXT NOT NULL,
	opened_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
	resolved_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS alerts_open_idx ON alerts (kind, host, port) WHERE resolved_at IS NULL;
`

// Update jobs.

func (s *Store) CreateUpdateJob(ctx context.Context, j *api.UpdateJob) error {
	h, _ := json.Marshal(j.Hosts)
	return s.db.QueryRow(ctx, `INSERT INTO update_jobs (version, status, hosts) VALUES ($1, $2, $3) RETURNING id, created_at`, j.Version, j.Status, h).Scan(&j.ID, &j.CreatedAt)
}

func (s *Store) SaveUpdateJob(ctx context.Context, j *api.UpdateJob) error {
	h, _ := json.Marshal(j.Hosts)
	_, err := s.db.Exec(ctx, `UPDATE update_jobs SET status=$2, hosts=$3, ended_at=$4 WHERE id=$1`, j.ID, j.Status, h, j.EndedAt)
	return err
}

func (s *Store) UpdateJobs(ctx context.Context, limit int) ([]api.UpdateJob, error) {
	rows, err := s.db.Query(ctx, `SELECT id, version, status, hosts, created_at, ended_at FROM update_jobs ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.UpdateJob{}
	for rows.Next() {
		var j api.UpdateJob
		var h []byte
		if err := rows.Scan(&j.ID, &j.Version, &j.Status, &h, &j.CreatedAt, &j.EndedAt); err != nil {
			return nil, err
		}
		json.Unmarshal(h, &j.Hosts)
		out = append(out, j)
	}
	return out, rows.Err()
}

// Suites.

func (s *Store) Suites(ctx context.Context) ([]api.Suite, error) {
	rows, err := s.db.Query(ctx, `SELECT id, name, specs FROM suites ORDER BY lower(name)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.Suite{}
	for rows.Next() {
		var su api.Suite
		var sp []byte
		if err := rows.Scan(&su.ID, &su.Name, &sp); err != nil {
			return nil, err
		}
		json.Unmarshal(sp, &su.Specs)
		out = append(out, su)
	}
	return out, rows.Err()
}

func (s *Store) Suite(ctx context.Context, id int64) (api.Suite, error) {
	var su api.Suite
	var sp []byte
	err := s.db.QueryRow(ctx, `SELECT id, name, specs FROM suites WHERE id=$1`, id).Scan(&su.ID, &su.Name, &sp)
	json.Unmarshal(sp, &su.Specs)
	return su, err
}

func (s *Store) SaveSuite(ctx context.Context, su *api.Suite) error {
	sp, _ := json.Marshal(su.Specs)
	if su.ID == 0 {
		return s.db.QueryRow(ctx, `INSERT INTO suites (name, specs) VALUES ($1, $2) RETURNING id`, su.Name, sp).Scan(&su.ID)
	}
	tag, err := s.db.Exec(ctx, `UPDATE suites SET name=$2, specs=$3 WHERE id=$1`, su.ID, su.Name, sp)
	if err == nil && tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return err
}

func (s *Store) DeleteSuite(ctx context.Context, id int64) error {
	_, err := s.db.Exec(ctx, `DELETE FROM suites WHERE id=$1`, id)
	return err
}

func (s *Store) CreateSuiteRun(ctx context.Context, sr *api.SuiteRun) error {
	return s.db.QueryRow(ctx, `INSERT INTO suite_runs (suite_id, suite_name, trigger, status) VALUES ($1, $2, $3, 'running') RETURNING id, started_at`,
		sr.SuiteID, sr.SuiteName, sr.Trigger).Scan(&sr.ID, &sr.StartedAt)
}

func (s *Store) SaveSuiteRun(ctx context.Context, sr *api.SuiteRun) error {
	_, err := s.db.Exec(ctx, `UPDATE suite_runs SET status=$2, verdict=$3, run_ids=$4, ended_at=$5 WHERE id=$1`, sr.ID, sr.Status, sr.Verdict, sr.RunIDs, sr.EndedAt)
	return err
}

func (s *Store) SuiteRuns(ctx context.Context, limit int) ([]api.SuiteRun, error) {
	rows, err := s.db.Query(ctx, `SELECT id, coalesce(suite_id, 0), suite_name, trigger, status, verdict, run_ids, started_at, ended_at FROM suite_runs ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.SuiteRun{}
	for rows.Next() {
		var sr api.SuiteRun
		if err := rows.Scan(&sr.ID, &sr.SuiteID, &sr.SuiteName, &sr.Trigger, &sr.Status, &sr.Verdict, &sr.RunIDs, &sr.StartedAt, &sr.EndedAt); err != nil {
			return nil, err
		}
		if sr.RunIDs == nil {
			sr.RunIDs = []int64{}
		}
		out = append(out, sr)
	}
	return out, rows.Err()
}

// Schedules.

func (s *Store) Schedules(ctx context.Context) ([]api.Schedule, error) {
	rows, err := s.db.Query(ctx, `SELECT sc.id, sc.name, sc.suite_id, su.name, sc.cron, sc.enabled, sc.last_run_at
		FROM schedules sc JOIN suites su ON su.id = sc.suite_id ORDER BY sc.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.Schedule{}
	for rows.Next() {
		var sc api.Schedule
		if err := rows.Scan(&sc.ID, &sc.Name, &sc.SuiteID, &sc.SuiteName, &sc.Cron, &sc.Enabled, &sc.LastRunAt); err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

func (s *Store) SaveSchedule(ctx context.Context, sc *api.Schedule) error {
	if sc.ID == 0 {
		return s.db.QueryRow(ctx, `INSERT INTO schedules (name, suite_id, cron, enabled) VALUES ($1, $2, $3, $4) RETURNING id`, sc.Name, sc.SuiteID, sc.Cron, sc.Enabled).Scan(&sc.ID)
	}
	tag, err := s.db.Exec(ctx, `UPDATE schedules SET name=$2, suite_id=$3, cron=$4, enabled=$5 WHERE id=$1`, sc.ID, sc.Name, sc.SuiteID, sc.Cron, sc.Enabled)
	if err == nil && tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return err
}

func (s *Store) TouchSchedule(ctx context.Context, id int64, at time.Time) {
	s.db.Exec(ctx, `UPDATE schedules SET last_run_at=$2 WHERE id=$1`, id, at)
}

func (s *Store) DeleteSchedule(ctx context.Context, id int64) error {
	_, err := s.db.Exec(ctx, `DELETE FROM schedules WHERE id=$1`, id)
	return err
}

// Alerts. OpenAlert is idempotent for an already-open (kind, host, port).

func (s *Store) OpenAlert(ctx context.Context, a api.Alert) (bool, error) {
	tag, err := s.db.Exec(ctx, `INSERT INTO alerts (kind, host, port, message)
		SELECT $1, $2, $3, $4 WHERE NOT EXISTS (SELECT 1 FROM alerts WHERE kind=$1 AND host=$2 AND port=$3 AND resolved_at IS NULL)`,
		a.Kind, a.Host, a.Port, a.Message)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// RenameAlertHost moves a host's open alerts to its new display name, so a
// rename neither strands them under the old name nor leaves them unresolvable.
func (s *Store) RenameAlertHost(ctx context.Context, old, new string) error {
	if old == new || old == "" || new == "" {
		return nil
	}
	_, err := s.db.Exec(ctx, `UPDATE alerts SET host=$2 WHERE host=$1 AND resolved_at IS NULL`, old, new)
	return err
}

func (s *Store) ResolveAlert(ctx context.Context, kind, host, port string) (bool, error) {
	tag, err := s.db.Exec(ctx, `UPDATE alerts SET resolved_at=now() WHERE kind=$1 AND host=$2 AND port=$3 AND resolved_at IS NULL`, kind, host, port)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// ClearAlert resolves one open alert by id (an operator action) and returns
// it; ok is false when no open alert has that id.
func (s *Store) ClearAlert(ctx context.Context, id int64) (api.Alert, bool, error) {
	var a api.Alert
	err := s.db.QueryRow(ctx, `UPDATE alerts SET resolved_at=now() WHERE id=$1 AND resolved_at IS NULL
		RETURNING id, kind, host, port, message, opened_at, resolved_at`, id).
		Scan(&a.ID, &a.Kind, &a.Host, &a.Port, &a.Message, &a.OpenedAt, &a.ResolvedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return a, false, nil
		}
		return a, false, err
	}
	return a, true, nil
}

// ClearAlerts resolves every open alert and returns them.
func (s *Store) ClearAlerts(ctx context.Context) ([]api.Alert, error) {
	rows, err := s.db.Query(ctx, `UPDATE alerts SET resolved_at=now() WHERE resolved_at IS NULL
		RETURNING id, kind, host, port, message, opened_at, resolved_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.Alert{}
	for rows.Next() {
		var a api.Alert
		if err := rows.Scan(&a.ID, &a.Kind, &a.Host, &a.Port, &a.Message, &a.OpenedAt, &a.ResolvedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) Alerts(ctx context.Context, openOnly bool, limit int) ([]api.Alert, error) {
	q := `SELECT id, kind, host, port, message, opened_at, resolved_at FROM alerts`
	if openOnly {
		q += ` WHERE resolved_at IS NULL`
	}
	rows, err := s.db.Query(ctx, q+` ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.Alert{}
	for rows.Next() {
		var a api.Alert
		if err := rows.Scan(&a.ID, &a.Kind, &a.Host, &a.Port, &a.Message, &a.OpenedAt, &a.ResolvedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
