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
