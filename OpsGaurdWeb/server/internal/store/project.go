// Project persistence: key project/<id> -> JSON Project.
// 管理层级「项目 → 集群 → 节点 → 容器/进程/中间件」的第一层：项目是集群的
// 分组容器；集群通过 Cluster.ProjectID 归属项目（缺省 = 未归属）。
package store

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/syndtr/goleveldb/leveldb"
)

// Project 是集群分组（项目）。
type Project struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Desc      string    `json:"desc,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	// ClusterCount 是归属此项目的集群数（运行时由 cluster.Service 统计填充，
	// 不持久化）。
	ClusterCount int `json:"cluster_count,omitempty"`
}

// projectKey 返回项目 key。
func projectKey(id string) string { return BucketProject + "/" + id }

// PutProject 写入（新增或更新）项目。
func (s *Store) PutProject(p *Project) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("marshal project %q: %w", p.ID, err)
	}
	return s.db.Put([]byte(projectKey(p.ID)), data, nil)
}

// GetProject 读取单个项目；不存在时返回 (nil, nil)。
func (s *Store) GetProject(id string) (*Project, error) {
	raw, err := s.get(projectKey(id))
	if err == leveldb.ErrNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p Project
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("decode project %q: %w", id, err)
	}
	return &p, nil
}

// DeleteProject 删除项目（不级联删除集群，仅解除归属）。
func (s *Store) DeleteProject(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Delete([]byte(projectKey(id)), nil)
}

// ListProjects 列出全部项目（按 ID 字典序）。
func (s *Store) ListProjects() ([]*Project, error) {
	var out []*Project
	err := s.iterate(BucketProject+"/", func(key string, value []byte) error {
		if strings.HasPrefix(key, BucketProject+"/idx/") {
			return nil // 跳过复合索引
		}
		var p Project
		if err := json.Unmarshal(value, &p); err != nil {
			return fmt.Errorf("decode project %q: %w", key, err)
		}
		out = append(out, &p)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
