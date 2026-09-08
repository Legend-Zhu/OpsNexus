// MCPAudit 管理端 MCP Server 的写工具调用审计。
//
//	mcp_audit/<seq%020d> -> MCPAudit{actor, tool, args 摘要, ok, error, cost}
//
// 只记写操作（scope=write 的工具）；只读探测不落库、仅 slog，口径与
// Worker 侧审计（deploy/scale 等才审计，check 类不审计）一致。入参大字段
// （config/question）由调用方只记长度，不落正文。
package store

import (
	"encoding/json"
	"fmt"
	"time"
)

// MCPAudit 一条 MCP 工具调用审计记录。
type MCPAudit struct {
	Seq    uint64    `json:"seq"`
	TS     time.Time `json:"ts"`
	Actor  string    `json:"actor"`          // MCP token 名（含来源 IP：name@ip）
	Tool   string    `json:"tool"`           // 工具名
	Args   string    `json:"args,omitempty"` // 入参摘要（≤512 字符；大字段只记长度）
	OK     bool      `json:"ok"`
	Error  string    `json:"error,omitempty"` // 失败原因（截断）
	CostMS int64     `json:"cost_ms"`
}

func mcpAuditKey(seq uint64) string {
	return fmt.Sprintf("%s/%020d", BucketMCPAudit, seq)
}

// SaveMCPAudit 追加一条 MCP 审计记录（分配单调 seq）。
func (s *Store) SaveMCPAudit(a *MCPAudit) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	seq, err := s.nextSeqLocked("mcp_audit")
	if err != nil {
		return err
	}
	a.Seq = seq
	if a.TS.IsZero() {
		a.TS = time.Now().UTC()
	}
	data, err := json.Marshal(a)
	if err != nil {
		return fmt.Errorf("marshal mcp audit: %w", err)
	}
	return s.db.Put([]byte(mcpAuditKey(seq)), data, nil)
}

// ListMCPAudit 最近审计记录（最新在前，limit ≤ 0 = 100）。
func (s *Store) ListMCPAudit(limit int) ([]*MCPAudit, error) {
	if limit <= 0 {
		limit = 100
	}
	var out []*MCPAudit
	err := s.iterate(BucketMCPAudit+"/", func(key string, value []byte) error {
		var a MCPAudit
		if err := json.Unmarshal(value, &a); err != nil {
			return fmt.Errorf("decode mcp audit %q: %w", key, err)
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
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
