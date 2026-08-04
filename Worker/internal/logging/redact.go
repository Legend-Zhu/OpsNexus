// Package logging provides a slog.Handler wrapper that masks sensitive
// attribute values (credentials, auth, secrets) before they reach the log
// output. This keeps config values like env=DB_PASSWORD=... or
// registryAuth.inline out of logs and events.
package logging

import (
	"context"
	"log/slog"
	"strings"
)

// sensitiveKeys match attribute keys exactly or by suffix. Matching is
// case-insensitive; keys like "env", "registryAuth", "dbPassword", "apiKey"
// are redacted, while "log_level"/"monitoring_enabled" are untouched.
var sensitiveKeys = []string{
	"env", "auth", "secret", "password", "passwd", "token",
	"apikey", "api_key", "credential", "cred",
}

// isSensitive reports whether an attribute key should be redacted.
func isSensitive(key string) bool {
	kl := strings.ToLower(key)
	for _, s := range sensitiveKeys {
		if kl == s || strings.HasSuffix(kl, s) {
			return true
		}
	}
	return false
}

// RedactHandler wraps another slog.Handler and masks sensitive attribute
// values with "[REDACTED]".
type RedactHandler struct {
	next slog.Handler
}

// NewRedactHandler wraps next with redaction.
func NewRedactHandler(next slog.Handler) *RedactHandler {
	return &RedactHandler{next: next}
}

func (h *RedactHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

// Handle clones the record, redacts sensitive attributes, and forwards it.
func (h *RedactHandler) Handle(ctx context.Context, r slog.Record) error {
	rec := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	rec.PC = r.PC
	r.Attrs(func(a slog.Attr) bool {
		if a.Value.Kind() == slog.KindGroup {
			// recurse into groups so nested secrets are caught too
			rec.AddAttrs(redactGroup(a))
		} else if isSensitive(a.Key) {
			// Sensitive key: redact regardless of value kind (strings, slices
			// like env=[DB_PASSWORD=...], any-typed secrets, ...).
			rec.AddAttrs(slog.String(a.Key, "[REDACTED]"))
		} else {
			rec.AddAttrs(a)
		}
		return true
	})
	return h.next.Handle(ctx, rec)
}

// redactGroup redacts sensitive entries inside a grouped attribute.
func redactGroup(g slog.Attr) slog.Attr {
	group := g.Value.Group() // []slog.Attr
	children := make([]any, 0, len(group))
	for _, c := range group {
		if c.Value.Kind() == slog.KindGroup {
			children = append(children, redactGroup(c))
		} else if isSensitive(c.Key) {
			children = append(children, slog.String(c.Key, "[REDACTED]"))
		} else {
			children = append(children, c)
		}
	}
	return slog.Group(g.Key, children...)
}

func (h *RedactHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &RedactHandler{next: h.next.WithAttrs(attrs)}
}

func (h *RedactHandler) WithGroup(name string) slog.Handler {
	return &RedactHandler{next: h.next.WithGroup(name)}
}
