# OpsGaurd IdP 接入指南

OpsGaurd 管理端可作为标准 **OIDC / OAuth 2.1 身份提供者（IdP）**，其他系统跳转过来完成认证。本文说明如何启用与对接。

## 一、启用 IdP

在管理端 `config.yaml` 增加：

```yaml
idp:
  enabled: true
  issuer: "https://opsguard.example.com"   # 对外可达地址，生产须 https（localhost 例外便于开发）
  access_token_ttl: "1h"                    # access token 有效期，默认 1h
  refresh_token_ttl: "720h"                 # refresh token 有效期，默认 30 天
```

启用后：
- 自动开启管理端认证中间件（若尚未配置 `token_secret` 或 `sso`），并播种默认 admin（`admin` / `opsguard-admin`，首登后请改密）。
- 暴露标准 OIDC 端点（见下表）。
- **签名 RSA 密钥**首次启动自动生成，持久化在 LevelDB（`idpkey/` bucket）；密钥泄露需在「身份提供者」管理页或直接删 LevelDB key 触发重新生成（会致已签发 token 失效）。

> ⚠️ 生产环境 issuer 必须 https。当前 `r.Run` 无内置 TLS，请在反向代理（nginx / ingress）做 TLS 终结。

## 二、OIDC 端点清单

| 端点 | 用途 |
|---|---|
| `GET /.well-known/openid-configuration` | Discovery（go-oidc 等库凭此自动接入） |
| `GET /api/v1/idp/jwks` | JWKS 公钥集（验签 ID/Access token） |
| `GET /api/v1/idp/authorize` | 授权端点 |
| `POST /api/v1/idp/token` | Token 端点（授权码兑换 / refresh） |
| `GET /api/v1/idp/userinfo` | UserInfo（需 Bearer access token） |
| `POST /api/v1/idp/introspect` | RFC 7662 内省（资源服务器校验 token） |
| `GET /api/v1/idp/logout` | 结束 IdP 会话 |

支持的 grant：`authorization_code`、`refresh_token`；强制 PKCE（公共客户端）+ 支持 PKCE（机密客户端）；签名算法 RS256。

## 三、注册 Client（依赖方）

管理员在管理台「系统设置 → 身份提供者」注册：

- **名称**：标识对接方
- **类型**：
  - **公共客户端**（PKCE-only，无 secret）：用于无法安全保存密钥的场景（MCP 工具、SPA、移动端）。授权时必须带 `code_challenge`。
  - **机密客户端**（client_secret）：用于后端服务。创建/轮换时返回一次性明文 secret，妥善保管。
- **回调地址**：精确匹配白名单（可多个）。授权请求的 `redirect_uri` 必须与其中之一**完全相等**。
- **Scope**：允许申请的 scope 子集；留空表示允许 `openid profile email`。

## 四、对接示例

### 4.1 Go（用 coreos/go-oidc 作为 RP）

```go
import (
    "context"
    "github.com/coreos/go-oidc/v3/oidc"
    "golang.org/x/oauth2"
)

provider, _ := oidc.NewProvider(ctx, "https://opsguard.example.com")
cfg := oauth2.Config{
    ClientID:     "cli-xxxx",          // 在管理台注册得到的 client_id
    ClientSecret: "",                  // 公共客户端留空
    Endpoint:     provider.Endpoint(),
    RedirectURL:  "https://your-app.example.com/cb",
    Scopes:       []string{oidc.ScopeOpenID, "profile", "email"},
}
// 公共客户端须设 PKCE verifier/challenge（go-oidc v3 自动处理）
// 跳转用户到 cfg.AuthCodeURL(state, oidc.Nonce(nonce))
// 回调后 cfg.Exchange(ctx, code) 拿 token，再用 provider.Verifier 校验 id_token
verifier := provider.Verifier(&oidc.Config{ClientID: cfg.ClientID})
idTok, _ := verifier.Verify(ctx, rawIDToken)
```

### 4.2 Worker / MCP 工具接入

Worker 侧设环境变量指向 OpsGaurd IdP：

```
OPSGUARD_IDP_ISSUER=https://opsguard.example.com
```

Worker 的 `/.well-known/oauth-protected-resource` 会把该 issuer 写入 `authorization_servers` 字段（RFC 9728），MCP 客户端据此发现 OpsGaurd IdP，走 OAuth 2.1（PKCE）授权码流获取 token，再用该 token 调用 Worker API。Worker 当前同时保留静态 bearer token（`WORKER_TOKENS`）作为兜底，可平滑迁移。

### 4.3 单点登录体验

用户在管理端登录后，OpsGaurd 写入 IdP 会话 cookie（`opsguard_idp_sid`）。此后该用户访问任意已注册 client 的授权请求，**无需再次输入密码**即自动完成授权（除非 client 传 `prompt=login` 强制重新认证）。

## 五、限制与后续

| 项 | 当前状态 | 说明 |
|---|---|---|
| RP 单点登出（back-channel） | 部分 | 当前仅销毁本地 IdP 会话；通知各 client 的 back-channel logout 待补 |
| 动态 client 注册 | 不支持 | client 仅由管理员预注册（企业内可信对接） |
| consent 页 | 不支持 | client 预注册即视为可信，授权码直接签发；`consent_required` 字段已预留 |
| 多实例部署 | 部分 | token/refresh/client 已落 LevelDB 可共享；IdP 会话 cookie 需共享存储（已知约束） |

## 六、HTTPS 与证书（含自签命令）

> ⚠️ 生产环境 issuer 必须 `https://`（localhost 例外）。OpsGaurd 进程本身跑明文 HTTP，证书装在反向代理（nginx / ingress）做 TLS 终结。

按场景三选一：

