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
