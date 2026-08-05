// Project handlers: 管理层级「项目 → 集群」的第一层。
// GET/POST /api/v1/projects，GET/PUT/DELETE /api/v1/projects/:id。
// 列表与详情返回 { project, cluster_count, clusters }（集群归属计数，不探测）。
package api

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/cluster"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
)

// projectRequest 项目创建/更新请求体。
type projectRequest struct {
	Name string `json:"name" binding:"required"`
	Desc string `json:"desc"`
}

// projectView 项目对外视图（含成员集群统计）。
type projectView struct {
	Project      *store.Project `json:"project"`
	ClusterCount int            `json:"cluster_count"`
	Clusters     []projectClRef `json:"clusters,omitempty"`
}

// projectClRef 项目下集群的轻量引用（无探测）。
type projectClRef struct {
	Name      string `json:"name"`
	Status    string `json:"status"`
	LastSeen  string `json:"last_seen,omitempty"`
	WorkerURL string `json:"worker_url,omitempty"`
}

// ListProjects godoc: GET /api/v1/projects
func (h *Handlers) ListProjects(c *gin.Context) {
	if h.clusters == nil {
		fail(c, http.StatusServiceUnavailable, "cluster service not initialized")
		return
	}
	items, err := h.clusters.ListProjects()
	if err != nil {
		fail(c, http.StatusInternalServerError, "list projects: "+err.Error())
		return
	}
	// 探测失败不应阻断项目列表（归属关系来自注册表，与在线状态无关）
	clusters, err := h.clusters.List(c.Request.Context())
	if err != nil {
		clusters = nil
	}
	views := make([]projectView, 0, len(items))
	for _, p := range items {
		views = append(views, buildProjectView(p, clusters, false))
	}
	ok(c, http.StatusOK, gin.H{"items": views})
}

// GetProject godoc: GET /api/v1/projects/:id
func (h *Handlers) GetProject(c *gin.Context) {
	if h.clusters == nil {
		fail(c, http.StatusServiceUnavailable, "cluster service not initialized")
		return
	}
	p, err := h.clusters.GetProject(c.Param("id"))
	if err != nil {
		var nf cluster.ErrProjectNotFound
		if errors.As(err, &nf) {
			fail(c, http.StatusNotFound, nf.Error())
			return
		}
		fail(c, http.StatusInternalServerError, "get project: "+err.Error())
		return
	}
	clusters, err := h.clusters.List(c.Request.Context())
	if err != nil {
		clusters = nil
	}
	ok(c, http.StatusOK, buildProjectView(p, clusters, true))
}

// CreateProject godoc: POST /api/v1/projects
func (h *Handlers) CreateProject(c *gin.Context) {
	if h.clusters == nil {
		fail(c, http.StatusServiceUnavailable, "cluster service not initialized")
		return
	}
	var req projectRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	p, err := h.clusters.CreateProject(req.Name, req.Desc)
	if err != nil {
		fail(c, http.StatusBadRequest, "create project: "+err.Error())
		return
	}
	ok(c, http.StatusCreated, gin.H{"project": p})
}

// UpdateProject godoc: PUT /api/v1/projects/:id
func (h *Handlers) UpdateProject(c *gin.Context) {
	if h.clusters == nil {
		fail(c, http.StatusServiceUnavailable, "cluster service not initialized")
		return
	}
	var req projectRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	p, err := h.clusters.UpdateProject(c.Param("id"), req.Name, req.Desc)
	if err != nil {
		var nf cluster.ErrProjectNotFound
		if errors.As(err, &nf) {
			fail(c, http.StatusNotFound, nf.Error())
			return
		}
		fail(c, http.StatusBadRequest, "update project: "+err.Error())
		return
	}
	ok(c, http.StatusOK, gin.H{"project": p})
}

// DeleteProject godoc: DELETE /api/v1/projects/:id
func (h *Handlers) DeleteProject(c *gin.Context) {
	if h.clusters == nil {
		fail(c, http.StatusServiceUnavailable, "cluster service not initialized")
		return
	}
	if err := h.clusters.DeleteProject(c.Param("id")); err != nil {
		var nf cluster.ErrProjectNotFound
		if errors.As(err, &nf) {
			fail(c, http.StatusNotFound, nf.Error())
			return
		}
		fail(c, http.StatusInternalServerError, "delete project: "+err.Error())
		return
	}
	ok(c, http.StatusOK, gin.H{"deleted": c.Param("id")})
}

// buildProjectView 组装项目视图：成员集群引用 + 计数。
// clusters 为已探测列表（探测失败时 nil）；withClusters 控制是否带集群明细。
func buildProjectView(p *store.Project, clusters []*store.Cluster, withClusters bool) projectView {
	v := projectView{Project: p}
	for _, c := range clusters {
		if c.ProjectID != "" && c.ProjectID == p.ID {
			v.ClusterCount++
			if withClusters {
				v.Clusters = append(v.Clusters, projectClRef{
					Name:      c.Name,
					Status:    string(c.Status),
					LastSeen:  c.LastSeen.UTC().Format("2006-01-02T15:04:05Z"),
					WorkerURL: c.WorkerURL,
				})
			}
		}
	}
	return v
}
