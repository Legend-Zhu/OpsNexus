// Package workerproxy is the gRPC client the management plane uses to talk to
// a cluster's Worker. It wraps the Worker's ManagementService (gRPC) with
// per-call bearer-token metadata, timeouts and error normalisation, so
// handlers get typed results instead of raw gRPC plumbing. One Client per
// cluster (built from the registry).
//
// The WorkerURL registered for a cluster points at the Worker's gRPC port
// (default :9080). The HTTP port (:8080) still serves /mcp, /healthz and the
// node-level local API; those are out of scope here.
//
// Method signatures are unchanged from the previous HTTP implementation so
// every upper layer (api/, patrol/, alertrule/, investigate/) needs no edits.
package workerproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
	pb "gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/workerproxy/pb"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

// ErrUnreachable 表示 Worker 连接失败（网络/超时/非 2xx），用于健康状态判定。
// Status is kept for API compatibility (502 for gRPC errors; the HTTP client
// previously populated it from the response).
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

// Client 是对单个 Worker 的 gRPC 客户端。
type Client struct {
	target string // host:port for gRPC dial
	token  string // bearer token（可空）
	conn   *grpc.ClientConn
	stub   pb.ManagementServiceClient

	// cache 集群数据缓存（LevelDB），读接口先查缓存、未命中回源并更新。
	// clusterName 用于缓存 key 命名。nil 表示不启用缓存。
	cache       cacheStore
	clusterName string
}

// cacheStore 是缓存读写接口（store.Store 实现）。在 cluster.Service 注入。
type cacheStore interface {
	GetClusterCache(cluster, resource string) *store.ClusterCache
	PutClusterCache(cluster, resource string, payload []byte) error
}

// SetCache 启用集群数据缓存（由 cluster.Service.WorkerClient 调用）。
func (c *Client) SetCache(st *store.Store, clusterName string) {
	c.cache = st
	c.clusterName = clusterName
}

// New 创建 Worker gRPC 客户端。baseURL 是 Worker 的 gRPC 端点
// (http(s)://host:9080 或 host:9080)；token 作为 bearer 元数据。
func New(baseURL, token string) *Client {
	target := grpcTarget(baseURL)
	// Tunnel frames carry up to 2MiB registry blob chunks (plus header room);
	// raise the per-call message cap so a chunked relay response and the
	// reverse-tunnel stream never trip the 4MiB default.
	conn, err := grpc.NewClient(target,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(16<<20),
			grpc.MaxCallSendMsgSize(16<<20),
		),
		// 流控窗口：grpc-go 默认每流接收窗口只有 64KiB，单条 HTTP/2 stream 的
		// 在途字节被卡死在 64KiB，吞吐 ≈ 64KiB/RTT。镜像层动辄几十上百 MiB，
		// 默认窗口会把它憋成几十 KB/s。这里把客户端声明的接收窗口（决定
		// worker→server 方向，即 push body 能灌多快）提到 32MiB/流、64MiB/连接，
		// 与 worker 服务端的对称设置配合，解除数量级瓶颈。
		grpc.WithInitialWindowSize(32<<20),
		grpc.WithInitialConnWindowSize(64<<20),
	)
	if err != nil {
		// grpc.NewClient only errors on an invalid target string; fall back to
		// a lazy dial that surfaces the error on first call via ErrUnreachable.
		conn = nil
	}
	c := &Client{target: target, token: token, conn: conn}
	if conn != nil {
		c.stub = pb.NewManagementServiceClient(conn)
	}
	return c
}

// Close releases the underlying gRPC connection.
func (c *Client) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

// SetTimeout is retained for API compatibility. The gRPC client uses
// per-context deadlines instead of a client-wide timeout; this is a no-op kept
// so callers (cluster.Service.WorkerClient) compile unchanged.
func (c *Client) SetTimeout(d time.Duration) {}

