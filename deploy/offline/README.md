# OpsGaurd 离线部署包（单节点 10.60.189.6）

目标机无外网、无 Docker，采用「本地构建镜像 → 导出 → 上传 → 离线安装」方式。

## 拓扑

```
10.60.189.6 (UOS Server 20, docker 27.5.1, 单节点 swarm manager)
├── opsguard-server  容器    页面+API :8080（内嵌 OCI 仓库 /v2、LevelDB /app/data）
└── opsguard_worker  swarm 全局服务   :8090（manager 角色：编排+MCP+监控，webhook→server ingest）

自然灾害集群（5×RKE K8s 节点，docker 已有，Worker 端口 6060）
├── 10.60.171.232 (NXYJGLT-YJ-b-1, docker 27.3.1)  swarm Leader + Worker manager 角色
├── 10.60.171.230 (NXYJGLT-YJ-b-2, docker 27.3.1)  node 角色
├── 10.60.171.249 (NXYJGLT-YJ-b-3, docker 27.3.1)  node 角色
├── 10.60.171.253 (NXYJGLT-YJ-4,   docker 20.10.24) node 角色
└── 10.60.171.231 (NXYJGLT-YJ-5,   docker 20.10.24) node 角色
```

## 文件

| 文件 | 部署位置 | 说明 |
|---|---|---|
| bundle/opsguard-server-1.0.0.tar … opsguard-server-1.2.0.tar | /opt/opsguard/images/ | 管理端镜像（本地构建导出；当前线 = 1.2.0） |
| bundle/opsguard-worker-1.0.0.tar … opsguard-worker-1.1.0.tar | /opt/opsguard/images/ | Worker 镜像（当前线 = 1.1.0） |
| bundle/docker-27.5.1.tgz | /opt/opsguard/offline/ | docker 静态二进制 |
| install-docker.sh / docker.service / containerd.service / daemon.json | /opt/opsguard/offline/ | 离线安装（含 swarm init、insecure-registries=10.60.189.6:8080） |
| agent-config.yaml | /etc/opsguard/agent-config.yaml | Worker 策略（blacklist、关 host exec、webhook→:8080） |
| stack.yml | /opt/opsguard/stack.yml | Worker swarm 服务（8090/9090，global，含 IdP 隧道 env） |
| ../../OpsGaurdWeb/deploy/config.docker.yaml | /opt/opsguard/server/config.yaml | 管理端配置（:8080、认证已开、registry 已开） |
| agent-config.disaster.yaml | 灾害集群 5 台 /etc/opsguard/agent-config.yaml | Worker 策略（auth enabled + token，5 台一致） |
| stack.disaster.yml | 232:/opt/opsguard/stack.disaster.yml | 灾害集群 Worker（6060/6061，global，mode:host，含 IdP 隧道 env） |

### 灾害集群（5 节点）部署记录与要点

1. 镜像 tar 格式：Docker 25+（含 containerd 存储）导出的 OCI layout（blobs/）
   与经典存储的 legacy（manifest.json + repositories）两种。**实测 docker
   20.10.24 / 27.3.1 均可直接 load OCI 格式**（1.1.0 起 bundle 里的 tar 统一
   为 OCI，一份全节点通用；"向 20.10 必须转 legacy"的旧记录已不适用）。
2. swarm 端口发布用 `mode: host`（非默认 ingress）——ingress 会把请求轮转到
   任意节点任务，node 角色任务没有编排 API（404）；host 模式保证管理端请求
   232:6060 恒落到本机 manager 任务，manager 代理节点 API 也走各自 IP:6060。
   注意：由 ingress 模式**更新**为 host 模式时各节点会残留旧 ingress 绑定
   （DOCKER-INGRESS DNAT + dockerd 占用 6060），外部流量被轮转/新任务起不来；
   正确做法是 `docker service rm` 后重新 deploy（全新部署直接生效无残留）。
   231 曾因残留触发 leave+rejoin，swarm 里留下 Down 幽灵节点，已 node rm。
   另：host 模式下 `docker service update --force` 滚动重启也可能瞬时报
   `host-mode port already in use`（start-first 顺序），swarm 会自动重试收敛，
   无需干预；个别情况收敛不了一侧时再 rm+deploy。
3. 管理端 workerproxy 默认超时 10s 对多节点聚合接口（/nodes ~16s）不足，
   1.0.1 起 WorkerClient 放宽到 30s（健康探测仍 10s 快速失败）。
4. 节点时钟必须同步：230 因时钟慢 9 分钟 join 时报 x509 not yet valid，
   `timedatectl set-time` 对齐 232 后恢复。段内无可用 NTP 源，建议后续把
   232 配成 chrony server 供全网对齐。
5. K8s 共存：canal/flannel 用 UDP 8472，swarm overlay 用 UDP 4789，互不冲突；
   swarm gossip 7946/tcp+udp 与管理 2377/tcp 段内直连。firewalld 均 inactive。
6. **网段开通情况（2026-08-06）**：防火墙已放行 189.6 → 10.60.171.232 的
   6060-6064/tcp，纳管通道就绪（集群 nxyj-cluster 已纳管、online）。
   未覆盖：232 → 189.6:8080（Worker webhook 推送告警需此方向；未开通前
   agent-config 的 webhooks 保持注释）。开通后：取消注释 +
   `docker service update --force opsguard_worker`。
