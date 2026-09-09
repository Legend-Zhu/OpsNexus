// Cluster registry persistence: key cluster/<name> -> JSON Cluster.
// Token is persisted but never serialised to clients (json:"-"); the API
// layer returns the masked form.
package store

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/syndtr/goleveldb/leveldb"
)

// ClusterStatus 集群接入状态。
type ClusterStatus string

const (
	ClusterOnline  ClusterStatus = "online"
	ClusterOffline ClusterStatus = "offline"
)

// Cluster 是集群注册表条目（对应设计方案 §七 cluster/<name>）。
type Cluster struct {
	Name string `json:"name"`
	// ProjectID 所属项目（管理层级「项目 → 集群」）；空 = 未归属。
	ProjectID string `json:"project_id,omitempty"`
	WorkerURL string `json:"worker_url"`
	// WorkerHTTPURL Worker 的 HTTP 端点（默认 http://<管理节点IP>:8080），
	// /mcp 与 /healthz 的基地址；与 gRPC 管理端口（WorkerURL，默认 9080）是
	// 两个独立端口。空 = 旧记录（MCP 地址沿用 mcp_url 原值）。
	WorkerHTTPURL string        `json:"worker_http_url,omitempty"`
	MCPURL        string        `json:"mcp_url,omitempty"`
	Token         string        `json:"token,omitempty"` // 落盘持久化；对外响应经 Public() 抹除
	Desc          string        `json:"desc,omitempty"`
	Status        ClusterStatus `json:"status"`
	LastSeen      time.Time     `json:"last_seen"`
	// HasToken 仅在 Public() 输出时填充：true=已配 token。不暴露值本身。
	HasToken bool `json:"has_token,omitempty"`
	// Inventory 纳管清单：声明集群里要纳管的外部对象（非 OpsGaurd 部署的
	// swarm service）。nil = 空清单（旧记录不受影响）。
	Inventory *InventoryConfig `json:"inventory,omitempty"`
	// NodeMonitoring 宿主机资源阈值（集群级，对本集群全部 swarm 节点生效）。
	// 由 server 侧 nodemon 循环周期采样各节点 host CPU/内存评估翻转，事件走
	// ingest 管线聚合成告警；与 AlertRule 不同，配置不下发 Worker。
	// nil = 未配置（旧记录不受影响）。
	NodeMonitoring *NodeMonitoring `json:"nodeMonitoring,omitempty"`
	// Err 最近一次健康探测错误（不持久化，运行时填充）
	Err string `json:"-"`
}

// NodeMonitoring 宿主机资源阈值配置（集群级）。
// 语义与 ResourceThreshold 对齐（百分比阈值、>= 触发），但作用对象是
// swarm 节点的宿主机整体（/proc 采样），不是某个服务的容器聚合。
type NodeMonitoring struct {
	Enabled bool `json:"enabled"`
	// CPUThreshold 宿主机 CPU 占用率阈值（百分比 1-100；0 = 不检查 CPU）。
	CPUThreshold int `json:"cpuThreshold,omitempty"`
	// MemThreshold 宿主机内存占用率阈值（百分比 1-100；0 = 不检查内存）。
	MemThreshold int `json:"memThreshold,omitempty"`
}

// Validate 校验阈值范围。Enabled=false 时允许全零阈值（整体停用的合法载荷）。
func (m *NodeMonitoring) Validate() error {
	if m == nil {
		return nil
	}
	for name, v := range map[string]int{"cpuThreshold": m.CPUThreshold, "memThreshold": m.MemThreshold} {
		if v < 0 || v > 100 {
			return fmt.Errorf("nodeMonitoring: %s must be 1-100 (0 = off), got %d", name, v)
		}
	}
	if m.Enabled && m.CPUThreshold == 0 && m.MemThreshold == 0 {
		return fmt.Errorf("nodeMonitoring: cpuThreshold/memThreshold 至少配置一个（0 = 不检查）")
	}
	return nil
}

// Public 返回去除敏感字段（Token）的对外视图，保留 HasToken 布尔标记与
// 最近一次探测错误（Err，供前端展示离线原因）。
func (c *Cluster) Public() *Cluster {
	out := *c
	out.HasToken = c.Token != ""
	out.Token = ""
	return &out
}

// clusterKey 返回注册表 key。
func clusterKey(name string) string { return BucketCluster + "/" + name }

// PutCluster 写入（新增或更新）集群注册表条目。
func (s *Store) PutCluster(c *Cluster) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("marshal cluster %q: %w", c.Name, err)
	}
	return s.db.Put([]byte(clusterKey(c.Name)), data, nil)
}

// GetCluster 读取单个集群；不存在时返回 (nil, nil)。
func (s *Store) GetCluster(name string) (*Cluster, error) {
	raw, err := s.get(clusterKey(name))
	if err == leveldb.ErrNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var c Cluster
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("decode cluster %q: %w", name, err)
	}
	return &c, nil
}

// DeleteCluster 删除集群注册表条目。
func (s *Store) DeleteCluster(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Delete([]byte(clusterKey(name)), nil)
}

// ListClusters 列出全部集群（按 name 字典序，即注册顺序）。
func (s *Store) ListClusters() ([]*Cluster, error) {
	var out []*Cluster
	err := s.iterate(BucketCluster+"/", func(key string, value []byte) error {
		if strings.HasPrefix(key, BucketCluster+"/idx/") {
			return nil // 跳过复合索引
		}
		var c Cluster
		if err := json.Unmarshal(value, &c); err != nil {
			return fmt.Errorf("decode cluster %q: %w", key, err)
		}
		out = append(out, &c)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
