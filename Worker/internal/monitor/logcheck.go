package monitor

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/config"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/docker"
)

// logCheck watches a service's aggregated log stream and emits EventLogMatch
// events when lines match configured patterns (with ignore filters and
// debounce). The stream reconnects automatically on EOF/error.
type logCheck struct {
	service string
	cli     docker.Client
	store   *EventStore
	log     *slog.Logger

	patterns []*regexp.Regexp
	ignores  []*regexp.Regexp
	level    Level
	action   string

	mu      sync.Mutex
	lastHit time.Time
	debounce time.Duration
}

func newLogCheck(service string, lc config.LogCheck, cli docker.Client, store *EventStore, log *slog.Logger) *logCheck {
	patterns := make([]*regexp.Regexp, 0, 1)
	if p, err := regexp.Compile(lc.Pattern); err == nil {
		patterns = append(patterns, p)
	}
	ignores := make([]*regexp.Regexp, 0, len(lc.Ignore))
	for _, ig := range lc.Ignore {
		if re, err := regexp.Compile(ig); err == nil {
			ignores = append(ignores, re)
		}
	}
	lvl := LevelWarn
	switch lc.Level {
	case "error":
		lvl = LevelError
	case "info":
		lvl = LevelInfo
	}
	if lc.Action == "" {
		lc.Action = "alert"
	}
	return &logCheck{
		service:  service,
		cli:      cli,
		store:    store,
		log:      log,
		patterns: patterns,
		ignores:  ignores,
		level:    lvl,
		action:   lc.Action,
		debounce: 10 * time.Second,
	}
}

// run streams the log until ctx is cancelled. A new stream is opened whenever
// the previous one ends or errors (with backoff).
func (l *logCheck) run(ctx context.Context) {
	backoff := time.Second
	for {
		err := l.streamOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		l.log.Warn("log stream ended, reconnecting", "service", l.service, "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
	}
}

func (l *logCheck) streamOnce(ctx context.Context) error {
	rc, err := l.cli.ServiceLogs(ctx, l.service, docker.LogsOptions{
		Follow:     true,
		Stdout:     true,
		Stderr:     true,
		Timestamps: false,
		Tail:       "all",
	})
	if err != nil {
		return err
	}
	defer rc.Close()

	dec := newLogStreamDecoder(rc)
	return dec.scanLines(func(line string) {
		// fast path: quick check before regexes
		if len(l.patterns) == 0 {
			return
		}
		matched := false
		for _, p := range l.patterns {
			if p.MatchString(line) {
				matched = true
				break
			}
		}
		if !matched {
			return
		}
		for _, ig := range l.ignores {
			if ig.MatchString(line) {
				return
			}
		}
		l.emit(line)
	})
}

func (l *logCheck) emit(line string) {
	// debounce: avoid flooding the store during a log burst
	l.mu.Lock()
	now := time.Now()
	if now.Sub(l.lastHit) < l.debounce {
		l.mu.Unlock()
		return
	}
	l.lastHit = now
	l.mu.Unlock()

	line = strings.TrimSpace(line)
	if len(line) > 500 {
		line = line[:500] + "..."
	}
	ev := Event{
		Service: l.service,
		Type:    EventLogMatch,
		Level:   l.level,
		Msg:     "log matched pattern",
		Detail:  line,
	}
	l.store.Add(ev)
	l.log.Warn("log match", "service", l.service, "detail", line)
}
