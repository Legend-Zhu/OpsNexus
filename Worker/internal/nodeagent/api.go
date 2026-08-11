// Package nodeagent serves the per-node local API: every Worker instance
// (manager or node role) exposes the node's local stats, container exec, and
// host command execution under /api/v1/local/*. The manager-role Worker
// aggregates/proxies these when answering cross-node MCP calls.
package nodeagent

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/agent"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/docker"
)

// API serves the local node endpoints.
type API struct {
	cli    docker.Client
	policy *agent.CommandPolicy
	log    *slog.Logger

	// statsCache 宿主机/容器资源采样的短时缓存：采样本身固定耗时 ~1s
	// （两次快照间隔），节点列表/监控页每次刷新都全量采样会造成明显卡顿。
	// TTL 内命中直接返回上次结果（快照型数据，5s 精度损失可忽略）。
	statsMu    sync.Mutex
	statsCache *StatsResp
	statsAt    time.Time
	// hostStatsCache 轻量 host-only 采样缓存（流式节点卡片专用，跳过
	// per-container 双快照）。与 statsCache 共用 statsMu。
	hostStatsCache *HostStatsResp
	hostStatsAt    time.Time
}

const statsCacheTTL = 5 * time.Second

// New builds the local API.
func New(cli docker.Client, policy *agent.CommandPolicy, log *slog.Logger) *API {
	if log == nil {
		log = slog.Default()
	}
	return &API{cli: cli, policy: policy, log: log}
}

// Routes returns the local endpoint handlers.
func (a *API) Routes() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"GET /api/v1/local/stats":         a.stats,
		"GET /api/v1/local/host-stats":    a.hostStats,
		"GET /api/v1/local/processes":     a.processes,
		"GET /api/v1/local/containers":    a.containers,
		"POST /api/v1/local/containers/restart": a.restartContainer,
		"POST /api/v1/local/exec":         a.exec,
		"POST /api/v1/local/host":         a.host,
		"GET /api/v1/local/logs":          a.logs,
		"GET /api/v1/local/check/port":    a.checkPort,
		"POST /api/v1/local/check/http":   a.checkHTTP,
		"POST /api/v1/local/check/flow":   a.checkFlow,
	}
}

// ---- exec (container) ----

type execReq struct {
	Container string   `json:"container"`         // container ID (or name)
	Command   []string `json:"command"`           // e.g. ["cat","/etc/nginx/nginx.conf"]
	Slot      int      `json:"slot,omitempty"`    // service task slot (alternative to container)
	Service   string   `json:"service,omitempty"` // service name, used with slot
}

type execResp struct {
	ContainerID string `json:"containerId"`
	ExitCode    int    `json:"exitCode"`
	Output      string `json:"output"`
}

func (a *API) exec(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req execReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if len(req.Command) == 0 {
		writeErr(w, http.StatusBadRequest, errEmpty("command"))
		return
	}
	// policy gate
	if err := a.policy.CheckContainer(joinCmd(req.Command)); err != nil {
		writeErr(w, http.StatusForbidden, err)
		return
	}
	cid := req.Container
	if cid == "" && req.Service != "" {
		got, err := a.findTaskContainer(ctx, req.Service, req.Slot)
		if err != nil {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		cid = got
	}
	if cid == "" {
		writeErr(w, http.StatusBadRequest, errEmpty("container or service+slot"))
		return
	}
	execID, err := a.cli.ContainerExecCreate(ctx, cid, req.Command)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	rc, err := a.cli.ExecStart(ctx, execID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	defer rc.Close()
	data, _ := readAll(rc)
	ei, err := a.cli.ExecInspect(ctx, execID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, execResp{ContainerID: cid, ExitCode: ei.ExitCode, Output: string(data)})
}

// ---- host (host command execution) ----

type hostReq struct {
	Command string `json:"command"`
}

type hostResp struct {
	Node      string `json:"node"`
	ExitCode  int    `json:"exitCode"`
	Output    string `json:"output"`
	ElapsedMS int64  `json:"elapsedMs"`
}

func (a *API) host(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req hostReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// policy gate (allowHostExec + blacklist/whitelist)
	if err := a.policy.CheckHost(req.Command); err != nil {
		writeErr(w, http.StatusForbidden, err)
		return
	}
	host, _ := hostname()
	out, code, elapsed, err := agent.RunHostCommand(ctx, req.Command, a.policy.TimeoutDuration())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, hostResp{Node: host, ExitCode: code, Output: out, ElapsedMS: elapsed.Milliseconds()})
}

// ---- helpers ----

func (a *API) findTaskContainer(ctx context.Context, service string, slot int) (string, error) {
	svc, err := a.cli.GetService(ctx, service)
	if err != nil {
		return "", err
	}
	tasks, err := a.cli.ServiceTasks(ctx, svc.ID)
	if err != nil {
		return "", err
	}
	for _, t := range tasks {
		if t.Status.State != "running" || t.Status.ContainerStatus.ContainerID == "" {
			continue
		}
		if slot == 0 || t.Slot == slot {
			return t.Status.ContainerStatus.ContainerID, nil
		}
	}
	return "", errNotFound("running container for service", service)
}
