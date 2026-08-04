package ingest

import (
	"path/filepath"
	"testing"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
)

func newTestIngest(t *testing.T) *Service {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "ogw-test"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return New(st)
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

// TestParseEvent 区分事件/审计 payload。
func TestParseEvent(t *testing.T) {
	e, isAudit, err := ParseEvent([]byte(`{"id":"ev-1","service":"web","type":"port_down","level":"error","msg":"x"}`))
	if err != nil || isAudit || e == nil || e.Service != "web" {
		t.Fatalf("event parse: e=%v isAudit=%v err=%v", e, isAudit, err)
	}
	_, isAudit, err = ParseEvent([]byte(`{"id":"a1","actor":"admin","action":"deploy","service":"web","ok":true}`))
	if err != nil || !isAudit {
		t.Fatalf("audit parse: isAudit=%v err=%v", isAudit, err)
	}
	if _, _, err = ParseEvent([]byte(`{bad`)); err == nil {
		t.Fatal("expected parse error")
	}
}

// TestValidateToken token 校验。
func TestValidateToken(t *testing.T) {
	if !ValidateToken("abc", "abc") {
		t.Fatal("equal token should pass")
	}
	if ValidateToken("abc", "def") {
		t.Fatal("mismatch should fail")
	}
	if !ValidateToken("", "") {
		t.Fatal("empty want → allow")
	}
	if ValidateToken("", "secret") {
		t.Fatal("empty got with want set should fail")
	}
}
