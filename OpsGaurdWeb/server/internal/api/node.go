// Node handlers: 管理层级「集群 → 节点 → 容器/进程」。
// GET /api/v1/clusters/:name/nodes              集群节点列表（含资源聚合）
// GET /api/v1/clusters/:name/nodes/:id/processes 节点宿主机进程
package api

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

// ListNodes godoc: GET /api/v1/clusters/:name/nodes
func (h *Handlers) ListNodes(c *gin.Context) {
	cli, got := h.workerClient(c)
	if !got {
		return
	}
	nodes, err := cli.ListNodes(c.Request.Context())
	if err != nil {
		proxyErr(c, "list nodes", err)
		return
	}
	ok(c, http.StatusOK, gin.H{"items": nodes})
}

// NodeProcesses godoc: GET /api/v1/clusters/:name/nodes/:id/processes?top=&limit=
// top=cpu|mem 排序，limit 默认 100。
func (h *Handlers) NodeProcesses(c *gin.Context) {
	cli, got := h.workerClient(c)
	if !got {
		return
	}
	limit, _ := strconv.Atoi(c.Query("limit"))
	procs, err := cli.ListProcesses(c.Request.Context(), c.Param("id"), c.Query("top"), limit)
	if err != nil {
		proxyErr(c, "list node processes", err)
		return
	}
	ok(c, http.StatusOK, procs)
}
