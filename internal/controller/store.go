package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Realishbird/SR-Net-Speed-Test/internal/api"
)

const schema = `
CREATE TABLE IF NOT EXISTS agents (
	id          BIGSERIAL PRIMARY KEY,
	uid         TEXT UNIQUE NOT NULL,
	secret_hash TEXT UNIQUE NOT NULL,
	hostname    TEXT NOT NULL DEFAULT '',
	name        TEXT NOT NULL DEFAULT '',
	tags        TEXT[] NOT NULL DEFAULT '{}',
	site        TEXT NOT NULL DEFAULT '',
	rack        TEXT NOT NULL DEFAULT '',
	inventory   JSONB NOT NULL DEFAULT '{}',
	last_seen   TIMESTAMPTZ,
	created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS ports (
	id            BIGSERIAL PRIMARY KEY,
	agent_id      BIGINT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
	rdma_dev      TEXT NOT NULL,
	port_num      INT NOT NULL,
	info          JSONB NOT NULL DEFAULT '{}',
	label         TEXT NOT NULL DEFAULT '',
	fabric        TEXT NOT NULL DEFAULT '',
	expected_gbps DOUBLE PRECISION NOT NULL DEFAULT 0,
	last_seen     TIMESTAMPTZ,
	UNIQUE (agent_id, rdma_dev, port_num)
);
CREATE TABLE IF NOT EXISTS enroll_tokens (
	token_hash TEXT PRIMARY KEY,
	expires_at TIMESTAMPTZ NOT NULL,
	used_at    TIMESTAMPTZ
);
CREATE TABLE IF NOT EXISTS runs (
	id         BIGSERIAL PRIMARY KEY,
	name       TEXT NOT NULL DEFAULT '',
	spec       JSONB NOT NULL,
	status     TEXT NOT NULL,
	verdict    TEXT NOT NULL DEFAULT '',
	error      TEXT NOT NULL DEFAULT '',
	summary    JSONB NOT NULL DEFAULT '{}',
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	started_at TIMESTAMPTZ,
	ended_at   TIMESTAMPTZ
);
CREATE TABLE IF NOT EXISTS legs (
	id             BIGSERIAL PRIMARY KEY,
	run_id         BIGINT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
	idx            INT NOT NULL,
	client_port_id BIGINT REFERENCES ports(id) ON DELETE SET NULL,
	server_port_id BIGINT REFERENCES ports(id) ON DELETE SET NULL,
	detail         JSONB NOT NULL DEFAULT '{}',
	client_result  JSONB,
	server_result  JSONB
);
CREATE INDEX IF NOT EXISTS legs_run_idx ON legs (run_id);
CREATE TABLE IF NOT EXISTS audit (
	id     BIGSERIAL PRIMARY KEY,
	at     TIMESTAMPTZ NOT NULL DEFAULT now(),
	action TEXT NOT NULL,
	detail JSONB
);
`

type Store struct{ db *pgxpool.Pool }

func OpenStore(ctx context.Context, url string) (*Store, error) {
	db, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(ctx, schema+schema3); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	// Runs in flight when the controller stopped cannot be resumed.
	_, err = db.Exec(ctx, `UPDATE runs SET status='failed', error='controller restarted during run', ended_at=now()
		WHERE status IN ('queued','running')`)
	return &Store{db: db}, err
}

func (s *Store) Close() { s.db.Close() }

func (s *Store) Audit(ctx context.Context, action string, detail any) {
	b, _ := json.Marshal(detail)
	s.db.Exec(ctx, `INSERT INTO audit (action, detail) VALUES ($1, $2)`, action, b)
}

// Enrollment tokens.

func (s *Store) CreateToken(ctx context.Context, hash string, exp time.Time) error {
	_, err := s.db.Exec(ctx, `INSERT INTO enroll_tokens (token_hash, expires_at) VALUES ($1, $2)`, hash, exp)
	return err
}

func (s *Store) ConsumeToken(ctx context.Context, hash string) error {
	tag, err := s.db.Exec(ctx, `UPDATE enroll_tokens SET used_at=now()
		WHERE token_hash=$1 AND used_at IS NULL AND expires_at > now()`, hash)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errors.New("enrollment token is invalid, used, or expired")
	}
	return nil
}

