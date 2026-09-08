package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/config"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/tool"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

// MCPServerClient 封装一个 MCP Server 的连接
type MCPServerClient struct {
	Name   string
	config config.MCPServerConfig
	client *mcpclient.Client
	tools  []tool.Tool
	mu     sync.RWMutex
}

// Manager MCP Server 管理器
type Manager struct {
	servers map[string]*MCPServerClient
	mu      sync.RWMutex
	logger  *log.Logger

	// registry 健康自愈时工具重注册/注销的目标（SetRegistry 注入；
	// nil = 只探测连接，不维护 registry）。
	registry *tool.Registry
	// stopCh 健康循环停止信号（StartHealthLoop 创建，Close 关闭）。
	stopCh   chan struct{}
	stopOnce sync.Once
}

// 健康自愈参数：探测周期 30s，单次探测/重建握手超时 10s。设计文档 P2
// 规划的指数退避暂以固定周期重试替代（Worker 重启后最多 30s 恢复采证）。
const (
	defaultHealthInterval = 30 * time.Second
	healthProbeTimeout    = 10 * time.Second
)

// NewManager 创建 MCP 管理器
func NewManager(logger *log.Logger) *Manager {
	return &Manager{
		servers: make(map[string]*MCPServerClient),
		logger:  logger,
	}
}

// SetRegistry 注入工具注册中心（健康自愈的注销/重注册目标）。
func (m *Manager) SetRegistry(r *tool.Registry) { m.registry = r }

// Initialize 初始化所有配置的 MCP Server
func (m *Manager) Initialize(ctx context.Context, configs []config.MCPServerConfig) error {
	for _, cfg := range configs {
		if err := m.AddServer(ctx, cfg); err != nil {
			m.logger.Printf("MCP server %q initialization failed: %v", cfg.Name, err)
			// 不中断其他 server 的初始化
			continue
		}
	}
	return nil
}

// AddServer 添加并初始化一个 MCP Server
func (m *Manager) AddServer(ctx context.Context, cfg config.MCPServerConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.servers[cfg.Name]; exists {
		return fmt.Errorf("mcp server %q already exists", cfg.Name)
	}

	sc := &MCPServerClient{
		Name:   cfg.Name,
		config: cfg,
	}

	// 创建客户端并完成初始化握手
	client, err := m.createClient(cfg)
	if err != nil {
		return err
	}
	if err := handshakeInit(ctx, client); err != nil {
		_ = client.Close()
		return fmt.Errorf("initialize mcp server %q: %w", cfg.Name, err)
	}

	sc.client = client

	// 获取工具列表
	if err := sc.refreshTools(ctx); err != nil {
		m.logger.Printf("Warning: failed to list tools from MCP server %q: %v", cfg.Name, err)
		// 工具列表获取失败不阻止 server 注册
	}

	m.servers[cfg.Name] = sc
	m.logger.Printf("MCP server %q connected (%s), %d tools available",
		cfg.Name, cfg.Transport, len(sc.tools))

	return nil
}

// createClient 按传输方式创建 MCP 客户端。
func (m *Manager) createClient(cfg config.MCPServerConfig) (*mcpclient.Client, error) {
	var client *mcpclient.Client
	var err error
	switch cfg.Transport {
	case "stdio":
		client, err = m.createStdioClient(cfg)
	case "sse":
		client, err = m.createSSEClient(cfg)
	case "streamable-http":
		client, err = m.createStreamableHTTPClient(cfg)
	default:
		return nil, fmt.Errorf("unsupported transport: %s", cfg.Transport)
	}
	if err != nil {
		return nil, fmt.Errorf("create %s client for %q: %w", cfg.Transport, cfg.Name, err)
	}
	return client, nil
}

// handshakeInit 完成 MCP 初始化握手（AddServer 与健康自愈重建共用）。
func handshakeInit(ctx context.Context, client *mcpclient.Client) error {
	initReq := mcp.InitializeRequest{
		Params: mcp.InitializeParams{
			ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
			Capabilities:    mcp.ClientCapabilities{},
			ClientInfo: mcp.Implementation{
				Name:    "AiNexus",
				Version: "1.0.0",
			},
		},
	}
	_, err := client.Initialize(ctx, initReq)
	return err
}

