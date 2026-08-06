package idp

import (
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
)

// IDPSessionCookieName 是 OpsGaurd IdP SSO 会话 cookie 的名称。
// 用户在管理端登录后写入此 cookie；/authorize 端点凭它判断是否已登录，
// 实现跨多个 RP 的单点登录体验。
const IDPSessionCookieName = "opsguard_idp_sid"

// Authorize 处理 OAuth 2.1 / OIDC 授权端点 GET /api/v1/idp/authorize。
//
// 流程：
//  1. 校验 client_id、redirect_uri（精确匹配白名单）、response_type=code、scope 子集。
//  2. 读 IdP 会话 cookie。无会话或 prompt=login → 302 到管理端登录页
//     （带 return_to 编码原 authorize 参数），登录成功后回跳继续授权。
//  3. 已有会话：生成一次性授权码，绑定 client/user/redirect_uri/scope/nonce/PKCE challenge。
//  4. 302 回 redirect_uri?code=&state=。
func (s *Service) Authorize(c *gin.Context) {
	q := c.Request.URL.Query()

	clientID := q.Get("client_id")
	redirectURI := q.Get("redirect_uri")
	responseType := q.Get("response_type")
	state := q.Get("state")
	scope := q.Get("scope")
	nonce := q.Get("nonce")
	codeChallenge := q.Get("code_challenge")
	codeChallengeMethod := q.Get("code_challenge_method")
	prompt := q.Get("prompt")

	// 1. 基础参数校验。
	client, err := s.clients.Get(clientID)
	if err != nil {
		authorizeError(c, redirectURI, "server_error", "internal error", state)
		return
	}
	if client == nil {
		authorizeError(c, redirectURI, "invalid_client", "unknown client", state)
		return
	}
	// redirect_uri 必须精确匹配白名单（OAuth 2.1 要求）。
	if !containsExact(client.RedirectURIs, redirectURI) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_redirect_uri"})
		return
	}
	if responseType != "code" {
		authorizeError(c, redirectURI, "unsupported_response_type", "response_type must be 'code'", state)
		return
	}
	// OAuth 2.1：授权码流程必须支持 PKCE。公共客户端必须有 code_challenge。
	if client.Public && codeChallenge == "" {
		authorizeError(c, redirectURI, "invalid_request", "public client requires PKCE code_challenge", state)
		return
	}
	if codeChallenge != "" && codeChallengeMethod != "S256" {
		authorizeError(c, redirectURI, "invalid_request", "code_challenge_method must be S256", state)
		return
	}
	// scope 必须是 client 允许范围的子集；openid 自动允许。
	scopes := splitScopes(scope)
	if !scopesAllowed(client.Scopes, scopes) {
		authorizeError(c, redirectURI, "invalid_scope", "requested scope exceeds client allowance", state)
		return
	}

	// 2. IdP 会话检查。
	sid, err := c.Cookie(IDPSessionCookieName)
	if err != nil || sid == "" {
		s.redirectToLogin(c, q, clientID)
		return
	}
	sess, err := s.st.GetIDPSession(sid)
	if err != nil {
		authorizeError(c, redirectURI, "server_error", "session lookup failed", state)
		return
	}
	if sess == nil {
		s.redirectToLogin(c, q, clientID)
		return
	}
	if prompt == "login" {
		// 强制重新认证：销毁当前会话后跳登录页。
		_ = s.st.DeleteIDPSession(sid)
		s.redirectToLogin(c, q, clientID)
		return
	}

	// 3. 签发授权码。
	code := randomToken(24)
	ac := &store.AuthCode{
		Code:                code,
		ClientID:            clientID,
		UserID:              sess.UserID,
		Username:            sess.Username,
		RedirectURI:         redirectURI,
		Scopes:              scopes,
		Nonce:               nonce,
		CodeChallenge:       codeChallenge,
		CodeChallengeMethod: codeChallengeMethod,
		AuthTime:            sess.CreatedAt,
		Expiry:              time.Now().Add(defaultAuthCodeTTL),
	}
	if err := s.st.PutAuthCode(ac); err != nil {
		authorizeError(c, redirectURI, "server_error", "persist auth code failed", state)
		return
	}

	// 4. 302 回 RP。
	u, _ := url.Parse(redirectURI)
	rq := u.Query()
	rq.Set("code", code)
	if state != "" {
		rq.Set("state", state)
	}
	u.RawQuery = rq.Encode()
	c.Redirect(http.StatusFound, u.String())
}

