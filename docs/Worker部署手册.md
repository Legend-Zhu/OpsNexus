# OpsGaurd Worker 部署手册

> 适用对象：OpsGaurd Worker（节点代理）。读者为负责把被管集群接入 OpsGaurd
> 平台的实施/运维人员。设计背景见 `docs/Worker-设计方案.md`，平台整体架构
> 见 `docs/OpsGaurd-系统技术总览.md`，离线环境实战记录见 `deploy/offline/README.md`。

## 1. Worker 是什么

Worker 是部署在**每台被纳管服务器**上的节点代理（Go 编写，单静态二进制），职责：

- **容器编排**：基于 Docker Swarm 的类 Rancher 编排器（部署/更新/扩缩/重启/删除服务）；
- **健康监控**：端口/HTTP/日志/资源四类探针，事件写入本地 SQLite 持久队列；
- **MCP Server**：`/mcp` 暴露 16 个工具（编排、日志、事件、命令执行、资源聚合），
  供平台 LLM Agent 调用；
- **命令执行**：容器内 exec 与宿主机命令（nsenter，默认关闭）的受管控入口。

**角色**（`role: auto` 时按本机 swarm 角色自动推导）：

| 角色 | 所在节点 | 提供能力 |
|---|---|---|
| manager | swarm manager 节点 | 编排 + MCP + gRPC 管理 API（管理端入口） |
| node | swarm worker 节点 | 仅本地 stats/exec/host/logs API，供 manager 跨节点代理 |

**关键架构点（部署前必读）**：

1. **通信方向是"管理端 → Worker"**。Worker 不主动注册、不发心跳；管理端
   （OpsGaurdWeb server）作为 gRPC 客户端主动连接 Worker 的 gRPC 端口拉取
   事件/审计流并下发管理调用（`SubscribeEvents`/`SubscribeAudit`/`Tunnel` 等），
   事件经 ack 游标续传，连接活性即心跳。因此**防火墙只需放行
   管理端 → Worker:gRPC 端口的单向访问**。
2. **不依赖 MySQL/Redis/MQ/etcd**。持久化用本地 SQLite（事件/审计队列），
   唯一外部依赖是本机 Docker daemon（`/var/run/docker.sock`）。
3. **反向隧道**：Worker 上的 `/idp-proxy/`（IdP 反向访问）与 `/v2/`（镜像
   pull/push 中继到管理端内嵌 OCI 仓库）经 gRPC `Tunnel` 双向流走，流池默认
   16 条，**两端 `OPSGUARD_TUNNEL_POOL` 必须一致**。

**部署流程总览**——本文档覆盖 ①–⑥ 与 ⑧，照做完成后到页面添加集群即可；
⑦ 为前提条件（管理端 server 自身的部署不在本文范围）：

| 步骤 | 内容 | 位置 |
|---|---|---|
| ① | 分发离线包到目标节点 | §3.2 |
| ② | 安装 Docker（含 daemon.json 核对） | §4.1 |
| ③ | 初始化 / 加入 Swarm | §4.2 |
| ④ | 配置 Worker Token 与 agent-config.yaml | §4.3–4.4 |
| ⑤ | 导入镜像、部署 worker stack | §4.5–4.6 |
| ⑥ | Worker 侧自检 | §6.1 |
| ⑦ | （前提）管理端 OpsGaurdWeb server 已部署、可登录页面 | `deploy/offline/README.md` |
| ⑧ | 页面添加集群 | §5 |

最终验收逐项打勾见 §6.4 端到端验收清单。

## 2. 前置条件

### 2.1 系统要求

