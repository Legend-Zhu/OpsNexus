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
	"time"
	"unicode/utf8"
)

// AnthropicProvider Anthropic 兼容 API 客户端
// 适用于：Anthropic Claude 及所有兼容 Anthropic Messages API 格式的服务
type AnthropicProvider struct {
	name    string
	apiKey  string
	baseURL string
	models  []string
	client  *http.Client
}

// NewAnthropicProvider 创建 Anthropic 兼容提供商
// name: 提供商名称（如 "anthropic"）
// baseURL: API 基础地址（如 "https://api.anthropic.com"）
// apiKey: API Key
// models: 支持的模型列表（如 ["claude-sonnet-4-20250514", "claude-opus-4-20250514"]）
func NewAnthropicProvider(name, baseURL, apiKey string, models []string) *AnthropicProvider {
	return &AnthropicProvider{
		name:    name,
		apiKey:  apiKey,
		baseURL: strings.TrimRight(baseURL, "/"),
		models:  models,
		client:  &http.Client{Timeout: 120 * time.Second},
	}
}

func (p *AnthropicProvider) Name() string           { return p.name }
func (p *AnthropicProvider) ToolFormat() ToolFormat { return ToolFormatAnthropic }
func (p *AnthropicProvider) Models() []string       { return p.models }

func (p *AnthropicProvider) SupportsModel(model string) bool {
	for _, m := range p.models {
		if m == model {
			return true
		}
	}
	return false
}

// --- 请求/响应结构体 ---

type anthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	System    any                `json:"system,omitempty"`
	Messages  []anthropicMessage `json:"messages"`
	Tools     []anthropicTool    `json:"tools,omitempty"`
	Stream    bool               `json:"stream"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type anthropicContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

type anthropicTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

type anthropicResponse struct {
	ID         string                  `json:"id"`
	Type       string                  `json:"type"`
	Role       string                  `json:"role"`
	Content    []anthropicContentBlock `json:"content"`
	Model      string                  `json:"model"`
	Usage      anthropicUsage          `json:"usage"`
	StopReason string                  `json:"stop_reason"`
}

type anthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type anthropicContentBlockDelta struct {
	Type        string `json:"type"`
	Text        string `json:"text,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
}

type anthropicMessageDelta struct {
	StopReason string `json:"stop_reason,omitempty"`
}

// --- 消息转换 ---

func (p *AnthropicProvider) convertToAnthropicMessages(messages []ChatMessage) (systemPrompt any, msgs []anthropicMessage) {
	var systemBlocks []anthropicContentBlock
	msgs = make([]anthropicMessage, 0, len(messages))

	for _, msg := range messages {
		if msg.Role == RoleSystem {
			systemBlocks = append(systemBlocks, anthropicContentBlock{Type: "text", Text: msg.Content})
			continue
		}

		am := anthropicMessage{Role: string(msg.Role)}

		if msg.Role == RoleTool {
			am.Content = []anthropicContentBlock{
				{Type: "tool_result", ToolUseID: msg.ToolCallID, Content: msg.Content},
			}
		} else if len(msg.ToolCalls) > 0 {
			blocks := make([]anthropicContentBlock, 0)
			if msg.Content != "" {
				blocks = append(blocks, anthropicContentBlock{Type: "text", Text: msg.Content})
			}
			for _, tc := range msg.ToolCalls {
				blocks = append(blocks, anthropicContentBlock{
					Type:  "tool_use",
					ID:    tc.ID,
					Name:  tc.Name,
					Input: json.RawMessage(tc.Arguments),
				})
			}
			am.Content = blocks
		} else {
			am.Content = msg.Content
		}

		msgs = append(msgs, am)
	}

	if len(systemBlocks) > 0 {
		if len(systemBlocks) == 1 {
			systemPrompt = systemBlocks[0].Text
		} else {
			systemPrompt = systemBlocks
		}
	}

	return systemPrompt, msgs
}

func (p *AnthropicProvider) convertToolsToAnthropic(tools []ToolDefinition) []anthropicTool {
	result := make([]anthropicTool, 0, len(tools))
	for _, t := range tools {
		result = append(result, anthropicTool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.Parameters,
		})
	}
	return result
}

