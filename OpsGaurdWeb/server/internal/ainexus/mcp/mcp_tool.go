package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"regexp"
	"strings"
	"unicode/utf8"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/provider"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/tool"

	"github.com/mark3labs/mcp-go/mcp"
)

// MaxToolResultBytes 单次工具调用的结果上限（rune 计）。
// 命令/日志类工具（如 exec_host_command）可能返回海量输出。1M 上下文下
// 允许工具结果全文进入（128KB），仅防单次调用把整个窗口塞爆；超限截断并
// 标注，保留头部（内容）与尾部（报错/摘要）。
const MaxToolResultBytes = 128 << 10 // 128KB

// MCPTool 封装 MCP 远程工具，实现 tool.Tool 接口
type MCPTool struct {
	serverName  string
	toolName    string
	toolDesc    string
	inputSchema mcp.ToolInputSchema
	client      MCPClient
}

// MCPClient 定义 MCP 客户端接口（方便测试和抽象）
type MCPClient interface {
	CallTool(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error)
}

// openAIToolNameMaxLen OpenAI function name 长度上限（规范 ^[a-zA-Z0-9_-]{1,64}$）。
const openAIToolNameMaxLen = 64

// toolNameInvalidChars OpenAI function name 非法字符（连续一段折叠成一个 _）。
var toolNameInvalidChars = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)

// Name 返回工具名称（带 server 前缀避免冲突）。
// 集群 server 名形如 "cluster:prod"，含 ":" 等 OpenAI function name 非法字符，
// 部分 openai_compatible 提供商会直接拒绝整个 tools 定义——统一清洗成合法名，
// 超长则以哈希后缀截断保唯一。原始 server 名仍保留在 Description 中供 LLM 识别集群。
//
// 中文等非 ASCII server 名（如 "cluster:自然灾害集群"）清洗后整段折叠成 "_"，
// 不同集群的工具名会完全同名、注册时互相挤掉——检测到非 ASCII 字符时在
// 尾部追加 server 名指纹（fnv32a 前 8 位 hex）保证跨集群唯一。
func (t *MCPTool) Name() string {
	// 如果工具名已经有 mcp_ 前缀就不重复加
	if len(t.toolName) > 4 && t.toolName[:4] == "mcp_" {
		return sanitizeToolName(t.toolName)
	}
	base := "mcp_" + t.serverName + "_" + t.toolName
	if hasNonASCII(t.serverName) {
		h := fnv.New32a()
		h.Write([]byte(t.serverName))
		base += fmt.Sprintf("_%08x", h.Sum32())
	}
	return sanitizeToolName(base)
}

// hasNonASCII 报告 s 是否含非 ASCII 字符（这些字符会被 sanitizeToolName
// 折叠成 "_"，导致原名的区分信息丢失、不同 server 清洗后同名）。
func hasNonASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] > 127 {
			return true
		}
	}
	return false
}

// sanitizeToolName 清洗成合法 OpenAI function name（[a-zA-Z0-9_-]，≤64）。
// 清洗后只剩 ASCII，按字节截断安全。
func sanitizeToolName(name string) string {
	name = toolNameInvalidChars.ReplaceAllString(name, "_")
	if len(name) <= openAIToolNameMaxLen {
		return name
	}
	h := fnv.New32a()
	h.Write([]byte(name))
	suffix := fmt.Sprintf("_%08x", h.Sum32())
	return name[:openAIToolNameMaxLen-len(suffix)] + suffix
}

// Description 返回工具描述
func (t *MCPTool) Description() string {
	return fmt.Sprintf("[MCP:%s] %s", t.serverName, t.toolDesc)
}

// ToolScopeFilter 返回会话级集群工具过滤器：保留内置工具（描述无 [MCP:]
// 前缀）与指定 server 的工具，其余集群的工具不下发给模型——多集群工具
// 全部在册的前提下，把模型可见面收敛到目标集群，杜绝跨集群误调用。
// serverName 形如 "cluster:自然灾害集群"（MCP server 原始名）。
func ToolScopeFilter(serverName string) func(provider.ToolDefinition) bool {
	prefix := "[MCP:" + serverName + "]"
	return func(td provider.ToolDefinition) bool {
		if !strings.HasPrefix(td.Description, "[MCP:") {
			return true
		}
		return strings.HasPrefix(td.Description, prefix)
	}
}

// Parameters 返回工具参数 JSON Schema
func (t *MCPTool) Parameters() map[string]any {
	params := make(map[string]any)
	if t.inputSchema.Type != "" {
		b, err := json.Marshal(t.inputSchema)
		if err != nil {
			return params
		}
		json.Unmarshal(b, &params)
	}
	return params
}

// Execute 通过 MCP 协议调用远程工具
func (t *MCPTool) Execute(ctx context.Context, params map[string]any) (tool.ToolResult, error) {
	// 构建 MCP 调用请求
	callReq := mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      t.toolName, // 使用原始工具名（不含前缀）
			Arguments: params,
		},
	}

	result, err := t.client.CallTool(ctx, callReq)
	if err != nil {
		return tool.NewErrorResult(fmt.Sprintf("MCP tool call failed: %s", err)), nil
	}

	// 将结果内容转换为字符串（上限 MaxToolResultBytes，1M 上下文下全文进
	// 上下文；超限保留头尾、截断中部，并标注）
	var contentStr string
	for _, c := range result.Content {
		var s string
		switch v := c.(type) {
		case mcp.TextContent:
			s = v.Text
		default:
			b, _ := json.Marshal(v)
			s = string(b)
		}
		remain := MaxToolResultBytes - utf8.RuneCountInString(contentStr)
		if remain <= 0 {
			contentStr += "\n…[more output truncated]"
			break
		}
		rs := []rune(s)
		if len(rs) > remain {
			// 保留头 + 尾，中部省略
			head := rs[:remain/2]
			tail := rs[len(rs)-remain/2:]
			contentStr += string(head) + fmt.Sprintf("\n…[中间省略 %d 字符]\n", len(rs)-remain) + string(tail)
			break
		}
		contentStr += s
	}

	if result.IsError {
		return tool.NewErrorResult(contentStr), nil
	}

	return tool.NewTextResult(contentStr), nil
}