| 项目 | 要求 |
|---|---|
| OS | Linux（离线环境验证过 CentOS/统信类），内核支持 overlay2 |
| Docker | 27.x（离线包 `deploy/offline/bundle/docker-27.5.1.tgz` + `install-docker*.sh`） |
| Swarm | 已 `docker swarm init`（manager）/ `swarm join`（node）——角色由 swarm 推导 |
| 资源 | 内存限额建议 ≥256M（stack 模板即 256M）；磁盘为 `/var/lib/opsguard` 预留数 GB（事件/审计 + 镜像 blob 缓存） |
| Worker 镜像 | `opsguard-worker:<tag>`（离线 tar 在 `deploy/offline/bundle/`） |

### 2.2 端口规划

| 端口 | 协议 | 用途 | 谁访问 |
|---|---|---|---|
| 8080（默认，可改） | HTTP | `/mcp`（MCP Streamable HTTP）+ `/healthz` + 节点 local API（stats/exec/host/logs） | 管理端、LLM Agent、同集群 worker |
| 9080（默认，可改） | gRPC | `ManagementService` 管理 API（唯一 server↔worker 管理通道） | **管理端 server** |

> 生产环境若与同机其他服务端口冲突需错开，参考既有部署：
> 单机环境用 **8090(HTTP)/9090(gRPC)**（避让管理端页面 8080），灾害集群用
> **6060/6061**。端口以 stack 的 `-addr`/`-grpc-addr` 启动参数为准。

**防火墙/网络策略**：

- 放行：管理端 server → 每个**被管集群 swarm manager 节点**的 gRPC 端口（如 6061）。
  跨网段隔离时需路由/NAT 映射（灾害集群即 171.x → 232:6061 的单向放行）。
- 同集群节点间：worker 跨节点代理（stats 聚合/exec 路由/日志流）走节点间
  HTTP 端口互通。
- **Worker → 管理端方向不需要任何入站放行**。

### 2.3 时钟同步

所有节点与管理端必须 NTP 对时。曾有节点时钟漂移导致 TLS x509 校验失败的
实战案例（`deploy/offline/README.md`）。

## 3. 构建与打包

### 3.1 本地构建（可选）

Worker 是 Go 1.25 module（`Worker/go.mod`），产物为**无 CGO 静态二进制**或容器镜像。

```sh
cd Worker

# 本地编译（Linux 交叉编译，离线环境实测可用）
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
  -ldflags "-s -w -X gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/version.Version=1.2.7" \
  -o worker ./cmd/worker

# 容器镜像（多阶段：golang:1.25-alpine 构建 → alpine:3.20 运行）
docker build -t opsguard-worker:1.2.7 -f deploy/Dockerfile .

# 离线分发：导出镜像 tar（注意 OCI 与 legacy 格式差异，见 §7 常见问题）
docker save opsguard-worker:1.2.7 -o opsguard-worker-1.2.7.tar
```

版本号经 `-ldflags -X ...internal/version.Version=...` 注入，启动日志会打印
`worker starting version=1.2.7`，用于升级后核对。

> **proto 变更**需在双端重新生成代码（`proto/gen.sh`；完全离线时用
> `Worker/cmd/genpatch` 打补丁）。只做部署不需关心。

### 3.2 分发准备（离线包 → 目标节点）

不做本地构建时（离线常态），直接用 `deploy/offline/bundle/` 里的现成镜像
tar。目标节点统一放 `/opt/opsguard/`（与既有环境一致的惯例路径）：

| 目标节点 | 目标路径 | 文件 | 来源 |
|---|---|---|---|
| 所有节点 | `/opt/opsguard/offline/` | `docker-27.5.1.tgz`、`docker.service`、`containerd.service`、`daemon.json` | `deploy/offline/` |
| manager 节点 | `/opt/opsguard/offline/` | `install-docker.sh` | `deploy/offline/` |
| node 节点 | `/opt/opsguard/offline/` | `install-docker-noinit.sh` | `deploy/offline/` |
| 所有节点 | `/opt/opsguard/images/` | `opsguard-worker-<ver>.tar` | `deploy/offline/bundle/` |
| 所有节点 | `/etc/opsguard/` | `agent-config.yaml`（按 §4.4 改好后分发） | 底稿 `deploy/offline/agent-config*.yaml` |
| manager 节点 | `/opt/opsguard/` | `stack.yml`（按 §4.6 改好后） | 底稿 `deploy/offline/stack*.yml` |

