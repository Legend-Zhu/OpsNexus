package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/config"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/provider"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/tool"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/usage"
)

// AgentEventType Agent 事件类型
type AgentEventType string

const (
	AgentEventText      AgentEventType = "text"
	AgentEventToolStart AgentEventType = "tool_start"
	AgentEventToolEnd   AgentEventType = "tool_end"
	AgentEventDone      AgentEventType = "done"
	AgentEventError     AgentEventType = "error"
)

// AgentEvent Agent 输出事件
type AgentEvent struct {
	Type       AgentEventType      `json:"type"`
	Content    string              `json:"content,omitempty"`
	ToolCall   *provider.ToolCall  `json:"tool_call,omitempty"`
	ToolResult *tool.ToolResult    `json:"tool_result,omitempty"`
	Usage      *provider.UsageInfo `json:"usage,omitempty"`
	Error      error               `json:"error,omitempty"`
}

// Agent ReAct Agent
type Agent struct {
	provider   provider.Provider
	registry   *tool.Registry
	config     config.AgentConfig
	logger     *log.Logger
	compressor Compressor // 旧轮次摘要压缩器（nil=仅硬删）
	prompts    ScenarioTemplateSource
	// toolFilter 可选工具过滤器（nil = 全量下发）。返回 false 的工具定义
	// 不进入模型请求（会话级集群 scoping：多集群工具全部在册，模型只见
	// 目标集群的，杜绝跨集群误调用）。过滤只影响下发，registry.Get 执行
	// 不受限。
	toolFilter func(provider.ToolDefinition) bool
}

// SetToolFilter 设置工具定义过滤器（须在 Run/RunStream 前调用）。
func (a *Agent) SetToolFilter(f func(provider.ToolDefinition) bool) { a.toolFilter = f }

// toolDefinitions 返回本轮下发给模型的工具定义（应用过滤器）。
func (a *Agent) toolDefinitions() []provider.ToolDefinition {
	defs := a.registry.ToolDefinitions()
	if a.toolFilter == nil {
		return defs
	}
	out := make([]provider.ToolDefinition, 0, len(defs))
	for _, d := range defs {
		if a.toolFilter(d) {
			out = append(out, d)
		}
	}
	return out
}

// toolScopeCtxKey request context 中的工具作用域 key（会话级集群 scoping）。
type toolScopeCtxKey struct{}

// WithToolScope 在 ctx 中声明本次请求仅下发的 MCP server 名
// （如 "cluster:自然灾害集群"），与 mcp.ToolScopeFilter 配套使用。
func WithToolScope(ctx context.Context, serverName string) context.Context {
	return context.WithValue(ctx, toolScopeCtxKey{}, serverName)
}

// ToolScopeFromContext 读取工具作用域（"" = 未声明，全量下发）。
func ToolScopeFromContext(ctx context.Context) string {
	v, _ := ctx.Value(toolScopeCtxKey{}).(string)
	return v
}

// ScenarioTemplateSource 提示词场景模板来源（mlops 运营层注入；nil =
// 代码内置默认）。引擎不依赖 mlops 包，仅依赖此最小接口。
type ScenarioTemplateSource interface {
	// ScenarioTemplate 返回场景 active 版本首条消息的模板原文；
	// ok=false 表示未自定义（用内置默认）。
	ScenarioTemplate(scenario string) (string, bool)
}

// ScenarioCompressSystem 压缩提示词场景 key（与 mlops Prompt Hub 同名约定）。
const ScenarioCompressSystem = "compress_system"

// New 创建 Agent。prompts 为提示词模板来源（可为 nil）。
func New(p provider.Provider, registry *tool.Registry, cfg config.AgentConfig, logger *log.Logger, prompts ScenarioTemplateSource) *Agent {
	a := &Agent{
		provider: p,
		registry: registry,
		config:   cfg,
		logger:   logger,
		prompts:  prompts,
	}
	// 配置了上下文预算 → 用同一 provider 做旧轮次摘要（类 Trae Memory）。
	// 不额外消耗：仅在每次裁剪前调用；失败自动退化为硬删。
	if cfg.MaxContextTokens > 0 && p != nil {
		a.compressor = NewLLMCompressor(p, 6000, prompts)
	}
	return a
}

// trimContext 在每次请求前把对话裁剪到上下文预算内。
// 优先摘要压缩（保留根因/关键操作/观测），未配置预算或摘要失败则硬删。
func (a *Agent) trimContext(ctx context.Context, conv *Conversation) {
	if a.config.MaxContextTokens > 0 {
		conv.Compress(ctx, a.compressor, a.config.MaxContextTokens, a.config.KeepToolRounds)
	}
}

