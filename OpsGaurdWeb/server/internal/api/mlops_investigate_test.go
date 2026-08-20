package api

import (
	"encoding/json"
	"testing"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/mlops"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/workerproxy"
)

func newTestMlopsStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// TestInvestigateTemplateV1MatchesLegacy P1 核心验收：investigate 两个场景
// 的内置 v1 模板渲染输出与代码内置组装（BuildInvestigateMessages）在
// 证据有/无、MCP 开/关的组合下逐字节一致——混合路径（一侧自定义、另一侧
// 走内置模板）与回滚 v1 后行为零变化的根基。
func TestInvestigateTemplateV1MatchesLegacy(t *testing.T) {
	events := json.RawMessage(`[{"id":"e1","service":"web","type":"port_down","level":"error","msg":"8080 down"}]`)
	audit := json.RawMessage(`[{"id":"a1","actor":"admin","action":"deploy","service":"web","ok":true}]`)
	logs := []workerproxy.LogLine{
		{TS: "t1", Stream: "stdout", Line: "started"},
		{TS: "t2", Stream: "stderr", Line: "bind: address already in use"},
	}
	nullEvents := json.RawMessage(`null`)

	cases := []struct {
		name          string
		events, audit json.RawMessage
		logs          []workerproxy.LogLine
		useMCP        bool
	}{
		{"full+mcp", events, audit, logs, true},
		{"full", events, audit, logs, false},
		{"no-evidence", nil, nullEvents, nil, false},
		{"no-evidence+mcp", nil, nil, nil, true},
		{"logs-only", nil, nil, logs, false},
	}
	for _, tc := range cases {
		alert := mkAlert()
		legacy := BuildInvestigateMessages(alert, tc.events, tc.audit, tc.logs, tc.useMCP)
		data := investigatePromptData(alert, tc.events, tc.audit, tc.logs, tc.useMCP)

		for i, scenario := range []string{mlops.ScenarioInvestigateSystem, mlops.ScenarioInvestigateUser} {
			got, err := mlops.RenderBuiltinMessages(scenario, &data)
			if err != nil {
				t.Fatalf("[%s] render %s: %v", tc.name, scenario, err)
			}
			want, _ := legacy[i]["content"].(string)
			if got != want {
				t.Fatalf("[%s] %s drifted:\n--- template ---\n%q\n--- builtin ---\n%q",
					tc.name, scenario, got, want)
			}
		}
	}
}

// TestInvestigateTemplateHybridCustomUser 仅自定义 user 场景：system 侧经
// 内置 v1 模板渲染，与代码内置组装完全一致（真实混合路径）。
func TestInvestigateTemplateHybridCustomUser(t *testing.T) {
	svc := mlops.New(newTestMlopsStore(t))
	if _, err := svc.SaveVersion("sc-"+mlops.ScenarioInvestigateUser, mlops.SaveInput{
		Activate: true,
		Version: mlops.VersionInput{Messages: []mlops.MessageInput{{Role: "user",
			Template: "自定义告警 {{.Alert.Title}}"}}},
	}, "test"); err != nil {
		t.Fatalf("save: %v", err)
	}

	alert := mkAlert()
	events := json.RawMessage(`[{"id":"e1"}]`)
	logs := []workerproxy.LogLine{{TS: "t1", Stream: "stdout", Line: "x"}}
	legacy := BuildInvestigateMessages(alert, events, nil, logs, true)

	msgs, ok := svc.InvestigateMessages(investigatePromptData(alert, events, nil, logs, true))
	if !ok {
		t.Fatal("expected template rendering after customization")
	}
	if msgs[0]["role"] != "system" || msgs[1]["role"] != "user" {
		t.Fatalf("unexpected roles: %+v", msgs)
	}
	// system 侧 = 内置 v1 模板渲染，须与代码内置完全一致
	if got, _ := msgs[0]["content"].(string); got != legacy[0]["content"] {
		t.Fatalf("hybrid system side drifted:\n%q\n---\n%q", got, legacy[0]["content"])
	}
	// user 侧 = 自定义模板
	if got, _ := msgs[1]["content"].(string); got != "自定义告警 [dev] 端口不可达：8080 down" {
		t.Fatalf("custom user side = %q", got)
	}
}

// TestInvestigateMessagesFallbackWithoutMlops 未启用 mlops（nil 服务）时
// helper 直接走代码内置组装。
func TestInvestigateMessagesFallbackWithoutMlops(t *testing.T) {
	h := &Handlers{}
	events := json.RawMessage(`[{"id":"e1"}]`)
	logs := []workerproxy.LogLine{{TS: "t", Stream: "stdout", Line: "l"}}
	alert := mkAlert()
	got := h.investigateMessages(alert, events, nil, logs, true)
	want := BuildInvestigateMessages(alert, events, nil, logs, true)
	if len(got) != len(want) {
		t.Fatalf("message count = %d, want %d", len(got), len(want))
	}
	if got[0]["content"] != want[0]["content"] || got[1]["content"] != want[1]["content"] {
		t.Fatal("fallback path must equal builtin assembly")
	}
}
