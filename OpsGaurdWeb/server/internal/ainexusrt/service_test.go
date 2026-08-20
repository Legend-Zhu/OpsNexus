package ainexusrt

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	ainexuscfg "gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/config"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/usage"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/cluster"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
)

// newTestService 构造临时目录上的运行时服务，初始为「未启用」。
func newTestService(t *testing.T) *Service {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	svc := New(st, cluster.New(st), &ainexuscfg.Config{})
	if err := svc.Init(context.Background()); err != nil {
		t.Fatalf("init: %v", err)
	}
	return svc
}

// validConfig 构造一个可用的网关配置（enabled + 单 provider）。
func validConfig(enabled bool) *ainexuscfg.Config {
	return &ainexuscfg.Config{
		Enabled: enabled,
		Providers: []ainexuscfg.ProviderConfig{{
			Name:    "test",
			Type:    ainexuscfg.ProviderTypeOpenAI,
			BaseURL: "http://127.0.0.1:1/v1", // 不可达，仅测构建/路由
			APIKey:  "sk-test",
			Models:  []ainexuscfg.ModelConfig{{Name: "m1"}},
		}},
	}
}

// TestInitDisabled 未启用时网关为 nil，启用后非 nil 且模型路由生效。
func TestInitDisabled(t *testing.T) {
	svc := newTestService(t)
	if got := svc.Server(); got != nil {
		t.Fatalf("Server() = %v, want nil (disabled)", got)
	}
	if err := svc.Update(context.Background(), validConfig(true)); err != nil {
		t.Fatalf("update enabled: %v", err)
	}
	srv := svc.Server()
	if srv == nil {
		t.Fatal("Server() = nil, want non-nil after enabling")
	}
	if models := srv.Models(); len(models) != 1 || models[0] != "m1" {
		t.Fatalf("Models() = %v, want [m1]", models)
	}
}

// TestHotReload 热重载换新网关：模型列表随配置变化。
func TestHotReload(t *testing.T) {
	svc := newTestService(t)
	if err := svc.Update(context.Background(), validConfig(true)); err != nil {
		t.Fatalf("first update: %v", err)
	}
	first := svc.Server()

	cfg := validConfig(true)
	cfg.Providers[0].Models = append(cfg.Providers[0].Models, ainexuscfg.ModelConfig{Name: "m2"})
	if err := svc.Update(context.Background(), cfg); err != nil {
		t.Fatalf("second update: %v", err)
	}
	second := svc.Server()
	if second == nil {
		t.Fatal("Server() = nil after reload")
	}
	if first == second {
		t.Fatal("expected a new gateway instance after reload")
	}
	if models := second.Models(); len(models) != 2 {
		t.Fatalf("Models() = %v, want [m1 m2]", models)
	}
}

// TestCarryAPIKey 空白 api_key 沿用旧值。
func TestCarryAPIKey(t *testing.T) {
	svc := newTestService(t)
	if err := svc.Update(context.Background(), validConfig(true)); err != nil {
		t.Fatalf("first update: %v", err)
	}

	cfg := validConfig(true)
	cfg.Providers[0].APIKey = "" // 留空 = 保持
	if err := svc.Update(context.Background(), cfg); err != nil {
		t.Fatalf("update with empty api_key: %v", err)
	}
	got := svc.Config()
	if got.Providers[0].APIKey != "sk-test" {
		t.Fatalf("api_key = %q, want carried %q", got.Providers[0].APIKey, "sk-test")
	}
}

// TestInvalidConfigKeepsOldGateway 非法配置 Update 报错，旧网关继续服务。
func TestInvalidConfigKeepsOldGateway(t *testing.T) {
	svc := newTestService(t)
	if err := svc.Update(context.Background(), validConfig(true)); err != nil {
		t.Fatalf("first update: %v", err)
	}
	before := svc.Server()

	// 无 provider 且 enabled → 非法
	if err := svc.Update(context.Background(), &ainexuscfg.Config{Enabled: true}); err == nil {
		t.Fatal("expected error for provider-less config")
	}
	if after := svc.Server(); after != before {
		t.Fatal("gateway must survive a failed reload")
	}
}

