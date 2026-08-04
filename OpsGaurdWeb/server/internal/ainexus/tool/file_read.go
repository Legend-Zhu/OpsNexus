package tool

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/config"
)

// FileReadTool 文件读取工具
type FileReadTool struct {
	maxSize int64
}

// NewFileReadTool 创建文件读取工具
func NewFileReadTool(cfg config.FileReadToolConfig) *FileReadTool {
	maxSize := cfg.MaxSize
	if maxSize <= 0 {
		maxSize = 1048576 // 1MB
	}
	return &FileReadTool{maxSize: maxSize}
}

func (t *FileReadTool) Name() string {
	return "file_read"
}

func (t *FileReadTool) Description() string {
	return "Read the contents of a file from the local filesystem. Supports reading specific line ranges."
}

func (t *FileReadTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "The path to the file to read",
			},
			"offset": map[string]any{
				"type":        "number",
				"description": "Line number to start reading from (1-based, optional)",
			},
			"limit": map[string]any{
				"type":        "number",
				"description": "Maximum number of lines to read (optional)",
			},
		},
		"required": []string{"path"},
	}
}

func (t *FileReadTool) Execute(ctx context.Context, params map[string]any) (ToolResult, error) {
	path, ok := params["path"].(string)
	if !ok || path == "" {
		return NewErrorResult("parameter 'path' is required and must be a string"), nil
	}

	// 检查文件是否存在
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return NewErrorResult(fmt.Sprintf("file not found: %s", path)), nil
		}
		return NewErrorResult(fmt.Sprintf("cannot access file: %s", err)), nil
	}

	// 检查是否是目录
	if info.IsDir() {
		return NewErrorResult(fmt.Sprintf("path is a directory, not a file: %s", path)), nil
	}

	// 检查文件大小
	if info.Size() > t.maxSize {
		return NewErrorResult(fmt.Sprintf("file too large: %d bytes (max %d bytes)", info.Size(), t.maxSize)), nil
	}

	// 读取文件
	f, err := os.Open(path)
	if err != nil {
		return NewErrorResult(fmt.Sprintf("cannot open file: %s", err)), nil
	}
	defer f.Close()

	content, err := io.ReadAll(f)
	if err != nil {
		return NewErrorResult(fmt.Sprintf("cannot read file: %s", err)), nil
	}

	// 如果指定了行范围
	offset, _ := params["offset"].(float64)
	limit, _ := params["limit"].(float64)

	if offset > 0 || limit > 0 {
		lines := strings.Split(string(content), "\n")
		start := 0
		if offset > 0 {
			start = int(offset) - 1 // 转为 0-based
			if start >= len(lines) {
				return NewErrorResult(fmt.Sprintf("offset %d exceeds file line count %d", int(offset), len(lines))), nil
			}
		}
		end := len(lines)
		if limit > 0 {
			calculated := start + int(limit)
			if calculated < end {
				end = calculated
			}
		}
		content = []byte(strings.Join(lines[start:end], "\n"))
	}

	result := fmt.Sprintf("File: %s (%d bytes)\n\n%s", path, info.Size(), string(content))
	return NewTextResult(result), nil
}