// --- API 调用 ---

func (p *AnthropicProvider) ChatCompletion(ctx context.Context, req *ChatRequest) (*ChatResponse, error) {
	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = 4096
	}

	systemPrompt, messages := p.convertToAnthropicMessages(req.Messages)

	antReq := anthropicRequest{
		Model:     req.Model,
		MaxTokens: maxTokens,
		System:    systemPrompt,
		Messages:  messages,
		Tools:     p.convertToolsToAnthropic(req.Tools),
		Stream:    false,
	}

	body, err := json.Marshal(antReq)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", p.apiKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("anthropic api error: status=%d body=%s", resp.StatusCode, string(respBody))
	}

	var antResp anthropicResponse
	if err := json.NewDecoder(resp.Body).Decode(&antResp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	return p.convertResponse(&antResp), nil
}

func (p *AnthropicProvider) ChatCompletionStream(ctx context.Context, req *ChatRequest) (<-chan StreamEvent, error) {
	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = 4096
	}

	systemPrompt, messages := p.convertToAnthropicMessages(req.Messages)

	antReq := anthropicRequest{
		Model:     req.Model,
		MaxTokens: maxTokens,
		System:    systemPrompt,
		Messages:  messages,
		Tools:     p.convertToolsToAnthropic(req.Tools),
		Stream:    true,
	}

	body, err := json.Marshal(antReq)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", p.apiKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("anthropic api error: status=%d body=%s", resp.StatusCode, string(respBody))
	}

	ch := make(chan StreamEvent, 64)
	go p.processStream(resp.Body, ch)
	return ch, nil
}

// processStream 处理 Anthropic SSE 流。
// usage 语义：input_tokens 在 message_start、output_tokens 在
// message_delta 送达；EventDone 统一在 message_stop（或流意外结束）时只发
// 送一次，携带合并 usage——不在 message_delta 提前发送，避免同一调用产生
// 重复完成事件。
func (p *AnthropicProvider) processStream(body io.ReadCloser, ch chan<- StreamEvent) {
	defer close(ch)
	defer body.Close()

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	toolCallsMap := make(map[int]*ToolCall)
	var currentEventType string
	var inputTokens, outputTokens int
	finishReason := ""
	doneSent := false

	for scanner.Scan() {
		line := scanner.Text()

		if strings.HasPrefix(line, "event: ") {
			currentEventType = strings.TrimPrefix(line, "event: ")
			continue
		}

		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")

		switch currentEventType {
		case "message_start":
			// input tokens 在消息开始时已知
			var event struct {
				Message struct {
					Usage *anthropicUsage `json:"usage"`
				} `json:"message"`
			}
			if err := json.Unmarshal([]byte(data), &event); err == nil && event.Message.Usage != nil {
				inputTokens = event.Message.Usage.InputTokens
				if event.Message.Usage.OutputTokens > 0 {
					outputTokens = event.Message.Usage.OutputTokens
				}
			}

		case "content_block_start":
			var event struct {
				Index        int                   `json:"index"`
				ContentBlock anthropicContentBlock `json:"content_block"`
			}
			if err := json.Unmarshal([]byte(data), &event); err != nil {
				continue
			}
			if event.ContentBlock.Type == "tool_use" {
				tc := &ToolCall{
					ID:   event.ContentBlock.ID,
					Name: event.ContentBlock.Name,
				}
				toolCallsMap[event.Index] = tc
				ch <- StreamEvent{Type: EventToolCallStart, ToolCall: tc}
			}

		case "content_block_delta":
			var event struct {
				Index int                        `json:"index"`
				Delta anthropicContentBlockDelta `json:"delta"`
			}
			if err := json.Unmarshal([]byte(data), &event); err != nil {
				continue
			}
			switch event.Delta.Type {
			case "text_delta":
				// 截断单条超长 chunk，防一次性塞爆 Agent 上下文
				content := event.Delta.Text
				if utf8.RuneCountInString(content) > MaxStreamChunkRunes {
					content = string([]rune(content)[:MaxStreamChunkRunes]) + "\n…[output truncated]"
				}
				ch <- StreamEvent{Type: EventContentDelta, Content: content}
			case "input_json_delta":
				if tc, ok := toolCallsMap[event.Index]; ok {
					tc.Arguments += event.Delta.PartialJSON
					ch <- StreamEvent{Type: EventToolCallDelta, ToolCall: tc}
				}
			}

		case "content_block_stop":
			var event struct {
				Index int `json:"index"`
			}
			if err := json.Unmarshal([]byte(data), &event); err != nil {
				continue
			}
			if tc, ok := toolCallsMap[event.Index]; ok {
				ch <- StreamEvent{Type: EventToolCallEnd, ToolCall: tc}
			}

		case "message_delta":
			var event struct {
				Delta anthropicMessageDelta `json:"delta"`
				Usage *anthropicUsage       `json:"usage,omitempty"`
			}
			if err := json.Unmarshal([]byte(data), &event); err != nil {
				continue
			}
			if event.Delta.StopReason != "" {
				finishReason = event.Delta.StopReason
			}
			if event.Usage != nil {
				if event.Usage.InputTokens > 0 {
					inputTokens = event.Usage.InputTokens
				}
				if event.Usage.OutputTokens > 0 {
					outputTokens = event.Usage.OutputTokens
				}
			}

		case "message_stop":
			if !doneSent {
				doneSent = true
				ch <- StreamEvent{
					Type:         EventDone,
					FinishReason: normalizeFinishReason(finishReason),
					Usage:        mergedUsage(inputTokens, outputTokens),
				}
			}

		case "error":
			ch <- StreamEvent{Type: EventError, Error: fmt.Errorf("anthropic stream error: %s", data)}
		}

		currentEventType = ""
	}

	if err := scanner.Err(); err != nil {
		ch <- StreamEvent{Type: EventError, Error: fmt.Errorf("read stream: %w", err)}
		return
	}

	// 流意外结束（无 message_stop）→ 兜底发送一次完成事件
	if !doneSent {
		ch <- StreamEvent{
			Type:         EventDone,
			FinishReason: normalizeFinishReason(finishReason),
			Usage:        mergedUsage(inputTokens, outputTokens),
		}
	}
}

