package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/config"
)

// HTTPRequestTool HTTP 请求工具
type HTTPRequestTool struct {
	timeout time.Duration
	client  *http.Client
}

// NewHTTPRequestTool 创建 HTTP 请求工具
func NewHTTPRequestTool(cfg config.HTTPRequestToolConfig) *HTTPRequestTool {
	return &HTTPRequestTool{
		timeout: cfg.Timeout,
		client: &http.Client{
			Timeout: cfg.Timeout,
		},
	}
}

func (t *HTTPRequestTool) Name() string {
	return "http_request"
}

func (t *HTTPRequestTool) Description() string {
	return "Send an HTTP request to a specified URL. Supports GET, POST, PUT, DELETE, PATCH methods with custom headers and body."
}

func (t *HTTPRequestTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"url": map[string]any{
				"type":        "string",
				"description": "The URL to send the request to",
			},
			"method": map[string]any{
				"type":        "string",
				"description": "HTTP method (GET, POST, PUT, DELETE, PATCH)",
				"enum":        []string{"GET", "POST", "PUT", "DELETE", "PATCH"},
				"default":     "GET",
			},
			"headers": map[string]any{
				"type":        "object",
				"description": "HTTP headers as key-value pairs",
				"additionalProperties": map[string]any{
					"type": "string",
				},
			},
			"body": map[string]any{
				"type":        "string",
				"description": "Request body (for POST, PUT, PATCH)",
			},
			"timeout": map[string]any{
				"type":        "number",
				"description": "Timeout in seconds (optional, overrides default)",
			},
		},
		"required": []string{"url"},
	}
}

func (t *HTTPRequestTool) Execute(ctx context.Context, params map[string]any) (ToolResult, error) {
	url, ok := params["url"].(string)
	if !ok || url == "" {
		return NewErrorResult("parameter 'url' is required and must be a string"), nil
	}

	method := "GET"
	if m, ok := params["method"].(string); ok && m != "" {
		method = strings.ToUpper(m)
	}

	// 构建请求体
	var body io.Reader
	if bodyStr, ok := params["body"].(string); ok && bodyStr != "" {
		body = bytes.NewReader([]byte(bodyStr))
	}

	// 创建请求
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return NewErrorResult(fmt.Sprintf("failed to create request: %s", err)), nil
	}

	// 设置请求头
	if headers, ok := params["headers"].(map[string]any); ok {
		for k, v := range headers {
			if vs, ok := v.(string); ok {
				req.Header.Set(k, vs)
			}
		}
	}

	// 如果有 body 且没设 Content-Type，默认设为 application/json
	if body != nil && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}

	// 自定义超时
	timeout := t.timeout
	if t, ok := params["timeout"].(float64); ok && t > 0 {
		timeout = time.Duration(t) * time.Second
	}
	t.client.Timeout = timeout

	// 发送请求
	resp, err := t.client.Do(req)
	if err != nil {
		return NewErrorResult(fmt.Sprintf("request failed: %s", err)), nil
	}
	defer resp.Body.Close()

	// 读取响应体（限制大小 1MB）
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
	if err != nil {
		return NewErrorResult(fmt.Sprintf("failed to read response: %s", err)), nil
	}

	// 格式化结果
	result := fmt.Sprintf("HTTP %s %s\n", method, url)
	result += fmt.Sprintf("Status: %d %s\n", resp.StatusCode, resp.Status)

	// 响应头
	result += "Response Headers:\n"
	for k, v := range resp.Header {
		result += fmt.Sprintf("  %s: %s\n", k, strings.Join(v, ", "))
	}

	// 响应体
	result += fmt.Sprintf("\nResponse Body:\n%s", string(respBody))

	// 尝试美化 JSON
	var prettyJSON bytes.Buffer
	if json.Indent(&prettyJSON, respBody, "", "  ") == nil {
		result = fmt.Sprintf("HTTP %s %s\n", method, url)
		result += fmt.Sprintf("Status: %d %s\n\n", resp.StatusCode, resp.Status)
		result += prettyJSON.String()
	}

	return NewTextResult(result), nil
}
