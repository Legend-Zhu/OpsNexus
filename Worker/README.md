# OpsGaurd Worker

Node agent for the OpsGaurd intelligent-ops platform: a Rancher-like container
orchestrator for **Docker Swarm**, plus health monitoring and an MCP server for
LLM agents. Design doc: `docs/Worker-设计方案.md`.

## Features

- **编排**：`POST /api/v1/services`（config YAML/JSON）→ 拉镜像 → 创建服务 →
  异步就绪轮询（task running + healthcheck healthy）→ operation 状态查询。
  Update/Scale/Restart/Remove 全生命周期，滚动更新、失败回滚语义由 swarm 驱动。
- **监控**：按 config 的 `monitoring` 块跑四类探针——端口连通（TCP）、接口健康
  （HTTP 状态码/正则 body）、日志异常（ServiceLogs 流 + 正则 + 去抖）、资源使用
  （容器 stats 差值 CPU%/内存%）。事件入内存 ring buffer，`GET /api/v1/events` 查询。
- **MCP（16 工具）**：Streamable HTTP（`/mcp`，协议 `2026-07-28`，stateless）。
  编排类 list/get/deploy/update/scale/restart/remove、get_service_logs、get_events、
  get_operation、list/get_node、get_self；**命令执行** `exec_in_container`（跨节点路由）
  与 `exec_host_command`（nsenter 宿主机，跨节点广播）；**指标** `get_resource_usage`
  （跨节点聚合）。`-mcp-stdio` 本地模式。
- **命令执行 + 黑白名单**：SSH 类入口（宿主机 + 容器内），策略由每节点
  `agent-config.yaml` 下发（`mode: blacklist|whitelist`、内置危险命令、超时、开关）。
  高风险动作强制 `confirm=true`。
- **每节点 worker（global）**：每台纳管服务器部署，提供本地接口
  `/api/v1/local/stats` `/exec` `/host` `/logs`（**SSE 流式日志**，等价
  `docker service logs -f`）；manager 跨节点代理实现 stats 聚合 / exec 路由 / host 广播。
- **私有仓库**：`registryAuth`（inline 或 swarm secret）经 `X-Registry-Auth` 传引擎；
  `imagePullPolicy=always` 预拉取。
- **安全**：日志脱敏（env/auth/secret 值 → `[REDACTED]`）；写操作要求 manager 节点
  （swarm control-plane）；宿主机执行需显式开启 `allowHostExec`；Bearer token 鉴权
  （`auth.tokens` + OAuth metadata 端点）；**mTLS 双向**（`-tls-ca` 强制客户端证书）；
  **审计日志**（编排/命令打点，`GET /api/v1/audit`，可 webhook 推送）。

## Build

```sh
# needs Go 1.25+ (or build via Docker)
go build -o worker ./cmd/worker

# container image
docker build -t opsguard-worker:1.0.0 -f deploy/Dockerfile .
```

## Run

```sh
# inside a swarm, on a manager node (orchestration + MCP + local node API)
./worker -addr :8080

# node-role worker (per-node stats/exec/host/logs only)
./worker -addr :8080 -agent-config /etc/opsguard/agent-config.yaml

# containerized: mount docker socket + agent config; privileged+pid=host for
# host command execution (nsenter). Remove for reduced security (no host exec).
docker run -d --name worker \
  --privileged --pid=host \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v /etc/opsguard/agent-config.yaml:/etc/opsguard/agent-config.yaml:ro \
  -p 8080:8080 opsguard-worker:1.0.0 -agent-config /etc/opsguard/agent-config.yaml

# swarm-wide deployment (global: one worker per node)
docker stack deploy -c deploy/stack.yml opsguard

# MCP over stdio (local agent, e.g. Claude Desktop / cursor)
./worker -mcp-stdio
```

## Agent config (command policy)

Every node mounts `agent-config.yaml` (template: `deploy/agent-config.yaml.example`)
governing command execution: `mode: blacklist|whitelist`, built-in dangerous
commands, `allowHostExec` (default off), `allowContainerExec`, `timeout`.

## Config

A service config is a YAML/JSON document with `service:` and `monitoring:`
blocks — see `docs/Worker-设计方案.md` §4.2 for the full schema. Example:

