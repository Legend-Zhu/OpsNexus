package nodemon

import (
	"context"
	"path/filepath"
	"testing"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ingest"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
)

// newTestService 构造带真实 store/ingest 的调度器（notifier 为 nil 的纯落库
// 模式），evaluate 的翻转事件经 ingest.HandleEvent 断言。
func newTestService(t *testing.T) (*Service, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "nodemon-test"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ingestSvc := ingest.New(st, nil)
	return New(st, nil, ingestSvc, nil), st
}

func TestEvaluateTransitionsOnce(t *testing.T) {
	svc, st := newTestService(t)
	nm := &store.NodeMonitoring{Enabled: true, CPUThreshold: 90, MemThreshold: 85}
	hostnames := map[string]string{"n1": "web1"}
	sample := func(cpu, mem float64) map[string]nodeSample {
		return map[string]nodeSample{"n1": {hostname: "web1", reachable: true, cpuPercent: cpu, memPercent: mem}}
	}

	// 首轮超限 → cpu/内存各一条 over 事件，聚合为 (cluster, node/web1,
	// resource_over) 一条告警，count=2。
	svc.evaluate("prod", nm, hostnames, sample(95, 90))
	alert, err := st.GetAlert(store.AlertID("prod", "node/web1", store.EventResourceOver))
	if err != nil {
		t.Fatalf("get alert: %v", err)
	}
	if alert == nil || alert.Status != store.AlertActive || alert.Count != 2 {
		t.Fatalf("expected active alert (count=2) after over transition, got %+v", alert)
	}

	// 持续超限（无翻转）→ 不重复发事件，告警计数不变。
	svc.evaluate("prod", nm, hostnames, sample(96, 91))
	alert, _ = st.GetAlert(store.AlertID("prod", "node/web1", store.EventResourceOver))
	if alert == nil || alert.Count != 2 {
		t.Fatalf("expected count stay 2 without transition, got %+v", alert)
	}

	// 全部回落 → 恢复事件关闭告警。
	svc.evaluate("prod", nm, hostnames, sample(50, 60))
	alert, _ = st.GetAlert(store.AlertID("prod", "node/web1", store.EventResourceOver))
	if alert == nil || alert.Status != store.AlertRecovered {
		t.Fatalf("expected recovered alert after recover transition, got %+v", alert)
	}
}

func TestEvaluateUnreachableAndDisabledMetric(t *testing.T) {
	svc, st := newTestService(t)
	// 只配 CPU 阈值：内存再高也不触发。
	nm := &store.NodeMonitoring{Enabled: true, CPUThreshold: 90}
	hostnames := map[string]string{"n1": "web1", "n2": "db1"}
	samples := map[string]nodeSample{
		// n1 可达但 CPU 未超；n2 不可达（样本占位，reachable=false）。
		"n1": {hostname: "web1", reachable: true, cpuPercent: 10, memPercent: 99},
		"n2": {hostname: "db1", reachable: false},
	}
	svc.evaluate("prod", nm, hostnames, samples)
	alerts, err := st.ListAlerts(store.AlertActive, "prod")
	if err != nil {
		t.Fatalf("list alerts: %v", err)
	}
	if len(alerts) != 0 {
		t.Fatalf("expected no alerts, got %+v", alerts)
	}

	// n1 CPU 超限；n2 恢复可达后 CPU 超限 → 两个节点各自一条告警。
	samples["n1"] = nodeSample{hostname: "web1", reachable: true, cpuPercent: 95}
	samples["n2"] = nodeSample{hostname: "db1", reachable: true, cpuPercent: 97}
	svc.evaluate("prod", nm, hostnames, samples)
	for _, svcName := range []string{"node/web1", "node/db1"} {
		a, _ := st.GetAlert(store.AlertID("prod", svcName, store.EventResourceOver))
		if a == nil || a.Status != store.AlertActive {
			t.Fatalf("expected active alert for %s, got %+v", svcName, a)
		}
	}
}

func TestEvaluateNodeRemovedRecovers(t *testing.T) {
	svc, st := newTestService(t)
	nm := &store.NodeMonitoring{Enabled: true, CPUThreshold: 90}

	hostnames := map[string]string{"n1": "web1", "n2": "db1"}
	svc.evaluate("prod", nm, hostnames, map[string]nodeSample{
		"n1": {hostname: "web1", reachable: true, cpuPercent: 95},
		"n2": {hostname: "db1", reachable: true, cpuPercent: 96},
	})
	// n2 从清单摘除（本轮 init 只剩 n1）→ 其告警自动恢复、状态清除。
	svc.evaluate("prod", nm, map[string]string{"n1": "web1"}, map[string]nodeSample{
		"n1": {hostname: "web1", reachable: true, cpuPercent: 95},
	})
	a, _ := st.GetAlert(store.AlertID("prod", "node/db1", store.EventResourceOver))
	if a == nil || a.Status != store.AlertRecovered {
		t.Fatalf("expected db1 alert recovered after node removal, got %+v", a)
	}
	// 状态清除后 n2 不再产生新事件（即使样本还在也不评估）。
	svc.evaluate("prod", nm, map[string]string{"n1": "web1"}, map[string]nodeSample{
		"n1": {hostname: "web1", reachable: true, cpuPercent: 95},
		"n2": {hostname: "db1", reachable: true, cpuPercent: 96},
	})
	a, _ = st.GetAlert(store.AlertID("prod", "node/db1", store.EventResourceOver))
	if a == nil || a.Status != store.AlertRecovered {
		t.Fatalf("expected removed node stay recovered, got %+v", a)
	}
}

func TestRunCycleOrphanRecoverOnConfigDisabled(t *testing.T) {
	svc, st := newTestService(t)
	nm := &store.NodeMonitoring{Enabled: true, CPUThreshold: 90}
	svc.evaluate("prod", nm, map[string]string{"n1": "web1"}, map[string]nodeSample{
		"n1": {hostname: "web1", reachable: true, cpuPercent: 95},
	})
	// 配置禁用（本轮 activeClusters 不含 prod）→ 状态清除并补发恢复事件。
	svc.runCycle(context.Background())
	a, _ := st.GetAlert(store.AlertID("prod", "node/web1", store.EventResourceOver))
	if a == nil || a.Status != store.AlertRecovered {
		t.Fatalf("expected orphan alert recovered after config disabled, got %+v", a)
	}
}

func TestNodeMonitoringValidate(t *testing.T) {
	cases := []struct {
		name string
		m    *store.NodeMonitoring
		ok   bool
	}{
		{"nil", nil, true},
		{"disabled zero", &store.NodeMonitoring{}, true},
		{"enabled cpu", &store.NodeMonitoring{Enabled: true, CPUThreshold: 90}, true},
		{"enabled mem", &store.NodeMonitoring{Enabled: true, MemThreshold: 85}, true},
		{"enabled zero thresholds", &store.NodeMonitoring{Enabled: true}, false},
		{"out of range", &store.NodeMonitoring{Enabled: true, CPUThreshold: 101}, false},
		{"negative", &store.NodeMonitoring{Enabled: true, MemThreshold: -1}, false},
	}
	for _, c := range cases {
		if err := c.m.Validate(); (err == nil) != c.ok {
			t.Errorf("%s: Validate()=%v, want ok=%v", c.name, err, c.ok)
		}
	}
}
