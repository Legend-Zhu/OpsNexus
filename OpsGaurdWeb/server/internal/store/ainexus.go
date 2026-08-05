// AiNexus 运行时网关配置持久化：key ainexus/runtime，值为 YAML 文本
// （ainexuscfg.Config 的 yaml tag 原生往返 time.Duration，不走 JSON）。
// 仅当页面保存过网关配置时写入；从未保存则不落库，以 config.yaml 为源。
package store

import (
	"github.com/syndtr/goleveldb/leveldb"
)

// GetAINexusRuntime 读取运行时网关配置 YAML；从未保存返回 (nil, nil)。
func (s *Store) GetAINexusRuntime() ([]byte, error) {
	raw, err := s.get(BucketAINexus + "/runtime")
	if err == leveldb.ErrNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// PutAINexusRuntime 持久化运行时网关配置 YAML（纯文本，非 JSON）。
func (s *Store) PutAINexusRuntime(yamlText string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Put([]byte(BucketAINexus+"/runtime"), []byte(yamlText), nil)
}
