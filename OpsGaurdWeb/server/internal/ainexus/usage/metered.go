package usage

import (
	"context"
	"time"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/provider"
)

// Metered 包装一个 provider.Provider：每次底层调用（非流式一次、流式
// 一整条流）生成一条 Record 上报 sink。上下文无计量元数据时完全透传、
// 不记录——未接入的调用路径（测试、临时探测）零影响。
type Metered struct {
	inner provider.Provider
	sink  Sink
}

// NewMetered 包装 provider（sink 为 nil 时等价于不计量）。
func NewMetered(p provider.Provider, sink Sink) *Metered {
	return &Metered{inner: p, sink: sink}
}

func (m *Metered) Name() string                    { return m.inner.Name() }
func (m *Metered) ToolFormat() provider.ToolFormat { return m.inner.ToolFormat() }
func (m *Metered) Models() []string                { return m.inner.Models() }
func (m *Metered) SupportsModel(model string) bool { return m.inner.SupportsModel(model) }

// ChatCompletion 非流式调用：结束（成功/失败）即上报。
func (m *Metered) ChatCompletion(ctx context.Context, req *provider.ChatRequest) (*provider.ChatResponse, error) {
	start := time.Now()
	resp, err := m.inner.ChatCompletion(ctx, req)
	m.record(ctx, req, start, respUsage(resp), err)
	return resp, err
}

// ChatCompletionStream 流式调用：包装事件流，流结束（含错误/取消）后用
// 最终 usage 上报；事件转发顺序与内容不变。
func (m *Metered) ChatCompletionStream(ctx context.Context, req *provider.ChatRequest) (<-chan provider.StreamEvent, error) {
	start := time.Now()
	eventCh, err := m.inner.ChatCompletionStream(ctx, req)
	if err != nil {
		m.record(ctx, req, start, nil, err)
		return nil, err
	}

	out := make(chan provider.StreamEvent, 64)
	go func() {
		defer close(out)
		var usage *provider.UsageInfo
		var streamErr error
		for ev := range eventCh {
			switch {
			case ev.Type == provider.EventDone && ev.Usage != nil:
				usage = ev.Usage
			case ev.Type == provider.EventError && streamErr == nil:
				streamErr = ev.Error
			}
			out <- ev
		}
		m.record(ctx, req, start, usage, streamErr)
	}()
	return out, nil
}

// record 统一构造并上报计量记录。无元数据或无 sink 时静默跳过。
func (m *Metered) record(ctx context.Context, req *provider.ChatRequest, start time.Time, u *provider.UsageInfo, err error) {
	meta, ok := FromContext(ctx)
	if !ok || m.sink == nil {
		return
	}
	finished := time.Now()
	rec := Record{
		OperationID:  meta.OperationID,
		CallID:       NewCallID(),
		ParentCallID: meta.ParentCallID,
		Provider:     m.inner.Name(),
		Model:        req.Model,
		Scenario:     meta.Scenario,
		EntryPoint:   meta.EntryPoint,
		Round:        meta.Round,
		StartedAt:    start,
		FinishedAt:   finished,
		LatencyMs:    finished.Sub(start).Milliseconds(),
	}
	if u != nil {
		rec.PromptTokens = u.PromptTokens
		rec.CompletionTokens = u.CompletionTokens
		rec.TotalTokens = u.TotalTokens
		rec.UsagePresent = true
	}
	fillStatus(ctx, &rec, err)
	m.sink.Record(rec)
}

// fillStatus 依据错误与 context 状态填写 OK/Status/Error。
func fillStatus(ctx context.Context, rec *Record, err error) {
	switch {
	case err == nil:
		rec.OK = true
		rec.Status = StatusSuccess
	case ctx.Err() != nil:
		rec.Status = StatusCanceled
		rec.Error = truncStr(err.Error(), 512)
	default:
		rec.Status = StatusProviderError
		rec.Error = truncStr(err.Error(), 512)
	}
}

// respUsage 从非流式响应提取 usage（零值视同缺失）。
func respUsage(resp *provider.ChatResponse) *provider.UsageInfo {
	if resp == nil {
		return nil
	}
	u := resp.Usage
	if u.PromptTokens == 0 && u.CompletionTokens == 0 && u.TotalTokens == 0 {
		return nil
	}
	return &u
}

func truncStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
