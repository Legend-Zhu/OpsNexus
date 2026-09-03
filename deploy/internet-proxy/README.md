# internet-proxy —— 内网管理端的受控出网代理

部署在互联网服务器 **10.50.182.57** 上，为无外网的管理端（**10.60.189.6**，
opsguard-server）经网闸（**10.60.114.2**）提供两类出网能力：

```
189.6 (opsguard-server)        网闸 10.60.114.2           182.57 (internet-proxy)
┌────────────────────┐    ┌─ 50376 ─→ 7070 ─┐    ┌─────────────────────────────┐
│ 模型 provider       │───▶│                 │───▶│ LLM 反代 → 82.156.119.58:5002│
│ base_url=:50376/v1  │    │                 │    │ （OpenAI 兼容多模型网关）    │
│                    │    ├─ 50378 ─→ 7072 ─┤    ├─────────────────────────────┤
│ 通知渠道(飞书)      │───▶│                 │───▶│ /notify → 群机器人(加签)     │
│ proxy=:50378/notify│    │                 │    │ /urgent → 应用+加急(5人)     │
└────────────────────┘    └─────────────────┘    └─────────────────────────────┘
```

## 端口分配（网闸任务 1731-1736）

| 任务号 | 网闸端口 | 182.57 端口 | 用途 |
| --- | --- | --- | --- |
| 1731 | 50376 | 7070 | LLM 反代 → `https://82.156.119.58:5002`（自签证书，`LLM_INSECURE=true`） |
| 1732 | 50377 | 7071 | 预留（备用模型源） |
| 1733 | 50378 | 7072 | 飞书通知代理（`/notify` 机器人 + `/urgent` 应用加急） |
| 1734-1736 | 50379-81 | 7073-75 | 预留 |

LLM 网关可用模型（2026-08 实测 `/v1/models`）：`deepseek-v4-flash`（本次采用）、
`deepseek-v4-pro`、`deepseek-v3.2`、`glm-5` / `glm-5.1` / `glm-5.2`、`kimi-k2.5/2.6`、
`qwen-3.6` 等，后续加模型只改管理端页面，代理不动。

## 组成

| 文件 | 说明 |
| --- | --- |
| `main.go` | 单文件 Go（纯 stdlib，无第三方依赖）：LLM 反代 + 飞书代理 |
| `proxy.env.example` | 配置模板（真实 `proxy.env` 含凭据，已 gitignore） |
| `deploy.sh` | 在 182.57 上安装 systemd 服务 `opsguard-internet-proxy` |
| `Dockerfile` / `docker-compose.yml` | 可选容器化路径 |

飞书凭据全部持有在代理侧（公网凭据不入内网 LevelDB）：

- 群机器人 webhook + 加签 secret → `/notify`，对应**所有级别**推送
- 飞书应用（app_id/secret）+ 群 chat_id + 值班 user_id ×5 → `/urgent`，
  发应用群消息并**应用内加急**对应值班用户，只绑 **error** 级
  （OpsGaurd 告警级别只有 error/warn/info，error 即最高级）

## 部署（182.57）

开发机交叉编译（产物在仓库根 `.deploytmp/`，已 gitignore）：

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

**暴露面（重要）**：proxy.env 的 `LISTEN_*` 绑定 `10.50.182.57`（网闸侧内网
IP），不监听公网；若宿主机没有绑定该 IP 的网卡（如容器模式），退回
`0.0.0.0` + 防火墙只对网闸/内网放行 7070-7075。网闸侧已限源 189.6。

**验证（182.57 本机）**：

```bash
curl http://10.50.182.57:7072/healthz            # 配置就绪状态
curl http://10.50.182.57:7070/v1/models \
  -H 'Authorization: Bearer <llm-key>'           # LLM 链路
# 自测走服务端口即可（直跑二进制不继承 systemd 的 EnvironmentFile）：
curl -X POST http://10.50.182.57:7072/notify -H 'Content-Type: application/json' \
  -d '{"channel_type":"feishu","content":"测试"}' # 群里应收到消息
# 卡片样式（代理 ≥ 2026-09 版本）：请求体带可选 "card" 对象即以
# msg_type=interactive 发送（/notify 直发卡片；/urgent 应用消息 content 传
# 卡片 JSON 字符串）。不带 card 时保持纯文本，旧版管理端不受影响。
curl -X POST http://10.50.182.57:7072/notify -H 'Content-Type: application/json' \
  -d '{"channel_type":"feishu","content":"文本兜底","card":{"header":{"title":{"tag":"plain_text","content":"告警 - 资源超限"},"template":"red"},"elements":[{"tag":"div","text":{"tag":"lark_md","content":"**摘要:** cpu usage 98.6% >= 85%"}}]}}'
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

- 异常排查页发起对话：确认**逐字流式**输出（SSE 过网闸正常，无缓冲）。
- 触发一条 error 告警（如临时把 CPU 阈值调到 1%）：群里收到机器人消息 +
   应用消息，值班用户收到加急 buzz；通知中心「发送记录」两条均 success。
- 触发一条 warn 告警：只有机器人消息，无加急。
- 智能巡检配「报告模型」出 AI 报告。

## 安全说明

- 网闸段（189.6→114.2）是明文 HTTP，LLM key 经 Authorization 头过网闸；
  网闸已限源 189.6，内网段可接受。要更强需两端 TLS，成本高，暂不做。
- 飞书 webhook / 加签 secret / 应用 secret / LLM key 只存在于
  182.57 的 `/etc/opsguard-internet-proxy.env`（0600）与本仓 gitignore 的
  `proxy.env`，不进 git、不进管理端 LevelDB。
- 代理不支持 `channel_type=webhook` 透传，避免被当作任意 URL 跳板（SSRF）。

## 运维

```bash
systemctl status opsguard-internet-proxy          # 运行状态
journalctl -u opsguard-internet-proxy -f          # 访问日志 / 失败详情
# 改配置：编辑 /etc/opsguard-internet-proxy.env && systemctl restart opsguard-internet-proxy
# 升级二进制：systemctl stop → install -m 0755 <new> /opt/opsguard/internet-proxy/internet-proxy → start
#（直接覆盖运行中的二进制会 text file busy）
```

## 部署记录

- **2026-08-21 首次部署（182.57，systemd `opsguard-internet-proxy`）**：
  - 网闸任务 1731（50376→7070）/ 1733（50378→7072）实测连通；
  - 189.6 管理端已配：provider `llm-proxy`（base_url `http://10.60.114.2:50376/v1`）+
    `deepseek-v4-flash`，网关启用，默认模型生效；通知渠道 `feishu-alert-group`（/notify）
    与 `feishu-urgent`（/urgent）+ error/warn/info 三条策略（error 双渠道）；
  - 验收全通过：AI 排查 SSE 对话、/notify 群消息、/urgent 应用消息+5 人加急。
- **教训**：飞书加急接口 `urgent_app` 是 **PATCH**（POST 返回 404 page not
  found），首发版本踩过，已在 main.go 修正；自测/手跑二进制需自行 source
  `/etc/opsguard-internet-proxy.env`（systemd 环境不继承）。
