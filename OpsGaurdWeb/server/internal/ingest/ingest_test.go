package ingest

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
)

func newTestIngest(t *testing.T) *Service {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "ogw-test"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return New(st, nil)
}

// fakeNotifier 记录每次通知调用（异步分发，测试用 waitNotify 等待）。
type fakeNotifier struct {
	mu       sync.Mutex
	subjects []string
}

func (f *fakeNotifier) NotifyAlert(_ context.Context, _ *store.Alert, subject string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subjects = append(f.subjects, subject)
	return nil
}

func (f *fakeNotifier) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.subjects)
}

// waitNotify 等待通知数达到 want（异步分发，轮询兜底 2s）。
func waitNotify(t *testing.T, f *fakeNotifier, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if f.count() >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("expected %d notifications, got %d", want, f.count())
}

// settleNotify 等待一小段时间后断言通知数不再增长（用于"不应再通知"）。
func settleNotify(t *testing.T, f *fakeNotifier, want int) {
	t.Helper()
	time.Sleep(100 * time.Millisecond)
	if got := f.count(); got != want {
		t.Fatalf("expected %d notifications after settle, got %d", want, got)
	}
}

func newTestIngestWithNotify(t *testing.T) (*Service, *fakeNotifier) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "ogw-test"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	fn := &fakeNotifier{}
	svc := New(st, fn)
	t.Cleanup(svc.Stop)
	return svc, fn
}

// TestHandleEventAggregate 事件 → 告警聚合 + 计数（不同事件 id 同键累加）。
func TestHandleEventAggregate(t *testing.T) {
	svc := newTestIngest(t)
	e := &store.IngestEvent{
		ID: "ev-1", Service: "web", Type: store.EventPortDown, Level: store.LevelError, Msg: "8080 down",
	}
	if err := svc.HandleEvent("dev", e); err != nil {
		t.Fatalf("handle 1: %v", err)
	}
	e2 := &store.IngestEvent{
		ID: "ev-2", Service: "web", Type: store.EventPortDown, Level: store.LevelError, Msg: "8080 down",
	}
	if err := svc.HandleEvent("dev", e2); err != nil {
		t.Fatalf("handle 2: %v", err)
	}

	alerts, err := svc.st.ListAlerts(store.AlertActive, "dev")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(alerts) != 1 {
		t.Fatalf("expected 1 alert, got %d", len(alerts))
	}
	a := alerts[0]
	if a.Count != 2 || a.Service != "web" || a.Cluster != "dev" || a.Status != store.AlertActive {
		t.Fatalf("unexpected alert: %+v", a)
	}
}

// TestDedupe 相同事件 id 幂等（Webhook 重试不重复计数）。
func TestDedupe(t *testing.T) {
	svc := newTestIngest(t)
	e := &store.IngestEvent{ID: "same-id", Service: "web", Type: store.EventPortDown, Level: store.LevelError}
	if err := svc.HandleEvent("dev", e); err != nil {
		t.Fatalf("handle 1: %v", err)
	}
	if err := svc.HandleEvent("dev", e); err != nil {
		t.Fatalf("handle 2 (dup): %v", err)
	}
	alerts, _ := svc.st.ListAlerts(store.AlertActive, "dev")
	if len(alerts) != 1 || alerts[0].Count != 1 {
		t.Fatalf("dedupe failed: %+v", alerts)
	}
	// 不同 id 但同键 → 累加
	e2 := &store.IngestEvent{ID: "other-id", Service: "web", Type: store.EventPortDown, Level: store.LevelError}
	if err := svc.HandleEvent("dev", e2); err != nil {
		t.Fatalf("handle 3: %v", err)
	}
	alerts, _ = svc.st.ListAlerts(store.AlertActive, "dev")
	if len(alerts) != 1 || alerts[0].Count != 2 {
		t.Fatalf("aggregate after dedupe failed: %+v", alerts)
	}
}

