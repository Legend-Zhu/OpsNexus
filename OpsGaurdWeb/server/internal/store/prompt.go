// Prompt Hub (MLOps P1): scenario-keyed prompt templates with version
// history. Builtin scenarios (investigate_system / investigate_user /
// compress_system / patrol_system) are materialized into the store on first
// customization; until then the code builtin (v1) is the source of truth.
//
// Key layout:
//
//	prompt/<id> -> Prompt{id,scenario,name,active_version,versions,...}
package store

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/syndtr/goleveldb/leveldb"
)

// Prompt 提示词模板（含版本历史）。
type Prompt struct {
	ID string `json:"id"`
	// Scenario 场景 key（investigate_system/investigate_user/compress_system/
	// patrol_system/custom）。
	Scenario string `json:"scenario"`
	Name     string `json:"name"`
	// ActiveVersion 当前生效版本号（回滚 = 改这里；v1 恒为代码内置默认）。
	ActiveVersion int `json:"active_version"`
	// Builtin 内置场景（不可删除，仅可还原默认）。
	Builtin bool `json:"builtin"`
	// Materialized 内置场景是否已落库（首次自定义时物化 v1；未物化 = 代码内置）。
	Materialized bool            `json:"materialized,omitempty"`
	Versions     []PromptVersion `json:"versions"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
}

// PromptVersion 单个版本。v1 为代码内置默认的拷贝（Reset 时从代码刷新）。
type PromptVersion struct {
	Version   int              `json:"version"`
	Messages  []PromptMessage  `json:"messages"`
	Variables []PromptVariable `json:"variables,omitempty"`
	Note      string           `json:"note,omitempty"`
	CreatedAt time.Time        `json:"created_at"`
	CreatedBy string           `json:"created_by,omitempty"`
}

// PromptMessage 一条消息模板（结构化，保留 system/user 角色）。
type PromptMessage struct {
	Role     string `json:"role"`     // system | user
	Template string `json:"template"` // text/template 原文
}

// PromptVariable 模板变量说明。
type PromptVariable struct {
	Name     string `json:"name"`
	Required bool   `json:"required"`
	Note     string `json:"note,omitempty"`
}

func promptKey(id string) string { return BucketPrompt + "/" + id }

// PutPrompt 写入提示词记录。
func (s *Store) PutPrompt(p *Prompt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("marshal prompt: %w", err)
	}
	return s.db.Put([]byte(promptKey(p.ID)), data, nil)
}

// GetPrompt 读取单条；不存在返回 (nil, nil)。
func (s *Store) GetPrompt(id string) (*Prompt, error) {
	raw, err := s.get(promptKey(id))
	if err == leveldb.ErrNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p Prompt
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("decode prompt %q: %w", id, err)
	}
	return &p, nil
}

// DeletePrompt 删除提示词记录（仅 custom 场景允许，服务层校验）。
func (s *Store) DeletePrompt(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Delete([]byte(promptKey(id)), nil)
}

// ListPrompts 列出全部已落库的提示词。
func (s *Store) ListPrompts() ([]*Prompt, error) {
	var out []*Prompt
	err := s.iterate(BucketPrompt+"/", func(_ string, value []byte) error {
		var p Prompt
		if err := json.Unmarshal(value, &p); err != nil {
			return fmt.Errorf("decode prompt: %w", err)
		}
		out = append(out, &p)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
