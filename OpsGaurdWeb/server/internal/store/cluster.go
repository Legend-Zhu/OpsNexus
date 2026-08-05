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
	Name      string        `json:"name"`
	// ProjectID 所属项目（管理层级「项目 → 集群」）；空 = 未归属。
	ProjectID string        `json:"project_id,omitempty"`
	WorkerURL string        `json:"worker_url"`
	MCPURL    string        `json:"mcp_url,omitempty"`
	Token     string        `json:"token,omitempty"` // 落盘持久化；对外响应经 Public() 抹除
	Desc      string        `json:"desc,omitempty"`
	Status    ClusterStatus `json:"status"`
	LastSeen  time.Time     `json:"last_seen"`
	// Err 最近一次健康探测错误（不持久化，运行时填充）
	Err string `json:"-"`
}

// Public 返回去除敏感字段（Token/Err）的对外视图。
func (c *Cluster) Public() *Cluster {
	out := *c
	out.Token = ""
	out.Err = ""
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
