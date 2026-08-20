package usage

import (
	"context"
	"errors"
	"sync"
	"testing"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/provider"
)

// recordingSink 线程安全收集记录。
type recordingSink struct {
	mu      sync.Mutex
	records []Record
}

func (r *recordingSink) Record(rec Record) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, rec)
}

func (r *recordingSink) snapshot() []Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Record(nil), r.records...)
}

// fakeProvider 按 hook 行为化。
type fakeProvider struct {
	chatFn   func(ctx context.Context, req *provider.ChatRequest) (*provider.ChatResponse, error)
	streamFn func(ctx context.Context, req *provider.ChatRequest) (<-chan provider.StreamEvent, error)
}

func (f *fakeProvider) Name() string                    { return "fp" }
func (f *fakeProvider) ToolFormat() provider.ToolFormat { return provider.ToolFormatOpenAI }
func (f *fakeProvider) Models() []string                { return []string{"m"} }
func (f *fakeProvider) SupportsModel(m string) bool     { return m == "m" }
func (f *fakeProvider) ChatCompletion(ctx context.Context, req *provider.ChatRequest) (*provider.ChatResponse, error) {
	return f.chatFn(ctx, req)
}
func (f *fakeProvider) ChatCompletionStream(ctx context.Context, req *provider.ChatRequest) (<-chan provider.StreamEvent, error) {
	return f.streamFn(ctx, req)
}

func TestMeteredChatCompletionRecordsSuccess(t *testing.T) {
	sink := &recordingSink{}
	fp := &fakeProvider{chatFn: func(_ context.Context, _ *provider.ChatRequest) (*provider.ChatResponse, error) {
		return &provider.ChatResponse{Usage: provider.UsageInfo{PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30}}, nil
	}}
	m := NewMetered(fp, sink)

	ctx := NewOperation(context.Background(), ScenarioChat, "/api/v1/ainexus/chat")
	ctx = WithRound(ctx, 3)
	if _, err := m.ChatCompletion(ctx, &provider.ChatRequest{Model: "m"}); err != nil {
		t.Fatalf("chat: %v", err)
	}

	recs := sink.snapshot()
	if len(recs) != 1 {
		t.Fatalf("expected 1 record, got %d", len(recs))
	}
	r := recs[0]
	if r.Scenario != ScenarioChat || r.Round != 3 || r.Provider != "fp" || r.Model != "m" {
		t.Fatalf("meta mismatch: %+v", r)
	}
	if r.OperationID == "" || r.CallID == "" || r.EntryPoint != "/api/v1/ainexus/chat" {
		t.Fatalf("ids missing: %+v", r)
	}
	if !r.UsagePresent || r.PromptTokens != 10 || r.CompletionTokens != 20 || r.TotalTokens != 30 {
		t.Fatalf("usage mismatch: %+v", r)
	}
	if !r.OK || r.Status != StatusSuccess || r.LatencyMs < 0 {
		t.Fatalf("status mismatch: %+v", r)
	}
}

func TestMeteredChatCompletionRecordsProviderError(t *testing.T) {
	sink := &recordingSink{}
	fp := &fakeProvider{chatFn: func(_ context.Context, _ *provider.ChatRequest) (*provider.ChatResponse, error) {
		return nil, errors.New("upstream 500")
	}}
	m := NewMetered(fp, sink)

	ctx := NewOperation(context.Background(), ScenarioInvestigate, "/inv")
	if _, err := m.ChatCompletion(ctx, &provider.ChatRequest{Model: "m"}); err == nil {
		t.Fatal("expected error")
	}
	recs := sink.snapshot()
	if len(recs) != 1 {
		t.Fatalf("expected 1 record, got %d", len(recs))
	}
	r := recs[0]
	if r.OK || r.Status != StatusProviderError || r.Error == "" || r.UsagePresent {
		t.Fatalf("error record mismatch: %+v", r)
	}
}

func TestMeteredRecordsCanceled(t *testing.T) {
	sink := &recordingSink{}
	fp := &fakeProvider{chatFn: func(ctx context.Context, _ *provider.ChatRequest) (*provider.ChatResponse, error) {
		return nil, ctx.Err()
	}}
	m := NewMetered(fp, sink)

	ctx, cancel := context.WithCancel(NewOperation(context.Background(), ScenarioChat, "/x"))
	cancel()
	if _, err := m.ChatCompletion(ctx, &provider.ChatRequest{Model: "m"}); err == nil {
		t.Fatal("expected error")
	}
	recs := sink.snapshot()
	if len(recs) != 1 || recs[0].Status != StatusCanceled {
		t.Fatalf("canceled record mismatch: %+v", recs)
	}
}

