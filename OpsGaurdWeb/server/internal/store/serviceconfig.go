// Service config snapshots: svccfg/<cluster>/<service> -> the exact config
// body (YAML) the service was last deployed/updated with. The frontend uses
// the snapshot to prefill the "edit service" dialog; without it the editor
// would have to reconstruct the config from the swarm spec (lossy).
package store

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/syndtr/goleveldb/leveldb"
)

// ServiceConfig 一条服务的部署配置快照。
type ServiceConfig struct {
	Cluster   string    `json:"cluster"`
	Service   string    `json:"service"`
	Config    string    `json:"config"` // 原始配置体（YAML/JSON，与部署请求一致）
	UpdatedAt time.Time `json:"updated_at"`
}

func serviceConfigKey(cluster, service string) string {
	return fmt.Sprintf("%s/%s/%s", BucketSvcCfg, cluster, service)
}

// PutServiceConfig 写入（新增或覆盖）服务配置快照。
func (s *Store) PutServiceConfig(cluster, service, config string) error {
	if cluster == "" || service == "" {
		return fmt.Errorf("svccfg: cluster and service are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sc := ServiceConfig{
		Cluster:   cluster,
		Service:   service,
		Config:    config,
		UpdatedAt: time.Now().UTC(),
	}
	data, err := json.Marshal(sc)
	if err != nil {
		return fmt.Errorf("marshal svccfg: %w", err)
	}
	return s.db.Put([]byte(serviceConfigKey(cluster, service)), data, nil)
}

// GetServiceConfig 读取服务配置快照；不存在返回 (nil, nil)。
func (s *Store) GetServiceConfig(cluster, service string) (*ServiceConfig, error) {
	raw, err := s.get(serviceConfigKey(cluster, service))
	if err == leveldb.ErrNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var sc ServiceConfig
	if err := json.Unmarshal(raw, &sc); err != nil {
		return nil, fmt.Errorf("decode svccfg %q/%q: %w", cluster, service, err)
	}
	return &sc, nil
}

// DeleteServiceConfig 删除服务配置快照（服务被移除时清理）。
func (s *Store) DeleteServiceConfig(cluster, service string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Delete([]byte(serviceConfigKey(cluster, service)), nil)
}
