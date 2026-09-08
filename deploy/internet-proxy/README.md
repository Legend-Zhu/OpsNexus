# internet-proxy —— 内网管理端的受控出网代理

部署在互联网服务器 **10.50.182.57** 上，为无外网的管理端（**10.60.189.6**，
opsguard-server）经网闸（**10.60.114.2**）提供两类出网能力，按性质拆成两个
组件（2026-09 起）：

- **nginx**：LLM 等通用 HTTP 反向代理——纯透传，新加上游只改配置 reload；
- **internet-proxy（Go）**：飞书通知代理——含加签/取 token/PATCH 加急等
  nginx 做不了的协议逻辑，凭据持有在代理侧。

```
189.6 (opsguard-server)        网闸 10.60.114.2           182.57
┌────────────────────┐    ┌─ 50376 ─→ 7070 ─┐    ┌─────────────────────────────┐
│ 模型 provider       │───▶│                 │───▶│ nginx 反代（单端口路径路由）  │
│ 主网关 :50376/v1    │    │                 │    │  /       → 82.156.119.58:5002
│ Qwen  :50376/qwen/v1│    │                 │    │  /qwen/  → 111.228.46.187:63001
│                    │    ├─ 50378 ─→ 7072 ─┤    ├─────────────────────────────┤
│ 通知渠道(飞书)      │───▶│                 │───▶│ internet-proxy(Go，仅飞书)   │
│ proxy=:50378/notify│    │                 │    │  /notify → 群机器人(加签)    │
└────────────────────┘    └─────────────────┘    │  /urgent → 应用+加急(5人)    │
                                                 └─────────────────────────────┘
```

## 端口分配（网闸任务 1731-1736）

| 任务号 | 网闸端口 | 182.57 端口 | 用途 |
| --- | --- | --- | --- |
| 1731 | 50376 | 7070 | LLM 反代（**nginx**，单端口路径路由）：`/qwen/*` → `http://111.228.46.187:63001`（Qwen vLLM），其余 → `https://82.156.119.58:5002`（主网关，自签证书） |
| 1732 | 50377 | 7071 | **预留**（网闸任务已开通但未使用——加模型源走 7070 路径路由，不再开端口） |
| 1733 | 50378 | 7072 | 飞书通知代理（**Go**，`/notify` 机器人 + `/urgent` 应用加急） |
| 1734-1736 | 50379-81 | 7073-75 | 预留 |

LLM 网关可用模型（2026-08 实测 `/v1/models`）：`deepseek-v4-flash`（本次采用）、
`deepseek-v4-pro`、`deepseek-v3.2`、`glm-5` / `glm-5.1` / `glm-5.2`、`kimi-k2.5/2.6`、
`qwen-3.6` 等，后续加模型只改管理端页面，代理不动。

## 新增代理（不改代码、不重发二进制）

| 需求 | 操作 |
| --- | --- |
| 新增模型源（OpenAI 兼容） | `nginx-llm.conf` 加一个 `location /<名>/ { proxy_pass http://<上游>/; ... }` 块 → reload；管理端加 provider，base_url 填 `http://10.60.114.2:50376/<名>/v1`。**同端口，不动网闸** |
| 新增/更换非 LLM 的通用 HTTP 上游（整端口独占） | 复制 `nginx-llm.conf` 里一个 server 块，改 `listen`/`proxy_pass` → reload（需同步开网闸任务） |
| 更换飞书群/加签密钥/应用凭据 | 改 `/etc/opsguard-internet-proxy.env` → `systemctl restart opsguard-internet-proxy` |
| 新增消息协议类型（如钉钉、短信） | 才需要改 internet-proxy 代码 |

## 组成

| 文件 | 说明 |
| --- | --- |
| `nginx-llm.conf` | LLM 反代 nginx 配置（含备用源模板，SSE 必须关缓冲） |
| `main.go` | 单文件 Go（纯 stdlib，无第三方依赖）：飞书通知代理 |
| `proxy.env.example` | 飞书代理配置模板（真实 `proxy.env` 含凭据，已 gitignore） |
| `deploy.sh` | 在 182.57 上安装 systemd 服务 `opsguard-internet-proxy` |
| `Dockerfile` / `docker-compose.yml` | 可选容器化路径（仅飞书代理） |

