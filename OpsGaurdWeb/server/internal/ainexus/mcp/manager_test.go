package mcp

import (
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/config"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/provider"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/tool"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

var openAIFunctionName = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

func TestMCPToolNameSanitized(t *testing.T) {
	cases := []struct {
		server, tool, want string
	}{
		// 集群 server 名的冒号必须被清洗（OpenAI function name 不允许 :）
		{"cluster:prod", "list_services", "mcp_cluster_prod_list_services"},
		{"cluster:dev", "exec_host_command", "mcp_cluster_dev_exec_host_command"},
		// 已带 mcp_ 前缀的远端工具不重复加前缀，但仍需清洗
		{"cluster:prod", "mcp_already-prefixed", "mcp_already-prefixed"},
		// 连续非法字符折叠成一个 _
		{"my server!!v2", "do_thing", "mcp_my_server_v2_do_thing"},
		// 非 ASCII 集群名清洗后整段折叠、不同集群会同名——必须追加 server 名
		// 指纹后缀（fnv32a 前 8 位）保唯一
		{"cluster:自然灾害集群", "list_services", "mcp_cluster__list_services_b786ef06"},
		{"cluster:应急指挥集群", "list_services", "mcp_cluster__list_services_cc825686"},
	}
	for _, c := range cases {
		got := (&MCPTool{serverName: c.server, toolName: c.tool}).Name()
		if got != c.want {
			t.Errorf("Name(%q, %q) = %q, want %q", c.server, c.tool, got, c.want)
		}
		if !openAIFunctionName.MatchString(got) {
			t.Errorf("Name(%q, %q) = %q 不符合 OpenAI function name 规范", c.server, c.tool, got)
		}
	}
}

// TestToolScopeFilter 会话级集群 scoping：只放行内置工具与目标集群的工具。
func TestToolScopeFilter(t *testing.T) {
	filter := ToolScopeFilter("cluster:自然灾害集群")
	tool := func(desc string) provider.ToolDefinition { return provider.ToolDefinition{Description: desc} }
	cases := []struct {
		desc string
		want bool
	}{
		{"[MCP:cluster:自然灾害集群] list services", true},
		{"[MCP:cluster:应急指挥集群] exec host command", false},
		{"Send an HTTP request to a specified URL", true}, // 内置工具
	}
	for _, c := range cases {
		if got := filter(tool(c.desc)); got != c.want {
			t.Errorf("filter(%q) = %v, want %v", c.desc, got, c.want)
		}
	}
}

// newTestUpstream 起一个带 list_services 工具的本地 MCP server（streamable-http），
// 返回测试 server 与可变上游（运行期可 AddTool/RemoveTool 模拟 Worker 工具变更）。
func newTestUpstream(t *testing.T, extra ...string) (*httptest.Server, *mcpserver.MCPServer) {
	t.Helper()
	upstream := mcpserver.NewMCPServer("test", "0.0.1")
	upstream.AddTool(mcp.NewTool("list_services"), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("ok"), nil
	})
	for _, name := range extra {
		n := name
		upstream.AddTool(mcp.NewTool(n), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText("ok"), nil
		})
	}
	mcpHTTP := mcpserver.NewStreamableHTTPServer(upstream)
	ts := httptest.NewServer(http.HandlerFunc(mcpHTTP.ServeHTTP))
	return ts, upstream
}

// TestRemoveServerUnregistersTools 移除 server 必须同步注销其工具，
// 防止连接已断的"幽灵工具"继续暴露给模型；重连后工具恢复。
func TestRemoveServerUnregistersTools(t *testing.T) {
	ts, _ := newTestUpstream(t)
	defer ts.Close()

	mgr := NewManager(log.Default())
	defer mgr.Close()
	reg := tool.NewRegistry()
	mgr.SetRegistry(reg)

	cfg := config.MCPServerConfig{Name: "cluster:prod", Transport: "streamable-http", URL: ts.URL + "/mcp"}
	if err := mgr.AddServer(context.Background(), cfg); err != nil {
		t.Fatalf("AddServer: %v", err)
	}
	if err := mgr.RegisterAllTools(reg); err != nil {
		t.Fatalf("RegisterAllTools: %v", err)
	}
	if reg.ToolCount() != 1 {
		t.Fatalf("after add: %d tools, want 1", reg.ToolCount())
	}

	if err := mgr.RemoveServer("cluster:prod"); err != nil {
		t.Fatalf("RemoveServer: %v", err)
	}
	if got := mgr.ServerNames(); len(got) != 0 {
		t.Fatalf("server names after remove = %v, want empty", got)
	}
	if reg.ToolCount() != 0 {
		t.Fatalf("tools after remove = %d, want 0（幽灵工具）", reg.ToolCount())
	}

	// 幂等
	if err := mgr.RemoveServer("cluster:prod"); err != nil {
		t.Fatalf("idempotent RemoveServer: %v", err)
	}

	// 重连后工具恢复
	if err := mgr.AddServer(context.Background(), cfg); err != nil {
		t.Fatalf("re-AddServer: %v", err)
	}
	if err := mgr.RegisterAllTools(reg); err != nil {
		t.Fatalf("re-RegisterAllTools: %v", err)
	}
	if reg.ToolCount() != 1 {
		t.Fatalf("after re-add: %d tools, want 1", reg.ToolCount())
	}
}

