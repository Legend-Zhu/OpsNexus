package mcp

import (
	"context"
	"fmt"
	"log"
	"sync"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/config"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/tool"

	mcpclient "github.com/mark3labs/mcp-go/client"
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
}

// NewManager 创建 MCP 管理器
func NewManager(logger *log.Logger) *Manager {
	return &Manager{
		servers: make(map[string]*MCPServerClient),
		logger:  logger,
	}
}

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

	// 根据传输方式创建客户端
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
		return fmt.Errorf("unsupported transport: %s", cfg.Transport)
	}

	if err != nil {
		return fmt.Errorf("create mcp client for %q: %w", cfg.Name, err)
	}

	// 初始化握手
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

	_, err = client.Initialize(ctx, initReq)
	if err != nil {
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
	client, err := mcpclient.NewSSEMCPClient(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("create SSE client: %w", err)
	}
	return client, nil
}

// createStreamableHTTPClient 创建 Streamable HTTP 传输客户端
func (m *Manager) createStreamableHTTPClient(cfg config.MCPServerConfig) (*mcpclient.Client, error) {
	client, err := mcpclient.NewStreamableHttpClient(cfg.URL)
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

// RegisterAllTools 将所有 MCP 工具注册到 ToolRegistry
func (m *Manager) RegisterAllTools(registry *tool.Registry) error {
	tools := m.Tools()
	for _, t := range tools {
		if err := registry.Register(t); err != nil {
			m.logger.Printf("Warning: failed to register MCP tool %q: %v", t.Name(), err)
			continue
		}
	}
	return nil
}

// Close 关闭所有 MCP Server 连接
func (m *Manager) Close() error {
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
