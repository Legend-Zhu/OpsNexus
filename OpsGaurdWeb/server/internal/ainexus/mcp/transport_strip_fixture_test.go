package mcp

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// TestRewriteSSEAgainstRealWorkerResponse 用真实 Worker 的 tools/list SSE
// 响应字节做回归：原始 data 载荷必须让 mark3labs 客户端解码失败（复现线上
// 问题），经 rewriteSSEDataLines 改写后必须解码成功且工具清单完整。
// 需要 fixture：MCP_SSE_FIXTURE 指向响应文件（缺省跳过，fixture 不入库）。
func TestRewriteSSEAgainstRealWorkerResponse(t *testing.T) {
	path := os.Getenv("MCP_SSE_FIXTURE")
	if path == "" {
		t.Skip("set MCP_SSE_FIXTURE to a real worker tools/list SSE response to run")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	// 负向对照：原始载荷解码必须失败（复现 outputSchema 数组 type 问题）。
	// 注意：data 行是完整 JSON-RPC 消息，工具清单在 result 里。
	type jsonrpcEnvelope struct {
		Result mcp.ListToolsResult `json:"result"`
	}
	var decodeErr error
	for _, line := range bytes.Split(body, []byte("\n")) {
		payload, ok := bytes.CutPrefix(line, []byte("data:"))
		if !ok || !bytes.Contains(payload, []byte("outputSchema")) {
			continue
		}
		var env jsonrpcEnvelope
		if decodeErr = json.Unmarshal(payload, &env); decodeErr == nil {
			t.Fatalf("fixture did not reproduce the bug: original payload decoded fine")
		}
		break
	}
	if decodeErr == nil {
		t.Fatal("fixture has no data line with outputSchema — not a tools/list response")
	}
	t.Logf("original payload decode error (expected): %v", decodeErr)

	// 改写后：无 outputSchema，mark3labs 可解码且工具清单完整
	rewritten := rewriteSSEDataLines(body)
	if bytes.Contains(rewritten, []byte("outputSchema")) {
		t.Fatal("rewritten body still contains outputSchema")
	}
	decoded := 0
	for _, line := range bytes.Split(rewritten, []byte("\n")) {
		payload, ok := bytes.CutPrefix(line, []byte("data:"))
		if !ok || !bytes.Contains(payload, []byte("outputSchema")) && !bytes.Contains(payload, []byte("\"tools\"")) {
			continue
		}
		var env jsonrpcEnvelope
		if err := json.Unmarshal(payload, &env); err != nil {
			t.Fatalf("rewritten payload decode failed: %v", err)
		}
		if len(env.Result.Tools) > 0 {
			decoded = len(env.Result.Tools)
			for _, tl := range env.Result.Tools {
				if tl.InputSchema.Type == "" {
					t.Fatalf("tool %q lost inputSchema", tl.Name)
				}
			}
		}
	}
	if decoded == 0 {
		t.Fatal("no tools decoded from rewritten fixture")
	}
	t.Logf("decoded %d tools from rewritten SSE fixture", decoded)
}
