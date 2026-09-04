// MCP 客户端传输层修补：剥除 tools/list 响应中的 outputSchema。
//
// 背景：管理端 MCP 客户端（mark3labs/mcp-go）把 Tool.outputSchema 解析为
// `Type string` 的严格 schema 结构，而 Worker 侧官方 go-sdk（v1.7.0）为
// 类型化工具自动生成的 outputSchema 中，可空字段会产生数组形 "type"
// （如 ["array","null"]）——只要有一个工具解析失败，整个 tools/list 响应
// 就解码失败，该集群的全部工具对 Agent 不可见（健康探测因此持续报错）。
// outputSchema 对客户端只是提示性元数据，剥除后不影响工具调用与结果解析。
package mcp

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// schemaStrippingTransport 包装底层 RoundTripper，对 application/json 响应
// 剥除 outputSchema 成员（其余响应原样透传，SSE 不受影响）。
type schemaStrippingTransport struct {
	base http.RoundTripper
}

func (t schemaStrippingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil || resp.StatusCode != http.StatusOK {
		return resp, err
	}
	ct := resp.Header.Get("Content-Type")
	switch {
	case strings.Contains(ct, "application/json"):
		body, err := readMaybeGzipped(resp)
		if err != nil {
			return resp, nil // 读取失败按原样放行，由上层按原错误处理
		}
		if !bytes.Contains(body, []byte("outputSchema")) {
			writeBack(resp, body)
			return resp, nil
		}
		var v any
		if json.Unmarshal(body, &v) != nil {
			writeBack(resp, body)
			return resp, nil
		}
		changed := false
		stripOutputSchema(v, &changed)
		if !changed {
			writeBack(resp, body)
			return resp, nil
		}
		cleaned, err := json.Marshal(v)
		if err != nil {
			writeBack(resp, body)
			return resp, nil
		}
		writeBack(resp, cleaned)
		return resp, nil

	case strings.Contains(ct, "text/event-stream") && req.Method == http.MethodPost:
		// Worker go-sdk 默认对 POST 请求也回 SSE（useSSE=!jsonResponse），
		// 且为请求级无状态会话：响应写完即关闭，可整体缓冲改写。
		// 逐行处理 data: 载荷，保持事件帧结构（行数/空行分隔）不变；
		// GET 长流（服务端通知订阅）不经过此分支，原样透传。
		body, err := readMaybeGzipped(resp)
		if err != nil {
			return resp, nil
		}
		if !bytes.Contains(body, []byte("outputSchema")) {
			writeBack(resp, body)
			return resp, nil
		}
		writeBack(resp, rewriteSSEDataLines(body))
		return resp, nil
	}
	return resp, nil
}

// rewriteSSEDataLines 对 SSE 文本中每个 data: 行的 JSON 载荷剥除
// outputSchema；非 JSON 行原样保留。
func rewriteSSEDataLines(body []byte) []byte {
	lines := bytes.Split(body, []byte("\n"))
	for i, line := range lines {
		payload, ok := bytes.CutPrefix(line, []byte("data:"))
		if !ok || !bytes.Contains(payload, []byte("outputSchema")) {
			continue
		}
		var v any
		if json.Unmarshal(payload, &v) != nil {
			continue
		}
		changed := false
		stripOutputSchema(v, &changed)
		if !changed {
			continue
		}
		if cleaned, err := json.Marshal(v); err == nil {
			lines[i] = append([]byte("data:"), cleaned...)
		}
	}
	return bytes.Join(lines, []byte("\n"))
}

// readMaybeGzipped 读取响应体（Go Transport 对自行声明的 gzip 会透明解压，
// 服务器直接强加的 Content-Encoding: gzip 则需手动处理，双保险）。
func readMaybeGzipped(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.Header.Get("Content-Encoding") == "gzip" {
		zr, zerr := gzip.NewReader(bytes.NewReader(body))
		if zerr != nil {
			return body, nil
		}
		defer zr.Close()
		return io.ReadAll(zr)
	}
	return body, nil
}

// writeBack 用（可能改写过的）body 替换响应体并修正长度头。
func writeBack(resp *http.Response, body []byte) {
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	resp.Header.Del("Content-Encoding")
}

// stripOutputSchema 递归删除 JSON 树中所有 outputSchema 键。
func stripOutputSchema(v any, changed *bool) {
	switch t := v.(type) {
	case map[string]any:
		if _, ok := t["outputSchema"]; ok {
			delete(t, "outputSchema")
			*changed = true
		}
		for _, child := range t {
			stripOutputSchema(child, changed)
		}
	case []any:
		for _, child := range t {
			stripOutputSchema(child, changed)
		}
	}
}
