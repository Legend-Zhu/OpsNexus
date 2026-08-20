// Package notify implements notification channels, policies and sending
// (P6). Channels are configurable, not preset; feishu/webhook drivers are
// built in. Channels that need public internet (feishu/sms) go through the
// configured internet forward-proxy (via_proxy) — the proxy holds the public
// credentials, they never enter the intranet store. Every send is recorded.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
)

// ErrNotFound 渠道/策略不存在。
type ErrNotFound struct{ Kind, ID string }

func (e ErrNotFound) Error() string { return fmt.Sprintf("%s %q not found", e.Kind, e.ID) }

// ErrInvalid 配置非法。
type ErrInvalid struct{ Msg string }

func (e ErrInvalid) Error() string { return "invalid notify config: " + e.Msg }

// Service 通知服务。
type Service struct {
	st *store.Store
	// client 复用（发送用，超时短）。
	client *http.Client
}

// New 创建通知服务。
func New(st *store.Store) *Service {
	return &Service{st: st, client: &http.Client{Timeout: 10 * time.Second}}
}

// --- 渠道 CRUD ---

// CreateChannel 创建渠道。
func (s *Service) CreateChannel(typ store.ChannelType, name string, config map[string]any, viaProxy bool, proxyURL string, enabled bool) (*store.NotifyChannel, error) {
	if name == "" {
		return nil, ErrInvalid{"name is required"}
	}
	switch typ {
	case store.ChannelFeishu, store.ChannelWebhook, store.ChannelSMS:
	default:
		return nil, ErrInvalid{"type must be feishu|sms|webhook"}
	}
	if err := validateChannelConfig(typ, config, viaProxy, proxyURL); err != nil {
		return nil, err
	}
	c := &store.NotifyChannel{
		ID:        fmt.Sprintf("ch-%d", time.Now().UnixNano()),
		Type:      typ,
		Name:      name,
		Config:    config,
		ViaProxy:  viaProxy,
		ProxyURL:  proxyURL,
		Enabled:   enabled,
		CreatedAt: time.Now().UTC(),
	}
	if err := s.st.PutChannel(c); err != nil {
		return nil, err
	}
	return c, nil
}

// UpdateChannel 更新渠道（enabled/name/config/proxy）。
func (s *Service) UpdateChannel(id string, name string, config map[string]any, viaProxy bool, proxyURL string, enabled bool) (*store.NotifyChannel, error) {
	c, err := s.st.GetChannel(id)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, ErrNotFound{"channel", id}
	}
	if name != "" {
		c.Name = name
	}
	if config != nil {
		c.Config = config
	}
	if err := validateChannelConfig(c.Type, c.Config, viaProxy, proxyURL); err != nil {
		return nil, err
	}
	c.ViaProxy = viaProxy
	c.ProxyURL = proxyURL
	c.Enabled = enabled
	if err := s.st.PutChannel(c); err != nil {
		return nil, err
	}
	return c, nil
}

// DeleteChannel 删除渠道。
func (s *Service) DeleteChannel(id string) error {
	c, err := s.st.GetChannel(id)
	if err != nil {
		return err
	}
	if c == nil {
		return ErrNotFound{"channel", id}
	}
	return s.st.DeleteChannel(id)
}

// ListChannels 列出渠道。
func (s *Service) ListChannels() ([]*store.NotifyChannel, error) { return s.st.ListChannels() }

func validateChannelConfig(typ store.ChannelType, config map[string]any, viaProxy bool, proxyURL string) error {
	switch typ {
	case store.ChannelFeishu:
		if !viaProxy {
			// 直连飞书：必须给 webhook_url（内网可达场景）
			if s, _ := config["webhook_url"].(string); s == "" {
				return ErrInvalid{"feishu channel needs webhook_url (or via_proxy)"}
			}
		}
	case store.ChannelWebhook:
		if s, _ := config["url"].(string); s == "" {
			return ErrInvalid{"webhook channel needs url"}
		}
	case store.ChannelSMS:
		if !viaProxy {
			return ErrInvalid{"sms channel must use via_proxy (intranet deployment)"}
		}
	}
	if viaProxy && strings.TrimSpace(proxyURL) == "" {
		return ErrInvalid{"via_proxy requires proxy_url"}
	}
	return nil
}

// --- 策略 CRUD ---

// UpsertPolicy 写入策略（按级别覆盖）。
func (s *Service) UpsertPolicy(p *store.NotifyPolicy) error {
	if p.Level == "" {
		return ErrInvalid{"level is required"}
	}
	// 校验渠道存在
	for _, id := range p.ChannelIDs {
		c, err := s.st.GetChannel(id)
		if err != nil {
			return err
		}
		if c == nil {
			return ErrNotFound{"channel", id}
		}
	}
	return s.st.PutPolicy(p)
}

// DeletePolicy 删除策略。
func (s *Service) DeletePolicy(level string) error {
	p, err := s.st.GetPolicy(level)
	if err != nil {
		return err
	}
	if p == nil {
		return ErrNotFound{"policy", level}
	}
	// 删除 = 写空策略
	return s.st.PutPolicy(&store.NotifyPolicy{Level: level})
}

// ListPolicies 列出策略。
func (s *Service) ListPolicies() ([]*store.NotifyPolicy, error) { return s.st.ListPolicies() }

// Records 列出发送记录（最新在前）。
func (s *Service) Records(limit int) ([]*store.NotifyRecord, error) {
	return s.st.ListNotifyRecords(limit)
}

// --- 发送 ---

