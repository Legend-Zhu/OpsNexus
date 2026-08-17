package invmonitor

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ingest"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
)

func newTestService(t *testing.T) (*Service, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "invmon-test"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return New(st, nil, ingest.New(st, nil), nil), st
}

func portSpec() checkSpec {
	return checkSpec{id: "port:8848", evType: store.EventPortDown, interval: time.Second}
}

// TestTransitions 失败→告警，持续失败不重复告警，恢复→关闭告警。
func TestTransitions(t *testing.T) {
	svc, st := newTestService(t)
	spec := portSpec()

	// 首次失败 → 产生 active 告警
	svc.recordResult("c1", "r-nacos", spec, false, "TCP 10.0.0.1:8848 不可达", nil)
	alerts, err := st.ListAlerts(store.AlertActive, "c1")
	if err != nil {
		t.Fatalf("list alerts: %v", err)
	}
	if len(alerts) != 1 {
		t.Fatalf("expected 1 active alert, got %d", len(alerts))
	}
	if alerts[0].Service != "r-nacos" || alerts[0].Type != store.EventPortDown {
		t.Fatalf("unexpected alert: %+v", alerts[0])
	}

	// 持续失败 → 不翻转，计数不增长
	svc.recordResult("c1", "r-nacos", spec, false, "TCP 10.0.0.1:8848 不可达", nil)
	alerts, _ = st.ListAlerts("", "c1")
	if len(alerts) != 1 || alerts[0].Count != 1 {
		t.Fatalf("expected single alert count=1, got %+v", alerts)
	}

	// 恢复 → 告警关闭
	svc.recordResult("c1", "r-nacos", spec, true, "", nil)
	active, _ := st.ListAlerts(store.AlertActive, "c1")
	if len(active) != 0 {
		t.Fatalf("expected no active alerts after recovery, got %d", len(active))
	}
	recovered, _ := st.ListAlerts(store.AlertRecovered, "c1")
	if len(recovered) != 1 {
		t.Fatalf("expected 1 recovered alert, got %d", len(recovered))
	}

	// 持续正常 → 无新事件
	svc.recordResult("c1", "r-nacos", spec, true, "", nil)
	recovered, _ = st.ListAlerts(store.AlertRecovered, "c1")
	if len(recovered) != 1 {
		t.Fatalf("expected still 1 recovered alert, got %d", len(recovered))
	}
}

// TestRPCErrorNoTransition RPC 失败不算确定性结果：不翻转、不告警，但节流生效。
func TestRPCErrorNoTransition(t *testing.T) {
	svc, st := newTestService(t)
	spec := portSpec()
	key := "c1/grafana/" + spec.id

	if !svc.due(key, time.Hour) {
		t.Fatal("first run should be due")
	}
	svc.recordResult("c1", "grafana", spec, false, "", errors.New("node unreachable"))
	alerts, _ := st.ListAlerts("", "c1")
	if len(alerts) != 0 {
		t.Fatalf("rpc error must not raise alerts, got %+v", alerts)
	}
	if svc.due(key, time.Hour) {
		t.Fatal("lastRun should be updated (throttled) after rpc error")
	}
}

// TestDueInterval 到期判定：interval 内不重复执行。
func TestDueInterval(t *testing.T) {
	svc, _ := newTestService(t)
	key := "c1/x/port:22"
	if !svc.due(key, time.Minute) {
		t.Fatal("unknown key should be due")
	}
	svc.touch(key)
	if svc.due(key, time.Minute) {
		t.Fatal("just-run key should not be due within interval")
	}
	if !svc.due(key, 0) {
		t.Fatal("zero interval should always be due")
	}
}

// TestParseInterval 非法/空值回退默认。
func TestParseInterval(t *testing.T) {
	if d := parseInterval("", time.Minute); d != time.Minute {
		t.Fatalf("empty → default, got %v", d)
	}
	if d := parseInterval("bogus", time.Minute); d != time.Minute {
		t.Fatalf("invalid → default, got %v", d)
	}
	if d := parseInterval("5s", time.Minute); d != 5*time.Second {
		t.Fatalf("5s expected, got %v", d)
	}
}

// TestParseHostPort 与 api 包 helper 行为对齐。
func TestParseHostPort(t *testing.T) {
	cases := map[string][2]string{
		"10.0.0.1":      {"10.0.0.1", ""},
		"10.0.0.1:8848": {"10.0.0.1", "8848"},
		"10.0.0.1:":     {"10.0.0.1", ""},
	}
	for in, want := range cases {
		h, p := parseHostPort(in)
		if h != want[0] || p != want[1] {
			t.Fatalf("parseHostPort(%q) = (%q,%q), want (%q,%q)", in, h, p, want[0], want[1])
		}
	}
}
