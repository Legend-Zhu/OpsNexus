// Secrets (密钥引用存储): secret/<name> -> Secret{name, value, ...}
// 供巡检 flow 检查等场景引用（YAML 里写 ${secret:name}，执行时替换），
// 避免凭据明文落流程定义/页面展示。列表接口绝不返回值。
package store

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/syndtr/goleveldb/leveldb"
)

// Secret 一条密钥（拨测账号、API token 等）。
type Secret struct {
	Name      string    `json:"name"`
	Value     string    `json:"value"` // 仅读写路径使用,列表不返回
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func secretKey(name string) string { return BucketSecret + "/" + name }

// PutSecret 写入（新增或覆盖）密钥。
func (s *Store) PutSecret(sec *Secret) error {
	if sec.Name == "" {
		return fmt.Errorf("secret name is empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	if sec.CreatedAt.IsZero() {
		if old, _ := s.get(secretKey(sec.Name)); len(old) > 0 {
			var prev Secret
			if json.Unmarshal(old, &prev) == nil {
				sec.CreatedAt = prev.CreatedAt
			}
		}
		if sec.CreatedAt.IsZero() {
			sec.CreatedAt = now
		}
	}
	sec.UpdatedAt = now
	data, err := json.Marshal(sec)
	if err != nil {
		return fmt.Errorf("marshal secret: %w", err)
	}
	return s.db.Put([]byte(secretKey(sec.Name)), data, nil)
}

// GetSecret 读取密钥值；不存在返回 (nil, nil)。
func (s *Store) GetSecret(name string) (*Secret, error) {
	raw, err := s.get(secretKey(name))
	if err == leveldb.ErrNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var sec Secret
	if err := json.Unmarshal(raw, &sec); err != nil {
		return nil, fmt.Errorf("decode secret %q: %w", name, err)
	}
	return &sec, nil
}

// ListSecrets 列出密钥名（不含值）。
func (s *Store) ListSecrets() ([]*Secret, error) {
	var out []*Secret
	err := s.iterate(BucketSecret+"/", func(key string, value []byte) error {
		var sec Secret
		if err := json.Unmarshal(value, &sec); err != nil {
			return fmt.Errorf("decode secret %q: %w", key, err)
		}
		sec.Value = "" // 列表不返回值
		out = append(out, &sec)
		return nil
	})
	return out, err
}

// DeleteSecret 删除密钥。
func (s *Store) DeleteSecret(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Delete([]byte(secretKey(name)), nil)
}
