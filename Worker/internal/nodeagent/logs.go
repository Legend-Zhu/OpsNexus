package nodeagent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/docker"
)

// logs streams a service's log lines as Server-Sent Events. Equivalent to
// `docker service logs -f <name>`.
//
//	GET /api/v1/local/logs?service=<name>[&follow=true|false][&tail=N][&since=RFC3339]
//
// Each log line is emitted as:
//
//	data: {"ts":"...","stream":"stdout|stderr","line":"..."}
//
// With follow=true the stream stays open (reconnect on EOF, like logcheck).
func (a *API) logs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	service := q.Get("service")
	if service == "" {
		writeErr(w, http.StatusBadRequest, errEmpty("service"))
		return
	}
	follow := q.Get("follow") == "true"
	tail := q.Get("tail")
	since := q.Get("since")

	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("streaming not supported"))
		return
	}
	ctx := r.Context()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	emit := func(stream, line string) {
		payload, _ := json.Marshal(map[string]string{
			"ts":     time.Now().UTC().Format(time.RFC3339),
			"stream": stream,
			"line":   line,
		})
		_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
		fl.Flush()
	}

	if follow {
		a.streamFollow(ctx, w, fl, service, tail, since, emit)
		return
	}
	a.streamOnce(ctx, w, service, tail, since, emit)
}

// streamOnce pulls the tail of the log and closes.
func (a *API) streamOnce(ctx context.Context, w http.ResponseWriter, service, tail, since string, emit func(stream, line string)) {
	rc, err := a.cli.ServiceLogs(ctx, service, docker.LogsOptions{
		Stdout: true, Stderr: true, Tail: tail, Since: since,
	})
	if err != nil {
		_, _ = fmt.Fprintf(w, "data: {\"error\":%q}\n\n", err.Error())
		return
	}
	defer rc.Close()
	_ = docker.DecodeLogLines(rc, func(line string) { emit("stdout", line) })
}

// streamFollow keeps the stream open, reconnecting when the log stream ends.
func (a *API) streamFollow(ctx context.Context, w http.ResponseWriter, fl http.Flusher, service, tail, since string, emit func(stream, line string)) {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		rc, err := a.cli.ServiceLogs(ctx, service, docker.LogsOptions{
			Follow: true, Stdout: true, Stderr: true, Tail: tail, Since: since,
		})
		if err != nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			continue
		}
		_ = docker.DecodeLogLines(rc, func(line string) { emit("stdout", line) })
		rc.Close()
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}
