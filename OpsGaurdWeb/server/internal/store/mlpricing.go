// 模型单价快照（MLOps P2，方案 §4.6）。
//
// 单价为定点整数：PriceInPerMMicro/PriceOutPerMMicro = 元/百万 token × 1e6
// （微元）。cost_minor = tokens × price / 1e6，整数截断到微元。
// 每次保存生成新 Version（unix nano），明细入账时快照 version + 金额，
// 修改单价只影响之后的调用，不回溯历史。
//
// Key layout:
//
//	mlpricing/<enc provider>/<enc model> -> MLPricing
package store

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/syndtr/goleveldb/leveldb"
)

// MLPricing 一个 (provider, model) 的当前单价快照。
type MLPricing struct {
	Provider          string    `json:"provider"`
	Model             string    `json:"model"`
	Currency          string    `json:"currency"`              // 当前固定 CNY
	PriceInPerMMicro  int64     `json:"price_in_per_m_micro"`  // 元/百万 token（输入），微元定点
	PriceOutPerMMicro int64     `json:"price_out_per_m_micro"` // 元/百万 token（输出）
	Version           string    `json:"version"`               // 快照版本（updated unix nano）
	UpdatedAt         time.Time `json:"updated_at"`
	UpdatedBy         string    `json:"updated_by,omitempty"`
	Note              string    `json:"note,omitempty"`
}

func mlPricingKey(provider, model string) string {
	return fmt.Sprintf("%s/%s/%s", BucketMLPricing, encKeyPart(provider), encKeyPart(model))
}

// SaveMLPricing 保存（覆盖）一个 (provider, model) 的单价。
func (s *Store) SaveMLPricing(p *MLPricing) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("marshal mlpricing: %w", err)
	}
	return s.db.Put([]byte(mlPricingKey(p.Provider, p.Model)), data, nil)
}

// GetMLPricing 读取单价；不存在返回 (nil, nil)。
func (s *Store) GetMLPricing(provider, model string) (*MLPricing, error) {
	raw, err := s.get(mlPricingKey(provider, model))
	if err == leveldb.ErrNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p MLPricing
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("decode mlpricing: %w", err)
	}
	return &p, nil
}

// ListMLPricings 列出全部单价（provider/model 字典序稳定）。
func (s *Store) ListMLPricings() ([]*MLPricing, error) {
	var out []*MLPricing
	err := s.iterate(BucketMLPricing+"/", func(_ string, value []byte) error {
		var p MLPricing
		if err := json.Unmarshal(value, &p); err != nil {
			return fmt.Errorf("decode mlpricing: %w", err)
		}
		out = append(out, &p)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].Model < out[j].Model
	})
	return out, nil
}

// DeleteMLPricing 删除单价（之后该模型调用 priced=false）。
func (s *Store) DeleteMLPricing(provider, model string) error {
	return s.db.Delete([]byte(mlPricingKey(provider, model)), nil)
}
