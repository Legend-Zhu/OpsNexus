// Ingested monitoring events and aggregated alerts (P3).
//
// Key layout (mirrors 设计方案 §七):
//
//	event/<seq>                       -> IngestEvent（经 gRPC SubscribeEvents 流收到，追加时序）
//	alert/<id>                        -> Alert（按 service+type 聚合）
//	alert/idx/<status>/<cluster>/<ts>/<id> -> "" （列表过滤/排序索引）
//
// Writes that touch alert + its index go through one WriteBatch so they
// commit atomically; read-modify-write (alert count/first/last ts) is
// serialised by the store-wide mutex (single instance).
package store

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/util"
)

// EventType 监控事件类型（与 Worker monitor.Event 对齐）。
type EventType string

// 事件类型常量。
const (
	EventPortDown        EventType = "port_down"
	EventHTTPUnhealthy   EventType = "http_unhealthy"
	EventLogMatch        EventType = "log_match"
	EventResourceOver    EventType = "resource_over"
	EventResourceRecover EventType = "resource_recovered"
	// EventContainerDown 容器失联/停止（非 Worker 事件，由 server 侧 invmonitor
	// 对 standalone-container 纳管对象做存活探测时产生）。
	EventContainerDown EventType = "container_down"
	// EventRecovered 通用恢复事件（非 Worker 事件，由 server 侧 invmonitor 在
	// 纳管对象探测从失败翻转为成功时产生），触发对应服务告警关闭。
	EventRecovered EventType = "recovered"
	// EventPatrolFailed 巡检异常转告警（非 Worker 事件，由 patrol syncAlerts 产生，
	// 告警 ID 用 AlertIDWithKey 按检查项细分）。
	EventPatrolFailed EventType = "patrol_failed"
)

// Level 事件级别。
type Level string

// 级别常量。
const (
	LevelInfo  Level = "info"
	LevelWarn  Level = "warn"
	LevelError Level = "error"
)

// IngestEvent 是经 gRPC SubscribeEvents 流收到的原始监控事件（subscriber 注入 cluster）。
type IngestEvent struct {
	ID      string    `json:"id"`
	Cluster string    `json:"cluster,omitempty"` // 订阅消费者按集群名注入
	TS      time.Time `json:"ts"`
	Service string    `json:"service"`
	Type    EventType `json:"type"`
	Level   Level     `json:"level"`
	Msg     string    `json:"msg"`
	Detail  string    `json:"detail,omitempty"`
}

// AlertStatus 告警生命周期状态。
type AlertStatus string

// 告警状态常量。
const (
	AlertActive    AlertStatus = "active"    // 存在未处理事件
	AlertAcked     AlertStatus = "acked"     // 人工认领
	AlertRecovered AlertStatus = "recovered" // 已恢复（事件驱动或人工）
)

// Alert 是按 (cluster, service, type) 聚合后的告警。
type Alert struct {
	ID        string      `json:"id"`
	Cluster   string      `json:"cluster"`
	Service   string      `json:"service"`
	Type      EventType   `json:"type"`
	Level     Level       `json:"level"`
	Title     string      `json:"title"`
	Status    AlertStatus `json:"status"`
	Count     int         `json:"count"`
	FirstTS   time.Time   `json:"first_ts"`
	LastTS    time.Time   `json:"last_ts"`
	AckedBy   string      `json:"acked_by,omitempty"`
	AckedAt   *time.Time  `json:"acked_at,omitempty"`
	RecoverAt *time.Time  `json:"recovered_at,omitempty"`
	// Investigations/LastInvestigationID 排查回写（对话式 troubleshoot 落库后
	// 关联）：告警列表展示「已排查」标记，形成告警→排查闭环。
	Investigations      int    `json:"investigations,omitempty"`
	LastInvestigationID string `json:"last_investigation_id,omitempty"`
	// NotifyCount/LastNotifyAt 通知回写：至少成功投递一个渠道后由 notify 标记，
	// 告警列表展示「已通知」。
	NotifyCount  int        `json:"notify_count,omitempty"`
	LastNotifyAt *time.Time `json:"last_notified_at,omitempty"`
	// LastEventID 最近一次归并事件的 id（订阅消费者幂等去重用，不对外返回）。
	LastEventID string `json:"-"`
}