// grpcTarget converts a baseURL (http://host:9080, host:9080, :9080) into a
// gRPC dial target (host:9080), stripping scheme and path.
func grpcTarget(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err == nil && u.Host != "" {
		return u.Host
	}
	// No scheme: strip any path, keep host:port.
	if i := strings.IndexByte(baseURL, '/'); i >= 0 {
		baseURL = baseURL[:i]
	}
	return baseURL
}

// callCtx attaches the bearer token as gRPC metadata and returns ctx with the
// per-client timeout applied (default 30s, matching the previous HTTP client).
func (c *Client) callCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	if c.token != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+c.token)
	}
	return context.WithTimeout(ctx, 30*time.Second)
}

// wrapErr converts a gRPC error into *ErrUnreachable so the caller's health
// logic (cluster probe) keeps working unchanged.
func (c *Client) wrapErr(err error) error {
	if err == nil {
		return nil
	}
	return &ErrUnreachable{URL: c.target, Err: err}
}

// --- Worker 契约类型（与旧 HTTP 实现一致，上层零改动）---

// SelfInfo 对应 Worker Self RPC。
type SelfInfo struct {
	NodeID       string `json:"nodeId"`
	Hostname     string `json:"hostname"`
	Role         string `json:"role"`
	Leader       bool   `json:"leader"`
	State        string `json:"state"`
	SwarmManager bool   `json:"swarmManager"`
	Addr         string `json:"addr,omitempty"`
}

// NodeStats 对应 Worker NodeStats RPC。
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

// Node 对应 Worker ListNodes 的单个节点视图。
type Node struct {
	ID             string  `json:"id"`
	Hostname       string  `json:"hostname"`
	Role           string  `json:"role"`
	State          string  `json:"state"`
	Availability   string  `json:"availability"`
	Addr           string  `json:"addr"`
	Leader         bool    `json:"leader"`
	ManagerReach   string  `json:"managerReachability,omitempty"`
	Reachable      bool    `json:"reachable"`
	CPUCores       float64 `json:"cpuCores"`
	MemBytes       uint64  `json:"memBytes"`
	CPUPercent     float64 `json:"cpuPercent"`
	MemPercent     float64 `json:"memPercent"`
	ContainerCount int     `json:"containerCount"`
}

// Process 对应 Worker 进程条目。
type Process struct {
	PID        int     `json:"pid"`
	Name       string  `json:"name"`
	Cmdline    string  `json:"cmdline,omitempty"`
	State      string  `json:"state"`
	MemKB      uint64  `json:"memKb"`
	CPUPercent float64 `json:"cpuPercent"`
}

// ProcessesResp 对应 Worker 进程列表响应。
type ProcessesResp struct {
	Node      string    `json:"node"`
	Total     int       `json:"total"`
	Processes []Process `json:"processes"`
}

// PortCheckResult 对应 Worker CheckPort 响应。
type PortCheckResult struct {
	Node      string `json:"node"`
	Host      string `json:"host"`
	Port      string `json:"port"`
	OK        bool   `json:"ok"`
	LatencyMS int64  `json:"latencyMs"`
	Error     string `json:"error,omitempty"`
}

// HTTPCheckRequest 对应 Worker CheckHTTP 请求体。
type HTTPCheckRequest struct {
	URL            string            `json:"url"`
	Method         string            `json:"method,omitempty"`
	Headers        map[string]string `json:"headers,omitempty"`
	ExpectedStatus []int             `json:"expectedStatus,omitempty"`
	ExpectedBody   string            `json:"expectedBody,omitempty"`
	Timeout        string            `json:"timeout,omitempty"`
}

// HTTPCheckResult 对应 Worker CheckHTTP 响应。
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
	Extract      map[string]string `json:"extract,omitempty"`
}

// FlowCheckRequest 对应 Worker CheckFlow 请求体。
type FlowCheckRequest struct {
	Steps   []FlowStep        `json:"steps"`
	Vars    map[string]string `json:"vars,omitempty"`
	Timeout string            `json:"timeout,omitempty"`
}

// FlowStepResult 单步结果。
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

