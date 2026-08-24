package mcp

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/audit"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/docker"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/orchestrator"

	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func joinArgs(cmd []string) string { return strings.Join(cmd, " ") }

func itoa(n int) string { return strconv.Itoa(n) }

// ---- get_resource_usage ----

type resIn struct {
	Name string `json:"name" description:"Service name"`
}

type containerRes struct {
	ContainerID string  `json:"containerId"`
	Node        string  `json:"node"`
	Slot        int     `json:"slot"`
	CPUPercent  float64 `json:"cpuPercent"`
	MemPercent  float64 `json:"memPercent"`
	MemUsage    uint64  `json:"memUsageBytes"`
	MemLimit    uint64  `json:"memLimitBytes"`
}

type resOut struct {
	Containers []containerRes `json:"containers"`
	CPUPercent float64        `json:"cpuPercent"` // aggregate mean across replicas
	MemPercent float64        `json:"memPercent"`
	Replicas   int            `json:"replicas"`
}

// ---- exec_in_container ----

type execIn struct {
	Service string   `json:"service" description:"Service name to exec into"`
	Command []string `json:"command" description:"Command and args, e.g. [\"ls\", \"/etc/nginx\"]"`
	Slot    int      `json:"slot,omitempty" description:"Task slot to target (1-based). Default: first running task"`
	Confirm bool     `json:"confirm,omitempty" description:"Must be true: exec runs arbitrary commands inside the container"`
}

type execOut struct {
	ContainerID string `json:"containerId"`
	Node        string `json:"node"`
	Slot        int    `json:"slot"`
	ExitCode    int    `json:"exitCode"`
	Output      string `json:"output"`
}

// ---- exec_host_command ----

type hostIn struct {
	Command string `json:"command" description:"Host command to run, e.g. \"df -h\""`
	Node    string `json:"node,omitempty" description:"Target node (hostname or node id). Default: all nodes"`
	Confirm bool   `json:"confirm" description:"Must be true: runs a command on the host OS (nsenter)"`
}

type hostOut struct {
	Results []hostResult `json:"results"`
}

type hostResult struct {
	Node      string `json:"node"`
	ExitCode  int    `json:"exitCode"`
	Output    string `json:"output"`
	ElapsedMS int64  `json:"elapsedMs"`
}

// registerToolsMetrics adds the resource-metrics, exec, and host-exec tools.
func (h *Handler) registerToolsMetrics(s *mcp.Server) {
	// get_resource_usage (cross-node aggregated)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_resource_usage",
		Description: "Query real-time CPU and memory usage of a service's task containers across all swarm nodes (percent of limits; CPU is a ~1s delta rate).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in resIn) (*mcp.CallToolResult, resOut, error) {
		out, err := h.resourceUsage(ctx, in.Name)
		if err != nil {
			return nil, resOut{}, err
		}
		return nil, out, nil
	})

	// exec_in_container (routes to the node running the task)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "exec_in_container",
		Description: "Run a command inside a service's running container, routed to whichever node runs the task. Dangerous: executes arbitrary commands as the container user; requires confirm=true.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in execIn) (*mcp.CallToolResult, execOut, error) {
		if !in.Confirm {
			return nil, execOut{}, fmt.Errorf("exec runs arbitrary commands inside the container; set confirm=true to proceed")
		}
		if len(in.Command) == 0 {
			return nil, execOut{}, fmt.Errorf("command is required")
		}
		out, err := h.execInContainer(ctx, in.Service, in.Slot, in.Command)
		if err != nil {
			h.auditAction(ctx, audit.ActionExec, joinArgs(in.Command), in.Service, false, err.Error())
			return nil, execOut{}, err
		}
		h.auditAction(ctx, audit.ActionExec, joinArgs(in.Command), in.Service, true, "slot="+itoa(out.Slot)+" node="+out.Node)
		return nil, out, nil
	})

	// exec_host_command (runs on the host OS via nsenter)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "exec_host_command",
		Description: "Run a command on a swarm node's host OS (nsenter, privileged worker). Targets all nodes by default, or a specific node. Subject to the command blacklist/whitelist policy; requires confirm=true.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in hostIn) (*mcp.CallToolResult, hostOut, error) {
		if !in.Confirm {
			return nil, hostOut{}, fmt.Errorf("host command execution is powerful; set confirm=true to proceed")
		}
		if in.Command == "" {
			return nil, hostOut{}, fmt.Errorf("command is required")
		}
		out, err := h.execHost(ctx, in.Command, in.Node)
		if err != nil {
			return nil, hostOut{}, err
		}
		h.auditAction(ctx, audit.ActionHostExec, in.Command, in.Node, true, "")
		return nil, out, nil
	})
}

// auditAction records a command-execution action in the audit log. The actor
// is taken from the request context (token name@ip via the auth middleware,
// or "stdio" for local stdio sessions).
func (h *Handler) auditAction(ctx context.Context, action audit.Action, command, target string, ok bool, detail string) {
	if h.audit == nil {
		return
	}
	cmd := command
	if len(cmd) > 200 {
		cmd = cmd[:200] + "..."
	}
	h.audit.Add(audit.Entry{
		Actor:   audit.ActorFromContext(ctx),
		Action:  action,
		Command: cmd,
		Target:  target,
		OK:      ok,
		Detail:  detail,
	})
}

