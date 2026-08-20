package handler

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/agent"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/config"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/provider"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/tool"
)

// AnthropicHandler Anthropic 格式接口处理器
type AnthropicHandler struct {
	modelRoutes map[string]provider.Provider
	registry    *tool.Registry
	config      config.Config
	logger      *log.Logger
	promptSrc   agent.ScenarioTemplateSource
}

// NewAnthropicHandler 创建 Anthropic 格式处理器。promptSrc 为提示词场景
// 模板来源（nil = 代码内置默认）。
func NewAnthropicHandler(modelRoutes map[string]provider.Provider, registry *tool.Registry, cfg config.Config, logger *log.Logger, promptSrc agent.ScenarioTemplateSource) *AnthropicHandler {
	return &AnthropicHandler{
		modelRoutes: modelRoutes,
		registry:    registry,
		config:      cfg,
		logger:      logger,
		promptSrc:   promptSrc,
	}
}

type anthropicRequest struct {
	Model     string   `json:"model"`
	MaxTokens int      `json:"max_tokens"`
	Messages  []antMsg `json:"messages"`
	System    string   `json:"system,omitempty"`
	Stream    bool     `json:"stream"`
	Tools     []any    `json:"tools,omitempty"`
}

type antMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Messages 处理 /v1/messages 请求
func (h *AnthropicHandler) Messages(c *gin.Context) {
	var req anthropicRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"type": "error", "error": gin.H{"type": "invalid_request_error", "message": err.Error()},
		})
		return
	}

	p, ok := h.modelRoutes[req.Model]
	if !ok {
		available := make([]string, 0, len(h.modelRoutes))
		for m := range h.modelRoutes {
			available = append(available, m)
		}
		c.JSON(http.StatusBadRequest, gin.H{
			"type": "error",
			"error": gin.H{
				"type":    "invalid_request_error",
				"message": fmt.Sprintf("model %q not found. Available models: %v", req.Model, available),
			},
		})
		return
	}

	ag := agent.New(p, h.registry, h.config.Agent, h.logger, h.promptSrc)

	conv := agent.NewConversation(req.Model)
	if req.System != "" {
		conv.AddSystemMessage(req.System)
	}
	for _, msg := range req.Messages {
		switch msg.Role {
		case "user":
			conv.AddUserMessage(msg.Content)
		case "assistant":
			conv.AddAssistantMessage(msg.Content)
		}
	}

	if req.Stream {
		h.handleStream(c, ag, conv, req.Model)
	} else {
		h.handleNonStream(c, ag, conv, req.Model, req.MaxTokens)
	}
}