// createStdioClient 创建 stdio 传输客户端
func (m *Manager) createStdioClient(cfg config.MCPServerConfig) (*mcpclient.Client, error) {
	env := make([]string, 0, len(cfg.Env)*2)
	for k, v := range cfg.Env {
		env = append(env, k+"="+v)
	}
	client, err := mcpclient.NewStdioMCPClient(cfg.Command, env, cfg.Args...)
	if err != nil {
		return nil, fmt.Errorf("create stdio client: %w", err)
	}
	return client, nil
}

// createSSEClient 创建 SSE 传输客户端
func (m *Manager) createSSEClient(cfg config.MCPServerConfig) (*mcpclient.Client, error) {
	var opts []transport.ClientOption
	if len(cfg.Headers) > 0 {
		opts = append(opts, transport.WithHeaders(cfg.Headers))
	}
	client, err := mcpclient.NewSSEMCPClient(cfg.URL, opts...)
	if err != nil {
		return nil, fmt.Errorf("create SSE client: %w", err)
	}
	return client, nil
}

// createStreamableHTTPClient 创建 Streamable HTTP 传输客户端。挂载
// outputSchema 剥除传输层：Worker go-sdk 生成的数组形 "type" outputSchema
// 会导致 mark3labs 客户端 tools/list 整包解码失败（见 transport_strip.go），
// 在传输层统一消解，Worker 侧零改动。
func (m *Manager) createStreamableHTTPClient(cfg config.MCPServerConfig) (*mcpclient.Client, error) {
	var opts []transport.StreamableHTTPCOption
	if len(cfg.Headers) > 0 {
		opts = append(opts, transport.WithHTTPHeaders(cfg.Headers))
	}
	opts = append(opts, transport.WithHTTPBasicClient(&http.Client{
		Transport: schemaStrippingTransport{base: http.DefaultTransport},
	}))
	client, err := mcpclient.NewStreamableHttpClient(cfg.URL, opts...)
	if err != nil {
		return nil, fmt.Errorf("create StreamableHTTP client: %w", err)
	}
	return client, nil
}

// refreshTools 刷新 MCP Server 的工具列表
func (sc *MCPServerClient) refreshTools(ctx context.Context) error {
	sc.mu.Lock()
	defer sc.mu.Unlock()

	result, err := sc.client.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		return fmt.Errorf("list tools: %w", err)
	}

	sc.tools = make([]tool.Tool, 0, len(result.Tools))
	for _, t := range result.Tools {
		mcpTool := &MCPTool{
			serverName:  sc.Name,
			toolName:    t.Name,
			toolDesc:    t.Description,
			inputSchema: t.InputSchema,
			client:      sc.client,
		}
		sc.tools = append(sc.tools, mcpTool)
	}

	return nil
}

// Tools 返回所有 MCP Server 的工具
func (m *Manager) Tools() []tool.Tool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var allTools []tool.Tool
	for _, sc := range m.servers {
		sc.mu.RLock()
		allTools = append(allTools, sc.tools...)
		sc.mu.RUnlock()
	}
	return allTools
}

// CallServerTool 对指定已连接 MCP server 直接调用一个工具（不走 agent）。
// 管理端 MCP Server 的 exec 透传等平台级编排使用；serverName 形如
// "cluster:prod"。返回拼接后的文本内容与 isError 标记（内容截断由调用方
// 按自己的预算执行）。
func (m *Manager) CallServerTool(ctx context.Context, serverName, toolName string, args map[string]any) (string, bool, error) {
	m.mu.RLock()
	sc, ok := m.servers[serverName]
	m.mu.RUnlock()
	if !ok || sc == nil {
		return "", false, fmt.Errorf("mcp server %q not connected", serverName)
	}
	sc.mu.RLock()
	cli := sc.client
	sc.mu.RUnlock()
	if cli == nil {
		return "", false, fmt.Errorf("mcp server %q client not ready", serverName)
	}
	res, err := cli.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: toolName, Arguments: args},
	})
	if err != nil {
		return "", false, fmt.Errorf("call %s/%s: %w", serverName, toolName, err)
	}
	var sb strings.Builder
	for _, c := range res.Content {
		switch v := c.(type) {
		case mcp.TextContent:
			sb.WriteString(v.Text)
		default:
			b, _ := json.Marshal(v)
			sb.Write(b)
		}
	}
	return sb.String(), res.IsError, nil
}

