package logging

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

// captureHandler collects log output as plain text.
type captureHandler struct {
	buf bytes.Buffer
}

func (c *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (c *captureHandler) Handle(_ context.Context, r slog.Record) error {
	c.buf.WriteString(r.Message + " ")
	r.Attrs(func(a slog.Attr) bool {
		c.buf.WriteString(a.Key + "=" + a.Value.String() + " ")
		return true
	})
	c.buf.WriteString("\n")
	return nil
}
func (c *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *captureHandler) WithGroup(string) slog.Handler      { return c }

func TestRedactStringSensitive(t *testing.T) {
	var cap captureHandler
	log := slog.New(NewRedactHandler(&cap))

	log.Info("deploy",
		"service", "web",
		"env", []string{"DB_PASSWORD=hunter2", "DEBUG=true"},
		"registryAuth", "eyJhdXRocyI6e30=",
		"log_level", "info", // must NOT be redacted
	)

	out := cap.buf.String()
	if strings.Contains(out, "hunter2") {
		t.Errorf("password leaked in log: %s", out)
	}
	if strings.Contains(out, "eyJhdXRocyI6e30=") {
		t.Errorf("registryAuth leaked in log: %s", out)
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Errorf("expected redaction marker, got: %s", out)
	}
	if !strings.Contains(out, "log_level=info") {
		t.Errorf("non-sensitive key should not be redacted: %s", out)
	}
	if !strings.Contains(out, "service=web") {
		t.Errorf("service name should not be redacted: %s", out)
	}
}

func TestRedactSuffixMatch(t *testing.T) {
	var cap captureHandler
	log := slog.New(NewRedactHandler(&cap))
	log.Info("x", "dbPassword", "s3cr3t", "apiToken", "tok", "plain", "ok")
	out := cap.buf.String()
	for _, leak := range []string{"s3cr3t", "tok"} {
		if strings.Contains(out, leak) {
			t.Errorf("%q leaked: %s", leak, out)
		}
	}
	if !strings.Contains(out, "plain=ok") {
		t.Errorf("plain attr should remain: %s", out)
	}
}
