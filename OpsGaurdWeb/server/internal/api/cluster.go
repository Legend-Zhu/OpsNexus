// Package api holds the HTTP handlers of the OpsGaurdWeb management plane.
// Cluster registry handlers (P1) read/write via cluster.Service — the store
// layer persists to LevelDB, and probing refreshes online/offline status.
package api

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/cluster"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
)

// --- 集群管理 ---

// addClusterRequest 接入集群的请求体。
type addClusterRequest struct {
	Name      string `json:"name" binding:"required"`
	ProjectID string `json:"project_id"`
	WorkerURL string `json:"worker_url" binding:"required"`
	MCPURL    string `json:"mcp_url"`
	Token     string `json:"token"`
	Desc      string `json:"desc"`
}

// ListClusters godoc: GET /api/v1/clusters
// 集群列表（含实时健康探测：online/offline + lastSeen + 最近错误）。
func (h *Handlers) ListClusters(c *gin.Context) {
	if h.clusters == nil {
		fail(c, http.StatusServiceUnavailable, "cluster service not initialized")
		return
	}
	items, err := h.clusters.List(c.Request.Context())
	if err != nil {
		fail(c, http.StatusInternalServerError, "list clusters: "+err.Error())
		return
	}
	pub := make([]*store.Cluster, 0, len(items))
	for _, it := range items {
		pub = append(pub, it.Public())
	}
	ok(c, http.StatusOK, gin.H{"items": pub})
}

// GetCluster godoc: GET /api/v1/clusters/:name
// 集群详情（含实时健康探测）。
func (h *Handlers) GetCluster(c *gin.Context) {
	if h.clusters == nil {
		fail(c, http.StatusServiceUnavailable, "cluster service not initialized")
		return
	}
	item, err := h.clusters.Get(c.Request.Context(), c.Param("name"))
	if err != nil {
		var nf cluster.ErrNotFound
		if errors.As(err, &nf) {
			fail(c, http.StatusNotFound, nf.Error())
			return
		}
		fail(c, http.StatusInternalServerError, "get cluster: "+err.Error())
		return
	}
	ok(c, http.StatusOK, item.Public())
}

// AddCluster godoc: POST /api/v1/clusters
// 接入集群：先探测 Worker（可达 + swarm manager），通过后落库。
func (h *Handlers) AddCluster(c *gin.Context) {
	if h.clusters == nil {
		fail(c, http.StatusServiceUnavailable, "cluster service not initialized")
		return
	}
	var req addClusterRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	item, err := h.clusters.Add(c.Request.Context(), &store.Cluster{
		Name:      req.Name,
		ProjectID: req.ProjectID,
		WorkerURL: req.WorkerURL,
		MCPURL:    req.MCPURL,
		Token:     req.Token,
		Desc:      req.Desc,
	})
	if err != nil {
		var pf cluster.ErrProbeFailed
		if errors.As(err, &pf) {
			fail(c, http.StatusBadGateway, pf.Error())
			return
		}
		fail(c, http.StatusBadRequest, "add cluster: "+err.Error())
		return
	}
	ok(c, http.StatusCreated, item.Public())
}

// UpdateCluster godoc: PUT /api/v1/clusters/:name
// 编辑集群（端点/token/描述/项目），不做重探测。
func (h *Handlers) UpdateCluster(c *gin.Context) {
	if h.clusters == nil {
		fail(c, http.StatusServiceUnavailable, "cluster service not initialized")
		return
	}
	var req addClusterRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	item, err := h.clusters.Update(c.Request.Context(), &store.Cluster{
		Name:      c.Param("name"),
		ProjectID: req.ProjectID,
		WorkerURL: req.WorkerURL,
		MCPURL:    req.MCPURL,
		Token:     req.Token,
		Desc:      req.Desc,
	})
	if err != nil {
		var nf cluster.ErrNotFound
		if errors.As(err, &nf) {
			fail(c, http.StatusNotFound, nf.Error())
			return
		}
		fail(c, http.StatusBadRequest, "update cluster: "+err.Error())
		return
	}
	ok(c, http.StatusOK, item.Public())
}

// RemoveCluster godoc: DELETE /api/v1/clusters/:name
func (h *Handlers) RemoveCluster(c *gin.Context) {
	if h.clusters == nil {
		fail(c, http.StatusServiceUnavailable, "cluster service not initialized")
		return
	}
	if err := h.clusters.Remove(c.Request.Context(), c.Param("name")); err != nil {
		var nf cluster.ErrNotFound
		if errors.As(err, &nf) {
			fail(c, http.StatusNotFound, nf.Error())
			return
		}
		fail(c, http.StatusInternalServerError, "remove cluster: "+err.Error())
		return
	}
	ok(c, http.StatusOK, gin.H{"removed": c.Param("name")})
}
