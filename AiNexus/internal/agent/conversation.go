package agent

import (
	"github.com/legeosoft/ainexus/internal/provider"
)

// Conversation 对话上下文
type Conversation struct {
	Messages []provider.ChatMessage `json:"messages"`
	Model    string                 `json:"model"`
	Stream   bool                   `json:"stream"`
}

// NewConversation 创建新对话
func NewConversation(model string) *Conversation {
	return &Conversation{
		Messages: make([]provider.ChatMessage, 0),
		Model:    model,
	}
}

// AddSystemMessage 添加系统消息
func (c *Conversation) AddSystemMessage(content string) {
	c.Messages = append(c.Messages, provider.ChatMessage{
		Role:    provider.RoleSystem,
		Content: content,
	})
}

// AddUserMessage 添加用户消息
func (c *Conversation) AddUserMessage(content string) {
	c.Messages = append(c.Messages, provider.ChatMessage{
		Role:    provider.RoleUser,
		Content: content,
	})
}

// AddAssistantMessage 添加助手消息
func (c *Conversation) AddAssistantMessage(content string) {
	c.Messages = append(c.Messages, provider.ChatMessage{
		Role:    provider.RoleAssistant,
		Content: content,
	})
}

// AddAssistantToolCalls 添加带工具调用的助手消息
func (c *Conversation) AddAssistantToolCalls(toolCalls []provider.ToolCall, content string) {
	c.Messages = append(c.Messages, provider.ChatMessage{
		Role:      provider.RoleAssistant,
		Content:   content,
		ToolCalls: toolCalls,
	})
}

// AddToolResult 添加工具调用结果消息
func (c *Conversation) AddToolResult(toolCallID string, content string) {
	c.Messages = append(c.Messages, provider.ChatMessage{
		Role:       provider.RoleTool,
		Content:    content,
		ToolCallID: toolCallID,
	})
}

// LastUserMessage 获取最后一条用户消息
func (c *Conversation) LastUserMessage() string {
	for i := len(c.Messages) - 1; i >= 0; i-- {
		if c.Messages[i].Role == provider.RoleUser {
			return c.Messages[i].Content
		}
	}
	return ""
}

// Clone 克隆对话上下文
func (c *Conversation) Clone() *Conversation {
	msgs := make([]provider.ChatMessage, len(c.Messages))
	copy(msgs, c.Messages)
	return &Conversation{
		Messages: msgs,
		Model:    c.Model,
		Stream:   c.Stream,
	}
}

// ToRequest 转换为 Provider 请求
func (c *Conversation) ToRequest(tools []provider.ToolDefinition, stream bool) *provider.ChatRequest {
	return &provider.ChatRequest{
		Model:    c.Model,
		Messages: c.Messages,
		Tools:    tools,
		Stream:   stream,
	}
}