// Ping 探测 Worker 存活（gRPC Ping RPC）。
func (c *Client) Ping(ctx context.Context) error {
	cctx, cancel := c.callCtx(ctx)
	defer cancel()
	_, err := c.stub.Ping(cctx, &pb.Empty{})
	return c.wrapErr(err)
}

// Self 获取 Worker 所在节点的 swarm 角色。
func (c *Client) Self(ctx context.Context) (SelfInfo, error) {
	cctx, cancel := c.callCtx(ctx)
	defer cancel()
	resp, err := c.stub.Self(cctx, &pb.Empty{})
	if err != nil {
		return SelfInfo{}, c.wrapErr(err)
	}
	return SelfInfo{
		NodeID: resp.GetNodeId(), Hostname: resp.GetHostname(), Role: resp.GetRole(),
		Leader: resp.GetLeader(), State: resp.GetState(), SwarmManager: resp.GetSwarmManager(), Addr: resp.GetAddr(),
	}, nil
}

// Services 列出集群服务（Docker 原生 JSON，P1 原样透传）。
// Services 列出集群服务（Docker 原生 JSON）。启用缓存时先返回缓存快照。
func (c *Client) Services(ctx context.Context, label string) (json.RawMessage, error) {
	if c.cache != nil {
		if cached := c.cache.GetClusterCache(c.clusterName, "services"); cached != nil {
			return json.RawMessage(cached.Payload), nil
		}
	}
	cctx, cancel := c.callCtx(ctx)
	defer cancel()
	resp, err := c.stub.ListServices(cctx, &pb.ListServicesRequest{Label: label})
	if err != nil {
		return nil, c.wrapErr(err)
	}
	raw := json.RawMessage(resp.GetServicesJson())
	if c.cache != nil {
		_ = c.cache.PutClusterCache(c.clusterName, "services", raw)
	}
	return raw, nil
}

// NodeStats 获取本节点资源统计。
func (c *Client) NodeStats(ctx context.Context) (NodeStats, error) {
	cctx, cancel := c.callCtx(ctx)
	defer cancel()
	resp, err := c.stub.NodeStats(cctx, &pb.Empty{})
	if err != nil {
		return NodeStats{}, c.wrapErr(err)
	}
	ns := NodeStats{Node: resp.GetNode()}
	for _, ct := range resp.GetContainers() {
		ns.Containers = append(ns.Containers, ContainerStat{
			ContainerID: ct.GetContainerId(), Service: ct.GetService(), TaskID: ct.GetTaskId(),
			CPUPercent: ct.GetCpuPercent(), MemPercent: ct.GetMemPercent(),
			MemUsage: ct.GetMemUsage(), MemLimit: ct.GetMemLimit(),
		})
	}
	return ns, nil
}

// ContainerInfo 对应 Worker NodeContainers 的单个容器条目（swarm 任务容器 +
// 宿主机 standalone 容器，如 r-nacos/grafana/nginxwebui）。
type ContainerInfo struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Image   string `json:"image"`
	State   string `json:"state"`
	Type    string `json:"type"`    // service | standalone
	Service string `json:"service"` // swarm service name when type=service
	Ports   string `json:"ports"`
}

// NodeContainers 获取指定节点上的全部容器（docker ps -a 等价）。
func (c *Client) NodeContainers(ctx context.Context, nodeID string) ([]ContainerInfo, error) {
	cctx, cancel := c.callCtx(ctx)
	defer cancel()
	resp, err := c.stub.NodeContainers(cctx, &pb.NodeContainersRequest{NodeId: nodeID})
	if err != nil {
		return nil, c.wrapErr(err)
	}
	out := make([]ContainerInfo, 0, len(resp.GetContainers()))
	for _, ct := range resp.GetContainers() {
		out = append(out, ContainerInfo{
			ID: ct.GetId(), Name: ct.GetName(), Image: ct.GetImage(),
			State: ct.GetState(), Type: ct.GetType(), Service: ct.GetService(),
			Ports: ct.GetPorts(),
		})
	}
	return out, nil
}