// handleStream 处理流式请求（Anthropic SSE 格式）
func (h *AnthropicHandler) handleStream(c *gin.Context, ag *agent.Agent, conv *agent.Conversation, model string) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	msgID := fmt.Sprintf("msg_%d", time.Now().UnixNano())

	// message_start
	startEvent := map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": msgID, "type": "message", "role": "assistant",
			"model": model, "content": []any{}, "status": "in_progress",
		},
	}
	data, _ := json.Marshal(startEvent)
	fmt.Fprintf(c.Writer, "event: message_start\ndata: %s\n\n", data)
	c.Writer.(http.Flusher).Flush()

	contentIndex := 0

	// content_block_start (text)
	textBlockStart := map[string]any{
		"type": "content_block_start", "index": contentIndex,
		"content_block": map[string]any{"type": "text", "text": ""},
	}
	data, _ = json.Marshal(textBlockStart)
	fmt.Fprintf(c.Writer, "event: content_block_start\ndata: %s\n\n", data)
	c.Writer.(http.Flusher).Flush()

	eventCh, err := ag.RunStream(c.Request.Context(), conv)
	if err != nil {
		errBlock := map[string]any{"type": "error", "error": map[string]any{"type": "server_error", "message": err.Error()}}
		data, _ := json.Marshal(errBlock)
		fmt.Fprintf(c.Writer, "event: error\ndata: %s\n\n", data)
		c.Writer.(http.Flusher).Flush()
		return
	}

	var outputTokens int

	for event := range eventCh {
		switch event.Type {
		case agent.AgentEventText:
			deltaEvent := map[string]any{
				"type": "content_block_delta", "index": contentIndex,
				"delta": map[string]any{"type": "text_delta", "text": event.Content},
			}
			data, _ := json.Marshal(deltaEvent)
			fmt.Fprintf(c.Writer, "event: content_block_delta\ndata: %s\n\n", data)
			c.Writer.(http.Flusher).Flush()
			outputTokens++

		case agent.AgentEventToolStart:
			// 关闭当前文本块
			stopEvent := map[string]any{"type": "content_block_stop", "index": contentIndex}
			data, _ := json.Marshal(stopEvent)
			fmt.Fprintf(c.Writer, "event: content_block_stop\ndata: %s\n\n", data)
			c.Writer.(http.Flusher).Flush()

			// 新增 tool_use 块
			contentIndex++
			toolStartEvent := map[string]any{
				"type": "content_block_start", "index": contentIndex,
				"content_block": map[string]any{
					"type": "tool_use", "id": event.ToolCall.ID, "name": event.ToolCall.Name, "input": map[string]any{},
				},
			}
			data, _ = json.Marshal(toolStartEvent)
			fmt.Fprintf(c.Writer, "event: content_block_start\ndata: %s\n\n", data)
			c.Writer.(http.Flusher).Flush()

		case agent.AgentEventToolEnd:
			deltaEvent := map[string]any{
				"type": "content_block_delta", "index": contentIndex,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": event.ToolCall.Arguments},
			}
			data, _ := json.Marshal(deltaEvent)
			fmt.Fprintf(c.Writer, "event: content_block_delta\ndata: %s\n\n", data)
			c.Writer.(http.Flusher).Flush()

			// 关闭 tool_use 块
			stopEvent := map[string]any{"type": "content_block_stop", "index": contentIndex}
			data, _ = json.Marshal(stopEvent)
			fmt.Fprintf(c.Writer, "event: content_block_stop\ndata: %s\n\n", data)
			c.Writer.(http.Flusher).Flush()

			contentIndex++

		case agent.AgentEventDone:
			if event.Usage != nil {
				outputTokens = event.Usage.CompletionTokens
			}

		case agent.AgentEventError:
			errDelta := map[string]any{
				"type": "content_block_delta", "index": contentIndex,
				"delta": map[string]any{"type": "text_delta", "text": fmt.Sprintf("\n[Error: %s]", event.Error.Error())},
			}
			data, _ := json.Marshal(errDelta)
			fmt.Fprintf(c.Writer, "event: content_block_delta\ndata: %s\n\n", data)
			c.Writer.(http.Flusher).Flush()
		}
	}

	// 关闭最后一个文本块
	stopEvent := map[string]any{"type": "content_block_stop", "index": contentIndex}
	data, _ = json.Marshal(stopEvent)
	fmt.Fprintf(c.Writer, "event: content_block_stop\ndata: %s\n\n", data)
	c.Writer.(http.Flusher).Flush()

	// message_delta
	msgDelta := map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": "end_turn", "status": "completed"},
		"usage": map[string]any{"output_tokens": outputTokens},
	}
	data, _ = json.Marshal(msgDelta)
	fmt.Fprintf(c.Writer, "event: message_delta\ndata: %s\n\n", data)
	c.Writer.(http.Flusher).Flush()

	// message_stop
	msgStop := map[string]any{"type": "message_stop"}
	data, _ = json.Marshal(msgStop)
	fmt.Fprintf(c.Writer, "event: message_stop\ndata: %s\n\n", data)
	c.Writer.(http.Flusher).Flush()
}

// handleNonStream 处理非流式请求
func (h *AnthropicHandler) handleNonStream(c *gin.Context, ag *agent.Agent, conv *agent.Conversation, model string, maxTokens int) {
	resp, err := ag.Run(c.Request.Context(), conv)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"type": "error", "error": gin.H{"type": "server_error", "message": err.Error()}})
		return
	}
	if len(resp.Choices) == 0 {
		c.JSON(http.StatusInternalServerError, gin.H{"type": "error", "error": gin.H{"type": "server_error", "message": "no response"}})
		return
	}

	choice := resp.Choices[0]
	contentBlocks := []any{}
	if choice.Message.Content != "" {
		contentBlocks = append(contentBlocks, map[string]any{"type": "text", "text": choice.Message.Content})
	}
	for _, tc := range choice.Message.ToolCalls {
		contentBlocks = append(contentBlocks, map[string]any{
			"type": "tool_use", "id": tc.ID, "name": tc.Name, "input": json.RawMessage(tc.Arguments),
		})
	}

	stopReason := "end_turn"
	if len(choice.Message.ToolCalls) > 0 {
		stopReason = "tool_use"
	}

	c.JSON(http.StatusOK, map[string]any{
		"id": resp.ID, "type": "message", "role": "assistant",
		"model": model, "content": contentBlocks,
		"status": "completed", "stop_reason": stopReason,
		"usage": map[string]any{
			"input_tokens":  resp.Usage.PromptTokens,
			"output_tokens": resp.Usage.CompletionTokens,
		},
	})
}
