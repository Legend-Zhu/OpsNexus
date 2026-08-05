// Investigation 排查会话落库（对话式 troubleshoot）。
//
//	investigation/<seq%020d> -> Investigation{id, alert_id, title, messages, conclusion, ...}
//
// 与巡检报告的区别：排查会话由用户驱动（选告警或自由提问），内容是多轮对话；
// 关联告警时回写 Alert.investigations/last_investigation_id 形成「告警→排查」闭环。
package store

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/syndtr/goleveldb/leveldb"
)

// Investigation 一次排查会话的落库记录。
type Investigation struct {
	ID         string    `json:"id"` // inv-<seq>
	AlertID    string    `json:"alert_id,omitempty"`
	Cluster    string    `json:"cluster,omitempty"`
	Title      string    `json:"title"`                // 告警标题或首条用户问题（截断）
	Messages   string    `json:"messages"`             // 对话 [{role,content}] JSON（保存前整体截断）
	Conclusion string    `json:"conclusion,omitempty"` // 末条 assistant 结论（截断）
	Model      string    `json:"model,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

func investigationKey(seq uint64) string {
	return fmt.Sprintf("%s/%020d", BucketInvestigation, seq)
}

// SaveInvestigation 新建（ID 为空，分配 inv-<seq>）或更新（按 ID 解析 seq）排查记录。
func (s *Store) SaveInvestigation(inv *Investigation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var seq uint64
	if inv.ID == "" {
		n, err := s.nextSeqLocked("investigation")
		if err != nil {
			return err
		}
		seq = n
		inv.ID = fmt.Sprintf("inv-%d", seq)
		inv.CreatedAt = time.Now().UTC()
	} else {
		if _, err := fmt.Sscanf(inv.ID, "inv-%d", &seq); err != nil {
			return fmt.Errorf("invalid investigation id %q", inv.ID)
		}
	}
	inv.UpdatedAt = time.Now().UTC()
	data, err := json.Marshal(inv)
	if err != nil {
		return fmt.Errorf("marshal investigation: %w", err)
	}
	return s.db.Put([]byte(investigationKey(seq)), data, nil)
}

// GetInvestigation 按 id 读取；不存在返回 (nil, nil)。
func (s *Store) GetInvestigation(id string) (*Investigation, error) {
	var seq uint64
	if _, err := fmt.Sscanf(id, "inv-%d", &seq); err != nil {
		return nil, fmt.Errorf("invalid investigation id %q", id)
	}
	raw, err := s.get(investigationKey(seq))
	if err == leveldb.ErrNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var inv Investigation
	if err := json.Unmarshal(raw, &inv); err != nil {
		return nil, fmt.Errorf("decode investigation %q: %w", id, err)
	}
	return &inv, nil
}

// ListInvestigations 按告警列出排查记录（alertID 空 = 全部；最新在前）。
func (s *Store) ListInvestigations(alertID string, limit int) ([]*Investigation, error) {
	var out []*Investigation
	err := s.iterate(BucketInvestigation+"/", func(key string, value []byte) error {
		var inv Investigation
		if err := json.Unmarshal(value, &inv); err != nil {
			return fmt.Errorf("decode investigation %q: %w", key, err)
		}
		if alertID != "" && inv.AlertID != alertID {
			return nil
		}
		out = append(out, &inv)
		return nil
	})
	if err != nil {
		return nil, err
	}
	// 倒序（最新在前）
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
