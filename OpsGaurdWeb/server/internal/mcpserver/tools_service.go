// 服务工具（service_*）：经 Worker 编排的部署/更新/扩缩/重启/删除/日志/
// 操作轮询/审计。全部要求 cluster 参数路由到目标集群。
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	mcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/workerproxy"
)

const deployHint = "operation is async: poll service_operation until status=done|failed (or healthy) before concluding success."

type clusterServiceIn struct {
	Cluster string `json:"cluster" description:"Cluster name (from cluster_list)"`
	Label   string `json:"label,omitempty" description:"Filter by label, e.g. app=web"`
}

type clusterServiceNameIn struct {
	Cluster string `json:"cluster" description:"Cluster name (from cluster_list)"`
	Service string `json:"service" description:"Service name"`
}

type serviceDeployIn struct {
	Cluster string `json:"cluster" description:"Cluster name (from cluster_list)"`
	Config  string `json:"config" description:"Worker service config, full YAML or JSON (same format as the platform deploy page / Worker deploy_service)"`
}

type serviceUpdateIn struct {
	Cluster string `json:"cluster" description:"Cluster name"`
	Service string `json:"service" description:"Service name to update"`
	Config  string `json:"config" description:"New full service config (YAML/JSON); get the current one via service_get.config first"`
}

type serviceScaleIn struct {
	Cluster  string `json:"cluster" description:"Cluster name"`
	Service  string `json:"service" description:"Service name"`
	Replicas uint64 `json:"replicas" description:"Target replica count (0 stops the service; requires confirm=true)"`
	Confirm  bool   `json:"confirm,omitempty" description:"Must be true when replicas=0"`
}

type serviceRemoveIn struct {
	Cluster string `json:"cluster" description:"Cluster name"`
	Service string `json:"service" description:"Service name to delete"`
	Confirm bool   `json:"confirm,omitempty" description:"Must be true (deletes the service and its tasks)"`
}

type serviceLogsIn struct {
	Cluster string `json:"cluster" description:"Cluster name"`
	Service string `json:"service" description:"Service name"`
	Tail    int    `json:"tail,omitempty" description:"Recent lines to return (default 200, max 1000)"`
	Since   string `json:"since,omitempty" description:"Only logs newer than this (duration like 10m, or RFC3339)"`
}

type serviceOpIn struct {
	Cluster string `json:"cluster" description:"Cluster name"`
	OpID    string `json:"op_id" description:"Operation id returned by service_deploy/update/scale/remove/restart"`
}

type serviceAuditIn struct {
	Cluster string `json:"cluster" description:"Cluster name"`
	Limit   int    `json:"limit,omitempty" description:"Max entries (default 20)"`
}

type opOut struct {
	ID       string   `json:"id"`
	Type     string   `json:"type"`
	Service  string   `json:"service"`
	Status   string   `json:"status"`
	Error    string   `json:"error,omitempty"`
	Steps    []string `json:"steps,omitempty"`
	Replicas uint64   `json:"replicas,omitempty"`
	Hint     string   `json:"hint,omitempty"`
}

func toOpOut(op workerproxy.Operation) opOut {
	return opOut{
		ID: op.ID, Type: op.Type, Service: op.Service, Status: string(op.Status),
		Error: op.Error, Steps: op.Steps, Replicas: op.Replicas,
		Hint: deployHint,
	}
}

type workloadOut struct {
	Name    string                    `json:"name"`
	Image   string                    `json:"image,omitempty"`
	Mode    string                    `json:"mode,omitempty"`
	Replica string                    `json:"replica,omitempty"`
	Running uint64                    `json:"running,omitempty"`
	Desired uint64                    `json:"desired,omitempty"`
	Ports   []workerproxy.PortMapping `json:"ports,omitempty"`
}

type serviceListOut struct {
	Items []workloadOut `json:"items"`
}

type serviceDetailOut struct {
	workloadOut
	Tasks   []workerproxy.TaskView `json:"tasks,omitempty"`
	Healthy int                    `json:"healthy"`
	Config  string                 `json:"config,omitempty"`
	Hint    string                 `json:"hint,omitempty"`
}

type serviceLogsOut struct {
	Lines string `json:"lines"`
}

type serviceAuditOut struct {
	Items json.RawMessage `json:"items"`
}

func toWorkloadOut(w workerproxy.Workload) workloadOut {
	return workloadOut{
		Name: w.Name, Image: w.Image, Mode: w.Mode, Replica: w.Replica,
		Running: w.Running, Desired: w.Desired, Ports: w.Ports,
	}
}

