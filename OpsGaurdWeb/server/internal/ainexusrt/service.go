// Package ainexusrt manages the embedded AiNexus gateway's runtime
// configuration: the config last saved from the management UI (persisted as
// YAML in LevelDB), hot-reload of the in-process gateway without restarting,
// and an atomic view of the current server so handlers never hold a stale
// pointer. Until the UI saves once, the ainexus block of config.yaml remains
// the source of truth (file config, not persisted).
package ainexusrt

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"

	"gopkg.in/yaml.v3"

	ainexuscfg "gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/config"
	ainexusserver "gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/server"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/ainexus/usage"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/cluster"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
)

// Service 运行时网关配置服务。
type Service struct {
	st       *store.Store
	clusters *cluster.Service
	logger   *log.Logger

	mu  sync.RWMutex
	cfg *ainexuscfg.Config    // 当前生效配置（含已保存的 api_key）
	srv *ainexusserver.Server // 当前内嵌网关（nil = 未启用）

	// usageSink 共享计量 sink：独立于网关实例存活，热重载构建新网关时
	// 复用同一实例（须在 Init 之前注入）。
	usageSink usage.Sink
	// promptSource 提示词场景模板来源（mlops 运营层；须在 Init 之前注入）。
	promptSource ainexusserver.PromptSource
	// modelBinder 场景模型绑定来源（mlops 运营层；须在 Init 之前注入）。
	// 场景未指定模型时按绑定选择；绑定模型不可路由时回退默认可用。
	modelBinder ScenarioModelBinder
}

// ScenarioModelBinder 场景模型绑定来源（mlops.Service 实现；nil = 无绑定）。
type ScenarioModelBinder interface {
	// ScenarioModel 返回场景绑定的模型名；ok=false 表示未绑定。
	ScenarioModel(scenario string) (string, bool)
}

// New 创建运行时网关配置服务。fileCfg 来自 config.yaml 的 ainexus 块，
// 仅在 LevelDB 尚无运行时配置（从未在页面保存过）时作为回退。
func New(st *store.Store, clusters *cluster.Service, fileCfg *ainexuscfg.Config) *Service {
	return &Service{
		st:       st,
		clusters: clusters,
		logger:   log.New(os.Stderr, "[OpsGaurdWeb.AiNexusRT] ", log.LstdFlags|log.Lshortfile),
		cfg:      cloneConfig(fileCfg),
	}
}

// SetUsageSink 注入共享计量 sink（须在 Init 之前调用）。sink 独立于网关
// 实例存活：热重载关闭旧网关不影响 sink，新网关构建时挂载同一实例，
// 保证配置切换前后计量不中断。
func (s *Service) SetUsageSink(sink usage.Sink) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.usageSink = sink
}

// SetPromptSource 注入提示词场景模板来源（须在 Init 之前调用）。热重载
// 构建新网关时复用同一来源。
func (s *Service) SetPromptSource(src ainexusserver.PromptSource) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.promptSource = src
}

// SetModelBinder 注入场景模型绑定来源（须在 Init 之前调用）。绑定不属于
// 网关配置，热重载不重置。
func (s *Service) SetModelBinder(b ScenarioModelBinder) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.modelBinder = b
}

// Init 加载运行时配置并构建网关：
//   - LevelDB 已有页面保存过的配置 → 以其为准（覆盖文件配置）；
//   - 否则回退 config.yaml 的 ainexus 块（不落库，文件保持为源）。
//
// enabled=true 时构建内嵌网关，失败返回错误（启动即退出）。
func (s *Service) Init(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	raw, err := s.st.GetAINexusRuntime()
	if err != nil {
		return fmt.Errorf("read runtime ainexus config: %w", err)
	}
	if len(raw) > 0 {
		var cfg ainexuscfg.Config
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			// 已保存的配置损坏 → 回退文件配置并提示（不阻塞启动）
			s.logger.Printf("runtime config corrupted (%v), falling back to file config", err)
		} else {
			s.cfg = &cfg
		}
	}

	next, err := build(ctx, s.cfg, s.usageSink, s.promptSource)
	if err != nil {
		return err
	}
	s.srv = next
	if next != nil {
		s.reconnectClustersLocked(ctx)
	}
	return nil
}

