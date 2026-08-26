package orchestrator

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/config"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/docker"
)

// TestMonitorConfigStoreRoundTrip Put/All/Delete 往返；禁用配置同样持久化
// （重启不得复活已禁用的监控）；文件不存在视为空。
func TestMonitorConfigStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "monitoring.json")
	s := NewMonitorConfigStore(path)

	all, err := s.All()
	if err != nil || len(all) != 0 {
		t.Fatalf("missing file should yield empty map, got %+v err=%v", all, err)
	}

	enabled := config.Monitoring{Enabled: true, PortChecks: []config.PortCheck{{Port: "8080"}}}
	disabled := config.Monitoring{Enabled: false}
	if err := s.Put("web", enabled); err != nil {
		t.Fatalf("put web: %v", err)
	}
	if err := s.Put("db", disabled); err != nil {
		t.Fatalf("put db: %v", err)
	}
	all, err = s.All()
	if err != nil || len(all) != 2 {
		t.Fatalf("all: %+v err=%v", all, err)
	}
	if !all["web"].Enabled || all["web"].PortChecks[0].Port != "8080" {
		t.Fatalf("web config lost: %+v", all["web"])
	}
	if all["db"].Enabled {
		t.Fatal("db disabled config should persist as disabled")
	}

	if err := s.Delete("web"); err != nil {
		t.Fatalf("delete web: %v", err)
	}
	if err := s.Delete("web"); err != nil {
		t.Fatalf("delete missing should be no-op: %v", err)
	}
	all, err = s.All()
	if err != nil || len(all) != 1 {
		t.Fatalf("after delete: %+v err=%v", all, err)
	}
}

// fakeLifecycleDocker 为 Deploy/Update/Remove 持久化测试提供最小可用环境：
// swarm manager 角色 + 可控的服务列表。
type fakeLifecycleDocker struct {
	fakeHealthDocker
	services []docker.Service
}

func (f *fakeLifecycleDocker) Info(context.Context) (docker.Info, error) {
	return docker.Info{Swarm: docker.SwarmInfo{NodeID: "n1", ControlAvailable: true}}, nil
}

func (f *fakeLifecycleDocker) ListServices(context.Context, docker.Filter) ([]docker.Service, error) {
	return f.services, nil
}

// fakeRegistrar 记录监控注册/注销调用。
type fakeRegistrar struct {
	mu         sync.Mutex
	registered map[string]config.Monitoring
	unreg      []string
}

func newFakeRegistrar() *fakeRegistrar {
	return &fakeRegistrar{registered: map[string]config.Monitoring{}}
}

func (f *fakeRegistrar) Register(service string, cfg *config.Monitoring) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if cfg != nil {
		f.registered[service] = *cfg
	}
}

func (f *fakeRegistrar) Unregister(service string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unreg = append(f.unreg, service)
}

func newLifecycleOrch(t *testing.T, d docker.Client) (*Orchestrator, *MonitorConfigStore, *fakeRegistrar) {
	t.Helper()
	monCfg := NewMonitorConfigStore(filepath.Join(t.TempDir(), "monitoring.json"))
	reg := newFakeRegistrar()
	o := New(d, NewOperationStore(10), nil)
	o.SetReadyTimeout(50 * time.Millisecond)
	o.SetMonitor(reg)
	o.SetMonitorConfigStore(monCfg)
	return o, monCfg, reg
}

func lifecycleConfig(name string, enabled bool) *config.Config {
	return &config.Config{
		Service:    config.Service{Name: name, Image: "nginx:alpine"},
		Monitoring: config.Monitoring{Enabled: enabled, PortChecks: []config.PortCheck{{Port: "8080"}}},
	}
}

// TestLifecyclePersistsMonitoring Deploy 落盘监控配置；Update 覆盖（含禁用）；
// Remove 清除。
func TestLifecyclePersistsMonitoring(t *testing.T) {
	o, monCfg, _ := newLifecycleOrch(t, &fakeLifecycleDocker{})
	ctx := context.Background()

	if _, err := o.Deploy(ctx, lifecycleConfig("web", true)); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	all, err := monCfg.All()
	if err != nil || len(all) != 1 || !all["web"].Enabled {
		t.Fatalf("deploy should persist enabled config: %+v err=%v", all, err)
	}

	// Update 下发禁用配置（管理端级联删除路径）→ 覆盖为禁用
	if _, err := o.Update(ctx, "web", lifecycleConfig("web", false)); err != nil {
		t.Fatalf("update: %v", err)
	}
	all, err = monCfg.All()
	if err != nil || len(all) != 1 || all["web"].Enabled {
		t.Fatalf("update should overwrite with disabled config: %+v err=%v", all, err)
	}

	if _, err := o.Remove(ctx, "web"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	all, err = monCfg.All()
	if err != nil || len(all) != 0 {
		t.Fatalf("remove should delete persisted config: %+v err=%v", all, err)
	}
}

// TestRestoreMonitors 启动重建：仍存活且启用的恢复注册；禁用的跳过；
// 服务已不存在的条目被清除。
func TestRestoreMonitors(t *testing.T) {
	d := &fakeLifecycleDocker{
		services: []docker.Service{
			{Spec: docker.ServiceSpec{Name: "web"}},
			{Spec: docker.ServiceSpec{Name: "db"}},
		},
	}
	o, monCfg, reg := newLifecycleOrch(t, d)

	// 预置持久化状态：web 启用、db 禁用、ghost 启用但服务已不存在
	if err := monCfg.Put("web", config.Monitoring{Enabled: true, PortChecks: []config.PortCheck{{Port: "8080"}}}); err != nil {
		t.Fatalf("put web: %v", err)
	}
	if err := monCfg.Put("db", config.Monitoring{Enabled: false}); err != nil {
		t.Fatalf("put db: %v", err)
	}
	if err := monCfg.Put("ghost", config.Monitoring{Enabled: true}); err != nil {
		t.Fatalf("put ghost: %v", err)
	}

	restored, pruned, err := o.RestoreMonitors(context.Background())
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if restored != 1 || pruned != 1 {
		t.Fatalf("want restored=1 pruned=1, got restored=%d pruned=%d", restored, pruned)
	}
	reg.mu.Lock()
	defer reg.mu.Unlock()
	if len(reg.registered) != 1 {
		t.Fatalf("only web should be re-registered, got %+v", reg.registered)
	}
	if _, ok := reg.registered["web"]; !ok {
		t.Fatalf("web not re-registered: %+v", reg.registered)
	}
	all, err := monCfg.All()
	if err != nil {
		t.Fatalf("all: %v", err)
	}
	if _, ok := all["ghost"]; ok {
		t.Fatal("ghost entry should be pruned")
	}
	if len(all) != 2 {
		t.Fatalf("web/db should remain persisted, got %+v", all)
	}
}

// TestRestoreMonitorsNoStore 未配置持久化存储时为无害空操作。
func TestRestoreMonitorsNoStore(t *testing.T) {
	o := New(&fakeLifecycleDocker{}, NewOperationStore(10), nil)
	restored, pruned, err := o.RestoreMonitors(context.Background())
	if err != nil || restored != 0 || pruned != 0 {
		t.Fatalf("no-store restore should be a no-op, got %d/%d err=%v", restored, pruned, err)
	}
}
