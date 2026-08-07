// Notification channels, policies, send records (P6), plus alert rules
// (Worker monitoring-config management, 决策⑦) and users.
//
// Key layout (mirrors 设计方案 §七):
//
//	notify/channel/<id>          -> NotifyChannel{id,type,name,config,via_proxy,proxy_url,enabled}
//	notify/policy/<level>        -> NotifyPolicy{level,channel_ids,receivers,escalate}
//	notify/record/<seq>          -> NotifyRecord{id,ts,channel_id,alert_id,target,status,error}
//	alertrule/<cluster>/<service> -> AlertRule{cluster,service,monitoring,updated_at}
//	user/<id>                    -> User{id,username,role,...}
package store

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/syndtr/goleveldb/leveldb"
)

// --- 通知渠道 ---

// ChannelType 渠道类型（可配置不预设，内置 feishu/webhook 驱动）。
type ChannelType string

// 渠道类型常量。
const (
	ChannelFeishu  ChannelType = "feishu"
	ChannelSMS     ChannelType = "sms" // 预留：经代理
	ChannelWebhook ChannelType = "webhook"
)

// NotifyChannel 通知渠道定义。
type NotifyChannel struct {
	ID   string      `json:"id"`
	Type ChannelType `json:"type"`
	Name string      `json:"name"`
	// Config 渠道参数（feishu: webhook_url；webhook: url/headers；sms: 经代理）。
	// 走 via_proxy 时，Config 不含公网凭据（凭据在代理侧）。
	Config   map[string]any `json:"config"`
	ViaProxy bool           `json:"via_proxy"`
	// ProxyURL 互联网转发代理地址（内网可达，发送器把 payload POST 给代理）。
	ProxyURL  string    `json:"proxy_url,omitempty"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
}

// NotifyPolicy 通知策略：级别 → 渠道 + 接收人。
type NotifyPolicy struct {
	Level      string   `json:"level"` // info | warn | error
	ChannelIDs []string `json:"channel_ids"`
	Receivers  []string `json:"receivers,omitempty"` // 手机号/用户标识
	// Escalate 预留：未处理升级（P6 简化先不做时间升级）。
	Escalate *EscalateConfig `json:"escalate,omitempty"`
}

// EscalateConfig 升级配置（占位）。
type EscalateConfig struct {
	AfterMinutes int `json:"after_minutes,omitempty"`
}

// NotifyRecord 发送记录。
type NotifyRecord struct {
	ID        string    `json:"id"`
	Seq       uint64    `json:"seq"`
	TS        time.Time `json:"ts"`
	ChannelID string    `json:"channel_id"`
	AlertID   string    `json:"alert_id,omitempty"`
	Title     string    `json:"title,omitempty"`
	Target    string    `json:"target,omitempty"`
	Status    string    `json:"status"` // success | failed
	Error     string    `json:"error,omitempty"`
}

// --- 告警规则（Worker monitoring config 管理，决策⑦） ---

// AlertRule 某集群某纳管对象的监控配置（管理端持久化副本 + 下发源）。
// Service 字段语义为"纳管对象 name"——可以是 swarm service name，
// 也可以是 inventory item name（standalone-container / host-service）。
type AlertRule struct {
	Cluster    string     `json:"cluster"`
	Service    string     `json:"service"` // 纳管对象 name（swarm service 或 inventory item）
	Monitoring Monitoring `json:"monitoring"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

// Monitoring 对应 Worker config.Monitoring（JSON 契约一致）。
type Monitoring struct {
	Enabled            bool                `json:"enabled,omitempty"`
	PortChecks         []PortCheck         `json:"portChecks,omitempty"`
	HTTPChecks         []HTTPCheck         `json:"httpChecks,omitempty"`
	LogChecks          []LogCheck          `json:"logChecks,omitempty"`
	ResourceThresholds []ResourceThreshold `json:"resourceThresholds,omitempty"`
}

// PortCheck TCP 端口探测。
type PortCheck struct {
	Port     string `json:"port"`
	Protocol string `json:"protocol,omitempty"`
	Interval string `json:"interval,omitempty"`
	Timeout  string `json:"timeout,omitempty"`
	Retries  int    `json:"retries,omitempty"`
}

// HTTPCheck HTTP 探活。
type HTTPCheck struct {
	URL            string            `json:"url"`
	Method         string            `json:"method,omitempty"`
	Headers        map[string]string `json:"headers,omitempty"`
	ExpectedStatus []int             `json:"expectedStatus,omitempty"`
	ExpectedBody   string            `json:"expectedBody,omitempty"`
	Interval       string            `json:"interval,omitempty"`
	Timeout        string            `json:"timeout,omitempty"`
}

// LogCheck 日志模式匹配。
type LogCheck struct {
	Pattern string   `json:"pattern"`
	Level   string   `json:"level,omitempty"`
	Ignore  []string `json:"ignore,omitempty"`
	Action  string   `json:"action,omitempty"`
}

// ResourceThreshold 资源阈值。
type ResourceThreshold struct {
	Metric    string `json:"metric"` // cpu | memory
	Threshold int    `json:"threshold"`
	Action    string `json:"action,omitempty"`
}

// --- 用户 ---

// User 平台用户（本地 fallback；SSO 用户经 OIDC 同步）。
type User struct {
	ID        string    `json:"id"`
	Username  string    `json:"username"`
	Password  string    `json:"password_hash,omitempty"` // 加盐哈希，落盘持久化；对外经 Public() 抹除
	Role      string    `json:"role"`                    // admin | viewer
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	// IdP 扩展字段（OIDC profile claims 下发用）。omitempty 保证旧记录反序列化无影响。
	Email       string    `json:"email,omitempty"`
	DisplayName string    `json:"display_name,omitempty"`
	Groups      []string  `json:"groups,omitempty"`
	UpdatedAt   time.Time `json:"updated_at,omitempty"`
}

// GetUserID 返回稳定主体标识，IdP 下发 sub 时优先用 ID（username 可能改名）。
func (u *User) GetUserID() string {
	if u == nil {
		return ""
	}
	if u.ID != "" {
		return u.ID
	}
	return u.Username
}

// Public 返回去除敏感字段（密码）的对外视图。
func (u *User) Public() *User {
	out := *u
	out.Password = ""
	return &out
}

// --- key helpers ---

func notifyChannelKey(id string) string { return "notify/channel/" + id }

func notifyPolicyKey(level string) string { return "notify/policy/" + level }

func notifyRecordKey(seq uint64) string { return fmt.Sprintf("notify/record/%020d", seq) }

func alertRuleKey(cluster, service string) string { return "alertrule/" + cluster + "/" + service }

func userKey(id string) string { return "user/" + id }

// --- NotifyChannel ---

// PutChannel 写入渠道。
func (s *Store) PutChannel(c *NotifyChannel) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("marshal channel: %w", err)
	}
	return s.db.Put([]byte(notifyChannelKey(c.ID)), data, nil)
}

