package provider

import (
	"context"
	"testing"
)

// TestAnthropicStreamUsageMerged input 来自 message_start、output 来自
// message_delta → message_stop 只发送一次 EventDone 并携带合并 usage。
func TestAnthropicStreamUsageMerged(t *testing.T) {
	sse := "event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":25,\"output_tokens\":1}}}\n\n" +
		"event: content_block_start\n" +
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n" +
		"event: message_delta\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":12}}\n\n" +
		"event: message_stop\n" +
		"data: {\"type\":\"message_stop\"}\n\n"
	srv := sseServer(t, sse)
	defer srv.Close()

	p := NewAnthropicProvider("t", srv.URL, "k", []string{"m"})
	ch, err := p.ChatCompletionStream(context.Background(), &ChatRequest{
		Model:    "m",
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
	if u == nil || u.PromptTokens != 25 || u.CompletionTokens != 12 || u.TotalTokens != 37 {
		t.Fatalf("done usage = %+v, want 25/12/37", u)
	}
	if done[0].FinishReason != "end_turn" {
		t.Fatalf("finish reason = %q, want end_turn", done[0].FinishReason)
	}
}

// TestAnthropicStreamEndsWithoutMessageStop 无 message_stop 的流 → 兜底
// 发送一次 EventDone，usage 已收到的部分不丢。
func TestAnthropicStreamEndsWithoutMessageStop(t *testing.T) {
	sse := "event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":8,\"output_tokens\":1}}}\n\n" +
		"event: message_delta\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":9}}\n\n"
	srv := sseServer(t, sse)
	defer srv.Close()

	p := NewAnthropicProvider("t", srv.URL, "k", []string{"m"})
	ch, err := p.ChatCompletionStream(context.Background(), &ChatRequest{Model: "m"})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	var done []StreamEvent
	for ev := range ch {
		if ev.Type == EventDone {
			done = append(done, ev)
		}
	}
	if len(done) != 1 || done[0].Usage == nil || done[0].Usage.TotalTokens != 17 {
		t.Fatalf("fallback done missing/usage lost: %+v", done)
	}
}
