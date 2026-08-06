// Package monitor implements the P2 health monitoring subsystem: port
// connectivity, HTTP liveness, log-pattern matching, and resource-usage
// thresholds, driven by each service's config.Monitoring block. Events land in
// a SQLite-backed EventStore (persistent, survives restarts) and can be queried
// via the HTTP API or streamed over gRPC (SubscribeEvents).
//
// The store is a durable queue: every event gets a monotonic sequence number,
// and subscribers resume from a cursor. Acknowledged entries are eligible for
// garbage collection. This replaces the in-memory ring buffer + webhook push
// model — events are no longer lost on restart or under event storms (up to
// disk capacity), and the network policy (server → worker only) is honored
// because the server opens the subscription stream.
package monitor

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (no CGO; matches CGO_ENABLED=0 build)
)

// EventType identifies the kind of monitoring event.
type EventType string

const (
	EventPortDown        EventType = "port_down"
	EventHTTPUnhealthy   EventType = "http_unhealthy"
	EventLogMatch        EventType = "log_match"
	EventResourceOver    EventType = "resource_over"
	EventResourceRecover EventType = "resource_recovered"
)

// Level is the severity of an event.
type Level string

const (
	LevelInfo  Level = "info"
	LevelWarn  Level = "warn"
	LevelError Level = "error"
)

// Event is a single monitoring observation.
type Event struct {
	Seq    int64     `json:"seq,omitempty"` // monotonic sequence (assigned on Add)
	ID     string    `json:"id"`
	TS     time.Time `json:"ts"`
	Service string   `json:"service"`
	Type   EventType `json:"type"`
	Level  Level     `json:"level"`
	Msg    string    `json:"msg"`
	Detail string    `json:"detail,omitempty"`
}

// EventStore is a SQLite-backed durable event queue. It preserves the original
// in-memory API (Add / List / AddSink) so monitor checks and the Manager are
// unchanged, and adds sequence-cursor + ack methods for gRPC subscription.
type EventStore struct {
	db  *sql.DB
	log *slog.Logger

	mu    sync.Mutex // guards sinks slice + cond
	cond  *sync.Cond
	sinks []func(Event)
}

// NewEventStore opens (or creates) a SQLite event store at path. The schema is
// initialized idempotently. Pass ":memory:" for tests.
func NewEventStore(path string, log *slog.Logger) (*EventStore, error) {
	if log == nil {
		log = slog.Default()
	}
	// busy_timeout lets concurrent writers (checks) wait briefly instead of
	// failing; WAL improves read concurrency. _txlock=immediate avoids
	// upgrade-deadlock on the write transaction.
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_txlock=immediate", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open event store %s: %w", path, err)
	}
	// Single writer connection serializes inserts (SQLite write lock).
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping event store %s: %w", path, err)
	}
	s := &EventStore{db: db, log: log}
	s.cond = sync.NewCond(&s.mu)
	if err := s.initSchema(); err != nil {
		db.Close()
		return nil, fmt.Errorf("init event store schema: %w", err)
	}
	return s, nil
}

// Close releases the underlying database handle.
func (s *EventStore) Close() error {
	return s.db.Close()
}

func (s *EventStore) initSchema() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS events (
	seq    INTEGER PRIMARY KEY AUTOINCREMENT,
	id     TEXT    NOT NULL,
	ts     TEXT    NOT NULL,
	service TEXT,
	type   TEXT,
	level  TEXT,
	msg    TEXT,
	detail TEXT,
	acked  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_events_ts ON events(ts);
`)
	return err
}

// AddSink registers a callback invoked with every added event.
func (s *EventStore) AddSink(fn func(Event)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sinks = append(s.sinks, fn)
}

// Add appends an event (assigning id, ts and seq), notifies sinks and wakes
// any blocked WaitNew callers. It returns the stored event with Seq populated.
func (s *EventStore) Add(e Event) Event {
	if e.ID == "" {
		e.ID = newEventID()
	}
	if e.TS.IsZero() {
		e.TS = time.Now().UTC()
	}
	_, err := s.db.Exec(
		`INSERT INTO events (id, ts, service, type, level, msg, detail) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.TS.UTC().Format(time.RFC3339Nano), e.Service, string(e.Type), string(e.Level), e.Msg, e.Detail,
	)
	if err != nil {
		// Persisting events must not crash monitoring. Log and return the
		// event anyway (callers ignore the error to keep check loops alive).
		s.log.Error("event store insert failed", "err", err, "event", e.ID)
		return e
	}
	// Read back the assigned sequence.
	row := s.db.QueryRow(`SELECT seq FROM events WHERE id = ?`, e.ID)
	if err := row.Scan(&e.Seq); err != nil {
		s.log.Error("event store read-back seq failed", "err", err, "event", e.ID)
	}

	// Notify sinks + wake WaitNew.
	s.mu.Lock()
	sinks := make([]func(Event), len(s.sinks))
	copy(sinks, s.sinks)
	s.cond.Broadcast()
	s.mu.Unlock()

	for _, fn := range sinks {
		fn(e)
	}
	return e
}

