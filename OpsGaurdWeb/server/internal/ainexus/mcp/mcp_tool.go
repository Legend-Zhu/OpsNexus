package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"unicode/utf8"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/tool"

	"github.com/mark3labs/mcp-go/mcp"
)

// MaxToolResultBytes 单次工具调用的结果上限（rune 计）。
// 命令/日志类工具（如 exec_host_command）可能返回海量输出，超出即截断并
// 标注，避免一次工具结果打爆上下文（防长排查失控的核心防线）。
const MaxToolResultBytes = 8000

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

// Name 返回工具名称（带 server 前缀避免冲突）
func (t *MCPTool) Name() string {
	// 如果工具名已经有 mcp_ 前缀就不重复加
	if len(t.toolName) > 4 && t.toolName[:4] == "mcp_" {
		return t.toolName
	}
	return "mcp_" + t.serverName + "_" + t.toolName
}

// Description 返回工具描述
func (t *MCPTool) Description() string {
	return fmt.Sprintf("[MCP:%s] %s", t.serverName, t.toolDesc)
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

	// 将结果内容转换为字符串（截断到 MaxToolResultBytes，防上下文打爆）
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
			contentStr += string(rs[:remain]) + "\n…[output truncated]"
			break
		}
		contentStr += s
	}

	if result.IsError {
		return tool.NewErrorResult(contentStr), nil
	}

	return tool.NewTextResult(contentStr), nil
}
