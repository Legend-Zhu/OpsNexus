package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/cluster"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/workerproxy"
)

// --- 工作负载（经 Worker 编排，代理到目标集群 Worker） ---

// workerClient 取目标集群的 Worker 客户端。
func (h *Handlers) workerClient(c *gin.Context) (*workerproxy.Client, bool) {
	if h.clusters == nil {
		fail(c, http.StatusServiceUnavailable, "cluster service not initialized")
		return nil, false
	}
	cli, err := h.clusters.WorkerClient(c.Param("name"))
	if err != nil {
		var nf cluster.ErrNotFound
		if errors.As(err, &nf) {
			fail(c, http.StatusNotFound, nf.Error())
			return nil, false
		}
		fail(c, http.StatusInternalServerError, "worker client: "+err.Error())
		return nil, false
	}
	return cli, true
}

// proxyErr 把 Worker 代理错误归一为 HTTP 状态码。
func proxyErr(c *gin.Context, action string, err error) {
	var ue *workerproxy.ErrUnreachable
	if errors.As(err, &ue) {
		if ue.Status >= 400 && ue.Status < 500 {
			fail(c, ue.Status, fmt.Sprintf("%s: %s", action, err.Error()))
			return
		}
		fail(c, http.StatusBadGateway, fmt.Sprintf("%s: %s", action, err.Error()))
		return
	}
	fail(c, http.StatusInternalServerError, fmt.Sprintf("%s: %s", action, err.Error()))
}

// ListWorkloads godoc: GET /api/v1/clusters/:name/workloads
func (h *Handlers) ListWorkloads(c *gin.Context) {
	cli, got := h.workerClient(c)
	if !got {
		return
	}
	items, err := cli.ListWorkloads(c.Request.Context(), c.Query("label"))
	if err != nil {
		proxyErr(c, "list workloads", err)
		return
	}
	ok(c, http.StatusOK, gin.H{"items": items})
}

// GetWorkload godoc: GET /api/v1/clusters/:name/workloads/:service
// 服务详情（tasks + 健康），并附最近一次部署/更新的配置快照（config）供
// 前端"编辑服务"预填；无快照（外部创建等）时省略。
func (h *Handlers) GetWorkload(c *gin.Context) {
	cli, got := h.workerClient(c)
	if !got {
		return
	}
	name := c.Param("service")
	detail, err := cli.GetWorkload(c.Request.Context(), name)
	if err != nil {
		proxyErr(c, "get workload", err)
		return
	}
	if sc, _ := h.clusters.ServiceConfig(c.Param("name"), name); sc != nil {
		detail.Config = sc.Config
	}
	ok(c, http.StatusOK, detail)
}

// deployWorkloadRequest 部署/更新请求体（config 为 Worker YAML/JSON 配置）。
type deployWorkloadRequest struct {
	Config string `json:"config" binding:"required"`
}

// DeployWorkload godoc: POST /api/v1/clusters/:name/workloads
// 部署服务：config 下发到 Worker，返回异步操作（前端轮询 ops/:id）。
// 成功后保存配置快照（编辑预填用）。
func (h *Handlers) DeployWorkload(c *gin.Context) {
	cli, got := h.workerClient(c)
	if !got {
		return
	}
	var req deployWorkloadRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	op, err := cli.Deploy(c.Request.Context(), req.Config)
	if err != nil {
		proxyErr(c, "deploy", err)
		return
	}
	if op.Service != "" {
		_ = h.clusters.SaveServiceConfig(c.Param("name"), op.Service, req.Config)
	}
	ok(c, http.StatusAccepted, op)
}

// UpdateWorkload godoc: PUT /api/v1/clusters/:name/workloads/:service
// 更新服务配置（整体替换，语义同部署）：config 下发到 Worker 的 Update RPC，
// 成功后更新配置快照。
func (h *Handlers) UpdateWorkload(c *gin.Context) {
	cli, got := h.workerClient(c)
	if !got {
		return
	}
	var req deployWorkloadRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	name := c.Param("service")
	op, err := cli.Update(c.Request.Context(), name, req.Config)
	if err != nil {
		proxyErr(c, "update", err)
		return
	}
	_ = h.clusters.SaveServiceConfig(c.Param("name"), name, req.Config)
	ok(c, http.StatusAccepted, op)
}