```sh
# 示例：在分发机/跳板机上推送到某节点
ssh root@<node-ip> 'mkdir -p /opt/opsguard/offline /opt/opsguard/images /etc/opsguard'
scp deploy/offline/{docker-27.5.1.tgz,docker.service,containerd.service,daemon.json,install-docker.sh} \
  root@<node-ip>:/opt/opsguard/offline/
scp deploy/offline/bundle/opsguard-worker-1.2.7.tar root@<node-ip>:/opt/opsguard/images/
scp agent-config.yaml root@<node-ip>:/etc/opsguard/agent-config.yaml
scp stack.yml root@<manager-ip>:/opt/opsguard/stack.yml
```

## 4. 安装步骤

以下按"一个被管集群（swarm）"为单位执行。单集群多节点时，步骤 4.1–4.5
**每台节点都要做**，4.6 只在 manager 节点执行一次。

### 4.1 安装 Docker（离线脚本）

分发包在 `deploy/offline/`：`docker-27.5.1.tgz` 静态包 + `install-docker.sh` /
`install-docker-noinit.sh` + `docker.service` / `containerd.service` /
`daemon.json`。把 tgz 与脚本放同一目录后执行：

```sh
# manager 节点：安装 docker，装完顺带执行 docker swarm init
bash install-docker.sh docker-27.5.1.tgz

# node 节点：只装 docker，不 init（装完按 §4.2 join）
bash install-docker-noinit.sh docker-27.5.1.tgz
```

脚本动作：解压静态包到 `/usr/local/bin` → 安装 systemd 单元 → 复制
`/etc/docker/daemon.json` → 启动 containerd/dockerd（→ manager 上
`swarm init`，已初始化则跳过）。

**daemon.json 两项必核对**（离线模板按单机环境写的，需按环境修改）：

```json
{
  "insecure-registries": ["<镜像拉取源地址>"],
  "log-driver": "json-file",
  "log-opts": { "max-size": "10m", "max-file": "3" }
}
```

- **`insecure-registries`**：凡以 HTTP 明文拉取的镜像源都要列出。跨网段集群
  走隧道中继时，**填中继点——swarm manager 上 worker 的 HTTP 地址**（如
  `10.60.171.232:6060`），且全集群各节点统一指同一地址，保证镜像引用一致；
  可直连管理端的形态填管理端地址（单机环境模板即 `10.60.189.6:8080`）。
  修改后 `systemctl restart docker`，用 `docker info | grep -i insecure` 核对。
- **`log-opts`**：容器日志轮转上限，防止业务日志撑爆磁盘，保持默认即可。

### 4.2 初始化 / 加入 Swarm

Worker 的 manager/node 角色由本机 swarm 角色推导，**swarm 拓扑即 worker 的
部署拓扑**：

```sh
# ① manager 节点：初始化（advertise-addr 填节点间互通网卡 IP）
docker swarm init --advertise-addr 10.60.171.232

# ② manager 上取 join token
docker swarm join-token worker     # node 节点用
docker swarm join-token manager    # 追加 manager（HA）用

# ③ 各 node 节点加入（2377 为集群管理端口）
docker swarm join 10.60.171.232:2377 --token <worker-token>

# ④（可选）追加 manager 实现 HA：用 manager token 执行同样的 join 命令，
#    或事后 docker node promote <node>。多 manager 时编排写操作由 leader
#    处理（worker 会自动转发到 leader）。
```

**节点间防火墙放行**（swarm 自身要求）：`2377/tcp`（集群管理）、
`7946/tcp+udp`（gossip）、`4789/udp`（overlay 网络 VXLAN）。

**验证**（manager 上执行）：`docker node ls` 应看到所有节点 `Status=Ready`
且 manager 带 `Leader` 标记。

