package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// WebhookPusher forwards every added event to a set of configured webhook
// URLs (e.g. the OpsGaurdWeb event-ingest endpoint). Delivery is fire-and-
// forget with exponential backoff retries; failures are logged, never block
// the event pipeline.
//
// The payload is the Event JSON; the receiver should dedupe by event id.
type WebhookPusher struct {
	urls   []string
	client *http.Client
	log    *slog.Logger

	// retries/backoff config
	maxRetries int
	baseDelay  time.Duration
	maxDelay   time.Duration

	// track in-flight goroutines for tests/shutdown (best-effort)
	wg sync.WaitGroup
}

// NewWebhookPusher builds a pusher for the given URLs (nil/empty disables).
func NewWebhookPusher(urls []string, log *slog.Logger) *WebhookPusher {
	if log == nil {
		log = slog.Default()
	}
	return &WebhookPusher{
		urls:       urls,
		client:     &http.Client{Timeout: 10 * time.Second},
		log:        log,
		maxRetries: 3,
		baseDelay:  time.Second,
		maxDelay:   30 * time.Second,
	}
}

// Sink returns an EventStore-compatible callback.
func (w *WebhookPusher) Sink() func(Event) {
	return func(e Event) {
		w.SendJSON(e)
	}
}

// SendJSON delivers an arbitrary JSON-serializable payload (monitor event,
// audit entry, ...) to every configured URL with retries.
func (w *WebhookPusher) SendJSON(v any) {
	if w == nil || len(w.urls) == 0 {
		return
	}
	w.wg.Add(1)
	go w.pushPayload(v)
}

// pushPayload marshals v and delivers it to all URLs.
func (w *WebhookPusher) pushPayload(v any) {
	defer w.wg.Done()
	body, err := json.Marshal(v)
	if err != nil {
		w.log.Error("webhook marshal failed", "err", err)
		return
	}
	for _, u := range w.urls {
		w.pushURL(u, body)
	}
}

func (w *WebhookPusher) pushURL(url string, body []byte) {
	delay := w.baseDelay
	for attempt := 0; attempt <= w.maxRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(delay)
			if delay < w.maxDelay {
				delay *= 2
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			cancel()
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := w.client.Do(req)
		cancel()
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return
			}
			w.log.Warn("webhook non-2xx", "url", url, "status", resp.StatusCode, "attempt", attempt)
		} else {
			w.log.Warn("webhook push failed", "url", url, "err", err, "attempt", attempt)
		}
	}
}

// Wait blocks until in-flight pushes finish (best-effort, used in tests).
func (w *WebhookPusher) Wait() {
	w.wg.Wait()
}
