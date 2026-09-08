// 诊断工具（node_* / probe_*）：节点观测与从集群内发起的主动拨测——
// 排查的证据面（全部只读）。node 参数缺省时从 manager leader 发起。
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"

	mcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/workerproxy"
)

type clusterIn struct {
	Cluster string `json:"cluster" description:"Cluster name (from cluster_list)"`
}

type probePortIn struct {
	Cluster string `json:"cluster" description:"Cluster name"`
	Host    string `json:"host" description:"Target host (service DNS name / node IP)"`
	Port    int    `json:"port" description:"Target TCP port"`
	Node    string `json:"node,omitempty" description:"Launch the probe from this node (empty = the worker's manager node)"`
}

type probePortOut struct {
	Node      string `json:"node"`
	Host      string `json:"host"`
	Port      string `json:"port"`
	OK        bool   `json:"ok"`
	LatencyMS int64  `json:"latency_ms,omitempty"`
	Error     string `json:"error,omitempty"`
	Hint      string `json:"hint,omitempty"`
}

type probeHTTPIn struct {
	Cluster      string            `json:"cluster" description:"Cluster name"`
	URL          string            `json:"url" description:"Target URL (build from service_get ports, e.g. http://<service>.<network>:8080/health)"`
	Method       string            `json:"method,omitempty" description:"HTTP method (default GET)"`
	Headers      map[string]string `json:"headers,omitempty" description:"Request headers"`
	ExpectStatus []int             `json:"expect_status,omitempty" description:"Status codes considered OK (empty = 2xx)"`
	TimeoutSec   int               `json:"timeout_sec,omitempty" description:"Timeout seconds (default 10)"`
	Node         string            `json:"node,omitempty" description:"Launch from this node (empty = manager)"`
}

type probeHTTPOut struct {
	Node      string `json:"node"`
	URL       string `json:"url"`
	OK        bool   `json:"ok"`
	Status    int    `json:"status,omitempty"`
	LatencyMS int64  `json:"latency_ms,omitempty"`
	Error     string `json:"error,omitempty"`
	Hint      string `json:"hint,omitempty"`
}

type nodeStatsOut struct {
	Node       string                      `json:"node"`
	Containers []workerproxy.ContainerStat `json:"containers"`
}

type nodeListOut struct {
	Items []workerproxy.Node `json:"items"`
}

type probeFlowStepIn struct {
	Name         string            `json:"name" description:"Step name, e.g. login"`
	URL          string            `json:"url" description:"Step URL (supports ${var} from previous extract)"`
	Method       string            `json:"method,omitempty" description:"HTTP method (default GET)"`
	Headers      map[string]string `json:"headers,omitempty" description:"Request headers"`
	Body         string            `json:"body,omitempty" description:"Request body"`
	ExpectStatus []int             `json:"expect_status,omitempty" description:"Status codes considered OK (empty = 2xx)"`
	ExpectBody   string            `json:"expect_body,omitempty" description:"Substring the body must contain"`
	Extract      map[string]string `json:"extract,omitempty" description:"Extract variables from response, var -> jsonpath-ish spec (worker flow check semantics)"`
}

type probeFlowIn struct {
	Cluster string            `json:"cluster" description:"Cluster name"`
	Steps   []probeFlowStepIn `json:"steps" description:"1-10 steps, executed in order"`
	Vars    map[string]string `json:"vars,omitempty" description:"Initial variables"`
	Timeout string            `json:"timeout,omitempty" description:"Overall timeout (e.g. 15s)"`
	Node    string            `json:"node,omitempty" description:"Launch from this node (empty = manager)"`
}

type probeFlowStepOut struct {
	Name      string   `json:"name"`
	OK        bool     `json:"ok"`
	Status    int      `json:"status,omitempty"`
	LatencyMS int64    `json:"latency_ms,omitempty"`
	Extracted []string `json:"extracted,omitempty"`
	Error     string   `json:"error,omitempty"`
}

type probeFlowOut struct {
	Node       string             `json:"node"`
	OK         bool               `json:"ok"`
	FailedStep string             `json:"failed_step,omitempty"`
	Steps      []probeFlowStepOut `json:"steps"`
	Hint       string             `json:"hint,omitempty"`
}

