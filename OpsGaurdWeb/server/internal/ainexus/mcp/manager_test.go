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