// GetChannel 读取渠道；不存在返回 (nil, nil)。
func (s *Store) GetChannel(id string) (*NotifyChannel, error) {
	raw, err := s.get(notifyChannelKey(id))
	if err == leveldb.ErrNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var c NotifyChannel
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("decode channel %q: %w", id, err)
	}
	return &c, nil
}

// DeleteChannel 删除渠道。
func (s *Store) DeleteChannel(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Delete([]byte(notifyChannelKey(id)), nil)
}

// ListChannels 列出全部渠道。
func (s *Store) ListChannels() ([]*NotifyChannel, error) {
	var out []*NotifyChannel
	err := s.iterate("notify/channel/", func(_ string, value []byte) error {
		var c NotifyChannel
		if err := json.Unmarshal(value, &c); err != nil {
			return fmt.Errorf("decode channel: %w", err)
		}
		out = append(out, &c)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// --- NotifyPolicy ---

// PutPolicy 写入策略（按级别覆盖）。
func (s *Store) PutPolicy(p *NotifyPolicy) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("marshal policy: %w", err)
	}
	return s.db.Put([]byte(notifyPolicyKey(p.Level)), data, nil)
}

// GetPolicy 读取策略；不存在返回 (nil, nil)。
func (s *Store) GetPolicy(level string) (*NotifyPolicy, error) {
	raw, err := s.get(notifyPolicyKey(level))
	if err == leveldb.ErrNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p NotifyPolicy
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("decode policy: %w", err)
	}
	return &p, nil
}

