package auth

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
)

func newTestAuth(t *testing.T, secret string) *Service {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "ogw-test"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return New(st, secret, time.Hour, nil)
}

// TestLoginTokenRoundtrip 创建用户 → 登录拿 token → 校验成功；错密码被拒。
func TestLoginTokenRoundtrip(t *testing.T) {
	svc := newTestAuth(t, "test-secret")
	if _, err := svc.CreateUser("admin", "pass123", "admin", true); err != nil {
		t.Fatalf("create: %v", err)
	}
	// 错误密码
	if _, err := svc.Login("admin", "wrong"); err == nil {
		t.Fatal("wrong password should fail")
	}
	// 正确登录
	token, err := svc.Login("admin", "pass123")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	username, role, err := svc.VerifyToken(token)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if username != "admin" || role != "admin" {
		t.Fatalf("unexpected identity: %s/%s", username, role)
	}
	// 篡改 token 被拒
	if _, _, err := svc.VerifyToken(token + "x"); err == nil {
		t.Fatal("tampered token should fail")
	}
}

// TestPasswordNotPlaintext 密码加盐哈希存储，不落明文。
func TestPasswordNotPlaintext(t *testing.T) {
	svc := newTestAuth(t, "s")
	if _, err := svc.CreateUser("u1", "secret-pass", "viewer", true); err != nil {
		t.Fatalf("create: %v", err)
	}
	u, err := svc.st.GetUserByUsername("u1")
	if err != nil || u == nil {
		t.Fatalf("get: %v", err)
	}
	if u.Password == "secret-pass" || u.Password == "" {
		t.Fatalf("password not hashed: %q", u.Password)
	}
}

// TestMiddleware 中间件：无 token 401、有效 token 放行、public 路径放行。
func TestMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := newTestAuth(t, "secret")
	if _, err := svc.CreateUser("op", "pw", "viewer", true); err != nil {
		t.Fatalf("create: %v", err)
	}
	token, _ := svc.Login("op", "pw")

	r := gin.New()
	r.Use(svc.Middleware([]string{"/healthz", "/api/v1/auth/login"}))
	r.GET("/healthz", func(c *gin.Context) { c.String(200, "ok") })
	r.GET("/protected", func(c *gin.Context) { c.String(200, c.GetString("username")) })
	r.POST("/api/v1/auth/login", func(c *gin.Context) { c.String(200, "login") })

	// public 放行
	req := httptest.NewRequest("GET", "/healthz", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("public path should pass, got %d", w.Code)
	}
	// 无 token
	req = httptest.NewRequest("GET", "/protected", nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no token should 401, got %d", w.Code)
	}
	// 有效 token
	req = httptest.NewRequest("GET", "/protected", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 200 || w.Body.String() != "op" {
		t.Fatalf("token should pass: %d %q", w.Code, w.Body.String())
	}
	// 过期 token
	expired, _ := svc.IssueTokenWithTTL("op", "viewer", -time.Minute)
	req = httptest.NewRequest("GET", "/protected", nil)
	req.Header.Set("Authorization", "Bearer "+expired)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expired token should 401, got %d", w.Code)
	}
}
