// Node handlers: 管理层级「集群 → 节点 → 容器/进程」。
// GET /api/v1/clusters/:name/nodes              集群节点列表（含资源聚合）
// GET /api/v1/clusters/:name/nodes/:id/processes 节点宿主机进程
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/workerproxy"
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

// StreamNodes godoc: GET /api/v1/clusters/:name/nodes/stream
// Server-Sent Events: emits an "init" event (base node list, immediately — no
// stats fan-out so the UI renders right away), then one "node" event per node
// as its stats sample completes (fetched concurrently, so a slow/unreachable
// node never blocks the others). Terminates with data: [DONE]. Auth via
// ?token= query (EventSource can't set Authorization header).
func (h *Handlers) StreamNodes(c *gin.Context) {
	cli, got := h.workerClient(c)
	if !got {
		return
	}
	fl, ok := c.Writer.(http.Flusher)
	if !ok {
		fail(c, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	c.Writer.WriteHeader(http.StatusOK)
	fl.Flush()

	stream, err := cli.WatchNodeStats(c.Request.Context())
	if err != nil {
		fmt.Fprintf(c.Writer, "event: error\ndata: %s\n\n", err.Error())
		fl.Flush()
		return
	}
	for {
		upd, err := stream.Recv()
		if err != nil {
			break
		}
		var data []byte
		switch upd.GetKind() {
		case "init":
			// 经 NodeFromPB 转成 camelCase JSON 视图：pb 结构体的 json tag
			// 是 snake_case + omitempty，直接 marshal 会让前端读不到
			// cpuCores/memBytes（节点详情显示 “—” / “0 B”）。
			data, _ = json.Marshal(workerproxy.NodesFromPB(upd.GetNodes()))
		case "node":
			data, _ = json.Marshal(map[string]any{
				"nodeId":         upd.GetNodeId(),
				"reachable":      upd.GetReachable(),
				"cpuPercent":     upd.GetCpuPercent(),
				"memPercent":     upd.GetMemPercent(),
				"memUsage":       upd.GetMemUsage(),
				"memLimit":       upd.GetMemLimit(),
				"containerCount": upd.GetContainerCount(),
			})
		default:
			continue
		}
		fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", upd.GetKind(), data)
		fl.Flush()
	}
	fmt.Fprintf(c.Writer, "data: [DONE]\n\n")
	fl.Flush()
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
