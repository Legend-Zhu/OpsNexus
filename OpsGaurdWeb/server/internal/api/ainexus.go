package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// --- AiNexus 整合（内嵌网关，非独立服务） ---
//
// AiNexus 的 Go 代码 vendor 进 server/internal/ainexus，与管理端同进程运行：
// 无独立端口、无进程间 HTTP。前端聊天页 → /api/v1/ainexus/chat → 进程内
// 直调内嵌网关的 OpenAI handler（SSE 写回同一响应流）；深度排查由内嵌
// ReAct Agent 经 MCP 连接集群 Worker 的 /mcp 采集证据。管理端可在上下文
// 中注入集群诊断信息（事件、日志、审计）供 Agent 排查。

// AINexusChat godoc: POST /api/v1/ainexus/chat
// 进程内调用内嵌 AiNexus 网关的 OpenAI 格式对话接口（SSE 流式）。
func (h *Handlers) AINexusChat(c *gin.Context) {
	if h.AINexus == nil {
		fail(c, http.StatusServiceUnavailable, "ainexus gateway is not enabled in config")
		return
	}
	h.AINexus.OpenAIHandler().ChatCompletions(c)
}

// AINexusHealth godoc: GET /api/v1/ainexus/health
// 内嵌网关健康（providers/models/tools/mcp_servers）。
func (h *Handlers) AINexusHealth(c *gin.Context) {
	if h.AINexus == nil {
		fail(c, http.StatusServiceUnavailable, "ainexus gateway is not enabled in config")
		return
	}
	h.AINexus.HealthHandler(c)
}

// ListAINexusModels godoc: GET /api/v1/ainexus/models
// 模型列表（名称/provider/类型），供前端模型选择器使用。
func (h *Handlers) ListAINexusModels(c *gin.Context) {
	if h.AINexus == nil {
		fail(c, http.StatusServiceUnavailable, "ainexus gateway is not enabled in config")
		return
	}
	h.AINexus.ModelsHandler(c)
}

// --- 内嵌网关原生端点（挂载在 /ainexus，兼容 AiNexus 自身 URL 契约） ---

// EmbedOpenAIHandler godoc: POST /ainexus/v1/chat/completions
func (h *Handlers) EmbedOpenAIHandler(c *gin.Context) {
	if h.AINexus == nil {
		fail(c, http.StatusServiceUnavailable, "ainexus gateway is not enabled in config")
		return
	}
	h.AINexus.OpenAIHandler().ChatCompletions(c)
}

// EmbedAnthropicHandler godoc: POST /ainexus/v1/messages
func (h *Handlers) EmbedAnthropicHandler(c *gin.Context) {
	if h.AINexus == nil {
		fail(c, http.StatusServiceUnavailable, "ainexus gateway is not enabled in config")
		return
	}
	h.AINexus.AnthropicHandler().Messages(c)
}

// EmbedModelsHandler godoc: GET /ainexus/v1/models
func (h *Handlers) EmbedModelsHandler(c *gin.Context) {
	if h.AINexus == nil {
		fail(c, http.StatusServiceUnavailable, "ainexus gateway is not enabled in config")
		return
	}
	h.AINexus.OpenAIHandler().Models(c)
}

// EmbedToolsHandler godoc: GET /ainexus/api/tools
func (h *Handlers) EmbedToolsHandler(c *gin.Context) {
	if h.AINexus == nil {
		fail(c, http.StatusServiceUnavailable, "ainexus gateway is not enabled in config")
		return
	}
	h.AINexus.ToolsHandler(c)
}

// EmbedMCPHandler godoc: GET /ainexus/api/mcp
func (h *Handlers) EmbedMCPHandler(c *gin.Context) {
	if h.AINexus == nil {
		fail(c, http.StatusServiceUnavailable, "ainexus gateway is not enabled in config")
		return
	}
	h.AINexus.MCPHandler(c)
}
