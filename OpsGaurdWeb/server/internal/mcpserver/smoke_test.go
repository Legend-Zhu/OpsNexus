// 端到端冒烟：起真实 HTTP 服务（中间件 + stateless Streamable HTTP），
// 用 go-sdk 客户端走 鉴权→initialize→tools/list 全流程，验证 /mcp 的
// 协议面与 P1 工具清单。
package mcpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// authTransport 给所有请求注入 Bearer（模拟助手侧配置）。
type authTransport struct {
	base   http.RoundTripper
	secret string
}

func (t *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+t.secret)
	return (t.base).RoundTrip(clone)
}

func startSmokeServer(t *testing.T, secret string) *httptest.Server {
	t.Helper()
	h := New(Deps{
		Config: Config{
			Enabled: true,
			Tokens:  []TokenConfig{{Name: "smoke", Secret: secret, Scope: "read"}},
		},
		Log: testLogger(),
	})
	mux := http.NewServeMux()
	mux.Handle("/mcp", middlewareAdapt(h.Middleware(), h.HTTPHandler()))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// middlewareAdapt 把 gin 中间件 + net/http handler 串起来（复刻 router 挂载）。
func middlewareAdapt(mw gin.HandlerFunc, next http.Handler) http.Handler {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Any("/mcp", mw, gin.WrapH(next))
	return engine
}

func TestSmokeToolsListOverHTTP(t *testing.T) {
	secret := "smoke-secret"
	srv := startSmokeServer(t, secret)

	cli := mcp.NewClient(&mcp.Implementation{Name: "smoke-client", Version: "0"}, nil)
	tr := &mcp.StreamableClientTransport{
		Endpoint:   srv.URL + "/mcp",
		HTTPClient: &http.Client{Transport: &authTransport{base: http.DefaultTransport, secret: secret}},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	session, err := cli.Connect(ctx, tr, nil)
	if err != nil {
		t.Fatalf("initialize/connect failed: %v", err)
	}
	defer session.Close()

	resp, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("tools/list failed: %v", err)
	}

	got := map[string]bool{}
	for _, tool := range resp.Tools {
		got[tool.Name] = true
	}
	// 清单抽查：每个域至少一个代表工具 + confirm/exec 工具齐全
	want := []string{
		"project_list", "project_create", "project_delete",
		"cluster_list", "cluster_add", "cluster_remove", "cluster_events",
		"service_list", "service_deploy", "service_scale", "service_remove", "service_logs", "service_operation",
		"image_list", "image_delete", "build_upload_begin", "build_submit", "build_get",
		"alert_list", "alert_get", "alert_ack",
		"investigation_start", "investigation_get", "investigation_continue",
		"node_list", "node_stats", "probe_port", "probe_http",
		"patrol_list", "patrol_create", "patrol_run", "patrol_report",
		"alertrule_list", "alertrule_save", "alertrule_delete",
		"notify_channels", "notify_channel_save", "notify_records",
		"service_exec", "node_exec",
	}
	for _, name := range want {
		if !got[name] {
			t.Errorf("tool %q missing from tools/list (have %d tools)", name, len(resp.Tools))
		}
	}
	if len(resp.Tools) != 60 {
		t.Errorf("tool count = %d, want 60 (P1 39 + P2 2 + P3 19)", len(resp.Tools))
	}

	// resources/list：P3 只读资源视图（4 清单 + 1 模板）
	res, err := session.ListResources(ctx, nil)
	if err != nil {
		t.Fatalf("resources/list failed: %v", err)
	}
	uris := map[string]bool{}
	for _, r := range res.Resources {
		uris[r.URI] = true
	}
	for _, want := range []string{
		"opsguard://clusters", "opsguard://alerts/active",
		"opsguard://patrols", "opsguard://images",
	} {
		if !uris[want] {
			t.Errorf("resource %q missing (have %d)", want, len(res.Resources))
		}
	}
	if len(res.Resources) < 4 {
		t.Errorf("resource count = %d, want >= 4", len(res.Resources))
	}
	// 资源模板随 templates/list 宣告
	tres, err := session.ListResourceTemplates(ctx, nil)
	if err != nil {
		t.Fatalf("resource templates/list failed: %v", err)
	}
	if len(tres.ResourceTemplates) < 1 {
		t.Errorf("resource templates = %d, want >= 1 (opsguard://clusters/{name}/services)", len(tres.ResourceTemplates))
	}
}

func TestSmokeUnauthorized(t *testing.T) {
	secret := "smoke-secret"
	srv := startSmokeServer(t, secret)

	// 无 token 客户端：initialize 应被 401 拒绝
	cli := mcp.NewClient(&mcp.Implementation{Name: "anon", Version: "0"}, nil)
	tr := &mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp"}
	if _, err := cli.Connect(context.Background(), tr, nil); err == nil {
		t.Fatal("connect without token must fail")
	}
}

func TestSmokeServerInstructions(t *testing.T) {
	h := New(Deps{Config: Config{Tokens: []TokenConfig{{Name: "t", Secret: "s", Scope: "read"}}}, Log: testLogger()})
	_ = h
	// Instructions 是给模型的运行手册：确认关键守则都在
	for _, want := range []string{"confirm=true", "cluster_list", "build_upload_begin", "investigation_start"} {
		// 直接断言常量内容（server.Instructions 无公开 getter，这里靠源码常量）
		if !strings.Contains(serverInstructions, want) {
			t.Errorf("instructions missing %q", want)
		}
	}
}