飞书凭据全部持有在代理侧（公网凭据不入内网 LevelDB）：

- 群机器人 webhook + 加签 secret → `/notify`，对应**所有级别**推送
- 飞书应用（app_id/secret）+ 群 chat_id + 值班 user_id ×5 → `/urgent`，
  发应用群消息并**应用内加急**对应值班用户，只绑 **error** 级
  （OpsGaurd 告警级别只有 error/warn/info，error 即最高级）

## 部署（182.57）

**nginx（LLM 反代）**——182.57 的 nginx 为**源码编译**（`--prefix=/usr/local/nginx`，
非包管理器安装，没有 `/etc/nginx`），conf 放 `conf.d/` 子目录（nginx.conf 已含
`include conf.d/*.conf;`）：

```bash
scp deploy/internet-proxy/nginx-llm.conf root@10.50.182.57:/usr/local/nginx/conf/conf.d/opsguard-llm.conf
ssh root@10.50.182.57 '/usr/local/nginx/sbin/nginx -t && /usr/local/nginx/sbin/nginx -s reload'
```

> 2026-09-08 前 nginx 编译参数只有 `--with-stream`（无 SSL 模块，`proxy_pass https://`
> 会报 `https protocol requires SSL support`）。已在 `/usr/local/src/nginx-1.14.0`
> 重编译加 `--with-http_ssl_module` 并 USR2 热切换，旧无 SSL 二进制备份为
> `sbin/nginx-nossl-bak`。该 nginx 还承载 8080（zhitan-base 前端）与 7079
> （kingbase TCP 代理），动它需谨慎。若服务器重装或升级 nginx，务必保留
> stream + http_ssl 两个模块。

**Go 二进制（飞书代理）**——开发机交叉编译（产物在仓库根 `.deploytmp/`，已 gitignore）：

```bash
cd deploy/internet-proxy
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags '-s -w' \
  -o ../../.deploytmp/internet-proxy .
```

分发并安装（scp 到 182.57 后 root 执行）：

```bash
scp .deploytmp/internet-proxy deploy/internet-proxy/{proxy.env,deploy.sh} root@10.50.182.57:/tmp/ip/
ssh root@10.50.182.57 'cd /tmp/ip && chmod +x deploy.sh internet-proxy && ./deploy.sh'
```

182.57 有外网时也可 `docker compose up -d --build`（见 compose 注释的暴露面说明）。

**首切顺序（旧版 Go 二进制同时占着 7070）**：`systemctl stop opsguard-internet-proxy`
→ `./deploy.sh`（新二进制只绑 7072）→ 部署 nginx conf 并 reload（此时 7070 释放，
nginx 接管）。期间 LLM 链路中断数十秒，飞书通知不受影响。旧 proxy.env 里残留的
`LISTEN_LLM/LLM_UPSTREAM/LLM_INSECURE` 新二进制直接忽略，env 文件可不改。

**暴露面（重要）**：`LISTEN_NOTIFY` 与 nginx `listen` 都绑定 `10.50.182.57`
（网闸侧内网 IP），不监听公网；若宿主机没有绑定该 IP 的网卡（如容器模式），
退回 `0.0.0.0` + 防火墙只对网闸/内网放行 7070-7075。网闸侧已限源 189.6。

**验证（182.57 本机）**：

