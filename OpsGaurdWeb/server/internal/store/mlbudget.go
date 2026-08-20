// 月度预算（MLOps P3，方案 §4.8）。超限只告警不拦截 LLM 请求；
// 档位（如 warn_at、100%）以 Notified 数组做稳定去重——月份+档位在
// 更新窗口内只通知一次，跨月新预算天然不复用旧通知状态。
//
// Key layout:
//
//	mlbudget/<yyyy-mm> -> MLBudget
package store

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/syndtr/goleveldb/leveldb"
)

// MLBudget 一个自然月的费用预算。
type MLBudget struct {
	Month      string    `json:"month"`       // 业务时区 yyyy-mm
	Currency   string    `json:"currency"`    // CNY
	LimitMinor int64     `json:"limit_minor"` // 预算上限（微元）
	WarnAt     float64   `json:"warn_at"`     // 预警档位 0~1（如 0.8）
	Notified   []float64 `json:"notified"`    // 已通知过的档位（0.8/1.0 …）
	UpdatedBy  string    `json:"updated_by,omitempty"`
	UpdatedAt  time.Time `json:"updated_at"`
}

func mlBudgetKey(month string) string { return BucketMLBudget + "/" + month }

// SaveMLBudget 保存（覆盖）指定月份预算。
func (s *Store) SaveMLBudget(b *MLBudget) error {
	data, err := json.Marshal(b)
	if err != nil {
		return fmt.Errorf("marshal mlbudget: %w", err)
	}
	return s.db.Put([]byte(mlBudgetKey(b.Month)), data, nil)
}

// GetMLBudget 读取预算；不存在返回 (nil, nil)。
func (s *Store) GetMLBudget(month string) (*MLBudget, error) {
	raw, err := s.get(mlBudgetKey(month))
	if err == leveldb.ErrNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var b MLBudget
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, fmt.Errorf("decode mlbudget: %w", err)
	}
	return &b, nil
}

// MarkBudgetNotified 原子地补记一个已通知档位：串行窗口内重读 → 查重 →
// 追加 → 落盘。已存在返回 false（并发/重复触发时只通知一次）。
func (s *Store) MarkBudgetNotified(month string, tier float64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	raw, err := s.db.Get([]byte(mlBudgetKey(month)), nil)
	if err != nil {
		return false, err
	}
	var b MLBudget
	if err := json.Unmarshal(raw, &b); err != nil {
		return false, fmt.Errorf("decode mlbudget: %w", err)
	}
	for _, t := range b.Notified {
		if t == tier {
			return false, nil
		}
	}
	b.Notified = append(b.Notified, tier)
	sort.Float64s(b.Notified)
	data, err := json.Marshal(&b)
	if err != nil {
		return false, err
	}
	return true, s.db.Put([]byte(mlBudgetKey(month)), data, nil)
}

// ListMLBudgets 列出全部预算（月份字典序）。
func (s *Store) ListMLBudgets() ([]*MLBudget, error) {
	var out []*MLBudget
	err := s.iterate(BucketMLBudget+"/", func(_ string, value []byte) error {
		var b MLBudget
		if err := json.Unmarshal(value, &b); err != nil {
			return fmt.Errorf("decode mlbudget: %w", err)
		}
		out = append(out, &b)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Month < out[j].Month })
	return out, nil
}

// DeleteMLBudget 删除指定月份预算。
func (s *Store) DeleteMLBudget(month string) error {
	if _, err := s.db.Get([]byte(mlBudgetKey(month)), nil); err == leveldb.ErrNotFound {
		return fmt.Errorf("budget %q not found", month)
	} else if err != nil {
		return err
	}
	return s.db.Delete([]byte(mlBudgetKey(month)), nil)
}
