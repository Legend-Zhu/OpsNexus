package agent

import (
	"context"
	"testing"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/provider"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/usage"
)

// captureProvider 记录压缩调用实际使用的模型与 ctx。
type captureProvider struct {
	model string
	ctx   context.Context
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
	return &provider.ChatResponse{
		Choices: []provider.ChatChoice{{Message: provider.ChatMessage{Content: "compressed"}, FinishReason: "stop"}},
	}, nil
}

// TestCompressorUsesConversationModelAndContext 压缩调用使用对话实际模型
// （而非写死的 "summary"），并继承调用方 ctx 的取消语义与计量场景。
func TestCompressorUsesConversationModelAndContext(t *testing.T) {
	cp := &captureProvider{}
	c := NewLLMCompressor(cp, 6000)

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
	c := NewLLMCompressor(cp, 6000)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = c.Compress(ctx, "m", []provider.ChatMessage{{Role: provider.RoleUser, Content: "x"}})
	if cp.ctx == nil || cp.ctx.Err() == nil {
		t.Fatal("compressor must propagate caller ctx (cancellation lost)")
	}
}
