// Package cluster implements the cluster registry business logic: CRUD over
// the store plus Worker health probing. Probing confirms a Worker is
// reachable and runs the swarm control plane before a cluster is admitted,
// and refreshes status/last_seen on every list/detail read.
package cluster

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/workerproxy"
)

// namePattern 集群名合法字符集：字母/数字开头，可含 . _ -，最长 63。
var namePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,62}$`)

// ErrNotFound 集群不存在。
type ErrNotFound struct{ Name string }

func (e ErrNotFound) Error() string { return fmt.Sprintf("cluster %q not found", e.Name) }

// ErrProbeFailed Worker 探测失败（不可达或非 swarm manager）。
type ErrProbeFailed struct {
	Name string
	Err  error
}

func (e ErrProbeFailed) Error() string {
	return fmt.Sprintf("cluster %q probe failed: %v", e.Name, e.Err)
}

// Service 集群注册表服务。
type Service struct {
	store *store.Store
	// probeTimeout 单次健康探测超时。
	probeTimeout time.Duration

	// onAdd / onRemove 集群增删回调（如事件订阅管理器跟随启停）。回调在
	// Add/Remove 成功落库后同步调用，失败不触发。
	onAdd    []func(name string)
	onRemove []func(name string)
}

// New 创建集群服务。
func New(st *store.Store) *Service {
	return &Service{store: st, probeTimeout: 5 * time.Second}
}

// OnClusterAdd 注册集群新增回调（可多个，按注册顺序调用）。
func (s *Service) OnClusterAdd(fn func(name string)) { s.onAdd = append(s.onAdd, fn) }

// OnClusterRemove 注册集群删除回调。
func (s *Service) OnClusterRemove(fn func(name string)) { s.onRemove = append(s.onRemove, fn) }

// List 返回全部集群，逐个探测并刷新状态（online/offline + lastSeen + err）。
// 探测失败不中断列表，仅标记 offline 并附带错误。
func (s *Service) List(ctx context.Context) ([]*store.Cluster, error) {
	clusters, err := s.store.ListClusters()
	if err != nil {
		return nil, err
	}
	for _, c := range clusters {
		s.probe(ctx, c)
	}
	return clusters, nil
}

// ListStatic 返回全部集群注册表条目（不探测状态，不抹除 token），供内部
// 模块（如 AiNexus 网关热重载后重连集群 MCP）读取端点/凭据，对外不可见。
func (s *Service) ListStatic() ([]*store.Cluster, error) {
	return s.store.ListClusters()
}

// Get 返回单个集群（含实时探测）。
func (s *Service) Get(ctx context.Context, name string) (*store.Cluster, error) {
	c, err := s.store.GetCluster(name)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, ErrNotFound{Name: name}
	}
	s.probe(ctx, c)
	return c, nil
}

// GetStatic 返回集群记录（不探测状态，用于内部模块读取 inventory 配置
// 等已有 worker client、无需重复探测的场景）。
func (s *Service) GetStatic(name string) (*store.Cluster, error) {
	c, err := s.store.GetCluster(name)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, ErrNotFound{Name: name}
	}
	return c, nil
}

// UpdateInventory 更新集群纳管清单（仅更新 Inventory 字段，不动其他字段）。
// inv 为 nil 时清空清单。
func (s *Service) UpdateInventory(ctx context.Context, name string, inv *store.InventoryConfig) (*store.Cluster, error) {
	existing, err := s.store.GetCluster(name)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, ErrNotFound{Name: name}
	}
	if inv != nil {
		if err := inv.Validate(); err != nil {
			return nil, fmt.Errorf("inventory: %w", err)
		}
	}
	existing.Inventory = inv
	if err := s.store.PutCluster(existing); err != nil {
		return nil, err
	}
	return existing, nil
}

// Add 接入一个新集群：先探测 Worker（可达 + swarm manager），通过后落库。
// 探测结果同时作为初始 status/last_seen。
func (s *Service) Add(ctx context.Context, in *store.Cluster) (*store.Cluster, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, fmt.Errorf("cluster name is required")
	}
	// 名称同时作为 URL 路径段（/clusters/:name）与 router-link 参数，限制
	// 字符集避免特殊字符破坏路由/编码。
	if !namePattern.MatchString(name) {
		return nil, fmt.Errorf("cluster name must match %s (letters, digits, . _ -; 1-63 chars)", namePattern)
	}
	if in.WorkerURL == "" {
		return nil, fmt.Errorf("worker_url is required")
	}
	if existing, err := s.store.GetCluster(name); err != nil {
		return nil, err
	} else if existing != nil {
		return nil, fmt.Errorf("cluster %q already exists", name)
	}
	// 归属项目需存在（管理层级「项目 → 集群」引用完整性）。
	if in.ProjectID != "" {
		if p, err := s.store.GetProject(in.ProjectID); err != nil {
			return nil, err
		} else if p == nil {
			return nil, fmt.Errorf("project %q not found", in.ProjectID)
		}
	}
	// 纳管清单校验（类型/ref/node 合法性 + 名称唯一）。
	if in.Inventory != nil {
		if err := in.Inventory.Validate(); err != nil {
			return nil, fmt.Errorf("inventory: %w", err)
		}
	}

	probeCtx, cancel := context.WithTimeout(ctx, s.probeTimeout)
	defer cancel()
	info, err := s.probeWorker(probeCtx, in.WorkerURL, in.Token)
	if err != nil {
		return nil, ErrProbeFailed{Name: name, Err: err}
	}

	// MCP 地址缺省取同一 manager 的 /mcp（Worker 仅 manager 挂载该端点）；
	// 显式传入 mcp_url 仍可覆盖（独立部署网关/入口等场景）。
	mcpURL := in.MCPURL
	if mcpURL == "" {
		mcpURL = strings.TrimRight(in.WorkerURL, "/") + "/mcp"
	}

	c := &store.Cluster{
		Name:      name,
		ProjectID: in.ProjectID,
		WorkerURL: in.WorkerURL,
		MCPURL:    mcpURL,
		Token:     in.Token,
		Desc:      in.Desc,
		Inventory: in.Inventory,
		Status:    store.ClusterOnline,
		LastSeen:  time.Now().UTC(),
	}
	c.Err = ""
	_ = info // 探测信息用于日志/审计（P1 暂不持久化节点元数据）

	if err := s.store.PutCluster(c); err != nil {
		return nil, err
	}
	for _, fn := range s.onAdd {
		fn(name)
	}
	return c, nil
}

// Update 更新集群描述/端点信息（不做重探测；重新探测走 List/Get）。
func (s *Service) Update(ctx context.Context, in *store.Cluster) (*store.Cluster, error) {
	existing, err := s.store.GetCluster(in.Name)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, ErrNotFound{Name: in.Name}
	}
	// ProjectID 用空串清除归属、非空赋值（前端永远传，支持改/清项目）。
	existing.ProjectID = in.ProjectID
	if in.WorkerURL != "" {
		existing.WorkerURL = in.WorkerURL
	}
	if in.MCPURL != "" {
		existing.MCPURL = in.MCPURL
	}
	if in.Token != "" {
		existing.Token = in.Token
	}
	if in.Desc != "" {
		existing.Desc = in.Desc
	}
	// Inventory: 仅在请求体显式包含时更新（nil = 不动现有清单）。
	if in.Inventory != nil {
		if err := in.Inventory.Validate(); err != nil {
			return nil, fmt.Errorf("inventory: %w", err)
		}
		existing.Inventory = in.Inventory
	}
	if err := s.store.PutCluster(existing); err != nil {
		return nil, err
	}
	return existing, nil
}

// Remove 移除集群注册表条目。
func (s *Service) Remove(ctx context.Context, name string) error {
	existing, err := s.store.GetCluster(name)
	if err != nil {
		return err
	}
	if existing == nil {
		return ErrNotFound{Name: name}
	}
	if err := s.store.DeleteCluster(name); err != nil {
		return err
	}
	for _, fn := range s.onRemove {
		fn(name)
	}
	return nil
}

// --- 项目（管理层级第一层：项目 → 集群） ---

// ErrProjectNotFound 项目不存在。
type ErrProjectNotFound struct{ ID string }

func (e ErrProjectNotFound) Error() string { return fmt.Sprintf("project %q not found", e.ID) }

// ListProjects 列出全部项目，附成员集群数（不探测集群状态）。
func (s *Service) ListProjects() ([]*store.Project, error) {
	projects, err := s.store.ListProjects()
	if err != nil {
		return nil, err
	}
	// 统计每个项目的成员集群数。
	clusters, err := s.store.ListClusters()
	if err != nil {
		return projects, nil // 统计失败不阻断列表
	}
	counts := map[string]int{}
	for _, c := range clusters {
		if c.ProjectID != "" {
			counts[c.ProjectID]++
		}
	}
	for _, p := range projects {
		p.ClusterCount = counts[p.ID]
	}
	return projects, nil
}

// GetProject 按 ID 读取项目。
func (s *Service) GetProject(id string) (*store.Project, error) {
	p, err := s.store.GetProject(id)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, ErrProjectNotFound{ID: id}
	}
	return p, nil
}

// CreateProject 新建项目（名称去重）。
func (s *Service) CreateProject(name, desc string) (*store.Project, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("project name is required")
	}
	projects, err := s.store.ListProjects()
	if err != nil {
		return nil, err
	}
	for _, p := range projects {
		if p.Name == name {
			return nil, fmt.Errorf("project %q already exists", name)
		}
	}
	p := &store.Project{
		ID:        "p-" + randomHex(8),
		Name:      name,
		Desc:      desc,
		CreatedAt: time.Now().UTC(),
	}
	if err := s.store.PutProject(p); err != nil {
		return nil, err
	}
	return p, nil
}

// UpdateProject 更新项目名称/描述。
func (s *Service) UpdateProject(id, name, desc string) (*store.Project, error) {
	p, err := s.GetProject(id)
	if err != nil {
		return nil, err
	}
	if name != "" {
		p.Name = name
	}
	p.Desc = desc
	if err := s.store.PutProject(p); err != nil {
		return nil, err
	}
	return p, nil
}

// DeleteProject 删除项目（不级联删除集群，仅解除归属）。
func (s *Service) DeleteProject(id string) error {
	if _, err := s.GetProject(id); err != nil {
		return err
	}
	return s.store.DeleteProject(id)
}

// WorkerClient 按集群名构造 Worker gRPC 客户端（带注册的 token）。
// 超时由 gRPC 客户端侧 SetTimeout 控制（当前为 no-op 占位，实际走 per-call ctx 超时）。
func (s *Service) WorkerClient(name string) (*workerproxy.Client, error) {
	c, err := s.store.GetCluster(name)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, ErrNotFound{Name: name}
	}
	cli := workerproxy.New(c.WorkerURL, c.Token)
	cli.SetTimeout(30 * time.Second)
	cli.SetCache(s.store, name)
	return cli, nil
}

// --- 服务配置快照（svccfg：部署/更新时保存原始 config，编辑预填用） ---

// SaveServiceConfig 保存（覆盖）集群内某个服务的配置快照。
func (s *Service) SaveServiceConfig(cluster, service, config string) error {
	return s.store.PutServiceConfig(cluster, service, config)
}

// ServiceConfig 读取服务配置快照；不存在返回 (nil, nil)。
func (s *Service) ServiceConfig(cluster, service string) (*store.ServiceConfig, error) {
	return s.store.GetServiceConfig(cluster, service)
}

// DeleteServiceConfig 删除服务配置快照（服务移除时清理）。
func (s *Service) DeleteServiceConfig(cluster, service string) error {
	return s.store.DeleteServiceConfig(cluster, service)
}

// --- 告警（P3，经 store.Alert） ---

// Alerts 列出告警（按状态+集群过滤，最新在前）。
func (s *Service) Alerts(status store.AlertStatus, clusterName string) ([]*store.Alert, error) {
	return s.store.ListAlerts(status, clusterName)
}

// AckAlert 认领告警（active → acked）。
func (s *Service) AckAlert(id, by string) (*store.Alert, error) {
	return s.store.SetAlertStatus(id, store.AlertAcked, by)
}

// RecoverAlert 人工恢复告警（active/acked → recovered）。
func (s *Service) RecoverAlert(id string) (*store.Alert, error) {
	return s.store.SetAlertStatus(id, store.AlertRecovered, "admin")
}

// Alert 按 id 读取单个告警（P4 深度排查入口）。
func (s *Service) Alert(id string) (*store.Alert, error) {
	return s.store.GetAlert(id)
}

// --- 排查会话（对话式 troubleshoot 落库，关联告警形成闭环） ---

// SaveInvestigation 保存（或更新）排查会话，并在关联告警时回写排查标记。
func (s *Service) SaveInvestigation(inv *store.Investigation) error {
	if err := s.store.SaveInvestigation(inv); err != nil {
		return err
	}
	if inv.AlertID != "" {
		return s.store.MarkAlertInvestigated(inv.AlertID, inv.ID)
	}
	return nil
}

// Investigation 按 id 读取排查会话。
func (s *Service) Investigation(id string) (*store.Investigation, error) {
	return s.store.GetInvestigation(id)
}

// Investigations 按告警列出排查会话（最新在前）。
func (s *Service) Investigations(alertID string, limit int) ([]*store.Investigation, error) {
	return s.store.ListInvestigations(alertID, limit)
}

// --- 密钥（巡检 flow 拨测账号等；列表绝不返回值） ---

// SaveSecret 写入（新增或覆盖）密钥。
func (s *Service) SaveSecret(name, value string) error {
	return s.store.PutSecret(&store.Secret{Name: name, Value: value})
}

// Secrets 列出密钥名（不含值）。
func (s *Service) Secrets() ([]*store.Secret, error) {
	return s.store.ListSecrets()
}

// DeleteSecret 删除密钥。
func (s *Service) DeleteSecret(name string) error {
	return s.store.DeleteSecret(name)
}

// MCPEndpoint 返回集群的 MCP 端点与 token（P4 按需连接 Worker /mcp）。
func (s *Service) MCPEndpoint(name string) (url, token string, err error) {
	c, err := s.store.GetCluster(name)
	if err != nil {
		return "", "", err
	}
	if c == nil {
		return "", "", ErrNotFound{Name: name}
	}
	return c.MCPURL, c.Token, nil
}

// probe 探测单个集群并就地更新其 status/lastSeen/err（不写库，读时快照）。
func (s *Service) probe(ctx context.Context, c *store.Cluster) {
	probeCtx, cancel := context.WithTimeout(ctx, s.probeTimeout)
	defer cancel()

	_, err := s.probeWorker(probeCtx, c.WorkerURL, c.Token)
	c.LastSeen = time.Now().UTC()
	if err != nil {
		c.Status = store.ClusterOffline
		c.Err = err.Error()
		return
	}
	c.Status = store.ClusterOnline
	c.Err = ""
}

// probeWorker 探测 Worker：可达 + 运行 swarm 控制面（manager 角色）。
func (s *Service) probeWorker(ctx context.Context, baseURL, token string) (workerproxy.SelfInfo, error) {
	cli := workerproxy.New(baseURL, token)
	info, err := cli.Self(ctx)
	if err != nil {
		return info, err
	}
	if !info.SwarmManager {
		return info, fmt.Errorf("worker %s is not a swarm manager (role=%s, swarmManager=%v)",
			baseURL, info.Role, info.SwarmManager)
	}
	return info, nil
}

// randomHex 生成 n 字节随机 hex（ID 用）。
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
