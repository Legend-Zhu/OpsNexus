package mlops

import (
	"encoding/json"
	"strings"
	"testing"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
)

func newTestService(t *testing.T) *Service {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return New(st)
}

func versionInput(tpl string) VersionInput {
	return VersionInput{Messages: []MessageInput{{Role: "system", Template: tpl}}}
}

// TestListIncludesBuiltinDefaults 未自定义时 List 返回内置场景的默认视图。
func TestListIncludesBuiltinDefaults(t *testing.T) {
	svc := newTestService(t)
	items, err := svc.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != len(scenarioDefs) {
		t.Fatalf("expected %d builtin prompts, got %d", len(scenarioDefs), len(items))
	}
	for _, p := range items {
		if !p.Builtin || p.Materialized || p.ActiveVersion != 1 || len(p.Versions) != 1 {
			t.Fatalf("unexpected builtin view: %+v", p)
		}
		if p.Versions[0].Messages[0].Template == "" {
			t.Fatalf("builtin v1 template empty: %+v", p)
		}
	}
}

// TestNotCustomizedScenarioTemplate 未自定义 → ok=false（引擎走内置默认）。
func TestNotCustomizedScenarioTemplate(t *testing.T) {
	svc := newTestService(t)
	if _, ok := svc.ScenarioTemplate(ScenarioCompressSystem); ok {
		t.Fatal("ScenarioTemplate should be false before customization")
	}
	if _, ok := svc.InvestigateMessages(InvestigateData{}); ok {
		t.Fatal("InvestigateMessages should be false before customization")
	}
}

// TestSaveVersionMaterializesAndActivates 内置场景首次保存：物化 v1、追加
// v2、激活后 ScenarioTemplate 返回自定义模板。
func TestSaveVersionMaterializesAndActivates(t *testing.T) {
	svc := newTestService(t)

	p, err := svc.SaveVersion(builtinPromptID(ScenarioCompressSystem), SaveInput{
		Activate: true,
		Version:  versionInput("压缩到 {{.MaxWords}} 字：{{.Content}}"),
	}, "alice")
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if len(p.Versions) != 2 || p.ActiveVersion != 2 || !p.Materialized || !p.Builtin {
		t.Fatalf("unexpected prompt after save: v%d active=%d materialized=%v",
			len(p.Versions), p.ActiveVersion, p.Materialized)
	}
	if p.Versions[0].Messages[0].Template != builtinCompressSystemTpl {
		t.Fatal("v1 must be the builtin default copy")
	}

	tpl, ok := svc.ScenarioTemplate(ScenarioCompressSystem)
	if !ok || tpl != "压缩到 {{.MaxWords}} 字：{{.Content}}" {
		t.Fatalf("ScenarioTemplate = %q ok=%v", tpl, ok)
	}

	// 审计留痕
	audits, err := svc.Audits(10)
	if err != nil || len(audits) != 1 {
		t.Fatalf("audits: %v len=%d", err, len(audits))
	}
	if audits[0].Action != "prompt_save" || audits[0].Operator != "alice" ||
		audits[0].BeforeHash == "" || audits[0].AfterHash == "" {
		t.Fatalf("unexpected audit: %+v", audits[0])
	}
}

// TestSaveVersionConflict expected_active_version 不匹配 → 409 语义冲突。
func TestSaveVersionConflict(t *testing.T) {
	svc := newTestService(t)
	id := builtinPromptID(ScenarioPatrolSystem)
	if _, err := svc.SaveVersion(id, SaveInput{Activate: true, Version: versionInput("v2")}, "a"); err != nil {
		t.Fatalf("first save: %v", err)
	}
	// 当前 active=2，携带过期的 expected=1 → 冲突
	_, err := svc.SaveVersion(id, SaveInput{ExpectedActiveVersion: 1, Version: versionInput("v3")}, "a")
	if _, ok := err.(ErrConflict); !ok {
		t.Fatalf("expected conflict, got %v", err)
	}
}

