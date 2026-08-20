// provider call 级用量明细与日聚合（MLOps P2，方案 §4.5/§4.7）。
//
// 计量单位是一次底层 provider 调用；金额为定点整数（cost_minor，单位
// 微元 = 1e-6 CNY），不用浮点累计。明细在写入时保存计价快照（priced/
// cost_minor/pricing_version），修改单价不回溯历史。
//
// Key layout:
//
//	mlusage/<seq%020d>          -> MLUsageRecord（seq 递增 = 时间序）
//	mlusage_call/<call_id>      -> seq（call_id 幂等索引，随明细 GC）
//	mlusage_day/<date>/<enc provider>/<enc model>/<enc scenario> -> MLUsageDay
package store

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/util"
)

// MLUsageRecord 一次底层 provider 调用的计量明细。
type MLUsageRecord struct {
	Seq              uint64    `json:"seq"`
	OperationID      string    `json:"operation_id"`
	CallID           string    `json:"call_id"`
	ParentCallID     string    `json:"parent_call_id,omitempty"`
	Provider         string    `json:"provider"`
	Model            string    `json:"model"`
	Scenario         string    `json:"scenario"`
	EntryPoint       string    `json:"entry_point,omitempty"`
	Round            int       `json:"round,omitempty"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	TotalTokens      int       `json:"total_tokens"`
	UsagePresent     bool      `json:"usage_present"` // false = 上游未返回 usage（≠0 token）
	OK               bool      `json:"ok"`
	Status           string    `json:"status"` // success | provider_error | canceled
	Error            string    `json:"error,omitempty"`
	Priced           bool      `json:"priced"` // false = 无单价（≠免费：免费是 priced=true 且金额 0）
	Currency         string    `json:"currency,omitempty"`
	CostMinor        int64     `json:"cost_minor"` // 微元（1e-6 CNY）
	PricingVersion   string    `json:"pricing_version,omitempty"`
	StartedAt        time.Time `json:"started_at"`
	FinishedAt       time.Time `json:"finished_at"`
	LatencyMs        int64     `json:"latency_ms"`
	Day              string    `json:"day"` // 业务时区日期 yyyy-mm-dd（写入时确定）
}

// MLUsageDay 一个（日, provider, model, scenario）组合的聚合行。
// 读改写只在 RecordMLUsage 的串行批处理路径内完成，保证不丢计数。
type MLUsageDay struct {
	Day              string    `json:"day"`
	Provider         string    `json:"provider"`
	Model            string    `json:"model"`
	Scenario         string    `json:"scenario"`
	Calls            int64     `json:"calls"`
	SuccessCalls     int64     `json:"success_calls"`
	ErrorCalls       int64     `json:"error_calls"` // provider_error
	CanceledCalls    int64     `json:"canceled_calls"`
	UnmeteredCalls   int64     `json:"unmetered_calls"` // usage 缺失
	PromptTokens     int64     `json:"prompt_tokens"`
	CompletionTokens int64     `json:"completion_tokens"`
	TotalTokens      int64     `json:"total_tokens"`
	PricedCalls      int64     `json:"priced_calls"`
	CostMinor        int64     `json:"cost_minor"`
	Operations       int64     `json:"operations"` // 当日不同 operation 数（估算，见 collector）
	FirstAt          time.Time `json:"first_at"`
	LastAt           time.Time `json:"last_at"`
}

// scanListCap 明细扫描的硬上限：90 天保留 × 单实例低流量下足够，
// 防御性避免无上限读入内存（超限返回 truncated 标记）。
const scanListCap = 20000

func mlUsageKey(seq uint64) string { return fmt.Sprintf("%s/%020d", BucketMLUsage, seq) }

func mlUsageCallKey(callID string) string { return BucketMLUsageCall + "/" + callID }

func mlUsageDayKey(day, provider, model, scenario string) string {
	return fmt.Sprintf("%s/%s/%s/%s/%s", BucketMLUsageDay, day,
		encKeyPart(provider), encKeyPart(model), encKeyPart(scenario))
}

// encKeyPart 把 key 层级段编码为 LevelDB 安全形式：可见 ASCII 中仅
// '%' 和 '/' 转义，其余非安全字节（空格/控制字符/非 ASCII）统一 %XX。
// 模型名可能含 '/'（如 "openai/gpt-4"），禁止裸拼 key。
func encKeyPart(s string) string {
	const hexDigits = "0123456789ABCDEF"
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '%' || c == '/' || c < 0x20 || c > 0x7e:
			out = append(out, '%', hexDigits[c>>4], hexDigits[c&0xf])
		default:
			out = append(out, c)
		}
	}
	return string(out)
}

// RecordMLUsage 原子写入一条计量明细：call_id 幂等检查 + seq 分配 +
// 明细 + 幂等索引 + 日聚合读改写在同一串行批处理中提交。
// 重复 call_id 返回 saved=false（幂等跳过，非错误）。
// bumpOperation 表示这是该 operation 当日的首条入账（collector 去重后
// 传入），用于日聚合的 Operations 估算计数。
// 调用方需先填好计价快照字段（Priced/CostMinor/PricingVersion/Day）。
func (s *Store) RecordMLUsage(rec *MLUsageRecord, bumpOperation bool) (saved bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if rec.CallID != "" {
		if _, err := s.db.Get([]byte(mlUsageCallKey(rec.CallID)), nil); err == nil {
			return false, nil // 重复完成事件：已入账
		} else if err != leveldb.ErrNotFound {
			return false, err
		}
	}
	seq, err := s.nextSeqLocked("mlusage")
	if err != nil {
		return false, fmt.Errorf("mlusage nextseq: %w", err)
	}
	rec.Seq = seq

	dayKey := mlUsageDayKey(rec.Day, rec.Provider, rec.Model, rec.Scenario)
	day := &MLUsageDay{Day: rec.Day, Provider: rec.Provider, Model: rec.Model, Scenario: rec.Scenario}
	if raw, err := s.db.Get([]byte(dayKey), nil); err == nil {
		if err := json.Unmarshal(raw, day); err != nil {
			return false, fmt.Errorf("decode mlusage_day %q: %w", dayKey, err)
		}
	} else if err != leveldb.ErrNotFound {
		return false, err
	}
	accrueUsageDay(day, rec, bumpOperation)

	batch := new(leveldb.Batch)
	detail, err := json.Marshal(rec)
	if err != nil {
		return false, fmt.Errorf("marshal mlusage: %w", err)
	}
	dayData, err := json.Marshal(day)
	if err != nil {
		return false, fmt.Errorf("marshal mlusage_day: %w", err)
	}
	batch.Put([]byte(mlUsageKey(seq)), detail)
	if rec.CallID != "" {
		batch.Put([]byte(mlUsageCallKey(rec.CallID)), []byte(fmt.Sprintf("%d", seq)))
	}
	batch.Put([]byte(dayKey), dayData)
	if err := s.db.Write(batch, nil); err != nil {
		return false, fmt.Errorf("write mlusage batch: %w", err)
	}
	return true, nil
}

// accrueUsageDay 把一条明细累计进日聚合行。
func accrueUsageDay(day *MLUsageDay, rec *MLUsageRecord, bumpOperation bool) {
	day.Calls++
	if bumpOperation {
		day.Operations++
	}
	switch {
	case rec.Status == "success":
		day.SuccessCalls++
	case rec.Status == "canceled":
		day.CanceledCalls++
	default:
		day.ErrorCalls++
	}
	if !rec.UsagePresent {
		day.UnmeteredCalls++
	}
	day.PromptTokens += int64(rec.PromptTokens)
	day.CompletionTokens += int64(rec.CompletionTokens)
	day.TotalTokens += int64(rec.TotalTokens)
	if rec.Priced {
		day.PricedCalls++
		day.CostMinor += rec.CostMinor
	}
	if day.FirstAt.IsZero() || rec.StartedAt.Before(day.FirstAt) {
		day.FirstAt = rec.StartedAt
	}
	if rec.StartedAt.After(day.LastAt) {
		day.LastAt = rec.StartedAt
	}
}

// MLUsageFilter 明细查询条件（全部可选）。
type MLUsageFilter struct {
	From, To      time.Time // StartedAt 范围（含两端，UTC 比较）
	Provider      string
	Model         string
	Scenario      string
	Status        string // success | provider_error | canceled
	OperationID   string
	BeforeSeq     uint64 // 游标：仅 seq < before_seq（0 = 不限）
	Limit, Offset int
}

// ListMLUsage 按过滤条件列出明细（seq 降序 = 最新在前）。
// total 为匹配条数（受 scanListCap 截断），truncated 表示超上限截断。
func (s *Store) ListMLUsage(f MLUsageFilter) (items []*MLUsageRecord, total int, truncated bool, err error) {
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	var matched []*MLUsageRecord
	trunc := false
	iterErr := s.iterate(BucketMLUsage+"/", func(key string, value []byte) error {
		var seq uint64
		if _, err := fmt.Sscanf(key, BucketMLUsage+"/%d", &seq); err != nil {
			return nil // 非明细键（防御）
		}
		var r MLUsageRecord
		if err := json.Unmarshal(value, &r); err != nil {
			return fmt.Errorf("decode mlusage %q: %w", key, err)
		}
		if !matchUsage(&r, &f) {
			return nil
		}
		if len(matched) >= scanListCap {
			trunc = true
			return nil
		}
		matched = append(matched, &r)
		return nil
	})
	if iterErr != nil {
		return nil, 0, false, iterErr
	}
	// seq 升序收集 → 反转为最新在前
	for i, j := 0, len(matched)-1; i < j; i, j = i+1, j-1 {
		matched[i], matched[j] = matched[j], matched[i]
	}
	total = len(matched)
	off := f.Offset
	if off > total {
		off = total
	}
	end := off + limit
	if end > total {
		end = total
	}
	return matched[off:end], total, trunc, nil
}

// ListMLUsageByOperation 一次业务 operation 下的全部 provider call（时间序）。
func (s *Store) ListMLUsageByOperation(operationID string) ([]*MLUsageRecord, error) {
	if operationID == "" {
		return nil, nil
	}
	var out []*MLUsageRecord
	err := s.iterate(BucketMLUsage+"/", func(_ string, value []byte) error {
		var r MLUsageRecord
		if err := json.Unmarshal(value, &r); err != nil {
			return fmt.Errorf("decode mlusage: %w", err)
		}
		if r.OperationID == operationID {
			out = append(out, &r)
		}
		return nil
	})
	return out, err
}

func matchUsage(r *MLUsageRecord, f *MLUsageFilter) bool {
	if !f.From.IsZero() && r.StartedAt.Before(f.From) {
		return false
	}
	if !f.To.IsZero() && r.StartedAt.After(f.To) {
		return false
	}
	if f.Provider != "" && r.Provider != f.Provider {
		return false
	}
	if f.Model != "" && r.Model != f.Model {
		return false
	}
	if f.Scenario != "" && r.Scenario != f.Scenario {
		return false
	}
	if f.Status != "" && r.Status != f.Status {
		return false
	}
	if f.OperationID != "" && r.OperationID != f.OperationID {
		return false
	}
	if f.BeforeSeq != 0 && r.Seq >= f.BeforeSeq {
		return false
	}
	return true
}

// ListMLUsageDays 列出日期范围（含两端，yyyy-mm-dd 字符串序）内的日聚合行。
func (s *Store) ListMLUsageDays(fromDay, toDay string) ([]*MLUsageDay, error) {
	var out []*MLUsageDay
	err := s.iterate(BucketMLUsageDay+"/", func(_ string, value []byte) error {
		var d MLUsageDay
		if err := json.Unmarshal(value, &d); err != nil {
			return fmt.Errorf("decode mlusage_day: %w", err)
		}
		if fromDay != "" && d.Day < fromDay {
			return nil
		}
		if toDay != "" && d.Day > toDay {
			return nil
		}
		out = append(out, &d)
		return nil
	})
	return out, err
}

// GCMLUsageBefore 删除 StartedAt 早于 cutoff 的明细（含幂等索引），
// 单批最多 batchLimit 条，返回本批删除数；日聚合不受影响。
// 反复调用直到返回 0 即清理完毕（限量分批避免长时间阻塞写入）。
func (s *Store) GCMLUsageBefore(cutoff time.Time, batchLimit int) (int, error) {
	if batchLimit <= 0 {
		batchLimit = 500
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	batch := new(leveldb.Batch)
	deleted := 0
	iter := s.db.NewIterator(util.BytesPrefix([]byte(BucketMLUsage+"/")), nil)
	for iter.Next() {
		var r MLUsageRecord
		if err := json.Unmarshal(iter.Value(), &r); err != nil {
			iter.Release()
			return deleted, fmt.Errorf("decode mlusage %q: %w", iter.Key(), err)
		}
		if !r.StartedAt.Before(cutoff) {
			continue
		}
		batch.Delete(iter.Key())
		if r.CallID != "" {
			batch.Delete([]byte(mlUsageCallKey(r.CallID)))
		}
		deleted++
		if deleted >= batchLimit {
			break
		}
	}
	iter.Release()
	if err := iter.Error(); err != nil {
		return deleted, err
	}
	if deleted > 0 {
		if err := s.db.Write(batch, nil); err != nil {
			return 0, fmt.Errorf("gc mlusage batch: %w", err)
		}
	}
	return deleted, nil
}
