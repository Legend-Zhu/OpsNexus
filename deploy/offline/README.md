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

安责险集群（3×裸机，2026-08-14 新装，Worker 端口 6060/6061）
├── 10.60.185.66 (NXYJGLT-AZBX-1, Kylin V10, docker 27.5.1)  swarm Leader + Worker manager 角色
├── 10.60.185.89 (NXYJGLT-AZBX-2, Kylin V10, docker 27.5.1)  node 角色
└── 10.60.185.86 (NXYJGLT-AZBX-3, Kylin V10, docker 27.5.1)  node 角色
```

## 文件

| 文件 | 部署位置 | 说明 |
|---|---|---|
| bundle/opsguard-server-1.0.0.tar … opsguard-server-1.2.0.tar | /opt/opsguard/images/ | 管理端镜像（本地构建导出；当前线 = 1.2.17，离线重打见记录 10/11/12/14/16/17/18/19/20） |
| bundle/opsguard-worker-1.0.0.tar … opsguard-worker-1.1.0.tar | /opt/opsguard/images/ | Worker 镜像（当前线 = 1.2.7，离线重打/中继分发见记录 10/11/12/15） |
| bundle/docker-27.5.1.tgz | /opt/opsguard/offline/ | docker 静态二进制 |
| install-docker.sh / docker.service / containerd.service / daemon.json | /opt/opsguard/offline/ | 离线安装（含 swarm init、insecure-registries=10.60.189.6:8080） |
| agent-config.yaml | /etc/opsguard/agent-config.yaml | Worker 策略（blacklist、关 host exec、webhook→:8080） |
| stack.yml | /opt/opsguard/stack.yml | Worker swarm 服务（8090/9090，global，含 IdP 隧道 env） |
| ../../OpsGaurdWeb/deploy/config.docker.yaml | /opt/opsguard/server/config.yaml | 管理端配置（:8080、认证已开、registry 已开） |
| agent-config.disaster.yaml | 灾害集群 5 台 /etc/opsguard/agent-config.yaml | Worker 策略（auth enabled + token，5 台一致） |
| stack.disaster.yml | 232:/opt/opsguard/stack.disaster.yml | 灾害集群 Worker（6060/6061，global，mode:host，含 IdP 隧道 env） |
| agent-config.azbx.yaml | 安责险 3 台 /etc/opsguard/agent-config.yaml | Worker 策略（auth enabled + 独立 token） |
| stack.azbx.yml | 66:/opt/opsguard/stack-azbx.yml | 安责险集群 Worker（6060/6061，global，mode:host，镜像=中继引用 1.2.7） |
| install-docker-noinit.sh | 安责险 worker 节点 /opt/opsguard/offline/ | docker 离线安装（不做 swarm init，装完 join manager） |

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
10. **镜像隧道中继 + blob 缓存（2026-08-10，server 1.2.4→1.2.5 / worker
   1.1.0→1.2.0）**：集群节点经 server↔worker 既有 gRPC 隧道从管理端内嵌
   OCI 仓库（`/v2`）拉镜像，**不开通「集群→189.6:8080」方向策略**。方案见
   `docs/镜像隧道中继方案.md`；本节只记部署要点与坑。
   - **无外网构建**：本机/189.6 均无外网（goproxy.cn、Docker Hub 全不可达），
     `docker build` 不可用。改为本地交叉编译 Linux 静态二进制
     （`CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build`，go mod cache 本地齐全）
     → 上传 189.6 → 离线重打镜像：`FROM opsguard-worker:1.1.0` +
     `COPY <新二进制> /usr/local/bin/worker`（server 同理 FROM 1.2.4）→ 本地
     `docker build`（纯 COPY 无网络）→ `docker save` 或直接分发。
   - **分发约束**：231/249/253 是 RKE 加固节点，**禁 SFTP 子系统**（shell 通道
     正常、sftp 协议握手直接被拒）；节点间 ssh 互信不通；Harbor `library/`
     对当前账号无 push 权限。实际做法：**232 起临时 HTTP 服务**
     （`cd /opt/opsguard/images && setsid python3 -m http.server 18099 --bind 10.60.171.232`，
     用完 `pkill -f "http.server 18099"`）→ 其余节点内网 `curl` 下载二进制。
   - **坑① docker cp 丢可执行权限**：`docker cp` 覆盖容器内二进制会丢
     `+x`，commit 出的镜像 exec 报 `permission denied`。
   - **坑② docker commit 固化 --entrypoint**：用 `--entrypoint chmod` 起容器
     再 commit，会把 chmod 写进镜像 Entrypoint（`docker run` 直接进 BusyBox
     chmod）。修法：`docker commit --change 'ENTRYPOINT ["/usr/local/bin/worker"]'`。
   - 每节点生成 1.2.0 镜像的标准动作（5 台各执行）：
     `docker create --name tmp-worker opsguard-worker:1.1.0` →
     `docker cp <二进制> tmp-worker:/usr/local/bin/worker` →
     `docker commit --change 'ENTRYPOINT ["/usr/local/bin/worker"]' tmp-worker opsguard-worker:1.2.0` →
     `docker rm tmp-worker`。更新：232 上
     `docker service update --image opsguard-worker:1.2.0 --env-add OPSGUARD_REGISTRY_CACHE_MB=2048 opsguard_worker`。
   - **节点接入中继**：5 台 `/etc/docker/daemon.json` 追加
     `"insecure-registries": ["10.60.171.232:6060"]`（先 `cp` 备份，python3
     改 JSON 保留原字段）→ `systemctl restart docker` 生效；镜像引用写
     `10.60.171.232:6060/<repo>:<tag>`。
   - 验证（已实测）：`curl 232:6060/v2/` → 200 +
     `Docker-Distribution-Api-Version`；`docker pull 10.60.171.232:6060/…` digest
     与 server 内嵌 registry 一致；7.9MB blob 分片经隧道拉取 sha256 逐字节一致；
     二次拉取命中 worker 侧缓存（server 无新增请求、数据零损坏）。
   - worker 缓存 env：`OPSGUARD_REGISTRY_CACHE_DIR`（缺省
     `<dataDir>/registry-cache`）、`OPSGUARD_REGISTRY_CACHE_MB`（MiB，0=不限；
     现网 232 配 2048）。
11. **中继双向化（2026-08-11，server 1.2.6 / worker 1.2.1）**：集群节点可直接
   **push** 镜像入内嵌仓库（`docker push 10.60.171.232:6060/<repo>:<tag>`，
   blob 分块上传 + manifest 提交，无需开通任何「集群→管理端」策略）。
   - worker 请求体分帧（req_chunked）+ server 多帧重组；**并发 push 帧交错**
     曾致 server 重组卡死整条隧道，已修（sendMu 串行化发送 + 缺终止帧补发）。
   - **worker 自身新版本优先走中继分发**（比逐台 commit 更优）：189.6 本地
     `docker tag/push 10.60.189.6:8080/library/opsguard-worker:<tag>` →
     5 台 `docker pull 10.60.171.232:6060/library/opsguard-worker:<tag>` →
     每台 `docker tag <中继引用> opsguard-worker:<tag>` → 232
     `docker service update --force opsguard_worker`。天然规避 docker cp
     丢执行位的坑（189.6 构建镜像权限正确）。
   - 已验证：253 上 `docker push 10.60.171.232:6060/library/build-api-test:v1`
     （11 层全量上传）→ catalog 可见 → 230 经中继 pull 回 digest 一致。
   - 注意：旧 worker（1.2.0）无并发修复，**并发 push 大 blob 会卡死隧道**；
     升级 worker 后再启用 push。
12. **隧道吞吐修复（2026-08-11，server 1.2.6→1.2.7 / worker 1.2.1→1.2.2 + 189.6
    本地 worker 1.1.0→1.2.2）**：经隧道拉/推镜像只有 35-70 KB/s（裸链路 3.9Gbps，
    慢约 10000 倍）。根因与修复见提交 `a2dd6ba`（`perf(relay)`）。
    - **根因**：① grpc-go 默认每流接收窗口 64KiB，单 HTTP/2 stream 在途字节卡死，
      吞吐 ≈ 64KiB/RTT——数量级主凶；② 整个集群一条 bidi stream 头阻塞 + 服务端
      并发 `Send` 无锁（旧 `sendMu` 仅 worker 侧有）；③ push body 整块缓冲阻塞
      recv 循环。
    - **修复**：两侧流控窗口提到 32MiB/流、64MiB/连接（client
      `WithInitialWindowSize`/`ConnWindow`；worker server `InitialWindowSize`/
      `ConnWindow`）；**隧道池化**——server 向每个 worker 开 N 条 Tunnel 流（默认
      16，`OPSGUARD_TUNNEL_POOL` 两端一致），每请求借独占一条，N 路并发各享独立
      流控窗口与 recv，干掉 worker 侧 `sendMu`/dispatch/streams map/id-demux、
      服务端每流单写者无需锁；push body 改 `io.Pipe` 流式转发（消除上百 MB 内存
      峰值）；registryproxy spool buf 32KiB→1MiB。proto/帧格式不变。
    - **互通性**：池化破坏「新 server↔旧 worker」（旧 worker 单 `stream` 字段被
      N 条流覆盖）——**部署顺序：先升全部 worker，再升 server**。本次：灾害 5 节点
      worker 1.2.1→1.2.2（中继 pull+tag 后 `service update`，host-mode 6060 端口
      冲突如记录 2 所述自愈）→ 189.6 本地 worker 1.1.0→1.2.2 → 最后 server 容器
      1.2.6→1.2.7（`docker rm -f` 后原样重建，保留 data/config/docker.sock 三挂载）。
    - **重打方式**：本地交叉编译 `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build`
      → 上传 189.6 → `FROM <旧镜像> + COPY <二进制> + RUN chmod +x`（纯 COPY 无网络，
      避开记录 10 坑①docker cp 丢执行位 / 坑②commit 固化 entrypoint）→ push
      189.6:8080 → 各节点经 232:6060 中继 pull+tag。
    - **实测**（50MB 不可压缩单层，经隧道 232↔189.6）：push 4.5s≈11MB/s、pull
      4.2s≈11.8MB/s，双向对称，较修前提升约 150-340 倍。（单层数字含 docker gzip
      + OCI 分块协议开销；in-process bench 单流 926MB/s / 池并发 1012MB/s；真实
      多层并发拉取聚合更高。）
    - **回滚**：旧镜像 server 1.2.6 / worker 1.2.1（灾害）·1.1.0（本地）均保留。
13. **纳管监控 + 节点统计修复（2026-08-18，server 1.2.9→1.2.10 / worker 灾害
    1.2.5→1.2.6、本地 1.2.3→1.2.6）**：提交 `e877ac2`（集群告警规则 tab +
    invmonitor）、`36d2fe1`（告警产生/恢复自动通知）、`74a542e`（幽灵告警治理）、
    及此前未发的 worker 修复 `8b18833/f55a0f7/01f6cb0/c2ba0a6`（SSE camelCase、
    healthy 跨节点、中继陈旧流、LocalStats 超时——**节点全 unreachable 的根治**）。
    - **重打方式**同记录 12（本地交叉编译 + FROM 旧镜像 COPY 二进制）；server
      镜像额外 `COPY web/ /app/web/`（前端不内嵌二进制，**重打必须带新 dist**）。
    - **分发顺序**：先 worker（灾害 5 节点经 232:6060 中继 pull+tag →
      `service update --image opsguard-worker:1.2.6`），再 189.6 本地 worker，
      最后 server 容器（顺序兼容：新 worker + 旧 server 可用）。
    - **已验证**：SSE `nodes/stream` 五节点 `reachable:true` 且 CPU/内存/容器数
      实时流入（此前全 0）；invmonitor 端到端冒烟（host-service 指向关闭端口
      → 20s 内 `port_down` 告警；清单↔规则自动同步增删）；告警手动恢复 API。

14. **MLOps 运营层上线（2026-08-20，server 1.2.10→1.2.11）**：提交 `b5ec4ea`
    （P0 计量与兼容性基础）、`bc0e4b3`（P1 结构化 Prompt Hub + P2 用量费用）、
    `57487a2`（P3 模型运营与预算）、`36c5a67`（P4 前端与文档收尾）。仅 server +
    前端（Worker 无改动，仍 1.2.6）。方案与操作口径见 `docs/MLOps-方案.md`（v0.5）。
    - **重打方式**同记录 13：本地交叉编译 `server-1.2.11`（29MB）+ `web-1.2.11.tar.gz`
      （530KB，含 MLOps 三 tab）上传 `/opt/opsguard/build/1.2.11/`（sha256 校验一致）；
      `FROM opsguard-server:1.2.10 + COPY server + rm -rf /app/web/* + COPY web/` 重打。
      注意：web tar 是平铺解包，构建前需把 `index.html`/`assets/` 归拢进 `web/` 子目录。
    - **配置变更**：`/opt/opsguard/server/config.yaml` 追加 `mlops: enabled: true`
      （备份 `config.yaml.bak-1.2.10`）；LevelDB 迁移 v2→v3 随启动自动完成（纯前缀新增）。
    - **容器重建**：`docker rm -f` 后原样 run（data/config/docker.sock 三挂载不变）。
    - **已验证**：healthz 200、首页 200（新 dist）、`/api/v1/mlops/*` 全路由挂载
      （未认证 401）、三集群事件订阅 + IdP 隧道 + registry 重连正常、本地 worker 1/1。
    - **回滚**：`docker rm -f opsguard-server` 后用 `opsguard-server:1.2.10` 原样
      run + 还原 `config.yaml.bak-1.2.10`（mlops.enabled=false 时 bucket 保留只读）。
    - 注意：AiNexus 网关未启用前（`ainexus_embedded=false`），MLOps 模型/费用无数据——
      在「系统设置 → AI 排查网关」配 provider 后计量自动开始。

15. **push 大层 30s 502 修复（2026-08-20，worker 1.2.6→1.2.7，server 无改动）**：
    worker HTTP server 的 `ReadTimeout: 30s` 会掐死任何超过 30 秒的 blob 上传——
    maxkb4j app 镜像 358MB 单层两次 push 均在 30s 边界 502。拉镜像的 WriteTimeout
    早已为 0（流式 blob），推镜像的读侧同样放开：**ReadTimeout 0 + IdleTimeout
    120s 兜底空闲连接**（`Worker/cmd/worker/main.go`）。
    - **发版**：本地交叉编译（`-ldflags` stamp 版本号，启动日志 `worker starting
      version=1.2.7` 可核）→ 189.6 重打（FROM 1.2.6 + COPY，同记录 12/13 模式）
      → push 内嵌仓库 → 灾害 5 节点经 232:6060 中继 pull+tag → 232
      `service update --image opsguard-worker:1.2.7`（global 5/5 收敛）→ 189.6
      本地 worker 同步 1.2.7。
    - **验证**：231 重推 `docker push 10.60.171.232:6060/maxkb4j/maxkb4j:latest`
      成功（358MB 层完整入库，digest `sha256:50b498c1…`）；服务端 catalog 出现
      `maxkb4j/maxkb4j`，manifest `Docker-Content-Digest` 与 push 端一致。
    - **app 切换**：PUT `/api/v1/clusters/nxyj-cluster/workloads/maxkb4j-app`
      配置快照 image 由 `10.60.171.253:20005/maxkb4j/maxkb4j:latest`（旧 Harbor）
      换 `10.60.171.232:6060/maxkb4j/maxkb4j:latest`；钉节点约束
      `node.hostname==NXYJGLT-YJ-5` 不变，滚动替换后 running/healthy 1/1。
    - 注意：安责险集群 worker 仍 1.2.5，**未含此修复**——经 66:6060 push 大层
      同样会 30s 502，下次发版需带上（本地交叉编译 + 66 重打分发同记录 4）。
16. **账号安全收敛（2026-08-21，server 1.2.11→1.2.12 + 前端，worker 无改动）**：
    改密自助 `PUT /api/v1/auth/password`（旧密码校验，输错返 400 不触发全局
    登出）+ 管理员重置 `PUT /api/v1/users/:username/password`；用户管理
    （列表/新增/重置）与 AiNexus 网关配置写/测试收敛 admin 角色；前端新增
    ChangePasswordDialog、巡检报告页迁至 patrol/ReportDelivery 等。
    - **重打方式**同记录 13/14：本地交叉编译 `server-1.2.12`（43MB）+
      `web-1.2.12.tar.gz`（544KB，**tar 内带 web/ 前缀**，解包即 COPY 免归拢）
      上传 `/opt/opsguard/build/1.2.12/`（sha256 校验一致）；
      `FROM opsguard-server:1.2.11 + COPY server + chmod + rm -rf /app/web/* +
      COPY web/` 重打 → `docker rm -f` 后原样 run（三挂载/8080/unless-stopped
      不变）。
    - **已验证**：healthz 200；`PUT /api/v1/auth/password` 未认证 401（新路由
      挂载）；首页引用新 bundle（`index-FOKnJfhI.js`）；**LevelDB 会话跨重启
      有效**（旧 token 复用）；三集群隧道重连（streams=16）；节点 SSE 5/5
      可达；`go test` router/auth/api 包通过。
    - **同晚事件备注（00:23-00:28 本地时间）**：worker 1.2.7 发版约 15 分钟后
      集群出现一波任务重启风暴 + 232 内核 soft lockup（load 5min 峰值 14，
      runc/JVM 线程卡死），期间节点全灰「不可达」。排查结论：**非 1.2.7 代码
      回归**——审计无管理端编排操作、worker 均为收到 SIGTERM 干净退出（外部
      swarm/docker 侧触发，疑似宿主机/docker 守护进程重启连锁），风暴后全部
      自愈（5/5 reachable，资源指标正常）。
17. **registry 页构建项目名可新建（2026-08-21，server 1.2.12→1.2.13 + 前端，
    worker 无改动）**：上传构建的「项目名」下拉虽配了 `allow-create`，但
    el-select 点击外部失焦会丢弃未回车的新名，体验上等于只能选已有命名空间。
    改为 `el-autocomplete`（自由输入 + 现有命名空间建议，`registry/Index.vue`），
    输入值永远保留在表单。
    - **重打方式**同记录 14/16 但**仅前端**（server 二进制不变）：本地
      `web-1.2.13.tar.gz`（545KB，tar 带 `web/` 前缀）上传
      `/opt/opsguard/build/1.2.13/`（sha256 校验一致）；
      `FROM opsguard-server:1.2.12 + rm -rf /app/web/* + COPY web/` 重打 →
      `docker rm -f` 后原样 run。
    - **已验证**：healthz 200；首页引用新 bundle（`index-CFod7ZBO.js`）；三集群
      online；节点 SSE azbx 3/3 `reachable:true` 实时指标（注意：REST `/nodes`
      的 `reachable/cpuPercent` 本就是零值占位，由 SSE 流填充，勿据此误判）；
      registry `/v2` 正常（当晚即有经页面构建的 `library/insurance:v0.0.1`）。
    - **回滚**：`docker rm -f` 后用 `opsguard-server:1.2.12` 原样 run。
18. **autocomplete 建议空白行修复（2026-08-21，server 1.2.13→1.2.14 + 前端，
    worker 无改动）**：记录 17 的回归——`fetch-suggestions` 回调传了纯字符串
    数组，而 el-autocomplete 默认模板按 `item[valueKey]`（即 `item.value`）
    取值渲染，字符串取 `.value` 为 undefined → 下拉全是空白行、点击选不中。
    改为返回 `{ value: p }` 对象数组。重打/验证/回滚同记录 17（新 bundle
    `index-E2zWGwlk.js`，回滚目标 1.2.13）。
19. **告警规则删除级联停止 + 登录页默认账号提示移除（2026-08-26，
    server 1.2.14→1.2.15→1.2.16，worker 无改动仍 1.2.7）**：
    - **1.2.15**（提交 `199efdb`/`51d4560`）：删除规则改为逆向下发——swarm 服务
      向 Worker 推 `enabled:false`、纳管对象清清单 Monitoring；停止失败保留
      规则（502）可 `?force=true` 强删。本地交叉编译 server（29MB）+ 新 dist
      打包上传 `/opt/opsguard/build/1.2.15/`，`FROM 1.2.14` 重打。
      验证：healthz 200、GET `/` `/login` 200、未认证 alertrules 401、旧容器留作
      `opsguard-server-1.2.14-backup`。
    - **1.2.16**（仅前端，同日跟进）：登录页移除「本地默认账号 admin / …」提示
      及死代码（`views/login/Index.vue`）。**注意 COPY 只覆盖同名文件**：首次
      重打后容器内仍残留 1.2.15 旧 chunk（含旧提示文案，页面已不引用但可按
      hash 直链访问）——重打改为 `FROM 1.2.15 + rm -rf /app/web/* + COPY web/`
      （同记录 14 口径），容器内 `grep -rl opsguard-admin /app/web/assets`
      确认零命中。验证：healthz 200、`/login` 200（bundle `index-CXMPMFPC.js`）。
    - **回滚**：`opsguard-server:1.2.14` / `:1.2.15` 镜像均保留，原样 run 即可。

20. **探测告警标题带服务对象名（2026-08-28，server 1.2.16→1.2.17，worker/前端
    无改动）**：TCP/HTTP 探测告警的飞书消息此前只有 `[集群] 端口不可达：TCP
    ip:port …`，看不出是哪个服务。根因：标题由 `ingest.AlertTitle` 拼接，只用
    了集群名+事件 Msg，事件自带的 Service（纳管对象名 / swarm 服务名）没进
    标题，而飞书通知只投递标题文本。改动（提交 5198b3c 一系列）：
    - `AlertTitle` 标题改 `[集群/服务] 类型：详情`（Service 缺失退化 `[集群]`）；
    - 新增 `TitleBody`：恢复类通知（事件恢复 + 手动恢复）剥掉标题自带前缀，
      避免 `[c/s] 告警已恢复：[c/s] …` 重复；
    - 存量活动告警在下一次同类事件合并时自动更新标题（UpsertAlert 覆盖
      Title），无需迁移；旧格式标题无前缀，TitleBody 原样返回兼容。
    - **发版**：本地交叉编译 `server-1.2.17`（29.8MB，sha256 389189e0…）上传
      `/opt/opsguard/build/1.2.17/`；`FROM opsguard-server:1.2.16 + COPY server
      + chmod` 重打（web 零改动，继承 1.2.16 的 web 层）；旧容器 rename 留
      `opsguard-server-1.2.16-backup` 后新容器原样 run（三挂载/unless-stopped/8080）。
    - **已验证**：healthz 200、三集群隧道池建立（232/66 各 16 流）、启动无
      error；端到端——local 清单临时加 `probe-selftest`（host-service 指向
      10.60.189.6:59999）→ 一个探测周期内产生 `port_down`，标题
      `[local/probe-selftest] 端口不可达：TCP …`，飞书 2s 内送达（error 策略
      两渠道）；删除临时条目后孤儿清理自动 recovered + 恢复通知同样新格式。
    - **回滚**：`docker rm -f opsguard-server` 后用 `opsguard-server:1.2.16`
      原样 run。

### 安责险集群（3 节点）部署记录（2026-08-14）

新纳管集群，3×裸机（无 K8s 共存），麒麟 V10 / x86_64，全部从零安装。
66（AZBX-1）为 swarm manager，89（AZBX-2）/86（AZBX-3）为 worker 节点。

1. **docker 安装**：66 用原版 install-docker.sh（含 swarm init）；86/89 用
   **install-docker-noinit.sh**（去掉 init，装完直接 `docker swarm join
   --token … 10.60.185.66:2377`）。daemon.json 的 insecure-registries 为
   `["10.60.189.6:8080", "10.60.185.66:6060"]`（后者=本集群 worker 中继）。
2. **坑① git autocrlf 脚本 CRLF**：工作区 checkout 的 install-docker.sh 带
   CRLF，bash 报 `set: pipefail：无效的选项名`。上传后
   `sed -i 's/\r$//' install-docker.sh` 修复；noinit 版为 LF 不受影响。
3. **坑② swarm 不自动创建 bind mount 源目录**：stack 引用
   `/var/lib/opsguard` 不存在时任务 Rejected（`bind source path does not
   exist`）。**全节点先 `mkdir -p /var/lib/opsguard`**；且 restart_policy
   max_attempts=3 耗尽后不再自愈，需 `docker service update --force
   opsguard_worker` 重新调度。
4. **worker 直接上 1.2.5**（新集群无旧包袱）：本地交叉编译二进制
   （.deploytmp/worker-1.2.5）→ 三台 load 1.1.0 tar → 66 上
   `FROM opsguard-worker:1.1.0 + COPY worker-1.2.5 + RUN chmod +x` 重打 →
   `docker save` → **66 起临时 HTTP（python3 -m http.server 18099）** →
   86/89 内网 curl 拉取 load（内网 67MB 秒级）。镜像引用 opsguard-worker:1.2.5。
5. **已验证**：3 节点 swarm Ready；`docker service logs` 确认 66
   `worker role=manager`（隧道池 streams=16、中继缓存 2048MB、反向隧道
   env 生效）、86/89 `role=node`；三台 `:6060/healthz` 全部 ok；
   auth enabled（token 见 agent-config.azbx.yaml，server 纳管注册时填）。
6. **纳管完成（2026-08-14）**：189.6 → 66 的 6060-6064/tcp 策略开通后，
   管理端页面注册 `azbx-cluster`（gRPC `http://10.60.185.66:6061` + token，
   归属应急厅项目）即在线；详情页 3 节点全部「可达」，CPU/内存指标实时
   上报；**中继验证**：66 `curl :6060/v2/` → 200（隧道打通，集群节点可经
   `10.60.185.66:6060/<repo>:<tag>` 从管理端内嵌仓库拉镜像，daemon.json
   已预置该 insecure-registries 条目）。

7. **worker 1.2.5→1.2.7（2026-08-21）**：补齐 push 大层 30s 502 修复
   （灾害集群记录 15）及 1.2.6 的节点统计/SSE/中继陈旧流等修复。
   - **首次尝试失败与教训**：无 SSH 通道时走了编排 API 全量替换（PUT
     `workloads/opsguard_worker`）——swarm 的 `ContainerSpec.Command` 是
     **完整可执行 argv**（不是 compose `command:` 那种拼在 ENTRYPOINT 后
     的 CMD 语义），载荷把 flags 当 Command 传 → `exec "-agent-config"
     not found` ×3 次重试耗尽 → 66 manager 任务挂死、集群失管约 20 分钟
     （89/86 未轮到，rolling pause；镜像源=66 worker 自身中继也随之中断）。
     API 全量替换另无法表达 hostname 模板。**该服务的 server 侧「编辑
     预填」快照仍是这次失败留下的错误 config——UI 编辑保存前勿直接用，
     用 CLI 管理**；待 translator 支持 hostname/完整 argv 后再修正。
   - **恢复与正确升级路径**（SSH 66）：`docker stack deploy -c
     /opt/opsguard/stack-azbx.yml opsguard` 还原原 spec（回 1.2.5、hostname
     恢复、3/3 运行）→ 三台 `docker pull 10.60.185.66:6060/library/
     opsguard-worker:1.2.7` 预拉（digest `7be086e4…` 与内嵌仓库一致）→
     66 上 `docker service update --image <中继全引用>` → **3/3 收敛**。
   - **镜像引用**改为中继全引用（swarm 按引用匹配本地已拉镜像，滚动期
     零拉取依赖）；66 上 stack 文件与仓内 stack.azbx.yml 已同步更新。
   - 已验证：三台启动日志 `version=1.2.7`（manager 隧道池 16 流 + 中继
     缓存 2048MB）；节点 SSE 3/3 `reachable:true`；azbx-cluster 在线。

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