已有 Docker 的环境跳过安装脚本，但需确认节点已加入目标 swarm
（`docker info | grep -i swarm`）。

### 4.3 配置 Worker Token（鉴权）

生产环境**必须**开启 Bearer token 鉴权：不开则 worker 的 gRPC/HTTP/MCP 端口
全部裸奔，而 MCP 里包含命令执行类工具。

**① 生成 token**（任一台安全机器上）：

```sh
openssl rand -hex 24
```

**② 配置到每台节点的 `/etc/opsguard/agent-config.yaml`**（配置文件全貌见 §4.4）：

```yaml
auth:
  enabled: true
  tokens:
    opsguard-server: "<openssl 生成的 secret>"
```

- map 的 key（`opsguard-server`）是 **token 名**，会作为审计日志里的 actor
  记录，取一个能识别调用来源的名字；
- **同一集群所有节点用同一份 token**（管理端只连 manager 上的 worker，但配置
  文件是每节点一份，保持一致便于维护）；
- HTTP（`/mcp`、local API）与 gRPC 共用同一 token 集，调用方带
  `Authorization: Bearer <secret>`（gRPC 为 metadata `authorization`）。

**③ 与管理端保持一致**：§5 注册集群时填的 token 必须与此 secret 完全一致，
否则注册探测直接失败（502）。集群已注册后，管理端可用
`PUT /api/v1/clusters/:name` 更新 token（该接口不重探测，改完需手动验证连通）。

**替代方式**：不改文件、用环境变量下发——
`WORKER_TOKENS="opsguard-server=<secret>"`（设置即自动启用 auth，无需再写
`auth.enabled: true`），适合配置统一下发的场景。

**④ 轮换**（怀疑泄露或定期）：

1. 各节点更新 `agent-config.yaml` 里的 secret，`docker stack deploy` 滚动重启
   worker（环境变量方式则更新 `WORKER_TOKENS` 重新部署）；
2. 管理端 `PUT /api/v1/clusters/:name` 更新为新 token；
3. 集群详情页确认在线、事件流恢复。

两步之间会出现短暂不匹配（事件流断开、探测失败），管理端会自动重连；窗口期
事件缓存于 worker 本地 SQLite 队列，恢复后带游标续传，不丢失。

### 4.4 准备 agent-config.yaml

**每台节点**放置 `/etc/opsguard/agent-config.yaml`（stack 会以只读方式挂进
容器）。模板：`Worker/deploy/agent-config.yaml.example`；生产实例：
`deploy/offline/agent-config.disaster.yaml`。

```yaml
worker:
  role: auto                    # auto|manager|node，按 swarm 角色自动推导
  listen: ":6060"               # HTTP（/mcp+/healthz+local API）
  grpcListen: ":6061"           # gRPC 管理 API，管理端连这里
  dataDir: "/var/lib/opsguard"  # 事件/审计 SQLite 队列（须挂卷持久化）

auth:
  enabled: true                 # 生产必须开启，否则 gRPC/HTTP/MCP 全裸奔
  tokens:
    opsguard-server: "<随机长 secret>"   # token 名会作为审计 actor 记录

commandPolicy:
  mode: blacklist               # blacklist|whitelist
  allowHostExec: false          # 宿主机命令执行（nsenter），默认关闭，按需开启
  allowContainerExec: true
  timeout: 30s
  blacklist:                    # 与内置危险命令集（rm -rf /、shutdown、mkfs…）合并
    - "curl http://|sh"
    - "wget http://|sh"
```

要点：

- token 的生成、与管理端的一致性、轮换流程见 §4.3。
- 已废弃字段 `webhooks` 不用配——事件改走 gRPC 流回推。
- `allowHostExec: true` 要求容器 `privileged + pid: host`，且 host 命令有额外
  风险，仅在明确需要时开启。

### 4.5 准备数据目录与镜像

每台节点：