// resourceUsage samples every running task container across all nodes: the
// manager queries each node's local worker for its stats, then aggregates.
func (h *Handler) resourceUsage(ctx context.Context, service string) (resOut, error) {
	tasks, err := h.runningTasks(ctx, service)
	if err != nil {
		return resOut{}, err
	}
	if len(tasks) == 0 {
		return resOut{}, fmt.Errorf("no running tasks for service %q", service)
	}

	// map node -> []task for this service
	byNode := map[string][]docker.Task{}
	for _, t := range tasks {
		byNode[t.NodeID] = append(byNode[t.NodeID], t)
	}

	// local node containers are read directly from this daemon
	selfID, _ := h.orch.SelfNodeID(ctx)

	out := resOut{Containers: make([]containerRes, 0, len(tasks))}
	var cpuSum, memSum float64
	replicas := 0

	for nodeID, nodeTasks := range byNode {
		var perNode []containerRes
		if nodeID == selfID {
			perNode = h.localContainerRes(ctx, nodeTasks)
		} else {
			perNode = h.remoteContainerRes(ctx, nodeID, nodeTasks)
		}
		out.Containers = append(out.Containers, perNode...)
		for _, c := range perNode {
			cpuSum += c.CPUPercent
			memSum += c.MemPercent
			replicas++
		}
	}
	if replicas == 0 {
		return resOut{}, fmt.Errorf("could not read stats for any task of %q (node workers may be down)", service)
	}
	out.Replicas = replicas
	out.CPUPercent = round2(cpuSum / float64(replicas))
	out.MemPercent = round2(memSum / float64(replicas))
	return out, nil
}

// localContainerRes reads stats for tasks running on this node.
func (h *Handler) localContainerRes(ctx context.Context, tasks []docker.Task) []containerRes {
	out := make([]containerRes, 0, len(tasks))
	for _, t := range tasks {
		cid := t.Status.ContainerStatus.ContainerID
		if cid == "" {
			continue
		}
		first, err := h.cli.ContainerStats(ctx, cid)
		if err != nil {
			continue
		}
		time.Sleep(time.Second)
		second, err := h.cli.ContainerStats(ctx, cid)
		if err != nil {
			continue
		}
		out = append(out, containerRes{
			ContainerID: cid,
			Node:        selfHostname(ctx, h),
			Slot:        t.Slot,
			CPUPercent:  round2(cpuDeltaPercent(first, second)),
			MemPercent:  round2(memPercentOf(second)),
			MemUsage:    second.MemoryStats.Usage,
			MemLimit:    second.MemoryStats.Limit,
		})
	}
	return out
}

// remoteContainerRes asks the node's local worker for the stats of its tasks.
func (h *Handler) remoteContainerRes(ctx context.Context, nodeID string, tasks []docker.Task) []containerRes {
	addrs, err := h.orch.NodeAddrs(ctx)
	if err != nil {
		return nil
	}
	addr, ok := addrs[nodeID]
	if !ok {
		return nil
	}
	nc := h.orch.NodeClientByAddr(addr)
	stats, err := nc.Stats(ctx)
	if err != nil {
		return nil
	}
	// filter to this service's containers on that node
	taskSet := map[string]bool{}
	slotOf := map[string]int{}
	for _, t := range tasks {
		taskSet[t.Status.ContainerStatus.ContainerID] = true
		slotOf[t.Status.ContainerStatus.ContainerID] = t.Slot
	}
	out := make([]containerRes, 0, len(stats.Containers))
	for _, c := range stats.Containers {
		if taskSet[c.ContainerID] {
			out = append(out, containerRes{
				ContainerID: c.ContainerID,
				Node:        stats.Node,
				Slot:        slotOf[c.ContainerID],
				CPUPercent:  c.CPUPercent,
				MemPercent:  c.MemPercent,
				MemUsage:    c.MemUsage,
				MemLimit:    c.MemLimit,
			})
		}
	}
	return out
}

