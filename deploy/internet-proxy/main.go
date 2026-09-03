// internet-proxy 部署在互联网服务器（如 10.50.182.57）上，为内网 OpsGaurd
// 管理端（经网闸 TCP 映射进入）提供两类受控出网能力：
//
//  1. LLM 反向代理（LISTEN_LLM，默认 :7070，网闸任务 1731 → 50376）
//     任意路径原样转发到 LLM_UPSTREAM（OpenAI 兼容网关），Authorization 头
//     透传，SSE 流式响应逐块 flush。管理端「模型接入」base_url 填
//     http://10.60.114.2:50376/v1 即可（程序自动拼 /chat/completions）。
//
//  2. 飞书通知代理（LISTEN_NOTIFY，默认 :7072，网闸任务 1733 → 50378）
//     POST /notify  群机器人 webhook 代发（自动加签），绑所有级别
//     POST /urgent  飞书应用发群消息 + 对值班用户应用内加急，绑 error 级
//     GET  /healthz
//     请求体为 OpsGaurd notify 的 via_proxy 协议：
//     {"channel_type":"feishu","config":{...},"content":"文本"}；可选
//     "card":{...} 交互卡片对象——非空时以 msg_type=interactive 发送
//     （webhook 直发卡片对象；应用消息 content 传卡片 JSON 字符串）；
//     可选 "post":{...} 富文本对象（zh_cn 结构，巡检报告）——以
//     msg_type=post 发送（webhook 包在 content.post 下；应用消息 content
//     传 zh_cn JSON 字符串）。card 优先于 post；均缺省时保持纯文本，
//     旧版管理端不受影响。
//     公网凭据（webhook、加签密钥、应用 secret）全部持有在本服务侧，不进内网。
//
// 自测：internet-proxy -selftest（机器人 + 应用各发一条群消息，不加急）；
// internet-proxy -selftest-urgent（额外走加急链路，会 buzz 值班用户，慎用）。
package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const feishuBase = "https://open.feishu.cn"

// --- 配置 ---

type config struct {
	ListenLLM    string
	ListenNotify string
	LLMUpstream  string
	LLMInsecure  bool

	FeishuWebhook       string
	FeishuWebhookSecret string
	FeishuAppID         string
	FeishuAppSecret     string
	FeishuChatID        string
	FeishuUrgentUsers   []string
}