type nodeProcessesIn struct {
	Cluster string `json:"cluster" description:"Cluster name"`
	Node    string `json:"node" description:"Node id or hostname (from node_list)"`
	Top     string `json:"top,omitempty" description:"Sort key: cpu | mem (default cpu)"`
	Filter  string `json:"filter,omitempty" description:"Substring filter on process name/cmdline"`
	Limit   int    `json:"limit,omitempty" description:"Max rows (default 100, max 500)"`
}

type nodeProcessesOut struct {
	Node      string                `json:"node"`
	Total     int                   `json:"total"`
	Processes []workerproxy.Process `json:"processes"`
}

func (h *Handler) registerDiagTools(s *mcp.Server) {
	// node_list
	mcp.AddTool(s, &mcp.Tool{
		Name:        "node_list",
		Description: "List swarm nodes of a cluster: role, state, reachability, CPU/mem totals.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in clusterIn) (*mcp.CallToolResult, nodeListOut, error) {
		cli, err := h.workerClient(in.Cluster)
		if err != nil {
			return nil, nodeListOut{}, err
		}
		nodes, err := cli.ListNodes(ctx)
		if err != nil {
			return nil, nodeListOut{}, err
		}
		if nodes == nil {
			nodes = []workerproxy.Node{}
		}
		return nil, nodeListOut{Items: nodes}, nil
	})

	// node_stats
	mcp.AddTool(s, &mcp.Tool{
		Name:        "node_stats",
		Description: "Per-container CPU/mem usage from the worker manager's view (same data as the platform cluster metrics page). Key evidence for OOM / CPU throttling; node breakdown via node_list.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in clusterIn) (*mcp.CallToolResult, nodeStatsOut, error) {
		cli, err := h.workerClient(in.Cluster)
		if err != nil {
			return nil, nodeStatsOut{}, err
		}
		stats, err := cli.NodeStats(ctx)
		if err != nil {
			return nil, nodeStatsOut{}, err
		}
		if stats.Containers == nil {
			stats.Containers = []workerproxy.ContainerStat{}
		}
		return nil, nodeStatsOut{Node: stats.Node, Containers: stats.Containers}, nil
	})

	// probe_port
	mcp.AddTool(s, &mcp.Tool{
		Name:        "probe_port",
		Description: "One-shot TCP dial from inside the cluster network. Locates where connectivity breaks: service-internal → cross-node → ingress.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in probePortIn) (*mcp.CallToolResult, probePortOut, error) {
		cli, err := h.workerClient(in.Cluster)
		if err != nil {
			return nil, probePortOut{}, err
		}
		nodeID := in.Node
		if nodeID != "" {
			if nodeID, err = h.resolveNodeID(ctx, cli, nodeID); err != nil {
				return nil, probePortOut{}, err
			}
		}
		r, err := cli.CheckPort(ctx, nodeID, in.Host, in.Port, "")
		if err != nil {
			return nil, probePortOut{}, err
		}
		out := probePortOut{Node: r.Node, Host: r.Host, Port: r.Port, OK: r.OK,
			LatencyMS: r.LatencyMS, Error: r.Error}
		if !out.OK {
			out.Hint = "compare with another node (probe_port node=...) to tell service-down from network-partition; cross-check service_get replica state"
		}
		return nil, out, nil
	})

	// probe_http
	mcp.AddTool(s, &mcp.Tool{
		Name:        "probe_http",
		Description: "One-shot HTTP request from inside the cluster network (status/latency/expected-status matching). Use for 5xx / timeout reproduction.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in probeHTTPIn) (*mcp.CallToolResult, probeHTTPOut, error) {
		cli, err := h.workerClient(in.Cluster)
		if err != nil {
			return nil, probeHTTPOut{}, err
		}
		nodeID := in.Node
		if nodeID != "" {
			if nodeID, err = h.resolveNodeID(ctx, cli, nodeID); err != nil {
				return nil, probeHTTPOut{}, err
			}
		}
		timeout := ""
		if in.TimeoutSec > 0 {
			timeout = fmt.Sprintf("%ds", in.TimeoutSec)
		}
		r, err := cli.CheckHTTP(ctx, nodeID, workerproxy.HTTPCheckRequest{
			URL: in.URL, Method: in.Method, Headers: in.Headers,
			ExpectedStatus: in.ExpectStatus, Timeout: timeout,
		})
		if err != nil {
			return nil, probeHTTPOut{}, err
		}
		out := probeHTTPOut{Node: r.Node, URL: r.URL, OK: r.OK, Status: r.Status,
			LatencyMS: r.LatencyMS, Error: r.Error}
		if !out.OK {
			out.Hint = "check service_logs for the same timestamp; try another node to isolate network vs service"
		}
		return nil, out, nil
	})

	// probe_flow
	mcp.AddTool(s, &mcp.Tool{
		Name:        "probe_flow",
		Description: "Multi-step HTTP transaction probe from inside the cluster (login → call → verify, with variable extraction between steps). Each step fails fast; the failing step name and error are returned.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in probeFlowIn) (*mcp.CallToolResult, probeFlowOut, error) {
		cli, err := h.workerClient(in.Cluster)
		if err != nil {
			return nil, probeFlowOut{}, err
		}
		nodeID := in.Node
		if nodeID != "" {
			if nodeID, err = h.resolveNodeID(ctx, cli, nodeID); err != nil {
				return nil, probeFlowOut{}, err
			}
		}
		if len(in.Steps) == 0 {
			return nil, probeFlowOut{}, fmt.Errorf("steps is required (1-10 steps)")
		}
		steps := make([]workerproxy.FlowStep, 0, len(in.Steps))
		for _, st := range in.Steps {
			steps = append(steps, workerproxy.FlowStep{
				Name: st.Name, URL: st.URL, Method: st.Method, Headers: st.Headers,
				Body: st.Body, ExpectStatus: st.ExpectStatus, ExpectBody: st.ExpectBody,
				Extract: st.Extract,
			})
		}
		r, err := cli.CheckFlow(ctx, nodeID, workerproxy.FlowCheckRequest{Steps: steps, Vars: in.Vars, Timeout: in.Timeout})
		if err != nil {
			return nil, probeFlowOut{}, err
		}
		out := probeFlowOut{
			Node: r.Node, OK: r.OK, FailedStep: r.FailedStep,
			Steps: make([]probeFlowStepOut, 0, len(r.Steps)),
		}
		for _, st := range r.Steps {
			out.Steps = append(out.Steps, probeFlowStepOut{
				Name: st.Name, OK: st.OK, Status: st.Status,
				LatencyMS: st.LatencyMS, Extracted: st.Extracted, Error: st.Error,
			})
		}
		if !out.OK {
			out.Hint = "inspect the failed step (status/error); service_logs of the upstream service for the same timestamp usually explains it"
		}
		return nil, out, nil
	})

	// node_processes
	mcp.AddTool(s, &mcp.Tool{
		Name:        "node_processes",
		Description: "List host processes of a node (pid/name/cpu/mem), for finding port listeners or resource hogs outside containers.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in nodeProcessesIn) (*mcp.CallToolResult, nodeProcessesOut, error) {
		cli, err := h.workerClient(in.Cluster)
		if err != nil {
			return nil, nodeProcessesOut{}, err
		}
		nodeID, err := h.resolveNodeID(ctx, cli, in.Node)
		if err != nil {
			return nil, nodeProcessesOut{}, err
		}
		limit := in.Limit
		if limit <= 0 {
			limit = 100
		}
		if limit > 500 {
			limit = 500
		}
		resp, err := cli.ListProcesses(ctx, nodeID, in.Top, limit, in.Filter)
		if err != nil {
			return nil, nodeProcessesOut{}, err
		}
		if resp.Processes == nil {
			resp.Processes = []workerproxy.Process{}
		}
		return nil, nodeProcessesOut{Node: resp.Node, Total: resp.Total, Processes: resp.Processes}, nil
	})
}

// resolveNodeID hostname/id → swarm node id（CheckPort/CheckHTTP/NodeStatsOn
// 均要求 node id；助手常拿的是 hostname）。
func (h *Handler) resolveNodeID(ctx context.Context, cli *workerproxy.Client, idOrHost string) (string, error) {
	nodes, err := cli.ListNodes(ctx)
	if err != nil {
		return "", err
	}
	for _, n := range nodes {
		if n.ID == idOrHost || n.Hostname == idOrHost {
			return n.ID, nil
		}
	}
	return "", fmt.Errorf("node %q not found in cluster (use node_list)", idOrHost)
}

// jsonUnmarshalInto 宽松 JSON 解码（证据透传场景，失败由调用方忽略）。
func jsonUnmarshalInto(raw json.RawMessage, v any) error {
	return json.Unmarshal(raw, v)
}
