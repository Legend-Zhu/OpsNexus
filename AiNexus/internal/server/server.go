package server

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/legeosoft/ainexus/internal/config"
	"github.com/legeosoft/ainexus/internal/handler"
	"github.com/legeosoft/ainexus/internal/mcp"
	"github.com/legeosoft/ainexus/internal/provider"
	"github.com/legeosoft/ainexus/internal/tool"
)

// Server HTTP 服务器
type Server struct {
	config       *config.Config
	providers    []provider.Provider          // 所有已注册的 provider
	modelRoutes  map[string]provider.Provider // model名称 -> provider 快速路由
	registry     *tool.Registry
	mcpMgr       *mcp.Manager
	logger       *log.Logger
	httpServer   *http.Server
}

// New 创建服务器
func New(cfg *config.Config) *Server {
	return &Server{
		config:      cfg,
		providers:   make([]provider.Provider, 0),
		modelRoutes: make(map[string]provider.Provider),
		registry:    tool.NewRegistry(),
		mcpMgr:      mcp.NewManager(log.New(os.Stderr, "[AiNexus.MCP] ", log.LstdFlags|log.Lshortfile)),
		logger:      log.New(os.Stderr, "[AiNexus] ", log.LstdFlags|log.Lshortfile),
	}
}

// Initialize 初始化所有模块
func (s *Server) Initialize(ctx context.Context) error {
	// 1. 初始化 Providers
	if err := s.initProviders(); err != nil {
		return fmt.Errorf("init providers: %w", err)
	}

	// 2. 注册内置工具
	if err := s.initTools(); err != nil {
		return fmt.Errorf("init tools: %w", err)
	}

	// 3. 初始化 MCP Servers
	if err := s.mcpMgr.Initialize(ctx, s.config.MCPServers); err != nil {
		s.logger.Printf("Warning: MCP initialization partially failed: %v", err)
	}

	// 4. 将 MCP 工具注册到 Registry
	if err := s.mcpMgr.RegisterAllTools(s.registry); err != nil {
		s.logger.Printf("Warning: MCP tool registration failed: %v", err)
	}

	s.logger.Printf("Initialized: %d providers, %d models, %d tools, %d MCP servers",
		len(s.providers), len(s.modelRoutes), s.registry.ToolCount(), len(s.mcpMgr.ServerNames()))

	return nil
}

// initProviders 根据配置创建所有 Provider
func (s *Server) initProviders() error {
	for _, pc := range s.config.Providers {
		// 提取模型名称列表
		modelNames := make([]string, 0, len(pc.Models))
		for _, m := range pc.Models {
			modelNames = append(modelNames, m.Name)
		}

		if len(modelNames) == 0 {
			return fmt.Errorf("provider %q has no models configured", pc.Name)
		}

		var p provider.Provider

		switch pc.Type {
		case config.ProviderTypeOpenAI:
			p = provider.NewOpenAIProvider(pc.Name, pc.BaseURL, pc.APIKey, modelNames)
		case config.ProviderTypeAnthropic:
			p = provider.NewAnthropicProvider(pc.Name, pc.BaseURL, pc.APIKey, modelNames)
		default:
			return fmt.Errorf("unsupported provider type: %s", pc.Type)
		}

		s.providers = append(s.providers, p)

		// 建立模型名 -> Provider 的路由映射
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

// ResolveProvider 根据 model 名称查找对应的 Provider
func (s *Server) ResolveProvider(model string) (provider.Provider, bool) {
	p, ok := s.modelRoutes[model]
	return p, ok
}

// AllModels 返回所有可用模型名
func (s *Server) AllModels() []string {
	models := make([]string, 0, len(s.modelRoutes))
	for m := range s.modelRoutes {
		models = append(models, m)
	}
	return models
}

// SetupRouter 配置路由
func (s *Server) SetupRouter() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()

	// 中间件
	r.Use(gin.Recovery())
	r.Use(gin.LoggerWithConfig(gin.LoggerConfig{SkipPaths: []string{"/health", "/"}}))

	// CORS（全局，不受鉴权影响）
	r.Use(s.corsMiddleware())

	// 不需要鉴权的路由
	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"status":      "ok",
			"providers":   len(s.providers),
			"models":      len(s.modelRoutes),
			"tools":       s.registry.ToolCount(),
			"mcp_servers": s.mcpMgr.ServerNames(),
		})
	})

	// 静态页面（不需要鉴权）
	r.Static("/web", "./web")
	r.GET("/", func(c *gin.Context) {
		c.File("./web/index.html")
	})

	// API 鉴权中间件（仅对 API 路径生效）
	if s.config.Server.APIKey != "" {
		r.Use(s.apiAuthMiddleware())
	}

	// OpenAI 兼容接口
	openaiH := handler.NewOpenAIHandler(s.modelRoutes, s.registry, *s.config, s.logger)

	v1 := r.Group("/v1")
	{
		v1.POST("/chat/completions", openaiH.ChatCompletions)
		v1.GET("/models", openaiH.Models)
	}

	// Anthropic 兼容接口
	anthropicH := handler.NewAnthropicHandler(s.modelRoutes, s.registry, *s.config, s.logger)
	r.POST("/v1/messages", anthropicH.Messages)

	// 管理接口
	r.GET("/api/tools", func(c *gin.Context) {
		tools := s.registry.ToolDefinitions()
		c.JSON(http.StatusOK, gin.H{"count": len(tools), "tools": tools})
	})
	r.GET("/api/mcp", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"servers": s.mcpMgr.ServerNames()})
	})
	r.GET("/api/models", func(c *gin.Context) {
		type modelInfo struct {
			Name     string `json:"name"`
			Provider string `json:"provider"`
			Type     string `json:"type"`
		}
		var models []modelInfo
		for _, pc := range s.config.Providers {
			for _, m := range pc.Models {
				models = append(models, modelInfo{
					Name:     m.Name,
					Provider: pc.Name,
					Type:     string(pc.Type),
				})
			}
		}
		c.JSON(http.StatusOK, gin.H{"count": len(models), "models": models})
	})

	return r
}

