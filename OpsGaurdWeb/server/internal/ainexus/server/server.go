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
	"strings"
	"text/template"

	"github.com/gin-gonic/gin"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/agent"
	ainexuscfg "gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/config"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/handler"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/mcp"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/provider"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/tool"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/usage"
)

// ScenarioPatrolSystem 巡检报告提示词场景 key（与 mlops Prompt Hub 同名约定）。
const ScenarioPatrolSystem = "patrol_system"

// patrolSystemPrompt 巡检报告内置 system 消息（与 mlops 内置 v1 模板等价，
// 由回归测试守护一致）。报告经飞书 post 富文本逐行展示，引导模型用列表
// 而非表格/样式标记（转换器仍会兜底降级）。
const patrolSystemPrompt = "你是智能运维巡检报告助手。基于巡检检查结果，给出简明、结构化的报告：异常概况、逐项说明、处置建议。不要编造数据。报告将在飞书通知中逐行展示：用短段落和以 - 开头的列表组织内容，不要输出 Markdown 表格，不要使用 # 标题、** 加粗、> 引用等标记。"

// PromptSource 提示词场景模板来源（mlops 运营层注入；nil = 代码内置默认）。
// 引擎只依赖此最小接口，不依赖 mlops 包。
type PromptSource interface {
	// ScenarioTemplate 返回场景 active 版本首条消息的模板原文；
	// ok=false 表示未自定义（用内置默认）。
	ScenarioTemplate(scenario string) (string, bool)
}

// Server 内嵌 AI 网关（无独立 HTTP 层）
type Server struct {
	config      *ainexuscfg.Config
	providers   []provider.Provider          // 所有已注册的 provider
	modelRoutes map[string]provider.Provider // model名称 -> provider 快速路由（仅启用模型）
	// enabledModels 启用模型按配置声明顺序（providers[].models 出现顺序），
	// 作为默认模型缺失/未指定时的稳定 fallback——不依赖 map 遍历顺序。
	enabledModels []string
	// disabledModels 配置中存在但被运营层禁用的模型（诊断用：区别于未知模型）。
	disabledModels map[string]bool
	registry       *tool.Registry
	mcpMgr         *mcp.Manager
	logger         *log.Logger

	openaiH    *handler.OpenAIHandler
	anthropicH *handler.AnthropicHandler

	// usageSink 底层调用计量 sink（nil = 不计量；见 WithUsageSink）。
	usageSink usage.Sink
	// promptSource 提示词场景模板来源（nil = 代码内置默认；见 WithPromptSource）。
	promptSource PromptSource
}

// Option 内嵌网关构建选项。
type Option func(*Server)

// WithUsageSink 为网关所有底层 LLM 调用挂共享计量 sink（nil = 不计量）。
// sink 生命周期由管理端运行时服务持有：热重载构建新网关实例时复用同一
// sink，保证配置切换前后计量不中断。
func WithUsageSink(sink usage.Sink) Option {
	return func(s *Server) { s.usageSink = sink }
}

// WithPromptSource 注入提示词场景模板来源（mlops 运营层；nil = 内置默认）。
// 与 usageSink 同样由外层服务持有，热重载复用。
func WithPromptSource(src PromptSource) Option {
	return func(s *Server) { s.promptSource = src }
}

// New 创建内嵌网关
func New(cfg *ainexuscfg.Config, opts ...Option) *Server {
	s := &Server{
		config:         cfg,
		providers:      make([]provider.Provider, 0),
		modelRoutes:    make(map[string]provider.Provider),
		disabledModels: make(map[string]bool),
		registry:       tool.NewRegistry(),
		mcpMgr:         mcp.NewManager(log.New(os.Stderr, "[OpsGaurdWeb.AiNexus.MCP] ", log.LstdFlags|log.Lshortfile)),
		logger:         log.New(os.Stderr, "[OpsGaurdWeb.AiNexus] ", log.LstdFlags|log.Lshortfile),
	}
	for _, opt := range opts {
		opt(s)
	}
	// 健康自愈的注销/重注册目标（先于 Initialize/MCP 连接注入）
	s.mcpMgr.SetRegistry(s.registry)
	return s
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

	// 健康自愈循环（探测周期 30s）：Worker 重启/网络抖动后自动重建连接
	// 并同步工具清单变更；Server.Close → mcpMgr.Close 停止。
	s.mcpMgr.StartHealthLoop(0)

	// 构建 gin handlers（管理端路由直接挂载）
	s.openaiH = handler.NewOpenAIHandler(s.modelRoutes, s.registry, *s.config, s.logger, s.promptSource)
	s.anthropicH = handler.NewAnthropicHandler(s.modelRoutes, s.registry, *s.config, s.logger, s.promptSource)

	s.logger.Printf("AiNexus embedded: %d providers, %d models, %d tools, %d MCP servers",
		len(s.providers), len(s.modelRoutes), s.registry.ToolCount(), len(s.mcpMgr.ServerNames()))
	return nil
}

