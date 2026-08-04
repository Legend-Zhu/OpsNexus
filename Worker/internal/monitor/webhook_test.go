package monitor

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestWebhookPusherDelivers verifies an event reaches the webhook URL.
func TestWebhookPusherDelivers(t *testing.T) {
	got := make(chan Event, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var e Event
		if err := json.NewDecoder(r.Body).Decode(&e); err != nil {
			t.Errorf("decode: %v", err)
		}
		got <- e
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := NewWebhookPusher([]string{srv.URL}, nil)
	p.maxRetries = 1
	evStore := NewEventStore(100)
	evStore.AddSink(p.Sink())

	evStore.Add(Event{Service: "svc-a", Type: EventLogMatch, Level: LevelWarn, Msg: "boom"})

	select {
	case e := <-got:
		if e.Service != "svc-a" || e.Type != EventLogMatch {
			t.Errorf("unexpected event: %+v", e)
		}
		if e.ID == "" {
			t.Error("event id should be assigned")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("event not delivered to webhook")
	}
	p.Wait()
}

// TestWebhookPusherRetries verifies a failing URL is retried then abandoned.
func TestWebhookPusherRetries(t *testing.T) {
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := NewWebhookPusher([]string{srv.URL}, nil)
	p.maxRetries = 2
	p.baseDelay = 10 * time.Millisecond
	evStore := NewEventStore(100)
	evStore.AddSink(p.Sink())
	evStore.Add(Event{Service: "s", Type: EventPortDown, Msg: "x"})
	p.Wait()

	if attempts < 2 {
		t.Errorf("expected retries, got %d attempts", attempts)
	}
}