// redirectToLogin 把用户导向管理端登录页，并把原 authorize 参数编码进 return_to，
// 登录成功后由 /auth/login 的 IdP 会话建立 + 前端回到 return_to 继续授权。
func (s *Service) redirectToLogin(c *gin.Context, origQuery url.Values, clientID string) {
	returnTo := "/api/v1/idp/authorize?" + origQuery.Encode()
	// 登录页是前端 SPA 路由；前端登录成功后检测到 return_to 参数则跳转过去。
	loginURL := "/login?return_to=" + url.QueryEscape(returnTo) + "&idp=1"
	c.Redirect(http.StatusFound, loginURL)
}

// EstablishSession 在管理端登录成功后调用：创建 IdP SSO 会话并写 cookie。
// 返回 sid 供调用方（system.Login）使用。maxAgeSec 为 0 时用默认会话有效期。
func (s *Service) EstablishSession(c *gin.Context, user *store.User, maxAgeSec int) (string, error) {
	if user == nil {
		return "", nil
	}
	sid := randomToken(24)
	ttl := time.Duration(maxAgeSec) * time.Second
	if ttl <= 0 {
		ttl = defaultIDPSessionTTL
	}
	sess := &store.IDPSession{
		SID:       sid,
		UserID:    user.GetUserID(),
		Username:  user.Username,
		Role:      user.Role,
		CreatedAt: time.Now().UTC(),
		LastSeen:  time.Now().UTC(),
		ExpiresAt: time.Now().Add(ttl).UTC(),
	}
	if err := s.st.PutIDPSession(sess); err != nil {
		return "", err
	}
	secure := strings.HasPrefix(s.cfg.Issuer, "https://")
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(IDPSessionCookieName, sid, int(ttl.Seconds()), "/", "", secure, true)
	return sid, nil
}

// authorizeError 把 OAuth 错误重定向回 RP 的 redirect_uri（标准 §4.1.2.1）。
// redirect_uri 为空时无法回跳，降级为本地 JSON 错误。
func authorizeError(c *gin.Context, redirectURI, errCode, errDesc, state string) {
	if redirectURI == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": errCode, "error_description": errDesc})
		return
	}
	u, perr := url.Parse(redirectURI)
	if perr != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_redirect_uri"})
		return
	}
	rq := u.Query()
	rq.Set("error", errCode)
	if errDesc != "" {
		rq.Set("error_description", errDesc)
	}
	if state != "" {
		rq.Set("state", state)
	}
	u.RawQuery = rq.Encode()
	c.Redirect(http.StatusFound, u.String())
}

// containsExact 精确匹配白名单（不做前缀/通配匹配）。
func containsExact(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

// splitScopes 把空格分隔的 scope 串拆成切片。
func splitScopes(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Fields(s)
}

// scopesAllowed 判断 requested 是否为 allowed 的子集（allowed 含 openid/profile/email）。
// allowed 为空时不做限制（向后兼容旧 client；管理员可按需收紧）。
func scopesAllowed(allowed, requested []string) bool {
	if len(allowed) == 0 {
		return true
	}
	set := make(map[string]bool, len(allowed)+3)
	for _, a := range allowed {
		set[a] = true
	}
	for _, r := range requested {
		if !set[r] {
			return false
		}
	}
	return true
}