```bash
curl http://10.50.182.57:7072/healthz            # 飞书代理配置就绪状态
curl http://10.50.182.57:7070/v1/models \
  -H 'Authorization: Bearer <llm-key>'           # LLM 链路（nginx）
# 自测走服务端口即可（直跑二进制不继承 systemd 的 EnvironmentFile）：
curl -X POST http://10.50.182.57:7072/notify -H 'Content-Type: application/json' \
  -d '{"channel_type":"feishu","content":"测试"}' # 群里应收到消息
# 卡片样式（代理 ≥ 2026-09 版本）：请求体带可选 "card" 对象即以
# msg_type=interactive 发送（/notify 直发卡片；/urgent 应用消息 content 传
# 卡片 JSON 字符串）。不带 card 时保持纯文本，旧版管理端不受影响。
curl -X POST http://10.50.182.57:7072/notify -H 'Content-Type: application/json' \
  -d '{"channel_type":"feishu","content":"文本兜底","card":{"header":{"title":{"tag":"plain_text","content":"告警 - 资源超限"},"template":"red"},"elements":[{"tag":"div","text":{"tag":"lark_md","content":"**摘要:** cpu usage 98.6% >= 85%"}}]}}'
# 富文本样式（代理 ≥ 2026-09 版本）：请求体带可选 "post" 对象（zh_cn
# 结构，巡检报告）即以 msg_type=post 发送；card 优先于 post，均缺省时
# 保持纯文本。
curl -X POST http://10.50.182.57:7072/notify -H 'Content-Type: application/json'   -d '{"channel_type":"feishu","content":"文本兜底","post":{"zh_cn":{"title":"巡检报告","content":[[{"tag":"text","text":"• 🟢 端口探活：正常"}]]}}}'
# 加急链路会 buzz 5 名值班用户，建议明确告知后执行：
curl -X POST http://10.50.182.57:7072/urgent -H 'Content-Type: application/json' \
  -d '{"channel_type":"feishu","content":"[测试] 加急链路验证"}'
```

## 管理端配置（189.6，全部页面操作）

前置连通性（server 容器内也要通，容器出网 SNAT 成宿主 IP，仍满足网闸白名单）：

```bash
curl http://10.60.114.2:50376/v1/models -H 'Authorization: Bearer <llm-key>'
curl http://10.60.114.2:50378/healthz
```

1. **MLOps → 模型接入**（admin）→ 添加 Provider：类型 `OpenAI 兼容`、
   Base URL `http://10.60.114.2:50376/v1`、API Key（LLM 网关 key）→
   添加模型 `deepseek-v4-flash` →「测试连接」→ 保存（热重载）。
   已配的第二个 Provider：`qwen-vllm`，Base URL `http://10.60.114.2:50376/qwen/v1`
   （Qwen vLLM 源经 7070 `/qwen/` 路由），模型 `qwen3.6`，2026-09-08 经
   `PUT /api/v1/ainexus/config` 配置并通过 `/config/test` 实测（623ms）。
2. **系统设置 → AI 排查网关**：启用 + 默认模型 `deepseek-v4-flash`。
3. **通知中心 → 渠道** 新建两条（都开「经互联网代理」）：

   | 渠道名 | 类型 | 代理地址 | 用途 |
   | --- | --- | --- | --- |
   | 飞书-告警群 | 飞书 | `http://10.60.114.2:50378/notify` | 全级别 |
   | 飞书-加急 | 飞书 | `http://10.60.114.2:50378/urgent` | error 级 |

4. **通知中心 → 策略**：`error` → 勾选两条渠道；`warn` / `info` → 只勾
   「飞书-告警群」。填接收人。**没配策略的级别不发通知**。
5. 用量费用计量需 server 配置 `mlops.enabled: true`（改后重启容器，可选）。

## 验收

- 异常排查页发起对话：确认**逐字流式**输出（SSE 过网闸 + nginx `proxy_buffering off` 正常，无缓冲）。
- 触发一条 error 告警（如临时把 CPU 阈值调到 1%）：群里收到机器人消息 +
   应用消息，值班用户收到加急 buzz；通知中心「发送记录」两条均 success。
- 触发一条 warn 告警：只有机器人消息，无加急。
- 智能巡检配「报告模型」出 AI 报告。

## 安全说明

- 网闸段（189.6→114.2）是明文 HTTP，LLM key 经 Authorization 头过网闸；
  网闸已限源 189.6，内网段可接受。要更强需两端 TLS，成本高，暂不做。
- 飞书 webhook / 加签 secret / 应用 secret / LLM key 分别只存在于
  182.57 的 `/etc/opsguard-internet-proxy.env`（0600）与 nginx 运行配置
  （key 本身不落 nginx 配置，仍由请求头透传），不进 git、不进管理端 LevelDB。
- 代理不支持 `channel_type=webhook` 透传，避免被当作任意 URL 跳板（SSRF）。

## 运维

