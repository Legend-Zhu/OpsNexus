package agent

import (
	"context"
	"errors"
	"io"
	"log"
	"testing"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/config"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/provider"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/tool"
)

// echoTool 固定返回文本的工具。
type echoTool struct{}

func (echoTool) Name() string        { return "echo" }
func (echoTool) Description() string { return "echo back" }
func (echoTool) Parameters() map[string]any {
	return map[string]any{"type": "object"}
}
func (echoTool) Execute(_ context.Context, _ map[string]any) (tool.ToolResult, error) {
	return tool.NewTextResult("done"), nil
}

// scriptedProvider 按脚本依次返回非流式响应，并记录每次调用。
type scriptedProvider struct {
	calls   int
	scripte []provider.ChatResponse
}

func (p *scriptedProvider) Name() string                    { return "sp" }
func (p *scriptedProvider) ToolFormat() provider.ToolFormat { return provider.ToolFormatOpenAI }
func (p *scriptedProvider) Models() []string                { return []string{"m"} }
func (p *scriptedProvider) SupportsModel(m string) bool     { return m == "m" }
func (p *scriptedProvider) ChatCompletion(_ context.Context, _ *provider.ChatRequest) (*provider.ChatResponse, error) {
	if p.calls >= len(p.scripte) {
		return nil, errors.New("script exhausted")
	}
	resp := p.scripte[p.calls]
	p.calls++
	return &resp, nil
}
func (p *scriptedProvider) ChatCompletionStream(_ context.Context, _ *provider.ChatRequest) (<-chan provider.StreamEvent, error) {
	return nil, errors.New("not used")
}

// TestRunAccumulatesUsageAcrossRounds 工具循环多轮：usage 累计进最终响应，
// 而非仅最后一轮。
func TestRunAccumulatesUsageAcrossRounds(t *testing.T) {
	reg := tool.NewRegistry()
	reg.MustRegister(echoTool{})
	p := &scriptedProvider{scripte: []provider.ChatResponse{
		{
			Choices: []provider.ChatChoice{{Message: provider.ChatMessage{
				ToolCalls: []provider.ToolCall{{ID: "c1", Name: "echo", Arguments: "{}"}},
			}, FinishReason: "tool_calls"}},
			Usage: provider.UsageInfo{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3},
		},
		{
			Choices: []provider.ChatChoice{{Message: provider.ChatMessage{Content: "final"}, FinishReason: "stop"}},
			Usage:   provider.UsageInfo{PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30},
		},
	}}
	ag := New(p, reg, config.AgentConfig{MaxToolRounds: 4}, log.New(io.Discard, "", 0))
	conv := NewConversation("m")
	conv.AddUserMessage("go")

	resp, err := ag.Run(context.Background(), conv)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if p.calls != 2 {
		t.Fatalf("provider calls = %d, want 2", p.calls)
	}
	if resp.Usage.PromptTokens != 11 || resp.Usage.CompletionTokens != 22 || resp.Usage.TotalTokens != 33 {
		t.Fatalf("usage = %+v, want 11/22/33", resp.Usage)
	}
}

// scriptedStreamProvider 流式脚本：第一轮返回工具调用，第二轮返回最终文本。
type scriptedStreamProvider struct {
	calls int
}

func (p *scriptedStreamProvider) Name() string                    { return "sp" }
func (p *scriptedStreamProvider) ToolFormat() provider.ToolFormat { return provider.ToolFormatOpenAI }
func (p *scriptedStreamProvider) Models() []string                { return []string{"m"} }
func (p *scriptedStreamProvider) SupportsModel(m string) bool     { return m == "m" }
func (p *scriptedStreamProvider) ChatCompletion(_ context.Context, _ *provider.ChatRequest) (*provider.ChatResponse, error) {
	return nil, errors.New("not used")
}
func (p *scriptedStreamProvider) ChatCompletionStream(_ context.Context, _ *provider.ChatRequest) (<-chan provider.StreamEvent, error) {
	p.calls++
	ch := make(chan provider.StreamEvent, 8)
	if p.calls == 1 {
		ch <- provider.StreamEvent{Type: provider.EventToolCallStart,
			ToolCall: &provider.ToolCall{ID: "c1", Name: "echo", Arguments: "{}"}}
		ch <- provider.StreamEvent{Type: provider.EventDone, FinishReason: "tool_calls",
			Usage: &provider.UsageInfo{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3}}
	} else {
		ch <- provider.StreamEvent{Type: provider.EventContentDelta, Content: "final"}
		ch <- provider.StreamEvent{Type: provider.EventDone, FinishReason: "stop",
			Usage: &provider.UsageInfo{PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30}}
	}
	close(ch)
	return ch, nil
}

// TestRunStreamAccumulatesUsageAcrossRounds 流式 ReAct 多轮：最终 Done 事件
// 携带整次请求的累计 usage。
func TestRunStreamAccumulatesUsageAcrossRounds(t *testing.T) {
	reg := tool.NewRegistry()
	reg.MustRegister(echoTool{})
	p := &scriptedStreamProvider{}
	ag := New(p, reg, config.AgentConfig{MaxToolRounds: 4}, log.New(io.Discard, "", 0))
	conv := NewConversation("m")
	conv.AddUserMessage("go")

	out, err := ag.RunStream(context.Background(), conv)
	if err != nil {
		t.Fatalf("run stream: %v", err)
	}
	var done *AgentEvent
	for ev := range out {
		if ev.Type == AgentEventError {
			t.Fatalf("unexpected error: %v", ev.Error)
		}
		if ev.Type == AgentEventDone {
			d := ev
			done = &d
		}
	}
	if done == nil {
		t.Fatal("done event missing")
	}
	if p.calls != 2 {
		t.Fatalf("provider calls = %d, want 2", p.calls)
	}
	if done.Usage == nil ||
		done.Usage.PromptTokens != 11 || done.Usage.CompletionTokens != 22 || done.Usage.TotalTokens != 33 {
		t.Fatalf("done usage = %+v, want 11/22/33", done.Usage)
	}
}
