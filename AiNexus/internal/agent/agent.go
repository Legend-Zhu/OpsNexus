package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"

	"github.com/legeosoft/ainexus/internal/config"
	"github.com/legeosoft/ainexus/internal/provider"
	"github.com/legeosoft/ainexus/internal/tool"
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
	provider provider.Provider
	registry *tool.Registry
	config   config.AgentConfig
	logger   *log.Logger
}

// New 创建 Agent
func New(p provider.Provider, registry *tool.Registry, cfg config.AgentConfig, logger *log.Logger) *Agent {
	return &Agent{
		provider: p,
		registry: registry,
		config:   cfg,
		logger:   logger,
	}
}

// RunStream 流式运行 Agent，返回事件 channel
func (a *Agent) RunStream(ctx context.Context, conv *Conversation) (<-chan AgentEvent, error) {
	toolDefs := a.registry.ToolDefinitions()
	req := conv.ToRequest(toolDefs, true)

	eventCh, err := a.provider.ChatCompletionStream(ctx, req)
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
		a.reactLoop(ctx, conv, eventCh, outCh, 0)
	}()
	return outCh, nil
}

// Run 非流式运行 Agent
func (a *Agent) Run(ctx context.Context, conv *Conversation) (*provider.ChatResponse, error) {
	toolDefs := a.registry.ToolDefinitions()

	for round := 0; round < a.config.MaxToolRounds; round++ {
		req := conv.ToRequest(toolDefs, false)
		resp, err := a.provider.ChatCompletion(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("chat completion round %d: %w", round, err)
		}

		if len(resp.Choices) == 0 {
			return nil, fmt.Errorf("no choices in response")
		}

		choice := resp.Choices[0]

		// 如果没有工具调用，直接返回
		if len(choice.Message.ToolCalls) == 0 {
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
// 注意：outCh 由 RunStream 中的 wrapper goroutine 负责关闭，此处不 close
func (a *Agent) reactLoop(ctx context.Context, conv *Conversation, eventCh <-chan provider.StreamEvent, outCh chan<- AgentEvent, round int) {
	// 收集当前轮次的文本和工具调用
	var currentText string
	toolCallsMap := make(map[int]*provider.ToolCall) // key: toolCallIndex
	var toolCallCounter int
	var usage *provider.UsageInfo
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
					usage = event.Usage
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
				Usage:   usage,
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
	toolDefs := a.registry.ToolDefinitions()
	req := conv.ToRequest(toolDefs, true)
	newEventCh, err := a.provider.ChatCompletionStream(ctx, req)
	if err != nil {
		outCh <- AgentEvent{
			Type:  AgentEventError,
			Error: fmt.Errorf("stream continuation failed: %w", err),
		}
		return
	}

	// 递归进入下一轮
	a.reactLoop(ctx, conv, newEventCh, outCh, round+1)
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
