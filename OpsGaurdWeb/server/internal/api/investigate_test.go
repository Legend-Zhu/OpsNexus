package api

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/workerproxy"
)

func mkAlert() *store.Alert {
	now := time.Now().UTC()
	return &store.Alert{
		ID: "al-x", Cluster: "dev", Service: "web", Type: store.EventPortDown,
		Level: store.LevelError, Title: "[dev] 端口不可达：8080 down",
		Count: 3, FirstTS: now, LastTS: now, Status: store.AlertActive,
	}
}

// TestBuildInvestigateMessages 完整上下文：告警详情 + 事件 + 审计 + 日志 + MCP 提示。
func TestBuildInvestigateMessages(t *testing.T) {
	events := json.RawMessage(`[{"id":"e1","service":"web","type":"port_down","level":"error","msg":"8080 down"}]`)
	audit := json.RawMessage(`[{"id":"a1","actor":"admin","action":"deploy","service":"web","ok":true}]`)
	logs := []workerproxy.LogLine{
		{TS: "t1", Stream: "stdout", Line: "started"},
		{TS: "t2", Stream: "stderr", Line: "listen tcp :8080: bind: address already in use"},
	}

	msgs := BuildInvestigateMessages(mkAlert(), events, audit, logs, true)
	if len(msgs) != 2 {
		t.Fatalf("expected system+user, got %d messages", len(msgs))
	}
	if msgs[0]["role"] != "system" || msgs[1]["role"] != "user" {
		t.Fatalf("unexpected roles: %+v", msgs)
	}
	sys, _ := msgs[0]["content"].(string)
	user, _ := msgs[1]["content"].(string)

	// 告警详情注入
	for _, want := range []string{"dev", "web", "port_down", "error", "8080 down", "次数: 3"} {
		if !strings.Contains(user, want) {
			t.Fatalf("user prompt missing %q:\n%s", want, user)
		}
	}
	// 证据链注入
	if !strings.Contains(user, "近期监控事件") || !strings.Contains(user, "address already in use") {
		t.Fatalf("user prompt missing evidence:\n%s", user)
	}
	if !strings.Contains(user, "近期操作审计") || !strings.Contains(user, "actor") {
		t.Fatalf("user prompt missing audit:\n%s", user)
	}
	// 日志行格式 [ts/stream] line
	if !strings.Contains(user, "[t2/stderr] listen") {
		t.Fatalf("log lines not formatted:\n%s", user)
	}
	// MCP 提示
	if !strings.Contains(sys, "MCP 工具") {
		t.Fatalf("system prompt should mention MCP tools when useMCP:\n%s", sys)
	}
}

// TestBuildInvestigateMessagesNoContext 无事件/审计/日志时 prompt 不含空段。
func TestBuildInvestigateMessagesNoContext(t *testing.T) {
	msgs := BuildInvestigateMessages(mkAlert(), nil, nil, nil, false)
	user, _ := msgs[1]["content"].(string)
	if strings.Contains(user, "近期监控事件") || strings.Contains(user, "服务日志") {
		t.Fatalf("prompt should omit empty sections:\n%s", user)
	}
	sys, _ := msgs[0]["content"].(string)
	if strings.Contains(sys, "MCP 工具") {
		t.Fatalf("system prompt should not mention MCP when disabled:\n%s", sys)
	}
}