func envStr(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func envBool(k string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(k))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func loadConfig() *config {
	c := &config{
		ListenLLM:    envStr("LISTEN_LLM", ":7070"),
		ListenNotify: envStr("LISTEN_NOTIFY", ":7072"),
		LLMUpstream:  strings.TrimRight(envStr("LLM_UPSTREAM", ""), "/"),
		LLMInsecure:  envBool("LLM_INSECURE"),
	}
	if s := envStr("FEISHU_URGENT_USER_IDS", ""); s != "" {
		for _, id := range strings.Split(s, ",") {
			if id = strings.TrimSpace(id); id != "" {
				c.FeishuUrgentUsers = append(c.FeishuUrgentUsers, id)
			}
		}
	}
	c.FeishuWebhook = envStr("FEISHU_WEBHOOK", "")
	c.FeishuWebhookSecret = envStr("FEISHU_WEBHOOK_SECRET", "")
	c.FeishuAppID = envStr("FEISHU_APP_ID", "")
	c.FeishuAppSecret = envStr("FEISHU_APP_SECRET", "")
	c.FeishuChatID = envStr("FEISHU_CHAT_ID", "")
	return c
}

// --- 飞书客户端 ---

type feishuClient struct {
	webhook       string
	webhookSecret string
	appID         string
	appSecret     string
	chatID        string
	urgentUsers   []string
	http          *http.Client

	mu       sync.Mutex
	token    string
	tokenExp time.Time
}

func newFeishu(c *config) *feishuClient {
	return &feishuClient{
		webhook:       c.FeishuWebhook,
		webhookSecret: c.FeishuWebhookSecret,
		appID:         c.FeishuAppID,
		appSecret:     c.FeishuAppSecret,
		chatID:        c.FeishuChatID,
		urgentUsers:   c.FeishuUrgentUsers,
		http:          &http.Client{Timeout: 8 * time.Second},
	}
}

// sendWebhook 经群机器人自定义 webhook 发消息（安全设置为加签时自动签名）。
// 飞书加签算法：key = timestamp + "\n" + secret，消息体为空，HMAC-SHA256 后
// base64，timestamp 与 sign 作为 query 参数传递。
// card 非空发 interactive 卡片，post 非空发 post 富文本，否则发纯文本。
func (f *feishuClient) sendWebhook(ctx context.Context, text string, card, post map[string]any) error {
	if f.webhook == "" {
		return errors.New("FEISHU_WEBHOOK 未配置")
	}
	q := url.Values{}
	if f.webhookSecret != "" {
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		mac := hmac.New(sha256.New, []byte(ts+"\n"+f.webhookSecret))
		q.Set("timestamp", ts)
		q.Set("sign", base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	}
	var payload any = map[string]any{
		"msg_type": "text",
		"content":  map[string]any{"text": text},
	}
	switch {
	case card != nil:
		payload = map[string]any{"msg_type": "interactive", "card": card}
	case post != nil:
		payload = map[string]any{"msg_type": "post", "content": map[string]any{"post": post}}
	}
	body, _ := json.Marshal(payload)
	ep := f.webhook
	if len(q) > 0 {
		ep += "?" + q.Encode()
	}
	var out struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := f.postJSON(ctx, ep, body, "", &out); err != nil {
		return err
	}
	if out.Code != 0 {
		return fmt.Errorf("webhook 返回 code=%d msg=%s", out.Code, out.Msg)
	}
	return nil
}

// tenantToken 取（并缓存）应用 tenant_access_token，过期前 5 分钟刷新。
func (f *feishuClient) tenantToken(ctx context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.token != "" && time.Now().Before(f.tokenExp) {
		return f.token, nil
	}
	body, _ := json.Marshal(map[string]string{
		"app_id":     f.appID,
		"app_secret": f.appSecret,
	})
	var out struct {
		Code   int    `json:"code"`
		Msg    string `json:"msg"`
		Token  string `json:"tenant_access_token"`
		Expire int    `json:"expire"`
	}
	if err := f.postJSON(ctx, feishuBase+"/open-apis/auth/v3/tenant_access_token/internal", body, "", &out); err != nil {
		return "", err
	}
	if out.Code != 0 || out.Token == "" {
		return "", fmt.Errorf("tenant_access_token code=%d msg=%s", out.Code, out.Msg)
	}
	ttl := time.Duration(out.Expire)*time.Second - 5*time.Minute
	if ttl < time.Minute {
		ttl = time.Minute
	}
	f.token, f.tokenExp = out.Token, time.Now().Add(ttl)
	return f.token, nil
}

// sendAppMessage 以应用身份发群消息，返回 message_id（加急作用其上）。
// card 非空发 interactive 卡片，post 非空发 post 富文本——im API 的
// content 均为对应消息体 JSON 字符串（card 即卡片对象，post 即 zh_cn 对象）。
func (f *feishuClient) sendAppMessage(ctx context.Context, text string, card, post map[string]any) (string, error) {
	if f.appID == "" || f.chatID == "" {
		return "", errors.New("FEISHU_APP_ID / FEISHU_CHAT_ID 未配置")
	}
	tok, err := f.tenantToken(ctx)
	if err != nil {
		return "", err
	}
	msgType := "text"
	var contentObj any = map[string]any{"text": text}
	switch {
	case card != nil:
		msgType = "interactive"
		contentObj = card
	case post != nil:
		msgType = "post"
		contentObj = post
	}
	contentJSON, _ := json.Marshal(contentObj)
	body, _ := json.Marshal(map[string]any{
		"receive_id": f.chatID,
		"msg_type":   msgType,
		"content":    string(contentJSON),
	})
	var out struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			MessageID string `json:"message_id"`
		} `json:"data"`
	}
	ep := feishuBase + "/open-apis/im/v1/messages?receive_id_type=chat_id"
	if err := f.postJSON(ctx, ep, body, tok, &out); err != nil {
		return "", err
	}
	if out.Code != 0 || out.Data.MessageID == "" {
		return "", fmt.Errorf("im/v1/messages code=%d msg=%s", out.Code, out.Msg)
	}
	return out.Data.MessageID, nil
}