// TestDefaultModelValidated 网关未启用时默认模型也须属于模型池。
func TestDefaultModelValidated(t *testing.T) {
	svc := newTestService(t)
	if err := svc.Update(context.Background(), validConfig(false)); err != nil {
		t.Fatalf("seed pool: %v", err)
	}

	// 未启用 + 默认模型不在模型池 → 拒绝
	bad := validConfig(false)
	bad.Providers[0].Models = []ainexuscfg.ModelConfig{{Name: "m1"}}
	bad.DefaultModel = "ghost"
	if err := svc.Update(context.Background(), bad); err == nil {
		t.Fatal("expected error for default model not in pool")
	}
	// 未启用 + 默认模型在模型池 → 允许
	good := validConfig(false)
	good.Providers[0].Models = []ainexuscfg.ModelConfig{{Name: "m1"}}
	good.DefaultModel = "m1"
	if err := svc.Update(context.Background(), good); err != nil {
		t.Fatalf("expected ok for in-pool default model, got: %v", err)
	}
	if cfg := svc.Config(); cfg.DefaultModel != "m1" {
		t.Fatalf("DefaultModel = %q, want m1", cfg.DefaultModel)
	}
}

// TestDisable 关闭网关后 Server() 为 nil，配置仍保存。
func TestDisable(t *testing.T) {
	svc := newTestService(t)
	if err := svc.Update(context.Background(), validConfig(true)); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if err := svc.Update(context.Background(), validConfig(false)); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if got := svc.Server(); got != nil {
		t.Fatal("Server() = non-nil after disabling")
	}
	if cfg := svc.Config(); cfg.Enabled {
		t.Fatal("config must stay persisted as disabled")
	}
}

// TestPersistence 保存后重启（新服务实例读同一数据目录）仍加载运行时配置。
func TestPersistence(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	svc := New(st, cluster.New(st), &ainexuscfg.Config{})
	if err := svc.Init(context.Background()); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := svc.Update(context.Background(), validConfig(true)); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	st2, err := store.Open(dir)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer st2.Close()
	svc2 := New(st2, cluster.New(st2), &ainexuscfg.Config{})
	if err := svc2.Init(context.Background()); err != nil {
		t.Fatalf("re-init: %v", err)
	}
	if srv := svc2.Server(); srv == nil {
		t.Fatal("Server() = nil after restart, runtime config not loaded")
	}
}

// TestConfigCopyIsolated 返回的配置拷贝与内部互不影响。
func TestConfigCopyIsolated(t *testing.T) {
	svc := newTestService(t)
	if err := svc.Update(context.Background(), validConfig(true)); err != nil {
		t.Fatalf("first update: %v", err)
	}
	got := svc.Config()
	got.Providers[0].Models = nil
	if cfg := svc.Config(); len(cfg.Providers[0].Models) != 1 {
		t.Fatal("mutating returned config leaked into internal state")
	}
}

// openaiStub 最小 OpenAI 兼容 /chat/completions 应答（非流式，带 usage）。
func openaiStub(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"r1","model":"m1","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop","index":0}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`))
	}))
}

// TestUsageSinkSurvivesHotReload 计量 sink 由外层服务持有：热重载换新网关
// 实例后，同一 sink 继续收到底层调用记录，场景归属 patrol_report。
func TestUsageSinkSurvivesHotReload(t *testing.T) {
	stub := openaiStub(t)
	defer stub.Close()

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	cfg := validConfig(true)
	cfg.Providers[0].BaseURL = stub.URL
	cfg.Agent = ainexuscfg.AgentConfig{MaxToolRounds: 2}

	var mu sync.Mutex
	var records []usage.Record
	sink := usage.SinkFunc(func(r usage.Record) {
		mu.Lock()
		defer mu.Unlock()
		records = append(records, r)
	})

	svc := New(st, cluster.New(st), &ainexuscfg.Config{})
	svc.SetUsageSink(sink)
	if err := svc.Init(context.Background()); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := svc.Update(context.Background(), cfg); err != nil {
		t.Fatalf("enable: %v", err)
	}

	srv := svc.Server()
	if _, err := srv.Summarize("m1", "巡检报告素材"); err != nil {
		t.Fatalf("summarize: %v", err)
	}

	// 热重载（同配置再保存）→ 新网关实例，sink 不变
	if err := svc.Update(context.Background(), cfg); err != nil {
		t.Fatalf("reload: %v", err)
	}
	srv2 := svc.Server()
	if srv == srv2 {
		t.Fatal("expected a new gateway instance after reload")
	}
	if _, err := srv2.Summarize("m1", "再次巡检"); err != nil {
		t.Fatalf("summarize after reload: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(records) != 2 {
		t.Fatalf("expected 2 usage records across reload, got %d", len(records))
	}
	for _, r := range records {
		if r.Scenario != usage.ScenarioPatrolReport || r.Model != "m1" || !r.OK || !r.UsagePresent {
			t.Fatalf("unexpected record: %+v", r)
		}
	}
	if records[0].OperationID == "" || records[0].OperationID == records[1].OperationID {
		t.Fatalf("each summarize should be its own operation: %q vs %q",
			records[0].OperationID, records[1].OperationID)
	}
}
