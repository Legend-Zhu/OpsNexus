package agent

import (
	"strings"
	"testing"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/provider"
)

// TestTrimKeepsAtomicToolRounds 裁剪按完整轮次删除：不产生孤立 tool 消息，
// 保留最近 keepRounds 个工具轮次 + 最新用户消息 + system。
func TestTrimKeepsAtomicToolRounds(t *testing.T) {
	conv := NewConversation("m")
	conv.AddSystemMessage("sys")
	conv.AddUserMessage("排查 web")
	// 5 轮工具调用，每轮 assistant(tool_calls)+2 条 tool 结果
	for r := 0; r < 5; r++ {
		conv.AddAssistantToolCalls([]provider.ToolCall{
			{ID: "c", Name: "exec", Arguments: `{"cmd":"probe"}`},
		}, "")
		conv.AddToolResult("c", strings.Repeat("x", 2000))
		conv.AddToolResult("c", strings.Repeat("y", 2000))
	}
	conv.AddUserMessage("继续排查")

	// 预算设为刚好容纳 ~2 轮 → 应删掉最旧的 3 轮，保留最近 2 轮 + 末尾 user + system
	conv.Trim(6000, 2)

	// 校验：无孤立 tool 消息（tool 必须处于 assistant(tool_calls) 起始的连续块内，
	// 一轮可有多个 tool 结果）
	inToolBlock := false
	for _, m := range conv.Messages {
		if m.Role == provider.RoleAssistant && len(m.ToolCalls) > 0 {
			inToolBlock = true
			continue
		}
		if m.Role == provider.RoleTool {
			if !inToolBlock {
				t.Fatalf("orphan tool message: %+v", m)
			}
			continue
		}
		inToolBlock = false
	}
	// system 保留
	if conv.Messages[0].Role != provider.RoleSystem {
		t.Fatal("system message must be preserved")
	}
	// 末尾用户消息保留
	last := conv.Messages[len(conv.Messages)-1]
	if last.Role != provider.RoleUser || last.Content != "继续排查" {
		t.Fatalf("last user message lost: %+v", last)
	}
	// 保留的工具轮次数 = 2（每轮 assistant+2 tool）
	rounds := 0
	for i := 0; i < len(conv.Messages); {
		if conv.Messages[i].Role == provider.RoleAssistant && len(conv.Messages[i].ToolCalls) > 0 {
			rounds++
			i += 3 // assistant + 2 tool
		} else {
			i++
		}
	}
	if rounds != 2 {
		t.Fatalf("expected 2 tool rounds kept, got %d", rounds)
	}
	// 预算内
	if conv.TokenCount() > 6000 {
		t.Fatalf("still over budget: %d", conv.TokenCount())
	}
}

// TestTrimWithinBudgetNoop 未超预算不裁剪。
func TestTrimWithinBudgetNoop(t *testing.T) {
	conv := NewConversation("m")
	conv.AddSystemMessage("sys")
	conv.AddUserMessage("hi")
	before := len(conv.Messages)
	conv.Trim(100000, 3)
	if len(conv.Messages) != before {
		t.Fatalf("should not trim when within budget")
	}
}

// TestTrimFallbackTruncate 保底区间仍超预算 → 截断最后一条消息内容。
func TestTrimFallbackTruncate(t *testing.T) {
	conv := NewConversation("m")
	conv.AddSystemMessage("sys")
	conv.AddUserMessage(strings.Repeat("z", 10000)) // 单条巨大

	conv.Trim(2000, 3)
	if conv.TokenCount() > 2000 {
		t.Fatalf("still over budget after fallback: %d", conv.TokenCount())
	}
	// system 必须仍在
	if conv.Messages[0].Role != provider.RoleSystem {
		t.Fatal("system lost in fallback")
	}
}

// TestTrimAllSystem 全 system（极端）不 panic。
func TestTrimAllSystem(t *testing.T) {
	conv := NewConversation("m")
	conv.AddSystemMessage("only")
	conv.Trim(10, 3)
	if len(conv.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(conv.Messages))
	}
}

// TestTokenCount 计数覆盖内容 + 工具参数。
func TestTokenCount(t *testing.T) {
	conv := NewConversation("m")
	conv.AddSystemMessage(strings.Repeat("a", 100))
	conv.AddAssistantToolCalls([]provider.ToolCall{{Arguments: strings.Repeat("b", 200)}}, "")
	// 100/2+4 + (200/2+4) ≈ 158
	if conv.TokenCount() < 100 {
		t.Fatalf("token count too low: %d", conv.TokenCount())
	}
}

