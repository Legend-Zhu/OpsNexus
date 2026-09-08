// MCP token 运行时表与用量统计。
//
//	mcp_token/<name>     -> MCPToken{secret_sha, scope, enabled, ...}
//	mcp_usage/<yyyy-mm-dd> -> {actor|tool|ok: count}（日聚合，flush 合并）
//
// 运行时 token 由管理 API（admin）增删，与 config.yaml 的静态种子并存：
// 静态为底、运行时可覆盖同名 scope / 禁用静态条目 / 新增条目；删除仅限
// 运行时条目（静态条目改 config.yaml）。secret 原文只在创建响应中出现
// 一次，落库的只有 SHA-256 摘要。
package store

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/syndtr/goleveldb/leveldb"
)

// MCPToken 运行时 MCP token。
type MCPToken struct {
	Name string `json:"name"`
	// SecretSHA hex 编码的 SHA-256(secret)。原文不落库。
	SecretSHA string `json:"secret_sha"`
	Scope     string `json:"scope"` // read | write
	// Static true = 来自 config.yaml 种子的运行时镜像（不可删除，仅可禁用/改 scope）。
	Static bool `json:"static,omitempty"`
	// Overridden true = 该条目覆盖了同名静态种子。
	Overridden bool      `json:"overridden,omitempty"`
	Enabled    bool      `json:"enabled"`
	CreatedAt  time.Time `json:"created_at"`
	// LastUsed 最近一次鉴权命中（懒更新，list 时落库）。
	LastUsed time.Time `json:"last_used,omitempty"`
}

func mcpTokenKey(name string) string { return BucketMCPToken + "/" + name }

// GetMCPToken 按名读取；不存在返回 (nil, nil)。
func (s *Store) GetMCPToken(name string) (*MCPToken, error) {
	raw, err := s.get(mcpTokenKey(name))
	if err == leveldb.ErrNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var t MCPToken
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, fmt.Errorf("decode mcp token %q: %w", name, err)
	}
	return &t, nil
}

// PutMCPToken 新增或覆盖。
func (s *Store) PutMCPToken(t *MCPToken) error {
	if t.Name == "" {
		return fmt.Errorf("mcp token name is required")
	}
	return s.put(mcpTokenKey(t.Name), t)
}

// DeleteMCPToken 删除（仅运行时条目；静态条目由调用方拒绝）。
func (s *Store) DeleteMCPToken(name string) error {
	return s.db.Delete([]byte(mcpTokenKey(name)), nil)
}

// ListMCPTokens 全部运行时 token（含静态镜像，name 排序由 LevelDB 保证）。
func (s *Store) ListMCPTokens() ([]*MCPToken, error) {
	var out []*MCPToken
	err := s.iterate(BucketMCPToken+"/", func(key string, value []byte) error {
		var t MCPToken
		if err := json.Unmarshal(value, &t); err != nil {
			return fmt.Errorf("decode mcp token %q: %w", key, err)
		}
		out = append(out, &t)
		return nil
	})
	return out, err
}

// --- 用量（日聚合） ---

// MCPUsageDay 单日用量聚合：key = "actor|tool|ok"（ok 为 0/1）。
type MCPUsageDay struct {
	Day    string           `json:"day"`
	Counts map[string]int64 `json:"counts"`
}

func mcpUsageKey(day string) string { return BucketMCPUsage + "/" + day }

// UsageDay 业务时区的当天 key（yyyy-mm-dd）。
func UsageDay(t time.Time, loc *time.Location) string {
	return t.In(loc).Format("2006-01-02")
}

// AddMCPUsage 把增量合并进指定日（供 flush 调用；key 冲突累加）。
func (s *Store) AddMCPUsage(day string, inc map[string]int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var cur MCPUsageDay
	raw, err := s.get(mcpUsageKey(day))
	if err == leveldb.ErrNotFound {
		// 新的一天，从零开始
	} else if err != nil {
		return err
	} else if err := json.Unmarshal(raw, &cur); err != nil {
		cur = MCPUsageDay{} // 记录损坏不阻断计数
	}
	cur.Day = day
	if cur.Counts == nil {
		cur.Counts = map[string]int64{}
	}
	for k, v := range inc {
		cur.Counts[k] += v
	}
	return s.put(mcpUsageKey(day), cur)
}

// ListMCPUsage 返回最近 days 天的日聚合（按天倒序）。
func (s *Store) ListMCPUsage(days int) ([]*MCPUsageDay, error) {
	var out []*MCPUsageDay
	err := s.iterate(BucketMCPUsage+"/", func(key string, value []byte) error {
		var d MCPUsageDay
		if err := json.Unmarshal(value, &d); err != nil {
			return fmt.Errorf("decode mcp usage %q: %w", key, err)
		}
		out = append(out, &d)
		return nil
	})
	if err != nil {
		return nil, err
	}
	// key 有序即日期有序；倒序（最新在前）
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	if days > 0 && len(out) > days {
		out = out[:days]
	}
	return out, nil
}
