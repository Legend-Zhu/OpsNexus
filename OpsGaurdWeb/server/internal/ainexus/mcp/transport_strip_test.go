package mcp

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTS 起一个本地 HTTP 测试上游。
func newTS(h http.Handler) *httptest.Server {
	return httptest.NewServer(h)
}

func stripTransportResponse(t *testing.T, body string, contentType string) (*http.Response, string) {
	t.Helper()
	base := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		_, _ = io.WriteString(w, body)
	})
	ts := newTS(base)
	defer ts.Close()
	resp, err := (&http.Client{Transport: schemaStrippingTransport{base: http.DefaultTransport}}).
		Get(ts.URL)
	if err != nil {
		t.Fatalf("roundtrip: %v", err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	return resp, string(got)
}

func TestSchemaStrippingTransportStripsOutputSchema(t *testing.T) {
	body := `{"jsonrpc":"2.0","result":{"tools":[
		{"name":"get_service","description":"d","inputSchema":{"type":"object"},
		 "outputSchema":{"type":["array","null"],"items":{"type":"object"}}},
		{"name":"list_services","description":"d2","inputSchema":{"type":"object"}}]}}`
	resp, got := stripTransportResponse(t, body, "application/json")
	if strings.Contains(got, "outputSchema") {
		t.Fatalf("outputSchema not stripped:\n%s", got)
	}
	if !strings.Contains(got, `"list_services"`) {
		t.Fatalf("tool entries must survive:\n%s", got)
	}
	if resp.ContentLength != int64(len(got)) {
		t.Errorf("Content-Length = %d, body len = %d", resp.ContentLength, len(got))
	}
}

func TestSchemaStrippingTransportPassthrough(t *testing.T) {
	// 无 outputSchema 的 JSON：原样透传（字节不变）
	plain := `{"jsonrpc":"2.0","result":{"tools":[{"name":"a","inputSchema":{"type":"object"}}]}}`
	_, got := stripTransportResponse(t, plain, "application/json")
	if got != plain {
		t.Fatalf("plain JSON must be unchanged:\n%s", got)
	}

	// GET 长流（服务端通知订阅，text/event-stream）不重写
	sse := "event: message\ndata: {\"outputSchema\":{\"type\":\"object\"}}\n\n"
	_, gotSSE := stripTransportResponse(t, sse, "text/event-stream")
	if !strings.Contains(gotSSE, "outputSchema") {
		t.Fatalf("GET SSE stream must pass through untouched:\n%s", gotSSE)
	}
}

// TestSchemaStrippingTransportRewritesSSEOnPost go-sdk 默认对 POST 也回
// SSE（tools/list 亦然）：data: 行内的 outputSchema 必须被剥除，且事件帧
// 结构（行数与空行分隔）保持不变。
func TestSchemaStrippingTransportRewritesSSEOnPost(t *testing.T) {
	body := "event: message\n" +
		"data: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"tools\":[{\"name\":\"get_service\",\"inputSchema\":{\"type\":\"object\"},\"outputSchema\":{\"type\":[\"array\",\"null\"]}}]}}\n" +
		"\n" +
		"data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/message\"}\n" +
		"\n"
	base := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, body)
	})
	ts := newTS(base)
	defer ts.Close()

	resp, err := (&http.Client{Transport: schemaStrippingTransport{base: http.DefaultTransport}}).
		Post(ts.URL, "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("roundtrip: %v", err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	gotStr := string(got)

	if strings.Contains(gotStr, "outputSchema") {
		t.Fatalf("SSE data line not stripped:\n%s", gotStr)
	}
	if !strings.Contains(gotStr, `"name":"get_service"`) || !strings.Contains(gotStr, "notifications/message") {
		t.Fatalf("other message content must survive:\n%s", gotStr)
	}
	if want := len(bytes.Split([]byte(body), []byte("\n"))); len(bytes.Split(got, []byte("\n"))) != want {
		t.Errorf("SSE line structure changed: %d lines, want %d", len(bytes.Split(got, []byte("\n"))), want)
	}
}

func TestSchemaStrippingTransportGzip(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write([]byte(`{"tools":[{"name":"a","outputSchema":{"type":["array","null"]}}]}`))
	_ = zw.Close()

	base := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(buf.Bytes())
	})
	ts := newTS(base)
	defer ts.Close()
	resp, err := (&http.Client{Transport: schemaStrippingTransport{base: http.DefaultTransport}}).Get(ts.URL)
	if err != nil {
		t.Fatalf("roundtrip: %v", err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(got), "outputSchema") {
		t.Fatalf("gzipped response not stripped:\n%s", got)
	}
	if resp.Header.Get("Content-Encoding") != "" {
		t.Errorf("Content-Encoding should be cleared after rewrite")
	}
}
