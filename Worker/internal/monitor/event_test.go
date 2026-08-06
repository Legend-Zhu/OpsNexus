package monitor

import (
	"context"
	"testing"
	"time"
)

func TestEventStoreAddList(t *testing.T) {
	s := mustOpenEventStore(t)
	defer s.Close()
	s.Add(Event{Service: "a", Type: EventPortDown, Level: LevelError, Msg: "down"})
	s.Add(Event{Service: "a", Type: EventHTTPUnhealthy, Level: LevelWarn, Msg: "5xx"})
	s.Add(Event{Service: "b", Type: EventResourceOver, Level: LevelWarn, Msg: "mem"})

	list := s.List("", "", 0)
	if len(list) != 3 {
		t.Fatalf("want 3 events, got %d", len(list))
	}
	// newest first
	if list[0].Type != EventResourceOver {
		t.Errorf("newest first: got %s", list[0].Type)
	}
	filtered := s.List("a", "", 0)
	if len(filtered) != 2 {
		t.Errorf("filter by service failed: got %d", len(filtered))
	}
	byType := s.List("", EventPortDown, 0)
	if len(byType) != 1 || byType[0].Type != EventPortDown {
		t.Errorf("filter by type failed: %+v", byType)
	}
}

func TestEventStoreSince(t *testing.T) {
	s := mustOpenEventStore(t)
	defer s.Close()
	e1 := s.Add(Event{Service: "a", Type: EventPortDown, Level: LevelError})
	e2 := s.Add(Event{Service: "a", Type: EventHTTPUnhealthy, Level: LevelWarn})
	s.Add(Event{Service: "a", Type: EventResourceOver, Level: LevelWarn})

	got := s.Since(e1.Seq, 0)
	if len(got) != 2 {
		t.Fatalf("Since(seq1) want 2, got %d", len(got))
	}
	if got[0].Seq != e2.Seq {
		t.Errorf("Since oldest-first: first seq = %d, want %d", got[0].Seq, e2.Seq)
	}
}

func TestEventStoreAckGC(t *testing.T) {
	s := mustOpenEventStore(t)
	defer s.Close()
	e := s.Add(Event{Service: "a", Type: EventPortDown, Level: LevelError, TS: time.Now().Add(-2 * time.Hour)})
	if err := s.Ack(e.Seq); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	if err := s.GC(time.Hour); err != nil {
		t.Fatalf("GC: %v", err)
	}
	if got := len(s.List("", "", 0)); got != 0 {
		t.Errorf("GC should remove old acked event, got %d", got)
	}
}

func TestEventStoreWaitNew(t *testing.T) {
	s := mustOpenEventStore(t)
	defer s.Close()
	e1 := s.Add(Event{Service: "a", Type: EventPortDown, Level: LevelError})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// Immediate delivery of e1.
	evs, err := s.WaitNew(ctx, 0)
	if err != nil {
		t.Fatalf("WaitNew: %v", err)
	}
	if len(evs) != 1 || evs[0].Seq != e1.Seq {
		t.Fatalf("WaitNew delivered wrong set: %+v", evs)
	}

	// Blocks until a new event is added.
	go func() {
		time.Sleep(100 * time.Millisecond)
		s.Add(Event{Service: "a", Type: EventHTTPUnhealthy, Level: LevelWarn})
	}()
	evs2, err := s.WaitNew(ctx, e1.Seq)
	if err != nil {
		t.Fatalf("WaitNew 2: %v", err)
	}
	if len(evs2) != 1 || evs2[0].Type != EventHTTPUnhealthy {
		t.Fatalf("WaitNew 2 delivered wrong set: %+v", evs2)
	}
}

func TestEventStoreWaitNewCtxCancel(t *testing.T) {
	s := mustOpenEventStore(t)
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := s.WaitNew(ctx, 0)
	if err == nil {
		t.Fatal("WaitNew with cancelled ctx and no events should return ctx error")
	}
}

func mustOpenEventStore(t *testing.T) *EventStore {
	t.Helper()
	s, err := NewEventStore(":memory:", nil)
	if err != nil {
		t.Fatalf("open event store: %v", err)
	}
	return s
}
