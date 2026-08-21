// Investigation (排查会话) 与巡检报告投递设置的 API。
// 排查会话：对话式 troubleshoot 落库，关联告警时回写「已排查」标记。
// 设置：settings/patrol-report 全局配置（系统设置「巡检报告」tab）。
package api

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/patrol"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
)

// 落库上限（rune 计）：messages 是完整对话 JSON，上限放宽；title/conclusion 精简。
const (
	investigationMessagesMax   = 64 << 10 // 64K 字符
	investigationConclusionMax = 8 << 10  // 8K 字符
	investigationTitleMax      = 120
)

// investigationIn 排查会话保存/更新请求。
type investigationIn struct {
	AlertID    string `json:"alert_id"`
	Cluster    string `json:"cluster"`
	Title      string `json:"title" binding:"required"`
	Messages   string `json:"messages" binding:"required"` // [{role,content}] JSON
	Conclusion string `json:"conclusion"`
	Model      string `json:"model"`
}

// SaveInvestigation godoc: POST /api/v1/investigations
// 保存排查会话；带 alert_id 时回写告警的排查标记（次数+1、最近排查 id）。
func (h *Handlers) SaveInvestigation(c *gin.Context) {
	if h.clusters == nil {
		fail(c, http.StatusServiceUnavailable, "cluster service not initialized")
		return
	}
	var req investigationIn
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	inv := &store.Investigation{
		AlertID:    req.AlertID,
		Cluster:    req.Cluster,
		Title:      capRunes(req.Title, investigationTitleMax),
		Messages:   capRunes(req.Messages, investigationMessagesMax),
		Conclusion: capRunes(req.Conclusion, investigationConclusionMax),
		Model:      req.Model,
	}
	if err := h.clusters.SaveInvestigation(inv); err != nil {
		fail(c, http.StatusInternalServerError, "save investigation: "+err.Error())
		return
	}
	ok(c, http.StatusOK, inv)
}

// UpdateInvestigation godoc: PUT /api/v1/investigations/:id
// 追问推进后更新同一条会话（messages/conclusion 覆盖）。
func (h *Handlers) UpdateInvestigation(c *gin.Context) {
	if h.clusters == nil {
		fail(c, http.StatusServiceUnavailable, "cluster service not initialized")
		return
	}
	inv, err := h.clusters.Investigation(c.Param("id"))
	if err != nil {
		fail(c, http.StatusInternalServerError, "get investigation: "+err.Error())
		return
	}
	if inv == nil {
		fail(c, http.StatusNotFound, "investigation not found")
		return
	}
	var req investigationIn
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	if req.Title != "" {
		inv.Title = capRunes(req.Title, investigationTitleMax)
	}
	inv.Messages = capRunes(req.Messages, investigationMessagesMax)
	inv.Conclusion = capRunes(req.Conclusion, investigationConclusionMax)
	if req.Model != "" {
		inv.Model = req.Model
	}
	if err := h.clusters.SaveInvestigation(inv); err != nil {
		fail(c, http.StatusInternalServerError, "save investigation: "+err.Error())
		return
	}
	ok(c, http.StatusOK, inv)
}

// GetInvestigation godoc: GET /api/v1/investigations/:id
func (h *Handlers) GetInvestigation(c *gin.Context) {
	if h.clusters == nil {
		fail(c, http.StatusServiceUnavailable, "cluster service not initialized")
		return
	}
	inv, err := h.clusters.Investigation(c.Param("id"))
	if err != nil {
		fail(c, http.StatusInternalServerError, "get investigation: "+err.Error())
		return
	}
	if inv == nil {
		fail(c, http.StatusNotFound, "investigation not found")
		return
	}
	ok(c, http.StatusOK, inv)
}

// ListInvestigations godoc: GET /api/v1/investigations?alert_id=&limit=
// 全量排查会话列表（最新在前）；alert_id 为空时含自由提问会话，供 troubleshoot 历史入口使用。
func (h *Handlers) ListInvestigations(c *gin.Context) {
	if h.clusters == nil {
		fail(c, http.StatusServiceUnavailable, "cluster service not initialized")
		return
	}
	limit := 100
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	items, err := h.clusters.Investigations(c.Query("alert_id"), limit)
	if err != nil {
		fail(c, http.StatusInternalServerError, "list investigations: "+err.Error())
		return
	}
	ok(c, http.StatusOK, gin.H{"items": items})
}

// ListAlertInvestigations godoc: GET /api/v1/alerts/:id/investigations
func (h *Handlers) ListAlertInvestigations(c *gin.Context) {
	if h.clusters == nil {
		fail(c, http.StatusServiceUnavailable, "cluster service not initialized")
		return
	}
	items, err := h.clusters.Investigations(c.Param("id"), 50)
	if err != nil {
		fail(c, http.StatusInternalServerError, "list investigations: "+err.Error())
		return
	}
	ok(c, http.StatusOK, gin.H{"items": items})
}

// capRunes 按字符数截断（避免 UTF-8 截断出半个汉字）。
func capRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…[truncated]"
}

// --- 巡检报告投递设置 ---

// GetPatrolReportSetting godoc: GET /api/v1/settings/patrol-report
// 未配置时返回默认（off）。
func (h *Handlers) GetPatrolReportSetting(c *gin.Context) {
	if h.patrolSvc == nil {
		fail(c, http.StatusServiceUnavailable, "patrol service not initialized")
		return
	}
	cfg, err := h.patrolSvc.ReportDeliveryConfig()
	if err != nil {
		fail(c, http.StatusInternalServerError, "get setting: "+err.Error())
		return
	}
	ok(c, http.StatusOK, cfg)
}

// PutPatrolReportSetting godoc: PUT /api/v1/settings/patrol-report
func (h *Handlers) PutPatrolReportSetting(c *gin.Context) {
	if h.patrolSvc == nil {
		fail(c, http.StatusServiceUnavailable, "patrol service not initialized")
		return
	}
	var cfg patrol.ReportDelivery
	if err := c.ShouldBindJSON(&cfg); err != nil {
		fail(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	if err := h.patrolSvc.SaveReportDeliveryConfig(cfg); err != nil {
		fail(c, http.StatusBadRequest, err.Error())
		return
	}
	ok(c, http.StatusOK, cfg)
}
