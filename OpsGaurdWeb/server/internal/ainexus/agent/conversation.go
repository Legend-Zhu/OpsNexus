package agent

import (
	"unicode/utf8"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/provider"
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

// TokenCount 粗估对话 token 数（rune/2 + 工具参数 + 每条消息开销），
// 用于上下文预算裁剪。精确 tokenizer 依赖外部依赖，离线环境用估算即可防爆。
func (c *Conversation) TokenCount() int {
	n := 0
	for _, m := range c.Messages {
		n += utf8.RuneCountInString(m.Content)/2 + 4 // 消息开销
		for _, tc := range m.ToolCalls {
			n += utf8.RuneCountInString(tc.Arguments) / 2
		}
	}
	return n
}

// Trim 将对话裁剪到 maxTokens 预算内。
//
// 关键约束：OpenAI 工具调用要求 role:tool 消息必须紧跟对应的
// assistant(tool_calls) 消息——简单按条裁剪会拆散配对导致 400。因此以
// “完整轮次”（一条带工具调用的 assistant + 其后连续的 tool 结果）为原子
// 单位：要么整轮保留、要么整轮删除，从最旧轮次开始删，且至少保留最近
// keepRounds 个工具轮次 + 最新一条非 system 消息。system 消息永不删除。
// 若保底区间仍超预算（单条极长，罕见），截断最后一条普通消息内容兜底。
func (c *Conversation) Trim(maxTokens, keepRounds int) {
	if maxTokens <= 0 || len(c.Messages) == 0 {
		return
	}
	if c.TokenCount() <= maxTokens {
		return
	}

	// 1. 按原子单位分组
	type grp struct{ start, end int; isTool bool }
	var groups []grp
	for i := 0; i < len(c.Messages); {
		m := c.Messages[i]
		if m.Role == provider.RoleAssistant && len(m.ToolCalls) > 0 {
			j := i + 1
			for j < len(c.Messages) && c.Messages[j].Role == provider.RoleTool {
				j++
			}
			groups = append(groups, grp{i, j, true})
			i = j
		} else {
			groups = append(groups, grp{i, i + 1, false})
			i++
		}
	}

	// 2. 从尾部确定必须保留的最靠前索引 keepFrom：
	//    至少保留最后一个非 system 组；若要求保留工具轮次，向前覆盖最近
	//    keepRounds 个工具轮次。
	keepFrom := 0
	found := false
	for gi := len(groups) - 1; gi >= 0; gi-- {
		if c.Messages[groups[gi].start].Role != provider.RoleSystem {
			keepFrom = gi
			found = true
			break
		}
	}
	if !found {
		return // 全是 system（理论上不存在）
	}
	if keepRounds > 0 {
		kept := 0
		for gi := keepFrom; gi >= 0; gi-- {
			if groups[gi].isTool {
				kept++
				if kept >= keepRounds {
					keepFrom = gi
					break
				}
			}
		}
	}

	// 3. 删除 [first, keepFrom) 组（跳过开头 system），它们构成连续消息段
	first := 0
	for first < keepFrom && c.Messages[groups[first].start].Role == provider.RoleSystem {
		first++
	}
	if first < keepFrom {
		start := groups[first].start
		end := groups[keepFrom].start
		kept := make([]provider.ChatMessage, 0, len(c.Messages)-(end-start))
		kept = append(kept, c.Messages[:start]...)
		kept = append(kept, c.Messages[end:]...)
		c.Messages = kept
	}

	// 4. 兜底：保底区间仍超预算 → 截断最后一条非 system 消息
	//    预留各消息固定开销 + 64 token 余量，避免 rune≈token 估算误差导致仍超。
	if c.TokenCount() > maxTokens {
		overhead := 4 * len(c.Messages)
		budget := maxTokens - overhead - 64
		if budget < 32 {
			budget = 32
		}
		limit := budget * 2 // rune 估算 ≈ token x2
		for i := len(c.Messages) - 1; i >= 0; i-- {
			if c.Messages[i].Role != provider.RoleSystem {
				if r := utf8.RuneCountInString(c.Messages[i].Content); r > limit {
					runes := []rune(c.Messages[i].Content)
					c.Messages[i].Content = string(runes[:limit]) + "\n…[context truncated]"
				}
				break
			}
		}
	}
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