// RunStream 流式运行 Agent，返回事件 channel
func (a *Agent) RunStream(ctx context.Context, conv *Conversation) (<-chan AgentEvent, error) {
	toolDefs := a.toolDefinitions()
	a.trimContext(ctx, conv)
	req := conv.ToRequest(toolDefs, true)

	eventCh, err := a.provider.ChatCompletionStream(usage.WithRound(ctx, 0), req)
	if err != nil {
		return nil, fmt.Errorf("start stream: %w", err)
	}

	outCh := make(chan AgentEvent, 128)
	go func() {
		defer close(outCh)
		defer func() {
			if r := recover(); r != nil {
				a.logger.Printf("FATAL: reactLoop panic (recovered): %v", r)
				// 尝试发送错误事件，如果 channel 还没关
				select {
				case outCh <- AgentEvent{
					Type:  AgentEventError,
					Error: fmt.Errorf("internal panic: %v", r),
				}:
				default:
				}
			}
		}()
		total := &provider.UsageInfo{}
		a.reactLoop(ctx, conv, eventCh, outCh, 0, total)
	}()
	return outCh, nil
}

// Run 非流式运行 Agent
func (a *Agent) Run(ctx context.Context, conv *Conversation) (*provider.ChatResponse, error) {
	toolDefs := a.toolDefinitions()

	var total provider.UsageInfo
	for round := 0; round < a.config.MaxToolRounds; round++ {
		a.trimContext(ctx, conv)
		req := conv.ToRequest(toolDefs, false)
		resp, err := a.provider.ChatCompletion(usage.WithRound(ctx, round), req)
		if err != nil {
			return nil, fmt.Errorf("chat completion round %d: %w", round, err)
		}

		if len(resp.Choices) == 0 {
			return nil, fmt.Errorf("no choices in response")
		}

		// 多轮 usage 累计：最终响应的 usage 反映整次请求总量，而非仅最后
		// 一轮（每轮底层调用另经 usage.Metered 单独计量）。
		total.PromptTokens += resp.Usage.PromptTokens
		total.CompletionTokens += resp.Usage.CompletionTokens
		total.TotalTokens += resp.Usage.TotalTokens

		choice := resp.Choices[0]

		// 如果没有工具调用，直接返回
		if len(choice.Message.ToolCalls) == 0 {
			resp.Usage = total
			return resp, nil
		}

		a.logger.Printf("Agent round %d: %d tool calls", round, len(choice.Message.ToolCalls))

		// 将助手消息（含工具调用）加入对话
		conv.AddAssistantToolCalls(choice.Message.ToolCalls, choice.Message.Content)

		// 执行所有工具调用
		results := a.executeToolCalls(ctx, choice.Message.ToolCalls)

		// 将工具结果加入对话
		for _, result := range results {
			conv.AddToolResult(result.ToolCallID, result.Content)
		}
	}

	return nil, fmt.Errorf("agent reached max tool rounds (%d)", a.config.MaxToolRounds)
}

// reactLoop ReAct 循环（流式）
// 注意：outCh 由 RunStream 中的 wrapper goroutine 负责关闭，此处不 close；
// total 跨轮累计 usage（最终 Done 事件携带整次请求总量）。
func (a *Agent) reactLoop(ctx context.Context, conv *Conversation, eventCh <-chan provider.StreamEvent, outCh chan<- AgentEvent, round int, total *provider.UsageInfo) {
	// 收集当前轮次的文本和工具调用
	var currentText string
	toolCallsMap := make(map[int]*provider.ToolCall) // key: toolCallIndex
	var toolCallCounter int
	doneReceived := false // 标记是否已收到 Provider 的流结束信号

	for event := range eventCh {
		switch event.Type {
		case provider.EventContentDelta:
			currentText += event.Content
			outCh <- AgentEvent{
				Type:    AgentEventText,
				Content: event.Content,
			}

		case provider.EventToolCallStart:
			tc := &provider.ToolCall{
				ID:        event.ToolCall.ID,
				Name:      event.ToolCall.Name,
				Arguments: event.ToolCall.Arguments,
			}
			idx := toolCallCounter
			toolCallCounter++
			toolCallsMap[idx] = tc
			outCh <- AgentEvent{
				Type:     AgentEventToolStart,
				ToolCall: tc,
			}

		case provider.EventToolCallDelta:
			// 从 delta 事件中更新最后一个工具调用的参数
			if event.ToolCall != nil && toolCallCounter > 0 {
				lastIdx := toolCallCounter - 1
				if tc, ok := toolCallsMap[lastIdx]; ok {
					tc.Arguments = event.ToolCall.Arguments
				}
			}

		case provider.EventToolCallEnd:
			// 使用 EventToolCallEnd 中的完整 ToolCall 更新 map 中的对应条目
			if event.ToolCall != nil {
				for _, tc := range toolCallsMap {
					if tc.ID == event.ToolCall.ID {
						tc.Name = event.ToolCall.Name
						tc.Arguments = event.ToolCall.Arguments
						break
					}
				}
			}

		case provider.EventDone:
			if !doneReceived {
				doneReceived = true
				if event.Usage != nil {
					total.PromptTokens += event.Usage.PromptTokens
					total.CompletionTokens += event.Usage.CompletionTokens
					total.TotalTokens += event.Usage.TotalTokens
				}
			}

		case provider.EventError:
			outCh <- AgentEvent{
				Type:  AgentEventError,
				Error: event.Error,
			}
			return
		}
	}

	// 流结束，收集所有工具调用
	var toolCalls []provider.ToolCall
	for _, tc := range toolCallsMap {
		toolCalls = append(toolCalls, *tc)
	}

	// 如果没有工具调用或已达到最大轮次，返回最终结果
	if len(toolCalls) == 0 || round >= a.config.MaxToolRounds-1 {
		if len(toolCalls) == 0 {
			outCh <- AgentEvent{
				Type:    AgentEventDone,
				Content: currentText,
				Usage:   totalUsagePtr(total),
			}
		} else {
			outCh <- AgentEvent{
				Type:  AgentEventError,
				Error: fmt.Errorf("agent reached max tool rounds (%d)", a.config.MaxToolRounds),
			}
		}
		return
	}

	// 有工具调用，执行工具
	a.logger.Printf("Agent round %d: %d tool calls, text: %q", round, len(toolCalls), currentText)

	// 将助手消息加入对话
	conv.AddAssistantToolCalls(toolCalls, currentText)

	// 注意：工具调用的 AgentEventToolStart 已在流式处理中发送，此处不再重复发送

	results := a.executeToolCalls(ctx, toolCalls)

	// 将工具结果加入对话并通知客户端
	for _, result := range results {
		conv.AddToolResult(result.ToolCallID, result.Content)
		outCh <- AgentEvent{
			Type:       AgentEventToolEnd,
			ToolCall:   &provider.ToolCall{ID: result.ToolCallID, Name: result.ToolName},
			ToolResult: &tool.ToolResult{Content: result.Content, IsError: result.IsError},
		}
	}

	// 再次调用 Provider，继续 ReAct 循环
	toolDefs := a.toolDefinitions()
	a.trimContext(ctx, conv)
	req := conv.ToRequest(toolDefs, true)
	newEventCh, err := a.provider.ChatCompletionStream(usage.WithRound(ctx, round+1), req)
	if err != nil {
		outCh <- AgentEvent{
			Type:  AgentEventError,
			Error: fmt.Errorf("stream continuation failed: %w", err),
		}
		return
	}

	// 递归进入下一轮
	a.reactLoop(ctx, conv, newEventCh, outCh, round+1, total)
}