// TestActivateAndReset 激活回滚 v1 后恢复默认；Reset 刷新 v1 并激活。
func TestActivateAndReset(t *testing.T) {
	svc := newTestService(t)
	id := builtinPromptID(ScenarioPatrolSystem)

	if _, err := svc.SaveVersion(id, SaveInput{Version: versionInput("自定义 v2")}, "a"); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, ok := svc.ScenarioTemplate(ScenarioPatrolSystem); ok {
		t.Fatal("saved without activate should not change active")
	}
	p, err := svc.Activate(id, 2, 0, "a")
	if err != nil || p.ActiveVersion != 2 {
		t.Fatalf("activate: %v", err)
	}
	if tpl, ok := svc.ScenarioTemplate(ScenarioPatrolSystem); !ok || tpl != "自定义 v2" {
		t.Fatalf("active template = %q ok=%v", tpl, ok)
	}

	// 回滚 v1 = 内置默认
	if _, err := svc.Activate(id, 1, 2, "a"); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if _, ok := svc.ScenarioTemplate(ScenarioPatrolSystem); ok {
		t.Fatal("v1 (builtin default) must report not-customized")
	}

	// Reset：v1 刷新为当前内置默认并激活（历史版本保留）
	if _, err := svc.SaveVersion(id, SaveInput{Activate: true, Version: versionInput("v3")}, "a"); err != nil {
		t.Fatalf("save v3: %v", err)
	}
	p, err = svc.Reset(id, "a")
	if err != nil || p.ActiveVersion != 1 || len(p.Versions) != 3 {
		t.Fatalf("reset: %v p=%+v", err, p)
	}
	if p.Versions[0].Messages[0].Template != builtinPatrolSystemTpl {
		t.Fatal("reset must refresh v1 from builtin default")
	}
}

// TestDeleteRules 内置不可删（用 reset），custom 可删。
func TestDeleteRules(t *testing.T) {
	svc := newTestService(t)
	if err := svc.Delete(builtinPromptID(ScenarioCompressSystem), "a"); err == nil {
		t.Fatal("builtin prompt must not be deletable")
	}

	p, err := svc.Create("实验模板", VersionInput{
		Messages: []MessageInput{{Role: "user", Template: "hello {{.Name}}"}},
		Variables: []VariableInput{{Name: "Name", Required: true}},
	}, "a")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if p.Scenario != ScenarioCustom || p.ActiveVersion != 1 {
		t.Fatalf("unexpected custom prompt: %+v", p)
	}
	if err := svc.Delete(p.ID, "a"); err != nil {
		t.Fatalf("delete custom: %v", err)
	}
	if _, err := svc.Get(p.ID); err == nil {
		t.Fatal("deleted prompt should be gone")
	}
}

// TestValidateVersion 拒绝：空消息、非法角色、语法错误、未知变量。
func TestValidateVersion(t *testing.T) {
	svc := newTestService(t)
	id := builtinPromptID(ScenarioInvestigateUser)

	cases := []struct {
		name string
		in   VersionInput
	}{
		{"empty", VersionInput{}},
		{"bad role", VersionInput{Messages: []MessageInput{{Role: "assistant", Template: "x"}}}},
		{"syntax", VersionInput{Messages: []MessageInput{{Role: "user", Template: "{{.Alert"}}}},
		{"unknown var", VersionInput{Messages: []MessageInput{{Role: "user", Template: "{{.NotAField}}"}}}},
	}
	for _, tc := range cases {
		if _, err := svc.SaveVersion(id, SaveInput{Version: tc.in}, "a"); err == nil {
			t.Fatalf("%s: expected rejection", tc.name)
		}
	}
	// custom 场景无法预知数据结构：未知变量只在预览时报错，保存放行
	p, err := svc.Create("c1", VersionInput{Messages: []MessageInput{{Role: "user", Template: "{{.AnyThing}}"}}}, "a")
	if err != nil {
		t.Fatalf("custom with unknown var should save: %v", err)
	}
	if _, err := svc.Preview(p.ID, 0, json.RawMessage(`{}`)); err == nil {
		t.Fatal("custom preview with missing key must fail (missingkey=error)")
	}
}

