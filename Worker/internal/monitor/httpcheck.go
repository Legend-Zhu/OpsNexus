package monitor

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"time"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/config"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/docker"
)

// httpCheck probes an HTTP(S) endpoint (typically the service's public URL or
// a health route) and emits EventHTTPUnhealthy when the status code is outside
// expectedStatus or the body does not match expectedBody.
//
// The probe runs inside the Worker container, so a URL whose host is
// "localhost"/"127.0.0.1" would point at the Worker itself, not the service.
// When the host is loopback we rewrite it to a node IP running a task of the
// service (routing mesh makes any node answer in ingress mode).
type httpCheck struct {
	service string
	cfg     config.HTTPCheck
	client  *http.Client
	store   *EventStore
	log     *slog.Logger
	bodyRe  *regexp.Regexp
	cli     docker.Client
}

func newHTTPCheck(service string, hc config.HTTPCheck, cli docker.Client, store *EventStore, log *slog.Logger) (*httpCheck, error) {
	timeout, err := time.ParseDuration(hc.Timeout)
	if err != nil {
		return nil, fmt.Errorf("httpCheck timeout: %w", err)
	}
	if hc.Method == "" {
		hc.Method = http.MethodGet
	}
	var re *regexp.Regexp
	if hc.ExpectedBody != "" {
		re, err = regexp.Compile(hc.ExpectedBody)
		if err != nil {
			return nil, fmt.Errorf("httpCheck expectedBody: %w", err)
		}
	}
	return &httpCheck{
		service: service,
		cfg:     hc,
		client:  &http.Client{Timeout: timeout},
		store:   store,
		log:     log,
		bodyRe:  re,
		cli:     cli,
	}, nil
}

func (h *httpCheck) run(ctx context.Context) {
	interval, err := time.ParseDuration(h.cfg.Interval)
	if err != nil || interval <= 0 {
		interval = 15 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	fails := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			ok, detail := h.probe(ctx)
			if ok {
				fails = 0
				continue
			}
			fails++
			if fails >= 2 { // two consecutive failures before alerting
				ev := Event{
					Service: h.service,
					Type:    EventHTTPUnhealthy,
					Level:   LevelError,
					Msg:     fmt.Sprintf("http %s %s failed", h.cfg.Method, h.cfg.URL),
					Detail:  detail,
				}
				h.store.Add(ev)
				h.log.Error("http check failed", "service", h.service, "detail", detail)
				fails = 0
			}
		}
	}
}

func (h *httpCheck) probe(ctx context.Context) (bool, string) {
	target, err := h.resolveTarget(ctx)
	if err != nil {
		return false, err.Error()
	}
	req, err := http.NewRequestWithContext(ctx, h.cfg.Method, target, nil)
	if err != nil {
		return false, err.Error()
	}
	for k, v := range h.cfg.Headers {
		req.Header.Set(k, v)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return false, err.Error()
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))

	if len(h.cfg.ExpectedStatus) > 0 {
		ok := false
		for _, s := range h.cfg.ExpectedStatus {
			if resp.StatusCode == s {
				ok = true
				break
			}
		}
		if !ok {
			return false, fmt.Sprintf("status %d not in expected %v", resp.StatusCode, h.cfg.ExpectedStatus)
		}
	}
	if h.bodyRe != nil && !h.bodyRe.Match(body) {
		return false, fmt.Sprintf("body does not match %q", h.cfg.ExpectedBody)
	}
	return true, ""
}

// resolveTarget rewrites loopback hosts to a task node IP so probes inside the
// Worker container reach the service. Non-loopback URLs are used as-is.
func (h *httpCheck) resolveTarget(ctx context.Context) (string, error) {
	u, err := url.Parse(h.cfg.URL)
	if err != nil {
		return "", err
	}
	host := u.Hostname()
	if host != "localhost" && host != "127.0.0.1" && host != "::1" {
		return h.cfg.URL, nil
	}
	addrs, err := serviceNodeAddrs(ctx, h.cli, h.service)
	if err != nil {
		return "", fmt.Errorf("resolve node for http check: %w", err)
	}
	if len(addrs) == 0 {
		return "", fmt.Errorf("no nodes running tasks for %s", h.service)
	}
	u.Host = net.JoinHostPort(addrs[0], u.Port())
	return u.String(), nil
}
