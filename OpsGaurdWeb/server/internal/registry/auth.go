// /v2 认证:HTTP Basic(内网 HTTP 场景,凭据为 base64 明文传输——漏扫基线
// 要求有认证;如需加密请后续在反向代理或本服务上 TLS)。账号支持 bcrypt
// 哈希($2a$/$2b$/$2y$,htpasswd 风格)或 {PLAIN} 前缀明文(仅建议测试)。
package registry

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"strings"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
)

// basicAuth 校验 /v2 请求的 Basic 凭据;users 为空 = 不启用认证(内网全信)。
func basicAuth(users map[string]string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if len(users) == 0 {
			c.Next()
			return
		}
		u, p, ok := parseBasic(c.GetHeader("Authorization"))
		if !ok || !checkUser(users, u, p) {
			c.Header("WWW-Authenticate", `Basic realm="opsguard-registry"`)
			ociError(c, 401, "UNAUTHORIZED", "authentication required")
			c.Abort()
			return
		}
		c.Next()
	}
}

func parseBasic(header string) (string, string, bool) {
	if !strings.HasPrefix(header, "Basic ") {
		return "", "", false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(header, "Basic "))
	if err != nil {
		return "", "", false
	}
	u, p, ok := strings.Cut(string(raw), ":")
	return u, p, ok
}

// checkUser 校验账号:bcrypt 哈希走 bcrypt.CompareHashAndPassword;
// {PLAIN} 前缀或裸明文走常量时间比较。
func checkUser(users map[string]string, user, password string) bool {
	stored, ok := users[user]
	if !ok {
		// 用户不存在也走一次比较,防用户名枚举的时序侧信道
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
		return false
	}
	if strings.HasPrefix(stored, "$2") {
		return bcrypt.CompareHashAndPassword([]byte(stored), []byte(password)) == nil
	}
	plain := strings.TrimPrefix(stored, "{PLAIN}")
	a := sha256.Sum256([]byte(plain))
	b := sha256.Sum256([]byte(password))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

var dummyHash = []byte("$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy")
