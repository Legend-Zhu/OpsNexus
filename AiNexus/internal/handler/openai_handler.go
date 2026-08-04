package handler

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/legeosoft/ainexus/internal/agent"
	"github.com/legeosoft/ainexus/internal/config"
	"github.com/legeosoft/ainexus/internal/provider"
	"github.com/legeosoft/ainexus/internal/tool"
)

// OpenAIHandler OpenAI 格式接口处理器
type OpenAIHandler struct {
	modelRoutes  map[string]provider.Provider // model名 -> provider
	registry     *tool.Registry
	config       config.Config
	logger       *log.Logger
	agentFactory func(provider.Provider) *agent.Agent
}

// NewOpenAIHandler 创建 OpenAI 格式处理器
func NewOpenAIHandler(modelRoutes map[string]provider.Provider, registry *tool.Registry, cfg config.Config, logger *log.Logger) *OpenAIHandler {
	h := &OpenAIHandler{
		modelRoutes: modelRoutes,
		registry:    registry,
		config:      cfg,
		logger:      logger,
	}
	h.agentFactory = func(p provider.Provider) *agent.Agent {
		return agent.New(p, registry, cfg.Agent, logger)
	}
	return h
}

// openaiChatRequest OpenAI Chat Completion 请求
type openaiChatRequest struct {
	Model       string    `json:"model"`
	Messages    []oaiMsg  `json:"messages"`
	Tools       []any     `json:"tools,omitempty"`
	Stream      bool      `json:"stream"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	Temperature *float64  `json:"temperature,omitempty"`
}

type oaiMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatCompletions 处理 /v1/chat/completions 请求
func (h *OpenAIHandler) ChatCompletions(c *gin.Context) {
	var req openaiChatRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": err.Error(), "type": "invalid_request_error"}})
		return
	}

	// 通过 model 名称查找 provider
	p, ok := h.modelRoutes[req.Model]
	if !ok {
		available := make([]string, 0, len(h.modelRoutes))
		for m := range h.modelRoutes {
			available = append(available, m)
		}
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{
				"message": fmt.Sprintf("model %q not found. Available models: %v", req.Model, available),
				"type":    "invalid_request_error",
			},
		})
		return
	}

	ag := h.agentFactory(p)

	conv := agent.NewConversation(req.Model)
	for _, msg := range req.Messages {
		switch msg.Role {
		case "system":
			conv.AddSystemMessage(msg.Content)
		case "user":
			conv.AddUserMessage(msg.Content)
		case "assistant":
			conv.AddAssistantMessage(msg.Content)
		}
	}

	if req.Stream {
		h.handleStream(c, ag, conv, req.Model)
	} else {
		h.handleNonStream(c, ag, conv, req.Model)
	}
}

// handleStream 处理流式请求
func (h *OpenAIHandler) handleStream(c *gin.Context, ag *agent.Agent, conv *agent.Conversation, model string) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	eventCh, err := ag.RunStream(c.Request.Context(), conv)
	if err != nil {
		c.SSEvent("", gin.H{"error": err.Error()})
		return
	}

	msgID := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	created := time.Now().Unix()
	toolCallIndex := 0

	for event := range eventCh {
		switch event.Type {
		case agent.AgentEventText:
			resp := openaiStreamResponse{
				ID: msgID, Object: "chat.completion.chunk", Created: created, Model: model,
				Choices: []openaiStreamChoice{{Index: 0, Delta: openaiStreamDelta{Content: event.Content}}},
			}
			data, _ := json.Marshal(resp)
			fmt.Fprintf(c.Writer, "data: %s\n\n", data)
			c.Writer.(http.Flusher).Flush()

		case agent.AgentEventToolStart:
			resp := openaiStreamResponse{
				ID: msgID, Object: "chat.completion.chunk", Created: created, Model: model,
				Choices: []openaiStreamChoice{{
					Index: 0,
					Delta: openaiStreamDelta{
						ToolCalls: []openaiToolCallDelta{{
							Index: toolCallIndex, ID: event.ToolCall.ID, Type: "function",
							Function: &openaiFuncDelta{Name: event.ToolCall.Name},
						}},
					},
				}},
			}
			data, _ := json.Marshal(resp)
			fmt.Fprintf(c.Writer, "data: %s\n\n", data)
			c.Writer.(http.Flusher).Flush()

		case agent.AgentEventToolEnd:
				delta := openaiToolCallDelta{
					Index: toolCallIndex,
					Function: &openaiFuncDelta{Arguments: event.ToolCall.Arguments},
				}
				if event.ToolResult != nil {
					delta.ToolResult = &openaiToolResult{
						Name:    event.ToolCall.Name,
						Content: event.ToolResult.Content,
						IsError: event.ToolResult.IsError,
					}
				}
				resp := openaiStreamResponse{
					ID: msgID, Object: "chat.completion.chunk", Created: created, Model: model,
					Choices: []openaiStreamChoice{{
						Index: 0,
						Delta: openaiStreamDelta{
							ToolCalls: []openaiToolCallDelta{delta},
						},
					}},
				}
				data, _ := json.Marshal(resp)
				fmt.Fprintf(c.Writer, "data: %s\n\n", data)
				c.Writer.(http.Flusher).Flush()
				toolCallIndex++

		case agent.AgentEventDone:
			finishReason := "stop"
			if toolCallIndex > 0 {
				finishReason = "tool_calls"
			}
			resp := openaiStreamResponse{
				ID: msgID, Object: "chat.completion.chunk", Created: created, Model: model,
				Choices: []openaiStreamChoice{{Index: 0, FinishReason: &finishReason, Delta: openaiStreamDelta{}}},
			}
			data, _ := json.Marshal(resp)
			fmt.Fprintf(c.Writer, "data: %s\n\n", data)
			c.Writer.(http.Flusher).Flush()

		case agent.AgentEventError:
			resp := openaiStreamResponse{
				ID: msgID, Object: "chat.completion.chunk", Created: created, Model: model,
				Choices: []openaiStreamChoice{{
					Index: 0, Delta: openaiStreamDelta{Content: fmt.Sprintf("[Error: %s]", event.Error.Error())},
				}},
			}
			data, _ := json.Marshal(resp)
			fmt.Fprintf(c.Writer, "data: %s\n\n", data)
			c.Writer.(http.Flusher).Flush()
		}
	}

	fmt.Fprintf(c.Writer, "data: [DONE]\n\n")
	c.Writer.(http.Flusher).Flush()
}

// handleNonStream 处理非流式请求
func (h *OpenAIHandler) handleNonStream(c *gin.Context, ag *agent.Agent, conv *agent.Conversation, model string) {
	resp, err := ag.Run(c.Request.Context(), conv)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": err.Error(), "type": "server_error"}})
		return
	}
	if len(resp.Choices) == 0 {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": "no response", "type": "server_error"}})
		return
	}

	choice := resp.Choices[0]
	msgResp := oaiMsgResp{Role: "assistant", Content: choice.Message.Content}
	for _, tc := range choice.Message.ToolCalls {
		msgResp.ToolCalls = append(msgResp.ToolCalls, openaiToolCallResp{
			ID: tc.ID, Type: "function",
			Function: openaiFuncResp{Name: tc.Name, Arguments: tc.Arguments},
		})
	}

	finishReason := choice.FinishReason
	if finishReason == "" {
		finishReason = "stop"
	}

	c.JSON(http.StatusOK, openaiNonStreamResponse{
		ID: resp.ID, Object: "chat.completion", Created: time.Now().Unix(), Model: model,
		Choices: []openaiChoice{{Index: 0, Message: msgResp, FinishReason: finishReason}},
		Usage:   oaiUsageResp{PromptTokens: resp.Usage.PromptTokens, CompletionTokens: resp.Usage.CompletionTokens, TotalTokens: resp.Usage.TotalTokens},
	})
}

// Models 处理 /v1/models 请求
func (h *OpenAIHandler) Models(c *gin.Context) {
	type modelObj struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	var models []modelObj
	for modelName, p := range h.modelRoutes {
		models = append(models, modelObj{
			ID: modelName, Object: "model", Created: time.Now().Unix(), OwnedBy: p.Name(),
		})
	}
	c.JSON(http.StatusOK, gin.H{"object": "list", "data": models})
}

// --- 响应结构体 ---

type openaiStreamResponse struct {
	ID      string              `json:"id"`
	Object  string              `json:"object"`
	Created int64               `json:"created"`
	Model   string              `json:"model"`
	Choices []openaiStreamChoice `json:"choices"`
}

type openaiStreamChoice struct {
	Index        int               `json:"index"`
	Delta        openaiStreamDelta `json:"delta"`
	FinishReason *string           `json:"finish_reason,omitempty"`
}

type openaiStreamDelta struct {
	Role      string               `json:"role,omitempty"`
	Content   string               `json:"content,omitempty"`
	ToolCalls []openaiToolCallDelta `json:"tool_calls,omitempty"`
}

type openaiToolCallDelta struct {
	Index      int               `json:"index"`
	ID         string            `json:"id,omitempty"`
	Type       string            `json:"type,omitempty"`
	Function   *openaiFuncDelta  `json:"function,omitempty"`
	ToolResult *openaiToolResult `json:"tool_result,omitempty"`
}

type openaiToolResult struct {
	Name    string `json:"name"`
	Content string `json:"content"`
	IsError bool   `json:"is_error"`
}

type openaiFuncDelta struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type openaiNonStreamResponse struct {
	ID      string          `json:"id"`
	Object  string          `json:"object"`
	Created int64           `json:"created"`
	Model   string          `json:"model"`
	Choices []openaiChoice  `json:"choices"`
	Usage   oaiUsageResp    `json:"usage"`
}

type openaiChoice struct {
	Index        int         `json:"index"`
	Message      oaiMsgResp  `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

type oaiMsgResp struct {
	Role      string                `json:"role"`
	Content   string                `json:"content"`
	ToolCalls []openaiToolCallResp  `json:"tool_calls,omitempty"`
}

type openaiToolCallResp struct {
	ID       string         `json:"id"`
	Type     string         `json:"type"`
	Function openaiFuncResp `json:"function"`
}

type openaiFuncResp struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type oaiUsageResp struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}
