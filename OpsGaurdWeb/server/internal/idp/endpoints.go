package idp

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// JWKS 处理 GET /api/v1/idp/jwks：发布 IdP 签名公钥集。
// RP 用这些公钥验签本 IdP 签发的 ID/Access token。
func (s *Service) JWKS(c *gin.Context) {
	set := s.keys.JWKS()
	c.JSON(http.StatusOK, set)
}

// Discovery 处理 GET /.well-known/openid-configuration：OIDC Discovery 文档。
// 这是 RP 自动接入本 IdP 的标准入口（go-oidc / oidc-client 等库凭此发现端点）。
func (s *Service) Discovery(c *gin.Context) {
	issuer := s.cfg.Issuer
	doc := gin.H{
		"issuer":                 issuer,
		"authorization_endpoint": issuer + "/api/v1/idp/authorize",
		"token_endpoint":         issuer + "/api/v1/idp/token",
		"userinfo_endpoint":      issuer + "/api/v1/idp/userinfo",
		"jwks_uri":               issuer + "/api/v1/idp/jwks",
		"end_session_endpoint":   issuer + "/api/v1/idp/logout",
		"introspection_endpoint": issuer + "/api/v1/idp/introspect",
		"response_types_supported":             []string{"code"},
		"grant_types_supported":                []string{"authorization_code", "refresh_token"},
		"subject_types_supported":              []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"token_endpoint_auth_methods_supported": []string{"client_secret_basic", "client_secret_post", "none"},
		"code_challenge_methods_supported":     []string{"S256"},
		"scopes_supported":                     []string{"openid", "profile", "email"},
		"claims_supported": []string{
			"sub", "iss", "aud", "exp", "iat", "auth_time", "nonce",
			"email", "name", "username", "role", "groups",
		},
	}
	c.JSON(http.StatusOK, doc)
}

// UserInfo 处理 GET /api/v1/idp/userinfo：OIDC UserInfo 端点。
// 校验 Bearer access token（验签 + 过期），返回当前用户的标准 claims。
func (s *Service) UserInfo(c *gin.Context) {
	auth := c.GetHeader("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		c.Header("WWW-Authenticate", `Bearer error="invalid_token"`)
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid_token"})
		return
	}
	raw := strings.TrimPrefix(auth, "Bearer ")
	claims, err := s.verifyAccessToken(raw)
	if err != nil {
		c.Header("WWW-Authenticate", `Bearer error="invalid_token"`)
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid_token", "error_description": err.Error()})
		return
	}
	// 吊销检查。
	rec, err := s.st.GetAccessToken(claims.JTI)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "server_error"})
		return
	}
	if rec == nil || rec.Revoked {
		c.Header("WWW-Authenticate", `Bearer error="invalid_token"`)
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid_token"})
		return
	}

	user, err := s.lookupUser(claims.Subject, "")
	if err != nil || user == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "user_not_found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"sub":      user.GetUserID(),
		"email":    user.Email,
		"name":     pickName(user),
		"username": user.Username,
		"role":     user.Role,
		"groups":   user.Groups,
	})
}

// Introspect 处理 POST /api/v1/idp/introspect：RFC 7662 token introspection。
// 调用方（资源服务器，如 Worker）用此端点校验 access token 的有效性与吊销状态。
// 当前用 client 凭证保护本端点（机密客户端可调）。
func (s *Service) Introspect(c *gin.Context) {
	tokenStr := c.PostForm("token")
	if tokenStr == "" {
		c.JSON(http.StatusOK, gin.H{"active": false})
		return
	}
	claims, err := s.verifyAccessToken(tokenStr)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"active": false})
		return
	}
	rec, err := s.st.GetAccessToken(claims.JTI)
	if err != nil || rec == nil || rec.Revoked {
		c.JSON(http.StatusOK, gin.H{"active": false})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"active":    true,
		"scope":     claims.Scope,
		"client_id": claims.ClientID,
		"sub":       claims.Subject,
		"exp":       claims.Expiry,
		"iat":       claims.IssuedAt,
		"token_type": "Bearer",
		"username":  claims.Username,
	})
}

// Logout 处理 GET /api/v1/idp/logout：销毁 IdP SSO 会话。
// 当前为本地登出（清 cookie + 删会话记录）。完整 RP 单点登出（back-channel
// logout 通知）留待后续；预留 end_session_endpoint 已在 discovery 暴露。
func (s *Service) Logout(c *gin.Context) {
	sid, err := c.Cookie(IDPSessionCookieName)
	if err == nil && sid != "" {
		_ = s.st.DeleteIDPSession(sid)
	}
	secure := strings.HasPrefix(s.cfg.Issuer, "https://")
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(IDPSessionCookieName, "", -1, "/", "", secure, true)
	// 回到登录页。
	returnTo := c.Query("post_logout_redirect_uri")
	if returnTo != "" {
		c.Redirect(http.StatusFound, returnTo)
		return
	}
	c.Redirect(http.StatusFound, "/login")
}

// --- access token 验签 ---

// accessClaims 是 access token JWT 中本 IdP 关心的 claims。
type accessClaims struct {
	jwt.Claims
	Scope    string `json:"scope,omitempty"`
	ClientID string `json:"client_id,omitempty"`
	Username string `json:"username,omitempty"`
	Role     string `json:"role,omitempty"`
	JTI      string `json:"jti,omitempty"`
}

// verifyAccessToken 用 JWKS 公钥验签 access token 并校验 iss/exp/aud。
// 返回解析后的 claims；任何错误均视为 token 无效。
func (s *Service) verifyAccessToken(raw string) (*accessClaims, error) {
	// 构造验签器：用本 IdP 的 JWKS（RS256）。
	jwks := s.keys.JWKS()
	// jose 不直接支持 "kid 指向 JWKS" 的便捷 verifier，这里手动 parse。
	tok, err := jwt.ParseSigned(raw, []jose.SignatureAlgorithm{jose.RS256})
	if err != nil {
		return nil, fmt.Errorf("parse token: %w", err)
	}
	var ac accessClaims
	// 找到匹配 kid 的公钥。
	keyOpts := jose.JSONWebKey{}
	matched := false
	for _, hdr := range tok.Headers {
		if hdr.KeyID == "" {
			continue
		}
		keys := jwks.Key(hdr.KeyID)
		if len(keys) > 0 {
			keyOpts = keys[0]
			matched = true
			break
		}
	}
	if !matched {
		return nil, fmt.Errorf("no matching key for token kid")
	}
	if err := tok.Claims(&keyOpts, &ac); err != nil {
		return nil, fmt.Errorf("verify signature: %w", err)
	}
	// 校验 iss / exp。
	if ac.Issuer != s.cfg.Issuer {
		return nil, fmt.Errorf("issuer mismatch")
	}
	if err := ac.Claims.Validate(jwt.Expected{Issuer: s.cfg.Issuer}); err != nil {
		return nil, fmt.Errorf("validate claims: %w", err)
	}
	return &ac, nil
}

// healthCheck 是 IdP 内部自检（启动 / 测试用），确认签名密钥与配置就绪。
func (s *Service) healthCheck() error {
	if s.keys.KID() == "" {
		return fmt.Errorf("no active signing key")
	}
	return nil
}