func eventKey(seq uint64) string {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], seq)
	return BucketEvent + "/" + string(b[:])
}

func alertKey(id string) string { return BucketAlert + "/" + id }

// alertIdxKey 告警索引 key（倒序：大 ts 在前，便于"最新在前"遍历）。
func alertIdxKey(status AlertStatus, cluster string, ts time.Time, id string) string {
	// 用最大 uint64 - unixnano 实现降序；ts 为 0 时用 0。
	var v uint64
	if !ts.IsZero() {
		v = ^uint64(ts.UnixNano()) // 大时间 → 小 key
	}
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	return fmt.Sprintf("%s/%s/%s/%s/%s", BucketAlert+"/idx", status, cluster, string(b[:]), id)
}

// SaveEvent 追加一条原始事件，返回分配的事件 key 序（seq）。
func (s *Store) SaveEvent(e *IngestEvent) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	seq, err := s.nextSeqLocked("event")
	if err != nil {
		return 0, err
	}
	data, err := json.Marshal(e)
	if err != nil {
		return 0, fmt.Errorf("marshal event: %w", err)
	}
	if err := s.db.Put([]byte(eventKey(seq)), data, nil); err != nil {
		return 0, err
	}
	return seq, nil
}

// ListEvents 按事件序倒序（最新在前）读取服务端事件库，可按集群/服务/类型
// 过滤（空 = 不过滤）；limit <= 0 取默认 50。纳管对象的探测事件（invmonitor
// 生成）只落在这里、不过 Worker，深度排查注入事件用本查询。
func (s *Store) ListEvents(cluster, service string, typ EventType, limit int) ([]*IngestEvent, error) {
	if limit <= 0 {
		limit = 50
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]*IngestEvent, 0, limit)
	iter := s.db.NewIterator(util.BytesPrefix([]byte(BucketEvent+"/")), nil)
	defer iter.Release()
	for iter.Last(); iter.Valid(); iter.Prev() {
		var e IngestEvent
		if err := json.Unmarshal(iter.Value(), &e); err != nil {
			continue
		}
		if cluster != "" && e.Cluster != cluster {
			continue
		}
		if service != "" && e.Service != service {
			continue
		}
		if typ != "" && e.Type != typ {
			continue
		}
		ec := e
		out = append(out, &ec)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// nextSeqLocked 递增序列（调用方持锁）。
func (s *Store) nextSeqLocked(kind string) (uint64, error) {
	key := BucketSeq + "/" + kind
	raw, err := s.db.Get([]byte(key), nil)
	var cur uint64
	if err == nil {
		cur = binary.BigEndian.Uint64(raw)
	} else if err != leveldb.ErrNotFound {
		return 0, err
	}
	cur++
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], cur)
	if err := s.db.Put([]byte(key), b[:], nil); err != nil {
		return 0, err
	}
	return cur, nil
}

// PutAlert 原子写入告警 + 索引（调用方持锁）。
func (s *Store) putAlertLocked(a *Alert) error {
	data, err := json.Marshal(a)
	if err != nil {
		return fmt.Errorf("marshal alert: %w", err)
	}
	batch := new(leveldb.Batch)
	batch.Put([]byte(alertKey(a.ID)), data)
	// 索引：只索引 active/acked（recovered 保留最近一条供审计，不参与列表主路径可再加）
	// 为简单起见：全部状态都写索引，状态变化时删除旧索引。
	if err := s.writeAlertIndexLocked(batch, a); err != nil {
		return err
	}
	return s.db.Write(batch, nil)
}

