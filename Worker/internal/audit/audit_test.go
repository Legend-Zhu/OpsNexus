package audit

import (
	"context"
	"testing"
	"time"
)

func TestStoreAddList(t *testing.T) {
	s := mustOpenStore(t)
	defer s.Close()
	s.Add(Entry{Action: ActionDeploy, Service: "a", OK: true})
	s.Add(Entry{Action: ActionScale, Service: "a", OK: true})
	s.Add(Entry{Action: ActionRemove, Service: "a", OK: true})

	list := s.List("", 0)
	if len(list) != 3 {
		t.Fatalf("want 3 entries, got %d", len(list))
	}
	// newest first (highest seq first)
	if list[0].Action != ActionRemove {
		t.Errorf("newest first: got %s", list[0].Action)
	}
	filtered := s.List(ActionScale, 0)
	if len(filtered) != 1 || filtered[0].Action != ActionScale {
		t.Errorf("filter by action failed: %+v", filtered)
	}
}

func TestStoreSinceAck(t *testing.T) {
	s := mustOpenStore(t)
	defer s.Close()
	e1 := s.Add(Entry{Action: ActionDeploy, Service: "a", OK: true})
	e2 := s.Add(Entry{Action: ActionScale, Service: "a", OK: true})
	s.Add(Entry{Action: ActionRemove, Service: "a", OK: true})

	// Since(e1.Seq) returns e2, e3 oldest-first.
	got := s.Since(e1.Seq, 0)
	if len(got) != 2 {
		t.Fatalf("Since(seq1) want 2, got %d", len(got))
	}
	if got[0].Action != ActionScale {
		t.Errorf("Since oldest-first: got %s", got[0].Action)
	}

	// Ack up to e2; GC should still keep them (not old enough).
	if err := s.Ack(e2.Seq); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	if err := s.GC(time.Minute); err != nil {
		t.Fatalf("GC: %v", err)
	}
	if got := len(s.List("", 0)); got != 3 {
		t.Errorf("GC with short maxAge should keep all, got %d", got)
	}
}

func TestStoreGCOldAcked(t *testing.T) {
	s := mustOpenStore(t)
	defer s.Close()
	e := s.Add(Entry{Action: ActionDeploy, Service: "a", OK: true, TS: time.Now().Add(-2 * time.Hour)})
	if err := s.Ack(e.Seq); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	if err := s.GC(time.Hour); err != nil {
		t.Fatalf("GC: %v", err)
	}
	if got := len(s.List("", 0)); got != 0 {
		t.Errorf("GC should remove old acked entry, got %d", got)
	}
}

func TestStoreWaitNew(t *testing.T) {
	s := mustOpenStore(t)
	defer s.Close()
	e1 := s.Add(Entry{Action: ActionDeploy, Service: "a", OK: true})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// Deliver e1 immediately (already present).
	evs, err := s.WaitNew(ctx, 0)
	if err != nil {
		t.Fatalf("WaitNew: %v", err)
	}
	if len(evs) != 1 || evs[0].Seq != e1.Seq {
		t.Fatalf("WaitNew delivered wrong set: %+v", evs)
	}

	// No new events → should block. Spawn an Add to wake it.
	go func() {
		time.Sleep(100 * time.Millisecond)
		s.Add(Entry{Action: ActionScale, Service: "a", OK: true})
	}()
	evs2, err := s.WaitNew(ctx, e1.Seq)
	if err != nil {
		t.Fatalf("WaitNew 2: %v", err)
	}
	if len(evs2) != 1 || evs2[0].Action != ActionScale {
		t.Fatalf("WaitNew 2 delivered wrong set: %+v", evs2)
	}
}

func TestActorContext(t *testing.T) {
	ctx := ContextWithActor(context.Background(), "ops-token")
	if got := ActorFromContext(ctx); got != "ops-token" {
		t.Fatalf("actor = %q", got)
	}
	if got := ActorFromContext(context.Background()); got != "anonymous" {
		t.Fatalf("default actor = %q", got)
	}
}

func mustOpenStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(":memory:", nil)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return s
}
