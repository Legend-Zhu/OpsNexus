package idp

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/store"
)

// TokenRequest 是 token 端点（application/x-www-form-urlencoded）的参数。
type TokenRequest struct {
	GrantType    string `form:"grant_type"`
	Code         string `form:"code"`
	RedirectURI  string `form:"redirect_uri"`
	ClientID     string `form:"client_id"`
	ClientSecret string `form:"client_secret"`
	CodeVerifier string `form:"code_verifier"`
	RefreshToken string `form:"refresh_token"`
	Scope        string `form:"scope"`
}

// TokenResponse 是成功响应（RFC 6749 §5.1）。
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token,omitempty"`
	IDToken      string `json:"id_token,omitempty"`
	Scope        string `json:"scope,omitempty"`
}

// Token 处理 POST /api/v1/idp/token。
// 支持 grant_type=authorization_code 与 refresh_token。
// 客户端认证：机密客户端用 Authorization: Basic 或 form 的 client_secret（bcrypt 校验）；
// 公共客户端免 secret，仅靠 PKCE。
func (s *Service) Token(c *gin.Context) {
	var req TokenRequest
	if err := c.ShouldBind(&req); err != nil {
		tokenError(c, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	// 允许 client 凭证通过 HTTP Basic 头覆盖 form 字段。
	if basicID, basicSecret, ok := c.Request.BasicAuth(); ok {
		if req.ClientID == "" {
			req.ClientID = basicID
		}
		if req.ClientSecret == "" {
			req.ClientSecret = basicSecret
		}
	}

	client, authed := s.authenticateClient(c, &req)
	if !authed {
		return // authenticateClient 已写入错误响应
	}

	switch req.GrantType {
	case "authorization_code":
		s.handleAuthorizationCode(c, &req, client)
	case "refresh_token":
		s.handleRefreshToken(c, &req, client)
	default:
		tokenError(c, http.StatusBadRequest, "unsupported_grant_type", "grant_type must be authorization_code or refresh_token")
	}
}

// authenticateClient 校验客户端认证：公共客户端放行（PKCE 兜底），机密客户端查 bcrypt。
func (s *Service) authenticateClient(c *gin.Context, req *TokenRequest) (*store.Client, bool) {
	client, err := s.clients.Get(req.ClientID)
	if err != nil {
		tokenError(c, http.StatusInternalServerError, "server_error", "client lookup failed")
		return nil, false
	}
	if client == nil {
		tokenError(c, http.StatusUnauthorized, "invalid_client", "unknown client")
		return nil, false
	}
	if client.Public {
		return client, true // 公共客户端免 secret，PKCE 在授权码分支校验
	}
	// 机密客户端：secret 必须匹配。
	if _, err := s.clients.Authenticate(req.ClientID, req.ClientSecret); err != nil {
		tokenError(c, http.StatusUnauthorized, "invalid_client", "authentication failed")
		return nil, false
	}
	return client, true
}

// handleAuthorizationCode 处理授权码兑换。
func (s *Service) handleAuthorizationCode(c *gin.Context, req *TokenRequest, client *store.Client) {
	ac, err := s.st.GetAuthCode(req.Code)
	if err != nil {
		tokenError(c, http.StatusInternalServerError, "server_error", "authcode lookup failed")
		return
	}
	if ac == nil {
		tokenError(c, http.StatusBadRequest, "invalid_grant", "unknown authorization code")
		return
	}
	if ac.Used {
		// code 被重用：安全事件，立即吊销其已签发的 token（OAuth 2.1 §7.5）。
		s.revokeCodeTokens(ac)
		tokenError(c, http.StatusBadRequest, "invalid_grant", "authorization code already used")
		return
	}
	if time.Now().After(ac.Expiry) {
		tokenError(c, http.StatusBadRequest, "invalid_grant", "authorization code expired")
		return
	}
	if ac.ClientID != client.ID {
		tokenError(c, http.StatusBadRequest, "invalid_grant", "code was issued to a different client")
		return
	}
	if ac.RedirectURI != req.RedirectURI {
		tokenError(c, http.StatusBadRequest, "invalid_grant", "redirect_uri mismatch")
		return
	}
	// PKCE 校验。
	if ac.CodeChallenge != "" {
		if req.CodeVerifier == "" {
			tokenError(c, http.StatusBadRequest, "invalid_grant", "missing code_verifier")
			return
		}
		sum := sha256.Sum256([]byte(req.CodeVerifier))
		expected := base64.RawURLEncoding.EncodeToString(sum[:])
		if expected != ac.CodeChallenge {
			tokenError(c, http.StatusBadRequest, "invalid_grant", "PKCE verification failed")
			return
		}
	}

	// 标记 code 已用（一次性）。删除而非置位，确保不可重用。
	if err := s.st.DeleteAuthCode(ac.Code); err != nil {
		tokenError(c, http.StatusInternalServerError, "server_error", "invalidate code failed")
		return
	}

	user, err := s.lookupUser(ac.UserID, ac.Username)
	if err != nil || user == nil {
		tokenError(c, http.StatusBadRequest, "invalid_grant", "user no longer exists")
		return
	}

	resp, err := s.issueTokens(c, client, user, ac.Scopes, ac.Nonce, ac.AuthTime)
	if err != nil {
		tokenError(c, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	resp.Scope = strings.Join(ac.Scopes, " ")
	c.JSON(http.StatusOK, resp)
}

// handleRefreshToken 处理 refresh_token 兑换（轮换式：发新 refresh，删旧）。
func (s *Service) handleRefreshToken(c *gin.Context, req *TokenRequest, client *store.Client) {
	rt, err := s.st.GetRefreshToken(req.RefreshToken)
	if err != nil {
		tokenError(c, http.StatusInternalServerError, "server_error", "refresh token lookup failed")
		return
	}
	if rt == nil || rt.Revoked {
		tokenError(c, http.StatusBadRequest, "invalid_grant", "unknown or revoked refresh token")
		return
	}
	if time.Now().After(rt.Expiry) {
		tokenError(c, http.StatusBadRequest, "invalid_grant", "refresh token expired")
		return
	}
	if rt.ClientID != client.ID {
		tokenError(c, http.StatusBadRequest, "invalid_grant", "refresh token issued to a different client")
		return
	}
	// scope 收紧：可选，不可放大。
	scopes := rt.Scopes
	if req.Scope != "" {
		reqScopes := splitScopes(req.Scope)
		if !scopesAllowed(scopes, reqScopes) {
			tokenError(c, http.StatusBadRequest, "invalid_scope", "requested scope exceeds original grant")
			return
		}
		scopes = reqScopes
	}

	user, err := s.lookupUser(rt.Sub, rt.Username)
	if err != nil || user == nil {
		tokenError(c, http.StatusBadRequest, "invalid_grant", "user no longer exists")
		return
	}

	// 删除旧 refresh token（轮换）。
	if err := s.st.DeleteRefreshToken(rt.Token); err != nil {
		tokenError(c, http.StatusInternalServerError, "server_error", "rotate refresh token failed")
		return
	}

	resp, err := s.issueTokens(c, client, user, scopes, "", time.Time{})
	if err != nil {
		tokenError(c, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	resp.Scope = strings.Join(scopes, " ")
	c.JSON(http.StatusOK, resp)
}

// issueTokens 签发 ID/Access/Refresh token 并落库。
func (s *Service) issueTokens(c *gin.Context, client *store.Client, user *store.User, scopes []string, nonce string, authTime time.Time) (*TokenResponse, error) {
	now := time.Now().UTC()
	accessTTL := s.clientAccessTTL(client)
	exp := now.Add(accessTTL)
	jti := uuid.NewString()

	// --- Access Token（JWT，RS256） ---
	accessClaims := map[string]any{
		"iss":       s.cfg.Issuer,
		"sub":       user.GetUserID(),
		"aud":       client.ID,
		"exp":       exp.Unix(),
		"iat":       now.Unix(),
		"jti":       jti,
		"client_id": client.ID,
		"scope":     strings.Join(scopes, " "),
		"username":  user.Username,
		"role":      user.Role,
	}
	accessToken, err := s.signJWT(accessClaims)
	if err != nil {
		return nil, fmt.Errorf("sign access token: %w", err)
	}
	// access token 影子记录（吊销 / introspect 用）。
	if err := s.st.PutAccessToken(&store.AccessTokenRecord{
		JTI:       jti,
		Sub:       user.GetUserID(),
		ClientID:  client.ID,
		Scope:     strings.Join(scopes, " "),
		Expiry:    exp,
		CreatedAt: now,
	}); err != nil {
		return nil, fmt.Errorf("persist access token: %w", err)
	}

	// --- ID Token（JWT，RS256），仅当请求 openid scope 时签发 ---
	idToken := ""
	if contains(scopes, "openid") {
		idClaims := map[string]any{
			"iss":       s.cfg.Issuer,
			"sub":       user.GetUserID(),
			"aud":       client.ID,
			"exp":       exp.Unix(),
			"iat":       now.Unix(),
			"auth_time": authTime.Unix(),
			"email":     user.Email,
			"name":      pickName(user),
			"username":  user.Username,
			"role":      user.Role,
		}
		if len(user.Groups) > 0 {
			idClaims["groups"] = user.Groups
		}
		if nonce != "" {
			idClaims["nonce"] = nonce
		}
		idToken, err = s.signJWT(idClaims)
		if err != nil {
			return nil, fmt.Errorf("sign id token: %w", err)
		}
	}

	// --- Refresh Token（不透明随机串） ---
	refreshToken := randomToken(32)
	refreshTTL := s.refreshTokenTTL
	if err := s.st.PutRefreshToken(&store.RefreshToken{
		Token:     refreshToken,
		Sub:       user.GetUserID(),
		Username:  user.Username,
		ClientID:  client.ID,
		Scopes:    scopes,
		Expiry:    now.Add(refreshTTL),
		CreatedAt: now,
	}); err != nil {
		return nil, fmt.Errorf("persist refresh token: %w", err)
	}

	return &TokenResponse{
		AccessToken:  accessToken,
		TokenType:    "Bearer",
		ExpiresIn:    int(accessTTL.Seconds()),
		RefreshToken: refreshToken,
		IDToken:      idToken,
	}, nil
}

// signJWT 用 signing keys 签发 claims 为紧凑序列化 JWT。
func (s *Service) signJWT(claims map[string]any) (string, error) {
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	return s.keys.Sign(payload)
}

// lookupUser 优先按 user.ID（ac.UserID / rt.Sub）查；ID 形态不匹配则退回 username。
func (s *Service) lookupUser(userID, username string) (*store.User, error) {
	users, err := s.st.ListUsers()
	if err != nil {
		return nil, err
	}
	for _, u := range users {
		if u.ID == userID {
			return u, nil
		}
	}
	if username != "" {
		for _, u := range users {
			if u.Username == username {
				return u, nil
			}
		}
	}
	return nil, nil
}

// revokeCodeTokens 在 code 被重用时吊销关联 token（尽力而为，OAuth 2.1 建议）。
// 当前实现：code 已删除，无法直接定位 jti；扩展时可把 jti 存进 AuthCode 实现精确吊销。
func (s *Service) revokeCodeTokens(_ *store.AuthCode) {
	// TODO: 在 AuthCode 增加 IssuedJTI 字段后实现精确吊销。当前依赖 token 短 TTL 自愈。
}

// clientAccessTTL 取 client 覆盖的 TTL，否则全局默认。
func (s *Service) clientAccessTTL(client *store.Client) time.Duration {
	if client.TokenTTL != "" {
		if d := parseTTL(client.TokenTTL, 0); d > 0 {
			return d
		}
	}
	return s.accessTokenTTL
}

// pickName 取展示名：DisplayName > Username。
func pickName(u *store.User) string {
	if u.DisplayName != "" {
		return u.DisplayName
	}
	return u.Username
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// tokenError 写入 OAuth token 端点错误响应（RFC 6749 §5.2）。
func tokenError(c *gin.Context, status int, errCode, desc string) {
	c.JSON(status, gin.H{"error": errCode, "error_description": desc})
}