func TestMeteredWithoutMetaNoRecord(t *testing.T) {
	sink := &recordingSink{}
	fp := &fakeProvider{chatFn: func(_ context.Context, _ *provider.ChatRequest) (*provider.ChatResponse, error) {
		return &provider.ChatResponse{}, nil
	}}
	m := NewMetered(fp, sink)

	// 无计量元数据（未从入口接入）→ 不记录、完全透传
	if _, err := m.ChatCompletion(context.Background(), &provider.ChatRequest{Model: "m"}); err != nil {
		t.Fatalf("chat: %v", err)
	}
	if recs := sink.snapshot(); len(recs) != 0 {
		t.Fatalf("expected no record, got %d", len(recs))
	}
}

func TestMeteredStreamRecordsAtEnd(t *testing.T) {
	sink := &recordingSink{}
	fp := &fakeProvider{streamFn: func(_ context.Context, _ *provider.ChatRequest) (<-chan provider.StreamEvent, error) {
		ch := make(chan provider.StreamEvent, 4)
		ch <- provider.StreamEvent{Type: provider.EventContentDelta, Content: "hi"}
		ch <- provider.StreamEvent{Type: provider.EventDone, FinishReason: "stop",
			Usage: &provider.UsageInfo{PromptTokens: 4, CompletionTokens: 5, TotalTokens: 9}}
		close(ch)
		return ch, nil
	}}
	m := NewMetered(fp, sink)

	ctx := NewOperation(context.Background(), ScenarioNativeChat, "/ainexus/v1/chat/completions")
	out, err := m.ChatCompletionStream(ctx, &provider.ChatRequest{Model: "m"})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	var sawDelta, sawDone bool
	for ev := range out {
		switch ev.Type {
		case provider.EventContentDelta:
			sawDelta = true
		case provider.EventDone:
			sawDone = true
		}
	}
	if !sawDelta || !sawDone {
		t.Fatalf("events not forwarded: delta=%v done=%v", sawDelta, sawDone)
	}

	recs := sink.snapshot()
	if len(recs) != 1 {
		t.Fatalf("expected 1 record after stream end, got %d", len(recs))
	}
	r := recs[0]
	if !r.UsagePresent || r.TotalTokens != 9 || !r.OK || r.Scenario != ScenarioNativeChat {
		t.Fatalf("stream record mismatch: %+v", r)
	}
}

func TestMeteredStreamStartErrorRecords(t *testing.T) {
	sink := &recordingSink{}
	fp := &fakeProvider{streamFn: func(_ context.Context, _ *provider.ChatRequest) (<-chan provider.StreamEvent, error) {
		return nil, errors.New("dial failed")
	}}
	m := NewMetered(fp, sink)

	ctx := NewOperation(context.Background(), ScenarioChat, "/x")
	if _, err := m.ChatCompletionStream(ctx, &provider.ChatRequest{Model: "m"}); err == nil {
		t.Fatal("expected error")
	}
	recs := sink.snapshot()
	if len(recs) != 1 || recs[0].Status != StatusProviderError || recs[0].UsagePresent {
		t.Fatalf("start-error record mismatch: %+v", recs)
	}
}

func TestNewChildKeepsOperation(t *testing.T) {
	ctx := NewOperation(context.Background(), ScenarioChat, "/x")
	child := NewChild(ctx, ScenarioCompress)
	m, ok := FromContext(child)
	if !ok {
		t.Fatal("child meta missing")
	}
	orig, _ := FromContext(ctx)
	if m.OperationID != orig.OperationID {
		t.Fatalf("operation id changed: %q vs %q", m.OperationID, orig.OperationID)
	}
	if m.Scenario != ScenarioCompress {
		t.Fatalf("scenario = %q, want %q", m.Scenario, ScenarioCompress)
	}
	// 无元数据时原样返回（不注入）
	if _, ok := FromContext(NewChild(context.Background(), ScenarioCompress)); ok {
		t.Fatal("NewChild on bare ctx should not inject meta")
	}
}

func TestWithRoundWithoutMetaNoop(t *testing.T) {
	if ctx := WithRound(context.Background(), 2); ctx != context.Background() {
		t.Fatal("WithRound on bare ctx should return unchanged ctx")
	}
}