// List returns events newest-first, optionally filtered by service and type.
// limit <= 0 returns all matching.
func (s *EventStore) List(service string, typ EventType, limit int) []Event {
	q := `SELECT seq, id, ts, service, type, level, msg, detail FROM events`
	var args []any
	where := ""
	if service != "" {
		where = " WHERE service = ?"
		args = append(args, service)
	}
	if typ != "" {
		if where == "" {
			where = " WHERE type = ?"
		} else {
			where += " AND type = ?"
		}
		args = append(args, string(typ))
	}
	q += where + " ORDER BY seq DESC"
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		s.log.Error("event store list failed", "err", err)
		return nil
	}
	defer rows.Close()
	return scanEvents(rows)
}

// Since returns up to limit events with seq > after, oldest-first (subscription
// replay order). limit <= 0 applies a default cap of 1000 to bound replay.
func (s *EventStore) Since(after int64, limit int) []Event {
	if limit <= 0 {
		limit = 1000
	}
	rows, err := s.db.Query(
		`SELECT seq, id, ts, service, type, level, msg, detail FROM events WHERE seq > ? ORDER BY seq ASC LIMIT ?`,
		after, limit,
	)
	if err != nil {
		s.log.Error("event store since failed", "err", err)
		return nil
	}
	defer rows.Close()
	return scanEvents(rows)
}

// LastSeq returns the highest seq currently stored, or 0 when empty.
func (s *EventStore) LastSeq() int64 {
	var seq int64
	err := s.db.QueryRow(`SELECT COALESCE(MAX(seq), 0) FROM events`).Scan(&seq)
	if err != nil {
		s.log.Error("event store lastseq failed", "err", err)
		return 0
	}
	return seq
}

// Ack marks seq (and everything before it) as acknowledged, enabling GC.
func (s *EventStore) Ack(seq int64) error {
	_, err := s.db.Exec(`UPDATE events SET acked = 1 WHERE seq <= ?`, seq)
	return err
}

// GC deletes acknowledged events older than maxAge. Call periodically.
func (s *EventStore) GC(maxAge time.Duration) error {
	cutoff := time.Now().UTC().Add(-maxAge).Format(time.RFC3339Nano)
	_, err := s.db.Exec(`DELETE FROM events WHERE acked = 1 AND ts < ?`, cutoff)
	return err
}

// WaitNew blocks until a new event with seq > lastSeen is available or ctx is
// done. Returns the events to deliver (oldest-first) and the new cursor. Used
// by the gRPC SubscribeEvents handler to avoid busy-polling.
func (s *EventStore) WaitNew(ctx context.Context, lastSeen int64) ([]Event, error) {
	// Fast path: events already pending.
	if evs := s.Since(lastSeen, 100); len(evs) > 0 {
		return evs, nil
	}
	// Watch ctx cancellation in a goroutine that broadcasts the cond, so the
	// blocked wait returns promptly on disconnect/shutdown.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			s.mu.Lock()
			s.cond.Broadcast() // wake the Wait
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
		if s.Since(lastSeen, 1) != nil { // something new arrived
			s.mu.Unlock()
			return s.Since(lastSeen, 100), nil
		}
		s.cond.Wait()
	}
}

func scanEvents(rows *sql.Rows) []Event {
	var out []Event
	for rows.Next() {
		var e Event
		var ts, service, typ, level sql.NullString
		if err := rows.Scan(&e.Seq, &e.ID, &ts, &service, &typ, &level, &e.Msg, &e.Detail); err != nil {
			continue
		}
		if t, err := time.Parse(time.RFC3339Nano, ts.String); err == nil {
			e.TS = t
		}
		e.Service = service.String
		e.Type = EventType(typ.String)
		e.Level = Level(level.String)
		out = append(out, e)
	}
	return out
}

func newEventID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return hex.EncodeToString([]byte(time.Now().Format("150405.000000000")))
	}
	return hex.EncodeToString(b[:])
}
