package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/docker"
)

// WorkerPort is the default HTTP port every node-role worker listens on. The
// manager proxies to node workers through this port; when workers listen on a
// custom port, override it via Orchestrator.SetWorkerPort.
const WorkerPort = "8080"

// NodeClient talks to a node-role worker's local API over HTTP.
type NodeClient struct {
	base  string
	hc    *http.Client
	token string // bearer token sent on every request when the node worker enforces auth
}

// NewNodeClient builds a client for a node worker at the given address
// (host or IP; the port is added if absent — the optional port argument
// overrides the default WorkerPort). token is forwarded as
// "Authorization: Bearer <token>" on each call; pass "" for auth-disabled
// clusters.
func NewNodeClient(addr, token string, port ...string) *NodeClient {
	base := addr
	if !hasPort(base) {
		p := WorkerPort
		if len(port) > 0 && port[0] != "" {
			p = port[0]
		}
		base = addr + ":" + p
	}
	if !hasScheme(base) {
		base = "http://" + base
	}
	return &NodeClient{
		base:  base,
		hc:    &http.Client{Timeout: 15 * time.Second},
		token: token,
	}
}

// withAuth sets the bearer header on a request when a token is configured.
func (n *NodeClient) withAuth(req *http.Request) {
	if n.token != "" {
		req.Header.Set("Authorization", "Bearer "+n.token)
	}
}

// NodeStats is the response shape of GET /api/v1/local/stats.
type NodeStats struct {
	Node           string              `json:"node"`
	Containers     []NodeContainerStat `json:"containers"`
	HostCPUPercent float64             `json:"hostCpuPercent,omitempty"`
	HostMemPercent float64             `json:"hostMemPercent,omitempty"`
	HostMemTotal   uint64              `json:"hostMemTotalBytes,omitempty"`
	HostMemUsed    uint64              `json:"hostMemUsedBytes,omitempty"`
	HostCPUCores   int                 `json:"hostCpuCores,omitempty"`
	ContainerCount int                 `json:"containerCount,omitempty"`
}

// NodeContainerStat mirrors nodeagent.containerStat.
type NodeContainerStat struct {
	ContainerID string  `json:"containerId"`
	Service     string  `json:"service,omitempty"`
	TaskID      string  `json:"taskId,omitempty"`
	CPUPercent  float64 `json:"cpuPercent"`
	MemPercent  float64 `json:"memPercent"`
	MemUsage    uint64  `json:"memUsageBytes"`
	MemLimit    uint64  `json:"memLimitBytes"`
}

// ExecResult is the response of POST /api/v1/local/exec.
type ExecResult struct {
	ContainerID string `json:"containerId"`
	ExitCode    int    `json:"exitCode"`
	Output      string `json:"output"`
}

// HostResult is the response of POST /api/v1/local/host.
type HostResult struct {
	Node      string `json:"node"`
	ExitCode  int    `json:"exitCode"`
	Output    string `json:"output"`
	ElapsedMS int64  `json:"elapsedMs"`
}

// ProcessInfo mirrors nodeagent.processInfo (GET /api/v1/local/processes).
type ProcessInfo struct {
	PID        int     `json:"pid"`
	Name       string  `json:"name"`
	Cmdline    string  `json:"cmdline,omitempty"`
	State      string  `json:"state"`
	MemKB      uint64  `json:"memKb"`
	CPUPercent float64 `json:"cpuPercent"`
}

// ProcessesResp is the response of GET /api/v1/local/processes.
type ProcessesResp struct {
	Node      string        `json:"node"`
	Total     int           `json:"total"`
	Processes []ProcessInfo `json:"processes"`
}

// PortCheckResult mirrors nodeagent.portCheckResp.
type PortCheckResult struct {
	Node      string `json:"node"`
	Host      string `json:"host"`
	Port      string `json:"port"`
	OK        bool   `json:"ok"`
	LatencyMS int64  `json:"latencyMs"`
	Error     string `json:"error,omitempty"`
}

// HTTPCheckRequest mirrors nodeagent.httpCheckReq.
type HTTPCheckRequest struct {
	URL            string            `json:"url"`
	Method         string            `json:"method,omitempty"`
	Headers        map[string]string `json:"headers,omitempty"`
	ExpectedStatus []int             `json:"expectedStatus,omitempty"`
	ExpectedBody   string            `json:"expectedBody,omitempty"`
	Timeout        string            `json:"timeout,omitempty"`
}

