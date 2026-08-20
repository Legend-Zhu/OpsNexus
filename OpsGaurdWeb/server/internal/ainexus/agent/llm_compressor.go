// LLM-based conversation summariser: compresses old ReAct rounds into
// concise "running notes" (Trae-Memory-style) so a long investigation keeps
// its causal thread (root cause, executed commands, key observations,
// conclusions) instead of dropping it. Falls back to a heuristic extractor
// when the LLM call fails (offline / provider down), so compression never
// blocks the loop.
package agent

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"text/template"
	"unicode/utf8"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/provider"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/usage"
)

// SummaryMode 摘要提取的语义类别（对应提示词中的分类维度）。
// Go text/template 语法（{{.MaxWords}}/{{.Content}}）；mlops 注入
// compress_system 场景 active 模板时可热替换，v1 与旧 %d/%s 版本逐字节等价
// （由本包回归测试守护）。
const summaryPromptTemplate = `你是会话压缩器。把下面的对话轮次压缩成一段不超过 {{.MaxWords}} 字的纪要,
只保留对后续排查有用的信息,按类别组织:
- 根因/结论:已确认或高度怀疑的根因
- 关键操作:执行过的命令/工具调用(名称+简要结果)
- 关键观测:数值(CPU/内存/端口等)与状态变化
- 待办/下一步:未完成的分析或建议
丢弃寒暄、重复与无关内容。直接输出纪要,不要解释。

对话轮次:
{{.Content}}
`

// maxCompressRenderBytes 压缩提示词渲染输出上限（防坏模板放大输入）。
const maxCompressRenderBytes = 1 << 20

// LLMCompressor 用内嵌 LLM 摘要旧轮次。
type LLMCompressor struct {
	p     provider.Provider
	maxIn int // 单轮参与摘要的最大字符（防超长轮次把摘要输入打爆）
	prompts ScenarioTemplateSource
}

// NewLLMCompressor 创建摘要压缩器。prompts 为提示词模板来源（可为 nil）。
func NewLLMCompressor(p provider.Provider, maxIn int, prompts ScenarioTemplateSource) *LLMCompressor {
	if maxIn <= 0 {
		maxIn = 6000
	}
	return &LLMCompressor{p: p, maxIn: maxIn, prompts: prompts}
}

// Compress 实现 Compressor 接口：摘要一段消息。
// ctx 传递取消信号与计量场景（派生 compress 子场景，归属同一 operation）；
// model 须为对话实际模型——此前写死 "summary" 会被模型池外名字拒绝或错路由。
func (c *LLMCompressor) Compress(ctx context.Context, model string, messages []provider.ChatMessage) string {
	if c == nil || c.p == nil || len(messages) == 0 {
		return ""
	}
	// 拼接轮次内容（限制输入）
	var b strings.Builder
	budget := c.maxIn
	for _, m := range messages {
		if b.Len() >= budget {
			break
		}
		part := stringifyMessage(m)
		remain := budget - b.Len()
		if utf8.RuneCountInString(part) > remain {
			part = string([]rune(part)[:remain]) + "…"
		}
		b.WriteString(part)
		b.WriteString("\n")
	}
	if b.Len() == 0 {
		return ""
	}

	// 单次同步调用，压缩失败返回空串 → 上层退化为硬删。
	// ctx 继承调用方取消语义（此前 context.Background() 导致无法随请求取消），
	// 并派生 compress 计量场景（费用口径独立可查）。
	ctx = usage.NewChild(ctx, usage.ScenarioCompress)

	// 提示词模板：mlops 注入的 compress_system active 模板优先，否则内置默认
	prompt, err := renderCompressPrompt(c.promptTemplate(), 80, b.String())
	if err != nil {
		return "" // 坏模板 → 压缩失败 → 上层退化为硬删
	}
	conv := NewConversation(model)
	conv.AddSystemMessage(prompt)
	conv.AddUserMessage("请压缩上面这段对话。")
	resp, err := c.p.ChatCompletion(ctx, conv.ToRequest(nil, false))
	if err != nil || resp == nil || len(resp.Choices) == 0 {
		return ""
	}
	note := strings.TrimSpace(resp.Choices[0].Message.Content)
	if note == "" || note == "[空]" {
		return ""
	}
	return note
}

// compressPromptData 压缩提示词模板变量。
type compressPromptData struct {
	MaxWords int
	Content  string
}

// promptTemplate 当前生效的压缩提示词模板（mlops active 优先，内置默认兜底）。
func (c *LLMCompressor) promptTemplate() string {
	if c.prompts != nil {
		if tpl, ok := c.prompts.ScenarioTemplate(ScenarioCompressSystem); ok {
			return tpl
		}
	}
	return summaryPromptTemplate
}

// renderCompressPrompt 渲染压缩提示词（语法/执行失败返回错误）。
func renderCompressPrompt(tplText string, maxWords int, content string) (string, error) {
	t, err := template.New("compress").Parse(tplText)
	if err != nil {
		return "", fmt.Errorf("parse compress prompt: %w", err)
	}
	var b bytes.Buffer
	if err := t.Execute(&b, compressPromptData{MaxWords: maxWords, Content: content}); err != nil {
		return "", fmt.Errorf("exec compress prompt: %w", err)
	}
	if b.Len() > maxCompressRenderBytes {
		return "", fmt.Errorf("compress prompt output exceeds %d bytes", maxCompressRenderBytes)
	}
	return b.String(), nil
}

// stringifyMessage 把一条消息转为可摘要文本。
func stringifyMessage(m provider.ChatMessage) string {
	switch m.Role {
	case provider.RoleUser:
		return "[用户] " + m.Content
	case provider.RoleAssistant:
		if len(m.ToolCalls) > 0 {
			var calls []string
			for _, tc := range m.ToolCalls {
				calls = append(calls, tc.Name+"("+tc.Arguments+")")
			}
			return "[助手] 调用工具: " + strings.Join(calls, ", ")
		}
		return "[助手] " + m.Content
	case provider.RoleTool:
		return "[工具结果] " + m.Content
	default:
		return m.Content
	}
}
