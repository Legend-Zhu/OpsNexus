package api

import (
	"testing"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/cluster"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
)

// newTestHandlers 构造带集群服务的 Handlers（临时 store，返回可播种的 store）。
func newTestHandlers(t *testing.T) (*Handlers, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return &Handlers{clusters: cluster.New(st)}, st
}

// TestClusterMCPServers 集群 manager 自动 MCP 合并：有 mcp_url 的集群返回
// cluster 标记条目，无 mcp_url 的集群跳过。
func TestClusterMCPServers(t *testing.T) {
	h, st := newTestHandlers(t)
	if err := st.PutCluster(&store.Cluster{Name: "dev", MCPURL: "http://dev:8080/mcp"}); err != nil {
		t.Fatalf("seed dev: %v", err)
	}
	if err := st.PutCluster(&store.Cluster{Name: "bare"}); err != nil {
		t.Fatalf("seed bare: %v", err)
	}

	views := h.clusterMCPServers()
	if len(views) != 1 {
		t.Fatalf("expected 1 cluster MCP, got %d: %+v", len(views), views)
	}
	v := views[0]
	if v.Name != "cluster:dev" || !v.Cluster || v.URL != "http://dev:8080/mcp" || v.Transport != "streamable-http" {
		t.Fatalf("unexpected cluster MCP view: %+v", v)
	}
}

// TestClusterMCPServersNilSvc 集群服务未初始化时静默返回空（不 panic）。
func TestClusterMCPServersNilSvc(t *testing.T) {
	h := &Handlers{}
	if got := h.clusterMCPServers(); got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}
}

// TestToConfigFiltersClusterMCP PUT 保存时过滤 cluster: 前缀的自动 MCP 条目，
// 避免与服务端自动连接重复。
func TestToConfigFiltersClusterMCP(t *testing.T) {
	req := ainexusConfigRequest{
		MCPServers: []ainexusMCPServerRequest{
			{Name: "cluster:dev", Transport: "streamable-http", URL: "http://dev:8080/mcp"},
			{Name: "custom-srv", Transport: "streamable-http", URL: "http://x:8080/mcp"},
		},
	}
	cfg, err := req.toConfig()
	if err != nil {
		t.Fatalf("toConfig: %v", err)
	}
	if len(cfg.MCPServers) != 1 {
		t.Fatalf("expected 1 MCP after filtering, got %d", len(cfg.MCPServers))
	}
	if cfg.MCPServers[0].Name != "custom-srv" {
		t.Fatalf("expected custom-srv to survive, got %q", cfg.MCPServers[0].Name)
	}
}
