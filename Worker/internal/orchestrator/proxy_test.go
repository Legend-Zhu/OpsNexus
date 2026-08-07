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
	_, _ = nc.Stats(context.Background())             //nolint:errcheck
	select {
	case a := <-gotAuth:
		if a != "" {
			t.Fatalf("expected no auth header when token empty, got %q", a)
		}
	default:
		t.Fatal("no request observed")
	}
}