func (s *Store) writeAlertIndexLocked(batch *leveldb.Batch, a *Alert) error {
	// 删除该告警在所有状态下的旧索引（状态可能变迁）
	for _, st := range []AlertStatus{AlertActive, AlertAcked, AlertRecovered} {
		// 旧索引无法精确枚举（ts 变化），直接按 id 前缀扫描删除
		prefix := fmt.Sprintf("%s/%s/%s/", BucketAlert+"/idx", st, a.Cluster)
		iter := s.db.NewIterator(util.BytesPrefix([]byte(prefix)), nil)
		var dels [][]byte
		for iter.Next() {
			key := append([]byte(nil), iter.Key()...)
			if strings.HasSuffix(string(key), "/"+a.ID) {
				dels = append(dels, key)
			}
		}
		iter.Release()
		for _, k := range dels {
			batch.Delete(k)
		}
	}
	batch.Put([]byte(alertIdxKey(a.Status, a.Cluster, a.LastTS, a.ID)), nil)
	return nil
}

// GetAlert 读取单个告警；不存在返回 (nil, nil)。
func (s *Store) GetAlert(id string) (*Alert, error) {
	raw, err := s.get(alertKey(id))
	if err == leveldb.ErrNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var a Alert
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("decode alert %q: %w", id, err)
	}
	return &a, nil
}

