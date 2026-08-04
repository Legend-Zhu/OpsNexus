// Package audit records every sensitive operation (service lifecycle changes
// and command executions) with the caller identity and the outcome, for
// traceability and forensics. Entries are kept in a ring buffer (queryable
// via GET /api/v1/audit) and can be forwarded to webhooks alongside monitor
// events.
package audit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
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
	ID        string    `json:"id"`
	TS        time.Time `json:"ts"`
	Actor     string    `json:"actor"`     // caller identity (token name / remote addr)
	Action    Action    `json:"action"`
	Service   string    `json:"service,omitempty"`
	Command   string    `json:"command,omitempty"` // exec/host command, truncated
	Target    string    `json:"target,omitempty"`  // node / container / replica
	OK        bool      `json:"ok"`
	Detail    string    `json:"detail,omitempty"`
}

// Store is a thread-safe ring buffer of audit entries.
type Store struct {
	mu    sync.Mutex
	evs   []Entry
	next  int
	count int
	cap   int

	sinks []func(Entry)
}

// NewStore creates a store holding up to cap entries.
func NewStore(cap int) *Store {
	if cap <= 0 {
		cap = 5000
	}
	return &Store{evs: make([]Entry, cap), cap: cap}
}

// AddSink registers a callback invoked with every new entry (webhook forwarding).
func (s *Store) AddSink(fn func(Entry)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sinks = append(s.sinks, fn)
}

// Add appends an entry (assigning id/ts) and notifies sinks.
func (s *Store) Add(e Entry) Entry {
	if e.ID == "" {
		e.ID = newID()
	}
	if e.TS.IsZero() {
		e.TS = time.Now().UTC()
	}
	s.mu.Lock()
	s.evs[s.next] = e
	s.next = (s.next + 1) % s.cap
	if s.count < s.cap {
		s.count++
	}
	sinks := make([]func(Entry), len(s.sinks))
	copy(sinks, s.sinks)
	s.mu.Unlock()

	for _, fn := range sinks {
		fn(e)
	}
	return e
}

// List returns entries newest-first, optionally filtered.
func (s *Store) List(action Action, limit int) []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Entry, 0, s.count)
	for i := 0; i < s.count; i++ {
		idx := (s.next - s.count + i + s.cap) % s.cap
		e := s.evs[idx]
		if action != "" && e.Action != action {
			continue
		}
		out = append(out, e)
	}
	// reverse → newest first
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
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
