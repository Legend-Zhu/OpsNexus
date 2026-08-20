// MLOps 管理操作审计（P1）：提示词/模型/价格/预算等写操作留痕。
// 敏感内容（模板正文、密钥）不落审计，只记录 hash 摘要。
//
// Key layout:
//
//	mlops_audit/<seq%020d> -> MLOpsAudit（seq 递增 = 时间序）
package store

import (
	"encoding/json"
	"fmt"
	"time"
)

// MLOpsAudit 一条管理操作审计记录。
type MLOpsAudit struct {
	ID         string    `json:"id"`
	Seq        uint64    `json:"seq"`
	Operator   string    `json:"operator"`
	Action     string    `json:"action"`      // prompt_save | prompt_activate | ...
	ObjectType string    `json:"object_type"` // prompt | model | pricing | budget
	ObjectID   string    `json:"object_id"`
	BeforeHash string    `json:"before_hash,omitempty"`
	AfterHash  string    `json:"after_hash,omitempty"`
	Result     string    `json:"result"` // ok | error
	Error      string    `json:"error,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

func mlopsAuditKey(seq uint64) string {
	return fmt.Sprintf("%s/%020d", BucketMLOpsAudit, seq)
}

// SaveMLOpsAudit 写入审计记录（Seq 由调用方经 NextSeq 分配）。
func (s *Store) SaveMLOpsAudit(a *MLOpsAudit) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := json.Marshal(a)
	if err != nil {
		return fmt.Errorf("marshal mlops audit: %w", err)
	}
	return s.db.Put([]byte(mlopsAuditKey(a.Seq)), data, nil)
}

// ListMLOpsAudits 列出审计记录（最新在前）。
func (s *Store) ListMLOpsAudits(limit int) ([]*MLOpsAudit, error) {
	var out []*MLOpsAudit
	err := s.iterate(BucketMLOpsAudit+"/", func(_ string, value []byte) error {
		var a MLOpsAudit
		if err := json.Unmarshal(value, &a); err != nil {
			return fmt.Errorf("decode mlops audit: %w", err)
		}
		out = append(out, &a)
		return nil
	})
	if err != nil {
		return nil, err
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
