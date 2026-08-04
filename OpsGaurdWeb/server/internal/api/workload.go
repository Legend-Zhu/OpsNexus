package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// --- 工作负载（经 Worker 编排，代理到目标集群 Worker） ---

// ListWorkloads godoc: GET /api/v1/clusters/:name/workloads
func (h *Handlers) ListWorkloads(c *gin.Context) {
	ok(c, http.StatusOK, gin.H{
		"message": "workload list — 骨架占位（未实现）",
		"cluster": c.Param("name"),
		"items":   []any{},
	})
}

// GetWorkload godoc: GET /api/v1/clusters/:name/workloads/:service
func (h *Handlers) GetWorkload(c *gin.Context) {
	ok(c, http.StatusOK, gin.H{
		"message": "workload detail — 骨架占位（未实现）",
		"cluster": c.Param("name"),
		"service": c.Param("service"),
	})
}

// --- 监控事件（经 Worker /api/v1/events） ---

// ListEvents godoc: GET /api/v1/clusters/:name/events
func (h *Handlers) ListEvents(c *gin.Context) {
	ok(c, http.StatusOK, gin.H{
		"message": "events — 骨架占位（未实现）",
		"cluster": c.Param("name"),
		"items":   []any{},
	})
}

// ListAudit godoc: GET /api/v1/clusters/:name/audit
func (h *Handlers) ListAudit(c *gin.Context) {
	ok(c, http.StatusOK, gin.H{
		"message": "audit — 骨架占位（未实现）",
		"cluster": c.Param("name"),
		"items":   []any{},
	})
}
