// Package audit records every sensitive operation (service lifecycle changes
// and command executions) with the caller identity and the outcome, for
// traceability and forensics. Entries are kept in a SQLite-backed store
// (queryable via GET /api/v1/audit and streamed over gRPC SubscribeAudit) and
// persisted across restarts.
package audit

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Action enumerates the audited operations.
type Action string

const (
	ActionDeploy   Action = "deploy"
	ActionUpdate   Action = "update"
	ActionScale    Action = "scale"
	ActionRestart  Action = "restart"
	ActionRemove   Action = "remove"
	ActionExec     Action = "exec_in_container"
	ActionHostExec Action = "exec_host_command"
)

// Entry is a single audit record.
type Entry struct {
	Seq     int64     `json:"seq,omitempty"` // monotonic sequence (assigned on Add)
	ID      string    `json:"id"`
	TS      time.Time `json:"ts"`
	Actor   string    `json:"actor"` // caller identity (token name / remote addr)
	Action  Action    `json:"action"`
	Service string    `json:"service,omitempty"`
	Command string    `json:"command,omitempty"` // exec/host command, truncated
	Target  string    `json:"target,omitempty"`  // node / container / replica
	OK      bool      `json:"ok"`
	Detail  string    `json:"detail,omitempty"`
}

// Store is a SQLite-backed durable audit store.
type Store struct {
	db  *sql.DB
	log *slog.Logger

	mu    sync.Mutex
	cond  *sync.Cond
	sinks []func(Entry)
}

// NewStore opens (or creates) a SQLite audit store at path. Pass ":memory:" for
// tests.
func NewStore(path string, log *slog.Logger) (*Store, error) {
	if log == nil {
		log = slog.Default()
	}
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_txlock=immediate", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open audit store %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping audit store %s: %w", path, err)
	}
	s := &Store{db: db, log: log}
	s.cond = sync.NewCond(&s.mu)
	if err := s.initSchema(); err != nil {
		db.Close()
		return nil, fmt.Errorf("init audit store schema: %w", err)
	}
	return s, nil
}

// Close releases the underlying database handle.
func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) initSchema() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS audit (
	seq     INTEGER PRIMARY KEY AUTOINCREMENT,
	id      TEXT    NOT NULL,
	ts      TEXT    NOT NULL,
	actor   TEXT,
	action  TEXT,
	service TEXT,
	command TEXT,
	target  TEXT,
	ok      INTEGER NOT NULL DEFAULT 0,
	detail  TEXT,
	acked   INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_audit_ts ON audit(ts);
`)
	return err
}

// AddSink registers a callback invoked with every new entry.
func (s *Store) AddSink(fn func(Entry)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sinks = append(s.sinks, fn)
}

// Add appends an entry (assigning id/ts/seq) and notifies sinks.
func (s *Store) Add(e Entry) Entry {
	if e.ID == "" {
		e.ID = newID()
	}
	if e.TS.IsZero() {
		e.TS = time.Now().UTC()
	}
	ok := 0
	if e.OK {
		ok = 1
	}
	_, err := s.db.Exec(
		`INSERT INTO audit (id, ts, actor, action, service, command, target, ok, detail) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.TS.UTC().Format(time.RFC3339Nano), e.Actor, string(e.Action), e.Service, e.Command, e.Target, ok, e.Detail,
	)
	if err != nil {
		s.log.Error("audit store insert failed", "err", err, "entry", e.ID)
		return e
	}
	row := s.db.QueryRow(`SELECT seq FROM audit WHERE id = ?`, e.ID)
	if err := row.Scan(&e.Seq); err != nil {
		s.log.Error("audit store read-back seq failed", "err", err, "entry", e.ID)
	}

	s.mu.Lock()
	sinks := make([]func(Entry), len(s.sinks))
	copy(sinks, s.sinks)
	s.cond.Broadcast()
	s.mu.Unlock()

	for _, fn := range sinks {
		fn(e)
	}
	return e
}