// Update 热重载网关配置（保存并立即生效，无需重启）：
//   - 空白 api_key 沿用当前已保存的值（前端密码框留空 = 不修改）；
//   - 校验失败 → 返回错误，旧网关继续服务；
//   - 构建新网关成功 → 持久化 YAML → 关旧网关 → 原子换指针。
//
// enabled=false 时关闭网关（/ainexus/* 返回 503），配置照常保存。
func (s *Service) Update(ctx context.Context, cfg *ainexuscfg.Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if cfg == nil {
		return fmt.Errorf("ainexus config is required")
	}
	carryKeys(s.cfg, cfg) // 空白 api_key 沿用旧值（禁用/启用切换也不丢失）
	if cfg.Enabled {
		if len(cfg.Providers) == 0 {
			return fmt.Errorf("at least one provider must be configured")
		}
		if err := cfg.Validate(); err != nil {
			return fmt.Errorf("ainexus config invalid: %w", err)
		}
	} else if err := validateDefaultModel(cfg); err != nil {
		// 网关未启用时不跑完整 Validate（其 server.addr 检查与内嵌场景无关），
		// 但默认模型仍须属于模型池，避免重新启用时才发现配置错误。
		return err
	}

	next, err := build(ctx, cfg, s.usageSink, s.promptSource)
	if err != nil {
		return err
	}

	yamlText, err := yaml.Marshal(cfg)
	if err != nil {
		closeServer(next)
		return fmt.Errorf("marshal runtime config: %w", err)
	}
	if err := s.st.PutAINexusRuntime(string(yamlText)); err != nil {
		closeServer(next)
		return fmt.Errorf("persist runtime config: %w", err)
	}

	closeServer(s.srv) // 旧网关（含 MCP 连接）关闭
	s.cfg = cfg
	s.srv = next
	if next != nil {
		s.reconnectClustersLocked(ctx)
	}
	s.logger.Printf("gateway config reloaded (enabled=%v, providers=%d)", cfg.Enabled, len(cfg.Providers))
	return nil
}

// Server 返回当前内嵌网关（nil = 未启用）。指针在热重载后失效，调用方须在
// 单次请求内完成使用，不要跨请求持有。
func (s *Service) Server() *ainexusserver.Server {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.srv
}

// SetModelEnabled 模型启停（MLOps 运营层）：读当前配置 → 修改目标模型
// enabled → 走 Update 全量热重载（构建校验成功才持久化并原子换网关）。
// 禁用最后一个启用模型被拒绝（网关必须保留至少一个可用模型）。
func (s *Service) SetModelEnabled(ctx context.Context, provider, model string, enabled bool) error {
	next := s.Config() // 深拷贝，失败不影响当前配置
	found := false
	enabledCount := 0
	for i := range next.Providers {
		if next.Providers[i].Name != provider {
			continue
		}
		for j := range next.Providers[i].Models {
			m := &next.Providers[i].Models[j]
			if m.Name != model {
				continue
			}
			found = true
			m.Enabled = enabled
			s.logger.Printf("mlops model toggle: %s/%s enabled=%v", provider, model, enabled)
		}
		for _, m := range next.Providers[i].Models {
			if m.Enabled {
				enabledCount++
			}
		}
	}
	if !found {
		return fmt.Errorf("model %q not found in provider %q", model, provider)
	}
	if !enabled && next.Enabled && enabledCount == 0 {
		return fmt.Errorf("cannot disable the last enabled model %q (gateway needs at least one)", model)
	}
	return s.Update(ctx, next)
}

// Summarize 巡检报告入口：场景未指定模型时应用 patrol_report 场景绑定；
// 绑定模型当前不可路由（被禁用/已移除）时回退网关默认可用并留痕。
func (s *Service) Summarize(model, prompt string) (string, error) {
	srv := s.Server()
	if srv == nil {
		return "", fmt.Errorf("ainexus gateway not enabled")
	}
	if model == "" {
		s.mu.RLock()
		binder := s.modelBinder
		s.mu.RUnlock()
		if binder != nil {
			if m, ok := binder.ScenarioModel(usage.ScenarioPatrolReport); ok && m != "" {
				if srv.IsModelRoutable(m) {
					model = m
				} else {
					s.logger.Printf("patrol_report binding %q not routable, falling back to default model", m)
				}
			}
		}
	}
	return srv.Summarize(model, prompt)
}

