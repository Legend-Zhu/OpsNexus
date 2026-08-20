package provider

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// sseServer 返回一段固定 SSE body 的测试服务。
func sseServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, body)
	}))
}

// collectEvents 收集到流关闭。
func collectEvents(ch <-chan StreamEvent) []StreamEvent {
	var out []StreamEvent
	for ev := range ch {
		out = append(out, ev)
	}
	return out
}

// TestOpenAIStreamUsageAfterFinish usage 在 finish_reason 之后以独立 chunk
// 到达（stream_options.include_usage 标准行为）→ 不再被丢弃，EventDone
// 恰好发送一次并携带最终 usage。
func TestOpenAIStreamUsageAfterFinish(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":7,\"total_tokens\":18}}\n\n" +
		"data: [DONE]\n\n"
	srv := sseServer(t, sse)
	defer srv.Close()

	p := NewOpenAIProvider("t", srv.URL, "k", []string{"m"})
	ch, err := p.ChatCompletionStream(context.Background(), &ChatRequest{
		Model:    "m",
		Stream:   true,
		Messages: []ChatMessage{{Role: RoleUser, Content: "q"}},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	events := collectEvents(ch)

	var done []StreamEvent
	for _, ev := range events {
		if ev.Type == EventError {
			t.Fatalf("unexpected error event: %v", ev.Error)
		}
		if ev.Type == EventDone {
			done = append(done, ev)
		}
	}
	if len(done) != 1 {
		t.Fatalf("expected exactly 1 EventDone, got %d", len(done))
	}
	u := done[0].Usage
	if u == nil || u.PromptTokens != 11 || u.CompletionTokens != 7 || u.TotalTokens != 18 {
		t.Fatalf("done usage = %+v, want 11/7/18", u)
	}
	if done[0].FinishReason != "stop" {
		t.Fatalf("finish reason = %q, want stop", done[0].FinishReason)
	}
}

// TestOpenAIStreamEndsWithoutDoneMarker 无 [DONE] 的流（异常 EOF）→ 兜底
// 发送一次 EventDone，已收到的 usage 不丢。
func TestOpenAIStreamEndsWithoutDoneMarker(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"x\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":4,\"total_tokens\":7}}\n\n"
	srv := sseServer(t, sse)
	defer srv.Close()

	p := NewOpenAIProvider("t", srv.URL, "k", []string{"m"})
	ch, err := p.ChatCompletionStream(context.Background(), &ChatRequest{
		Model:    "m",
		Messages: []ChatMessage{{Role: RoleUser, Content: "q"}},
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	var done []StreamEvent
	for ev := range ch {
		if ev.Type == EventDone {
			done = append(done, ev)
		}
	}
	if len(done) != 1 || done[0].Usage == nil || done[0].Usage.TotalTokens != 7 {
		t.Fatalf("fallback done missing/usage lost: %+v", done)
	}
}

// TestOpenAIStreamToolCallsThenUsage 工具调用流：工具结束事件只发送一次，
// EventDone 携带 usage。
func TestOpenAIStreamToolCallsThenUsage(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"function\":{\"name\":\"exec\",\"arguments\":\"{}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":6,\"total_tokens\":11}}\n\n" +
		"data: [DONE]\n\n"
	srv := sseServer(t, sse)
	defer srv.Close()

	p := NewOpenAIProvider("t", srv.URL, "k", []string{"m"})
	ch, err := p.ChatCompletionStream(context.Background(), &ChatRequest{Model: "m"})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	var toolEnds, dones int
	var usage *UsageInfo
	for ev := range ch {
		switch ev.Type {
		case EventToolCallEnd:
			toolEnds++
		case EventDone:
			dones++
			usage = ev.Usage
		}
	}
	if toolEnds != 1 {
		t.Fatalf("expected 1 tool-call end, got %d", toolEnds)
	}
	if dones != 1 || usage == nil || usage.TotalTokens != 11 {
		t.Fatalf("dones=%d usage=%+v", dones, usage)
	}
}
