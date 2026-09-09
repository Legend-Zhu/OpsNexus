package api

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/mlops"
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

	msgs := BuildInvestigateMessages(mkAlert(), events, audit, logs, true, nil)
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
	msgs := BuildInvestigateMessages(mkAlert(), nil, nil, nil, false, nil)
	user, _ := msgs[1]["content"].(string)
	if strings.Contains(user, "近期监控事件") || strings.Contains(user, "服务日志") {
		t.Fatalf("prompt should omit empty sections:\n%s", user)
	}
	sys, _ := msgs[0]["content"].(string)
	if strings.Contains(sys, "MCP 工具") {
		t.Fatalf("system prompt should not mention MCP when disabled:\n%s", sys)
	}
}

// mkInventoryDecl 构造一个 standalone-container 纳管声明（r-nacos 典型形态）。
func mkInventoryDecl() *mlops.InvestigateInventory {
	return &mlops.InvestigateInventory{
		Name:     "r-nacos",
		Type:     "standalone-container",
		Ref:      "rnacos",
		Node:     "node-1",
		Ports:    []string{"8848", "9848"},
		Category: "middleware",
		Desc:     "注册/配置中心",
	}
}

// TestBuildInvestigateMessagesInventory 纳管对象：注入声明 + 节点侧采证指引，
// system 明确告知对象非 swarm 服务。
func TestBuildInvestigateMessagesInventory(t *testing.T) {
	decl := mkInventoryDecl()
	msgs := BuildInvestigateMessages(mkAlert(), nil, nil, nil, true, decl)
	sys, _ := msgs[0]["content"].(string)
	user, _ := msgs[1]["content"].(string)

	// system：对象说明句（类型 + 非 swarm + 节点侧工具）
	for _, want := range []string{"纳管对象说明", "standalone-container", "list_services 看不到它", "check_port/check_http", "list_host_processes", "list_node_containers"} {
		if !strings.Contains(sys, want) {
			t.Fatalf("system prompt missing %q:\n%s", want, sys)
		}
	}
	// user：声明段（字段齐全、空段省略）
	for _, want := range []string{"【纳管对象声明】", "名称: r-nacos", "类型: standalone-container", "引用: rnacos", "所在节点: node-1", "端口: 8848, 9848", "分类: middleware", "描述: 注册/配置中心"} {
		if !strings.Contains(user, want) {
			t.Fatalf("user prompt missing %q:\n%s", want, user)
		}
	}
	// 声明在事件段之前（告警 → 声明 → 事件 顺序）
	if strings.Index(user, "【纳管对象声明】") > strings.Index(user, "请给出根因分析") {
		t.Fatalf("declaration should precede the closing instruction:\n%s", user)
	}
}

// TestBuildInvestigateMessagesInventoryMinimal 最小声明：可选字段行省略，
// 监控配置多行文本原样注入。
func TestBuildInvestigateMessagesInventoryMinimal(t *testing.T) {
	decl := &mlops.InvestigateInventory{
		Name:       "AI-Chat",
		Type:       "host-service",
		Ref:        "10.60.171.231",
		Monitoring: "{\n  \"portChecks\": [\n    {\n      \"port\": \"3000\"\n    }\n  ]\n}",
	}
	msgs := BuildInvestigateMessages(mkAlert(), nil, nil, nil, false, decl)
	user, _ := msgs[1]["content"].(string)
	for _, want := range []string{"名称: AI-Chat", "类型: host-service", "引用: 10.60.171.231", "监控配置:", "portChecks"} {
		if !strings.Contains(user, want) {
			t.Fatalf("user prompt missing %q:\n%s", want, user)
		}
	}
	for _, absent := range []string{"所在节点:", "端口:", "分类:", "描述:"} {
		if strings.Contains(user, absent) {
			t.Fatalf("user prompt should omit empty field %q:\n%s", absent, user)
		}
	}
	sys, _ := msgs[0]["content"].(string)
	if strings.Contains(sys, "纳管对象说明") != true {
		t.Fatalf("system prompt should carry managed-object note:\n%s", sys)
	}
}
