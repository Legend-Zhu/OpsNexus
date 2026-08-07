package mcp

import (
	"context"
	"fmt"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/config"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/docker"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/monitor"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/orchestrator"

	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ---- input/output types for the typed handlers ----

type nameIn struct {
	Name string `json:"name" description:"Service name"`
}

type labelIn struct {
	Label string `json:"label,omitempty" description:"Filter services by label, e.g. app=web"`
}

type configIn struct {
	Config string `json:"config" description:"Service config in YAML or JSON (see design doc §4.2)"`
}

type updateIn struct {
	Name   string `json:"name" description:"Service name to update"`
	Config string `json:"config" description:"New service config in YAML or JSON"`
}

type scaleIn struct {
	Name     string `json:"name" description:"Service name"`
	Replicas uint64 `json:"replicas" description:"Target replica count (0 stops the service; requires confirm=true)"`
	Confirm  bool   `json:"confirm,omitempty" description:"Must be true when replicas=0"`
}

type removeIn struct {
	Name    string `json:"name" description:"Service name to delete"`
	Confirm bool   `json:"confirm" description:"Must be true (deletes the service and its tasks)"`
}

type logsIn struct {
	Name string `json:"name" description:"Service name"`
	Tail int    `json:"tail,omitempty" description:"Number of recent lines to return (default 200)"`
}

type eventsIn struct {
	Service string `json:"service,omitempty" description:"Filter by service name"`
	Type    string `json:"type,omitempty" description:"Filter by event type (port_down|http_unhealthy|log_match|resource_over|resource_recovered)"`
	Limit   int    `json:"limit,omitempty" description:"Max events to return (default 50)"`
}

type opIn struct {
	ID string `json:"id" description:"Operation id"`
}

type nodeIn struct {
	ID string `json:"id" description:"Node id"`
}

type opOut struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Service   string `json:"service"`
	Status    string `json:"status"`
	ServiceID string `json:"serviceId,omitempty"`
	Replicas  uint64 `json:"replicas,omitempty"`
	Error     string `json:"error,omitempty"`
}

type serviceSummary struct {
	Name    string `json:"name"`
	ID      string `json:"id"`
	Mode    string `json:"mode"`
	Image   string `json:"image"`
	Running uint64 `json:"running"`
	Desired uint64 `json:"desired"`
}

type detailOut struct {
	Service docker.Service `json:"service"`
	Tasks   []docker.Task  `json:"tasks"`
	Running int            `json:"running"`
	Desired int            `json:"desired"`
	Healthy int            `json:"healthy"`
}

type eventsOut struct {
	Events []monitorEvent `json:"events"`
}

type logsOut struct {
	Lines []string `json:"lines"`
}

type nodesOut struct {
	Nodes []nodeSummary `json:"nodes"`
}

type nodeOut struct {
	Node nodeSummary `json:"node"`
}

type nodeSummary struct {
	ID        string `json:"id"`
	Hostname  string `json:"hostname"`
	Role      string `json:"role"`
	State     string `json:"state"`
	Reachable string `json:"reachable,omitempty"`
	Leader    bool   `json:"leader,omitempty"`
}

// monitorEvent mirrors monitor.Event without importing monitor in tool types
// (keeps the JSON wire shape identical).
type monitorEvent struct {
	ID      string `json:"id"`
	TS      string `json:"ts"`
	Service string `json:"service"`
	Type    string `json:"type"`
	Level   string `json:"level"`
	Msg     string `json:"msg"`
	Detail  string `json:"detail,omitempty"`
}

