// AiNexus 网关配置的页面管理端点（P7）：GET 返回脱敏视图（api_key 只给
// 布尔标记、headers/env 只给 key 列表），PUT 在线保存并热重载内嵌网关
// （空白 api_key/header 值 = 沿用旧值），POST /test 验证单个 provider 连通性。
package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	ainexuscfg "gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/config"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/provider"
)

// --- 脱敏视图（GET） ---

type ainexusConfigView struct {
	Enabled   bool                  `json:"enabled"`
	Active    bool                  `json:"active"` // 网关是否已加载（enabled 且构建成功）
	Providers []ainexusProviderView `json:"providers"`
	// DefaultModel AI 排查网关默认模型（模型池之一；空 = 用首个可用）。
	DefaultModel string                 `json:"default_model,omitempty"`
	Tools        ainexusToolsView       `json:"tools"`
	Agent        ainexusAgentView       `json:"agent"`
	MCPServers   []ainexusMCPServerView `json:"mcp_servers"`
}

type ainexusProviderView struct {
	Name      string             `json:"name"`
	Type      string             `json:"type"`
	BaseURL   string             `json:"base_url"`
	APIKeySet bool               `json:"api_key_set"`
	Models    []ainexusModelView `json:"models"`
}

type ainexusModelView struct {
	Name        string  `json:"name"`
	DisplayName string  `json:"display_name,omitempty"`
	MaxTokens   int     `json:"max_tokens,omitempty"`
	Temperature float64 `json:"temperature,omitempty"`
	// Enabled 模型级启停（MLOps 运营层）。视图如实回显；旧编辑页未提交
	// 该字段时（写入侧 *bool 为 nil）服务端按 provider+model 沿用已保存值，
	// 防止旧页面保存覆盖 MLOps 的启停状态。
	Enabled bool `json:"enabled"`
}

type ainexusToolsView struct {
	Command     ainexusCommandView `json:"command"`
	HTTPRequest ainexusHTTPView    `json:"http_request"`
	FileRead    ainexusFileView    `json:"file_read"`
}

type ainexusCommandView struct {
	Enabled         bool     `json:"enabled"`
	AllowedCommands []string `json:"allowed_commands"`
	Timeout         string   `json:"timeout"` // 时长文本，如 "30s"
	WorkDir         string   `json:"work_dir"`
}

type ainexusHTTPView struct {
	Enabled bool   `json:"enabled"`
	Timeout string `json:"timeout"`
}

type ainexusFileView struct {
	Enabled bool  `json:"enabled"`
	MaxSize int64 `json:"max_size"`
}

type ainexusAgentView struct {
	MaxToolRounds     int  `json:"max_tool_rounds"`
	ParallelToolCalls bool `json:"parallel_tool_calls"`
	MaxContextTokens  int  `json:"max_context_tokens"`
	KeepToolRounds    int  `json:"keep_tool_rounds"`
}

type ainexusMCPServerView struct {
	Name       string   `json:"name"`
	Transport  string   `json:"transport"`
	URL        string   `json:"url,omitempty"`
	Command    string   `json:"command,omitempty"`
	Args       []string `json:"args,omitempty"`
	EnvKeys    []string `json:"env_keys,omitempty"`
	HeaderKeys []string `json:"headers_keys,omitempty"`
	// Cluster 标记该条目为集群 manager 的自动 MCP（服务端加载/热重载时
	// 自动连接，页面只读），区别于用户自定义 MCP。
	Cluster bool `json:"cluster,omitempty"`
}

// --- 写入请求（PUT，空白 api_key/header 值 = 沿用旧值） ---

type ainexusConfigRequest struct {
	Enabled      bool                      `json:"enabled"`
	DefaultModel string                    `json:"default_model,omitempty"` // 网关默认模型（模型池之一；空 = 首个可用）
	Providers    []ainexusProviderRequest  `json:"providers"`
	Tools        ainexusToolsRequest       `json:"tools"`
	Agent        ainexusAgentRequest       `json:"agent"`
	MCPServers   []ainexusMCPServerRequest `json:"mcp_servers"`
}

type ainexusProviderRequest struct {
	Name    string              `json:"name"`
	Type    string              `json:"type"`
	BaseURL string              `json:"base_url"`
	APIKey  string              `json:"api_key"` // 留空 = 保持已保存值
	Models  []ainexusModelRequest `json:"models"`
}