// EnrollAgent creates an agent, or re-keys the existing one with the same
// hostname so a reinstall keeps its name, tags and port labels.
func (s *Store) EnrollAgent(ctx context.Context, uid, secretHash string, inv api.Inventory) (string, error) {
	invJSON, _ := json.Marshal(inv)
	var gotUID string
	err := s.db.QueryRow(ctx, `UPDATE agents SET secret_hash=$1, inventory=$2, last_seen=now()
		WHERE hostname=$3 RETURNING uid`, secretHash, invJSON, inv.Hostname).Scan(&gotUID)
	if err == nil {
		return gotUID, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	_, err = s.db.Exec(ctx, `INSERT INTO agents (uid, secret_hash, hostname, name, inventory, last_seen)
		VALUES ($1, $2, $3, $3, $4, now())`, uid, secretHash, inv.Hostname, invJSON)
	return uid, err
}

func (s *Store) AgentBySecret(ctx context.Context, secretHash string) (int64, error) {
	var id int64
	err := s.db.QueryRow(ctx, `SELECT id FROM agents WHERE secret_hash=$1`, secretHash).Scan(&id)
	return id, err
}

// SaveInventory records the latest inventory and syncs the port table.
// Ports the agent no longer reports are removed; labels on the rest are kept.
func (s *Store) SaveInventory(ctx context.Context, agentID int64, inv api.Inventory) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	invJSON, _ := json.Marshal(inv)
	if _, err := tx.Exec(ctx, `UPDATE agents SET inventory=$1, hostname=$2, last_seen=now() WHERE id=$3`, invJSON, inv.Hostname, agentID); err != nil {
		return err
	}
	var keep []int64
	for _, p := range inv.Ports {
		info, _ := json.Marshal(p)
		var id int64
		err := tx.QueryRow(ctx, `INSERT INTO ports (agent_id, rdma_dev, port_num, info, last_seen)
			VALUES ($1, $2, $3, $4, now())
			ON CONFLICT (agent_id, rdma_dev, port_num) DO UPDATE SET info=EXCLUDED.info, last_seen=now()
			RETURNING id`, agentID, p.RdmaDev, p.PortNum, info).Scan(&id)
		if err != nil {
			return err
		}
		keep = append(keep, id)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM ports WHERE agent_id=$1 AND NOT (id = ANY($2))`, agentID, keep); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) TouchAgent(ctx context.Context, agentID int64) {
	s.db.Exec(ctx, `UPDATE agents SET last_seen=now() WHERE id=$1`, agentID)
}

// hostRec is a host with its ports, as the orchestrator and API need them.
type hostRec struct {
	api.Host
	inv api.Inventory
}

func (s *Store) Hosts(ctx context.Context) ([]hostRec, error) {
	rows, err := s.db.Query(ctx, `SELECT id, uid, name, hostname, tags, site, rack, inventory, coalesce(last_seen, 'epoch') FROM agents ORDER BY lower(name), id`)
	if err != nil {
		return nil, err
	}
	var hosts []hostRec
	byID := map[int64]int{}
	for rows.Next() {
		var h hostRec
		var inv []byte
		if err := rows.Scan(&h.ID, &h.UID, &h.Name, &h.Hostname, &h.Tags, &h.Site, &h.Rack, &inv, &h.LastSeen); err != nil {
			rows.Close()
			return nil, err
		}
		json.Unmarshal(inv, &h.inv)
		h.Arch, h.OS, h.AgentVersion = h.inv.Arch, h.inv.OS, h.inv.AgentVersion
		h.Ports = []api.HostPort{}
		byID[h.ID] = len(hosts)
		hosts = append(hosts, h)
	}
	rows.Close()
	prow, err := s.db.Query(ctx, `SELECT id, agent_id, info, label, fabric, expected_gbps FROM ports ORDER BY agent_id, rdma_dev, port_num`)
	if err != nil {
		return nil, err
	}
	defer prow.Close()
	for prow.Next() {
		var hp api.HostPort
		var agentID int64
		var info []byte
		if err := prow.Scan(&hp.ID, &agentID, &info, &hp.Label, &hp.Fabric, &hp.ExpectedGbps); err != nil {
			return nil, err
		}
		var p api.Port
		json.Unmarshal(info, &p)
		hp.RdmaDev, hp.PortNum, hp.Netdev, hp.Addrs = p.RdmaDev, p.PortNum, p.Netdev, p.Addrs
		hp.State, hp.SpeedMbps, hp.FWVersion, hp.Transceiver = p.State, p.SpeedMbps, p.FWVersion, p.Transceiver
		hp.LLDPSwitch, hp.LLDPPort = p.LLDPSwitch, p.LLDPPort
		if i, ok := byID[agentID]; ok {
			hosts[i].Ports = append(hosts[i].Ports, hp)
		}
	}
	return hosts, prow.Err()
}

func (s *Store) UpdateHost(ctx context.Context, id int64, u api.HostUpdate) error {
	tag, err := s.db.Exec(ctx, `UPDATE agents SET
		name = coalesce($2, name), tags = coalesce($3, tags), site = coalesce($4, site), rack = coalesce($5, rack)
		WHERE id=$1`, id, u.Name, u.Tags, u.Site, u.Rack)
	if err == nil && tag.RowsAffected() == 0 {
		err = pgx.ErrNoRows
	}
	return err
}

func (s *Store) DeleteHost(ctx context.Context, id int64) error {
	_, err := s.db.Exec(ctx, `DELETE FROM agents WHERE id=$1`, id)
	return err
}

func (s *Store) UpdatePort(ctx context.Context, id int64, u api.PortUpdate) error {
	tag, err := s.db.Exec(ctx, `UPDATE ports SET
		label = coalesce($2, label), fabric = coalesce($3, fabric), expected_gbps = coalesce($4, expected_gbps)
		WHERE id=$1`, id, u.Label, u.Fabric, u.ExpectedGbps)
	if err == nil && tag.RowsAffected() == 0 {
		err = pgx.ErrNoRows
	}
	return err
}

// Runs.

func (s *Store) CreateRun(ctx context.Context, spec api.RunSpec, legs []api.Leg, portIDs [][2]int64) (int64, []int64, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return 0, nil, err
	}
	defer tx.Rollback(ctx)
	specJSON, _ := json.Marshal(spec)
	var runID int64
	if err := tx.QueryRow(ctx, `INSERT INTO runs (name, spec, status) VALUES ($1, $2, 'queued') RETURNING id`, spec.Name, specJSON).Scan(&runID); err != nil {
		return 0, nil, err
	}
	ids := make([]int64, len(legs))
	for i, l := range legs {
		d, _ := json.Marshal(l)
		if err := tx.QueryRow(ctx, `INSERT INTO legs (run_id, idx, client_port_id, server_port_id, detail) VALUES ($1, $2, $3, $4, $5) RETURNING id`,
			runID, l.Index, portIDs[i][0], portIDs[i][1], d).Scan(&ids[i]); err != nil {
			return 0, nil, err
		}
	}
	return runID, ids, tx.Commit(ctx)
}

func (s *Store) SetRunStatus(ctx context.Context, id int64, status string) error {
	q := `UPDATE runs SET status=$2 WHERE id=$1`
	if status == "running" {
		q = `UPDATE runs SET status=$2, started_at=now() WHERE id=$1`
	}
	_, err := s.db.Exec(ctx, q, id, status)
	return err
}

func (s *Store) FinishRun(ctx context.Context, id int64, status, verdict, errMsg string, sum api.RunSummary) error {
	b, _ := json.Marshal(sum)
	_, err := s.db.Exec(ctx, `UPDATE runs SET status=$2, verdict=$3, error=$4, summary=$5, ended_at=now() WHERE id=$1`, id, status, verdict, errMsg, b)
	return err
}

func (s *Store) SaveLeg(ctx context.Context, l api.Leg, client, server *api.LegResult) error {
	d, _ := json.Marshal(l)
	c, _ := json.Marshal(client)
	sv, _ := json.Marshal(server)
	_, err := s.db.Exec(ctx, `UPDATE legs SET detail=$2, client_result=$3, server_result=$4 WHERE id=$1`, l.ID, d, c, sv)
	return err
}

func (s *Store) Runs(ctx context.Context, limit int) ([]api.Run, error) {
	rows, err := s.db.Query(ctx, `SELECT id, name, spec, status, verdict, error, summary, created_at, started_at, ended_at
		FROM runs ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []api.Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) Run(ctx context.Context, id int64) (api.Run, error) {
	r, err := scanRun(s.db.QueryRow(ctx, `SELECT id, name, spec, status, verdict, error, summary, created_at, started_at, ended_at
		FROM runs WHERE id=$1`, id))
	if err != nil {
		return r, err
	}
	rows, err := s.db.Query(ctx, `SELECT id, detail FROM legs WHERE run_id=$1 ORDER BY idx`, id)
	if err != nil {
		return r, err
	}
	defer rows.Close()
	for rows.Next() {
		var l api.Leg
		var d []byte
		var legID int64
		if err := rows.Scan(&legID, &d); err != nil {
			return r, err
		}
		json.Unmarshal(d, &l)
		l.ID = legID
		r.Legs = append(r.Legs, l)
	}
	return r, rows.Err()
}

func scanRun(row pgx.Row) (api.Run, error) {
	var r api.Run
	var spec, sum []byte
	err := row.Scan(&r.ID, &r.Name, &spec, &r.Status, &r.Verdict, &r.Error, &sum, &r.CreatedAt, &r.StartedAt, &r.EndedAt)
	json.Unmarshal(spec, &r.Spec)
	json.Unmarshal(sum, &r.Summary)
	return r, err
}
