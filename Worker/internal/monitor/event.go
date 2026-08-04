// Package monitor implements the P2 health monitoring subsystem: port
// connectivity, HTTP liveness, log-pattern matching, and resource-usage
// thresholds, driven by each service's config.Monitoring block. Events land in
// an in-memory EventStore (ring buffer) and can be queried via the HTTP API or
// pushed to a webhook.
package monitor

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// EventType identifies the kind of monitoring event.
type EventType string

const (
	EventPortDown       EventType = "port_down"
	EventHTTPUnhealthy  EventType = "http_unhealthy"
	EventLogMatch       EventType = "log_match"
	EventResourceOver   EventType = "resource_over"
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
	ID      string    `json:"id"`
	TS      time.Time `json:"ts"`
	Service string    `json:"service"`
	Type    EventType `json:"type"`
	Level   Level     `json:"level"`
	Msg     string    `json:"msg"`
	Detail  string    `json:"detail,omitempty"`
}

// EventStore is a thread-safe ring buffer of events.
type EventStore struct {
	mu    sync.Mutex
	evs   []Event // circular buffer
	next  int
	count int
	cap   int

	// sinks receive a copy of every added event (used by the webhook pusher).
	// They are invoked synchronously in Add; push implementations must not
	// block (spawn their own goroutines / queues).
	sinks []func(Event)
}

// NewEventStore creates a store holding up to cap events (oldest evicted).
func NewEventStore(cap int) *EventStore {
	if cap <= 0 {
		cap = 10000
	}
	return &EventStore{evs: make([]Event, cap), cap: cap}
}

// AddSink registers a callback invoked with every added event.
func (s *EventStore) AddSink(fn func(Event)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sinks = append(s.sinks, fn)
}

// Add appends an event, assigning id and timestamp if empty, and notifies sinks.
func (s *EventStore) Add(e Event) Event {
	if e.ID == "" {
		e.ID = newEventID()
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
	sinks := make([]func(Event), len(s.sinks))
	copy(sinks, s.sinks)
	s.mu.Unlock()

	for _, fn := range sinks {
		fn(e)
	}
	return e
}

// List returns events newest-first, optionally filtered by service and type.
// limit <= 0 returns all matching.
func (s *EventStore) List(service string, typ EventType, limit int) []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Event, 0, s.count)
	// iterate oldest → newest, then reverse for newest-first
	for i := 0; i < s.count; i++ {
		idx := (s.next - s.count + i + s.cap) % s.cap
		e := s.evs[idx]
		if service != "" && e.Service != service {
			continue
		}
		if typ != "" && e.Type != typ {
			continue
		}
		out = append(out, e)
	}
	// reverse
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
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
