// Monitor & alert handlers (P3): alert list/ack/recover, and node resource
// metrics via the Worker. Event ingest is now driven by the gRPC event
// subscriber (internal/ingest/subscriber.go), not an HTTP webhook endpoint.
package api

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
)


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
	// P6 联动：ack 通知
	if h.notifySvc != nil {
		_ = h.notifySvc.NotifyAlert(c.Request.Context(), a, fmt.Sprintf("[%s/%s] 告警已认领（%s）", a.Cluster, a.Service, actor))
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
