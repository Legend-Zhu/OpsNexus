package sse

import (
	"fmt"
	"net/http"
)

// Writer SSE 流式写入器
type Writer struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

// NewWriter 创建 SSE 写入器
func NewWriter(w http.ResponseWriter) (*Writer, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, fmt.Errorf("response writer does not support flushing")
	}

	// 设置 SSE 响应头
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // 禁用 nginx 缓冲

	return &Writer{w: w, flusher: flusher}, nil
}

// WriteEvent 写入一个 SSE 事件
// event: 事件类型（可选，空则不写 event 行）
// data: 事件数据
func (s *Writer) WriteEvent(event, data string) error {
	if event != "" {
		if _, err := fmt.Fprintf(s.w, "event: %s\n", event); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(s.w, "data: %s\n\n", data); err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}

// WriteData 写入一个无 event 类型的 SSE 数据
func (s *Writer) WriteData(data string) error {
	return s.WriteEvent("", data)
}

// WriteComment 写入 SSE 注释（用于保持连接活跃）
func (s *Writer) WriteComment(comment string) error {
	if _, err := fmt.Fprintf(s.w, ": %s\n\n", comment); err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}

// Done 写入 [DONE] 标记（OpenAI 格式）
func (s *Writer) Done() error {
	return s.WriteData("[DONE]")
}
