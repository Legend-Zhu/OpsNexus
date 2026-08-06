// Package workerproxy is the HTTP client the management plane uses to talk
// to a cluster's Worker. It wraps the Worker's public API (healthz, self,
// services, events, audit, local stats) with per-request bearer token,
// timeouts and error normalisation, so handlers get typed results instead
// of raw HTTP plumbing. One Client per cluster (built from the registry).
//
// Contract reference (Worker):
//
//	GET  /healthz                      -> {"status":"ok",...}
//	GET  /api/v1/self                  -> SelfInfo{nodeId,hostname,role,leader,state,swarmManager,addr}
//	GET  /api/v1/services?label=       -> []Service  (opaque here, P2 consumes)
//	GET  /api/v1/local/stats           -> {node, containers[]}
//	GET  /api/v1/events?service&type&limit -> []Event  (opaque here, P3 consumes)
//	GET  /api/v1/audit?action&limit    -> []AuditEntry (opaque here)
package workerproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ErrUnreachable 表示 Worker 连接失败（网络/超时/非 2xx），用于健康状态判定。
type ErrUnreachable struct {
	URL    string
	Status int
	Err    error
}

func (e *ErrUnreachable) Error() string {
	if e.Status > 0 {
		return fmt.Sprintf("worker %s returned status %d: %v", e.URL, e.Status, e.Err)
	}
	return fmt.Sprintf("worker %s unreachable: %v", e.URL, e.Err)
}

// Client 是对单个 Worker 的 HTTP 客户端。
type Client struct {
	baseURL string // e.g. http://<管理节点IP>:8080
	token   string // bearer token（可空）
	http    *http.Client
}