// totalUsagePtr 返回跨轮累计 usage 的拷贝（整次请求未获得任何 usage 时为 nil）。
func totalUsagePtr(total *provider.UsageInfo) *provider.UsageInfo {
	if total == nil || (total.PromptTokens == 0 && total.CompletionTokens == 0 && total.TotalTokens == 0) {
		return nil
	}
	u := *total
	return &u
}

// toolCallResult 工具调用结果
type toolCallResult struct {
	ToolCallID string
	ToolName   string
	Content    string
	IsError    bool
}

// executeToolCalls 执行工具调用
func (a *Agent) executeToolCalls(ctx context.Context, toolCalls []provider.ToolCall) []toolCallResult {
	results := make([]toolCallResult, len(toolCalls))

	if a.config.ParallelToolCalls && len(toolCalls) > 1 {
		var wg sync.WaitGroup
		for i, tc := range toolCalls {
			wg.Add(1)
			go func(idx int, call provider.ToolCall) {
				defer wg.Done()
				results[idx] = a.executeSingleTool(ctx, call)
			}(i, tc)
		}
		wg.Wait()
	} else {
		for i, tc := range toolCalls {
			results[i] = a.executeSingleTool(ctx, tc)
		}
	}

	return results
}

// executeSingleTool 执行单个工具调用
func (a *Agent) executeSingleTool(ctx context.Context, tc provider.ToolCall) toolCallResult {
	a.logger.Printf("Executing tool: %s (call_id: %s)", tc.Name, tc.ID)

	var params map[string]any
	if tc.Arguments != "" {
		if err := json.Unmarshal([]byte(tc.Arguments), &params); err != nil {
			return toolCallResult{
				ToolCallID: tc.ID,
				ToolName:   tc.Name,
				Content:    fmt.Sprintf("Failed to parse arguments: %s", err),
				IsError:    true,
			}
		}
	}
	if params == nil {
		params = make(map[string]any)
	}

	t, ok := a.registry.Get(tc.Name)
	if !ok {
		return toolCallResult{
			ToolCallID: tc.ID,
			ToolName:   tc.Name,
			Content:    fmt.Sprintf("Tool %q not found", tc.Name),
			IsError:    true,
		}
	}

	result, err := t.Execute(ctx, params)
	if err != nil {
		return toolCallResult{
			ToolCallID: tc.ID,
			ToolName:   tc.Name,
			Content:    fmt.Sprintf("Tool execution error: %s", err),
			IsError:    true,
		}
	}

	a.logger.Printf("Tool %s completed (error=%v)", tc.Name, result.IsError)
	return toolCallResult{
		ToolCallID: tc.ID,
		ToolName:   tc.Name,
		Content:    result.Content,
		IsError:    result.IsError,
	}
}