// registerTools registers the agent-facing tools.
func (h *Handler) registerTools(s *mcp.Server) {
	// list_services
	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_services",
		Description: "List swarm services with replica counts. Optional label filter, e.g. app=web.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in labelIn) (*mcp.CallToolResult, []serviceSummary, error) {
		svcs, err := h.orch.ListServices(context.Background(), in.Label)
		if err != nil {
			return nil, nil, err
		}
		out := make([]serviceSummary, 0, len(svcs))
		for _, s := range svcs {
			out = append(out, serviceSummary{
				Name:    s.Spec.Name,
				ID:      s.ID,
				Mode:    modeString(s.Spec.Mode),
				Image:   s.Spec.TaskTemplate.ContainerSpec.Image,
				Running: s.ServiceStatus.RunningTasks,
				Desired: s.ServiceStatus.DesiredTasks,
			})
		}
		return nil, out, nil
	})

	// get_service
	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_service",
		Description: "Get a service's full detail: spec, tasks, running/desired/healthy counts.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in nameIn) (*mcp.CallToolResult, detailOut, error) {
		d, err := h.orch.Inspect(context.Background(), in.Name)
		if err != nil {
			return nil, detailOut{}, err
		}
		return nil, detailOut{Service: d.Service, Tasks: d.Tasks, Running: d.Running, Desired: d.Desired, Healthy: d.Healthy}, nil
	})

	// deploy_service
	mcp.AddTool(s, &mcp.Tool{
		Name:        "deploy_service",
		Description: "Deploy a new service from a config (YAML/JSON). Returns a pending operation; poll get_operation for convergence.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in configIn) (*mcp.CallToolResult, opOut, error) {
		cfg, err := parseConfig(in.Config)
		if err != nil {
			return nil, opOut{}, err
		}
		op, err := h.orch.Deploy(context.Background(), cfg)
		if err != nil {
			return nil, opOut{}, err
		}
		return nil, toOpOut(op), nil
	})

	// update_service
	mcp.AddTool(s, &mcp.Tool{
		Name:        "update_service",
		Description: "Update an existing service from a new config (rolling update).",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in updateIn) (*mcp.CallToolResult, opOut, error) {
		cfg, err := parseConfig(in.Config)
		if err != nil {
			return nil, opOut{}, err
		}
		op, err := h.orch.Update(context.Background(), in.Name, cfg)
		if err != nil {
			return nil, opOut{}, err
		}
		return nil, toOpOut(op), nil
	})

	// scale_service
	mcp.AddTool(s, &mcp.Tool{
		Name:        "scale_service",
		Description: "Scale a service to the given replica count. replicas=0 stops the service and requires confirm=true.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in scaleIn) (*mcp.CallToolResult, opOut, error) {
		if in.Replicas == 0 && !in.Confirm {
			return nil, opOut{}, fmt.Errorf("scaling to 0 stops the service; set confirm=true to proceed")
		}
		op, err := h.orch.Scale(context.Background(), in.Name, in.Replicas)
		if err != nil {
			return nil, opOut{}, err
		}
		return nil, toOpOut(op), nil
	})

	// restart_service
	mcp.AddTool(s, &mcp.Tool{
		Name:        "restart_service",
		Description: "Force swarm to re-create a service's tasks (docker service update --force).",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in nameIn) (*mcp.CallToolResult, opOut, error) {
		op, err := h.orch.Restart(context.Background(), in.Name)
		if err != nil {
			return nil, opOut{}, err
		}
		return nil, toOpOut(op), nil
	})

	// remove_service (destructive: requires confirm)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "remove_service",
		Description: "Delete a service and its tasks. Destructive: requires confirm=true.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in removeIn) (*mcp.CallToolResult, opOut, error) {
		if !in.Confirm {
			return nil, opOut{}, fmt.Errorf("removing a service is destructive; set confirm=true to proceed")
		}
		op, err := h.orch.Remove(context.Background(), in.Name)
		if err != nil {
			return nil, opOut{}, err
		}
		return nil, toOpOut(op), nil
	})

	// get_service_logs
	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_service_logs",
		Description: "Return the most recent log lines of a service (all tasks aggregated).",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in logsIn) (*mcp.CallToolResult, logsOut, error) {
		tail := in.Tail
		if tail <= 0 {
			tail = 200
		}
		lines, err := h.recentLogs(context.Background(), in.Name, tail)
		if err != nil {
			return nil, logsOut{}, err
		}
		return nil, logsOut{Lines: lines}, nil
	})

	// get_events
	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_events",
		Description: "Query recent monitoring events (port_down, http_unhealthy, log_match, resource_over, resource_recovered).",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in eventsIn) (*mcp.CallToolResult, eventsOut, error) {
		limit := in.Limit
		if limit <= 0 {
			limit = 50
		}
		evs := h.mon.Events(in.Service, monitorEventType(in.Type), 0, limit)
		out := eventsOut{Events: make([]monitorEvent, 0, len(evs))}
		for _, e := range evs {
			out.Events = append(out.Events, monitorEvent{
				ID: e.ID, TS: e.TS.UTC().Format("2006-01-02T15:04:05Z"),
				Service: e.Service, Type: string(e.Type), Level: string(e.Level),
				Msg: e.Msg, Detail: e.Detail,
			})
		}
		return nil, out, nil
	})

	// get_operation
	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_operation",
		Description: "Poll the status of a lifecycle operation (deploy/update/scale/restart/remove).",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in opIn) (*mcp.CallToolResult, opOut, error) {
		op, ok := h.orch.GetOperation(in.ID)
		if !ok {
			return nil, opOut{}, fmt.Errorf("operation %q not found", in.ID)
		}
		return nil, toOpOut(&op), nil
	})

	// list_nodes
	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_nodes",
		Description: "List swarm nodes with role and health status.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, nodesOut, error) {
		nodes, err := h.cli.ListNodes(context.Background(), nil)
		if err != nil {
			return nil, nodesOut{}, err
		}
		out := nodesOut{Nodes: make([]nodeSummary, 0, len(nodes))}
		for _, n := range nodes {
			out.Nodes = append(out.Nodes, toNodeSummary(n))
		}
		return nil, out, nil
	})

	// get_node
	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_node",
		Description: "Get a single swarm node by id or hostname.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in nodeIn) (*mcp.CallToolResult, nodeOut, error) {
		nodes, err := h.cli.ListNodes(context.Background(), nil)
		if err != nil {
			return nil, nodeOut{}, err
		}
		for _, n := range nodes {
			if n.ID == in.ID || n.Description.Hostname == in.ID {
				return nil, nodeOut{Node: toNodeSummary(n)}, nil
			}
		}
		return nil, nodeOut{}, fmt.Errorf("node %q not found", in.ID)
	})

	// get_self
	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_self",
		Description: "Report this Worker's own node identity and swarm role (nodeId, role, leader, swarmManager).",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, orchestrator.SelfInfo, error) {
		si, err := h.orch.Self(context.Background())
		if err != nil {
			return nil, orchestrator.SelfInfo{}, err
		}
		return nil, si, nil
	})
}

