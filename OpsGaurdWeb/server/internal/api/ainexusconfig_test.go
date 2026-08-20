package api

import (
	"testing"

	ainexuscfg "gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/config"
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
	cfg, err := req.toConfig(nil)
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

// TestToConfigCarriesModelEnabled 旧编辑页未提交模型 enabled（nil）时
// 沿用已保存的启停状态；显式提交以提交值为准；新模型默认启用。
func TestToConfigCarriesModelEnabled(t *testing.T) {
	old := &ainexuscfg.Config{
		Providers: []ainexuscfg.ProviderConfig{{
			Name: "p1",
			Models: []ainexuscfg.ModelConfig{
				{Name: "keep-off", Enabled: false},
				{Name: "keep-on", Enabled: true},
			},
		}},
	}
	no := false
	req := ainexusConfigRequest{
		Providers: []ainexusProviderRequest{{
			Name: "p1",
			Models: []ainexusModelRequest{
				{Name: "keep-off"},                  // 旧页面未提交 → 沿用 false
				{Name: "keep-on"},                   // 旧页面未提交 → 沿用 true
				{Name: "explicit-off", Enabled: &no}, // 显式提交 → 以提交为准
				{Name: "new-model"},                 // 新模型 → 默认启用
			},
		}},
	}
	cfg, err := req.toConfig(carryModelEnabled(old))
	if err != nil {
		t.Fatalf("toConfig: %v", err)
	}
	models := cfg.Providers[0].Models
	want := []struct {
		name string
		on   bool
	}{
		{"keep-off", false}, {"keep-on", true}, {"explicit-off", false}, {"new-model", true},
	}
	if len(models) != len(want) {
		t.Fatalf("expected %d models, got %d", len(want), len(models))
	}
	for i, w := range want {
		if models[i].Name != w.name || models[i].Enabled != w.on {
			t.Fatalf("model %q: enabled=%v, want %v", models[i].Name, models[i].Enabled, w.on)
		}
	}
}
