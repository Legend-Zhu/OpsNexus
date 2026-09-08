// OAuth 2.1 接入与 MCP resources 测试。
package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// fakeIdPValidator 可编程的 IdP 校验器（测试替身）。
type fakeIdPValidator struct {
	accept map[string]string // secret -> scope
}

func (f fakeIdPValidator) ValidateAccessToken(raw string) (subject, username, scope string, err error) {
	scope, ok := f.accept[raw]
	if !ok {
		return "", "", "", context.Canceled // 任意错误均可
	}
	return "sub-" + raw, "alice-" + raw, scope, nil
}

func TestIdPFallback(t *testing.T) {
	h := &Handler{
		auth: newAuthorizer([]TokenConfig{{Name: "static", Secret: "static-secret", Scope: "read"}}),
		deps: Deps{
			IdP: fakeIdPValidator{accept: map[string]string{
				"jwt-write": "openid mcp:write",
				"jwt-exec":  "mcp:exec",
				"jwt-plain": "openid profile",
			}},
		},
	}

	// 静态 token 优先（表命中即不查 IdP）
	id, ok := h.authenticateAny("static-secret")
	if !ok || id.Name != "static" {
		t.Fatalf("static token should win: %+v ok=%v", id, ok)
	}
	// IdP token：mcp:write → write
	id, ok = h.authenticateAny("jwt-write")
	if !ok || id.Name != "idp:alice-jwt-write" || !id.canWrite() || id.canExec() {
		t.Fatalf("idp write mapping broken: %+v ok=%v", id, ok)
	}
	// IdP token：mcp:exec → exec
	id, ok = h.authenticateAny("jwt-exec")
	if !ok || !id.canExec() || !id.canWrite() {
		t.Fatalf("idp exec mapping broken: %+v ok=%v", id, ok)
	}
	// IdP token：无 mcp scope → 只读（最小授权）
	id, ok = h.authenticateAny("jwt-plain")
	if !ok || id.canWrite() {
		t.Fatalf("idp plain scope must be read-only: %+v ok=%v", id, ok)
	}
	// 未知 token：静态与 IdP 都拒
	if _, ok := h.authenticateAny("nope"); ok {
		t.Fatal("unknown token must fail both paths")
	}
	// 无 IdP 时回退路径关闭
	h2 := &Handler{auth: newAuthorizer(nil)}
	if _, ok := h2.authenticateAny("jwt-write"); ok {
		t.Fatal("idp fallback must be off when IdP is nil")
	}
}

func TestMetadataEndpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &Handler{
		deps: Deps{
			IdP:       fakeIdPValidator{accept: map[string]string{}},
			IdPIssuer: "https://opsguard.example.com",
		},
	}

	req := httptest.NewRequest(http.MethodGet, "/.well-known/oauth-protected-resource", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req
	h.Metadata(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("metadata should 200, got %d", rec.Code)
	}
	var doc struct {
		Resource             string   `json:"resource"`
		AuthorizationServers []string `json:"authorization_servers"`
		ScopesSupported      []string `json:"scopes_supported"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("metadata not json: %v", err)
	}
	if !strings.HasPrefix(doc.Resource, "https://") {
		t.Fatalf("resource should honor X-Forwarded-Proto: %q", doc.Resource)
	}
	if len(doc.AuthorizationServers) != 1 || doc.AuthorizationServers[0] != "https://opsguard.example.com" {
		t.Fatalf("authorization_servers wrong: %v", doc.AuthorizationServers)
	}
	found := false
	for _, s := range doc.ScopesSupported {
		if s == "mcp:exec" {
			found = true
		}
	}
	if !found {
		t.Fatalf("scopes_supported should include mcp:exec: %v", doc.ScopesSupported)
	}
}

func TestResourcesRegistered(t *testing.T) {
	h := New(Deps{
		Config: Config{Tokens: []TokenConfig{{Name: "t", Secret: "s", Scope: "read"}}},
		Log:    testLogger(),
	})
	if h.srv == nil {
		t.Fatal("server not built")
	}
	// resources 注册失败会让 New panic（SDK AddResource 语义）；
	// 完整读取链路由 smoke_test 的 HTTP 冒烟覆盖。
}