```sh
mkdir -p /var/lib/opsguard

# 导入离线镜像（在线环境改为 docker pull 或从管理端内嵌仓库拉取）
docker load -i opsguard-worker-<ver>.tar
```

### 4.6 编写 stack 并部署

stack 模板：`Worker/deploy/stack.yml`；生产实例：
`deploy/offline/stack.yml`（单机）/ `stack.disaster.yml` / `stack.azbx.yml`。
核心形态为 **`mode: global`（每节点一个 worker）+ 挂载 docker.sock/配置/数据目录**：

```yaml
version: "3.8"
services:
  worker:
    image: opsguard-worker:1.2.7        # 与导入镜像 tag 一致
    hostname: "{{.Node.Hostname}}"
    privileged: true
    pid: host
    deploy:
      mode: global
      restart_policy: { condition: any, delay: 5s, max_attempts: 3 }
      resources: { limits: { memory: 256M } }
    environment:
      - DOCKER_HOST=unix:///var/run/docker.sock
      # IdP 反向隧道基址：集群内服务经 worker /idp-proxy/ 访问管理端 IdP，
      # 填集群内可达的 worker HTTP 地址
      - OPSGUARD_TUNNEL_BASE=http://<manager-ip>:6060
      # - OPSGUARD_TUNNEL_POOL=16          # 隧道流池，两端须一致，默认 16
      # - OPSGUARD_IDP_PUBLIC_ISSUER=http://<idp-ip>:8080
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
      - /etc/opsguard/agent-config.yaml:/etc/opsguard/agent-config.yaml:ro
      - /var/lib/opsguard:/var/lib/opsguard
    command: ["-agent-config", "/etc/opsguard/agent-config.yaml",
              "-addr", ":6060", "-grpc-addr", ":6061"]
    ports:
      - { mode: host, published: 6060, target: 6060 }   # HTTP，host 模式不走 ingress 网格 DNAT，生产栈同款
      - { mode: host, published: 6061, target: 6061 }   # gRPC（管理端连这里）
    logging:
      driver: json-file
      options: { max-size: "10m", max-file: "3" }
```

在 **manager 节点**部署：

```sh
docker stack deploy -c stack.yml opsguard
```

**注意：swarm 会静默忽略 `privileged` 和 `pid: host`**。global stack 形态下
宿主机进程页只能看到 worker 容器内进程；若需要真正的宿主机全量进程
（`allowHostExec: true` 场景），改用普通容器方式部署（§4.7）。

### 4.7 替代部署形态：docker run 单容器

不用 swarm stack 时（或需要 host exec 能力时），在每台节点：

```sh
docker run -d --name worker \
  --privileged --pid=host --network host --restart unless-stopped \
  -v /var/run/docker.sock:/var/run/docker.sock:ro \
  -v /etc/opsguard/agent-config.yaml:/etc/opsguard/agent-config.yaml:ro \
  -v /var/lib/opsguard:/var/lib/opsguard \
  opsguard-worker:1.2.7 \
  -agent-config /etc/opsguard/agent-config.yaml -addr :6060 -grpc-addr :6061
```

`--privileged --pid=host` 仅为宿主机命令执行（nsenter）所需；不需要 host exec
时去掉这两项以收窄权限。

### 4.8 启动参数速查

| 参数 | 默认 | 说明 |
|---|---|---|
| `-addr` | `:8080` | HTTP 监听（/mcp + /healthz + local API） |
| `-grpc-addr` | `:9080` | gRPC 管理 API 监听（仅 manager 角色） |
| `-agent-config` | `/etc/opsguard/agent-config.yaml` | worker 策略配置 |
| `-config` | — | 编排用 service config（启动校验用） |
| `-data-dir` | `/var/lib/opsguard` | 事件/审计 SQLite 目录 |
| `-log-level` | `info` | debug/info/warn/error；`-log-json` 切 JSON 日志 |
| `-tls-cert/-tls-key/-tls-ca` | — | HTTPS/mTLS（`-tls-ca` 时强制客户端证书） |
| `-mcp-stdio` | — | MCP 改走 stdio（本地 Claude Desktop/cursor 调试用） |