// ListPolicies 列出全部策略。
func (s *Store) ListPolicies() ([]*NotifyPolicy, error) {
	var out []*NotifyPolicy
	err := s.iterate("notify/policy/", func(_ string, value []byte) error {
		var p NotifyPolicy
		if err := json.Unmarshal(value, &p); err != nil {
			return fmt.Errorf("decode policy: %w", err)
		}
		out = append(out, &p)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// --- NotifyRecord ---

// SaveNotifyRecord 写入发送记录。
func (s *Store) SaveNotifyRecord(r *NotifyRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("marshal record: %w", err)
	}
	return s.db.Put([]byte(notifyRecordKey(r.Seq)), data, nil)
}

// ListNotifyRecords 列出发送记录（最新在前）。
func (s *Store) ListNotifyRecords(limit int) ([]*NotifyRecord, error) {
	var out []*NotifyRecord
	err := s.iterate("notify/record/", func(_ string, value []byte) error {
		var r NotifyRecord
		if err := json.Unmarshal(value, &r); err != nil {
			return fmt.Errorf("decode record: %w", err)
		}
		out = append(out, &r)
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

// --- AlertRule ---

// PutAlertRule 写入告警规则。
func (s *Store) PutAlertRule(r *AlertRule) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("marshal alert rule: %w", err)
	}
	return s.db.Put([]byte(alertRuleKey(r.Cluster, r.Service)), data, nil)
}

// GetAlertRule 读取规则；不存在返回 (nil, nil)。
func (s *Store) GetAlertRule(cluster, service string) (*AlertRule, error) {
	raw, err := s.get(alertRuleKey(cluster, service))
	if err == leveldb.ErrNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r AlertRule
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("decode alert rule: %w", err)
	}
	return &r, nil
}

// DeleteAlertRule 删除规则。
func (s *Store) DeleteAlertRule(cluster, service string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Delete([]byte(alertRuleKey(cluster, service)), nil)
}

// ListAlertRules 列出全部规则。
func (s *Store) ListAlertRules() ([]*AlertRule, error) {
	var out []*AlertRule
	err := s.iterate("alertrule/", func(_ string, value []byte) error {
		var r AlertRule
		if err := json.Unmarshal(value, &r); err != nil {
			return fmt.Errorf("decode alert rule: %w", err)
		}
		out = append(out, &r)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// --- User ---

// PutUser 写入用户。
func (s *Store) PutUser(u *User) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := json.Marshal(u)
	if err != nil {
		return fmt.Errorf("marshal user: %w", err)
	}
	return s.db.Put([]byte(userKey(u.ID)), data, nil)
}

// GetUserByUsername 按用户名查用户（登录用，返回含 Password）。
func (s *Store) GetUserByUsername(username string) (*User, error) {
	users, err := s.ListUsers()
	if err != nil {
		return nil, err
	}
	for _, u := range users {
		if u.Username == username {
			return u, nil
		}
	}
	return nil, nil
}

// ListUsers 列出全部用户。
func (s *Store) ListUsers() ([]*User, error) {
	var out []*User
	err := s.iterate("user/", func(_ string, value []byte) error {
		var u User
		if err := json.Unmarshal(value, &u); err != nil {
			return fmt.Errorf("decode user: %w", err)
		}
		out = append(out, &u)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