// ScaleWorkload godoc: POST /api/v1/clusters/:name/workloads/:service/scale
func (h *Handlers) ScaleWorkload(c *gin.Context) {
	cli, got := h.workerClient(c)
	if !got {
		return
	}
	var body struct {
		Replicas uint64 `json:"replicas" binding:"required"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		fail(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	op, err := cli.Scale(c.Request.Context(), c.Param("service"), body.Replicas)
	if err != nil {
		proxyErr(c, "scale", err)
		return
	}
	ok(c, http.StatusAccepted, op)
}

// RestartWorkload godoc: POST /api/v1/clusters/:name/workloads/:service/restart
func (h *Handlers) RestartWorkload(c *gin.Context) {
	cli, got := h.workerClient(c)
	if !got {
		return
	}
	op, err := cli.Restart(c.Request.Context(), c.Param("service"))
	if err != nil {
		proxyErr(c, "restart", err)
		return
	}
	ok(c, http.StatusAccepted, op)
}

// RemoveWorkload godoc: DELETE /api/v1/clusters/:name/workloads/:service
// 移除服务，成功后清理其配置快照。
func (h *Handlers) RemoveWorkload(c *gin.Context) {
	cli, got := h.workerClient(c)
	if !got {
		return
	}
	name := c.Param("service")
	op, err := cli.Remove(c.Request.Context(), name)
	if err != nil {
		proxyErr(c, "remove", err)
		return
	}
	_ = h.clusters.DeleteServiceConfig(c.Param("name"), name)
	ok(c, http.StatusOK, op)
}

// GetWorkloadOperation godoc: GET /api/v1/clusters/:name/workloads/ops/:id
// 查询异步编排操作（部署/缩放/重启 的进度与终态）。
func (h *Handlers) GetWorkloadOperation(c *gin.Context) {
	cli, got := h.workerClient(c)
	if !got {
		return
	}
	op, err := cli.Operation(c.Request.Context(), c.Param("id"))
	if err != nil {
		proxyErr(c, "get operation", err)
		return
	}
	ok(c, http.StatusOK, op)
}

// StreamWorkloadLogs godoc: GET /api/v1/clusters/:name/workloads/:service/logs
// SSE 透传 Worker 日志流（follow 可选；数据格式 data: {"ts","stream","line"}）。
func (h *Handlers) StreamWorkloadLogs(c *gin.Context) {
	cli, got := h.workerClient(c)
	if !got {
		return
	}
	follow := c.Query("follow") == "true"
	tail, _ := strconv.Atoi(c.Query("tail"))

	fl, ok := c.Writer.(http.Flusher)
	if !ok {
		fail(c, http.StatusInternalServerError, "streaming not supported")
		return
	}
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	err := cli.StreamLogs(c.Request.Context(), c.Param("service"), follow, tail, c.Query("since"),
		func(ll workerproxy.LogLine) bool {
			fmt.Fprintf(c.Writer, "data: %s\n\n", mustJSON(ll))
			fl.Flush()
			return true
		})
	if err != nil {
		var ue *workerproxy.ErrUnreachable
		if errors.As(err, &ue) && ue.Status == http.StatusNotFound {
			fmt.Fprintf(c.Writer, "event: error\ndata: {\"message\": %q}\n\n", "service not found")
			fl.Flush()
			return
		}
		fmt.Fprintf(c.Writer, "event: error\ndata: {\"message\": %q}\n\n", err.Error())
		fl.Flush()
	}
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return `{"error":"marshal failed"}`
	}
	return string(b)
}

// --- 监控事件 / 审计（经 Worker /api/v1/events、/api/v1/audit） ---

// ListEvents godoc: GET /api/v1/clusters/:name/events?service=&type=&limit=&after_seq=
// after_seq 为倒序分页游标：返回 seq < after_seq 的更早一页（0 = 最新一页）。
func (h *Handlers) ListEvents(c *gin.Context) {
	cli, got := h.workerClient(c)
	if !got {
		return
	}
	limit, _ := strconv.Atoi(c.Query("limit"))
	afterSeq, _ := strconv.ParseInt(c.Query("after_seq"), 10, 64)
	items, err := cli.Events(c.Request.Context(), c.Query("service"), c.Query("type"), afterSeq, limit)
	if err != nil {
		proxyErr(c, "list events", err)
		return
	}
	ok(c, http.StatusOK, gin.H{"items": items})
}

// ListAudit godoc: GET /api/v1/clusters/:name/audit
func (h *Handlers) ListAudit(c *gin.Context) {
	cli, got := h.workerClient(c)
	if !got {
		return
	}
	limit, _ := strconv.Atoi(c.Query("limit"))
	items, err := cli.Audit(c.Request.Context(), c.Query("action"), limit)
	if err != nil {
		proxyErr(c, "list audit", err)
		return
	}
	ok(c, http.StatusOK, gin.H{"items": items})
}