环境变量覆盖（优先级高于 yaml）：`WORKER_ROLE`、`WORKER_TOKENS`（设置即启用
auth）、`WORKER_ALLOW_HOST_EXEC`、`WORKER_ALLOW_CONTAINER`、
`WORKER_COMMAND_TIMEOUT`、`OPSGUARD_TUNNEL_BASE`、`OPSGUARD_TUNNEL_POOL`、
`OPSGUARD_IDP_ISSUER`/`OPSGUARD_IDP_PUBLIC_ISSUER`、
`OPSGUARD_REGISTRY_CACHE_DIR`/`OPSGUARD_REGISTRY_CACHE_MB`（镜像 blob LRU 缓存）。

## 5. 管理端接入（页面添加集群）

Worker 部署完成后，管理员在**管理端页面**把该集群登记进来，管理端才会主动
连接 worker——worker 无需任何注册动作。前提：OpsGaurdWeb server 已部署、
可登录页面（server 自身部署不在本文范围，见 `deploy/offline/README.md`）。

### 5.1 添加前预检：管理端 → Worker 连通性

在**管理端 server 所在主机**上，先验证能到达 worker 的 gRPC 端口（页面探测
走的就是这条路，提前发现网络问题）：

```sh
nc -zv <manager-ip> 6061
# 无 nc 时用 bash 内建：
timeout 3 bash -c '</dev/tcp/<manager-ip>/6061' && echo OK
```

不通则先解决防火墙/路由/NAT（§2.2），否则页面上必然探测失败。

### 5.2 页面操作

登录管理端 → **集群管理**页 → 新增集群，按表填写：

| 表单项 | 必填 | 填写内容 |
|---|---|---|
| 集群名称 | ✅ | 自定义，如 `disaster-cluster`（创建后不可改名） |
| 所属项目 | — | 下拉选择已有项目，便于按项目分组筛选 |
| **Manager 地址** | ✅ | `http://<swarm-manager-IP>:<gRPC端口>`，如 `http://10.60.171.232:6061`。**填 gRPC 端口**（后端剥掉 scheme 后 dial gRPC），填成 HTTP 端口会探测失败 |
| **Token** | 生产必填* | 与 §4.3 配置到 worker `auth.tokens` 的同一个 secret。表单标注"可选"，但 worker 开了鉴权而此处不填，探测/连接会被拒绝；若两边都不开鉴权，页面会出现"⚠ 未配置 token——任何能访问 gRPC 端口的人均可操作集群"的裸奔警告 |
| 描述 | — | 环境用途备注 |

两点说明：

- Web 表单没有 MCP 地址字段。如需给 LLM Agent 配 MCP 端点
  （`http://<manager-ip>:<HTTP端口>/mcp`），改用 API 注册（`POST /api/v1/clusters`，
  请求体含 `name/project_id/worker_url/mcp_url/token/desc/inventory`）或在管理端
  配置文件预置集群（`clusters.<name>.worker_url / token / mcp_url / desc`，
  示例见 `OpsGaurdWeb/deploy/config.docker.yaml`），效果与页面注册相同。
- 纳管清单（inventory，声明 `docker run` 独立容器 / 宿主机服务等外部纳管对象）
  为可选的后置配置，见 `docs/集群纳管清单方案.md`，不影响首次接入。

### 5.3 提交后

提交时管理端会**先探测**（5 秒超时）：连接 worker 的 gRPC 端口并确认对端是
swarm manager 角色，失败返回 502（`ErrProbeFailed`），按 §6.3 排查。成功后：

- 集群列表**状态**列为在线，**最近探测**时间开始刷新（管理端周期探活）；
- 进入集群详情页，可见各节点列表及实时 CPU/内存指标（`WatchNodeStats` 流）。