// HTTPCheckResult mirrors nodeagent.httpCheckResp.
type HTTPCheckResult struct {
	Node      string `json:"node"`
	URL       string `json:"url"`
	OK        bool   `json:"ok"`
	Status    int    `json:"status"`
	LatencyMS int64  `json:"latencyMs"`
	Error     string `json:"error,omitempty"`
}

// FlowStep mirrors nodeagent.flowStepReq（多步 HTTP 事务探测的一个步骤）。
type FlowStep struct {
	Name         string            `json:"name"`
	URL          string            `json:"url"`
	Method       string            `json:"method,omitempty"`
	Headers      map[string]string `json:"headers,omitempty"`
	Body         string            `json:"body,omitempty"`
	ExpectStatus []int             `json:"expectStatus,omitempty"`
	ExpectBody   string            `json:"expectBody,omitempty"`
	Extract      map[string]string `json:"extract,omitempty"` // var -> "$.json.path" 或 "re:正则"
}

// FlowCheckRequest mirrors nodeagent.flowReq。
type FlowCheckRequest struct {
	Steps   []FlowStep        `json:"steps"`
	Vars    map[string]string `json:"vars,omitempty"`
	Timeout string            `json:"timeout,omitempty"`
}

// FlowStepResult mirrors nodeagent.flowStepResp（提取值不回显，只有变量名）。
type FlowStepResult struct {
	Name      string   `json:"name"`
	OK        bool     `json:"ok"`
	Status    int      `json:"status"`
	LatencyMS int64    `json:"latencyMs"`
	Extracted []string `json:"extracted,omitempty"`
	Error     string   `json:"error,omitempty"`
}

// FlowCheckResult mirrors nodeagent.flowResp。
type FlowCheckResult struct {
	Node       string           `json:"node"`
	OK         bool             `json:"ok"`
	Steps      []FlowStepResult `json:"steps"`
	FailedStep string           `json:"failedStep,omitempty"`
	LatencyMS  int64            `json:"latencyMs"`
	Error      string           `json:"error,omitempty"`
}

// Processes fetches the node's host process list (top=cpu|mem, limit=N;
// filter matches name/cmdline substring, case-insensitive).
func (n *NodeClient) Processes(ctx context.Context, top, limit, filter string) (ProcessesResp, error) {
	q := url.Values{}
	if top != "" {
		q.Set("top", top)
	}
	if limit != "" {
		q.Set("limit", limit)
	}
	if filter != "" {
		q.Set("filter", filter)
	}
	path := "/api/v1/local/processes"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out ProcessesResp
	if err := n.getJSON(ctx, path, &out); err != nil {
		return ProcessesResp{}, err
	}
	return out, nil
}

// Containers lists every container on the node (swarm tasks + standalone
// docker run containers) via GET /api/v1/local/containers.
func (n *NodeClient) Containers(ctx context.Context) ([]NodeContainerInfo, error) {
	var out struct {
		Node       string              `json:"node"`
		Containers []NodeContainerInfo `json:"containers"`
	}
	if err := n.getJSON(ctx, "/api/v1/local/containers", &out); err != nil {
		return nil, err
	}
	return out.Containers, nil
}

// NodeContainerInfo mirrors nodeagent.ContainerInfo (GET /api/v1/local/containers).
type NodeContainerInfo struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Image   string `json:"image"`
	State   string `json:"state"`
	Type    string `json:"type"`    // service | standalone
	Service string `json:"service"` // swarm service name when type=service
	Ports   string `json:"ports"`
}

// CheckPort runs an ad-hoc TCP probe from the node worker.
func (n *NodeClient) CheckPort(ctx context.Context, host, port, timeout string) (PortCheckResult, error) {
	q := url.Values{"host": {host}, "port": {port}}
	if timeout != "" {
		q.Set("timeout", timeout)
	}
	var out PortCheckResult
	if err := n.getJSON(ctx, "/api/v1/local/check/port?"+q.Encode(), &out); err != nil {
		return PortCheckResult{}, err
	}
	return out, nil
}

// CheckHTTP runs an ad-hoc HTTP probe from the node worker.
func (n *NodeClient) CheckHTTP(ctx context.Context, req HTTPCheckRequest) (HTTPCheckResult, error) {
	var out HTTPCheckResult
	if err := n.postJSON(ctx, "/api/v1/local/check/http", req, &out); err != nil {
		return HTTPCheckResult{}, err
	}
	return out, nil
}