// RegisterAllTools 将所有 MCP 工具注册到 ToolRegistry。
// 语义幂等：同一 server 的重复全量注册（多 server 逐个接入时各自触发一次
// 全量）按同源跳过——同名且 Description 同源（[MCP:<server>] 前缀一致）视为
// 已注册的同一工具，静默跳过；同名不同源才是真冲突（清洗后命名挤占，该
// 工具对 agent 不可见），记 ERROR 留痕并检查 server 命名。
func (m *Manager) RegisterAllTools(registry *tool.Registry) error {
	m.registerAll(registry, m.Tools())
	return nil
}

func (m *Manager) registerAll(registry *tool.Registry, tools []tool.Tool) {
	for _, t := range tools {
		if existing, ok := registry.Get(t.Name()); ok {
			if existing.Description() == t.Description() {
				continue
			}
			m.logger.Printf("ERROR: MCP tool registration conflict on %q: %q (%v) conflicts with existing registration "+
				"(tool not visible to agent; check server naming)", t.Name(), t.Description(), existing.Description())
			continue
		}
		if err := registry.Register(t); err != nil {
			if mt, ok := t.(*MCPTool); ok {
				m.logger.Printf("ERROR: MCP tool registration failed: server %q tool %q: %v "+
					"(tool not visible to agent; check server naming)", mt.serverName, t.Name(), err)
			} else {
				m.logger.Printf("ERROR: failed to register MCP tool %q: %v", t.Name(), err)
			}
		}
	}
}

// RemoveServer 断开并移除一个 MCP server，其工具一并从 registry 注销
// （集群删除时调用，防"幽灵工具"）。server 不存在时幂等返回 nil。
func (m *Manager) RemoveServer(name string) error {
	m.mu.Lock()
	sc, ok := m.servers[name]
	if !ok {
		m.mu.Unlock()
		return nil
	}
	delete(m.servers, name)
	tools := sc.toolsSnapshot()
	m.mu.Unlock()

	if m.registry != nil {
		for _, t := range tools {
			m.registry.Unregister(t.Name())
		}
	}
	if err := sc.client.Close(); err != nil {
		return fmt.Errorf("close mcp server %q: %w", name, err)
	}
	m.logger.Printf("MCP server %q removed (%d tools unregistered)", name, len(tools))
	return nil
}

// StartHealthLoop 启动健康自愈循环（幂等）：周期性对每个 server 执行
// ListTools 探测——成功则增量同步工具清单变更（新增注册/移除注销）；
// 失败则整连接重建（关旧客户端 → 重新握手 → 重注册工具），覆盖 Worker
// 重启/网络抖动后的采证自愈。Close 停止循环。
func (m *Manager) StartHealthLoop(interval time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopCh != nil {
		return
	}
	stop := make(chan struct{})
	m.stopCh = stop
	go func() {
		if interval <= 0 {
			interval = defaultHealthInterval
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				m.healthCheckOnce()
			}
		}
	}()
}

// healthCheckOnce 单轮健康检查：逐 server 探测 → 失败重建 → 成功刷新工具清单。
func (m *Manager) healthCheckOnce() {
	for _, name := range m.ServerNames() {
		m.mu.RLock()
		sc := m.servers[name]
		m.mu.RUnlock()
		if sc == nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), healthProbeTimeout)
		if err := sc.probe(ctx); err != nil {
			m.logger.Printf("mcp server %q probe failed (%v), rebuilding connection", name, err)
			m.rebuildServer(ctx, sc)
		} else {
			m.refreshServerTools(ctx, sc)
		}
		cancel()
	}
}

// probe 以 ListTools 作轻量连通性探测。
func (sc *MCPServerClient) probe(ctx context.Context) error {
	sc.mu.RLock()
	client := sc.client
	sc.mu.RUnlock()
	if client == nil {
		return fmt.Errorf("mcp server %q has no client", sc.Name)
	}
	_, err := client.ListTools(ctx, mcp.ListToolsRequest{})
	return err
}