```yaml
service:
  name: web
  image: nginx:alpine
  replicas: 3
  ports:
    - { published: 8080, target: 80, mode: ingress }
  resources:
    limits: { cpu: "1.0", memory: "512Mi" }
  healthcheck:
    test: ["CMD-SHELL", "wget -qO- http://localhost/ >/dev/null 2>&1 || exit 1"]
    interval: 5s
    timeout: 3s
    retries: 3
    startPeriod: 5s
monitoring:
  enabled: true
  portChecks:
    - { port: "8080", interval: 10s, timeout: 3s }
  httpChecks:
    - { url: "http://localhost:8080/health", expectedStatus: [200], interval: 15s }
  logChecks:
    - { pattern: "ERROR|panic", action: alert }
  resourceThresholds:
    - { metric: cpu, threshold: 80 }
    - { metric: memory, threshold: 85 }
```

## HTTP API

**Orchestration (manager-role):**

| Method | Path | Description |
|---|---|---|
| POST | `/api/v1/services` | Deploy (config body) |
| GET | `/api/v1/services` | List (`?label=`) |
| GET | `/api/v1/services/{name}` | Detail + tasks + health |
| POST | `/api/v1/services/{name}` | Update |
| DELETE | `/api/v1/services/{name}` | Remove |
| POST | `/api/v1/services/{name}/scale` | Scale `{replicas:N}` |
| POST | `/api/v1/services/{name}/restart` | Force re-create tasks |
| GET | `/api/v1/operations[/{id}]` | Operation tracking |
| GET | `/api/v1/events` | Monitoring events (`?service=&type=&limit=`) |
| GET | `/api/v1/audit` | Audit log (lifecycle + command executions, `?action=&limit=`) |
| GET | `/api/v1/self` | This node's swarm role |
| GET | `/.well-known/oauth-protected-resource` | OAuth 2.1 resource metadata (public) |
| GET | `/healthz` | Liveness (public) |

> **Auth**: when `auth.enabled=true` (agent config), every endpoint above except
> the public ones requires `Authorization: Bearer <token>` (token name becomes
> the audit actor). Tokens can be distributed centrally via `WORKER_TOKENS`.

**Per-node local (every node; used by the manager for cross-node calls):**

| Method | Path | Description |
|---|---|---|
| GET | `/api/v1/local/stats` | Local swarm container CPU%/Mem% |
| POST | `/api/v1/local/exec` | Exec in a container `{container|service+slot, command}` |
| POST | `/api/v1/local/host` | Host command (nsenter; policy-gated) |
| GET | `/api/v1/local/logs` | **SSE log stream** `?service=&follow=&tail=&since=` |

## MCP

Endpoint `POST /mcp` (Streamable HTTP, protocol `2026-07-28`, stateless). 16 tools:
`deploy_service`, `update_service`, `scale_service`, `restart_service`,
`remove_service`, `list_services`, `get_service`, `get_service_logs`,
`get_events`, `get_operation`, `list_nodes`, `get_node`, `get_self`,
`get_resource_usage` (cross-node aggregate), `exec_in_container` (cross-node
route), `exec_host_command` (host nsenter, cross-node broadcast).
Destructive/high-risk ops (`remove_service`, `scale=0`, both exec tools)
require `confirm=true`.

## Project layout

```
cmd/worker/        entrypoint (agent config + role detection + HTTP/MCP/monitor/nodeagent)
internal/agent/    agent config + command policy (blacklist/whitelist) + nsenter host exec
internal/config/   service config schema + validation
internal/docker/   Docker Engine REST API client (stdlib, no SDK) + log framing decoder
internal/orchestrator/  lifecycle + operation tracking + HTTP API + cross-node proxy
internal/monitor/  port/http/log/resource checks + event store
internal/nodeagent/     per-node local API (stats/exec/host/SSE logs)
internal/mcp/      MCP server (go-sdk, 2026-07-28) + exec/resource tools
internal/logging/  slog redaction
deploy/            Dockerfile + swarm stack (global, privileged) + agent-config template
```

## Roadmap (see design doc §九)

- [x] P0 scaffold · P1 orchestration API · P2 monitoring · P3 MCP
- [x] P4 per-node workers + cross-node proxy (stats/exec/host) · P5 hardening
      (redaction, TLS, command policy, SSE logs, bearer auth + OAuth metadata,
      audit log, leader-failover write proxy, env-based config distribution)
- [ ] OAuth 2.1 authorization-code flow (external AS), webhook push for audit,
      `subscriptions/listen`, mTLS two-way, swarm autolock