// NotifyAlert 按告警级别匹配策略，逐渠道发送并记录。
// 无匹配策略 = 静默（不报错）。至少一个渠道投递成功时回写告警的
// 「已通知」标记（NotifyCount/LastNotifyAt，前端列表据此展示）。
func (s *Service) NotifyAlert(ctx context.Context, alert *store.Alert, subject string) error {
	policy, err := s.st.GetPolicy(string(alert.Level))
	if err != nil {
		return err
	}
	if policy == nil || len(policy.ChannelIDs) == 0 {
		return nil
	}
	sent := 0
	for _, cid := range policy.ChannelIDs {
		ch, err := s.st.GetChannel(cid)
		if err != nil {
			continue
		}
		if ch == nil || !ch.Enabled {
			continue
		}
		if s.sendAndRecord(ctx, ch, alert, subject) == nil {
			sent++
		}
	}
	if sent > 0 {
		_ = s.st.MarkAlertNotified(alert.ID)
	}
	return nil
}

// sendAndRecord 发送并记录结果，返回发送错误（nil 表示投递成功）。
func (s *Service) sendAndRecord(ctx context.Context, ch *store.NotifyChannel, alert *store.Alert, subject string) error {
	rec := &store.NotifyRecord{
		ID:        fmt.Sprintf("nr-%d", time.Now().UnixNano()),
		TS:        time.Now().UTC(),
		ChannelID: ch.ID,
		AlertID:   alert.ID,
		Title:     subject,
		Target:    ch.Name,
		Status:    "success",
	}
	sendErr := s.send(ctx, ch, subject, alert)
	if sendErr != nil {
		rec.Status = "failed"
		rec.Error = sendErr.Error()
	}
	seq, err := s.st.NextSeq("notify")
	if err != nil {
		return sendErr
	}
	rec.Seq = seq
	_ = s.st.SaveNotifyRecord(rec)
	return sendErr
}

// Send 向指定渠道发送任意内容（巡检报告等非告警场景）。
// 逐渠道发送并记录；不存在/禁用的渠道跳过；单渠道失败不阻断其余渠道，
// 返回首个错误（逐渠道明细见发送记录）。
func (s *Service) Send(ctx context.Context, channelIDs []string, title, content string) error {
	var firstErr error
	for _, cid := range channelIDs {
		ch, err := s.st.GetChannel(cid)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if ch == nil || !ch.Enabled {
			continue
		}
		if err := s.sendAndRecordGeneric(ctx, ch, title, content); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// sendAndRecordGeneric 通用内容发送并记录（alert_id 留空），返回发送错误。
func (s *Service) sendAndRecordGeneric(ctx context.Context, ch *store.NotifyChannel, title, content string) error {
	rec := &store.NotifyRecord{
		ID:        fmt.Sprintf("nr-%d", time.Now().UnixNano()),
		TS:        time.Now().UTC(),
		ChannelID: ch.ID,
		Title:     title,
		Target:    ch.Name,
		Status:    "success",
	}
	err := s.sendGeneric(ctx, ch, title, content)
	if err != nil {
		rec.Status = "failed"
		rec.Error = err.Error()
	}
	seq, serr := s.st.NextSeq("notify")
	if serr == nil {
		rec.Seq = seq
		_ = s.st.SaveNotifyRecord(rec)
	}
	return err
}

// sendGeneric 按渠道类型发送通用内容（与告警负载区分开：webhook 为
// {title, content} 而非内嵌告警对象；文本类渠道为「标题+正文」纯文本）。
func (s *Service) sendGeneric(ctx context.Context, ch *store.NotifyChannel, title, content string) error {
	text := title + "\n\n" + content
	if ch.ViaProxy {
		payload := map[string]any{
			"channel_type": ch.Type,
			"config":       ch.Config,
			"content":      text,
		}
		body, _ := json.Marshal(payload)
		return s.postJSON(ctx, ch.ProxyURL, body)
	}
	switch ch.Type {
	case store.ChannelFeishu:
		url, _ := ch.Config["webhook_url"].(string)
		body, _ := json.Marshal(map[string]any{"msg_type": "text", "content": map[string]any{"text": text}})
		return s.postJSON(ctx, url, body)
	case store.ChannelWebhook:
		url, _ := ch.Config["url"].(string)
		body, _ := json.Marshal(map[string]any{"title": title, "content": content})
		return s.postJSON(ctx, url, body)
	default:
		return ErrInvalid{"unsupported channel type " + string(ch.Type)}
	}
}

// send 按渠道类型发送。
func (s *Service) send(ctx context.Context, ch *store.NotifyChannel, content string, alert *store.Alert) error {
	// 互联网代理转发：内网管理端不直连公网，payload POST 给代理（代理带自身
	// 凭据调公网 API，公网凭据不入内网）。
	if ch.ViaProxy {
		payload := map[string]any{
			"channel_type": ch.Type,
			"config":       ch.Config, // 无公网凭据（如短信签名在代理侧）
			"content":      content,
		}
		body, _ := json.Marshal(payload)
		return s.postJSON(ctx, ch.ProxyURL, body)
	}
	switch ch.Type {
	case store.ChannelFeishu:
		url, _ := ch.Config["webhook_url"].(string)
		body, _ := json.Marshal(map[string]any{"msg_type": "text", "content": map[string]any{"text": content}})
		return s.postJSON(ctx, url, body)
	case store.ChannelWebhook:
		url, _ := ch.Config["url"].(string)
		body, _ := json.Marshal(map[string]any{"title": content, "alert": alert})
		return s.postJSON(ctx, url, body)
	default:
		return ErrInvalid{"unsupported channel type " + string(ch.Type)}
	}
}

func (s *Service) postJSON(ctx context.Context, url string, body []byte) error {
	if url == "" {
		return ErrInvalid{"empty target url"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("target returned %d", resp.StatusCode)
	}
	return nil
}
