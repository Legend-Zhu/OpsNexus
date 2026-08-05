// Generic key-value settings: settings/<key> -> JSON value.
// 全局配置项（如巡检报告投递策略 patrol-report），页面编辑、服务读取。
package store

import (
	"encoding/json"
	"fmt"

	"github.com/syndtr/goleveldb/leveldb"
)

func settingKey(key string) string { return BucketSettings + "/" + key }

// GetSetting 读取 settings/<key> 的 JSON 值到 v；不存在返回 (false, nil)。
func (s *Store) GetSetting(key string, v any) (bool, error) {
	raw, err := s.get(settingKey(key))
	if err == leveldb.ErrNotFound {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return false, fmt.Errorf("decode setting %q: %w", key, err)
	}
	return true, nil
}

// PutSetting 写入 settings/<key>（JSON 序列化）。
func (s *Store) PutSetting(key string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal setting %q: %w", key, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Put([]byte(settingKey(key)), data, nil)
}
