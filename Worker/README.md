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
- **MCP**：Streamable HTTP（`/mcp`，协议 `2026-07-28`）+ 12 个工具 + 3 个资源，
  供 LLM Agent 部署/查询/操作服务。`-mcp-stdio` 本地模式。
- **私有仓库**：`registryAuth`（inline 或 swarm secret）经 `X-Registry-Auth` 传引擎；
  `imagePullPolicy=always` 预拉取。
- **安全**：日志脱敏（env/auth/secret 值 → `[REDACTED]`）；写操作要求 manager 节点
  （swarm control-plane）；TLS 可选（`-tls-cert/-tls-key`）。

## Build

```sh
# needs Go 1.25+ (or build via Docker)
go build -o worker ./cmd/worker

# container image
docker build -t opsguard-worker:1.0.0 -f deploy/Dockerfile .
```

## Run

```sh
# inside a swarm (hostname == swarm node hostname), on a manager node
./worker -addr :8080

# containerized (recommended): mount the docker socket
docker run -d --name worker \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -p 8080:8080 opsguard-worker:1.0.0

# swarm-wide deployment
docker stack deploy -c deploy/stack.yml opsguard

# MCP over stdio (local agent, e.g. Claude Desktop / cursor)
./worker -mcp-stdio
```

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
| GET | `/api/v1/self` | This node's swarm role |
| GET | `/healthz` | Liveness |

## MCP

Endpoint `POST /mcp` (Streamable HTTP, protocol `2026-07-28`). Tools:
`deploy_service`, `update_service`, `scale_service`, `restart_service`,
`remove_service`, `list_services`, `get_service`, `get_service_logs`,
`get_events`, `get_operation`, `list_nodes`, `get_node`, `get_self`.
Destructive ops require `confirm=true`.

## Project layout

```
cmd/worker/        entrypoint
internal/config/   config schema + validation
internal/docker/   Docker Engine REST API client (stdlib, no SDK)
internal/orchestrator/  lifecycle + operation tracking + HTTP API
internal/monitor/  port/http/log/resource checks + event store
internal/mcp/      MCP server (go-sdk, 2026-07-28)
internal/logging/  slog redaction
deploy/            Dockerfile + swarm stack
```

## Roadmap (see design doc §九)

- [x] P0 scaffold · P1 orchestration API · P2 monitoring · P3 MCP
- [x] P4 HA awareness (self role, manager guard) · P5 hardening (redaction, TLS, stack)
- [ ] OAuth 2.1 on `/mcp`, per-node stats aggregation, webhook push, audit log
