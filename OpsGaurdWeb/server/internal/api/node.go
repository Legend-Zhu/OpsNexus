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

// NodeProcesses godoc: GET /api/v1/clusters/:name/nodes/:id/processes?top=&limit=&filter=
// top=cpu|mem 排序，limit 默认 100，filter 按名称/cmdline 子串过滤。
func (h *Handlers) NodeProcesses(c *gin.Context) {
	cli, got := h.workerClient(c)
	if !got {
		return
	}
	limit, _ := strconv.Atoi(c.Query("limit"))
	procs, err := cli.ListProcesses(c.Request.Context(), c.Param("id"), c.Query("top"), limit, c.Query("filter"))
	if err != nil {
		proxyErr(c, "list node processes", err)
		return
	}
	ok(c, http.StatusOK, procs)
}

// NodeContainers godoc: GET /api/v1/clusters/:name/nodes/:id/containers
// 节点上的全部容器（swarm 任务容器 + standalone docker run 容器，如 r-nacos）。
func (h *Handlers) NodeContainers(c *gin.Context) {
	cli, got := h.workerClient(c)
	if !got {
		return
	}
	cs, err := cli.NodeContainers(c.Request.Context(), c.Param("id"))
	if err != nil {
		proxyErr(c, "list node containers", err)
		return
	}
	ok(c, http.StatusOK, gin.H{"items": cs})
}

// NodeContainerRestart godoc: POST /api/v1/clusters/:name/nodes/:id/containers/restart
// 重启节点上的一个容器（standalone docker run 容器；body: {"container": "名称或ID"}）。
func (h *Handlers) NodeContainerRestart(c *gin.Context) {
	cli, got := h.workerClient(c)
	if !got {
		return
	}
	var req struct {
		Container string `json:"container" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	if err := cli.RestartContainer(c.Request.Context(), c.Param("id"), req.Container); err != nil {
		proxyErr(c, "restart container", err)
		return
	}
	ok(c, http.StatusOK, gin.H{"restarted": req.Container})
}
