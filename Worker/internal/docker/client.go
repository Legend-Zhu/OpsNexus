// Package docker wraps the subset of the Docker Engine / Swarm REST API that
// Worker needs. It talks to the daemon directly over a unix socket or TCP+TLS
// (no SDK dependency), mirroring the endpoints documented in
// docs/Worker-设计方案.md §4–§5. The rest of the codebase depends on the small
// Client interface defined here so it stays testable.
package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
)

// Client is the Engine/Swarm operation surface used by Worker.
type Client interface {
	// Connectivity
	Ping(ctx context.Context) error
	ServerVersion(ctx context.Context) (string, error)
	Close() error

	// Services
	ServiceCreate(ctx context.Context, spec ServiceSpec, registryAuth string) (string, error)
	ServiceUpdate(ctx context.Context, id string, spec ServiceSpec, registryAuth string) error
	ServiceRemove(ctx context.Context, id string) error
	ServiceScale(ctx context.Context, id string, replicas uint64) error
	ServiceRestart(ctx context.Context, id string) error
	ListServices(ctx context.Context, f Filter) ([]Service, error)
	GetService(ctx context.Context, id string) (Service, error)

	// Images & secrets
	ImagePull(ctx context.Context, ref, registryAuth string) error
	GetSecret(ctx context.Context, name string) (Secret, error)

	// Tasks
	ListTasks(ctx context.Context, f Filter) ([]Task, error)
	ServiceTasks(ctx context.Context, serviceID string) ([]Task, error)

	// Nodes
	ListNodes(ctx context.Context, f Filter) ([]Node, error)
	SelfNode(ctx context.Context) (Node, error)
	Info(ctx context.Context) (Info, error)

	// Logs / containers / stats
	ServiceLogs(ctx context.Context, serviceID string, opts LogsOptions) (io.ReadCloser, error)
	ListContainers(ctx context.Context, f Filter) ([]Container, error)
	// ListAllContainers lists ALL containers on the node (running and exited),
	// including standalone `docker run` containers — the node container view
	// (r-nacos, grafana, nginxwebui, ...) is built from this.
	ListAllContainers(ctx context.Context) ([]Container, error)
	ContainerStats(ctx context.Context, containerID string) (Stats, error)
	ContainerInspect(ctx context.Context, containerID string) (ContainerInspect, error)
	ContainerExecCreate(ctx context.Context, containerID string, cmd []string) (string, error)
	ExecStart(ctx context.Context, execID string) (io.ReadCloser, error)
	ExecInspect(ctx context.Context, execID string) (ExecInspect, error)
}

// Filter is a map of filter key → list of values, encoded as the Engine API
// "filters" query parameter (JSON object). e.g. {"service": ["abc"]}.
type Filter map[string][]string

// LogsOptions mirrors the service logs query parameters.
type LogsOptions struct {
	Follow     bool
	Stdout     bool
	Stderr     bool
	Timestamps bool
	Tail       string // "all" or a number
	Since      string // RFC3339 or UNIX timestamp
	Details    bool
}

// httpClient implements Client over the Docker Engine REST API.
type httpClient struct {
	base   string
	hc     *http.Client
	apiVer string // engine API version, e.g. "1.43"; "" uses negotiated default
}

// New creates a Client configured from DOCKER_HOST / DOCKER_TLS_VERIFY /
// DOCKER_CERT_PATH (matching the docker CLI environment). The daemon does not
// need to be reachable at construction time; call Ping to check.
func New() (Client, error) {
	return NewWithHost(os.Getenv("DOCKER_HOST"))
}

// NewWithHost builds a Client for an explicit DOCKER_HOST value. Pass "" for
// the default unix socket. Used by tests.
func NewWithHost(host string) (Client, error) {
	tlsCfg, err := loadTLSConfig()
	if err != nil {
		return nil, fmt.Errorf("docker TLS: %w", err)
	}
	base, tr, err := buildTransport(host, tlsCfg)
	if err != nil {
		return nil, err
	}
	return &httpClient{
		base: base,
		hc:   &http.Client{Transport: tr, Timeout: 0}, // per-request timeouts via ctx
	}, nil
}

func (c *httpClient) Close() error { return nil }

func (c *httpClient) Ping(ctx context.Context) error {
	_, _, err := c.do(ctx, http.MethodHead, "/_ping", nil, nil, nil)
	return err
}

