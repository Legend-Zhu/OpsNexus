package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/docker"
)

// WorkerPort is the HTTP port every node-role worker listens on. The manager
// proxies to node workers through this port.
const WorkerPort = "8080"

// NodeClient talks to a node-role worker's local API over HTTP.
type NodeClient struct {
	base string
	hc   *http.Client
}

// NewNodeClient builds a client for a node worker at the given address
// (host or IP; the port is added if absent).
func NewNodeClient(addr string) *NodeClient {
	base := addr
	if !hasPort(base) {
		base = addr + ":" + WorkerPort
	}
	if !hasScheme(base) {
		base = "http://" + base
	}
	return &NodeClient{
		base: base,
		hc:   &http.Client{Timeout: 15 * time.Second},
	}
}

// NodeStats is the response shape of GET /api/v1/local/stats.
type NodeStats struct {
	Node       string        `json:"node"`
	Containers []NodeContainerStat `json:"containers"`
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

// ---- http helpers ----

func (n *NodeClient) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, n.base+path, nil)
	if err != nil {
		return err
	}
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
	return NewNodeClient(addr)
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
	return NewNodeClient(addr), nil
}
