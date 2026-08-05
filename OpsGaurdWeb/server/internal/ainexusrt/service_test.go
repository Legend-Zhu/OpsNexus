package ainexusrt

import (
	"context"
	"testing"

	ainexuscfg "gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/config"
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
		t.Fatalf("update: %v", err)
	}
	got := svc.Config()
	got.Providers[0].Models = nil
	if cfg := svc.Config(); len(cfg.Providers[0].Models) != 1 {
		t.Fatal("mutating returned config leaked into internal state")
	}
}
