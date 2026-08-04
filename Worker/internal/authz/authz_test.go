package authz

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/agent"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/audit"
)

func TestDisabledAllows(t *testing.T) {
	mw := New(&agent.AuthConfig{Enabled: false})
	ok := false
	srv := httptest.NewServer(mw.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ok = true
	})))
	defer srv.Close()
	resp, _ := http.Get(srv.URL)
	resp.Body.Close()
	if !ok {
		t.Fatal("disabled auth should pass through")
	}
}

func TestMissingTokenRejected(t *testing.T) {
	mw := New(&agent.AuthConfig{Enabled: true, Tokens: map[string]string{"admin": "s3cr3t"}})
	srv := httptest.NewServer(mw.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})))
	defer srv.Close()
	resp, _ := http.Get(srv.URL)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing token: want 401, got %d", resp.StatusCode)
	}
}

func TestWrongTokenRejected(t *testing.T) {
	mw := New(&agent.AuthConfig{Enabled: true, Tokens: map[string]string{"admin": "s3cr3t"}})
	srv := httptest.NewServer(mw.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	req.Header.Set("Authorization", "Bearer wrong")
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token: want 401, got %d", resp.StatusCode)
	}
}

func TestValidTokenPasses(t *testing.T) {
	mw := New(&agent.AuthConfig{Enabled: true, Tokens: map[string]string{"admin": "s3cr3t"}})
	got := ""
	srv := httptest.NewServer(mw.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = audit.ActorFromContext(r.Context())
	})))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	req.Header.Set("Authorization", "Bearer s3cr3t")
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid token: want 200, got %d", resp.StatusCode)
	}
	if got != "admin" {
		t.Fatalf("actor should be token name 'admin', got %q", got)
	}
}
