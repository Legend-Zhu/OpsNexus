package audit

import (
	"context"
	"testing"
)

func TestStoreAddList(t *testing.T) {
	s := NewStore(10)
	s.Add(Entry{Action: ActionDeploy, Service: "a", OK: true})
	s.Add(Entry{Action: ActionScale, Service: "a", OK: true})
	s.Add(Entry{Action: ActionRemove, Service: "a", OK: true})

	list := s.List("", 0)
	if len(list) != 3 {
		t.Fatalf("want 3 entries, got %d", len(list))
	}
	// newest first
	if list[0].Action != ActionRemove {
		t.Errorf("newest first: got %s", list[0].Action)
	}
	filtered := s.List(ActionScale, 0)
	if len(filtered) != 1 || filtered[0].Action != ActionScale {
		t.Errorf("filter by action failed: %+v", filtered)
	}
}

func TestStoreRingEvict(t *testing.T) {
	s := NewStore(3)
	for i := 0; i < 5; i++ {
		s.Add(Entry{Action: ActionDeploy, Service: "s", OK: true})
	}
	if got := len(s.List("", 0)); got != 3 {
		t.Fatalf("ring should hold 3, got %d", got)
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
