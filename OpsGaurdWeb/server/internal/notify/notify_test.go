package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
)

func newTestNotify(t *testing.T) *Service {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "ogw-test"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return New(st)
}

// TestChannelCRUD 渠道创建/校验/更新/删除。
func TestChannelCRUD(t *testing.T) {
	svc := newTestNotify(t)

	// 直连 webhook 渠道
	ch, err := svc.CreateChannel(store.ChannelWebhook, "ops-webhook", map[string]any{"url": "http://127.0.0.1:9/hook"}, false, "", true)
	if err != nil {
		t.Fatalf("create webhook: %v", err)
	}
	// 直连飞书缺 webhook_url → 拒绝
	if _, err := svc.CreateChannel(store.ChannelFeishu, "fs", map[string]any{}, false, "", true); err == nil {
		t.Fatal("feishu without url should be rejected")
	}
	// 飞书走代理 → 需要 proxy_url
	if _, err := svc.CreateChannel(store.ChannelFeishu, "fs-proxy", map[string]any{}, true, "", true); err == nil {
		t.Fatal("via_proxy without proxy_url should be rejected")
	}
	// sms 必须走代理
	if _, err := svc.CreateChannel(store.ChannelSMS, "sms", map[string]any{}, false, "", true); err == nil {
		t.Fatal("sms without proxy should be rejected")
	}

	// 更新 + 删除
	upd, err := svc.UpdateChannel(ch.ID, "renamed", nil, false, "", false)
	if err != nil || upd.Name != "renamed" || upd.Enabled {
		t.Fatalf("update: %+v err=%v", upd, err)
	}
	if err := svc.DeleteChannel(ch.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := svc.DeleteChannel(ch.ID); err == nil {
		t.Fatal("double delete should fail")
	}
}

// TestNotifyAlertByLevel 按级别策略发送：webhook 渠道收到 payload + 记录落库。
func TestNotifyAlertByLevel(t *testing.T) {
	svc := newTestNotify(t)
	var hits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(200)
	}))
	defer ts.Close()

	ch, err := svc.CreateChannel(store.ChannelWebhook, "hook", map[string]any{"url": ts.URL}, false, "", true)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// error 级策略 → 该渠道
	if err := svc.UpsertPolicy(&store.NotifyPolicy{Level: "error", ChannelIDs: []string{ch.ID}}); err != nil {
		t.Fatalf("policy: %v", err)
	}
	// info 级无策略
	if err := svc.UpsertPolicy(&store.NotifyPolicy{Level: "info", ChannelIDs: []string{}}); err != nil {
		t.Fatalf("policy info: %v", err)
	}

	alert := &store.Alert{ID: "al-1", Cluster: "dev", Service: "web", Level: store.LevelError, Title: "t"}
	if err := svc.NotifyAlert(context.Background(), alert, "[dev/web] port down"); err != nil {
		t.Fatalf("notify: %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("expected 1 send, got %d", hits.Load())
	}
	records, err := svc.Records(10)
	if err != nil || len(records) != 1 {
		t.Fatalf("records: %v len=%d", err, len(records))
	}
	if records[0].Status != "success" || records[0].AlertID != "al-1" {
		t.Fatalf("unexpected record: %+v", records[0])
	}
}

// TestNotifyViaProxy 走互联网代理：payload 带 channel_type/config，不含公网凭据。
func TestNotifyViaProxy(t *testing.T) {
	svc := newTestNotify(t)
	var got map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = jsonDecode(r, &got)
		w.WriteHeader(200)
	}))
	defer ts.Close()

	ch, err := svc.CreateChannel(store.ChannelFeishu, "fs", map[string]any{"sign": "proxy-side-only"}, true, ts.URL, true)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := svc.UpsertPolicy(&store.NotifyPolicy{Level: "warn", ChannelIDs: []string{ch.ID}}); err != nil {
		t.Fatalf("policy: %v", err)
	}
	alert := &store.Alert{ID: "al-2", Cluster: "dev", Service: "api", Level: store.LevelWarn}
	if err := svc.NotifyAlert(context.Background(), alert, "warn msg"); err != nil {
		t.Fatalf("notify: %v", err)
	}
	if got == nil || got["channel_type"] != "feishu" || got["content"] == nil {
		t.Fatalf("unexpected proxy payload: %+v", got)
	}
	// 公网凭据不落内网库
	ch2, _ := svc.st.GetChannel(ch.ID)
	if _, ok := ch2.Config["webhook_url"]; ok {
		t.Fatal("public webhook_url should not be stored for via_proxy channel")
	}
}

// TestSendFailureRecorded 发送失败 → 记录 failed。
func TestSendFailureRecorded(t *testing.T) {
	svc := newTestNotify(t)
	ch, err := svc.CreateChannel(store.ChannelWebhook, "dead", map[string]any{"url": "http://127.0.0.1:1/hook"}, false, "", true)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := svc.UpsertPolicy(&store.NotifyPolicy{Level: "error", ChannelIDs: []string{ch.ID}}); err != nil {
		t.Fatalf("policy: %v", err)
	}
	alert := &store.Alert{ID: "al-3", Cluster: "c", Service: "s", Level: store.LevelError}
	if err := svc.NotifyAlert(context.Background(), alert, "x"); err != nil {
		t.Fatalf("notify: %v", err)
	}
	records, _ := svc.Records(10)
	if len(records) != 1 || records[0].Status != "failed" || records[0].Error == "" {
		t.Fatalf("expected failed record: %+v", records)
	}
}

// TestSendGenericRecordsSuccess 通用发送（非告警路径）成功也必须落发送记录
// ——预算通知复用该路径；回归 sendAndRecordGeneric 在 NextSeq 成功时提前
// return、跳过 SaveNotifyRecord 的缺陷。
func TestSendGenericRecordsSuccess(t *testing.T) {
	svc := newTestNotify(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer ts.Close()

	ch, err := svc.CreateChannel(store.ChannelWebhook, "gen", map[string]any{"url": ts.URL}, false, "", true)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := svc.Send(context.Background(), []string{ch.ID}, "预算提醒", "本月已用 80%"); err != nil {
		t.Fatalf("send: %v", err)
	}
	records, err := svc.Records(10)
	if err != nil || len(records) != 1 {
		t.Fatalf("records: %v len=%d", err, len(records))
	}
	if records[0].Status != "success" || records[0].AlertID != "" || records[0].Title != "预算提醒" {
		t.Fatalf("unexpected record: %+v", records[0])
	}
}

func jsonDecode(r *http.Request, v any) error {
	return json.NewDecoder(r.Body).Decode(v)
}
