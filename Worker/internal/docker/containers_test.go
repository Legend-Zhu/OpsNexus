package docker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRestartContainer 验证 POST /containers/{id}/restart 的请求形态与
// 非 2xx 错误映射。
func TestRestartContainer(t *testing.T) {
	var gotPath string
	var gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	cli, err := NewWithHost(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := cli.RestartContainer(context.Background(), "r-nacos"); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if gotPath != "/containers/r-nacos/restart" || gotMethod != http.MethodPost {
		t.Fatalf("want POST /containers/r-nacos/restart, got %s %s", gotMethod, gotPath)
	}

	// 404 → 错误（供上层映射）。
	srvErr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"message":"No such container: nope"}`))
	}))
	defer srvErr.Close()
	cliErr, err := NewWithHost(srvErr.URL)
	if err != nil {
		t.Fatal(err)
	}
	err = cliErr.RestartContainer(context.Background(), "nope")
	if err == nil || !strings.Contains(err.Error(), "No such container") {
		t.Fatalf("want 404 error with docker message, got %v", err)
	}
}
