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
		// 飞书附 interactive 卡片（对齐群内告警模板）；content 保留为纯文本
		// 兜底——旧版代理忽略 card 字段时退回文本样式，行为不变。
		if ch.Type == store.ChannelFeishu {
			payload["card"] = s.alertCard(alert, content)
		}
		body, _ := json.Marshal(payload)
		return s.postJSON(ctx, ch.ProxyURL, body)
	}
	switch ch.Type {
	case store.ChannelFeishu:
		url, _ := ch.Config["webhook_url"].(string)
		body, _ := json.Marshal(map[string]any{
			"msg_type": "interactive",
			"card":     s.alertCard(alert, content),
		})
		return s.postJSON(ctx, url, body)
	case store.ChannelWebhook:
		url, _ := ch.Config["url"].(string)
		body, _ := json.Marshal(map[string]any{"title": content, "alert": alert})
		return s.postJSON(ctx, url, body)
	default:
		return ErrInvalid{"unsupported channel type " + string(ch.Type)}
	}
}

// --- 飞书告警卡片 ---

// alertCard 构造告警通知的飞书 interactive 卡片，样式对齐群内既有模板：
// 红色头「告警 - X」/ 绿色头「恢复 - X」，双列字段（告警/级别/对象/系统）
// + 摘要/详情 + 分隔线 + 时间。
func (s *Service) alertCard(alert *store.Alert, subject string) map[string]any {
	kind, template := "告警", "red"
	switch alert.Status {
	case store.AlertRecovered:
		kind, template = "恢复", "green"
	case store.AlertAcked:
		kind, template = "认领", "orange"
	}
	name := eventTypeLabel(alert.Type)
	md := func(format string, args ...any) map[string]any {
		return map[string]any{"tag": "lark_md", "content": fmt.Sprintf(format, args...)}
	}
	short := func(text map[string]any) map[string]any {
		return map[string]any{"is_short": true, "text": text}
	}
	elements := []any{
		map[string]any{"tag": "div", "fields": []any{
			short(md("**告警:** %s", name)),
			short(md("**级别:** %s", levelLabel(alert.Level))),
			short(md("**对象:** %s", alertTarget(alert))),
			short(md("**系统:** %s", s.systemName(alert.Cluster))),
		}},
		map[string]any{"tag": "div", "text": md("**摘要:** %s", alertSummary(alert))},
		map[string]any{"tag": "div", "text": md("**详情:** %s", alertDetail(alert, subject))},
		map[string]any{"tag": "hr"},
		map[string]any{"tag": "div", "text": md("**时间:** %s", alert.LastTS.Local().Format("2006-01-02 15:04:05"))},
	}
	return map[string]any{
		"config": map[string]any{"wide_screen_mode": true},
		"header": map[string]any{
			"title":    map[string]any{"tag": "plain_text", "content": kind + " - " + name},
			"template": template,
		},
		"elements": elements,
	}
}

// eventTypeLabel 事件类型中文名（兜底展示原值）。
func eventTypeLabel(t store.EventType) string {
	switch t {
	case store.EventPortDown:
		return "端口不可达"
	case store.EventHTTPUnhealthy:
		return "HTTP 健康检查失败"
	case store.EventLogMatch:
		return "日志异常匹配"
	case store.EventResourceOver:
		return "资源超限"
	case store.EventContainerDown:
		return "容器不可用"
	case store.EventPatrolFailed:
		return "巡检异常"
	case store.EventResourceRecover, store.EventRecovered:
		return "恢复"
	default:
		return string(t)
	}
}

// levelLabel 告警级别展示（彩色圆点对齐群模板「🔴 紧急」）。
func levelLabel(l store.Level) string {
	switch l {
	case store.LevelError:
		return "🔴 紧急"
	case store.LevelWarn:
		return "🟠 重要"
	default:
		return "🔵 提示"
	}
}

// alertTarget 告警对象：cluster 或 cluster/service。
func alertTarget(a *store.Alert) string {
	if a.Service == "" {
		return a.Cluster
	}
	return a.Cluster + "/" + a.Service
}

// systemName 解析集群所属项目名（卡片「系统」字段）；未归属或项目缺失时回退集群名。
func (s *Service) systemName(cluster string) string {
	if c, err := s.st.GetCluster(cluster); err == nil && c != nil && c.ProjectID != "" {
		if p, err := s.st.GetProject(c.ProjectID); err == nil && p != nil && p.Name != "" {
			return p.Name
		}
	}
	return cluster
}

// stripBracketPrefixes 去掉消息开头连续的「[xxx] 」前缀。
func stripBracketPrefixes(s string) string {
	for strings.HasPrefix(s, "[") {
		end := strings.Index(s, "]")
		if end < 0 {
			return s
		}
		s = strings.TrimLeft(s[end+1:], " ")
	}
	return s
}

// stripTargetRefs 去掉消息中的对象标记（"[cluster]"、"[cluster/service]"）——
// 对象信息已由卡片「对象」字段单独展示，正文不再重复；随后再剥离开头剩余的
// 连续 "[xxx] " 前缀（兼容其他来源的标记）。
func stripTargetRefs(a *store.Alert, s string) string {
	// 先替换「标记+后随空格」再替换裸标记，避免残留孤立空格
	for _, tok := range []string{
		"[" + a.Cluster + "/" + a.Service + "] ",
		"[" + a.Cluster + "] ",
		"[" + a.Cluster + "/" + a.Service + "]",
		"[" + a.Cluster + "]",
	} {
		s = strings.ReplaceAll(s, tok, "")
	}
	// 兼容其他来源的前缀标记；subject 均为单行文案，空白收敛安全
	return strings.TrimLeft(stripBracketPrefixes(collapseSpaces(s)), " ")
}

// collapseSpaces 连续空白收敛为单空格。
func collapseSpaces(s string) string { return strings.Join(strings.Fields(s), " ") }

// alertSummary 摘要：告警标题去掉对象标记（如「资源超限：cpu usage 98.6% >= 85%」）。
func alertSummary(a *store.Alert) string { return stripTargetRefs(a, a.Title) }

// alertDetail 详情：通知原文去掉对象标记。新建告警时原文与标题相同，补充处理
// 建议对齐群模板「…，请检查服务状态」；恢复/累计等场景保留原文语境。
func alertDetail(a *store.Alert, subject string) string {
	d := stripTargetRefs(a, subject)
	if d == alertSummary(a) {
		d += "，请检查服务状态"
	}
	return d
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