// RestartContainer 重启指定节点上的容器（standalone docker run 容器）。
// nodeID 为 node ID 或 hostname（worker 的 ResolveNodeAddr 语义）。
func (c *Client) RestartContainer(ctx context.Context, nodeID, container string) error {
	cctx, cancel := c.callCtx(ctx)
	defer cancel()
	resp, err := c.stub.RestartContainer(cctx, &pb.ContainerActionRequest{NodeId: nodeID, Container: container})
	if err != nil {
		return c.wrapErr(err)
	}
	if !resp.GetOk() {
		return fmt.Errorf("restart container %q on %s: %s", container, nodeID, resp.GetMessage())
	}
	return nil
}

// ListNodes 获取集群节点列表。
// ListNodes 获取集群节点列表。启用缓存时先返回缓存快照（避免页面白屏），
// 后台刷新并更新缓存。
func (c *Client) ListNodes(ctx context.Context) ([]Node, error) {
	if c.cache != nil {
		if cached := c.cache.GetClusterCache(c.clusterName, "nodes"); cached != nil {
			var out []Node
			if err := json.Unmarshal(cached.Payload, &out); err == nil {
				return out, nil
			}
		}
	}
	cctx, cancel := c.callCtx(ctx)
	defer cancel()
	resp, err := c.stub.ListNodes(cctx, &pb.Empty{})
	if err != nil {
		return nil, c.wrapErr(err)
	}
	out := make([]Node, 0, len(resp.GetNodes()))
	for _, n := range resp.GetNodes() {
		out = append(out, Node{
			ID: n.GetId(), Hostname: n.GetHostname(), Role: n.GetRole(), State: n.GetState(),
			Availability: n.GetAvailability(), Addr: n.GetAddr(), Leader: n.GetLeader(),
			ManagerReach: n.GetManagerReach(), Reachable: n.GetReachable(),
			CPUCores: n.GetCpuCores(), MemBytes: n.GetMemBytes(),
			CPUPercent: n.GetCpuPercent(), MemPercent: n.GetMemPercent(),
			ContainerCount: int(n.GetContainerCount()),
		})
	}
	if c.cache != nil {
		if raw, err := json.Marshal(out); err == nil {
			_ = c.cache.PutClusterCache(c.clusterName, "nodes", raw)
		}
	}
	return out, nil
}

// ListProcesses 获取指定节点的宿主机进程。
func (c *Client) ListProcesses(ctx context.Context, nodeID, top string, limit int, filter string) (ProcessesResp, error) {
	cctx, cancel := c.callCtx(ctx)
	defer cancel()
	resp, err := c.stub.NodeProcesses(cctx, &pb.NodeProcessesRequest{
		NodeId: nodeID, Top: top, Limit: int32(limit), Filter: filter,
	})
	if err != nil {
		return ProcessesResp{}, c.wrapErr(err)
	}
	out := ProcessesResp{Node: resp.GetNode(), Total: int(resp.GetTotal())}
	for _, p := range resp.GetProcesses() {
		out.Processes = append(out.Processes, Process{
			PID: int(p.GetPid()), Name: p.GetName(), Cmdline: p.GetCmdline(),
			State: p.GetState(), MemKB: p.GetMemKb(), CPUPercent: p.GetCpuPercent(),
		})
	}
	return out, nil
}

// CheckPort 从指定节点发起一次性 TCP 探测。
func (c *Client) CheckPort(ctx context.Context, nodeID, host string, port int, timeout string) (PortCheckResult, error) {
	cctx, cancel := c.callCtx(ctx)
	defer cancel()
	resp, err := c.stub.CheckPort(cctx, &pb.CheckPortRequest{
		NodeId: nodeID, Host: host, Port: int32(port), Timeout: timeout,
	})
	if err != nil {
		return PortCheckResult{}, c.wrapErr(err)
	}
	return PortCheckResult{
		Node: resp.GetNode(), Host: resp.GetHost(), Port: resp.GetPort(),
		OK: resp.GetOk(), LatencyMS: resp.GetLatencyMs(), Error: resp.GetError(),
	}, nil
}

