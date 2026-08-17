package store

import (
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "ogw-test"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mkEvent(cluster, service string, typ EventType, level Level, id string) *IngestEvent {
	return &IngestEvent{
		ID: id, Cluster: cluster, TS: time.Now().UTC(),
		Service: service, Type: typ, Level: level, Msg: "test msg",
	}
}

// TestAlertAggregation 同一 (cluster,service,type) 多次事件聚合为一条告警并计数。
func TestAlertAggregation(t *testing.T) {
	s := newTestStore(t)
	id := AlertID("dev", "web", EventPortDown)

	// 两条同键事件
	if _, err := s.SaveEvent(mkEvent("dev", "web", EventPortDown, LevelError, "e1")); err != nil {
		t.Fatalf("save e1: %v", err)
	}
	a := &Alert{
		ID: id, Cluster: "dev", Service: "web", Type: EventPortDown,
		Level: LevelError, Title: "[dev] port down", Status: AlertActive,
		Count: 1, FirstTS: time.Now().UTC(), LastTS: time.Now().UTC(), LastEventID: "e1",
	}
	if _, created, err := s.UpsertAlert(a); err != nil || !created {
		t.Fatalf("upsert 1: created=%v err=%v", created, err)
	}
	a2 := *a
	a2.LastTS = time.Now().UTC()
	a2.LastEventID = "e2"
	merged, created, err := s.UpsertAlert(&a2)
	if err != nil || created {
		t.Fatalf("upsert 2: created=%v err=%v", created, err)
	}
	if merged.Count != 2 {
		t.Fatalf("upsert 2: merged count=%d, want 2", merged.Count)
	}

	got, err := s.GetAlert(id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got == nil {
		t.Fatal("expected alert")
	}
	// LastEventID 仅内存去重辅助（json:"-" 不落盘），读回为空是预期
	if got.Count != 2 || got.Status != AlertActive {
		t.Fatalf("unexpected aggregation: %+v", got)
	}

	// 列表按 active 过滤
	items, err := s.ListAlerts(AlertActive, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 1 || items[0].Count != 2 {
		t.Fatalf("unexpected list: %+v", items)
	}
}

// TestAlertAckRecover 认领 → 恢复 → 索引随状态迁移。
func TestAlertAckRecover(t *testing.T) {
	s := newTestStore(t)
	id := AlertID("dev", "web", EventHTTPUnhealthy)
	now := time.Now().UTC()
	a := &Alert{
		ID: id, Cluster: "dev", Service: "web",
		Type: EventHTTPUnhealthy, Level: LevelWarn, Title: "t",
		Status: AlertActive, Count: 1, FirstTS: now, LastTS: now, LastEventID: "e1",
	}
	if _, _, err := s.UpsertAlert(a); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// ack
	acked, err := s.SetAlertStatus(a.ID, AlertAcked, "alice")
	if err != nil {
		t.Fatalf("ack: %v", err)
	}
	if acked == nil || acked.Status != AlertAcked || acked.AckedBy != "alice" || acked.AckedAt == nil {
		t.Fatalf("unexpected ack: %+v", acked)
	}
	// active 列表应空，acked 列表 1 条
	if items, _ := s.ListAlerts(AlertActive, ""); len(items) != 0 {
		t.Fatalf("active should be empty: %+v", items)
	}
	if items, _ := s.ListAlerts(AlertAcked, ""); len(items) != 1 {
		t.Fatalf("acked should have 1: %+v", items)
	}

	// recover
	rec, err := s.SetAlertStatus(a.ID, AlertRecovered, "admin")
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if rec == nil || rec.Status != AlertRecovered || rec.RecoverAt == nil {
		t.Fatalf("unexpected recover: %+v", rec)
	}
	// 全状态列表 1 条；acked 列表空
	if items, _ := s.ListAlerts("", ""); len(items) != 1 {
		t.Fatalf("all should have 1: %+v", items)
	}
	if items, _ := s.ListAlerts(AlertAcked, ""); len(items) != 0 {
		t.Fatalf("acked should be empty after recover: %+v", items)
	}
}

// TestAlertClusterFilter 按集群过滤。
func TestAlertClusterFilter(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()
	for _, c := range []string{"dev", "prod"} {
		a := &Alert{
			ID: AlertID(c, "web", EventPortDown), Cluster: c, Service: "web", Type: EventPortDown,
			Level: LevelError, Title: "t", Status: AlertActive, Count: 1,
			FirstTS: now, LastTS: now, LastEventID: "e",
		}
		if _, _, err := s.UpsertAlert(a); err != nil {
			t.Fatalf("upsert %s: %v", c, err)
		}
	}
	if items, _ := s.ListAlerts(AlertActive, "dev"); len(items) != 1 || items[0].Cluster != "dev" {
		t.Fatalf("dev filter: %+v", items)
	}
	if items, _ := s.ListAlerts("", "prod"); len(items) != 1 || items[0].Cluster != "prod" {
		t.Fatalf("prod filter: %+v", items)
	}
}

// TestUpsertAlertCreatedFlag created 三态：新建=true、合并=false、recovered 复发=true（换新 id）。
func TestUpsertAlertCreatedFlag(t *testing.T) {
	s := newTestStore(t)
	mk := func() *Alert {
		now := time.Now().UTC()
		return &Alert{
			ID: AlertID("dev", "web", EventLogMatch), Cluster: "dev", Service: "web",
			Type: EventLogMatch, Level: LevelWarn, Title: "t", Status: AlertActive,
			Count: 1, FirstTS: now, LastTS: now,
		}
	}
	stored, created, err := s.UpsertAlert(mk())
	if err != nil || !created || stored == nil {
		t.Fatalf("create: stored=%+v created=%v err=%v", stored, created, err)
	}
	origID := stored.ID

	stored, created, err = s.UpsertAlert(mk())
	if err != nil || created {
		t.Fatalf("merge: created=%v err=%v", created, err)
	}
	if stored.ID != origID || stored.Count != 2 {
		t.Fatalf("merge: stored=%+v, want id=%s count=2", stored, origID)
	}

	if _, err := s.SetAlertStatus(origID, AlertRecovered, "admin"); err != nil {
		t.Fatalf("recover: %v", err)
	}
	stored, created, err = s.UpsertAlert(mk())
	if err != nil || !created {
		t.Fatalf("reactivate: created=%v err=%v", created, err)
	}
	if stored.ID == origID || stored.Count != 1 || stored.Status != AlertActive {
		t.Fatalf("reactivate: stored=%+v, want new id count=1 active", stored)
	}
}

// TestMarkAlertNotified 通知回写：次数累加 + 最近通知时间；告警不存在静默成功。
func TestMarkAlertNotified(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()
	a := &Alert{
		ID: AlertID("dev", "web", EventPortDown), Cluster: "dev", Service: "web",
		Type: EventPortDown, Level: LevelError, Title: "t", Status: AlertActive,
		Count: 1, FirstTS: now, LastTS: now,
	}
	if _, _, err := s.UpsertAlert(a); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := s.MarkAlertNotified(a.ID); err != nil {
			t.Fatalf("mark %d: %v", i, err)
		}
	}
	got, err := s.GetAlert(a.ID)
	if err != nil || got == nil {
		t.Fatalf("get: %v", err)
	}
	if got.NotifyCount != 2 || got.LastNotifyAt == nil {
		t.Fatalf("unexpected notify mark: %+v", got)
	}
	if err := s.MarkAlertNotified("al-nonexistent"); err != nil {
		t.Fatalf("missing alert should be silent: %v", err)
	}
}

// TestSaveEventSeq 事件序列单调递增。
func TestSaveEventSeq(t *testing.T) {
	s := newTestStore(t)
	seq1, err := s.SaveEvent(mkEvent("dev", "web", EventPortDown, LevelError, "e1"))
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	seq2, _ := s.SaveEvent(mkEvent("dev", "web", EventPortDown, LevelError, "e2"))
	if seq1 != 1 || seq2 != 2 {
		t.Fatalf("expected seq 1,2 got %d,%d", seq1, seq2)
	}
}