func (c *httpClient) ServerVersion(ctx context.Context) (string, error) {
	var v versionInfo
	if err := c.getJSON(ctx, "/version", nil, &v); err != nil {
		return "", err
	}
	return v.Version, nil
}

// path prefixes every endpoint with the (optional) API version, so requests
// are versioned like /v1.43/services. When apiVer is empty the daemon uses its
// default (latest), which is fine for P0.
func (c *httpClient) path(p string) string {
	if c.apiVer == "" {
		return p
	}
	return "/v" + c.apiVer + p
}

// do issues a request and returns raw body + status. Body is nil for GETs.
func (c *httpClient) do(ctx context.Context, method, path string, query url.Values, body any, headers map[string]string) ([]byte, int, error) {
	full := c.base + c.path(path)
	if len(query) > 0 {
		full += "?" + query.Encode()
	}
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, fmt.Errorf("marshal request: %w", err)
		}
		reqBody = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, full, reqBody)
	if err != nil {
		return nil, 0, fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read response: %w", err)
	}
	return data, resp.StatusCode, nil
}

// getJSON issues a GET and decodes JSON into out on success.
func (c *httpClient) getJSON(ctx context.Context, path string, query url.Values, out any) error {
	data, code, err := c.do(ctx, http.MethodGet, path, query, nil, nil)
	if err != nil {
		return err
	}
	return decodeOrError(data, code, out)
}

// postJSON issues a POST with a JSON body and decodes the response.
func (c *httpClient) postJSON(ctx context.Context, path string, query url.Values, body, out any, headers map[string]string) error {
	data, code, err := c.do(ctx, http.MethodPost, path, query, body, headers)
	if err != nil {
		return err
	}
	return decodeOrError(data, code, out)
}

// postJSONStream issues a POST with a JSON body and returns the raw response
// body as a stream (for endpoints whose response is a byte stream, e.g.
// /exec/{id}/start). The caller closes the reader.
func (c *httpClient) postJSONStream(ctx context.Context, path string, body any) (io.ReadCloser, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+c.path(path), bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, apiErrorFrom(resp.StatusCode, data)
	}
	return resp.Body, nil
}

// delete issues a DELETE and expects 2xx/404.
func (c *httpClient) del(ctx context.Context, path string, query url.Values) error {
	_, code, err := c.do(ctx, http.MethodDelete, path, query, nil, nil)
	if err != nil {
		return err
	}
	if code >= 400 && code != http.StatusNotFound {
		return apiErrorFrom(code, nil)
	}
	return nil
}

// postRaw issues a POST and returns the raw body (for endpoints returning a stream or text).
func (c *httpClient) postRaw(ctx context.Context, path string, query url.Values, headers map[string]string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+c.path(path), nil)
	if err != nil {
		return nil, err
	}
	if len(query) > 0 {
		req.URL.RawQuery = query.Encode()
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, apiErrorFrom(resp.StatusCode, b)
	}
	return resp.Body, nil
}

// getStream issues a GET that returns a long-lived stream (logs). The caller
// closes the reader.
func (c *httpClient) getStream(ctx context.Context, path string, query url.Values) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+c.path(path), nil)
	if err != nil {
		return nil, err
	}
	if len(query) > 0 {
		req.URL.RawQuery = query.Encode()
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, apiErrorFrom(resp.StatusCode, b)
	}
	return resp.Body, nil
}

// decodeOrError decodes JSON on 2xx, otherwise returns an API error.
func decodeOrError(data []byte, code int, out any) error {
	if code >= 400 {
		return apiErrorFrom(code, data)
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode response (status %d): %w", code, err)
	}
	return nil
}

func apiErrorFrom(code int, body []byte) error {
	var ae apiError
	if len(body) > 0 {
		_ = json.Unmarshal(body, &ae)
	}
	if ae.Message == "" {
		return fmt.Errorf("docker api error %d", code)
	}
	return fmt.Errorf("docker api error %d: %s", code, ae.Message)
}

// filtersQuery encodes a Filter as the "filters" query parameter value.
func filtersQuery(f Filter) url.Values {
	q := url.Values{}
	if len(f) > 0 {
		b, _ := json.Marshal(f)
		q.Set("filters", string(b))
	}
	return q
}

// Compile-time assertion.
var _ Client = (*httpClient)(nil)
