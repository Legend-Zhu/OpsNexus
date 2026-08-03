package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// OpenAIProvider OpenAI 兼容 API 客户端
// 适用于：OpenAI、GLM、DeepSeek、Moonshot、Ollama、vLLM 等所有兼容 OpenAI 格式的服务
type OpenAIProvider struct {
	name    string
	apiKey  string
	baseURL string
	models  []string
	client  *http.Client
}

// NewOpenAIProvider 创建 OpenAI 兼容提供商
func NewOpenAIProvider(name, baseURL, apiKey string, models []string) *OpenAIProvider {
	return &OpenAIProvider{
		name:    name,
		apiKey:  apiKey,
		baseURL: strings.TrimRight(baseURL, "/"),
		models:  models,
		client:  &http.Client{},
	}
}

func (p *OpenAIProvider) Name() string         { return p.name }
func (p *OpenAIProvider) ToolFormat() ToolFormat { return ToolFormatOpenAI }
func (p *OpenAIProvider) Models() []string      { return p.models }

func (p *OpenAIProvider) SupportsModel(model string) bool {
	for _, m := range p.models {
		if m == model {
			return true
		}
	}
	return false
}

// --- 请求/响应结构体 ---

type openaiRequest struct {
	Model        string           `json:"model"`
	Messages     []openaiMessage  `json:"messages"`
	Tools        []openaiTool     `json:"tools,omitempty"`
	Stream       bool             `json:"stream"`
	MaxTokens    int              `json:"max_tokens,omitempty"`
	Temperature  *float64         `json:"temperature,omitempty"`
	StreamOptions *streamOptions  `json:"stream_options,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type openaiMessage struct {
	Role       string           `json:"role"`
	Content    any              `json:"content"`
	ToolCalls  []openaiToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

type openaiToolCall struct {
	ID       string             `json:"id,omitempty"`
	Type     string             `json:"type"`
	Function openaiFunctionCall `json:"function"`
}

type openaiFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type openaiTool struct {
	Type     string       `json:"type"`
	Function openaiFuncDef `json:"function"`
}

type openaiFuncDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type openaiResponse struct {
	ID      string         `json:"id"`
	Model   string         `json:"model"`
	Choices []openaiChoice `json:"choices"`
	Usage   openaiUsage    `json:"usage,omitempty"`
}

type openaiChoice struct {
	Message      openaiMessage `json:"message"`
	FinishReason string        `json:"finish_reason"`
	Index        int           `json:"index"`
}

type openaiUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type openaiStreamChunk struct {
	ID      string               `json:"id"`
	Model   string               `json:"model"`
	Choices []openaiStreamChoice `json:"choices"`
	Usage   *openaiUsage         `json:"usage,omitempty"`
}

type openaiStreamChoice struct {
	Index        int               `json:"index"`
	Delta        openaiStreamDelta `json:"delta"`
	FinishReason *string           `json:"finish_reason"`
}

type openaiStreamDelta struct {
	Role      string                `json:"role,omitempty"`
	Content   *string               `json:"content,omitempty"`
	ToolCalls []openaiToolCallDelta `json:"tool_calls,omitempty"`
}

type openaiToolCallDelta struct {
	Index    int              `json:"index"`
	ID       string           `json:"id,omitempty"`
	Type     string           `json:"type,omitempty"`
	Function *openaiFuncDelta `json:"function,omitempty"`
}

type openaiFuncDelta struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// --- 消息转换 ---

func (p *OpenAIProvider) convertToOpenaiMessages(messages []ChatMessage) []openaiMessage {
	result := make([]openaiMessage, 0, len(messages))
	for _, msg := range messages {
		om := openaiMessage{
			Role:       string(msg.Role),
			ToolCallID: msg.ToolCallID,
		}
		if msg.ToolCallID != "" {
			om.Content = msg.Content
		} else if len(msg.ToolCalls) > 0 {
			om.Content = msg.Content
			for _, tc := range msg.ToolCalls {
				om.ToolCalls = append(om.ToolCalls, openaiToolCall{
					ID:   tc.ID,
					Type: "function",
					Function: openaiFunctionCall{
						Name:      tc.Name,
						Arguments: tc.Arguments,
					},
				})
			}
		} else {
			om.Content = msg.Content
		}
		result = append(result, om)
	}
	return result
}

func (p *OpenAIProvider) convertToolsToOpenai(tools []ToolDefinition) []openaiTool {
	result := make([]openaiTool, 0, len(tools))
	for _, t := range tools {
		result = append(result, openaiTool{
			Type: "function",
			Function: openaiFuncDef{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Parameters,
			},
		})
	}
	return result
}

// --- API 调用 ---

func (p *OpenAIProvider) ChatCompletion(ctx context.Context, req *ChatRequest) (*ChatResponse, error) {
	oaiReq := openaiRequest{
		Model:     req.Model,
		Messages:  p.convertToOpenaiMessages(req.Messages),
		Tools:     p.convertToolsToOpenai(req.Tools),
		MaxTokens: req.MaxTokens,
		Stream:    false,
	}
	if req.Temperature > 0 {
		oaiReq.Temperature = &req.Temperature
	}

	body, err := json.Marshal(oaiReq)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("openai api error: status=%d body=%s", resp.StatusCode, string(respBody))
	}

	var oaiResp openaiResponse
	if err := json.NewDecoder(resp.Body).Decode(&oaiResp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	return p.convertResponse(&oaiResp), nil
}

func (p *OpenAIProvider) ChatCompletionStream(ctx context.Context, req *ChatRequest) (<-chan StreamEvent, error) {
	oaiReq := openaiRequest{
		Model:        req.Model,
		Messages:     p.convertToOpenaiMessages(req.Messages),
		Tools:        p.convertToolsToOpenai(req.Tools),
		Stream:       true,
		StreamOptions: &streamOptions{IncludeUsage: true},
		MaxTokens:    req.MaxTokens,
	}
	if req.Temperature > 0 {
		oaiReq.Temperature = &req.Temperature
	}

	body, err := json.Marshal(oaiReq)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("openai api error: status=%d body=%s", resp.StatusCode, string(respBody))
	}

	ch := make(chan StreamEvent, 64)
	go p.processStream(resp.Body, ch)
	return ch, nil
}

// processStream 处理 SSE 流
func (p *OpenAIProvider) processStream(body io.ReadCloser, ch chan<- StreamEvent) {
	defer close(ch)
	defer body.Close()

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	toolCallsMap := make(map[int]*ToolCall)
	streamDone := false // 标记流是否已结束

	for scanner.Scan() {
		if streamDone {
			break
		}
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			// 发送所有累积的工具调用结束事件
			for _, tc := range toolCallsMap {
				ch <- StreamEvent{Type: EventToolCallEnd, ToolCall: tc}
			}
			ch <- StreamEvent{Type: EventDone}
			streamDone = true
			continue
		}

		var chunk openaiStreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			ch <- StreamEvent{Type: EventError, Error: fmt.Errorf("unmarshal stream chunk: %w", err)}
			return
		}

		for _, choice := range chunk.Choices {
			delta := choice.Delta

			// 文本内容增量
			if delta.Content != nil && *delta.Content != "" {
				ch <- StreamEvent{Type: EventContentDelta, Content: *delta.Content}
			}

			// 工具调用增量
			for _, tc := range delta.ToolCalls {
				existing, ok := toolCallsMap[tc.Index]
				if !ok {
					newTC := &ToolCall{
						ID:        tc.ID,
						Name:      tc.Function.Name,
						Arguments: tc.Function.Arguments,
					}
					toolCallsMap[tc.Index] = newTC
					ch <- StreamEvent{Type: EventToolCallStart, ToolCall: newTC}
				} else {
					if tc.Function != nil {
						existing.Arguments += tc.Function.Arguments
						ch <- StreamEvent{Type: EventToolCallDelta, ToolCall: existing}
					}
				}
			}

			// finish_reason（只在收到 stop 或 tool_calls 时触发 EventDone）
			if choice.FinishReason != nil {
				reason := *choice.FinishReason
				if reason == "tool_calls" {
					for _, tc := range toolCallsMap {
						ch <- StreamEvent{Type: EventToolCallEnd, ToolCall: tc}
					}
				}
				ch <- StreamEvent{Type: EventDone, FinishReason: reason}
				streamDone = true
			}
		}

		// 用量信息（独立发送，不重复触发 EventDone）
		if chunk.Usage != nil && !streamDone {
			// usage 单独出现时只更新，不发 EventDone
			_ = chunk.Usage
		}
	}

	if !streamDone {
		// 流意外结束，确保发送完成事件
		ch <- StreamEvent{Type: EventDone}
	}

	if err := scanner.Err(); err != nil {
		ch <- StreamEvent{Type: EventError, Error: fmt.Errorf("read stream: %w", err)}
	}
}

// convertResponse 将 OpenAI 响应转换为通用响应
func (p *OpenAIProvider) convertResponse(oaiResp *openaiResponse) *ChatResponse {
	choices := make([]ChatChoice, 0, len(oaiResp.Choices))
	for _, c := range oaiResp.Choices {
		msg := ChatMessage{
			Role:    Role(c.Message.Role),
			Content: contentToString(c.Message.Content),
		}
		for _, tc := range c.Message.ToolCalls {
			msg.ToolCalls = append(msg.ToolCalls, ToolCall{
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			})
		}
		choices = append(choices, ChatChoice{
			Message:      msg,
			FinishReason: c.FinishReason,
			Index:        c.Index,
		})
	}
	return &ChatResponse{
		ID:      oaiResp.ID,
		Model:   oaiResp.Model,
		Choices: choices,
		Usage: UsageInfo{
			PromptTokens:     oaiResp.Usage.PromptTokens,
			CompletionTokens: oaiResp.Usage.CompletionTokens,
			TotalTokens:      oaiResp.Usage.TotalTokens,
		},
	}
}

// contentToString 将 content 字段转换为字符串
func contentToString(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case nil:
		return ""
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}