// urgentApp 对已发消息做应用内加急（buzz）。单次最多 5 个用户，超出分批。
func (f *feishuClient) urgentApp(ctx context.Context, messageID string, users []string) error {
	if len(users) == 0 {
		slog.Warn("urgent: FEISHU_URGENT_USER_IDS 为空，仅发群消息不加急")
		return nil
	}
	tok, err := f.tenantToken(ctx)
	if err != nil {
		return err
	}
	for i := 0; i < len(users); i += 5 {
		end := i + 5
		if end > len(users) {
			end = len(users)
		}
		body, _ := json.Marshal(map[string]any{"user_id_list": users[i:end]})
		var out struct {
			Code int    `json:"code"`
			Msg  string `json:"msg"`
		}
		// 加急是 PATCH（POST 返回 404 page not found），见飞书 im-v1/message/urgent_app。
		ep := feishuBase + "/open-apis/im/v1/messages/" + messageID + "/urgent_app?user_id_type=user_id"
		if err := f.doJSON(ctx, http.MethodPatch, ep, body, tok, &out); err != nil {
			return err
		}
		if out.Code != 0 {
			return fmt.Errorf("urgent_app code=%d msg=%s（检查 user_id 是否为 user_id 类型）", out.Code, out.Msg)
		}
	}
	return nil
}

func (f *feishuClient) postJSON(ctx context.Context, ep string, body []byte, bearer string, out any) error {
	return f.doJSON(ctx, http.MethodPost, ep, body, bearer, out)
}

func (f *feishuClient) doJSON(ctx context.Context, method, ep string, body []byte, bearer string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, ep, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := f.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// --- HTTP 服务 ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// notifyHandler 处理 OpsGaurd via_proxy 协议。urgent=true 走应用 + 加急链路。
func notifyHandler(f *feishuClient, urgent bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "POST only"})
			return
		}
		// OpsGaurd notify 侧 client 超时 10s，代理侧整体预算收紧到 8s。
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()

		var p struct {
			ChannelType string         `json:"channel_type"`
			Content     string         `json:"content"`
			Card        map[string]any `json:"card"`
			Post        map[string]any `json:"post"`
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "read body: " + err.Error()})
			return
		}
		if err := json.Unmarshal(body, &p); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "bad json: " + err.Error()})
			return
		}
		if p.Content == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "content is empty"})
			return
		}
		switch p.ChannelType {
		case "feishu", "":
		case "sms":
			writeJSON(w, http.StatusNotImplemented, map[string]any{"ok": false, "error": "sms 未实现"})
			return
		default:
			// webhook 等类型不做透传，避免代理被当作任意 URL 跳板（SSRF）。
			writeJSON(w, http.StatusNotImplemented, map[string]any{"ok": false, "error": "channel_type " + p.ChannelType + " 不支持"})
			return
		}

		if !urgent {
			if err := f.sendWebhook(ctx, p.Content, p.Card, p.Post); err != nil {
				slog.Error("notify(webhook) 失败", "err", err, "content_len", len(p.Content),
					"card", p.Card != nil, "post", p.Post != nil)
				writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"ok": true})
			return
		}
		msgID, err := f.sendAppMessage(ctx, p.Content, p.Card, p.Post)
		if err != nil {
			slog.Error("notify(urgent) 应用消息失败", "err", err)
			writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		if err := f.urgentApp(ctx, msgID, f.urgentUsers); err != nil {
			slog.Error("notify(urgent) 加急失败", "err", err, "message_id", msgID)
			writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message_id": msgID})
	}
}

func healthHandler(c *config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":           true,
			"llm_upstream": c.LLMUpstream,
			"webhook":      c.FeishuWebhook != "",
			"app":          c.FeishuAppID != "" && c.FeishuChatID != "",
			"urgent_users": len(c.FeishuUrgentUsers),
		})
	}
}