// initProviders 根据配置创建所有 Provider。禁用模型（enabled=false，
// MLOps 运营层）不进路由，但保留在 disabledModels 供诊断；一个 provider
// 的全部模型都被禁用时 provider 仍注册（模型列表为空）。
func (s *Server) initProviders() error {
	for _, pc := range s.config.Providers {
		modelNames := make([]string, 0, len(pc.Models))
		for _, m := range pc.Models {
			if !m.Enabled {
				s.disabledModels[m.Name] = true
				s.logger.Printf("  Model disabled (skipped): %s -> %s", m.Name, pc.Name)
				continue
			}
			modelNames = append(modelNames, m.Name)
		}
		if len(pc.Models) == 0 {
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

		// 计量包装：每次底层调用（非流式一次/流式一整条）产生一条记录；
		// 上下文无计量元数据时自动透传不记录。
		if s.usageSink != nil {
			p = usage.NewMetered(p, s.usageSink)
		}

		s.providers = append(s.providers, p)
		for _, m := range modelNames {
			if _, exists := s.modelRoutes[m]; exists {
				return fmt.Errorf("model %q is already registered by another provider", m)
			}
			s.modelRoutes[m] = p
			s.enabledModels = append(s.enabledModels, m)
			s.logger.Printf("  Model routed: %s -> %s (%s)", m, pc.Name, pc.Type)
		}
		s.logger.Printf("Provider registered: %s (type=%s, base_url=%s, models=%v)",
			pc.Name, pc.Type, pc.BaseURL, modelNames)
	}

	if len(s.providers) == 0 {
		return fmt.Errorf("no providers configured")
	}
	if len(s.enabledModels) == 0 {
		return fmt.Errorf("no enabled models: all models are disabled by mlops operations")
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

// Models 返回所有可用（启用）模型名，按配置声明顺序稳定返回
// （模型选择器数据源；不包含被运营层禁用的模型）。
func (s *Server) Models() []string {
	out := make([]string, len(s.enabledModels))
	copy(out, s.enabledModels)
	return out
}

// IsModelRoutable 模型是否在当前路由表中（启用）。
func (s *Server) IsModelRoutable(model string) bool {
	_, ok := s.modelRoutes[model]
	return ok
}

// IsModelDisabled 模型是否因运营层禁用而被跳过（区别于未配置的未知模型）。
func (s *Server) IsModelDisabled(model string) bool {
	return s.disabledModels[model]
}

// TestModel 对一个已启用模型发一次最小真实请求（MLOps 显式健康测试）。
// 走当前路由（含计量包装），计量归属 scenario=health。
func (s *Server) TestModel(ctx context.Context, model string) error {
	p, ok := s.modelRoutes[model]
	if !ok {
		if s.disabledModels[model] {
			return fmt.Errorf("model %q is disabled", model)
		}
		return fmt.Errorf("model %q not found", model)
	}
	ctx = usage.NewOperation(ctx, usage.ScenarioHealth, "mlops_model_health")
	_, err := p.ChatCompletion(ctx, &provider.ChatRequest{
		Model:     model,
		MaxTokens: 8,
		Messages:  []provider.ChatMessage{{Role: provider.RoleUser, Content: "ping"}},
	})
	return err
}

// MCPNames 返回已连接的 MCP Server 名称
func (s *Server) MCPNames() []string { return s.mcpMgr.ServerNames() }

// Summarize 非流式生成一段文本（巡检报告/摘要）。model 为空用配置的默认
// 模型（DefaultModel），再回退首个可用。复用 ReAct Agent（含上下文预算/
// 摘要压缩）与工具注册中心，供 patrol 等业务进程内调用；失败返回错误，
// 不阻塞调用方。
func (s *Server) Summarize(model, prompt string) (string, error) {
	if s.openaiH == nil {
		return "", fmt.Errorf("ainexus gateway not initialized")
	}
	model = s.ResolveModel(model)
	p, ok := s.modelRoutes[model]
	if !ok {
		return "", fmt.Errorf("model %q not found", model)
	}
	ag := agent.New(p, s.registry, s.config.Agent, s.logger, s.promptSource)
	conv := agent.NewConversation(model)
	conv.AddSystemMessage(s.patrolSystemMessage())
	conv.AddUserMessage(prompt)
	// 巡检报告调用归属 patrol_report 场景（计量 operation 起点）
	ctx := usage.NewOperation(context.Background(), usage.ScenarioPatrolReport, "summarize")
	resp, err := ag.Run(ctx, conv)
	if err != nil {
		return "", fmt.Errorf("summarize: %w", err)
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("summarize: no response")
	}
	return resp.Choices[0].Message.Content, nil
}

// patrolSystemMessage 巡检报告 system 消息：mlops patrol_system 场景
// active 模板优先（渲染失败回退内置并留痕），否则代码内置默认。
func (s *Server) patrolSystemMessage() string {
	if s.promptSource != nil {
		if tpl, ok := s.promptSource.ScenarioTemplate(ScenarioPatrolSystem); ok {
			txt, err := renderStaticTemplate(tpl)
			if err == nil {
				return txt
			}
			s.logger.Printf("patrol_system prompt render failed (%v), falling back to builtin", err)
		}
	}
	return patrolSystemPrompt
}

// renderStaticTemplate 渲染无变量场景模板（任何模板动作都会执行失败，
// 以此强制 patrol_system 保持纯文本）。
func renderStaticTemplate(tplText string) (string, error) {
	t, err := template.New("scenario").Parse(tplText)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if err := t.Execute(&b, struct{}{}); err != nil {
		return "", err
	}
	return b.String(), nil
}

// ResolveModel 解析模型名：空 → 场景绑定/配置默认模型（DefaultModel）→
// 配置声明顺序的首个启用模型（稳定 fallback，不依赖 map 遍历顺序）。
// 返回 "" 表示无可用模型。显式指定的模型原样返回（启用与否由调用方
// 检查 IsModelDisabled/IsModelRoutable 决定错误语义）。
func (s *Server) ResolveModel(model string) string {
	if model != "" {
		return model
	}
	if s.config.DefaultModel != "" {
		if _, ok := s.modelRoutes[s.config.DefaultModel]; ok {
			return s.config.DefaultModel
		}
	}
	if len(s.enabledModels) > 0 {
		return s.enabledModels[0]
	}
	return ""
}

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

// RemoveMCPCluster 断开并移除集群 Worker MCP（集群删除时调用），其全部
// 工具一并从 registry 注销。server 不存在时幂等。
func (s *Server) RemoveMCPCluster(name string) {
	if err := s.mcpMgr.RemoveServer("cluster:" + name); err != nil {
		s.logger.Printf("remove mcp cluster %q: %v", name, err)
	}
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
// 工具定义列表，兼容 AiNexus 原生 /api/tools 契约；by_source 按
// 内置/[MCP:server] 分组计数，供接入页对账（每个集群应注册满其工具数，
// 缺口说明注册被重名挤掉或连接不全）。
func (s *Server) ToolsHandler(c *gin.Context) {
	tools := s.registry.ToolDefinitions()
	bySource := map[string]int{}
	for _, t := range tools {
		src := "builtin"
		if strings.HasPrefix(t.Description, "[MCP:") {
			if end := strings.Index(t.Description, "]"); end > 0 {
				src = t.Description[:end+1]
			}
		}
		bySource[src]++
	}
	c.JSON(http.StatusOK, gin.H{"count": len(tools), "tools": tools, "by_source": bySource})
}

// MCPHandler godoc: GET /ainexus/api/mcp
// 已连接的 MCP Server 列表，兼容 AiNexus 原生 /api/mcp 契约
func (s *Server) MCPHandler(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"servers": s.mcpMgr.ServerNames()})
}

// Close 关闭 MCP 连接（进程退出时调用）
func (s *Server) Close() error { return s.mcpMgr.Close() }