// refreshServerTools 连接正常时同步工具清单变更：Worker 侧新增的工具注册
// 进 registry，被移除的注销（防幽灵工具）。既有工具静默保留。
func (m *Manager) refreshServerTools(ctx context.Context, sc *MCPServerClient) {
	if m.registry == nil {
		return
	}
	oldNames := map[string]bool{}
	for _, t := range sc.toolsSnapshot() {
		oldNames[t.Name()] = true
	}
	if err := sc.refreshTools(ctx); err != nil {
		return // 下个周期重试
	}
	fresh := sc.toolsSnapshot()
	newNames := map[string]bool{}
	for _, t := range fresh {
		newNames[t.Name()] = true
	}
	for _, t := range fresh {
		if oldNames[t.Name()] {
			continue
		}
		if err := m.registry.Register(t); err != nil {
			m.logger.Printf("Warning: register refreshed tool %q: %v", t.Name(), err)
		} else {
			m.logger.Printf("mcp server %q tool added: %s", sc.Name, t.Name())
		}
	}
	for n := range oldNames {
		if !newNames[n] && m.registry.Unregister(n) {
			m.logger.Printf("mcp server %q tool removed: %s", sc.Name, n)
		}
	}
}

// rebuildServer 重建一个 server 的连接（Worker 重启/网络抖动后自愈）：
// 先注销其全部工具（摘除悬空客户端的调用面），再重建客户端并重新握手，
// 成功后重注册工具。失败保留 server 条目（工具已摘除），下个周期重试。
func (m *Manager) rebuildServer(ctx context.Context, sc *MCPServerClient) {
	name := sc.Name
	if m.registry != nil {
		for _, t := range sc.toolsSnapshot() {
			m.registry.Unregister(t.Name())
		}
	}
	sc.mu.RLock()
	old := sc.client
	sc.mu.RUnlock()
	if old != nil {
		_ = old.Close()
	}

	client, err := m.createClient(sc.config)
	if err != nil {
		m.logger.Printf("ERROR: mcp server %q rebuild failed: %v (tools unregistered, retry next cycle)", name, err)
		return
	}
	if err := handshakeInit(ctx, client); err != nil {
		_ = client.Close()
		m.logger.Printf("ERROR: mcp server %q rebuild handshake failed: %v (retry next cycle)", name, err)
		return
	}
	sc.mu.Lock()
	sc.client = client
	sc.mu.Unlock()
	if err := sc.refreshTools(ctx); err != nil {
		m.logger.Printf("Warning: mcp server %q reconnected but list tools failed: %v", name, err)
	}
	if m.registry != nil {
		for _, t := range sc.toolsSnapshot() {
			if err := m.registry.Register(t); err != nil {
				m.logger.Printf("Warning: re-register tool %q after rebuild: %v", t.Name(), err)
			}
		}
	}
	m.logger.Printf("MCP server %q reconnected (%d tools)", name, len(sc.toolsSnapshot()))
}

// toolsSnapshot 返回工具列表快照。
func (sc *MCPServerClient) toolsSnapshot() []tool.Tool {
	sc.mu.RLock()
	defer sc.mu.RUnlock()
	return append([]tool.Tool(nil), sc.tools...)
}

// Close 关闭所有 MCP Server 连接（并停止健康自愈循环）
func (m *Manager) Close() error {
	m.stopOnce.Do(func() {
		m.mu.Lock()
		if m.stopCh != nil {
			close(m.stopCh)
			m.stopCh = nil
		}
		m.mu.Unlock()
	})
	m.mu.Lock()
	defer m.mu.Unlock()

	var errs []error
	for name, sc := range m.servers {
		if err := sc.client.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close mcp server %q: %w", name, err))
		}
		m.logger.Printf("MCP server %q closed", name)
	}
	m.servers = make(map[string]*MCPServerClient)

	if len(errs) > 0 {
		return fmt.Errorf("errors closing mcp servers: %v", errs)
	}
	return nil
}

// ServerNames 返回所有已连接的 MCP Server 名称
func (m *Manager) ServerNames() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	names := make([]string, 0, len(m.servers))
	for name := range m.servers {
		names = append(names, name)
	}
	return names
}
