package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// --- AiNexus 整合（异常排查） ---
//
// AiNexus 是一个独立的 AI 对话网关/Agent 服务（仓库 ./AiNexus），对外暴露
// OpenAI/Anthropic 双格式流式接口，并内置命令执行、HTTP 请求、文件读取工具，
// 可连接 MCP（含本平台 Worker 的 /mcp）。管理端将其作为一个服务整合：
// 前端聊天页 → 本服务代理 → AiNexus（SSE 透传），并可在上下文中注入
// 集群诊断信息（事件、日志、审计）供 Agent 排查。

// ListAINexusModels godoc: GET /api/v1/ainexus/models
// 代理 AiNexus /v1/models（模型列表），供前端模型选择器使用。
func (h *Handlers) ListAINexusModels(c *gin.Context) {
	ok(c, http.StatusOK, gin.H{
		"message": "ainexus models — 骨架占位（未实现）",
		"items":   []any{},
	})
}

// AINexusChat godoc: POST /api/v1/ainexus/chat
// 代理 AiNexus OpenAI 格式对话接口（SSE 流式透传）。
func (h *Handlers) AINexusChat(c *gin.Context) {
	ok(c, http.StatusOK, gin.H{
		"message": "ainexus chat — 骨架占位（未实现，将 SSE 透传）",
	})
}

// AINexusHealth godoc: GET /api/v1/ainexus/health
func (h *Handlers) AINexusHealth(c *gin.Context) {
	ok(c, http.StatusOK, gin.H{"message": "ainexus health — 骨架占位（未实现）"})
}