// execInContainer routes the exec to the node running the task.
func (h *Handler) execInContainer(ctx context.Context, service string, slot int, cmd []string) (execOut, error) {
	tasks, err := h.runningTasks(ctx, service)
	if err != nil {
		return execOut{}, err
	}
	var target docker.Task
	if slot > 0 {
		for _, t := range tasks {
			if t.Slot == slot {
				target = t
				break
			}
		}
		if target.Status.ContainerStatus.ContainerID == "" {
			return execOut{}, fmt.Errorf("service %q has no running task on slot %d", service, slot)
		}
	} else {
		target = tasks[0]
	}
	cid := target.Status.ContainerStatus.ContainerID

	selfID, _ := h.orch.SelfNodeID(ctx)
	if target.NodeID == selfID {
		// local exec via docker engine API
		execID, err := h.cli.ContainerExecCreate(ctx, cid, cmd)
		if err != nil {
			return execOut{}, err
		}
		rc, err := h.cli.ExecStart(ctx, execID)
		if err != nil {
			return execOut{}, err
		}
		defer rc.Close()
		data, _ := io.ReadAll(rc)
		ei, err := h.cli.ExecInspect(ctx, execID)
		if err != nil {
			return execOut{}, err
		}
		return execOut{
			ContainerID: cid,
			Node:        selfHostname(ctx, h),
			Slot:        target.Slot,
			ExitCode:    ei.ExitCode,
			Output:      string(data),
		}, nil
	}

	// remote exec via the node's local worker
	nc, err := h.nodeClientForTask(ctx, target)
	if err != nil {
		return execOut{}, err
	}
	res, err := nc.Exec(ctx, cid, "", 0, cmd)
	if err != nil {
		return execOut{}, err
	}
	return execOut{
		ContainerID: res.ContainerID,
		Node:        nodeNameFor(ctx, h, target.NodeID),
		Slot:        target.Slot,
		ExitCode:    res.ExitCode,
		Output:      res.Output,
	}, nil
}

// execHost runs a command on the host of one or all nodes via their local
// workers.
func (h *Handler) execHost(ctx context.Context, command, node string) (hostOut, error) {
	addrs, err := h.orch.NodeAddrs(ctx)
	if err != nil {
		return hostOut{}, err
	}
	if len(addrs) == 0 {
		return hostOut{}, fmt.Errorf("no ready swarm nodes")
	}

	out := hostOut{Results: make([]hostResult, 0, len(addrs))}
	for nodeID, addr := range addrs {
		if node != "" && nodeID != node && addr != node {
			continue
		}
		res, err := h.orch.NodeClientByAddr(addr).Host(ctx, command)
		if err != nil {
			out.Results = append(out.Results, hostResult{Node: addr, ExitCode: -1, Output: "node worker unreachable: " + err.Error()})
			continue
		}
		out.Results = append(out.Results, hostResult{
			Node: res.Node, ExitCode: res.ExitCode, Output: res.Output, ElapsedMS: res.ElapsedMS,
		})
	}
	if len(out.Results) == 0 {
		return hostOut{}, fmt.Errorf("node %q not found", node)
	}
	return out, nil
}

// ---- helpers ----

// runningTasks returns the running task list of a service.
func (h *Handler) runningTasks(ctx context.Context, service string) ([]docker.Task, error) {
	svc, err := h.cli.GetService(ctx, service)
	if err != nil {
		return nil, err
	}
	tasks, err := h.cli.ServiceTasks(ctx, svc.ID)
	if err != nil {
		return nil, err
	}
	var running []docker.Task
	for _, t := range tasks {
		if t.Status.State == "running" && t.Status.ContainerStatus.ContainerID != "" {
			running = append(running, t)
		}
	}
	return running, nil
}

func (h *Handler) nodeClientForTask(ctx context.Context, task docker.Task) (*orchestrator.NodeClient, error) {
	addrs, err := h.orch.NodeAddrs(ctx)
	if err != nil {
		return nil, err
	}
	addr, ok := addrs[task.NodeID]
	if !ok {
		return nil, fmt.Errorf("no address for node %s", task.NodeID)
	}
	return h.orch.NodeClientByAddr(addr), nil
}

func nodeNameFor(ctx context.Context, h *Handler, nodeID string) string {
	nodes, err := h.cli.ListNodes(ctx, nil)
	if err != nil {
		return nodeID
	}
	for _, n := range nodes {
		if n.ID == nodeID {
			return n.Description.Hostname
		}
	}
	return nodeID
}

func selfHostname(ctx context.Context, h *Handler) string {
	si, err := h.orch.Self(ctx)
	if err != nil {
		return "self"
	}
	return si.Hostname
}

// cpuDeltaPercent computes the CPU usage percentage between two stats
// snapshots (docker stats algorithm, normalized by core count).
func cpuDeltaPercent(prev, cur docker.Stats) float64 {
	dt := cur.CPUStats.SystemCPUUsage - prev.CPUStats.SystemCPUUsage
	if dt == 0 {
		return 0
	}
	dtCPU := cur.CPUStats.CPUUsage.TotalUsage - prev.CPUStats.CPUUsage.TotalUsage
	pct := float64(dtCPU) / float64(dt) * 100
	cores := float64(cur.CPUStats.OnlineCPUs)
	if cores > 0 {
		pct *= cores
	}
	return pct
}

// memPercentOf computes memory usage percent, excluding page cache.
func memPercentOf(st docker.Stats) float64 {
	if st.MemoryStats.Limit == 0 {
		return 0
	}
	usage := st.MemoryStats.Usage
	if v, ok := st.MemoryStats.Stats["inactive_file"]; ok && usage > v {
		usage -= v
	}
	return float64(usage) / float64(st.MemoryStats.Limit) * 100
}

func round2(f float64) float64 {
	return float64(int64(f*100+0.5)) / 100
}
