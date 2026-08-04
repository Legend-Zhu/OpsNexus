package workerproxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestSelfDecode 解码 Worker /api/v1/self 响应。
func TestSelfDecode(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok123" {
			w.WriteHeader(401)
			return
		}
		w.Write([]byte(`{"nodeId":"n1","hostname":"h1","role":"manager","leader":true,"state":"ready","swarmManager":true,"addr":"10.0.0.1"}`))
	}))
	defer ts.Close()

	cli := New(ts.URL, "tok123")
	si, err := cli.Self(context.Background())
	if err != nil {
		t.Fatalf("self: %v", err)
	}
	if !si.SwarmManager || si.Role != "manager" || si.NodeID != "n1" {
		t.Fatalf("unexpected self: %+v", si)
	}
}

// TestAuthRequired 无 token 请求 → 401 → ErrUnreachable。
func TestAuthRequired(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.WriteHeader(401)
			w.Write([]byte(`unauthorized`))
			return
		}
		w.WriteHeader(200)
	}))
	defer ts.Close()

	cli := New(ts.URL, "")
	_, err := cli.Self(context.Background())
	if err == nil {
		t.Fatal("expected error for missing token")
	}
	ue, ok := err.(*ErrUnreachable)
	if !ok {
		t.Fatalf("expected *ErrUnreachable, got %T", err)
	}
	if ue.Status != 401 {
		t.Fatalf("expected status 401, got %d", ue.Status)
	}
}

// TestNodeStats 解码 /api/v1/local/stats。
func TestNodeStats(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"node":"node-1","containers":[{"containerId":"c1","service":"web","cpuPercent":3.5,"memPercent":20.1,"memUsageBytes":1048576,"memLimitBytes":5242880}]}`))
	}))
	defer ts.Close()

	ns, err := New(ts.URL, "").NodeStats(context.Background())
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if ns.Node != "node-1" || len(ns.Containers) != 1 || ns.Containers[0].Service != "web" {
		t.Fatalf("unexpected stats: %+v", ns)
	}
}

// TestUnreachable 连接失败 → ErrUnreachable（status=0）。
func TestUnreachable(t *testing.T) {
	cli := New("http://127.0.0.1:1", "")
	_, err := cli.Self(context.Background())
	if err == nil {
		t.Fatal("expected error for unreachable worker")
	}
	if _, ok := err.(*ErrUnreachable); !ok {
		t.Fatalf("expected *ErrUnreachable, got %T", err)
	}
}
