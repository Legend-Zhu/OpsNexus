// Monitor & alert handlers (P3): webhook ingest, alert list/ack/recover,
// and node resource metrics via the Worker.
package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ingest"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
)

// --- 告警 ingest（Worker webhook 入口） ---

// IngestEvent godoc: POST /api/v1/ingest/events?cluster=<name>[&token=<t>]
// Worker webhook 推送入口：监控事件与审计条目同 URL（按字段区分）。
// 事件落库并按 (cluster,service,type) 聚合告警；审计条目 P3 暂忽略。
func (h *Handlers) IngestEvent(c *gin.Context) {
	if h.ingestSvc == nil {
		fail(c, http.StatusServiceUnavailable, "ingest service not initialized")
		return
	}
	if !ingest.ValidateToken(c.Query("token"), h.ingestToken) {
		fail(c, http.StatusUnauthorized, "invalid ingest token")
		return
	}
	cluster := c.Query("cluster")
	if cluster == "" {
		fail(c, http.StatusBadRequest, "cluster query parameter is required")
		return
	}
	body, err := ingest.LimitReader(c.Request.Body, 1<<20) // 1MB 上限
	if err != nil {
		fail(c, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	if len(body) == 0 {
		fail(c, http.StatusBadRequest, "empty body")
		return
	}
	e, isAudit, err := ingest.ParseEvent(body)
	if err != nil {
		fail(c, http.StatusBadRequest, "parse: "+err.Error())
		return
	}
	if isAudit {
		// 审计 webhook：P3 先落库为事件桶外的审计桶（占位，后续 P6 通知消费）
		ok(c, http.StatusOK, gin.H{"accepted": "audit", "cluster": cluster})
		return
	}
	if err := h.ingestSvc.HandleEvent(cluster, e); err != nil {
		fail(c, http.StatusInternalServerError, "ingest: "+err.Error())
		return
	}
	ok(c, http.StatusOK, gin.H{"accepted": "event", "id": e.ID})
}

// --- 告警列表 / 认领 / 恢复 ---

// ListAlerts godoc: GET /api/v1/alerts?cluster=&status=active|acked|recovered
func (h *Handlers) ListAlerts(c *gin.Context) {
	if h.clusters == nil {
		fail(c, http.StatusServiceUnavailable, "cluster service not initialized")
		return
	}
	status := store.AlertStatus(c.Query("status"))
	switch status {
	case store.AlertActive, store.AlertAcked, store.AlertRecovered, "":
	default:
		fail(c, http.StatusBadRequest, "invalid status: "+c.Query("status"))
		return
	}
	items, err := h.clusters.Alerts(status, c.Query("cluster"))
	if err != nil {
		fail(c, http.StatusInternalServerError, "list alerts: "+err.Error())
		return
	}
	ok(c, http.StatusOK, gin.H{"items": items})
}

// AckAlert godoc: POST /api/v1/alerts/:id/ack
func (h *Handlers) AckAlert(c *gin.Context) {
	if h.clusters == nil {
		fail(c, http.StatusServiceUnavailable, "cluster service not initialized")
		return
	}
	actor := c.GetHeader("X-Actor")
	if actor == "" {
		actor = "admin" // P6 SSO 后替换为登录用户
	}
	a, err := h.clusters.AckAlert(c.Param("id"), actor)
	if err != nil {
		fail(c, http.StatusInternalServerError, "ack: "+err.Error())
		return
	}
	if a == nil {
		fail(c, http.StatusNotFound, "alert not found")
		return
	}
	ok(c, http.StatusOK, a)
}

// RecoverAlert godoc: POST /api/v1/alerts/:id/recover
func (h *Handlers) RecoverAlert(c *gin.Context) {
	if h.clusters == nil {
		fail(c, http.StatusServiceUnavailable, "cluster service not initialized")
		return
	}
	a, err := h.clusters.RecoverAlert(c.Param("id"))
	if err != nil {
		fail(c, http.StatusInternalServerError, "recover: "+err.Error())
		return
	}
	if a == nil {
		fail(c, http.StatusNotFound, "alert not found")
		return
	}
	ok(c, http.StatusOK, a)
}

// --- 节点资源视图（经 Worker /api/v1/local/stats） ---

// ClusterMetrics godoc: GET /api/v1/clusters/:name/metrics
// 节点资源（容器 CPU/内存 统计），前端监控页数据源。
func (h *Handlers) ClusterMetrics(c *gin.Context) {
	cli, got := h.workerClient(c)
	if !got {
		return
	}
	stats, err := cli.NodeStats(c.Request.Context())
	if err != nil {
		proxyErr(c, "node stats", err)
		return
	}
	ok(c, http.StatusOK, stats)
}
