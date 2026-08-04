// Package server embeds the AiNexus AI gateway into the OpsGaurdWeb
// management plane. Vendored from ./AiNexus (repo root) and adapted for
// single-process embedding: the standalone HTTP layer (own port, own
// router, own API-key auth, static /web) is stripped — the gateway's gin
// handlers are mounted directly on the management router, and the
// management-plane auth covers everything. MCP clients, providers,
// tools and the ReAct agent run in-process, unchanged.
package server

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/gin-gonic/gin"

	ainexuscfg "gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/config"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/handler"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/mcp"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/provider"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/tool"
)

// Server 内嵌 AI 网关（无独立 HTTP 层）
type Server struct {
	config      *ainexuscfg.Config
	providers   []provider.Provider          // 所有已注册的 provider
	modelRoutes map[string]provider.Provider // model名称 -> provider 快速路由
	registry    *tool.Registry
	mcpMgr      *mcp.Manager
	logger      *log.Logger

	openaiH    *handler.OpenAIHandler
	anthropicH *handler.AnthropicHandler
}

// New 创建内嵌网关
func New(cfg *ainexuscfg.Config) *Server {
	return &Server{
		config:      cfg,
		providers:   make([]provider.Provider, 0),
		modelRoutes: make(map[string]provider.Provider),
		registry:    tool.NewRegistry(),
		mcpMgr:      mcp.NewManager(log.New(os.Stderr, "[OpsGaurdWeb.AiNexus.MCP] ", log.LstdFlags|log.Lshortfile)),
		logger:      log.New(os.Stderr, "[OpsGaurdWeb.AiNexus] ", log.LstdFlags|log.Lshortfile),
	}
}

// Initialize 初始化所有模块（providers → tools → MCP servers → handlers）
func (s *Server) Initialize(ctx context.Context) error {
	if err := s.initProviders(); err != nil {
		return fmt.Errorf("init providers: %w", err)
	}

	if err := s.initTools(); err != nil {
		return fmt.Errorf("init tools: %w", err)
	}

	if err := s.mcpMgr.Initialize(ctx, s.config.MCPServers); err != nil {
		s.logger.Printf("Warning: MCP initialization partially failed: %v", err)
	}

	if err := s.mcpMgr.RegisterAllTools(s.registry); err != nil {
		s.logger.Printf("Warning: MCP tool registration failed: %v", err)
	}

	// 构建 gin handlers（管理端路由直接挂载）
	s.openaiH = handler.NewOpenAIHandler(s.modelRoutes, s.registry, *s.config, s.logger)
	s.anthropicH = handler.NewAnthropicHandler(s.modelRoutes, s.registry, *s.config, s.logger)

	s.logger.Printf("AiNexus embedded: %d providers, %d models, %d tools, %d MCP servers",
		len(s.providers), len(s.modelRoutes), s.registry.ToolCount(), len(s.mcpMgr.ServerNames()))
	return nil
}

// initProviders 根据配置创建所有 Provider
func (s *Server) initProviders() error {
	for _, pc := range s.config.Providers {
		modelNames := make([]string, 0, len(pc.Models))
		for _, m := range pc.Models {
			modelNames = append(modelNames, m.Name)
		}
		if len(modelNames) == 0 {
			return fmt.Errorf("provider %q has no models configured", pc.Name)
		}

		var p provider.Provider
		switch pc.Type {
		case ainexuscfg.ProviderTypeOpenAI:
			p = provider.NewOpenAIProvider(pc.Name, pc.BaseURL, pc.APIKey, modelNames)
		case ainexuscfg.ProviderTypeAnthropic:
			p = provider.NewAnthropicProvider(pc.Name, pc.BaseURL, pc.APIKey, modelNames)
		default:
			return fmt.Errorf("unsupported provider type: %s", pc.Type)
		}

		s.providers = append(s.providers, p)
		for _, m := range modelNames {
			if _, exists := s.modelRoutes[m]; exists {
				return fmt.Errorf("model %q is already registered by another provider", m)
			}
			s.modelRoutes[m] = p
			s.logger.Printf("  Model routed: %s -> %s (%s)", m, pc.Name, pc.Type)
		}
		s.logger.Printf("Provider registered: %s (type=%s, base_url=%s, models=%v)",
			pc.Name, pc.Type, pc.BaseURL, modelNames)
	}

	if len(s.providers) == 0 {
		return fmt.Errorf("no providers configured")
	}
	return nil
}