// CheckHTTP 从指定节点发起一次性 HTTP 探测。
func (c *Client) CheckHTTP(ctx context.Context, nodeID string, req HTTPCheckRequest) (HTTPCheckResult, error) {
	cctx, cancel := c.callCtx(ctx)
	defer cancel()
	resp, err := c.stub.CheckHTTP(cctx, &pb.CheckHTTPRequest{
		NodeId: nodeID, Url: req.URL, Method: req.Method, Headers: req.Headers,
		ExpectedStatus: intsToInt32s(req.ExpectedStatus), ExpectedBody: req.ExpectedBody, Timeout: req.Timeout,
	})
	if err != nil {
		return HTTPCheckResult{}, c.wrapErr(err)
	}
	return HTTPCheckResult{
		Node: resp.GetNode(), URL: resp.GetUrl(), OK: resp.GetOk(), Status: int(resp.GetStatus()),
		LatencyMS: resp.GetLatencyMs(), Error: resp.GetError(),
	}, nil
}

// CheckFlow 从指定节点发起多步 HTTP 事务探测。
func (c *Client) CheckFlow(ctx context.Context, nodeID string, req FlowCheckRequest) (FlowCheckResult, error) {
	cctx, cancel := c.callCtx(ctx)
	defer cancel()
	var steps []*pb.FlowStep
	for _, s := range req.Steps {
		steps = append(steps, &pb.FlowStep{
			Name: s.Name, Url: s.URL, Method: s.Method, Headers: s.Headers, Body: s.Body,
			ExpectStatus: intsToInt32s(s.ExpectStatus), ExpectBody: s.ExpectBody, Extract: s.Extract,
		})
	}
	resp, err := c.stub.CheckFlow(cctx, &pb.CheckFlowRequest{
		NodeId: nodeID, Steps: steps, Vars: req.Vars, Timeout: req.Timeout,
	})
	if err != nil {
		return FlowCheckResult{}, c.wrapErr(err)
	}
	out := FlowCheckResult{
		Node: resp.GetNode(), OK: resp.GetOk(), FailedStep: resp.GetFailedStep(),
		LatencyMS: resp.GetLatencyMs(), Error: resp.GetError(),
	}
	for _, sr := range resp.GetSteps() {
		out.Steps = append(out.Steps, FlowStepResult{
			Name: sr.GetName(), OK: sr.GetOk(), Status: int(sr.GetStatus()),
			LatencyMS: sr.GetLatencyMs(), Extracted: sr.GetExtracted(), Error: sr.GetError(),
		})
	}
	return out, nil
}

// Events 获取监控事件（原始 JSON，P3 消费）。
func (c *Client) Events(ctx context.Context, service, typ string, afterSeq int64, limit int) (json.RawMessage, error) {
	cctx, cancel := c.callCtx(ctx)
	defer cancel()
	resp, err := c.stub.ListEvents(cctx, &pb.ListEventsRequest{
		Service: service, Type: typ, Limit: int32(limit), AfterSeq: afterSeq,
	})
	if err != nil {
		return nil, c.wrapErr(err)
	}
	return json.RawMessage(resp.GetEventsJson()), nil
}

// Audit 获取审计记录（原始 JSON）。
func (c *Client) Audit(ctx context.Context, action string, limit int) (json.RawMessage, error) {
	cctx, cancel := c.callCtx(ctx)
	defer cancel()
	resp, err := c.stub.ListAudit(cctx, &pb.ListAuditRequest{Action: action, Limit: int32(limit)})
	if err != nil {
		return nil, c.wrapErr(err)
	}
	return json.RawMessage(resp.GetEntriesJson()), nil
}

// intsToInt32s converts a Go int slice to the protobuf int32 slice used by the
// check request types.
func intsToInt32s(in []int) []int32 {
	if len(in) == 0 {
		return nil
	}
	out := make([]int32, len(in))
	for i, v := range in {
		out[i] = int32(v)
	}
	return out
}
