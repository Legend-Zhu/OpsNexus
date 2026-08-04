// Patrol (YAML 巡检流程), its execution records and AI reports (P5).
//
// Key layout (mirrors 设计方案 §七):
//
//	patrol/<id>                    -> Patrol{id,name,description,cron,enabled,yaml,...}
//	patrolrun/<seq>                -> PatrolRun（seq 递增 = 时间序，最新在后）
//	report/<patrol_run_id>         -> Report{patrol_run_id, ai_summary}
package store

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/syndtr/goleveldb/leveldb"
)

// Patrol 巡检流程定义（YAML 存储原文，执行时解析）。
type Patrol struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Cron        string    `json:"cron"`              // 5 字段 cron 表达式
	Enabled     bool      `json:"enabled"`
	YAML        string    `json:"yaml"`              // 流程定义原文（校验/解析在 patrol 服务）
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// PatrolRunStatus 巡检执行状态。
type PatrolRunStatus string

// 巡检执行状态常量。
const (
	RunRunning PatrolRunStatus = "running"
	RunSuccess PatrolRunStatus = "success"
	RunFailed  PatrolRunStatus = "failed"
)

// PatrolRun 一次巡检执行记录。
type PatrolRun struct {
	ID         string          `json:"id"`
	PatrolID   string          `json:"patrol_id"`
	Seq        uint64          `json:"seq"`
	StartedAt  time.Time       `json:"started_at"`
	FinishedAt *time.Time      `json:"finished_at,omitempty"`
	Status     PatrolRunStatus `json:"status"`
	Error      string          `json:"error,omitempty"`
	// Anomalies 检查阶段发现的异常列表（每个检查项的结论）。
	Anomalies []Anomaly `json:"anomalies,omitempty"`
	// ReportID 关联的 AI 报告（report/<id>）。
	ReportID string `json:"report_id,omitempty"`
}

// Anomaly 单个检查项的结果。
type Anomaly struct {
	Check   string `json:"check"`   // 检查项标识，如 "resource/web/cpu>85"
	Cluster string `json:"cluster,omitempty"`
	Service string `json:"service,omitempty"`
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
	Data    string `json:"data,omitempty"` // 检查原始数据（截断）
}

// Report 巡检 AI 报告。
type Report struct {
	ID          string    `json:"id"` // = patrol_run_id
	PatrolRunID string    `json:"patrol_run_id"`
	PatrolID    string    `json:"patrol_id,omitempty"`
	Summary     string    `json:"ai_summary"`
	Model       string    `json:"model,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

func patrolKey(id string) string { return BucketPatrol + "/" + id }

func patrolRunKey(seq uint64) string {
	return fmt.Sprintf("%s/%020d", BucketPatrolRun, seq)
}

func reportKey(runID string) string { return BucketReport + "/" + runID }

// --- Patrol CRUD ---

// PutPatrol 写入（新增或更新）巡检流程。
func (s *Store) PutPatrol(p *Patrol) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("marshal patrol: %w", err)
	}
	return s.db.Put([]byte(patrolKey(p.ID)), data, nil)
}

// GetPatrol 读取单个流程；不存在返回 (nil, nil)。
func (s *Store) GetPatrol(id string) (*Patrol, error) {
	raw, err := s.get(patrolKey(id))
	if err == leveldb.ErrNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p Patrol
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("decode patrol %q: %w", id, err)
	}
	return &p, nil
}

// DeletePatrol 删除流程（执行记录保留，仅移除定义）。
func (s *Store) DeletePatrol(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Delete([]byte(patrolKey(id)), nil)
}

// ListPatrols 列出全部流程。
func (s *Store) ListPatrols() ([]*Patrol, error) {
	var out []*Patrol
	err := s.iterate(BucketPatrol+"/", func(key string, value []byte) error {
		var p Patrol
		if err := json.Unmarshal(value, &p); err != nil {
			return fmt.Errorf("decode patrol %q: %w", key, err)
		}
		out = append(out, &p)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// --- PatrolRun ---

// SavePatrolRun 写入执行记录（新增或状态更新）。
func (s *Store) SavePatrolRun(r *PatrolRun) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("marshal run: %w", err)
	}
	return s.db.Put([]byte(patrolRunKey(r.Seq)), data, nil)
}

// GetPatrolRun 按 id 读取执行记录。
func (s *Store) GetPatrolRun(id string) (*PatrolRun, error) {
	// id 格式 "run-<seq>"
	var seq uint64
	if _, err := fmt.Sscanf(id, "run-%d", &seq); err != nil {
		return nil, fmt.Errorf("invalid run id %q", id)
	}
	raw, err := s.get(patrolRunKey(seq))
	if err == leveldb.ErrNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r PatrolRun
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("decode run %q: %w", id, err)
	}
	return &r, nil
}

// ListPatrolRuns 按流程列出执行记录（最新在前）。
func (s *Store) ListPatrolRuns(patrolID string, limit int) ([]*PatrolRun, error) {
	var out []*PatrolRun
	err := s.iterate(BucketPatrolRun+"/", func(key string, value []byte) error {
		var r PatrolRun
		if err := json.Unmarshal(value, &r); err != nil {
			return fmt.Errorf("decode run %q: %w", key, err)
		}
		if patrolID != "" && r.PatrolID != patrolID {
			return nil
		}
		out = append(out, &r)
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

// --- Report ---

// SaveReport 写入 AI 报告（id = patrol_run_id）。
func (s *Store) SaveReport(r *Report) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("marshal report: %w", err)
	}
	return s.db.Put([]byte(reportKey(r.ID)), data, nil)
}

// GetReport 按 id 读取报告。
func (s *Store) GetReport(id string) (*Report, error) {
	raw, err := s.get(reportKey(id))
	if err == leveldb.ErrNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r Report
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("decode report %q: %w", id, err)
	}
	return &r, nil
}

// ListReports 按流程列出报告（经执行记录反查，最新在前）。
func (s *Store) ListReports(patrolID string, limit int) ([]*Report, error) {
	runs, err := s.ListPatrolRuns(patrolID, limit)
	if err != nil {
		return nil, err
	}
	var out []*Report
	for _, r := range runs {
		if r.ReportID == "" {
			continue
		}
		rep, err := s.GetReport(r.ReportID)
		if err != nil {
			return nil, err
		}
		if rep != nil {
			out = append(out, rep)
		}
	}
	return out, nil
}