// New 创建 Worker 客户端。
func New(baseURL, token string) *Client {
	return &Client{
		baseURL: baseURL,
		token:   token,
		http: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// do 发起请求并解码 JSON 到 v；非 2xx 返回 *ErrUnreachable。
func (c *Client) do(ctx context.Context, method, path string, query map[string]string, body []byte, v any) error {
	url := c.baseURL + path
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return &ErrUnreachable{URL: url, Err: err}
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	q := req.URL.Query()
	for k, val := range query {
		if val != "" {
			q.Set(k, val)
		}
	}
	req.URL.RawQuery = q.Encode()

	resp, err := c.http.Do(req)
	if err != nil {
		return &ErrUnreachable{URL: url, Err: err}
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20)) // 8MB 上限
	if err != nil {
		return &ErrUnreachable{URL: url, Status: resp.StatusCode, Err: err}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &ErrUnreachable{URL: url, Status: resp.StatusCode, Err: fmt.Errorf("%s", truncate(string(raw), 512))}
	}
	if v == nil {
		return nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return &ErrUnreachable{URL: url, Status: resp.StatusCode, Err: fmt.Errorf("decode: %w", err)}
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// --- Worker 契约类型（P1 用到的子集） ---

// SelfInfo 对应 Worker GET /api/v1/self。
type SelfInfo struct {
	NodeID       string `json:"nodeId"`
	Hostname     string `json:"hostname"`
	Role         string `json:"role"`         // manager | worker
	Leader       bool   `json:"leader"`       // manager-only: swarm Raft leader
	State        string `json:"state"`        // node Status.State
	SwarmManager bool   `json:"swarmManager"` // 该 daemon 是否运行 swarm 控制面
	Addr         string `json:"addr,omitempty"`
}

// NodeStats 对应 Worker GET /api/v1/local/stats。
type NodeStats struct {
	Node       string          `json:"node"`
	Containers []ContainerStat `json:"containers"`
}

// ContainerStat 单个容器的资源使用。
type ContainerStat struct {
	ContainerID string  `json:"containerId"`
	Service     string  `json:"service,omitempty"`
	TaskID      string  `json:"taskId,omitempty"`
	CPUPercent  float64 `json:"cpuPercent"`
	MemPercent  float64 `json:"memPercent"`
	MemUsage    uint64  `json:"memUsageBytes"`
	MemLimit    uint64  `json:"memLimitBytes"`
}

// Node 对应 Worker GET /api/v1/nodes 的单个节点视图
// （管理层级「集群 → 节点」）。
type Node struct {
	ID             string  `json:"id"`
	Hostname       string  `json:"hostname"`
	Role           string  `json:"role"`         // manager | worker
	State          string  `json:"state"`        // ready | down | ...
	Availability   string  `json:"availability"` // active | pause | drain
	Addr           string  `json:"addr"`
	Leader         bool    `json:"leader"`
	ManagerReach   string  `json:"managerReachability,omitempty"`
	Reachable      bool    `json:"reachable"` // 节点 Worker 可达
	CPUCores       float64 `json:"cpuCores"`
	MemBytes       uint64  `json:"memBytes"`
	CPUPercent     float64 `json:"cpuPercent"`
	MemPercent     float64 `json:"memPercent"`
	ContainerCount int     `json:"containerCount"`
}

// Process 对应 Worker GET /api/v1/local/processes 的单个进程。
type Process struct {
	PID        int     `json:"pid"`
	Name       string  `json:"name"`
	Cmdline    string  `json:"cmdline,omitempty"`
	State      string  `json:"state"`
	MemKB      uint64  `json:"memKb"`
	CPUPercent float64 `json:"cpuPercent"`
}

// ProcessesResp 对应 Worker 的进程列表响应。
type ProcessesResp struct {
	Node      string    `json:"node"`
	Total     int       `json:"total"`
	Processes []Process `json:"processes"`
}

// PortCheckResult 对应 Worker GET /api/v1/nodes/{id}/check/port 的响应。
type PortCheckResult struct {
	Node      string `json:"node"`
	Host      string `json:"host"`
	Port      string `json:"port"`
	OK        bool   `json:"ok"`
	LatencyMS int64  `json:"latencyMs"`
	Error     string `json:"error,omitempty"`
}

// HTTPCheckRequest 对应 Worker POST /api/v1/nodes/{id}/check/http 的请求体。
type HTTPCheckRequest struct {
	URL            string            `json:"url"`
	Method         string            `json:"method,omitempty"`
	Headers        map[string]string `json:"headers,omitempty"`
	ExpectedStatus []int             `json:"expectedStatus,omitempty"` // 空 = 任意 2xx
	ExpectedBody   string            `json:"expectedBody,omitempty"`   // 正则
	Timeout        string            `json:"timeout,omitempty"`
}

// HTTPCheckResult 对应 Worker POST /api/v1/nodes/{id}/check/http 的响应。
type HTTPCheckResult struct {
	Node      string `json:"node"`
	URL       string `json:"url"`
	OK        bool   `json:"ok"`
	Status    int    `json:"status"`
	LatencyMS int64  `json:"latencyMs"`
	Error     string `json:"error,omitempty"`
}

// FlowStep 多步 HTTP 事务探测的一个步骤。
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

// FlowCheckRequest 对应 Worker POST /api/v1/nodes/{id}/check/flow 的请求体。
type FlowCheckRequest struct {
	Steps   []FlowStep        `json:"steps"`
	Vars    map[string]string `json:"vars,omitempty"`
	Timeout string            `json:"timeout,omitempty"`
}

// FlowStepResult 单步结果（提取值不回显，只有变量名）。
type FlowStepResult struct {
	Name      string   `json:"name"`
	OK        bool     `json:"ok"`
	Status    int      `json:"status"`
	LatencyMS int64    `json:"latencyMs"`
	Extracted []string `json:"extracted,omitempty"`
	Error     string   `json:"error,omitempty"`
}

// FlowCheckResult 多步事务探测结果。
type FlowCheckResult struct {
	Node       string           `json:"node"`
	OK         bool             `json:"ok"`
	Steps      []FlowStepResult `json:"steps"`
	FailedStep string           `json:"failedStep,omitempty"`
	LatencyMS  int64            `json:"latencyMs"`
	Error      string           `json:"error,omitempty"`
}

// --- 方法 ---

// Ping 探测 Worker 存活（GET /healthz）。
func (c *Client) Ping(ctx context.Context) error {
	var out map[string]any
	return c.do(ctx, http.MethodGet, "/healthz", nil, nil, &out)
}

// Self 获取 Worker 所在节点的 swarm 角色（GET /api/v1/self）。
func (c *Client) Self(ctx context.Context) (SelfInfo, error) {
	var si SelfInfo
	err := c.do(ctx, http.MethodGet, "/api/v1/self", nil, nil, &si)
	return si, err
}

// Services 列出集群服务（GET /api/v1/services），P1 原样透传。
func (c *Client) Services(ctx context.Context, label string) (json.RawMessage, error) {
	var out json.RawMessage
	err := c.do(ctx, http.MethodGet, "/api/v1/services", map[string]string{"label": label}, nil, &out)
	return out, err
}

// NodeStats 获取节点资源统计（GET /api/v1/local/stats）。
func (c *Client) NodeStats(ctx context.Context) (NodeStats, error) {
	var ns NodeStats
	err := c.do(ctx, http.MethodGet, "/api/v1/local/stats", nil, nil, &ns)
	return ns, err
}

// ListNodes 获取集群节点列表（GET /api/v1/nodes）。
func (c *Client) ListNodes(ctx context.Context) ([]Node, error) {
	var nodes []Node
	err := c.do(ctx, http.MethodGet, "/api/v1/nodes", nil, nil, &nodes)
	return nodes, err
}

// ListProcesses 获取指定节点的宿主机进程（GET /api/v1/nodes/{id}/processes）。
// filter 为名称/cmdline 子串（大小写不敏感），空 = 不过滤。
func (c *Client) ListProcesses(ctx context.Context, nodeID, top string, limit int, filter string) (ProcessesResp, error) {
	var out ProcessesResp
	q := map[string]string{"top": top, "filter": filter}
	if limit > 0 {
		q["limit"] = fmt.Sprintf("%d", limit)
	}
	err := c.do(ctx, http.MethodGet, "/api/v1/nodes/"+nodeID+"/processes", q, nil, &out)
	return out, err
}

// CheckPort 从指定节点发起一次性 TCP 探测（GET /api/v1/nodes/{id}/check/port）。
func (c *Client) CheckPort(ctx context.Context, nodeID, host string, port int, timeout string) (PortCheckResult, error) {
	var out PortCheckResult
	q := map[string]string{"host": host, "port": fmt.Sprintf("%d", port), "timeout": timeout}
	err := c.do(ctx, http.MethodGet, "/api/v1/nodes/"+nodeID+"/check/port", q, nil, &out)
	return out, err
}

// CheckHTTP 从指定节点发起一次性 HTTP 探测（POST /api/v1/nodes/{id}/check/http）。
func (c *Client) CheckHTTP(ctx context.Context, nodeID string, req HTTPCheckRequest) (HTTPCheckResult, error) {
	var out HTTPCheckResult
	body, err := json.Marshal(req)
	if err != nil {
		return out, err
	}
	err = c.do(ctx, http.MethodPost, "/api/v1/nodes/"+nodeID+"/check/http", nil, body, &out)
	return out, err
}

// CheckFlow 从指定节点发起多步 HTTP 事务探测（POST /api/v1/nodes/{id}/check/flow）。
func (c *Client) CheckFlow(ctx context.Context, nodeID string, req FlowCheckRequest) (FlowCheckResult, error) {
	var out FlowCheckResult
	body, err := json.Marshal(req)
	if err != nil {
		return out, err
	}
	err = c.do(ctx, http.MethodPost, "/api/v1/nodes/"+nodeID+"/check/flow", nil, body, &out)
	return out, err
}

// Events 获取监控事件（GET /api/v1/events），P3 消费，P1 原样透传。
func (c *Client) Events(ctx context.Context, service, typ string, limit int) (json.RawMessage, error) {
	var out json.RawMessage
	q := map[string]string{"service": service, "type": typ}
	if limit > 0 {
		q["limit"] = fmt.Sprintf("%d", limit)
	}
	err := c.do(ctx, http.MethodGet, "/api/v1/events", q, nil, &out)
	return out, err
}

// Audit 获取审计记录（GET /api/v1/audit）。
func (c *Client) Audit(ctx context.Context, action string, limit int) (json.RawMessage, error) {
	var out json.RawMessage
	q := map[string]string{"action": action}
	if limit > 0 {
		q["limit"] = fmt.Sprintf("%d", limit)
	}
	err := c.do(ctx, http.MethodGet, "/api/v1/audit", q, nil, &out)
	return out, err
}