func (h *Handler) registerServiceTools(s *mcp.Server) {
	// service_list
	mcp.AddTool(s, &mcp.Tool{
		Name:        "service_list",
		Description: "List swarm services in a cluster with image and replica status.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in clusterServiceIn) (*mcp.CallToolResult, serviceListOut, error) {
		cli, err := h.workerClient(in.Cluster)
		if err != nil {
			return nil, serviceListOut{}, err
		}
		items, err := cli.ListWorkloads(ctx, in.Label)
		if err != nil {
			return nil, serviceListOut{}, err
		}
		out := make([]workloadOut, 0, len(items))
		for _, w := range items {
			out = append(out, toWorkloadOut(w))
		}
		return nil, serviceListOut{Items: out}, nil
	})

	// service_get
	mcp.AddTool(s, &mcp.Tool{
		Name:        "service_get",
		Description: "Get one service: tasks, health counts, ports, and the current config snapshot (use it as the base for service_update).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in clusterServiceNameIn) (*mcp.CallToolResult, serviceDetailOut, error) {
		cli, err := h.workerClient(in.Cluster)
		if err != nil {
			return nil, serviceDetailOut{}, err
		}
		d, err := cli.GetWorkload(ctx, in.Service)
		if err != nil {
			return nil, serviceDetailOut{}, err
		}
		if sc, _ := h.deps.Clusters.ServiceConfig(in.Cluster, in.Service); sc != nil {
			d.Config = sc.Config
		}
		out := serviceDetailOut{
			workloadOut: toWorkloadOut(d.Workload),
			Tasks:       d.Tasks,
			Healthy:     d.Healthy,
			Config:      truncStr(d.Config),
			Hint:        "to change the service, edit config and call service_update",
		}
		return nil, out, nil
	})

	// service_deploy
	mcp.AddTool(s, &mcp.Tool{
		Name:        "service_deploy",
		Description: "Deploy a new service from a Worker config (YAML/JSON). Images built via build_submit are referenced as registry.opsguard/<name>:<tag>. Returns a pending operation; poll service_operation. Replicas/ports/env/image are fields of the config, not of this tool.",
	}, audited(h, "service_deploy",
		func(in serviceDeployIn) map[string]string {
			return map[string]string{"cluster": in.Cluster, "config": in.Config}
		},
		func(ctx context.Context, in serviceDeployIn) (opOut, error) {
			cli, err := h.workerClient(in.Cluster)
			if err != nil {
				return opOut{}, err
			}
			op, err := cli.Deploy(ctx, in.Config)
			if err != nil {
				return opOut{}, err
			}
			if op.Service != "" {
				_ = h.deps.Clusters.SaveServiceConfig(in.Cluster, op.Service, in.Config)
			}
			return toOpOut(op), nil
		}))

	// service_update
	mcp.AddTool(s, &mcp.Tool{
		Name:        "service_update",
		Description: "Rolling-update a service with a new full config (get current via service_get.config, edit, submit). Returns a pending operation.",
	}, audited(h, "service_update",
		func(in serviceUpdateIn) map[string]string {
			return map[string]string{"cluster": in.Cluster, "service": in.Service, "config": in.Config}
		},
		func(ctx context.Context, in serviceUpdateIn) (opOut, error) {
			cli, err := h.workerClient(in.Cluster)
			if err != nil {
				return opOut{}, err
			}
			op, err := cli.Update(ctx, in.Service, in.Config)
			if err != nil {
				return opOut{}, err
			}
			_ = h.deps.Clusters.SaveServiceConfig(in.Cluster, in.Service, in.Config)
			return toOpOut(op), nil
		}))

	// service_scale
	mcp.AddTool(s, &mcp.Tool{
		Name:        "service_scale",
		Description: "Scale a service to the given replica count. replicas=0 stops the service and requires confirm=true.",
	}, audited(h, "service_scale",
		func(in serviceScaleIn) map[string]string {
			return map[string]string{"cluster": in.Cluster, "service": in.Service, "replicas": strconv.FormatUint(in.Replicas, 10)}
		},
		func(ctx context.Context, in serviceScaleIn) (opOut, error) {
			if in.Replicas == 0 && !in.Confirm {
				return opOut{}, fmt.Errorf("scaling to 0 stops the service; restate this impact to the user and set confirm=true to proceed")
			}
			cli, err := h.workerClient(in.Cluster)
			if err != nil {
				return opOut{}, err
			}
			op, err := cli.Scale(ctx, in.Service, in.Replicas)
			if err != nil {
				return opOut{}, err
			}
			return toOpOut(op), nil
		}))

	// service_restart
	mcp.AddTool(s, &mcp.Tool{
		Name:        "service_restart",
		Description: "Force swarm to re-create a service's tasks (rolling restart). Use for stuck tasks or to reload the image tag digest.",
	}, audited(h, "service_restart",
		func(in clusterServiceNameIn) map[string]string {
			return map[string]string{"cluster": in.Cluster, "service": in.Service}
		},
		func(ctx context.Context, in clusterServiceNameIn) (opOut, error) {
			cli, err := h.workerClient(in.Cluster)
			if err != nil {
				return opOut{}, err
			}
			op, err := cli.Restart(ctx, in.Service)
			if err != nil {
				return opOut{}, err
			}
			return toOpOut(op), nil
		}))

	// service_remove
	mcp.AddTool(s, &mcp.Tool{
		Name:        "service_remove",
		Description: "Delete a service and its tasks. Destructive: requires confirm=true.",
	}, audited(h, "service_remove",
		func(in serviceRemoveIn) map[string]string {
			return map[string]string{"cluster": in.Cluster, "service": in.Service}
		},
		func(ctx context.Context, in serviceRemoveIn) (opOut, error) {
			if !in.Confirm {
				return opOut{}, fmt.Errorf("removing service %q on cluster %q deletes it and all its tasks; "+
					"restate this impact to the user and set confirm=true to proceed", in.Service, in.Cluster)
			}
			cli, err := h.workerClient(in.Cluster)
			if err != nil {
				return opOut{}, err
			}
			op, err := cli.Remove(ctx, in.Service)
			if err != nil {
				return opOut{}, err
			}
			_ = h.deps.Clusters.DeleteServiceConfig(in.Cluster, in.Service)
			return toOpOut(op), nil
		}))

	// service_logs
	mcp.AddTool(s, &mcp.Tool{
		Name:        "service_logs",
		Description: "Recent log lines of a service (all tasks aggregated, one snapshot — not a stream).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in serviceLogsIn) (*mcp.CallToolResult, serviceLogsOut, error) {
		cli, err := h.workerClient(in.Cluster)
		if err != nil {
			return nil, serviceLogsOut{}, err
		}
		tail := in.Tail
		if tail <= 0 {
			tail = 200
		}
		if tail > 1000 {
			tail = 1000
		}
		var lines []string
		err = cli.StreamLogs(ctx, in.Service, false, tail, in.Since, func(ll workerproxy.LogLine) bool {
			lines = append(lines, fmt.Sprintf("[%s/%s] %s", ll.TS, ll.Stream, ll.Line))
			return true
		})
		if err != nil {
			return nil, serviceLogsOut{}, err
		}
		if lines == nil {
			lines = []string{"(no logs in range)"}
		}
		return nil, serviceLogsOut{Lines: truncStr(joinLines(lines))}, nil
	})

	// service_operation
	mcp.AddTool(s, &mcp.Tool{
		Name:        "service_operation",
		Description: "Poll an async lifecycle operation (deploy/update/scale/restart/remove) until it reaches done|failed|partial|canceled.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in serviceOpIn) (*mcp.CallToolResult, opOut, error) {
		cli, err := h.workerClient(in.Cluster)
		if err != nil {
			return nil, opOut{}, err
		}
		op, err := cli.Operation(ctx, in.OpID)
		if err != nil {
			return nil, opOut{}, err
		}
		return nil, toOpOut(op), nil
	})

	// service_audit
	mcp.AddTool(s, &mcp.Tool{
		Name:        "service_audit",
		Description: "Recent operation audit of the cluster's worker (deploy/update/scale/restart/remove/exec), newest first.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in serviceAuditIn) (*mcp.CallToolResult, serviceAuditOut, error) {
		cli, err := h.workerClient(in.Cluster)
		if err != nil {
			return nil, serviceAuditOut{}, err
		}
		limit := in.Limit
		if limit <= 0 {
			limit = 20
		}
		raw, err := cli.Audit(ctx, "", limit)
		if err != nil {
			return nil, serviceAuditOut{}, err
		}
		return nil, serviceAuditOut{Items: truncJSON(raw)}, nil
	})
}

// workerClient 按集群名取 Worker 客户端（service/diag 工具共用第一步）。
func (h *Handler) workerClient(clusterName string) (*workerproxy.Client, error) {
	if h.deps.Clusters == nil {
		return nil, fmt.Errorf("cluster service not initialized")
	}
	return h.deps.Clusters.WorkerClient(clusterName)
}

// truncJSON JSON 体积防线：超限时截断并打标。
func truncJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) <= maxResultBytes {
		return raw
	}
	return json.RawMessage(fmt.Sprintf("%s…[truncated at %d bytes]", string(raw[:maxResultBytes]), maxResultBytes))
}

// formatTime 时间统一 RFC3339（空值空串）。
func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