// initTools 注册内置工具
func (s *Server) initTools() error {
	if s.config.Tools.Command.Enabled {
		s.registry.MustRegister(tool.NewCommandExecutor(s.config.Tools.Command))
		s.logger.Printf("Tool registered: command_executor")
	}
	if s.config.Tools.HTTPRequest.Enabled {
		s.registry.MustRegister(tool.NewHTTPRequestTool(s.config.Tools.HTTPRequest))
		s.logger.Printf("Tool registered: http_request")
	}
	if s.config.Tools.FileRead.Enabled {
		s.registry.MustRegister(tool.NewFileReadTool(s.config.Tools.FileRead))
		s.logger.Printf("Tool registered: file_read")
	}
	return nil
}

// --- 供管理端路由挂载的 gin handlers ---

// OpenAIHandler 返回 OpenAI 格式处理器（/v1/chat/completions、/v1/models）
func (s *Server) OpenAIHandler() *handler.OpenAIHandler { return s.openaiH }

// AnthropicHandler 返回 Anthropic 格式处理器（/v1/messages）
func (s *Server) AnthropicHandler() *handler.AnthropicHandler { return s.anthropicH }

// Models 返回所有可用模型名（模型选择器数据源）
func (s *Server) Models() []string {
	models := make([]string, 0, len(s.modelRoutes))
	for m := range s.modelRoutes {
		models = append(models, m)
	}
	return models
}

// MCPNames 返回已连接的 MCP Server 名称
func (s *Server) MCPNames() []string { return s.mcpMgr.ServerNames() }

// AddMCPCluster 动态连接一个集群的 Worker MCP（streamable-http + Bearer
// token），使 ReAct Agent 在排查时可调用该集群 Worker 的 16 工具采集证据。
// 幂等：同名集群已连接则直接返回。Worker /mcp 即 Streamable HTTP 传输。
// 连接成功后把该 server 的工具注册进 registry（RegisterAllTools 幂等，
// 已注册的同名工具跳过）。
func (s *Server) AddMCPCluster(name, url, token string) error {
	serverName := "cluster:" + name
	for _, n := range s.mcpMgr.ServerNames() {
		if n == serverName {
			return nil // 已连接
		}
	}
	if url == "" {
		return fmt.Errorf("mcp url is empty for cluster %q", name)
	}
	cfg := ainexuscfg.MCPServerConfig{
		Name:      serverName,
		Transport: "streamable-http",
		URL:       url,
	}
	if token != "" {
		cfg.Headers = map[string]string{"Authorization": "Bearer " + token}
	}
	if err := s.mcpMgr.AddServer(context.Background(), cfg); err != nil {
		return err
	}
	return s.mcpMgr.RegisterAllTools(s.registry)
}

// ToolCount 返回已注册工具数
func (s *Server) ToolCount() int { return s.registry.ToolCount() }

// HealthHandler godoc: GET /ainexus/health
// 内嵌网关健康状态（providers/models/tools/mcp_servers）
func (s *Server) HealthHandler(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status":      "ok",
		"embedded":    true,
		"providers":   len(s.providers),
		"models":      len(s.modelRoutes),
		"tools":       s.registry.ToolCount(),
		"mcp_servers": s.mcpMgr.ServerNames(),
	})
}

// ModelsHandler godoc: GET /ainexus/api/models
// 模型详情（名称/provider/类型），兼容 AiNexus 原生 /api/models 契约
func (s *Server) ModelsHandler(c *gin.Context) {
	type modelInfo struct {
		Name     string `json:"name"`
		Provider string `json:"provider"`
		Type     string `json:"type"`
	}
	var models []modelInfo
	for _, pc := range s.config.Providers {
		for _, m := range pc.Models {
			models = append(models, modelInfo{Name: m.Name, Provider: pc.Name, Type: string(pc.Type)})
		}
	}
	c.JSON(http.StatusOK, gin.H{"count": len(models), "models": models})
}

// ToolsHandler godoc: GET /ainexus/api/tools
// 工具定义列表，兼容 AiNexus 原生 /api/tools 契约
func (s *Server) ToolsHandler(c *gin.Context) {
	tools := s.registry.ToolDefinitions()
	c.JSON(http.StatusOK, gin.H{"count": len(tools), "tools": tools})
}

// MCPHandler godoc: GET /ainexus/api/mcp
// 已连接的 MCP Server 列表，兼容 AiNexus 原生 /api/mcp 契约
func (s *Server) MCPHandler(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"servers": s.mcpMgr.ServerNames()})
}

// Close 关闭 MCP 连接（进程退出时调用）
func (s *Server) Close() error { return s.mcpMgr.Close() }