| 场景 | 推荐方案 | 证书分发 |
|---|---|---|
| 纯本机开发 | `http://localhost` / `http://127.0.0.1` | 无需证书（代码已开例外） |
| 内网生产 | 自签 **CA** + 该 CA 签发的服务证书 | 把 CA 证书分发给所有 RP 的系统信任池 |
| 有正式域名 | Let's Encrypt / 内网 PKI | 自动信任，免维护 |

### 6.1 自签 CA + 服务证书（内网生产推荐）

下面的命令生成一个自签 CA，再用它签发 OpsGaurd 用的服务证书（含 SAN 多域名/IP）。关键是**签 CA 这一步用 CA 证书（非服务私钥）去分发给客户端**——这样以后轮换服务证书不用再逐台重装。

```bash
# === 1) 生成自签 CA（ca.key + ca.crt）—— 一次，长期使用，妥善保管 ca.key ===
openssl genrsa -out ca.key 4096
openssl req -x509 -new -nodes -key ca.key -sha256 -days 3650 \
  -subj "/CN=OpsGaurd Internal CA" \
  -addext "basicConstraints=critical,CA:TRUE,pathlen:0" \
  -addext "keyUsage=critical,keyCertSign,cRLSign" \
  -out ca.crt

# === 2) 生成服务证书私钥 + CSR ===
openssl genrsa -out opsguard.key 2048

# 3) 准备 SAN 扩展（多域名/多IP 都写这里；RP 调用时必须命中其中之一）
cat > opsguard.ext <<'EOF'
authorityKeyIdentifier=keyid,issuer
basicConstraints=CA:FALSE
keyUsage = digitalSignature, keyEncipherment
extendedKeyUsage = serverAuth
subjectAltName = @alt_names

[alt_names]
DNS.1 = opsguard.example.com
DNS.2 = opsguard
IP.1  = 10.0.0.10
IP.2  = 192.168.1.10
EOF

# === 4) 用 CA 签发服务证书（有效期 1 年，到期用同一 CA 重签，ca.key 不动） ===
openssl x509 -req -in opsguard.csr \
  -CA ca.crt -CAkey ca.key -CAcreateserial \
  -out opsguard.crt -days 365 -sha256 \
  -extfile opsguard.ext
```

产物：
- `ca.crt` —— **CA 根证书，分发给所有 RP**（下文 6.3）
- `opsguard.key` + `opsguard.crt` —— 服务证书私钥与证书，装到 nginx/ingress（下文 6.2）
- `ca.key`、`ca.srl` —— **CA 私钥，严格保密**，只在续签服务证书时用到

### 6.2 nginx 做 TLS 终结

```nginx
server {
    listen 443 ssl;
    server_name opsguard.example.com;

    ssl_certificate     /etc/nginx/tls/opsguard.crt;   # 上一步的 opsguard.crt
    ssl_certificate_key /etc/nginx/tls/opsguard.key;   # 上一步的 opsguard.key
    ssl_protocols       TLSv1.2 TLSv1.3;

    # IdP 端点与前端 SPA 都转发给本机 OpsGaurd（明文 HTTP，:8090）
    location / {
        proxy_pass http://127.0.0.1:8090;
        proxy_set_header Host              $host;
        proxy_set_header X-Real-IP         $remote_addr;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto https;   # 让 OpsGaurd 知道外面是 https
    }
}
```

对应 `config.yaml`：
```yaml
idp:
  enabled: true
  issuer: "https://opsguard.example.com"   # 对外 https 地址，须命中服务证书 SAN
```

### 6.3 RP 客户端信任自签 CA

分两种思路，**优先选 6.3.1（装系统信任池）**，因为它一次配置全局生效、不污染代码。

#### 6.3.1 把 CA 装进操作系统信任池（推荐）

**Linux（RHEL/CentOS）**：
```bash
sudo cp ca.crt /etc/pki/ca-trust/source/anchors/opsguard-ca.crt
sudo update-ca-trust extract
```
**Linux（Debian/Ubuntu）**：
```bash
sudo cp ca.crt /usr/local/share/ca-certificates/opsguard-ca.crt
sudo update-ca-certificates
```
装好后，go-oidc / go 客户端默认 `oidc.NewProvider(ctx, ...)` 会自动信任（Go 读系统证书池）。

**RP 本身是 nginx/ingress 反代**：同样把 CA 放到反代验证上游或做 mTLS 的位置。

#### 6.3.2 Go 客户端代码级信任（不便动系统时）

把 `ca.crt` 与程序一起部署，构造带 CA 池的 http.Client：

```go
import (
    "crypto/x509"
    "net/http"
    "os"

    "github.com/coreos/go-oidc/v3/oidc"
)

func newProviderWithCA(issuer, caPath string) (*oidc.Provider, error) {
    caPEM, err := os.ReadFile(caPath)
    if err != nil {
        return nil, err
    }
    pool := x509.NewCertPool()
    if !pool.AppendCertsFromPEM(caPEM) {
        return nil, fmt.Errorf("invalid CA pem")
    }
    client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
    return oidc.NewProvider(oidc.ClientContext(context.Background(), client), issuer)
}
```

> ❌ 不要在生产用 `InsecureSkipVerify: true` 绕过校验——它跳过所有证书检查，易被中间人。仅本机调试临时用。

### 6.4 续签服务证书

服务证书到期前，用原 CA（`ca.key` 不变）重签，nginx reload 即可，RP 端无需任何改动（CA 没变）：

```bash
openssl x509 -req -in opsguard.csr \
  -CA ca.crt -CAkey ca.key -CAcreateserial \
  -out opsguard.crt -days 365 -sha256 -extfile opsguard.ext
sudo systemctl reload nginx
```

CA 私钥（`ca.key`）泄漏需重新生成整套 CA + 所有服务证书，并把新 CA 重新分发到所有 RP。

