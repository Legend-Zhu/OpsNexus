// Package api holds the HTTP handlers of the OpsGaurdWeb management plane.
// The skeletons below register the route surface only — business logic lands
// in later iterations. Every handler returns the standard envelope via
// response.go.
package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// --- 集群管理（多集群，类 Rancher） ---

// ListClusters godoc: GET /api/v1/clusters
func (h *Handlers) ListClusters(c *gin.Context) {
	ok(c, http.StatusOK, gin.H{
		"message": "cluster list — 骨架占位（未实现）",
		"items":   []any{},
	})
}

// GetCluster godoc: GET /api/v1/clusters/:name
func (h *Handlers) GetCluster(c *gin.Context) {
	ok(c, http.StatusOK, gin.H{
		"message": "cluster detail — 骨架占位（未实现）",
		"name":    c.Param("name"),
	})
}

// AddCluster godoc: POST /api/v1/clusters
func (h *Handlers) AddCluster(c *gin.Context) {
	ok(c, http.StatusOK, gin.H{"message": "cluster add — 骨架占位（未实现）"})
}

// RemoveCluster godoc: DELETE /api/v1/clusters/:name
func (h *Handlers) RemoveCluster(c *gin.Context) {
	ok(c, http.StatusOK, gin.H{"message": "cluster remove — 骨架占位（未实现）"})
}