// ainexusModelRequest 模型写入项。Enabled 指针：nil = 未提交（旧页面），
// 服务端沿用已保存值；显式 true/false 才更新。
type ainexusModelRequest struct {
	Name        string   `json:"name"`
	DisplayName string   `json:"display_name,omitempty"`
	MaxTokens   int      `json:"max_tokens,omitempty"`
	Temperature float64  `json:"temperature,omitempty"`
	Enabled     *bool    `json:"enabled,omitempty"`
}

type ainexusToolsRequest struct {
	Command     ainexusCommandRequest `json:"command"`
	HTTPRequest ainexusHTTPRequest    `json:"http_request"`
	FileRead    ainexusFileRequest    `json:"file_read"`
}

type ainexusCommandRequest struct {
	Enabled         bool     `json:"enabled"`
	AllowedCommands []string `json:"allowed_commands"`
	Timeout         string   `json:"timeout"` // 空 = 默认 30s
	WorkDir         string   `json:"work_dir"`
}

type ainexusHTTPRequest struct {
	Enabled bool   `json:"enabled"`
	Timeout string `json:"timeout"` // 空 = 默认 15s
}

type ainexusFileRequest struct {
	Enabled bool  `json:"enabled"`
	MaxSize int64 `json:"max_size"`
}

type ainexusAgentRequest struct {
	MaxToolRounds     int  `json:"max_tool_rounds"`
	ParallelToolCalls bool `json:"parallel_tool_calls"`
	MaxContextTokens  int  `json:"max_context_tokens"`
	KeepToolRounds    int  `json:"keep_tool_rounds"`
}

type ainexusMCPServerRequest struct {
	Name      string            `json:"name"`
	Transport string            `json:"transport"`
	URL       string            `json:"url,omitempty"`
	Command   string            `json:"command,omitempty"`
	Args      []string          `json:"args,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"` // 空值 = 保持已保存值
}

// GetAINexusConfig godoc: GET /api/v1/ainexus/config
// 返回当前生效网关配置的脱敏视图（api_key 仅标记是否已设置；headers/env 仅列 key）。
// 未在页面保存过时返回 config.yaml 的 ainexus 块。mcp_servers 额外合并各
// 集群 manager 的自动 MCP（cluster:true，服务端加载/热重载时已自动连接）。
func (h *Handlers) GetAINexusConfig(c *gin.Context) {
	if h.AINexusRT == nil {
		fail(c, http.StatusServiceUnavailable, "ainexus runtime not initialized")
		return
	}
	view := buildAINexusView(h.AINexusRT.Config(), h.AINexusRT.Server() != nil)
	view.MCPServers = append(view.MCPServers, h.clusterMCPServers()...)
	ok(c, http.StatusOK, view)
}

// clusterMCPServers 收集各集群 manager 的自动 MCP（有 mcp_url 的集群）。
// 集群服务未初始化或列表失败时静默返回空（不阻断配置展示）。
func (h *Handlers) clusterMCPServers() []ainexusMCPServerView {
	if h.clusters == nil {
		return nil
	}
	clusters, err := h.clusters.ListStatic()
	if err != nil {
		return nil
	}
	var out []ainexusMCPServerView
	for _, c := range clusters {
		if c.MCPURL == "" {
			continue
		}
		out = append(out, ainexusMCPServerView{
			Name:      "cluster:" + c.Name,
			Transport: "streamable-http",
			URL:       c.MCPURL,
			Cluster:   true,
		})
	}
	return out
}

