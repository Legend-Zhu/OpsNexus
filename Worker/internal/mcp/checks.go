package mcp

import (
	"context"
	"fmt"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/orchestrator"

	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Ad-hoc node probes: one-shot port/HTTP checks and host process discovery,
// fanned out to node workers via the local API (node empty = all ready nodes;
// a single unreachable node degrades to an error entry, like exec_host_command).
// All read-only — no confirm, no audit.

// ---- check_port ----

type checkPortIn struct {
	Host    string `json:"host" description:"Target host or IP to probe. For middleware bound to a node's own host (MySQL, Redis...), use the node IP — 127.0.0.1 would point at the worker container itself"`
	Port    int    `json:"port" description:"Target TCP port, e.g. 3306"`
	Node    string `json:"node,omitempty" description:"Probe from this node only (hostname, node id, or addr). Default: all ready nodes"`
	Timeout string `json:"timeout,omitempty" description:"Dial timeout, e.g. \"3s\" (default 3s, max 10s)"`
}

type checkPortOut struct {
	Results []orchestrator.PortCheckResult `json:"results"`
}

// ---- check_http ----

type checkHTTPIn struct {
	URL            string            `json:"url" description:"Full URL to probe, e.g. http://10.0.0.1:8080/healthz. For host-bound services use the node IP, not 127.0.0.1"`
	Method         string            `json:"method,omitempty" description:"HTTP method (default GET)"`
	Headers        map[string]string `json:"headers,omitempty" description:"Extra request headers"`
	ExpectedStatus []int             `json:"expectedStatus,omitempty" description:"Acceptable status codes. Default: any 2xx"`
	ExpectedBody   string            `json:"expectedBody,omitempty" description:"Regex the response body must match"`
	Timeout        string            `json:"timeout,omitempty" description:"Request timeout, e.g. \"3s\" (default 3s, max 10s)"`
	Node           string            `json:"node,omitempty" description:"Probe from this node only (hostname, node id, or addr). Default: all ready nodes"`
}

type checkHTTPOut struct {
	Results []orchestrator.HTTPCheckResult `json:"results"`
}

// ---- list_host_processes ----

type hostProcsIn struct {
	Filter string `json:"filter,omitempty" description:"Case-insensitive substring matched against process name/cmdline, e.g. \"java\" or \"redis-server\". Default: no filter"`
	Node   string `json:"node,omitempty" description:"List processes on this node only (hostname, node id, or addr). Default: all ready nodes"`
	Top    string `json:"top,omitempty" description:"Sort by: cpu (default) | mem"`
	Limit  int    `json:"limit,omitempty" description:"Max processes per node (default 100, max 500)"`
}

type hostProcsResult struct {
	Node      string                     `json:"node"`
	Total     int                        `json:"total"`
	Processes []orchestrator.ProcessInfo `json:"processes,omitempty"`
	Error     string                     `json:"error,omitempty"`
}

type hostProcsOut struct {
	Results []hostProcsResult `json:"results"`
}

// registerToolsChecks adds the ad-hoc probe tools.
func (h *Handler) registerToolsChecks(s *mcp.Server) {
	// check_port (TCP connectivity to any host:port, per node)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "check_port",
		Description: "One-shot TCP connectivity probe to any host:port, run from swarm nodes (all ready nodes by default, or a specific one). Read-only. Typical use: is host-installed middleware (MySQL, Redis, ...) listening? Probes originate from the node's worker container network — target the node IP, not 127.0.0.1.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in checkPortIn) (*mcp.CallToolResult, checkPortOut, error) {
		if in.Host == "" {
			return nil, checkPortOut{}, fmt.Errorf("host is required")
		}
		if in.Port <= 0 || in.Port > 65535 {
			return nil, checkPortOut{}, fmt.Errorf("port must be within 1-65535")
		}
		out, err := h.checkPort(context.Background(), in)
		if err != nil {
			return nil, checkPortOut{}, err
		}
		return nil, out, nil
	})

	// check_http (HTTP endpoint probe, per node)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "check_http",
		Description: "One-shot HTTP probe of any URL, run from swarm nodes (all ready nodes by default, or a specific one): status check (default any 2xx) plus optional body regex. Read-only. Probes originate from the node's worker container network — target the node IP, not 127.0.0.1.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in checkHTTPIn) (*mcp.CallToolResult, checkHTTPOut, error) {
		if in.URL == "" {
			return nil, checkHTTPOut{}, fmt.Errorf("url is required")
		}
		out, err := h.checkHTTP(context.Background(), in)
		if err != nil {
			return nil, checkHTTPOut{}, err
		}
		return nil, out, nil
	})

	// list_host_processes (host process discovery, per node)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_host_processes",
		Description: "List host OS processes on swarm nodes (all ready nodes by default, or a specific one), optionally filtered by a name/cmdline substring — find host-installed middleware or Java processes (e.g. filter=\"java\"). Returns pid/name/cmdline/state/memKb/cpuPercent sorted by cpu (or mem). Read-only.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in hostProcsIn) (*mcp.CallToolResult, hostProcsOut, error) {
		out, err := h.hostProcesses(context.Background(), in)
		if err != nil {
			return nil, hostProcsOut{}, err
		}
		return nil, out, nil
	})
}

