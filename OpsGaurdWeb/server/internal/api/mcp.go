// 管理端 MCP Server 的管理 API（admin only）：token 运行时管理、调用
// 审计查询、用量统计、接入信息。前端「系统设置 → MCP 接入」tab 消费。
package api

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

// MCPTokens godoc: GET /api/v1/mcp/tokens
// token 列表（静态种子镜像 + 运行时条目；不含 secret）。
func (h *Handlers) ListMCPTokens(c *gin.Context) {
	mc := h.mcpSvc
	if mc == nil {
		fail(c, http.StatusServiceUnavailable, "mcp server is not enabled")
		return
	}
	items, err := mc.ListTokens()
	if err != nil {
		fail(c, http.StatusInternalServerError, err.Error())
		return
	}
	ok(c, http.StatusOK, gin.H{"items": items})
}

// CreateMCPToken godoc: POST /api/v1/mcp/tokens
// 新建运行时 token。secret 原文仅在本次响应返回一次（落库只有摘要）。
func (h *Handlers) CreateMCPToken(c *gin.Context) {
	mc := h.mcpSvc
	if mc == nil {
		fail(c, http.StatusServiceUnavailable, "mcp server is not enabled")
		return
	}
	var body struct {
		Name  string `json:"name" binding:"required"`
		Scope string `json:"scope"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		fail(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	view, secret, err := mc.CreateToken(body.Name, body.Scope)
	if err != nil {
		fail(c, http.StatusBadRequest, err.Error())
		return
	}
	ok(c, http.StatusOK, gin.H{"token": view, "secret": secret,
		"note": "secret 只显示这一次，请立即保存"})
}

// DeleteMCPToken godoc: DELETE /api/v1/mcp/tokens/:name
// 删除运行时 token（config.yaml 静态种子拒绝）。
func (h *Handlers) DeleteMCPToken(c *gin.Context) {
	mc := h.mcpSvc
	if mc == nil {
		fail(c, http.StatusServiceUnavailable, "mcp server is not enabled")
		return
	}
	if err := mc.DeleteToken(c.Param("name")); err != nil {
		fail(c, http.StatusBadRequest, err.Error())
		return
	}
	ok(c, http.StatusOK, gin.H{"deleted": c.Param("name")})
}

// SetMCPTokenEnabled godoc: PUT /api/v1/mcp/tokens/:name/enabled
// 启停（静态种子也可禁用；热生效）。
func (h *Handlers) SetMCPTokenEnabled(c *gin.Context) {
	mc := h.mcpSvc
	if mc == nil {
		fail(c, http.StatusServiceUnavailable, "mcp server is not enabled")
		return
	}
	var body struct {
		Enabled *bool `json:"enabled" binding:"required"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Enabled == nil {
		fail(c, http.StatusBadRequest, "invalid request: enabled(bool) is required")
		return
	}
	if err := mc.SetTokenEnabled(c.Param("name"), *body.Enabled); err != nil {
		fail(c, http.StatusBadRequest, err.Error())
		return
	}
	ok(c, http.StatusOK, gin.H{"name": c.Param("name"), "enabled": *body.Enabled})
}

// MCPAudit godoc: GET /api/v1/mcp/audit?actor=&tool=&limit=
// 写工具调用审计（最新在前）。
func (h *Handlers) ListMCPAudit(c *gin.Context) {
	mc := h.mcpSvc
	if mc == nil {
		fail(c, http.StatusServiceUnavailable, "mcp server is not enabled")
		return
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	items, err := mc.AuditQuery(c.Query("actor"), c.Query("tool"), limit)
	if err != nil {
		fail(c, http.StatusInternalServerError, err.Error())
		return
	}
	ok(c, http.StatusOK, gin.H{"items": items})
}

// MCPUsage godoc: GET /api/v1/mcp/usage?days=7
// token×tool 调用计数聚合。
func (h *Handlers) MCPUsage(c *gin.Context) {
	mc := h.mcpSvc
	if mc == nil {
		fail(c, http.StatusServiceUnavailable, "mcp server is not enabled")
		return
	}
	days, _ := strconv.Atoi(c.DefaultQuery("days", "7"))
	ok(c, http.StatusOK, mc.Usage(days))
}

// MCPInfo godoc: GET /api/v1/mcp/info
// 接入信息：启用状态/端点路径/token 数（端点完整地址由前端按 location 拼）。
func (h *Handlers) MCPInfo(c *gin.Context) {
	mc := h.mcpSvc
	if mc == nil {
		ok(c, http.StatusOK, gin.H{"enabled": false})
		return
	}
	ok(c, http.StatusOK, gin.H{
		"enabled": true,
		"tokens":  mc.TokenNames(),
	})
}