// TestRecoverByService 恢复事件 → 同服务 active/acked 告警全部 recovered。
func TestRecoverByService(t *testing.T) {
	svc := newTestIngest(t)
	if err := svc.HandleEvent("dev", &store.IngestEvent{ID: "e1", Service: "web", Type: store.EventPortDown, Level: store.LevelError}); err != nil {
		t.Fatalf("handle e1: %v", err)
	}
	if err := svc.HandleEvent("dev", &store.IngestEvent{ID: "e2", Service: "web", Type: store.EventHTTPUnhealthy, Level: store.LevelWarn}); err != nil {
		t.Fatalf("handle e2: %v", err)
	}
	// ack 其中一条
	alerts, _ := svc.st.ListAlerts(store.AlertActive, "dev")
	if len(alerts) != 2 {
		t.Fatalf("expected 2 alerts, got %d", len(alerts))
	}
	if _, err := svc.st.SetAlertStatus(alerts[0].ID, store.AlertAcked, "alice"); err != nil {
		t.Fatalf("ack: %v", err)
	}

	// 恢复事件
	if err := svc.HandleEvent("dev", &store.IngestEvent{ID: "e3", Service: "web", Type: store.EventResourceRecover, Level: store.LevelInfo}); err != nil {
		t.Fatalf("handle recover: %v", err)
	}
	all, _ := svc.st.ListAlerts("", "dev")
	if len(all) != 2 {
		t.Fatalf("expected 2 alerts total, got %d", len(all))
	}
	for _, a := range all {
		if a.Status != store.AlertRecovered {
			t.Fatalf("alert %s should be recovered, got %s", a.ID, a.Status)
		}
	}
}

// TestNotifyOnCreateOnly 新建通知一次；同键后续事件（聚合计数）不再通知。
func TestNotifyOnCreateOnly(t *testing.T) {
	svc, fn := newTestIngestWithNotify(t)
	for _, id := range []string{"e1", "e2", "e3"} {
		if err := svc.HandleEvent("dev", &store.IngestEvent{ID: id, Service: "web", Type: store.EventPortDown, Level: store.LevelError}); err != nil {
			t.Fatalf("handle %s: %v", id, err)
		}
	}
	settleNotify(t, fn, 1)
	// 不同服务是新告警 → 再通知一次
	if err := svc.HandleEvent("dev", &store.IngestEvent{ID: "e4", Service: "api", Type: store.EventPortDown, Level: store.LevelError}); err != nil {
		t.Fatalf("handle e4: %v", err)
	}
	waitNotify(t, fn, 2)
}

// TestNotifyMilestone 聚合次数到达 10 的里程碑时补一次提醒。
func TestNotifyMilestone(t *testing.T) {
	svc, fn := newTestIngestWithNotify(t)
	for i := 0; i < 9; i++ {
		if err := svc.HandleEvent("dev", &store.IngestEvent{ID: "ev-" + string(rune('a'+i)), Service: "web", Type: store.EventLogMatch, Level: store.LevelWarn}); err != nil {
			t.Fatalf("handle %d: %v", i, err)
		}
	}
	settleNotify(t, fn, 1) // 仅新建那次
	// 第 10 次事件 → count=10 → 里程碑提醒
	if err := svc.HandleEvent("dev", &store.IngestEvent{ID: "ev-j", Service: "web", Type: store.EventLogMatch, Level: store.LevelWarn}); err != nil {
		t.Fatalf("handle 10th: %v", err)
	}
	waitNotify(t, fn, 2)
	// 第 11 次不再提醒
	if err := svc.HandleEvent("dev", &store.IngestEvent{ID: "ev-k", Service: "web", Type: store.EventLogMatch, Level: store.LevelWarn}); err != nil {
		t.Fatalf("handle 11th: %v", err)
	}
	settleNotify(t, fn, 2)
}

// TestNotifyRecoverAndReactivate 恢复通知 + 复发重激活再次通知。
func TestNotifyRecoverAndReactivate(t *testing.T) {
	svc, fn := newTestIngestWithNotify(t)
	if err := svc.HandleEvent("dev", &store.IngestEvent{ID: "e1", Service: "web", Type: store.EventResourceOver, Level: store.LevelWarn}); err != nil {
		t.Fatalf("handle e1: %v", err)
	}
	waitNotify(t, fn, 1) // 新建通知

	// 恢复事件 → 关闭告警 + 恢复通知
	if err := svc.HandleEvent("dev", &store.IngestEvent{ID: "e2", Service: "web", Type: store.EventResourceRecover, Level: store.LevelInfo}); err != nil {
		t.Fatalf("handle recover: %v", err)
	}
	waitNotify(t, fn, 2)

	// 复发 → 重激活（新 id）→ 再次通知
	if err := svc.HandleEvent("dev", &store.IngestEvent{ID: "e3", Service: "web", Type: store.EventResourceOver, Level: store.LevelWarn}); err != nil {
		t.Fatalf("handle e3: %v", err)
	}
	waitNotify(t, fn, 3)
}

// (TestParseEvent / TestValidateToken removed — the webhook HTTP entry they
// tested is gone, replaced by the gRPC SubscribeEvents subscriber.)
