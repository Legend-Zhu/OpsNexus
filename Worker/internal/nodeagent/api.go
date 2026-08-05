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
	"time"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/agent"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/docker"
)

// API serves the local node endpoints.
type API struct {
	cli    docker.Client
	policy *agent.CommandPolicy
	log    *slog.Logger
}

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
		"GET /api/v1/local/stats":      a.stats,
		"GET /api/v1/local/processes":  a.processes,
		"POST /api/v1/local/exec":      a.exec,
		"POST /api/v1/local/host":      a.host,
		"GET /api/v1/local/logs":       a.logs,
	}
}

// ---- stats ----

type containerStat struct {
	ContainerID string  `json:"containerId"`
	Service     string  `json:"service,omitempty"`
	TaskID      string  `json:"taskId,omitempty"`
	CPUPercent  float64 `json:"cpuPercent"`
	MemPercent  float64 `json:"memPercent"`
	MemUsage    uint64  `json:"memUsageBytes"`
	MemLimit    uint64  `json:"memLimitBytes"`
}

type statsResp struct {
	Node       string          `json:"node"`
	Containers []containerStat `json:"containers"`
}

func (a *API) stats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	host, _ := hostname()
	// containers that are part of a swarm service
	cs, err := a.cli.ListContainers(ctx, docker.Filter{"label": {"com.docker.swarm.service.id"}})
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	resp := statsResp{Node: host, Containers: make([]containerStat, 0, len(cs))}
	for _, c := range cs {
		if c.State != "running" {
			continue
		}
		first, err := a.cli.ContainerStats(ctx, c.ID)
		if err != nil {
			continue
		}
		time.Sleep(time.Second)
		second, err := a.cli.ContainerStats(ctx, c.ID)
		if err != nil {
			continue
		}
		csd := containerStat{
			ContainerID: c.ID,
			Service:     c.Labels["com.docker.swarm.service.name"],
			TaskID:      c.Labels["com.docker.swarm.task.id"],
			CPUPercent:  round2(cpuDeltaPercent(first, second)),
			MemPercent:  round2(memPercentOf(second)),
			MemUsage:    second.MemoryStats.Usage,
			MemLimit:    second.MemoryStats.Limit,
		}
		resp.Containers = append(resp.Containers, csd)
	}
	writeJSON(w, http.StatusOK, resp)
}

// ---- exec (container) ----

type execReq struct {
	Container string   `json:"container"`           // container ID (or name)
	Command   []string `json:"command"`             // e.g. ["cat","/etc/nginx/nginx.conf"]
	Slot      int      `json:"slot,omitempty"`      // service task slot (alternative to container)
	Service   string   `json:"service,omitempty"`   // service name, used with slot
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
