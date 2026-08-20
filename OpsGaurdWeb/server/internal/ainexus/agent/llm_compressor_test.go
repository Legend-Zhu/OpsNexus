package agent

import (
	"context"
	"strings"
	"testing"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/provider"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/usage"
)

// captureProvider 记录压缩调用实际使用的模型、system 消息与 ctx。
type captureProvider struct {
	model  string
	ctx    context.Context
	system string
}

func (p *captureProvider) Name() string                    { return "cp" }
func (p *captureProvider) ToolFormat() provider.ToolFormat { return provider.ToolFormatOpenAI }
func (p *captureProvider) Models() []string                { return []string{"m"} }
func (p *captureProvider) SupportsModel(m string) bool     { return m == "m" }
func (p *captureProvider) ChatCompletionStream(context.Context, *provider.ChatRequest) (<-chan provider.StreamEvent, error) {
	return nil, context.Canceled
}
func (p *captureProvider) ChatCompletion(ctx context.Context, req *provider.ChatRequest) (*provider.ChatResponse, error) {
	p.model = req.Model
	p.ctx = ctx
	for _, m := range req.Messages {
		if m.Role == provider.RoleSystem {
			p.system = m.Content
		}
	}
	return &provider.ChatResponse{
		Choices: []provider.ChatChoice{{Message: provider.ChatMessage{Content: "compressed"}, FinishReason: "stop"}},
	}, nil
}

// TestCompressorUsesConversationModelAndContext 压缩调用使用对话实际模型
// （而非写死的 "summary"），并继承调用方 ctx 的取消语义与计量场景。
func TestCompressorUsesConversationModelAndContext(t *testing.T) {
	cp := &captureProvider{}
	c := NewLLMCompressor(cp, 6000, nil)

	base := usage.NewOperation(context.Background(), usage.ScenarioChat, "/api/v1/ainexus/chat")
	note := c.Compress(base, "glm-x", []provider.ChatMessage{
		{Role: provider.RoleUser, Content: "排查 web"},
	})
	if note != "compressed" {
		t.Fatalf("note = %q, want compressed", note)
	}
	if cp.model != "glm-x" {
		t.Fatalf("compress model = %q, want glm-x", cp.model)
	}
	m, ok := usage.FromContext(cp.ctx)
	if !ok {
		t.Fatal("usage meta missing on compressor call")
	}
	if m.Scenario != usage.ScenarioCompress {
		t.Fatalf("scenario = %q, want %q", m.Scenario, usage.ScenarioCompress)
	}
	orig, _ := usage.FromContext(base)
	if m.OperationID != orig.OperationID {
		t.Fatal("compress call must stay in the same operation")
	}
}

// TestCompressorPropagatesCancellation 压缩调用继承可取消 ctx
// （此前 context.Background() 导致无法随请求取消）。
func TestCompressorPropagatesCancellation(t *testing.T) {
	cp := &captureProvider{}
	c := NewLLMCompressor(cp, 6000, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = c.Compress(ctx, "m", []provider.ChatMessage{{Role: provider.RoleUser, Content: "x"}})
	if cp.ctx == nil || cp.ctx.Err() == nil {
		t.Fatal("compressor must propagate caller ctx (cancellation lost)")
	}
}

// legacySummaryTpl 旧 %d/%s 版本的逐字拷贝（等价性对照用）。
const legacySummaryTpl = `你是会话压缩器。把下面的对话轮次压缩成一段不超过 %d 字的纪要,
只保留对后续排查有用的信息,按类别组织:
- 根因/结论:已确认或高度怀疑的根因
- 关键操作:执行过的命令/工具调用(名称+简要结果)
- 关键观测:数值(CPU/内存/端口等)与状态变化
- 待办/下一步:未完成的分析或建议
丢弃寒暄、重复与无关内容。直接输出纪要,不要解释。

对话轮次:
%s
`

// TestCompressBuiltinTemplateMatchesLegacy 内置 v1 模板（Go template 版）
// 渲染结果与旧 %d/%s 替换逐字节一致——保证未自定义/回滚 v1 时行为零变化。
func TestCompressBuiltinTemplateMatchesLegacy(t *testing.T) {
	content := "[用户] 排查 web\n[助手] 调用工具: probe\n"
	legacy := strings.Replace(legacySummaryTpl, "%d", "80", 1)
	legacy = strings.Replace(legacy, "%s", content, 1)

	got, err := renderCompressPrompt(summaryPromptTemplate, 80, content)
	if err != nil {
		t.Fatalf("render builtin: %v", err)
	}
	if got != legacy {
		t.Fatalf("builtin template drifted from legacy:\n--- got ---\n%q\n--- want ---\n%q", got, legacy)
	}
}

// staticSource 固定返回一个场景模板。
type staticSource struct{ tpl string }

func (s staticSource) ScenarioTemplate(scenario string) (string, bool) {
	if scenario == ScenarioCompressSystem {
		return s.tpl, true
	}
	return "", false
}

// TestCompressorUsesCustomTemplate mlops 注入 compress_system active 模板时，
// 压缩调用使用注入模板的渲染结果。
func TestCompressorUsesCustomTemplate(t *testing.T) {
	cp := &captureProvider{}
	src := staticSource{tpl: "SUM {{.MaxWords}}:\n{{.Content}}"}
	c := NewLLMCompressor(cp, 6000, src)

	note := c.Compress(context.Background(), "m", []provider.ChatMessage{{Role: provider.RoleUser, Content: "hello"}})
	if note != "compressed" {
		t.Fatalf("note = %q", note)
	}
	if want := "SUM 80:\n[用户] hello\n"; cp.system != want {
		t.Fatalf("system = %q, want %q", cp.system, want)
	}
}

// TestCompressorBadTemplateFallsBack 坏注入模板（未知变量）→ 压缩失败返回
// 空串，上层退化为硬删。
func TestCompressorBadTemplateFallsBack(t *testing.T) {
	cp := &captureProvider{}
	src := staticSource{tpl: "坏 {{.Nope}}"}
	c := NewLLMCompressor(cp, 6000, src)

	if note := c.Compress(context.Background(), "m", []provider.ChatMessage{{Role: provider.RoleUser, Content: "x"}}); note != "" {
		t.Fatalf("bad template must yield empty note, got %q", note)
	}
	if cp.model != "" {
		t.Fatal("bad template must not reach the provider")
	}
}
