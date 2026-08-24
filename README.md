# OpsGaurd — 智能运维全链路自动化平台

OpsGaurd 用于在一个管理面统一纳管多个**网络隔离**的 Docker Swarm 集群，覆盖容器编排、监控告警、智能巡检、AI 异常排查、MLOps 与镜像分发全链路。

平台由管理端（单二进制 server + Vue3 前端）与每集群 Worker 组成，只依赖「管理端 → 集群 Worker」的单向 gRPC 通路，集群无需反向访问管理端，隔离网络无需开放反向防火墙洞。

## 核心特性

| 能力域 | 说明 |
| --- | --- |
| 容器编排 | 服务部署 / 更新 / 扩缩容 / 重启 / 删除，异步 operation 就绪判定，滚动更新 |
| 监控告警 | 端口 / HTTP / 日志 / 资源四类周期探针，事件聚合为告警，规则管理与自动通知（飞书 / webhook / 短信） |
| 智能巡检 | YAML 巡检任务 + cron 调度 + AI 巡检报告 + 报告投递 + 异常项自动转告警 |
| AI 异常排查 | 内嵌 AiNexus 网关（OpenAI / Anthropic 兼容端点），ReAct agent + MCP 工具直连各集群 Worker，真实执行运维操作 |
| MLOps | Prompt Hub（版本化提示词）、模型接入与启停、call 级用量计量与费用报表、月度预算 |
| 镜像仓库 | 管理端内嵌 OCI registry（/v2 协议），集群内 docker daemon 经 gRPC 隧道中继 pull/push；页面传包构建 |
| 身份认证 | 本地用户 + SSO（OIDC RP）；OpsGaurd 自身作为 OIDC IdP 向集群内系统（如 r-nacos）发牌 |
| 纳管清单 | 非 swarm 的 standalone 容器 / 宿主机服务的清单化注册与周期探测 |
| 审计安全 | 敏感操作审计回推、命令黑白名单、日志脱敏、Bearer token + 可选 mTLS |

## 设计要点

1. **单向网络策略**——平台通信只依赖「管理端 → 集群 Worker」单向通路。事件回传（`SubscribeEvents`/`SubscribeAudit` 双向流）、集群内系统访问管理端 IdP / 镜像仓库（`Tunnel` 反向隧道）全部由 server 发起承载。
2. **零外部依赖**——无 MySQL / Redis / 消息队列。管理端用嵌入式 LevelDB，Worker 用纯 Go SQLite，部署一套 OpsGaurd 只需要 Docker 本身。
3. **离线优先**——目标环境为无外网内网，安装包、镜像、proto 代码生成（`genpatch`）均有离线路径，见 `deploy/offline/` 部署 runbook。
4. **单二进制管理端**——server 进程同时托管 REST API、内嵌 AI 网关、内嵌 OCI 仓库、内嵌 OIDC IdP 和前端 SPA 静态文件。

## 总体架构

```
                         ┌────────────────────────────────────────────────┐
                         │              管理机（单节点）                    │
   浏览器 ──HTTP/SSE──▶  │  opsguard-server (Go 单二进制)                  │
                         │  ├─ REST API  /api/v1/*                        │
                         │  ├─ SPA 静态托管 (Vue3 dist)                   │
                         │  ├─ AiNexus AI 网关 /ainexus/*                  │
                         │  ├─ 内嵌 OCI 仓库 /v2/*                         │
                         │  ├─ 内嵌 OIDC IdP /api/v1/idp/*                 │
                         │  └─ LevelDB (./data)                            │
                         └───────────┬────────────────────────────────────┘
                                     │  gRPC（server 主动发起，bearer: worker token）
        ┌────────────────────────────┼────────────────────────────┐
        ▼                            ▼                            ▼
 ┌──────────────┐            ┌──────────────┐            ┌──────────────┐
 │ 集群 A Worker │            │ 集群 B Worker │   ……       │ 集群 N Worker │
 │ (swarm global│            │ 每集群一个    │            │              │
 │  每节点一个)  │            │ leader 执行   │            │              │
 │ HTTP :8080   │            │ 写操作        │            │              │
 │ gRPC :9080   │            │              │            │              │
 │ SQLite 队列  │            │              │            │              │
 │ docker.sock  │            │              │            │              │
 └──────────────┘            └──────────────┘            └──────────────┘

 集群内消费方（经 Worker 本地入口，免反向洞）：
   ├─ /idp-proxy/*   → r-nacos 等访问管理端 IdP（走 Tunnel 反向隧道）
   └─ /v2/*          → docker daemon pull/push 管理端内嵌仓库（走 Tunnel 反向隧道）
```