// TestPreviewScenarioSchema 内置场景按数据结构解码渲染。
func TestPreviewScenarioSchema(t *testing.T) {
	svc := newTestService(t)
	msgs, err := svc.Preview(builtinPromptID(ScenarioInvestigateUser), 0, json.RawMessage(
		`{"alert":{"cluster":"dev","service":"web","type":"port_down","level":"error","title":"8080 down","count":3,"first_ts":"t1","last_ts":"t2"},"events":"[{\"id\":\"e1\"}]"}`))
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Role != "user" {
		t.Fatalf("unexpected messages: %+v", msgs)
	}
	for _, want := range []string{"集群: dev", "服务: web", "标题: 8080 down", "次数: 3", "近期监控事件", "[{\"id\":\"e1\"}]"} {
		if !strings.Contains(msgs[0].Content, want) {
			t.Fatalf("preview missing %q:\n%s", want, msgs[0].Content)
		}
	}
	// 数据结构与场景不符 → 拒绝
	if _, err := svc.Preview(builtinPromptID(ScenarioInvestigateUser), 0, json.RawMessage(`{"alert":"not-an-object"}`)); err == nil {
		t.Fatal("schema mismatch must be rejected")
	}
}

// TestInvestigateMessagesCustomizedOnlyOneSide 仅自定义 user 场景：system 走
// 内置 v1 模板渲染，两条消息均产出。
func TestInvestigateMessagesCustomizedOnlyOneSide(t *testing.T) {
	svc := newTestService(t)
	if _, err := svc.SaveVersion(builtinPromptID(ScenarioInvestigateUser), SaveInput{
		Activate: true,
		Version: VersionInput{Messages: []MessageInput{{Role: "user",
			Template: "告警 {{.Alert.Title}}；MCP={{.UseMCP}}{{if .Events}}；事件={{.Events}}{{end}}"}}},
	}, "a"); err != nil {
		t.Fatalf("save: %v", err)
	}
	msgs, ok := svc.InvestigateMessages(InvestigateData{
		Alert: InvestigateAlert{Title: "T"},
		Events: "E",
	})
	if !ok {
		t.Fatal("expected customized rendering")
	}
	if len(msgs) != 2 || msgs[0]["role"] != "system" || msgs[1]["role"] != "user" {
		t.Fatalf("unexpected messages: %+v", msgs)
	}
	if got := msgs[1]["content"]; got != "告警 T；MCP=false；事件=E" {
		t.Fatalf("user render = %q", got)
	}
	sys, _ := msgs[0]["content"].(string)
	if !strings.Contains(sys, "资深运维工程师") {
		t.Fatalf("system should render via builtin v1: %q", sys)
	}
}

// TestBadActiveTemplateFallsBack 存量 active 模板渲染失败 → ok=false 回退
//（线上调用方回退代码内置组装），fallback 计数可观测。
func TestBadActiveTemplateFallsBack(t *testing.T) {
	svc := newTestService(t)
	// 直接落库坏 active 模板（绕过保存校验），模拟历史脏数据
	p := synthBuiltin(findScenarioDef(ScenarioInvestigateUser))
	p.Materialized = true
	p.Versions = append(p.Versions, store.PromptVersion{
		Version:  2,
		Messages: []store.PromptMessage{{Role: "user", Template: "坏的 {{.Nope}}"}},
	})
	p.ActiveVersion = 2
	if err := svc.st.PutPrompt(p); err != nil {
		t.Fatalf("seed: %v", err)
	}
	before := svc.Fallbacks()
	if _, ok := svc.InvestigateMessages(InvestigateData{Alert: InvestigateAlert{Title: "T"}}); ok {
		t.Fatal("bad active template must fall back (ok=false)")
	}
	if svc.Fallbacks() <= before {
		t.Fatal("fallback counter must increase")
	}
}