// ListAlerts 按状态+集群过滤列出告警，最新在前。status 为空则全部状态；
// cluster 非空时无论 status 是否为空都按集群过滤。
func (s *Store) ListAlerts(status AlertStatus, cluster string) ([]*Alert, error) {
	prefix := BucketAlert + "/idx"
	if status != "" {
		prefix += "/" + string(status)
		if cluster != "" {
			prefix += "/" + cluster
		}
	}
	var out []*Alert
	err := s.iterate(prefix, func(key string, _ []byte) error {
		// 提取 id（key 尾段）
		segs := strings.Split(key, "/")
		id := segs[len(segs)-1]
		if id == "" {
			return nil
		}
		a, err := s.GetAlert(id)
		if err != nil {
			return err
		}
		if a != nil {
			// status 为空 + cluster 过滤：这里无法用前缀表达，读取后过滤
			if cluster != "" && a.Cluster != cluster {
				return nil
			}
			out = append(out, a)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SaveEvent + upsert alert 的原子入口：ingest 服务用。
// UpsertAlert 不存在则创建，存在则累加 count/更新 last_ts/合并事件。
// 返回最终落库的告警与 created（新建或 recovered 复发重激活为 true，调用方
// 据此决定是否通知；合并计数时可通过返回告警的 Count 做里程碑判断）。
func (s *Store) UpsertAlert(a *Alert) (*Alert, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, err := s.get(alertKey(a.ID))
	if err != nil && err != leveldb.ErrNotFound {
		return nil, false, err
	}
	if err == leveldb.ErrNotFound || len(existing) == 0 {
		return a, true, s.putAlertLocked(a)
	}
	var old Alert
	if err := json.Unmarshal(existing, &old); err != nil {
		return nil, false, fmt.Errorf("decode existing alert: %w", err)
	}
	// 合并：保留旧状态，累加计数，更新时间与最近事件
	if old.Status == AlertActive || old.Status == AlertAcked {
		old.Count++
		if old.FirstTS.IsZero() {
			old.FirstTS = a.FirstTS
		}
		old.LastTS = a.LastTS
		if a.Level != "" {
			old.Level = a.Level
		}
		old.Title = a.Title
		old.LastEventID = a.LastEventID
		return &old, false, s.putAlertLocked(&old)
	}
	// recovered 的告警再收到新事件 → 重新激活（新 id）
	a.ID = newAlertID()
	a.Status = AlertActive
	a.Count = 1
	return a, true, s.putAlertLocked(a)
}

// UpsertKeyedAlert 以 a.ID 为确定键 upsert 告警（巡检按检查项维护生命周期用）。
// 与 UpsertAlert 的差异：recovered 后复发**原地重激活**（保留确定 ID，不换新 id），
// 便于调用方按固定 ID 做恢复/重激活配对。返回 created=true 表示新建或复发
// 重激活（调用方据此决定是否通知）。
func (s *Store) UpsertKeyedAlert(a *Alert) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, err := s.get(alertKey(a.ID))
	if err != nil && err != leveldb.ErrNotFound {
		return false, err
	}
	if err == leveldb.ErrNotFound || len(existing) == 0 {
		return true, s.putAlertLocked(a)
	}
	var old Alert
	if err := json.Unmarshal(existing, &old); err != nil {
		return false, fmt.Errorf("decode existing alert: %w", err)
	}
	if old.Status == AlertActive || old.Status == AlertAcked {
		old.Count++
		if old.FirstTS.IsZero() {
			old.FirstTS = a.FirstTS
		}
		old.LastTS = a.LastTS
		if a.Level != "" {
			old.Level = a.Level
		}
		old.Title = a.Title
		return false, s.putAlertLocked(&old)
	}
	// recovered 复发 → 原地重激活（保留确定 ID，清理 ack/recover 痕迹）
	old.Status = AlertActive
	old.Count = 1
	old.FirstTS = a.FirstTS
	old.LastTS = a.LastTS
	old.Title = a.Title
	if a.Level != "" {
		old.Level = a.Level
	}
	old.AckedBy = ""
	old.AckedAt = nil
	old.RecoverAt = nil
	return true, s.putAlertLocked(&old)
}

// MarkAlertInvestigated 回写告警的排查关联（次数+1、最近排查 id）。
// 告警不存在时静默成功（告警可能已被清理，不阻断排查落库）。
func (s *Store) MarkAlertInvestigated(id, invID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, err := s.GetAlert(id)
	if err != nil || a == nil {
		return err
	}
	a.Investigations++
	a.LastInvestigationID = invID
	return s.putAlertLocked(a)
}

// MarkAlertNotified 回写告警的通知标记（次数+1、最近通知时间）。
// 告警不存在时静默成功（告警可能已被清理，不阻断通知流程）。
func (s *Store) MarkAlertNotified(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, err := s.GetAlert(id)
	if err != nil || a == nil {
		return err
	}
	a.NotifyCount++
	now := time.Now().UTC()
	a.LastNotifyAt = &now
	return s.putAlertLocked(a)
}

// AlertIDWithKey 带附加标识的告警 ID（巡检按检查项细分告警，避免同
// (cluster, service, type) 的不同检查项聚成一条）。
func AlertIDWithKey(cluster, service string, typ EventType, key string) string {
	h := sha256.Sum256([]byte(cluster + "|" + service + "|" + string(typ) + "|" + key))
	return "al-" + hex.EncodeToString(h[:6])
}

// SetAlertStatus 原子更新告警状态（认领/恢复/重激活），维护索引。
func (s *Store) SetAlertStatus(id string, status AlertStatus, by string) (*Alert, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, err := s.GetAlert(id)
	if err != nil {
		return nil, err
	}
	if a == nil {
		return nil, nil
	}
	now := time.Now().UTC()
	a.Status = status
	switch status {
	case AlertAcked:
		a.AckedBy = by
		a.AckedAt = &now
	case AlertRecovered:
		a.RecoverAt = &now
	case AlertActive:
		// 重激活
		a.AckedBy = ""
		a.AckedAt = nil
		a.RecoverAt = nil
	}
	if err := s.putAlertLocked(a); err != nil {
		return nil, err
	}
	return a, nil
}

// AlertID 由 (cluster, service, type) 派生确定性的斜杠安全 ID。
// 复合键含 "/"，直接用作 key 会破坏 alert/<id> 与索引的段结构，故哈希之。
func AlertID(cluster, service string, typ EventType) string {
	h := sha256.Sum256([]byte(cluster + "|" + service + "|" + string(typ)))
	return "al-" + hex.EncodeToString(h[:6])
}

// newAlertID 生成随机告警 id（recovered 后重激活用）。
func newAlertID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("al-%d", time.Now().UnixNano())
	}
	return "al-" + hex.EncodeToString(b[:])
}