// checkPort fans the TCP probe out to the target nodes.
func (h *Handler) checkPort(ctx context.Context, in checkPortIn) (checkPortOut, error) {
	addrs, err := h.targetAddrs(ctx, in.Node)
	if err != nil {
		return checkPortOut{}, err
	}
	out := checkPortOut{Results: make([]orchestrator.PortCheckResult, 0, len(addrs))}
	for _, addr := range addrs {
		res, err := h.orch.NodeClientByAddr(addr).CheckPort(ctx, in.Host, itoa(in.Port), in.Timeout)
		if err != nil {
			out.Results = append(out.Results, orchestrator.PortCheckResult{
				Node: addr, Host: in.Host, Port: itoa(in.Port),
				Error: "node worker unreachable: " + err.Error(),
			})
			continue
		}
		out.Results = append(out.Results, res)
	}
	return out, nil
}

// checkHTTP fans the HTTP probe out to the target nodes.
func (h *Handler) checkHTTP(ctx context.Context, in checkHTTPIn) (checkHTTPOut, error) {
	addrs, err := h.targetAddrs(ctx, in.Node)
	if err != nil {
		return checkHTTPOut{}, err
	}
	req := orchestrator.HTTPCheckRequest{
		URL:            in.URL,
		Method:         in.Method,
		Headers:        in.Headers,
		ExpectedStatus: in.ExpectedStatus,
		ExpectedBody:   in.ExpectedBody,
		Timeout:        in.Timeout,
	}
	out := checkHTTPOut{Results: make([]orchestrator.HTTPCheckResult, 0, len(addrs))}
	for _, addr := range addrs {
		res, err := h.orch.NodeClientByAddr(addr).CheckHTTP(ctx, req)
		if err != nil {
			out.Results = append(out.Results, orchestrator.HTTPCheckResult{
				Node: addr, URL: in.URL,
				Error: "node worker unreachable: " + err.Error(),
			})
			continue
		}
		out.Results = append(out.Results, res)
	}
	return out, nil
}

// hostProcesses fans the process listing out to the target nodes.
func (h *Handler) hostProcesses(ctx context.Context, in hostProcsIn) (hostProcsOut, error) {
	addrs, err := h.targetAddrs(ctx, in.Node)
	if err != nil {
		return hostProcsOut{}, err
	}
	limit := ""
	if in.Limit > 0 {
		limit = itoa(in.Limit)
	}
	out := hostProcsOut{Results: make([]hostProcsResult, 0, len(addrs))}
	for _, addr := range addrs {
		res, err := h.orch.NodeClientByAddr(addr).Processes(ctx, in.Top, limit, in.Filter)
		if err != nil {
			out.Results = append(out.Results, hostProcsResult{
				Node: addr, Error: "node worker unreachable: " + err.Error(),
			})
			continue
		}
		out.Results = append(out.Results, hostProcsResult{
			Node: res.Node, Total: res.Total, Processes: res.Processes,
		})
	}
	return out, nil
}

// targetAddrs 把 node 过滤条件（空=全部 ready 节点；否则按 node ID / addr /
// hostname 匹配单个节点）解析成 nodeID→addr 集合。
func (h *Handler) targetAddrs(ctx context.Context, node string) (map[string]string, error) {
	addrs, err := h.orch.NodeAddrs(ctx)
	if err != nil {
		return nil, err
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("no ready swarm nodes")
	}
	if node == "" {
		return addrs, nil
	}
	for nodeID, addr := range addrs {
		if nodeID == node || addr == node {
			return map[string]string{nodeID: addr}, nil
		}
	}
	// hostname 兜底（execHost 只匹配 ID/addr，这里对齐 list_nodes 的可见标识）
	nodes, err := h.cli.ListNodes(ctx, nil)
	if err == nil {
		for _, n := range nodes {
			if n.Description.Hostname != node {
				continue
			}
			if addr, ok := addrs[n.ID]; ok {
				return map[string]string{n.ID: addr}, nil
			}
		}
	}
	return nil, fmt.Errorf("node %q not found", node)
}
