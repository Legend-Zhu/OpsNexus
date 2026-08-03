package tool

import "context"

// Tool 工具接口
type Tool interface {
	// Name 工具名称
	Name() string
	// Description 工具描述
	Description() string
	// Parameters 工具参数 JSON Schema
	Parameters() map[string]any
	// Execute 执行工具
	Execute(ctx context.Context, params map[string]any) (ToolResult, error)
}

// ToolResult 工具执行结果
type ToolResult struct {
	Content string `json:"content"`
	IsError bool   `json:"is_error"`
}

// NewTextResult 创建文本结果
func NewTextResult(content string) ToolResult {
	return ToolResult{Content: content, IsError: false}
}

// NewErrorResult 创建错误结果
func NewErrorResult(errMsg string) ToolResult {
	return ToolResult{Content: errMsg, IsError: true}
}
