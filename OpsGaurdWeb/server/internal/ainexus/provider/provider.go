package provider

import (
	"context"
)

// ToolFormat 工具格式类型
type ToolFormat string

const (
	ToolFormatOpenAI    ToolFormat = "openai"
	ToolFormatAnthropic ToolFormat = "anthropic"

	// MaxStreamChunkRunes 单条流式 chunk 的最大字符数：上游模型/网关可能
	// 一次吐超长文本，Agent 会把它累积进对话。1M 上下文下允许较大单条
	// （32K rune），仅防极端单条塞爆窗口。
	MaxStreamChunkRunes = 32 << 10
)

// Role 消息角色
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// ChatMessage 通用消息格式
type ChatMessage struct {
	Role       Role       `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

// ToolCall 工具调用
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolDefinition 工具定义（通用格式）
type ToolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// ChatRequest 通用对话请求
type ChatRequest struct {
	Model       string           `json:"model"`
	Messages    []ChatMessage    `json:"messages"`
	Tools       []ToolDefinition `json:"tools,omitempty"`
	Stream      bool             `json:"stream"`
	MaxTokens   int              `json:"max_tokens,omitempty"`
	Temperature float64          `json:"temperature,omitempty"`
}

// ChatResponse 通用对话响应（非流式）
type ChatResponse struct {
	ID      string       `json:"id"`
	Model   string       `json:"model"`
	Choices []ChatChoice `json:"choices"`
	Usage   UsageInfo    `json:"usage"`
}

// ChatChoice 对话选择
type ChatChoice struct {
	Message      ChatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
	Index        int         `json:"index"`
}

// UsageInfo 用量信息
type UsageInfo struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// StreamEventType 流式事件类型
type StreamEventType string

const (
	EventContentDelta  StreamEventType = "content_delta"
	EventToolCallStart StreamEventType = "tool_call_start"
	EventToolCallDelta StreamEventType = "tool_call_delta"
	EventToolCallEnd   StreamEventType = "tool_call_end"
	EventDone          StreamEventType = "done"
	EventError         StreamEventType = "error"
)

// StreamEvent 流式事件
type StreamEvent struct {
	Type         StreamEventType `json:"type"`
	Content      string          `json:"content,omitempty"`
	ToolCall     *ToolCall       `json:"tool_call,omitempty"`
	FinishReason string          `json:"finish_reason,omitempty"`
	Error        error           `json:"error,omitempty"`
	Usage        *UsageInfo      `json:"usage,omitempty"`
}

// Provider LLM 提供商接口
type Provider interface {
	// ChatCompletionStream 流式对话补全
	ChatCompletionStream(ctx context.Context, req *ChatRequest) (<-chan StreamEvent, error)
	// ChatCompletion 非流式对话补全
	ChatCompletion(ctx context.Context, req *ChatRequest) (*ChatResponse, error)
	// ToolFormat 返回该 Provider 的工具调用格式
	ToolFormat() ToolFormat
	// Name 返回提供商名称
	Name() string
	// Models 返回可用模型列表
	Models() []string
	// SupportsModel 检查是否支持指定模型
	SupportsModel(model string) bool
}
