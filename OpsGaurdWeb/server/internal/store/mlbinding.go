// 场景模型绑定（MLOps P3，方案 §5.3）。绑定独立于网关配置与 Prompt 版本，
// 只影响之后的新 operation；历史 usage 以实际调用模型为准。
//
// Key layout:
//
//	mlbinding/<scenario> -> ScenarioBinding
package store

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/syndtr/goleveldb/leveldb"
)

// ScenarioBinding 一个业务场景的默认模型绑定。
type ScenarioBinding struct {
	Scenario  string    `json:"scenario"` // chat | investigate | native_chat | patrol_report
	Model     string    `json:"model"`
	UpdatedBy string    `json:"updated_by,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// SaveScenarioBinding 保存（覆盖）一个场景的绑定。
func (s *Store) SaveScenarioBinding(b *ScenarioBinding) error {
	if b.Scenario == "" {
		return fmt.Errorf("scenario is required")
	}
	data, err := json.Marshal(b)
	if err != nil {
		return fmt.Errorf("marshal mlbinding: %w", err)
	}
	return s.db.Put([]byte(BucketMLBinding+"/"+b.Scenario), data, nil)
}

// GetScenarioBinding 读取绑定；不存在返回 (nil, nil)。
func (s *Store) GetScenarioBinding(scenario string) (*ScenarioBinding, error) {
	raw, err := s.get(BucketMLBinding + "/" + scenario)
	if err == leveldb.ErrNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var b ScenarioBinding
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, fmt.Errorf("decode mlbinding: %w", err)
	}
	return &b, nil
}

// ListScenarioBindings 列出全部绑定（场景字典序）。
func (s *Store) ListScenarioBindings() ([]*ScenarioBinding, error) {
	var out []*ScenarioBinding
	err := s.iterate(BucketMLBinding+"/", func(_ string, value []byte) error {
		var b ScenarioBinding
		if err := json.Unmarshal(value, &b); err != nil {
			return fmt.Errorf("decode mlbinding: %w", err)
		}
		out = append(out, &b)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Scenario < out[j].Scenario })
	return out, nil
}

// DeleteScenarioBinding 删除绑定（场景回退默认可用模型）。
func (s *Store) DeleteScenarioBinding(scenario string) error {
	return s.db.Delete([]byte(BucketMLBinding+"/"+scenario), nil)
}
