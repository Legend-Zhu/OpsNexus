package agent

import (
	"context"
	"unicode/utf8"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/provider"
)

// Compressor 把一段旧对话压缩成一句话纪要。
// 摘要式压缩(类 Trae Memory 思路):保留根因/关键命令/观测值/结论等有信息密度的
// 内容,替代硬删除;摘要本身也计入 token 预算,避免摘要膨胀。
type Compressor interface {
	// Compress 压缩 messages 为一段纪要文本；返回空串表示压缩失败（调用方
	// 退化回硬删）。ctx 传递取消信号与计量场景（派生 scenario=compress）；
	// model 为对话实际模型（压缩调用沿用同一模型）。
	Compress(ctx context.Context, model string, messages []provider.ChatMessage) string
}

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

// Trim 将对话裁剪到 maxTokens 预算内（无压缩器时的硬删版本，保留给
// 未配置 LLM 摘要的场景）。
//
// 关键约束：OpenAI 工具调用要求 role:tool 消息必须紧跟对应的
// assistant(tool_calls) 消息——简单按条裁剪会拆散配对导致 400。因此以
// “完整轮次”（一条带工具调用的 assistant + 其后连续的 tool 结果）为原子
// 单位：要么整轮保留、要么整轮删除，从最旧轮次开始删，且至少保留最近
// keepRounds 个工具轮次 + 最新一条非 system 消息。system 消息永不删除。
// 若保底区间仍超预算（单条极长，罕见），截断最后一条普通消息内容兜底。
func (c *Conversation) Trim(maxTokens, keepRounds int) {
	c.Compress(context.Background(), nil, maxTokens, keepRounds)
}

// Compress 将对话裁剪到 maxTokens 预算内，优先用摘要压缩：
//
//  1. 保底区间：最近 keepRounds 个工具轮次 + 最新非 system 消息，完整保留；
//  2. 可压缩区：更早的轮次，逐轮调用 compressor 生成一句话纪要，聚合成一条
//     [历史摘要] 消息替换原轮次（system 永不参与）；
//  3. 兜底：摘要仍超预算或摘要失败时，按完整轮次硬删；单条超长内容截断。
//
// compressor 为 nil 时直接走硬删（等价于旧 Trim 行为）。
func (c *Conversation) Compress(ctx context.Context, compressor Compressor, maxTokens, keepRounds int) {
	if maxTokens <= 0 || len(c.Messages) == 0 {
		return
	}
	if c.TokenCount() <= maxTokens {
		return
	}

	// 1. 按原子单位分组
	type grp struct {
		start, end int
		isTool     bool
	}
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

	// 2. 确定保底区起点：最后一个非 system 组；向前覆盖最近 keepRounds 个工具轮次
	keepFrom := len(groups) - 1
	for keepFrom >= 0 && c.Messages[groups[keepFrom].start].Role == provider.RoleSystem {
		keepFrom--
	}
	if keepFrom < 0 {
		return // 全是 system
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

	// 3. 可压缩区 = [first, keepFrom)（跳过开头 system）
	first := 0
	for first < keepFrom && c.Messages[groups[first].start].Role == provider.RoleSystem {
		first++
	}
	if first >= keepFrom {
		c.trimFallback(maxTokens)
		return
	}

	if compressor != nil {
		// 逐组压缩成纪要（从旧到新，便于摘要器理解上下文顺序）
		var notes []string
		for gi := first; gi < keepFrom; gi++ {
			msgs := c.Messages[groups[gi].start:groups[gi].end]
			if note := compressor.Compress(ctx, c.Model, msgs); note != "" {
				notes = append(notes, note)
			}
		}
		if len(notes) > 0 {
			// 用一条 [历史摘要] 消息替换整个可压缩区
			summary := "[历史摘要]\n" + joinNotes(notes)
			summaryMsg := provider.ChatMessage{Role: provider.RoleUser, Content: summary}
			kept := make([]provider.ChatMessage, 0, len(c.Messages)+1)
			kept = append(kept, c.Messages[:groups[first].start]...)
			kept = append(kept, summaryMsg)
			kept = append(kept, c.Messages[groups[keepFrom].start:]...)
			c.Messages = kept

			// 摘要后仍超预算 → 兜底硬删/截断（先试再删一轮保底外内容）
			if c.TokenCount() > maxTokens {
				c.trimFallback(maxTokens)
			}
			return
		}
		// 摘要全部失败 → 退化为硬删
	}

	// 4. 无压缩器或摘要失败：硬删可压缩区
	kept := make([]provider.ChatMessage, 0, len(c.Messages))
	kept = append(kept, c.Messages[:groups[first].start]...)
	kept = append(kept, c.Messages[groups[keepFrom].start:]...)
	c.Messages = kept

	c.trimFallback(maxTokens)
}

// trimFallback 兜底：仍超预算时截断最后一条非 system 消息内容
// （预留各消息固定开销 + 64 token 余量，避免 rune≈token 估算误差）。
func (c *Conversation) trimFallback(maxTokens int) {
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
				// 保留头尾 + 中部省略标记
				head := runes[:limit/2]
				tail := runes[len(runes)-limit/2:]
				c.Messages[i].Content = string(head) + "\n…[中间省略 %d 字符]\n" + string(tail)
			}
			break
		}
	}
}

// joinNotes 拼接逐轮纪要。
func joinNotes(notes []string) string {
	out := ""
	for i, n := range notes {
		if i > 0 {
			out += "\n"
		}
		out += "- " + n
	}
	return out
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