// withLog 简易访问日志。
func withLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		slog.Info("http", "remote", r.RemoteAddr, "method", r.Method,
			"path", r.URL.Path, "status", sw.status, "dur_ms", time.Since(start).Milliseconds())
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// newLLMProxy 构建到 LLM_UPSTREAM 的反向代理。SSE 必须立即 flush
// （FlushInterval=-1），否则管理端 AI 排查对话退化为"卡住后一次性吐完"。
func newLLMProxy(c *config) (http.Handler, error) {
	u, err := url.Parse(c.LLMUpstream)
	if err != nil {
		return nil, err
	}
	rp := httputil.NewSingleHostReverseProxy(u)
	rp.FlushInterval = -1
	tr := &http.Transport{
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: c.LLMInsecure},
		ResponseHeaderTimeout: 2 * time.Minute,
		IdleConnTimeout:       90 * time.Second,
	}
	rp.Transport = tr
	rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		slog.Error("llm proxy 上游错误", "err", err, "path", r.URL.Path)
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": "upstream: " + err.Error()})
	}
	return rp, nil
}

func runSelftest(c *config, f *feishuClient, urgent bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	host, _ := os.Hostname()
	msg := fmt.Sprintf("[internet-proxy 自测] host=%s time=%s（部署验证消息，可忽略）", host, time.Now().Format("15:04:05"))
	if err := f.sendWebhook(ctx, msg, nil, nil); err != nil {
		slog.Error("自测失败：webhook", "err", err)
		os.Exit(1)
	}
	slog.Info("自测通过：webhook 群消息已发送")
	msgID, err := f.sendAppMessage(ctx, msg, nil, nil)
	if err != nil {
		slog.Error("自测失败：应用消息（检查应用是否入群、im 权限）", "err", err)
		os.Exit(1)
	}
	slog.Info("自测通过：应用群消息已发送", "message_id", msgID)
	if urgent {
		if err := f.urgentApp(ctx, msgID, f.urgentUsers); err != nil {
			slog.Error("自测失败：加急（检查 urgent user_id 类型与权限）", "err", err)
			os.Exit(1)
		}
		slog.Info("自测通过：已对值班用户加急")
	}
}

func main() {
	selftest := flag.Bool("selftest", false, "发一条机器人+应用群消息验证凭据后退出（不加急）")
	selftestUrgent := flag.Bool("selftest-urgent", false, "在 selftest 基础上额外对值班用户加急（会 buzz，慎用）")
	flag.Parse()

	c := loadConfig()
	f := newFeishu(c)

	if *selftest || *selftestUrgent {
		runSelftest(c, f, *selftestUrgent)
		return
	}

	slog.Info("internet-proxy 启动",
		"listen_llm", c.ListenLLM, "listen_notify", c.ListenNotify,
		"llm_upstream", c.LLMUpstream, "llm_insecure", c.LLMInsecure,
		"webhook", c.FeishuWebhook != "", "app", c.FeishuAppID != "",
		"urgent_users", len(c.FeishuUrgentUsers))

	errCh := make(chan error, 2)

	notifyMux := http.NewServeMux()
	notifyMux.HandleFunc("/notify", notifyHandler(f, false))
	notifyMux.HandleFunc("/urgent", notifyHandler(f, true))
	notifyMux.HandleFunc("/healthz", healthHandler(c))
	notifyMux.HandleFunc("/", healthHandler(c))
	go func() {
		errCh <- http.ListenAndServe(c.ListenNotify, withLog(notifyMux))
	}()

	if c.LLMUpstream != "" {
		llm, err := newLLMProxy(c)
		if err != nil {
			slog.Error("LLM_UPSTREAM 非法", "err", err)
			os.Exit(1)
		}
		llmMux := http.NewServeMux()
		llmMux.Handle("/", llm)
		go func() {
			errCh <- http.ListenAndServe(c.ListenLLM, withLog(llmMux))
		}()
	} else {
		slog.Warn("LLM_UPSTREAM 未配置，LLM 反代未启动（仅通知代理模式）")
	}

	slog.Error("退出", "err", <-errCh)
	os.Exit(1)
}