// TestHealthCheckSyncsToolChanges 健康检查成功路径：Worker 侧新增/移除
// 工具时，registry 增量同步（新增注册、移除注销）。
func TestHealthCheckSyncsToolChanges(t *testing.T) {
	ts, upstream := newTestUpstream(t)
	defer ts.Close()

	mgr := NewManager(log.Default())
	defer mgr.Close()
	reg := tool.NewRegistry()
	mgr.SetRegistry(reg)

	if err := mgr.AddServer(context.Background(), config.MCPServerConfig{
		Name: "cluster:prod", Transport: "streamable-http", URL: ts.URL + "/mcp",
	}); err != nil {
		t.Fatalf("AddServer: %v", err)
	}
	if err := mgr.RegisterAllTools(reg); err != nil {
		t.Fatalf("RegisterAllTools: %v", err)
	}

	// Worker 侧新增工具 → 单轮健康检查后注册
	upstream.AddTool(mcp.NewTool("get_service"), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("ok"), nil
	})
	mgr.healthCheckOnce()
	if reg.ToolCount() != 2 {
		t.Fatalf("after tool add: %d tools, want 2", reg.ToolCount())
	}

	// Worker 侧移除工具 → 单轮健康检查后注销
	upstream.DeleteTools("get_service")
	mgr.healthCheckOnce()
	if reg.ToolCount() != 1 {
		t.Fatalf("after tool removal: %d tools, want 1（幽灵工具未注销）", reg.ToolCount())
	}
}

// TestHealthCheckRebuildsOnFailure 连接失效（Worker 重启）后，健康检查应
// 摘除该集群工具并保留 server 条目（下轮重试重建）。
func TestHealthCheckRebuildsOnFailure(t *testing.T) {
	ts, _ := newTestUpstream(t)

	mgr := NewManager(log.Default())
	defer mgr.Close()
	reg := tool.NewRegistry()
	mgr.SetRegistry(reg)

	if err := mgr.AddServer(context.Background(), config.MCPServerConfig{
		Name: "cluster:prod", Transport: "streamable-http", URL: ts.URL + "/mcp",
	}); err != nil {
		t.Fatalf("AddServer: %v", err)
	}
	if err := mgr.RegisterAllTools(reg); err != nil {
		t.Fatalf("RegisterAllTools: %v", err)
	}

	// 模拟 Worker 重启：上游不可达
	ts.Close()

	mgr.healthCheckOnce()
	if reg.ToolCount() != 0 {
		t.Fatalf("after outage: %d tools, want 0（不可达集群的工具应摘除）", reg.ToolCount())
	}
	if got := mgr.ServerNames(); len(got) != 1 {
		t.Fatalf("server entry should be kept for retry, got %v", got)
	}
}

// TestMCPToolNameUniqueAcrossChineseClusters 中文集群名的工具清洗后不得同名，
// 否则 Registry 拒绝重名注册、后接入集群的 20 个工具全部对 agent 不可见。
func TestMCPToolNameUniqueAcrossChineseClusters(t *testing.T) {
	r := tool.NewRegistry()
	for _, srv := range []string{"cluster:自然灾害集群", "cluster:应急指挥集群"} {
		for _, tn := range []string{"list_services", "get_service_logs", "check_http"} {
			mt := &MCPTool{serverName: srv, toolName: tn}
			if err := r.Register(mt); err != nil {
				t.Fatalf("register %s/%s: %v", srv, tn, err)
			}
		}
	}
	if got := r.ToolCount(); got != 6 {
		t.Fatalf("registry tool count = %d, want 6（两个中文集群的工具必须共存）", got)
	}
}

func TestMCPToolNameTruncation(t *testing.T) {
	long := strings.Repeat("a", 100)
	toolA := (&MCPTool{serverName: "s", toolName: long + "x"}).Name()
	toolB := (&MCPTool{serverName: "s", toolName: long + "y"}).Name()

	if len(toolA) != openAIToolNameMaxLen {
		t.Fatalf("len = %d, want %d", len(toolA), openAIToolNameMaxLen)
	}
	if !openAIFunctionName.MatchString(toolA) {
		t.Fatalf("截断后仍含非法字符: %q", toolA)
	}
	// 前 64 字符相同但原文不同的名字，哈希后缀必须区分开
	if toolA == toolB {
		t.Fatalf("超长名字哈希后缀未保唯一: %q", toolA)
	}
}

// TestAddServerSendsHeaders 端到端验证 cfg.Headers（如集群 Bearer token）
// 真正随 MCP Initialize/ListTools 请求发出——此前 createStreamableHTTPClient
// 完全丢弃 Headers，Worker 启用 auth 时会 401。
func TestAddServerSendsHeaders(t *testing.T) {
	const token = "test-secret-token"

	upstream := mcpserver.NewMCPServer("test", "0.0.1")
	upstream.AddTool(mcp.NewTool("list_services"), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("ok"), nil
	})
	mcpHTTP := mcpserver.NewStreamableHTTPServer(upstream)

	var sawAuth bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer "+token {
			sawAuth = true
		}
		mcpHTTP.ServeHTTP(w, r)
	}))
	defer ts.Close()

	mgr := NewManager(log.Default())
	defer mgr.Close()
	err := mgr.AddServer(context.Background(), config.MCPServerConfig{
		Name:      "cluster:prod",
		Transport: "streamable-http",
		URL:       ts.URL + "/mcp",
		Headers:   map[string]string{"Authorization": "Bearer " + token},
	})
	if err != nil {
		t.Fatalf("AddServer: %v", err)
	}
	if !sawAuth {
		t.Fatal("MCP 请求未携带 Authorization header")
	}

	// 工具应以清洗后的名字可见，且 Description 保留原始集群标识
	tools := mgr.Tools()
	if len(tools) != 1 {
		t.Fatalf("tools = %d, want 1", len(tools))
	}
	if got := tools[0].Name(); got != "mcp_cluster_prod_list_services" {
		t.Fatalf("tool name = %q", got)
	}
	if !strings.Contains(tools[0].Description(), "[MCP:cluster:prod]") {
		t.Fatalf("description 未保留集群标识: %q", tools[0].Description())
	}
}