// List returns entries newest-first, optionally filtered by action.
// limit <= 0 returns all matching.
func (s *Store) List(action Action, limit int) []Entry {
	q := `SELECT seq, id, ts, actor, action, service, command, target, ok, detail FROM audit`
	var args []any
	if action != "" {
		q += " WHERE action = ?"
		args = append(args, string(action))
	}
	q += " ORDER BY seq DESC"
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		s.log.Error("audit store list failed", "err", err)
		return nil
	}
	defer rows.Close()
	return scanEntries(rows)
}

// Since returns up to limit entries with seq > after, oldest-first.
func (s *Store) Since(after int64, limit int) []Entry {
	if limit <= 0 {
		limit = 1000
	}
	rows, err := s.db.Query(
		`SELECT seq, id, ts, actor, action, service, command, target, ok, detail FROM audit WHERE seq > ? ORDER BY seq ASC LIMIT ?`,
		after, limit,
	)
	if err != nil {
		s.log.Error("audit store since failed", "err", err)
		return nil
	}
	defer rows.Close()
	return scanEntries(rows)
}

// LastSeq returns the highest seq currently stored, or 0 when empty.
func (s *Store) LastSeq() int64 {
	var seq int64
	err := s.db.QueryRow(`SELECT COALESCE(MAX(seq), 0) FROM audit`).Scan(&seq)
	if err != nil {
		s.log.Error("audit store lastseq failed", "err", err)
		return 0
	}
	return seq
}

// Ack marks seq (and everything before it) as acknowledged, enabling GC.
func (s *Store) Ack(seq int64) error {
	_, err := s.db.Exec(`UPDATE audit SET acked = 1 WHERE seq <= ?`, seq)
	return err
}

// GC deletes acknowledged entries older than maxAge.
func (s *Store) GC(maxAge time.Duration) error {
	cutoff := time.Now().UTC().Add(-maxAge).Format(time.RFC3339Nano)
	_, err := s.db.Exec(`DELETE FROM audit WHERE acked = 1 AND ts < ?`, cutoff)
	return err
}

// WaitNew blocks until a new entry with seq > lastSeen is available or ctx is
// done. Used by the gRPC SubscribeAudit handler.
func (s *Store) WaitNew(ctx context.Context, lastSeen int64) ([]Entry, error) {
	if evs := s.Since(lastSeen, 100); len(evs) > 0 {
		return evs, nil
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			s.mu.Lock()
			s.cond.Broadcast()
			s.mu.Unlock()
		case <-done:
		}
	}()
	s.mu.Lock()
	for {
		if ctx.Err() != nil {
			s.mu.Unlock()
			return nil, ctx.Err()
		}
		if s.Since(lastSeen, 1) != nil {
			s.mu.Unlock()
			return s.Since(lastSeen, 100), nil
		}
		s.cond.Wait()
	}
}

func scanEntries(rows *sql.Rows) []Entry {
	var out []Entry
	for rows.Next() {
		var e Entry
		var ts, actor, action, service, command, target, detail sql.NullString
		var ok int
		if err := rows.Scan(&e.Seq, &e.ID, &ts, &actor, &action, &service, &command, &target, &ok, &detail); err != nil {
			continue
		}
		if t, err := time.Parse(time.RFC3339Nano, ts.String); err == nil {
			e.TS = t
		}
		e.Actor = actor.String
		e.Action = Action(action.String)
		e.Service = service.String
		e.Command = command.String
		e.Target = target.String
		e.OK = ok == 1
		e.Detail = detail.String
		out = append(out, e)
	}
	return out
}

func newID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return hex.EncodeToString([]byte(time.Now().Format("150405.000000000")))
	}
	return hex.EncodeToString(b[:])
}

// ---- actor context ----

type actorKey struct{}

// ContextWithActor stores the caller identity (token name / remote addr) in
// the context for audit logging. Set by the auth middleware.
func ContextWithActor(ctx context.Context, actor string) context.Context {
	return context.WithValue(ctx, actorKey{}, actor)
}

// ActorFromContext returns the stored actor, or "anonymous" when unset.
func ActorFromContext(ctx context.Context) string {
	if a, ok := ctx.Value(actorKey{}).(string); ok && a != "" {
		return a
	}
	return "anonymous"
}
