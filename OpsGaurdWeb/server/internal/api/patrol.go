// Patrol handlers (P5): YAML flow CRUD, run-on-demand, execution records and
// AI reports.
package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/patrol"
)

// --- 智能巡检（YAML 流程 + 内置调度 + AI 报告） ---

// ListPatrols godoc: GET /api/v1/patrols
func (h *Handlers) ListPatrols(c *gin.Context) {
	if h.patrolSvc == nil {
		fail(c, http.StatusServiceUnavailable, "patrol service not initialized")
		return
	}
	items, err := h.patrolSvc.List()
	if err != nil {
		fail(c, http.StatusInternalServerError, "list patrols: "+err.Error())
		return
	}
	ok(c, http.StatusOK, gin.H{"items": items})
}

// GetPatrol godoc: GET /api/v1/patrols/:id
func (h *Handlers) GetPatrol(c *gin.Context) {
	if h.patrolSvc == nil {
		fail(c, http.StatusServiceUnavailable, "patrol service not initialized")
		return
	}
	p, err := h.patrolSvc.Get(c.Param("id"))
	if err != nil {
		var nf patrol.ErrNotFound
		if errors.As(err, &nf) {
			fail(c, http.StatusNotFound, nf.Error())
			return
		}
		fail(c, http.StatusInternalServerError, "get patrol: "+err.Error())
		return
	}
	ok(c, http.StatusOK, p)
}

// createPatrolRequest 创建/更新流程请求体。
type createPatrolRequest struct {
	Name        string `json:"name" binding:"required"`
	Description string `json:"description"`
	Cron        string `json:"cron" binding:"required"`
	Enabled     bool   `json:"enabled"`
	YAML        string `json:"yaml" binding:"required"`
}

// CreatePatrol godoc: POST /api/v1/patrols
func (h *Handlers) CreatePatrol(c *gin.Context) {
	if h.patrolSvc == nil {
		fail(c, http.StatusServiceUnavailable, "patrol service not initialized")
		return
	}
	var req createPatrolRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	p, err := h.patrolSvc.Create(req.Name, req.Description, req.Cron, req.YAML, req.Enabled)
	if err != nil {
		var inv patrol.ErrInvalidFlow
		if errors.As(err, &inv) {
			fail(c, http.StatusBadRequest, inv.Error())
			return
		}
		fail(c, http.StatusInternalServerError, "create patrol: "+err.Error())
		return
	}
	ok(c, http.StatusCreated, p)
}

// UpdatePatrol godoc: PUT /api/v1/patrols/:id
func (h *Handlers) UpdatePatrol(c *gin.Context) {
	if h.patrolSvc == nil {
		fail(c, http.StatusServiceUnavailable, "patrol service not initialized")
		return
	}
	var req createPatrolRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	p, err := h.patrolSvc.Update(c.Param("id"), req.Name, req.Description, req.Cron, req.YAML, req.Enabled)
	if err != nil {
		var nf patrol.ErrNotFound
		if errors.As(err, &nf) {
			fail(c, http.StatusNotFound, nf.Error())
			return
		}
		var inv patrol.ErrInvalidFlow
		if errors.As(err, &inv) {
			fail(c, http.StatusBadRequest, inv.Error())
			return
		}
		fail(c, http.StatusInternalServerError, "update patrol: "+err.Error())
		return
	}
	ok(c, http.StatusOK, p)
}

// DeletePatrol godoc: DELETE /api/v1/patrols/:id
func (h *Handlers) DeletePatrol(c *gin.Context) {
	if h.patrolSvc == nil {
		fail(c, http.StatusServiceUnavailable, "patrol service not initialized")
		return
	}
	if err := h.patrolSvc.Delete(c.Param("id")); err != nil {
		var nf patrol.ErrNotFound
		if errors.As(err, &nf) {
			fail(c, http.StatusNotFound, nf.Error())
			return
		}
		fail(c, http.StatusInternalServerError, "delete patrol: "+err.Error())
		return
	}
	ok(c, http.StatusOK, gin.H{"deleted": c.Param("id")})
}

// RunPatrol godoc: POST /api/v1/patrols/:id/run
// 立即执行一次巡检。
func (h *Handlers) RunPatrol(c *gin.Context) {
	if h.patrolSvc == nil {
		fail(c, http.StatusServiceUnavailable, "patrol service not initialized")
		return
	}
	run, err := h.patrolSvc.Run(c.Request.Context(), c.Param("id"))
	if err != nil {
		var nf patrol.ErrNotFound
		if errors.As(err, &nf) {
			fail(c, http.StatusNotFound, nf.Error())
			return
		}
		fail(c, http.StatusInternalServerError, "run patrol: "+err.Error())
		return
	}
	ok(c, http.StatusAccepted, run)
}

// ListPatrolRuns godoc: GET /api/v1/patrols/:id/runs?limit=
func (h *Handlers) ListPatrolRuns(c *gin.Context) {
	if h.patrolSvc == nil {
		fail(c, http.StatusServiceUnavailable, "patrol service not initialized")
		return
	}
	limit := parseLimit(c.Query("limit"), 20)
	items, err := h.patrolSvc.Runs(c.Param("id"), limit)
	if err != nil {
		fail(c, http.StatusInternalServerError, "list runs: "+err.Error())
		return
	}
	ok(c, http.StatusOK, gin.H{"items": items})
}

// ListPatrolReports godoc: GET /api/v1/patrols/:id/reports?limit=
func (h *Handlers) ListPatrolReports(c *gin.Context) {
	if h.patrolSvc == nil {
		fail(c, http.StatusServiceUnavailable, "patrol service not initialized")
		return
	}
	limit := parseLimit(c.Query("limit"), 20)
	items, err := h.patrolSvc.Reports(c.Param("id"), limit)
	if err != nil {
		fail(c, http.StatusInternalServerError, "list reports: "+err.Error())
		return
	}
	ok(c, http.StatusOK, gin.H{"items": items})
}

// RunByID godoc: GET /api/v1/patrols/:id/runs/:runId
func (h *Handlers) GetPatrolRun(c *gin.Context) {
	if h.patrolSvc == nil {
		fail(c, http.StatusServiceUnavailable, "patrol service not initialized")
		return
	}
	run, err := h.patrolSvc.RunByID(c.Param("runId"))
	if err != nil {
		fail(c, http.StatusNotFound, "run not found")
		return
	}
	ok(c, http.StatusOK, run)
}

func parseLimit(s string, def int) int {
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return def
	}
	if n > 100 {
		return 100
	}
	return n
}