// fakeCompressor 记录被压缩的轮次并返回固定纪要。
type fakeCompressor struct {
	calls int
}

func (f *fakeCompressor) Compress(msgs []provider.ChatMessage) string {
	f.calls++
	return "root cause: port conflict; commands: probe; observations: cpu 95%"
}

// TestCompressSummarisesOldRounds 摘要压缩：旧轮次被替换为一条 [历史摘要]，
// 保底区（最近工具轮次 + 末尾用户消息 + system）完整保留，无孤立 tool。
func TestCompressSummarisesOldRounds(t *testing.T) {
	conv := NewConversation("m")
	conv.AddSystemMessage("sys")
	conv.AddUserMessage("排查 web")
	for r := 0; r < 5; r++ {
		conv.AddAssistantToolCalls([]provider.ToolCall{{ID: "c", Name: "exec", Arguments: `{"cmd":"probe"}`}}, "")
		conv.AddToolResult("c", strings.Repeat("x", 2000))
		conv.AddToolResult("c", strings.Repeat("y", 2000))
	}
	conv.AddUserMessage("继续排查")

	fc := &fakeCompressor{}
	conv.Compress(fc, 6000, 2)

	// 摘要被调用（有旧轮次可压缩）
	if fc.calls == 0 {
		t.Fatal("compressor not called")
	}
	// 摘要消息出现
	hasSummary := false
	for _, m := range conv.Messages {
		if strings.Contains(m.Content, "[历史摘要]") {
			hasSummary = true
		}
	}
	if !hasSummary {
		t.Fatalf("summary message missing: %+v", conv.Messages)
	}
	// 保底：system 保留、末尾用户消息保留、最近 2 个工具轮次完整保留
	if conv.Messages[0].Role != provider.RoleSystem {
		t.Fatal("system must be preserved")
	}
	last := conv.Messages[len(conv.Messages)-1]
	if last.Role != provider.RoleUser || last.Content != "继续排查" {
		t.Fatalf("last user lost: %+v", last)
	}
	// 无孤立 tool
	inToolBlock := false
	for _, m := range conv.Messages {
		if m.Role == provider.RoleAssistant && len(m.ToolCalls) > 0 {
			inToolBlock = true
			continue
		}
		if m.Role == provider.RoleTool {
			if !inToolBlock {
				t.Fatalf("orphan tool after compress: %+v", m)
			}
			continue
		}
		inToolBlock = false
	}
	// 预算内
	if conv.TokenCount() > 6000 {
		t.Fatalf("over budget after compress: %d", conv.TokenCount())
	}
}

// TestCompressFallbackWhenCompressorFails 摘要失败（返回空串）→ 退化为硬删，
// 仍守预算且无孤立 tool。
func TestCompressFallbackWhenCompressorFails(t *testing.T) {
	conv := NewConversation("m")
	conv.AddSystemMessage("sys")
	conv.AddUserMessage("排查")
	for r := 0; r < 4; r++ {
		conv.AddAssistantToolCalls([]provider.ToolCall{{ID: "c", Name: "exec", Arguments: `{}`}}, "")
		conv.AddToolResult("c", strings.Repeat("z", 1500))
	}
	conv.AddUserMessage("继续")

	// 压缩器总是失败
	failing := &fakeCompressorFail{}
	conv.Compress(failing, 3000, 1)

	if conv.TokenCount() > 3000 {
		t.Fatalf("over budget after fallback: %d", conv.TokenCount())
	}
	if conv.Messages[0].Role != provider.RoleSystem {
		t.Fatal("system lost")
	}
}

type fakeCompressorFail struct{}

func (f *fakeCompressorFail) Compress(_ []provider.ChatMessage) string { return "" }

// TestCompressNilCompressor 无压缩器 → 硬删（等价旧 Trim）。
func TestCompressNilCompressor(t *testing.T) {
	conv := NewConversation("m")
	conv.AddSystemMessage("sys")
	conv.AddUserMessage("排查")
	for r := 0; r < 4; r++ {
		conv.AddAssistantToolCalls([]provider.ToolCall{{ID: "c", Name: "exec", Arguments: `{}`}}, "")
		conv.AddToolResult("c", strings.Repeat("q", 1500))
	}
	conv.AddUserMessage("继续")

	conv.Compress(nil, 3000, 1)
	if conv.TokenCount() > 3000 {
		t.Fatalf("over budget: %d", conv.TokenCount())
	}
	if conv.Messages[0].Role != provider.RoleSystem {
		t.Fatal("system lost")
	}
}