// normalizeFinishReason 把 Anthropic 的 tool_use 归一为通用 tool_calls。
func normalizeFinishReason(reason string) string {
	if reason == "tool_use" {
		return "tool_calls"
	}
	return reason
}

// mergedUsage 合并输入/输出 token（均为 0 时返回 nil，视同 usage 缺失）。
func mergedUsage(in, out int) *UsageInfo {
	if in == 0 && out == 0 {
		return nil
	}
	return &UsageInfo{
		PromptTokens:     in,
		CompletionTokens: out,
		TotalTokens:      in + out,
	}
}

// convertResponse 将 Anthropic 响应转换为通用响应
func (p *AnthropicProvider) convertResponse(antResp *anthropicResponse) *ChatResponse {
	msg := ChatMessage{Role: RoleAssistant}
	var toolCalls []ToolCall

	for _, block := range antResp.Content {
		switch block.Type {
		case "text":
			msg.Content += block.Text
		case "tool_use":
			toolCalls = append(toolCalls, ToolCall{
				ID:        block.ID,
				Name:      block.Name,
				Arguments: string(block.Input),
			})
		}
	}
	msg.ToolCalls = toolCalls

	finishReason := antResp.StopReason
	if finishReason == "tool_use" {
		finishReason = "tool_calls"
	}

	return &ChatResponse{
		ID:    antResp.ID,
		Model: antResp.Model,
		Choices: []ChatChoice{
			{
				Message:      msg,
				FinishReason: finishReason,
				Index:        0,
			},
		},
		Usage: UsageInfo{
			PromptTokens:     antResp.Usage.InputTokens,
			CompletionTokens: antResp.Usage.OutputTokens,
			TotalTokens:      antResp.Usage.InputTokens + antResp.Usage.OutputTokens,
		},
	}
}
