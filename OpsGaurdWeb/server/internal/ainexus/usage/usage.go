// Package usage provides per-provider-call metering plumbing for the
// embedded AiNexus gateway: call metadata propagated via context.Context,
// a Sink interface receiving one Record per underlying LLM call, and a
// Metered provider decorator. The gateway core stays independent of the
// MLOps service — an absent sink or absent context metadata disables
// metering entirely with zero behavioral impact.
//
// 计量单位是一次底层 provider 调用（ChatCompletion 一次，或一条完整
// ChatCompletionStream 流），不是一次业务请求：Agent 工具轮次与上下文
// 压缩都会产生独立的底层调用，各自形成一条 Record。
package usage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"time"
)

// 场景标识（业务入口 → scenario）。费用报表按 scenario 维度聚合。
const (
	ScenarioChat         = "chat"          // /api/v1/ainexus/chat（对话式排查）
	ScenarioInvestigate  = "investigate"   // /api/v1/ainexus/investigate（深度排查）
	ScenarioNativeChat   = "native_chat"   // /ainexus/v1/* 原生兼容端点
	ScenarioPatrolReport = "patrol_report" // Server.Summarize（巡检 AI 报告）
	ScenarioCompress     = "compress"      // Agent 上下文摘要压缩（内部调用）
	ScenarioHealth       = "health"        // 连通性测试（/config/test）
)

// 调用结束状态。
const (
	StatusSuccess       = "success"
	StatusProviderError = "provider_error"
	StatusCanceled      = "canceled"
)

// Record 一次底层 provider 调用的计量记录。
type Record struct {
	OperationID      string // 一次业务请求（chat/investigate/patrol...）的 ID
	CallID           string // 本次 provider 调用 ID（幂等去重用）
	ParentCallID     string // 触发本次调用的上层调用（预留，compress 场景用）
	Provider         string // provider 名称
	Model            string // 请求模型名
	Scenario         string // chat | investigate | native_chat | patrol_report | compress | health
	EntryPoint       string // 入口标识（API route 或 summarize）
	Round            int    // Agent ReAct 轮次（0 起；非 Agent 调用为 0）
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	UsagePresent     bool // 上游是否返回 usage（缺失 ≠ 0 token，报表单列）
	OK               bool
	Status           string // success | provider_error | canceled
	Error            string
	StartedAt        time.Time
	FinishedAt       time.Time
	LatencyMs        int64
}

// Sink 计量回调。实现必须自行保证并发安全且不阻塞调用方响应。
type Sink interface {
	Record(Record)
}

// SinkFunc 函数适配器。
type SinkFunc func(Record)

func (f SinkFunc) Record(r Record) { f(r) }

// CallMeta 随请求传播的计量元数据（context 携带，业务入口创建一次）。
type CallMeta struct {
	OperationID  string
	ParentCallID string
	Scenario     string
	EntryPoint   string
	Round        int
}

type ctxKey struct{}

// NewOperation 在业务入口创建计量元数据（生成新 operation_id）。
// 已存在元数据时整体覆盖——入口应只调用一次。
func NewOperation(ctx context.Context, scenario, entryPoint string) context.Context {
	return context.WithValue(ctx, ctxKey{}, CallMeta{
		OperationID: newID("op"),
		Scenario:    scenario,
		EntryPoint:  entryPoint,
	})
}

// NewChild 派生子场景：同一 operation、独立 scenario（压缩调用用）。
// 无元数据时原样返回（未接入计量的调用路径不做任何事）。
func NewChild(ctx context.Context, scenario string) context.Context {
	m, ok := FromContext(ctx)
	if !ok {
		return ctx
	}
	m.Scenario = scenario
	return context.WithValue(ctx, ctxKey{}, m)
}

// WithRound 标注 Agent ReAct 轮次。无元数据时原样返回。
func WithRound(ctx context.Context, round int) context.Context {
	m, ok := FromContext(ctx)
	if !ok {
		return ctx
	}
	m.Round = round
	return context.WithValue(ctx, ctxKey{}, m)
}

// FromContext 读取计量元数据。
func FromContext(ctx context.Context) (CallMeta, bool) {
	m, ok := ctx.Value(ctxKey{}).(CallMeta)
	return m, ok
}

// NewCallID 生成一次 provider 调用的唯一 ID。
func NewCallID() string { return newID("call") }

func newID(prefix string) string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 失败极罕见；退化为纳秒时间戳保底
		return prefix + "-" + time.Now().Format("20060102150405.000000000")
	}
	return prefix + "-" + hex.EncodeToString(b[:])
}