// EffectiveModel 场景模型解析（MLOps 报表/绑定校验用）：requested 非空
// 原样返回；否则按场景绑定（可路由才生效）→ 网关默认可用。
func (s *Service) EffectiveModel(scenario, requested string) string {
	srv := s.Server()
	if srv == nil || requested != "" {
		return requested
	}
	s.mu.RLock()
	binder := s.modelBinder
	s.mu.RUnlock()
	if binder != nil {
		if m, ok := binder.ScenarioModel(scenario); ok && m != "" && srv.IsModelRoutable(m) {
			return m
		}
	}
	return srv.ResolveModel("")
}

// Config 返回当前生效配置的深拷贝（供 API 层脱敏视图/测试，避免与内部共享）。
func (s *Service) Config() *ainexuscfg.Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneConfig(s.cfg)
}

// build 按配置构建内嵌网关；enabled=false 返回 (nil, nil)。校验失败返回
// 错误（新网关不会生效，旧网关不受影响）。sink 为共享计量 sink（可 nil）。
func build(ctx context.Context, cfg *ainexuscfg.Config, sink usage.Sink, promptSrc ainexusserver.PromptSource) (*ainexusserver.Server, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("ainexus config invalid: %w", err)
	}
	srv := ainexusserver.New(cfg,
		ainexusserver.WithUsageSink(sink),
		ainexusserver.WithPromptSource(promptSrc),
	)
	if err := srv.Initialize(ctx); err != nil {
		return nil, fmt.Errorf("ainexus initialize: %w", err)
	}
	return srv, nil
}

// validateDefaultModel 校验默认模型属于模型池（providers[].models）。
func validateDefaultModel(cfg *ainexuscfg.Config) error {
	if cfg.DefaultModel == "" {
		return nil
	}
	for _, p := range cfg.Providers {
		for _, m := range p.Models {
			if m.Name == cfg.DefaultModel {
				return nil
			}
		}
	}
	return fmt.Errorf("default_model %q is not in the model pool (providers[].models)", cfg.DefaultModel)
}

// closeServer 关闭网关（幂等，nil 直接返回）。
func closeServer(srv *ainexusserver.Server) {
	if srv == nil {
		return
	}
	_ = srv.Close()
}

// reconnectClustersLocked 把各集群 Worker 的 MCP 重新注册进网关（热重载后
// 集群 MCP 连接随旧网关一起关闭，需重连）。失败仅记日志，不阻断加载。
func (s *Service) reconnectClustersLocked(ctx context.Context) {
	if s.srv == nil {
		return
	}
	clusters, err := s.clusters.ListStatic()
	if err != nil {
		s.logger.Printf("list clusters for MCP reconnect: %v", err)
		return
	}
	for _, c := range clusters {
		if c.MCPURL == "" {
			continue
		}
		if err := s.srv.AddMCPCluster(c.Name, c.MCPURL, c.Token); err != nil {
			s.logger.Printf("reconnect MCP for cluster %q: %v", c.Name, err)
		}
	}
}

// carryKeys 把旧配置中非空的 api_key 填入新配置（前端密码框留空 = 沿用旧
// 值）。按 provider 名匹配；新 provider 无旧值则保持空（由 Validate 拦下）。
func carryKeys(old, next *ainexuscfg.Config) {
	byName := make(map[string]string, len(old.Providers))
	for _, p := range old.Providers {
		byName[p.Name] = p.APIKey
	}
	for i := range next.Providers {
		if next.Providers[i].APIKey == "" {
			next.Providers[i].APIKey = byName[next.Providers[i].Name]
		}
	}
}

// cloneConfig 深拷贝网关配置（map/slice 全部复制，防止调用方与内部共享）。
func cloneConfig(c *ainexuscfg.Config) *ainexuscfg.Config {
	if c == nil {
		return &ainexuscfg.Config{}
	}
	out := *c
	out.Providers = make([]ainexuscfg.ProviderConfig, len(c.Providers))
	for i, p := range c.Providers {
		cp := p
		cp.Models = append([]ainexuscfg.ModelConfig(nil), p.Models...)
		cp.Extra = cloneAnyMap(p.Extra)
		out.Providers[i] = cp
	}
	out.MCPServers = make([]ainexuscfg.MCPServerConfig, len(c.MCPServers))
	for i, m := range c.MCPServers {
		cm := m
		cm.Args = append([]string(nil), m.Args...)
		cm.Env = cloneStrMap(m.Env)
		cm.Headers = cloneStrMap(m.Headers)
		out.MCPServers[i] = cm
	}
	return &out
}

// cloneStrMap 复制 map[string]string。
func cloneStrMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// cloneAnyMap 复制 map[string]any（值按原样共享，网关配置中不深含可变结构）。
func cloneAnyMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