7. **鉴权配置必须 5 台一致（2026-08-08 统一）**：agent-config.disaster.yaml
   为 auth enabled + token 版；232 已配置，230/249/253/231 曾漏配（旧版
   auth disabled）。节点不一致时 manager→node 代理（带 manager token）在
   开启鉴权的节点成功、未开启的节点虽能通但端口裸奔。改配置后
   `docker service update --force opsguard_worker` 滚动重启全部任务生效。
8. **升级记录（2026-08-08）**：server 1.1.5 → **1.2.0**（服务编辑/事件游标
   分页/纳管清单表单化/standalone 容器重启/节点加载性能）；worker 1.0.8 →
   **1.1.0**（RestartContainer RPC、stats 5s 缓存、事件 after_seq 分页、
   ResolveNodeAddr 空 id=本机）。升级动作：5 节点 `docker load` 新 tar →
   `docker service update --image opsguard-worker:1.1.0 opsguard_worker`。
   本地 worker 1.0.3 → 1.1.0 并补配 IdP 隧道 env（TUNNEL_BASE/IDP_PUBLIC_ISSUER，
   见 stack.yml）——旧 worker 缺 Tunnel RPC，server 会每 32s 重连报
   `unknown method Tunnel` 刷日志。
9. **IdP 反向隧道**：集群内服务（r-nacos 等）经 worker `/idp-proxy/` 访问
   管理端 IdP，需 worker env：`OPSGUARD_TUNNEL_BASE=<集群内可达的 worker HTTP
   地址>` + `OPSGUARD_IDP_PUBLIC_ISSUER=<政务外网 issuer>`。缺 env 时 server
   日志报 `idp tunnel not enabled on this worker`（INFO 级，功能未启用）。

## 部署步骤（已完成，供重建参考）

```bash
# 本地（有网）
cd OpsGaurdWeb && npm run build --prefix web   # 先出前端 dist
docker build -t opsguard-server:1.2.0 -f deploy/Dockerfile .
cd Worker && docker build -t opsguard-worker:1.1.0 -f deploy/Dockerfile .
docker save -o opsguard-server-1.2.0.tar opsguard-server:1.2.0
docker save -o opsguard-worker-1.1.0.tar opsguard-worker:1.1.0
# 上传全部文件后，目标机：
bash /opt/opsguard/offline/install-docker.sh /opt/opsguard/offline/docker-27.5.1.tgz
docker load -i /opt/opsguard/images/opsguard-server-1.2.0.tar
docker load -i /opt/opsguard/images/opsguard-worker-1.1.0.tar
docker run -d --name opsguard-server --restart unless-stopped -p 8080:8080 \
  -v /opt/opsguard/server/data:/app/data \
  -v /opt/opsguard/server/config.yaml:/app/configs/config.yaml:ro \
  -v /var/run/docker.sock:/var/run/docker.sock opsguard-server:1.2.0
docker stack deploy -c /opt/opsguard/stack.yml opsguard
# 灾害集群：5 节点 load opsguard-worker-1.1.0.tar，232 上 service update --image
```

## 运维

- 页面：http://10.60.189.6:8080 （admin / opsguard-admin，首登后改密）
- 已纳管集群：`local` → http://10.60.189.6:8090
- 日志：`docker logs opsguard-server` / `docker service logs opsguard_worker`
- 升级：重新构建导出镜像 → 上传 load → `docker rm -f opsguard-server && docker run …`（数据在 /opt/opsguard/server/data 卷内，不受影响）/ `docker service update --image opsguard-worker:<新tag> opsguard_worker`
- AiNexus：页面「系统设置 → AI 排查网关」配 API key 后即启用（配置热重载，无需重启）

## 已知事项

- swarm stack 会忽略 `privileged/pid`（日志提示 Ignoring unsupported options），host exec 已按策略关闭，无影响。
- `localhost:8090` 在本机不通（本机 OUTPUT 链不过 ingress DNAT），用 `10.60.189.6:8090` 访问正常。
- registry /v2 内网免认证（users 空）；daemon.json 已把 10.60.189.6:8080 加入 insecure-registries。

## 限制：swarm 模式下 Worker 看不到宿主机全量进程

swarm service **不支持** `pid: host` 与 `privileged`（stack deploy 时静默忽略，
日志可见 `Ignoring unsupported options: pid, privileged`）。因此以 swarm 服务
运行的 Worker 处在自己的 PID 命名空间里：

- 管理台「节点 → 进程」只能列出 worker 容器内的进程（通常 1 条），**不是宿主机全量进程**；
- 节点资源统计仅覆盖容器视角；`exec_host_command`（nsenter 进宿主 PID 1）不可用
  （agent-config 里 `allowHostExec: false`，本就关闭，如需开启必须先解决部署形态）。

编排/监控/MCP/容器内命令执行不受影响（走挂载的 docker.sock）。

**需要完整主机可视性时**（单机场景推荐），改用普通容器跑 Worker：

```bash
docker service rm opsguard_worker
docker run -d --name opsguard-worker --restart unless-stopped \
  --privileged --pid host --network host \
  -v /var/run/docker.sock:/var/run/docker.sock:ro \
  -v /etc/opsguard/agent-config.yaml:/etc/opsguard/agent-config.yaml:ro \
  opsguard-worker:1.0.1 -agent-config /etc/opsguard/agent-config.yaml -addr :8090
```

role=auto 仍识别为 manager（查的是本机 daemon 的 swarm 状态），编排能力不变；
代价是 Worker 自身不再是 swarm 服务，失去 swarm 的副本自愈（由 `--restart` 兜底）。
