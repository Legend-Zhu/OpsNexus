package store

import (
	"path/filepath"
	"testing"
	"time"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "ogw-store-test"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestSettingRoundtrip(t *testing.T) {
	s := openTemp(t)

	type reportCfg struct {
		Mode       string   `json:"mode"`
		ChannelIDs []string `json:"channel_ids"`
	}
	var cfg reportCfg
	ok, err := s.GetSetting("patrol-report", &cfg)
	if err != nil || ok {
		t.Fatalf("missing setting should be (false, nil), got (%v, %v)", ok, err)
	}
	if err := s.PutSetting("patrol-report", reportCfg{Mode: "anomaly", ChannelIDs: []string{"ch1", "ch2"}}); err != nil {
		t.Fatalf("put: %v", err)
	}
	ok, err = s.GetSetting("patrol-report", &cfg)
	if err != nil || !ok {
		t.Fatalf("get: (%v, %v)", ok, err)
	}
	if cfg.Mode != "anomaly" || len(cfg.ChannelIDs) != 2 {
		t.Fatalf("unexpected cfg: %+v", cfg)
	}
}

func TestInvestigationSaveGetList(t *testing.T) {
	s := openTemp(t)

	inv := &Investigation{AlertID: "al-1", Cluster: "dev", Title: "排查：端口不通", Messages: `[]`}
	if err := s.SaveInvestigation(inv); err != nil {
		t.Fatalf("save: %v", err)
	}
	if inv.ID != "inv-1" || inv.CreatedAt.IsZero() {
		t.Fatalf("unexpected inv: %+v", inv)
	}
	// 第二条（另一个告警 + 一条自由提问）
	_ = s.SaveInvestigation(&Investigation{AlertID: "al-2", Title: "t2", Messages: `[]`})
	_ = s.SaveInvestigation(&Investigation{Title: "自由提问", Messages: `[]`})

	got, err := s.GetInvestigation("inv-1")
	if err != nil || got == nil || got.Title == "" {
		t.Fatalf("get: %+v %v", got, err)
	}
	list, err := s.ListInvestigations("al-1", 0)
	if err != nil || len(list) != 1 {
		t.Fatalf("list by alert: %v len=%d", err, len(list))
	}
	all, _ := s.ListInvestigations("", 0)
	if len(all) != 3 || all[0].ID != "inv-3" { // 最新在前
		t.Fatalf("list all: len=%d first=%v", len(all), all[0].ID)
	}

	// 更新（追问后覆盖同一条）
	inv.Conclusion = "根因：端口未监听"
	if err := s.SaveInvestigation(inv); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _ = s.GetInvestigation("inv-1")
	if got.Conclusion == "" || got.UpdatedAt.Before(got.CreatedAt) {
		t.Fatalf("update not applied: %+v", got)
	}
	missing, err := s.GetInvestigation("inv-999")
	if err != nil || missing != nil {
		t.Fatalf("missing id should be (nil, nil), got (%v, %v)", missing, err)
	}
	if _, err := s.GetInvestigation("bogus"); err == nil {
		t.Fatal("malformed id should error")
	}
}

func TestUpsertKeyedAlertLifecycle(t *testing.T) {
	s := openTemp(t)
	id := AlertIDWithKey("dev", "", EventPatrolFailed, "port/10.0.0.1:3306")
	if id == AlertID("dev", "", EventPatrolFailed) {
		t.Fatal("AlertIDWithKey should differ from AlertID when key is set")
	}
	mk := func() *Alert {
		return &Alert{
			ID: id, Cluster: "dev", Type: EventPatrolFailed, Level: LevelWarn,
			Title: "巡检异常：port/10.0.0.1:3306", Status: AlertActive, Count: 1,
			FirstTS: time.Now().UTC(), LastTS: time.Now().UTC(),
		}
	}

	created, err := s.UpsertKeyedAlert(mk())
	if err != nil || !created {
		t.Fatalf("first upsert: created=%v err=%v", created, err)
	}
	// 持续失败：count 累加，非 created
	created, _ = s.UpsertKeyedAlert(mk())
	if created {
		t.Fatal("second upsert should not be created")
	}
	a, _ := s.GetAlert(id)
	if a.Count != 2 {
		t.Fatalf("count=%d, want 2", a.Count)
	}
	// 恢复后再失败：原地重激活，ID 不变
	if _, err := s.SetAlertStatus(id, AlertRecovered, "patrol"); err != nil {
		t.Fatalf("recover: %v", err)
	}
	created, _ = s.UpsertKeyedAlert(mk())
	if !created {
		t.Fatal("relapse should be created=true (for notify)")
	}
	a, _ = s.GetAlert(id)
	if a.ID != id || a.Status != AlertActive || a.Count != 1 || a.RecoverAt != nil {
		t.Fatalf("reactivate: %+v", a)
	}
}

func TestMarkAlertInvestigated(t *testing.T) {
	s := openTemp(t)
	id := AlertID("dev", "web", EventPortDown)
	_ = s.putAlertLocked(&Alert{
		ID: id, Cluster: "dev", Service: "web", Type: EventPortDown,
		Level: LevelError, Title: "t", Status: AlertActive, Count: 1,
		FirstTS: time.Now().UTC(), LastTS: time.Now().UTC(),
	})
	if err := s.MarkAlertInvestigated(id, "inv-1"); err != nil {
		t.Fatalf("mark: %v", err)
	}
	_ = s.MarkAlertInvestigated(id, "inv-2")
	a, _ := s.GetAlert(id)
	if a.Investigations != 2 || a.LastInvestigationID != "inv-2" {
		t.Fatalf("unexpected alert: %+v", a)
	}
	// 不存在的告警静默成功
	if err := s.MarkAlertInvestigated("al-none", "inv-1"); err != nil {
		t.Fatalf("missing alert should be silent: %v", err)
	}
}
