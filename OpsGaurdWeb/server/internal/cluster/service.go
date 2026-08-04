// Package cluster implements the cluster registry business logic: CRUD over
// the store plus Worker health probing. Probing confirms a Worker is
// reachable and runs the swarm control plane before a cluster is admitted,
// and refreshes status/last_seen on every list/detail read.
package cluster

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/workerproxy"
)

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
}

// New 创建集群服务。
func New(st *store.Store) *Service {
	return &Service{store: st, probeTimeout: 5 * time.Second}
}

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

// Add 接入一个新集群：先探测 Worker（可达 + swarm manager），通过后落库。
// 探测结果同时作为初始 status/last_seen。
func (s *Service) Add(ctx context.Context, in *store.Cluster) (*store.Cluster, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, fmt.Errorf("cluster name is required")
	}
	if in.WorkerURL == "" {
		return nil, fmt.Errorf("worker_url is required")
	}
	if existing, err := s.store.GetCluster(name); err != nil {
		return nil, err
	} else if existing != nil {
		return nil, fmt.Errorf("cluster %q already exists", name)
	}

	probeCtx, cancel := context.WithTimeout(ctx, s.probeTimeout)
	defer cancel()
	info, err := s.probeWorker(probeCtx, in.WorkerURL, in.Token)
	if err != nil {
		return nil, ErrProbeFailed{Name: name, Err: err}
	}

	c := &store.Cluster{
		Name:      name,
		WorkerURL: in.WorkerURL,
		MCPURL:    in.MCPURL,
		Token:     in.Token,
		Desc:      in.Desc,
		Status:    store.ClusterOnline,
		LastSeen:  time.Now().UTC(),
	}
	c.Err = ""
	_ = info // 探测信息用于日志/审计（P1 暂不持久化节点元数据）

	if err := s.store.PutCluster(c); err != nil {
		return nil, err
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
	return s.store.DeleteCluster(name)
}

// WorkerClient 按集群名构造 Worker HTTP 客户端（带注册的 token）。
func (s *Service) WorkerClient(name string) (*workerproxy.Client, error) {
	c, err := s.store.GetCluster(name)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, ErrNotFound{Name: name}
	}
	return workerproxy.New(c.WorkerURL, c.Token), nil
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