```bash
systemctl status opsguard-internet-proxy          # 飞书代理运行状态
journalctl -u opsguard-internet-proxy -f          # 访问日志 / 失败详情
# 改配置：编辑 /etc/opsguard-internet-proxy.env && systemctl restart opsguard-internet-proxy
# 升级二进制：systemctl stop → install -m 0755 <new> /opt/opsguard/internet-proxy/internet-proxy → start
#（直接覆盖运行中的二进制会 text file busy）
nginx -t && systemctl reload nginx                # 包管理器版 nginx（通用路径）
/usr/local/nginx/sbin/nginx -t && /usr/local/nginx/sbin/nginx -s reload   # 182.57 源码编译版
tail -f /usr/local/nginx/logs/access.log /usr/local/nginx/logs/error.log  # 182.57 LLM 链路排障
```

## 部署记录

- **2026-09-08 Qwen 模型源接入（单端口路径路由定型）**：
  - 7070 改为按路径前缀路由：`/qwen/*` → `http://111.228.46.187:63001`（vLLM，
    模型 `qwen3.6`，明文 HTTP），其余路径 → 主网关。**新加模型源 = nginx 加
    location 块 + reload，不再新开端口/网闸任务**；1732/50377→7071 任务虽已
    开通但按此设计闲置保留；
  - 管理端经 `PUT /api/v1/ainexus/config` 追加 provider `qwen-vllm`（GET 脱敏
    视图合并：已存 provider api_key 置空沿用、cluster 自动 MCP 不回传），热重
    载后 `/config/test` 实测 `qwen3.6` ok（623ms）；模型池现为 deepseek-v4-flash /
    deepseek-v4-pro / qwen3.6；
  - 管理端 API 的 MCP Server（60 工具）不覆盖模型 provider 配置，自动化走
    `PUT /api/v1/ainexus/config`（admin token）。
- **2026-09-08 拆分落地（182.57 实际切换完成）**：
  - nginx 1.14.0 重编译加 `--with-http_ssl_module`（原编译无 SSL，`proxy_pass https://`
    报错），USR2 热切换零中断，8080/7079 存量业务不受影响；nginx.conf 增
    `include conf.d/*.conf;`，`opsguard-llm.conf` 放入并监听 `10.50.182.57:7070`；
  - 按切换顺序执行：停旧服务（释放 7070）→ 换 nginx 二进制 → USR2 → deploy.sh
    装新飞书代理（启动日志 webhook=true app=true urgent_users=5）；
  - 验证全通过：182.57 本机 healthz 全绿、7070 `/v1/models` 200、/notify 群消息
    实发成功；189.6 经网闸 50378 healthz 全绿、50376 `/v1/models` 200；
  - 此后新增模型源/通用上游 = nginx 配置加 server 块 + reload，不再交叉编译
    重发二进制；新加消息协议类型才需要改 Go 代码。
- **2026-09-08 拆分：LLM 反代移交 nginx，internet-proxy 精简为飞书代理**：
  - main.go 删除 `LISTEN_LLM/LLM_UPSTREAM` 反代逻辑；7070 由 nginx server 块
    承接（`nginx-llm.conf`），网闸任务 1731 映射与管理端 base_url 均不变；
  - 此后新增模型源/通用上游 = nginx 配置加 server 块 + reload，不再交叉编译
    重发二进制；新加消息协议类型才需要改 Go 代码。
- **2026-08-21 首次部署（182.57，systemd `opsguard-internet-proxy`）**：
  - 网闸任务 1731（50376→7070）/ 1733（50378→7072）实测连通；
  - 189.6 管理端已配：provider `llm-proxy`（base_url `http://10.60.114.2:50376/v1`）+
    `deepseek-v4-flash`，网关启用，默认模型生效；通知渠道 `feishu-alert-group`（/notify）
    与 `feishu-urgent`（/urgent）+ error/warn/info 三条策略（error 双渠道）；
  - 验收全通过：AI 排查 SSE 对话、/notify 群消息、/urgent 应用消息+5 人加急。
- **教训**：飞书加急接口 `urgent_app` 是 **PATCH**（POST 返回 404 page not
  found），首发版本踩过，已在 main.go 修正；自测/手跑二进制需自行 source
  `/etc/opsguard-internet-proxy.env`（systemd 环境不继承）。