// CheckFlow runs a multi-step HTTP transaction probe from the node worker.
func (n *NodeClient) CheckFlow(ctx context.Context, req FlowCheckRequest) (FlowCheckResult, error) {
	var out FlowCheckResult
	if err := n.postJSON(ctx, "/api/v1/local/check/flow", req, &out); err != nil {
		return FlowCheckResult{}, err
	}
	return out, nil
}

// Stats fetches the node's local container stats.
func (n *NodeClient) Stats(ctx context.Context) (NodeStats, error) {
	var out NodeStats
	if err := n.getJSON(ctx, "/api/v1/local/stats", &out); err != nil {
		return NodeStats{}, err
	}
	return out, nil
}

// Exec runs a command inside a container on the node.
func (n *NodeClient) Exec(ctx context.Context, container, service string, slot int, cmd []string) (ExecResult, error) {
	body := map[string]any{
		"container": container,
		"service":   service,
		"slot":      slot,
		"command":   cmd,
	}
	var out ExecResult
	if err := n.postJSON(ctx, "/api/v1/local/exec", body, &out); err != nil {
		return ExecResult{}, err
	}
	return out, nil
}

// Host runs a command on the node's host.
func (n *NodeClient) Host(ctx context.Context, command string) (HostResult, error) {
	var out HostResult
	if err := n.postJSON(ctx, "/api/v1/local/host", map[string]string{"command": command}, &out); err != nil {
		return HostResult{}, err
	}
	return out, nil
}

// Proxy forwards an arbitrary request to this node worker's HTTP API and
// returns the status code + body (used by non-leader managers to forward
// write operations to the leader).
func (n *NodeClient) Proxy(method, path string, body []byte) (int, []byte, error) {
	req, err := http.NewRequest(method, n.base+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	n.withAuth(req)
	resp, err := n.hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data, nil
}

// ---- http helpers ----

func (n *NodeClient) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, n.base+path, nil)
	if err != nil {
		return err
	}
	n.withAuth(req)
	resp, err := n.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("node api %s: %s", path, errBody(data))
	}
	return json.Unmarshal(data, out)
}

func (n *NodeClient) postJSON(ctx context.Context, path string, body any, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.base+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	n.withAuth(req)
	resp, err := n.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("node api %s: %s", path, errBody(data))
	}
	return json.Unmarshal(data, out)
}

func errBody(data []byte) string {
	var m map[string]string
	if json.Unmarshal(data, &m) == nil && m["error"] != "" {
		return m["error"]
	}
	return string(data)
}

func hasPort(addr string) bool {
	for i := 0; i < len(addr); i++ {
		if addr[i] == ':' {
			return true
		}
	}
	return false
}

func hasScheme(addr string) bool {
	return len(addr) > 7 && (addr[:7] == "http://" || addr[:8] == "https://")
}

// nodeAddrs returns the address of every swarm node (from docker node ls).
func (o *Orchestrator) nodeAddrs(ctx context.Context) (map[string]string, error) {
	nodes, err := o.cli.ListNodes(ctx, nil)
	if err != nil {
		return nil, err
	}
	out := map[string]string{} // nodeID -> addr
	for _, n := range nodes {
		if n.Status.State == "ready" && n.Status.Addr != "" {
			out[n.ID] = n.Status.Addr
		}
	}
	return out, nil
}

// NodeAddrs exposes nodeID -> addr for all ready swarm nodes (used by the MCP
// layer for cross-node aggregation).
func (o *Orchestrator) NodeAddrs(ctx context.Context) (map[string]string, error) {
	return o.nodeAddrs(ctx)
}

// NodeClientByAddr builds a client for a node worker at the given address.
func (o *Orchestrator) NodeClientByAddr(addr string) *NodeClient {
	return NewNodeClient(addr, o.authToken, o.nodePort())
}

// SelfNodeID returns this daemon's swarm node ID.
func (o *Orchestrator) SelfNodeID(ctx context.Context) (string, error) {
	n, err := o.cli.SelfNode(ctx)
	if err != nil {
		return "", err
	}
	return n.ID, nil
}

// nodeClientFor finds the node worker client for a task (by its node ID).
func (o *Orchestrator) nodeClientForTask(ctx context.Context, task docker.Task) (*NodeClient, error) {
	if task.NodeID == "" {
		return nil, fmt.Errorf("task %s has no node assignment", task.ID)
	}
	addrs, err := o.nodeAddrs(ctx)
	if err != nil {
		return nil, err
	}
	addr, ok := addrs[task.NodeID]
	if !ok {
		return nil, fmt.Errorf("no address for node %s", task.NodeID)
	}
	return NewNodeClient(addr, o.authToken, o.nodePort()), nil
}