// UpdateAINexusConfig godoc: PUT /api/v1/ainexus/config
// 在线保存网关配置并热重载（无需重启）。校验/构建失败时旧网关继续服务。
func (h *Handlers) UpdateAINexusConfig(c *gin.Context) {
	if h.AINexusRT == nil {
		fail(c, http.StatusServiceUnavailable, "ainexus runtime not initialized")
		return
	}
	var req ainexusConfigRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	cfg, err := req.toConfig(carryModelEnabled(h.AINexusRT.Config()))
	if err != nil {
		fail(c, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.AINexusRT.Update(c.Request.Context(), cfg); err != nil {
		fail(c, http.StatusBadGateway, err.Error())
		return
	}
	view := buildAINexusView(h.AINexusRT.Config(), h.AINexusRT.Server() != nil)
	view.MCPServers = append(view.MCPServers, h.clusterMCPServers()...)
	ok(c, http.StatusOK, view)
}

// TestAINexusProvider godoc: POST /api/v1/ainexus/config/test
// 用请求体里的 provider 参数构造临时客户端发一次最小请求，验证连通性。
// 失败返回 200 + ok=false（避免全局错误 toast 与表单内展示双重提示）。
func (h *Handlers) TestAINexusProvider(c *gin.Context) {
	var req ainexusTestRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, "invalid request: "+err.Error())
		return
	}
	if req.BaseURL == "" || req.Model == "" {
		fail(c, http.StatusBadRequest, "base_url and model are required")
		return
	}
	name := req.Name
	if name == "" {
		name = req.Type
	}
	apiKey := req.APIKey
	if apiKey == "" && h.AINexusRT != nil {
		// api_key 被脱敏，页面留空时回退已保存的 key（按 provider 名匹配）
		for _, p := range h.AINexusRT.Config().Providers {
			if p.Name == name {
				apiKey = p.APIKey
				break
			}
		}
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()

	err := probeProvider(ctx, req.Type, name, req.BaseURL, apiKey, req.Model)
	latency := time.Since(start)
	if err != nil {
		ok(c, http.StatusOK, gin.H{"ok": false, "latency_ms": latency.Milliseconds(), "error": err.Error()})
		return
	}
	ok(c, http.StatusOK, gin.H{"ok": true, "latency_ms": latency.Milliseconds(), "model": req.Model})
}

// ainexusTestRequest 连通性测试请求体。
type ainexusTestRequest struct {
	Name    string `json:"name"`
	Type    string `json:"type"` // openai_compatible | anthropic_compatible
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`
	Model   string `json:"model"`
}

// probeProvider 发一次最小非流式请求验证 provider 连通。
func probeProvider(ctx context.Context, ptype, name, baseURL, apiKey, model string) error {
	var p provider.Provider
	switch ptype {
	case string(ainexuscfg.ProviderTypeOpenAI), "":
		p = provider.NewOpenAIProvider(name, baseURL, apiKey, []string{model})
	case string(ainexuscfg.ProviderTypeAnthropic):
		p = provider.NewAnthropicProvider(name, baseURL, apiKey, []string{model})
	default:
		return fmt.Errorf("type must be openai_compatible or anthropic_compatible")
	}
	_, err := p.ChatCompletion(ctx, &provider.ChatRequest{
		Model:     model,
		MaxTokens: 8,
		Messages:  []provider.ChatMessage{{Role: provider.RoleUser, Content: "ping"}},
	})
	return err
}

// toConfig 把写入请求转换为网关配置；时长文本解析失败返回错误。
// carryEnabled：旧编辑页未提交模型 enabled（nil）时按 provider+model
// 沿用旧配置值，新模型默认启用。
func (req *ainexusConfigRequest) toConfig(carryEnabled map[string]bool) (*ainexuscfg.Config, error) {
	cfg := &ainexuscfg.Config{Enabled: req.Enabled, DefaultModel: req.DefaultModel}

	commandTimeout, err := parseDur(req.Tools.Command.Timeout, 30*time.Second)
	if err != nil {
		return nil, err
	}
	httpTimeout, err := parseDur(req.Tools.HTTPRequest.Timeout, 15*time.Second)
	if err != nil {
		return nil, err
	}
	cfg.Tools = ainexuscfg.ToolsConfig{
		Command: ainexuscfg.CommandToolConfig{
			Enabled:         req.Tools.Command.Enabled,
			AllowedCommands: req.Tools.Command.AllowedCommands,
			Timeout:         commandTimeout,
			WorkDir:         req.Tools.Command.WorkDir,
		},
		HTTPRequest: ainexuscfg.HTTPRequestToolConfig{
			Enabled: req.Tools.HTTPRequest.Enabled,
			Timeout: httpTimeout,
		},
		FileRead: ainexuscfg.FileReadToolConfig{
			Enabled: req.Tools.FileRead.Enabled,
			MaxSize: req.Tools.FileRead.MaxSize,
		},
	}
	cfg.Agent = ainexuscfg.AgentConfig{
		MaxToolRounds:     req.Agent.MaxToolRounds,
		ParallelToolCalls: req.Agent.ParallelToolCalls,
		MaxContextTokens:  req.Agent.MaxContextTokens,
		KeepToolRounds:    req.Agent.KeepToolRounds,
	}

	cfg.Providers = make([]ainexuscfg.ProviderConfig, 0, len(req.Providers))
	for _, p := range req.Providers {
		pc := ainexuscfg.ProviderConfig{
			Name:    p.Name,
			Type:    ainexuscfg.ProviderType(p.Type),
			BaseURL: p.BaseURL,
			APIKey:  p.APIKey,
		}
		for _, m := range p.Models {
			enabled := true // 新模型默认启用
			if m.Enabled != nil {
				enabled = *m.Enabled
			} else if carried, ok := carryEnabled[p.Name+"\x00"+m.Name]; ok {
				enabled = carried // 旧页面未提交 → 沿用已保存的启停状态
			}
			pc.Models = append(pc.Models, ainexuscfg.ModelConfig{
				Name:        m.Name,
				DisplayName: m.DisplayName,
				MaxTokens:   m.MaxTokens,
				Temperature: m.Temperature,
				Enabled:     enabled,
			})
		}
		cfg.Providers = append(cfg.Providers, pc)
	}

	for _, m := range req.MCPServers {
		// 集群 manager 的自动 MCP（cluster: 前缀）由服务端统一管理，
		// 用户保存时过滤，避免与自动连接重复（前端误提交/旧缓存兜底）。
		if strings.HasPrefix(m.Name, "cluster:") {
			continue
		}
		cfg.MCPServers = append(cfg.MCPServers, ainexuscfg.MCPServerConfig{
			Name:      m.Name,
			Transport: m.Transport,
			Command:   m.Command,
			Args:      m.Args,
			Env:       m.Env,
			URL:       m.URL,
			Headers:   m.Headers,
		})
	}
	return cfg, nil
}

// carryModelEnabled 从当前配置提取 (provider, model) -> enabled，
// 供 toConfig 在旧页面未提交 enabled 字段时沿用。
func carryModelEnabled(cfg *ainexuscfg.Config) map[string]bool {
	out := make(map[string]bool)
	if cfg == nil {
		return out
	}
	for _, p := range cfg.Providers {
		for _, m := range p.Models {
			out[p.Name+"\x00"+m.Name] = m.Enabled
		}
	}
	return out
}

// parseDur 解析时长文本，空串回退默认值。
func parseDur(s string, def time.Duration) (time.Duration, error) {
	if s == "" {
		return def, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	return d, nil
}

// buildAINexusView 从网关配置构造脱敏视图。
func buildAINexusView(cfg *ainexuscfg.Config, active bool) ainexusConfigView {
	if cfg == nil {
		cfg = &ainexuscfg.Config{}
	}
	v := ainexusConfigView{
		Enabled:      cfg.Enabled,
		Active:       active,
		DefaultModel: cfg.DefaultModel,
		Tools: ainexusToolsView{
			Command: ainexusCommandView{
				Enabled:         cfg.Tools.Command.Enabled,
				AllowedCommands: cfg.Tools.Command.AllowedCommands,
				Timeout:         cfg.Tools.Command.Timeout.String(),
				WorkDir:         cfg.Tools.Command.WorkDir,
			},
			HTTPRequest: ainexusHTTPView{
				Enabled: cfg.Tools.HTTPRequest.Enabled,
				Timeout: cfg.Tools.HTTPRequest.Timeout.String(),
			},
			FileRead: ainexusFileView{
				Enabled: cfg.Tools.FileRead.Enabled,
				MaxSize: cfg.Tools.FileRead.MaxSize,
			},
		},
		Agent: ainexusAgentView{
			MaxToolRounds:     cfg.Agent.MaxToolRounds,
			ParallelToolCalls: cfg.Agent.ParallelToolCalls,
			MaxContextTokens:  cfg.Agent.MaxContextTokens,
			KeepToolRounds:    cfg.Agent.KeepToolRounds,
		},
	}
	for _, p := range cfg.Providers {
		pv := ainexusProviderView{
			Name:      p.Name,
			Type:      string(p.Type),
			BaseURL:   p.BaseURL,
			APIKeySet: p.APIKey != "",
		}
		for _, m := range p.Models {
			pv.Models = append(pv.Models, ainexusModelView{
				Name:        m.Name,
				DisplayName: m.DisplayName,
				MaxTokens:   m.MaxTokens,
				Temperature: m.Temperature,
				Enabled:     m.Enabled,
			})
		}
		v.Providers = append(v.Providers, pv)
	}
	for _, m := range cfg.MCPServers {
		v.MCPServers = append(v.MCPServers, ainexusMCPServerView{
			Name:       m.Name,
			Transport:  m.Transport,
			URL:        m.URL,
			Command:    m.Command,
			Args:       m.Args,
			EnvKeys:    mapKeys(m.Env),
			HeaderKeys: mapKeys(m.Headers),
		})
	}
	return v
}

// mapKeys 返回 map 的 key 列表（顺序不稳定，仅用于展示/编辑名）。
func mapKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