// corsMiddleware CORS 中间件
func (s *Server) corsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Content-Type, Authorization, x-api-key, anthropic-version")
		c.Header("Access-Control-Max-Age", "86400")
		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

// apiAuthMiddleware API Key 鉴权中间件（仅对 /v1/ 和 /api/ 路径生效）
func (s *Server) apiAuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// 跳过非 API 路径
		path := c.Request.URL.Path
		if !strings.HasPrefix(path, "/v1/") && !strings.HasPrefix(path, "/api/") {
			c.Next()
			return
		}

		apiKey := s.config.Server.APIKey

		auth := c.GetHeader("Authorization")
		if auth != "" {
			if len(auth) > 7 && auth[:7] == "Bearer " {
				if auth[7:] == apiKey {
					c.Next()
					return
				}
			}
			if auth == apiKey {
				c.Next()
				return
			}
		}

		if c.GetHeader("x-api-key") == apiKey {
			c.Next()
			return
		}

		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
			"error": gin.H{"message": "Invalid API key", "type": "authentication_error"},
		})
	}
}

// Run 启动服务器
func (s *Server) Run() error {
	r := s.SetupRouter()

	s.httpServer = &http.Server{
		Addr:    s.config.Server.Addr,
		Handler: r,
	}

	go func() {
		quit := make(chan os.Signal, 1)
		signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
		<-quit
		s.logger.Println("Shutting down server...")

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if err := s.httpServer.Shutdown(ctx); err != nil {
			s.logger.Printf("Server shutdown error: %v", err)
		}
		if err := s.mcpMgr.Close(); err != nil {
			s.logger.Printf("MCP manager close error: %v", err)
		}
		s.logger.Println("Server exited")
	}()

	s.logger.Printf("Server starting on %s", s.config.Server.Addr)
	s.logger.Printf("  Chat UI:       http://localhost%s/", s.config.Server.Addr)
	s.logger.Printf("  OpenAI API:    http://localhost%s/v1/chat/completions", s.config.Server.Addr)
	s.logger.Printf("  Anthropic API: http://localhost%s/v1/messages", s.config.Server.Addr)
	s.logger.Printf("  Models:        http://localhost%s/v1/models", s.config.Server.Addr)
	s.logger.Printf("  Available models: %v", s.AllModels())

	if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("server error: %w", err)
	}
	return nil
}

// Shutdown 手动关闭服务器
func (s *Server) Shutdown(ctx context.Context) error {
	if s.httpServer != nil {
		if err := s.httpServer.Shutdown(ctx); err != nil {
			return err
		}
	}
	return s.mcpMgr.Close()
}