## 6. 部署验证

### 6.1 Worker 侧

```sh
# 服务在跑、每节点一个副本
docker stack services opsguard        # REPLICAS 应为 x/x
docker service ps opsguard_worker     # 各节点 task 均 Running

# 版本核对（与升级目标版本一致）
docker service logs opsguard_worker --tail 20 | grep version

# 存活探针（HTTP 端口）
curl -s http://localhost:6060/healthz

# gRPC 端口在监听
ss -lntp | grep 6061

# manager 角色确认（编排/MCP 只在 manager 生效）
docker node ls                        # 本节点是否 manager
```

### 6.2 管理端侧

1. 集群列表中该集群**状态在线**（探测通过）；
2. 集群详情页能看到各节点及实时 CPU/内存（`WatchNodeStats` 流正常）；
3. 触发一次事件验证上行链路：在集群里停一个被监控服务，数秒内管理端
   事件/告警出现（`SubscribeEvents` 流正常）。

### 6.3 探测失败排查清单

- `worker_url` 填的是 **gRPC 端口**（如 6061），不是 HTTP 端口；
- 管理端 → worker 的 gRPC 端口网络不通（防火墙/路由/NAT，注意单向策略只需
  管理端→worker 方向放行）；
- token 不一致（`agent-config.yaml` 与注册表单）；
- 该节点不是 swarm manager（探测要求 manager 角色）；
- auth 未开启但带了 token（或反之）。

### 6.4 端到端验收清单

一个集群从空机器到页面接入完成的逐项验收（按序执行）：

| # | 检查项 | 命令 / 位置 | 预期 |
|---|---|---|---|
| 1 | 离线包已分发（每节点） | `ls /opt/opsguard/offline /opt/opsguard/images` | §3.2 清单齐全 |
| 2 | Docker 就绪（每节点） | `docker version` | Server 27.x 正常输出 |
| 3 | insecure-registries 核对（每节点） | `docker info \| grep -i insecure` | 中继点/管理端地址 |
| 4 | Swarm 就绪（manager 上） | `docker node ls` | 全节点 Ready，Leader 在预期节点 |
| 5 | Token 已配置（每节点） | `grep -A2 '^auth' /etc/opsguard/agent-config.yaml` | `enabled: true` 且各节点同一 secret |
| 6 | 镜像已导入（每节点） | `docker images \| grep opsguard-worker` | 目标版本存在 |
| 7 | worker 服务运行（manager 上） | `docker stack services opsguard` | REPLICAS 为 x/x |
| 8 | 版本正确 | `docker service logs opsguard_worker --tail 20 \| grep version` | 目标版本号 |
| 9 | healthz（每节点） | `curl -s http://localhost:6060/healthz` | 200 |
| 10 | gRPC 端口监听（manager） | `ss -lntp \| grep 6061` | LISTEN |
| 11 | 管理端→worker 连通（server 主机） | `nc -zv <manager-ip> 6061` | succeed |
| 12 | 页面添加集群 | 集群管理 → 新增 | 提交成功、无 502 |
| 13 | 集群在线 | 列表状态列 / 集群详情页 | 在线，节点与 CPU/内存指标实时刷新 |

## 7. 升级与回滚

### 7.1 升级顺序：先全部 Worker，后 Server

**必须先升级所有集群的 worker，再升级管理端 server**。隧道流有池化互通性，
旧/新版本混搭期间可能互通异常；反方向（旧 worker + 新 server）会因 worker
缺 RPC 持续报 `unknown method Tunnel` 并每 32 秒重连刷日志。离线环境 12/13
号升级记录即由此确认此顺序。

### 7.2 标准升级步骤（离线 stack 形态）