server 始终是 gRPC **客户端**，Worker 是服务端（gRPC `:9080`）；Worker 同时也是 **MCP 服务端**（`/mcp` Streamable HTTP + stdio，20 个运维工具），server 内嵌的 AiNexus 网关经各集群 Worker 的 `/mcp` 调用工具，实现 AI 排查对集群的真实操作。

## 仓库结构

```
├── OpsGaurdWeb/
│   ├── server/        # 管理端 Go 服务（REST API + AI 网关 + OCI 仓库 + OIDC IdP + SPA 托管）
│   └── web/           # 前端 Vue3 SPA
├── Worker/            # 每集群 Worker（Go 静态二进制，swarm global 模式每节点一个）
├── proto/             # opsguard.proto：server ↔ Worker 的 gRPC 契约
├── deploy/            # 离线部署 runbook（offline/）与内网出网代理（internet-proxy/）
└── docs/              # 系统技术总览与各子系统设计方案（中文）
```

## 技术栈

| 组件 | 语言/运行时 | 核心依赖 | 存储 |
| --- | --- | --- | --- |
| server | Go 1.25 | gin、grpc + protobuf、goleveldb、coreos/go-oidc、mcp-go、robfig/cron | LevelDB |
| Worker | Go 1.25 | grpc、modelcontextprotocol/go-sdk、modernc.org/sqlite、自研 Docker Engine/Swarm REST 客户端 | SQLite |
| web | TypeScript 5.7 | Vue 3.5、Vite 6、Element Plus、Pinia、vue-router、axios、js-yaml | — |

## 快速开始

生产形态是 Docker Swarm：管理端自身 `docker run` 单容器（`:8080`），各被管集群以 swarm `global` 模式每节点部署一个 Worker。目标环境通常无外网，离线安装流程（docker 静态包 → swarm init/join → 镜像离线 load）见：

- **`deploy/offline/README.md`** — 离线部署 runbook（含真实集群拓扑与踩坑记录）
- **`docs/OpsGaurd-系统技术总览.md`** — 配置参考（`config.yaml` / `agent-config.yaml` / 环境变量 / 端口矩阵）

有外网环境下也可以直接构建镜像部署：`OpsGaurdWeb/deploy/Dockerfile`（管理端，产物内含前端 dist）、`Worker/deploy/Dockerfile`（Worker）。

## 文档（docs/）

- [OpsGaurd 系统技术总览](docs/OpsGaurd-系统技术总览.md) —— 架构、通信、存储、安全、部署全景（从这里开始读）
- [OpsGaurd 场景操作手册](docs/OpsGaurd-场景操作手册.md)
- [Worker 设计方案](docs/Worker-设计方案.md) / [OpsGaurdWeb 设计方案](docs/OpsGaurdWeb-设计方案.md)
- [AiNexus Skill 与 MCP 设计方案](docs/AiNexus-Skill与MCP-设计方案.md)
- [MLOps 方案](docs/MLOps-方案.md) / [注册配置中心 r-nacos 方案](docs/注册配置中心-r-nacos-方案.md)
- [K8s 替换方案](docs/K8s替换方案.md) / [K8s 迁移前置整改计划](docs/K8s迁移前置-服务间调用统一整改计划.md)
- [镜像隧道中继方案](docs/镜像隧道中继方案.md) / [集群纳管清单方案](docs/集群纳管清单方案.md) / [healthy 跨节点汇聚方案](docs/healthy跨节点汇聚方案.md)
- [IdP 接入指南](docs/IdP-接入指南.md)

## 开发与构建

- **proto 变更**：`proto/gen.sh` 双端生成；环境无 protoc / 无 module proxy 时用 `Worker/cmd/genpatch` 离线打补丁。
- **Worker 构建**：`Worker/deploy/Dockerfile`（CGO_ENABLED=0 静态编译），版本号经 `-ldflags` 注入。
- **server 构建**：`OpsGaurdWeb/deploy/Dockerfile`，产物内含 `web/dist`；前端独立 `npm run build`（vue-tsc 校验）。
- **前端开发**：`web/` 下 Vite dev server（5173），`/api` 代理到 `http://localhost:8090`。

## 许可证

[GNU Affero General Public License v3.0](LICENSE)

## 镜像仓库

- Gitee：https://gitee.com/legeosoft_legendzhu/OpsGaurd
- GitHub：https://github.com/Legend-Zhu/OpsNexus
