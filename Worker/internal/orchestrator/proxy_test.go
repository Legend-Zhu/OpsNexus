package orchestrator

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestNodeClientSendsBearer 鉴权开启时 NodeClient 的每次请求都必须带
// "Authorization: Bearer <token>"，否则节点 worker 本地 API 以 401 拒绝。
func TestNodeClientSendsBearer(t *testing.T) {
	gotAuth := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth <- r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(ProcessesResp{}) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)

	// Override base to the test server (bypass addr parsing).
	nc := &NodeClient{base: srv.URL, hc: srv.Client(), token: "s3cr3t"}
	_, err := nc.Processes(context.Background(), "cpu", "10", "")
	if err != nil {
		t.Fatalf("processes: %v", err)
	}
	select {
	case a := <-gotAuth:
		if a != "Bearer s3cr3t" {
			t.Fatalf("expected bearer header, got %q", a)
		}
	default:
		t.Fatal("no Authorization header received")
	}
}

// TestNodeClientNoTokenWhenDisabled 鉴权未开（token 空）时不发 Authorization 头。
func TestNodeClientNoTokenWhenDisabled(t *testing.T) {
	gotAuth := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth <- r.Header.Get("Authorization")
		_, _ = io.WriteString(w, "{}")
	}))
	t.Cleanup(srv.Close)

	nc := &NodeClient{base: srv.URL, hc: srv.Client()} // token 留空
	_, _ = nc.Stats(context.Background())              //nolint:errcheck
	select {
	case a := <-gotAuth:
		if a != "" {
			t.Fatalf("expected no auth header when token empty, got %q", a)
		}
	default:
		t.Fatal("no request observed")
	}
}

// TestNodeClientContainerHealth 批量健康查询：ids 以 JSON 数组发给节点
// worker，返回的 health map 原样透传；节点侧 4xx/5xx（如旧版 worker 无此
// 端点）返回 error，由调用方按"该节点全部不健康"降级。
func TestNodeClientContainerHealth(t *testing.T) {
	var gotBody struct {
		IDs []string `json:"ids"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/local/containers/health" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody) //nolint:errcheck
		_ = json.NewEncoder(w).Encode(ContainerHealthResp{
			Node:   "worker-01",
			Health: map[string]string{"c1": "healthy", "c2": "unhealthy"},
		}) //nolint:errcheck
	}))
	t.Cleanup(srv.Close)

	nc := &NodeClient{base: srv.URL, hc: srv.Client()}
	health, err := nc.ContainerHealth(context.Background(), []string{"c1", "c2", "c3"})
	if err != nil {
		t.Fatalf("container health: %v", err)
	}
	if len(gotBody.IDs) != 3 || gotBody.IDs[0] != "c1" {
		t.Fatalf("ids not forwarded: %+v", gotBody)
	}
	if health["c1"] != "healthy" || health["c2"] != "unhealthy" {
		t.Fatalf("bad health map: %v", health)
	}
	if _, ok := health["c3"]; ok {
		t.Fatalf("missing id should stay absent, got %v", health)
	}

	// 节点端点报错（旧版 worker 404）→ error 返回。
	errSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"unknown endpoint"}`) //nolint:errcheck
	}))
	t.Cleanup(errSrv.Close)
	nc = &NodeClient{base: errSrv.URL, hc: errSrv.Client()}
	if _, err = nc.ContainerHealth(context.Background(), []string{"c1"}); err == nil {
		t.Fatal("expected error on 404 response")
	}
}