```sh
# 1. 各集群 manager 节点导入新镜像
docker load -i opsguard-worker-<new>.tar

# 2. 修改 stack.yml 的 image tag，滚动更新（global 模式逐节点替换）
docker stack deploy -c stack.yml opsguard

# 3. 逐节点核对
docker service ps opsguard_worker --no-trunc
docker service logs opsguard_worker --tail 20 | grep version   # 新版本号
curl -s http://localhost:6060/healthz

# 4. 管理端侧确认各集群在线、事件流正常，之后再升级 server
```

无法整栈更新时（离线环境常用），可基于旧镜像 COPY 新二进制重打：

```sh
docker run --rm -it --name tmp opsguard-worker:<old> sh   # 确认旧镜像内路径
docker build -t opsguard-worker:<new> - <<'EOF'
FROM opsguard-worker:<old>
COPY worker /usr/local/bin/worker
EOF
```

> 实战坑：`docker cp` 进容器会丢执行位，重打包用 Dockerfile `COPY` 并确认
> 源文件带执行位；`docker commit` 会把 ENTRYPOINT 固化，优先用 Dockerfile。

### 7.3 回滚

stack 形态直接把 image tag 改回旧版本重新 `docker stack deploy` 即可
（swarm 滚动替换，事件不丢——SQLite 队列在 `/var/lib/opsguard` 挂卷里，
恢复后 gRPC 流带游标续传）。保留上一版本镜像 tar 是回滚的前提。

## 8. 卸载

```sh
docker stack rm opsguard            # 或 docker rm -f worker（docker run 形态）
# 视需要清理（会丢事件/审计历史）：
rm -rf /var/lib/opsguard /etc/opsguard/agent-config.yaml
```

管理端侧删除集群（`DELETE /api/v1/clusters/:name`）即断开连接与指标订阅。

## 9. 常见问题（实战记录）

| 现象 | 原因与处理 |
|---|---|
| host exec 不生效 / 宿主机进程只见 worker 自身 | swarm **静默忽略** `privileged`/`pid: host`。需要真实宿主机能力时改用 `docker run --privileged --pid=host --network host` 普通容器（§4.7） |
| server 日志周期性刷 `unknown method Tunnel` | worker 版本过旧缺 RPC。按 §7.1 先升 worker；每 32s 重连一次是正常重试节奏 |
| 镜像 push 到一半 502 | 隧道经 HTTP 反代时 ReadTimeout 30s 掐断大层传输；调大反代读超时 |
| 隧道传输仅 ~35KB/s | gRPC 流控窗口 64KiB 限制；升级到已调大窗口的版本 |
| 节点 x509 证书错误 | 时钟漂移，NTP 对时（§2.3） |
| 端口冲突（worker 起不来） | HTTP 8080 与管理端页面同机冲突；用 `-addr`/`-grpc-addr` 错开（参考单机环境 8090/9090） |
| 镜像 tar 导入失败 | OCI 与 legacy 格式差异：`docker load` 报错时确认导出方式，或在目标版本 Docker 上重新 `docker save` |
| ingress 端口改 `mode: host` 后老连接不通 | 修改残留 DNAT 规则，必要时重启 docker 或清理 iptables DNAT |
| swarm 里 `docker cp` 后二进制不可执行 | cp 丢执行位；改用 Dockerfile COPY 重打镜像（§7.2） |
| 事件断流后历史丢失 | `/var/lib/opsguard` 未挂卷。挂卷后断线重连带游标续拉，ack 后才 GC |

## 10. 相关文档

- `docs/Worker-设计方案.md` —— worker 设计（部署模型、gRPC API、config schema、命令策略）
- `docs/OpsGaurd-系统技术总览.md` —— §五 通信设计、§十 配置参考、§十一 端口矩阵
- `deploy/offline/README.md` —— 离线环境部署 runbook 与全部实战升级记录
- `Worker/README.md` —— 功能/API/构建速览
- `docs/镜像隧道中继方案.md` —— `/v2` 镜像中继与 Tunnel 协议
- `docs/IdP-接入指南.md` —— `/idp-proxy/` 相关