// ---- helpers ----

// parseConfig parses YAML/JSON config (defaults + validation applied).
func parseConfig(s string) (*config.Config, error) {
	cfg, err := config.LoadBytes("deploy.yaml", []byte(s))
	if err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	return cfg, nil
}

func toOpOut(op *orchestrator.Operation) opOut {
	return opOut{
		ID: op.ID, Type: string(op.Type), Service: op.Service,
		Status: string(op.Status), ServiceID: op.ServiceID,
		Replicas: op.Replicas, Error: op.Error,
	}
}

func modeString(m docker.ServiceMode) string {
	switch {
	case m.Replicated != nil:
		return "replicated"
	case m.Global != nil:
		return "global"
	}
	return ""
}

func toNodeSummary(n docker.Node) nodeSummary {
	ns := nodeSummary{
		ID: n.ID, Hostname: n.Description.Hostname,
		Role: n.Spec.Role, State: n.Status.State,
	}
	if n.ManagerStatus != nil {
		ns.Reachable = n.ManagerStatus.Reachability
		ns.Leader = n.ManagerStatus.Leader
	}
	return ns
}

func monitorEventType(s string) monitor.EventType {
	switch s {
	case "port_down":
		return monitor.EventPortDown
	case "http_unhealthy":
		return monitor.EventHTTPUnhealthy
	case "log_match":
		return monitor.EventLogMatch
	case "resource_over":
		return monitor.EventResourceOver
	case "resource_recovered":
		return monitor.EventResourceRecover
	}
	return monitor.EventType("")
}
