# K8s（RKE）下线替换方案：业务微服务平移到 Swarm + OpsGaurd

> 目标：把自然灾害应急集群（10.60.171.x）上的 RKE K8s 整体下线，其承载的业务微服务**原样平移**到已在运行的 Docker Swarm，由 OpsGaurd 统一纳管。应用零改动、前门零改动、注册中心零改动。
>
> 盘点时间：2026-08-06，全部为线上实测（docker/iptables/nginx 配置直取），非推测。

---

## 一、现状盘点

### 1.1 集群拓扑：两套编排并存

```
                        ┌─────────────────── Swarm（5 节点，已在跑 OpsGaurd）───────────────────┐
                        │                                                                        │
  用户/运维             │   b-1(232)★Leader   b-2(230)      b-3(249)      YJ-4(253)   YJ-5(231)  │
  172.28.49.151 ───────►│   nginxwebui        ★K8s控制面     ★K8s控制面     Harbor      ★etcd单点  │
  (232 的外网映射)       │   grafana/loki      业务Pod×9      业务Pod×5      nacos-dm8    AI服务     │
                        │   rnacos/map-server                pr-micaps等   (已弃迁,流   (未核查)   │
                        │   ★r-nacos: 8848/9848        （注册中心已由253→232，见 §1.2 注）        │
                        └────────────────────────────────────────────────────────────────────────┘

  ★ = RKE K8s 组件：kube-apiserver/scheduler/controller-manager/kubelet/kube-proxy 在 b-1/b-2/b-3；
      etcd 单独在 231（--etcd-servers=https://10.60.171.231:2379，apiserver 启动参数实测）；
      Rancher v2.5.17 在 253（容器 rancher_v2517-rancher_server-1_old）。
      253 的 nacos-server-dm8 已停用：注册/配置中心 8-07 迁到 232 r-nacos，253 仅剩 DNAT 过渡桥。
```

### 1.2 K8s 里的业务负载（default 命名空间，全部实测）

| 服务 | 副本 | 所在节点 | 容器端口 | NodePort | 镜像（Harbor 253:20005） | 备注 |
|---|---|---|---|---|---|---|
| admin-server | 2 | 230 ×2 | 8080 | **30007** | library/admin-server | **同样挂宿主 `/data/dbte`**（k8s-dep.json，与 data-server 同） |
| data-server | 2 | 230 ×2 | 20081 | **30000** | nx/data-server:v202512111 | **挂宿主 `/data/dbte`** |
| monitor-server | 4 | 230 ×2 + 249 ×2 | 8081 | **30004** | monitor-server:v202507102（无项目前缀） | |
| plotting-server | 3 | 232 + 249 + 231 | 20083 | **30002** | library/plotting-server@sha256:fd02cbb2… | |
| mapcache-server | 3 | 232 + 249 + 231 | 18084 | **30005** | library/mapcache-server@sha256:eab4b676… | |
| neo4j-server | 1 | 230 | 8080 | **30008** | library/neo4j-server@sha256:2c40ffad… | 无数据卷（见风险 R3） |
| notice-test | 1 | 230 | 8083 | 30003 | library/notice-test | 测试 |
| external-test | 1 | 230 | 8082 | 30001 | library/external-test | 测试 |
| plan-server | 1 | 231 | 8080 | 30006 | nx/plan-server（231 本地 + Harbor nx 项目） | 非弃用（§4.1 批次⑥已确认，231 Running） |
| prdl-external-online-server | 1 | 231 | 8083 | 30009 | nx/external-online-server:v202503201 | 非弃用（§4.1 批次⑥已确认，231 Running） |
| minio | **0** | — | 9000/9011 | — | — | 只有 Service 无 Pod（见风险 R4） |

> 2026-08-21 复核注：副本数与镜像引用已按 k8s-*.json 资源转储修正（原表为 08-10 前快照：plotting/mapcache 曾记 2 副本、plan/external-online 曾记 0 副本"疑似弃用"）。P4 预拉取用的 digest 取自运行容器 `.Config.Image` 实况，与本表 Deployment spec 的 tag 引用并存不矛盾。

**所有业务 Pod 的自定义环境变量以 5 个为基础**（monitor-server 实为 7 个——多 `SPRING_CLOUD_NACOS_DISCOVERY_PORT=30004` 与 `SPRING_CLOUD_NACOS_DISCOVERY_IP` 空值；notice-test 7 个、external-test 6 个；其余为 K8s 自动注入的 service 变量）：

```bash
NACOS_ADDR=10.60.171.253:20011      # nacos-server-dm8（253 容器，8848→20011，9848→21011）
NACOS_NAMESPACE=test
spring.profiles.active=nacos        # 配置也从 nacos 配置中心拉
springdoc.swagger-ui.enabled=false
springdoc.api-docs.enabled=false
```

→ 服务发现与配置**全部走 nacos，不依赖 K8s DNS/Service**。这是平移能做到"应用零改动"的关键。

> 2026-08-07 更新：注册/配置中心已从 253 的 nacos-server-dm8 迁到 **232 的 r-nacos**（`10.60.171.232:8848`，详见《注册配置中心-r-nacos-方案.md》附录 B）。当前业务经 253 的 DNAT 桥转发到 232（过渡态）；**Swarm 迁移时 env 应直写 `NACOS_ADDR=10.60.171.232:8848`**，不再走 253，迁移完成后拆除 DNAT 桥。下文示例已按此更新。
>
> 2026-08-10 更新：r-nacos 控制台（`http://172.28.49.151:20005/rnacos/`）OAuth2 登录已对接 OpsGaurd IdP 打通（排障记录见《注册配置中心-r-nacos-方案.md》附录 C）——§五 SOP 中"控制台确认实例数"的操作面从旧 nacos（253:20011，已废）改到 r-nacos 控制台；8848 老控制台默认关闭，勿再访问 253:20011/nacos。

### 1.3 流量链路（北门）

```
用户 → 172.28.49.151:200xx → 232 nginxwebui（host 网络容器，真正的前门网关）
     → upstream 直打 10.60.171.230:3000x（K8s NodePort）→ kube-proxy iptables → 业务 Pod
```

- nginxwebui 按**端口**分流（无 server_name），listen 20003/20004/20005/20007/20014/20016/20017/20030/20031/21000/20000(ssl) 等，上游大多是 `230:30000~30009`。
- 部分上游指向**非 K8s** 目标，本次替换不受影响：231:20007/20008/3000/3300（AI 等）、249:20001（pr-fire-monitor/micaps）、232:20020/3000（grafana）、外部地址（10.60.114.2、10.60.171.176、172.28.40.6 等）。
- **ingress-nginx DaemonSet 占了每台 b 节点的 80/443，但集群里一条 Ingress 规则都没有**（controller 的 nginx.conf 只有默认 404 server）——纯占位，这就是之前"80 端口 404"的根由。（2026-08-21 复核：k8s-svc.json 里有 3 个 Ingress 派生 Service——ingress-860c…/9be8…/aec6…，ownerReferences 指向的 Ingress 对象已不存在，疑为删除残留，需上环境确认；不影响"80/443 被占"的结论。）

### 1.4 K8s 实际在干什么 vs 白付的成本

| K8s 在承担的（要接续） | K8s 白付的成本（要甩掉） |
|---|---|
| 跑 8 个 Java 微服务容器 | kube-apiserver/scheduler/controller-manager ×3 台常驻 |
| NodePort 30000-30009 端口暴露 | canal（calico+flannel）+ kube-proxy 的庞大 iptables 规则 |
| —（服务发现走 nacos，没用 CoreDNS） | etcd 单点压在 231（还混跑 AI 服务），风险高 |
| —（Ingress 零规则） | ingress-nginx 白占 80/443 |
| —（无 PVC，几乎无状态） | Rancher/cattle agent 每台常驻 |
| | 每 Pod 额外注入上百个无用 service 环境变量 |

**结论：K8s 在这套环境里只提供了"容器调度 + NodePort"两个职能，Swarm 完全覆盖，平移代价低、收益明确（释放三台机器的控制面开销、释放 80/443、消灭 etcd 单点、运维面统一到 OpsGaurd）。**

---

## 二、替换总体思路：三个"零改动"

| 层 | 手法 | 效果 |
|---|---|---|
| **应用零改动** | Swarm 服务用**同一镜像（digest 钉版）+ 同 5 个 env** | 照常注册到 r-nacos(232:8848, ns=test)、照常拉同一份配置 |
| **前门零改动** | Swarm 服务 published 端口 = **现 NodePort 同端口**（30000-30009） | nginxwebui upstream 不动、用户访问入口不动 |
| **注册零改动** | 注册/配置中心已就位：**232 r-nacos**（8-07 迁入完成，业务当前经 253 DNAT 过渡）；Swarm 新实例 env **直写 232:8848**，与老 K8s Pod（走 DNAT）同中心、无感知 | 消费方无感、双实例并存期 ns=test 同一中心内互相可见；最后一个服务迁完后拆除 253 的 DNAT 桥 |

**切换模式**：K8s NodePort 与 Swarm 发布端口在同一节点上不能共存（iptables 都会抢），所以每个服务按「**先无端口预热 → 停 K8s → 删 Service 释放端口 → Swarm 加端口**」四步切换，单服务中断窗口约 1~2 分钟，逐服务灰度，可回退。

---

## 三、迁移前置检查（动手前一次性做完）

```bash
# P1. 231（YJ-5）核查——它还裸跑着 AI 等服务（nginxwebui 上游指向 231:20007/20008/3000/3300），
#     拆 etcd 前必须确认这些容器及其端口清单；xshell 重连 231 后：
docker ps --format "{{.Names}} {{.Ports}}"

# P2. minio 下落确认（K8s 里只有 Service 无 Pod，业务 data-server env 引用了它）：
#     在 231/253/230 上找 minio 容器或进程；确认 data-server 实际连的 minio 地址。

# P3. /data/dbte 宿主目录分布（data-server 依赖，里面是 DBTE 密透配置）：
#     确认 b-1/b-3 是否也有该目录；没有则从 230 拷贝或把 data-server 约束在 230。
ls -ld /data/dbte          # 各 b 节点执行

# P4. 镜像预拉取测试（节点本地 /root/.docker/config.json 已有 253:20005 凭据，docker pull 直接可用）：
docker pull 10.60.171.253:20005/library/data-server@sha256:5688ab340c47b94bb5c8e19c39cc4a905e558572ccc02b709748a93ba1d5deed
#     每个 digest 从对应运行容器的 .Config.Image 取（见 §1.2 表，admin/monitor/notice/external 的 digest 用
#     docker inspect 补齐）。预拉取后 OpsGaurd 部署用 imagePullPolicy: missing 直接用本地镜像。

# P4b. （可选）镜像也可从管理端内嵌 registry 经 gRPC 隧道拉取——不开通「集群→管理端」策略，
#      方案与部署见《镜像隧道中继方案.md》（已实现，2026-08-10 真机验证）：
#     节点 /etc/docker/daemon.json 加 "insecure-registries": ["10.60.171.232:6060"]（需重启 docker 生效）；
#     镜像引用写 10.60.171.232:6060/<repo>:<tag>（repo 与内嵌仓库同名，如 opsguard-worker），
#     docker pull 即经 server↔worker 隧道拉取；worker 侧 blob 缓存（232 现配 2048MB）
#     使多节点同镜像只有第一份过隧道。

# P5. plan-server / prdl-external-online-server 已 0 副本——与业务确认是弃用还是停用；
#     弃用则不迁，K8s 拆除时一并清掉 Service。

# P6. Swarm overlay 网络：232 上已存在 `opsguard_default`（worker 部署时创建，8-08）；
#     业务服务直接复用该网络；若想独立命名再建 ops-net：
docker network create -d overlay ops-net
```

---

## 四、迁移顺序与部署模板

### 4.1 顺序（风险从低到高灰度）

| 批次 | 服务 | 理由 |
|---|---|---|
| ① 练手 | notice-test、external-test | 测试负载，切坏无感 |
| ② 单副本 | neo4j-server | 单副本，先确认 R3（数据归属） |
| ③ 核心双副本 | data-server、admin-server | 有 /data/dbte 依赖的先处理挂载 |
| ④ 多副本 | monitor-server（4） | 跨 230/249 |
| ⑤ 边界 | plotting-server、mapcache-server | 跨 232/249；mapcache 不注册 nacos（见 4.2.1 env 对照表） |
| ⑥ 收尾 | plan-server、prdl-external-online-server | 已确认要迁（在 231 Running，非弃用）；镜像在 231 本地与 Harbor nx 项目 |

### 4.2.1 逐服务 env 迁移对照表（2026-08-11，从 K8s Deployment spec 权威提取）

Rancher 部署时各服务靠**环境变量覆盖**注入 nacos 配置。迁移到 Swarm 时 env **原样照搬**，仅一处改动：`NACOS_ADDR` 从旧 nacos（253:20011，DNAT 过渡）改直连 r-nacos（`10.60.171.232:8848`）。以下为各 Deployment 的自定义 env（已剔除 K8s 自动注入的 `*_SERVICE_*`/`*_PORT_*` 变量）。

| 服务 | NACOS_ADDR（迁移后） | NACOS_NAMESPACE | spring.profiles.active | 其他 env（原样照搬） | 备注 |
|---|---|---|---|---|---|
| admin-server | `10.60.171.232:8848` | `test` | `nacos` | `springdoc.api-docs.enabled=false`、`springdoc.swagger-ui.enabled=false` | |
| data-server | `10.60.171.232:8848` | `test` | `nacos` | 同上 springdoc×2 | 另有 bind `/data/dbte` |
| monitor-server | `10.60.171.232:8848` | `test` | `nacos` | 同上 springdoc×2 + `SPRING_CLOUD_NACOS_DISCOVERY_PORT=30004` | ⚠️ **丢弃 `SPRING_CLOUD_NACOS_DISCOVERY_IP`**（转储中为空值；若 Rancher 表单出现字面 `undefined` 也一律不设，Swarm 取容器真实 IP） |
| neo4j-server | `10.60.171.232:8848` | `test` | `nacos` | 同上 springdoc×2 | R3：先确认无本地数据卷（真库疑似 253 `neo4j-neo4j-1`） |
| notice-test | `10.60.171.232:8848` | `test` | `nacos` | springdoc×2 + `logging.level.root=debug`、`userCenter.ifSendNoticeMsg=false` | 批次① |
| external-test | `10.60.171.232:8848` | `test` | `nacos` | springdoc×2 + `schedule-enable=true` | 批次① |
| plotting-server | `10.60.171.232:8848` | `test` | `nacos` | 同上 springdoc×2 | |
| plan-server | `10.60.171.232:8848` | `test` | `nacos` | 同上 springdoc×2 | 批次⑥ |
| prdl-external-online-server | `10.60.171.232:8848` | `test` | `nacos` | 同上 springdoc×2 | 批次⑥；镜像 `nx/external-online-server:v202503201` |
| **mapcache-server** | **无 NACOS env** | — | **`nx`** | `spring.data.mongodb.host=10.60.171.253`、`spring.data.mongodb.port=20016` | **不注册 nacos**——照搬原样，不带任何 NACOS_* 变量 |

> 共性：`NACOS_NAMESPACE=test`、`spring.profiles.active=nacos`、`springdoc.swagger-ui.enabled=false`、`springdoc.api-docs.enabled=false` 为 9 个 nacos 系服务通用。
> 迁移后双实例并存期：新 Swarm 实例（直连 232）与老 K8s Pod（走 253 DNAT→232）进**同一 r-nacos、同 ns=test**，互相可见，验证手法 = r-nacos 控制台看实例数翻倍。

### 4.2 OpsGaurd config.Service 部署模板（以 data-server 为例）

管理台 → `nxyj-cluster` 集群 → 容器与服务 → 部署服务，类别「业务」：

```yaml
service:
  name: data-server
  image: 10.60.171.253:20005/library/data-server@sha256:5688ab340c47b94bb5c8e19c39cc4a905e558572ccc02b709748a93ba1d5deed
  # 镜像来源二选一：① Harbor 253:20005（业务镜像所在，digest 钉版）；② 内嵌 registry
  # 中继 10.60.171.232:6060/<repo>:<tag>（经 gRPC 隧道，无需 Harbor 凭据，见 P4b/《镜像隧道中继方案.md》）
  replicas: 2
  imagePullPolicy: missing          # 用 P4 预拉取的本地镜像，绕开 swarm 拉取鉴权
  env:                              # 与 K8s Pod 一致，仅 NACOS_ADDR 改直连 232（r-nacos）
    - "NACOS_ADDR=10.60.171.232:8848"
    - "NACOS_NAMESPACE=test"
    - "spring.profiles.active=nacos"
    - "springdoc.swagger-ui.enabled=false"
    - "springdoc.api-docs.enabled=false"
  mounts:                           # 仅 data-server 有；其余服务删掉这段
    - { type: bind, source: /data/dbte, target: /data/dbte }
  networks: [ops-net]
  placement:
    constraints: [node.hostname==NXYJGLT-YJ-b-2]   # 见 4.3 端口策略；选 mesh 模式可放宽
  labels: { category: business, app: data-server, migrated-from: k8s }
  restart: { condition: any, delay: 5s }
  healthcheck:                      # 端口存活即可（Java 服务无独立 health 路径时）
    test: ["CMD-SHELL", "nc -z localhost 20081 || exit 1"]
    interval: 15s
    timeout: 5s
    retries: 5
    startPeriod: 60s                # Java 启动慢，给足
monitoring:
  portChecks: [{ port: "20081" }]
```

> 注意：**第一阶段不要填 `ports:`**——先让服务在 overlay 网络里跑起来（预热），验证后再加端口（见 §五）。

### 4.3 端口发布的两种模式（切换时二选一）

| 模式 | 做法 | 优点 | 代价 |
|---|---|---|---|
| **A. ingress 网格（推荐）** | `ports: [{ target: 20081, published: 30000 }]`（默认 mode: ingress） | nginxwebui 仍打 230:30000 不变；副本可调度到任意 b 节点，天然 HA | 端口在全部 5 个节点上打开（含 253/231），多一跳 mesh 转发 |
| B. host 钉节点 | `mode: host` + placement 钉 b-2 | 与现状逐字节一致，无 mesh 开销 | 副本只能落在 230，无节点级 HA |

建议：先用 A 跑通；后续想优化成"多节点 upstream"时，再改 nginxwebui 的 upstream 为 230/232/249 多 server（真 HA），那是独立优化项。

---

## 五、单服务切换 SOP（每个服务照做，约 10 分钟）

以 data-server（30000）为例：

```text
1. OpsGaurd 部署 swarm 服务（无 ports）→ 等健康检查通过
2. r-nacos 控制台（http://172.28.49.151:20005/rnacos/，OAuth2→OpsGaurd 统一认证 或 本地账号登录；
   旧 nacos 253:20011 控制台已废，8848 老控制台默认关闭）→ 服务管理 → ns=test：
   data-server 实例数从 2 → 4（2 老 K8s Pod + 2 新 swarm 容器）✔ 说明注册/配置拉取正常
3. 【中断开始】Rancher UI（http://10.60.171.253，admin）→ 集群 → default →
   Workload：data-server scale 到 0（等 2 个 Pod 消失）
   （没有 kubectl/kubeconfig；Rancher UI 是唯一 K8s 操作面。
     禁止只 docker stop 容器——Deployment 会重建，必须先 scale 0。）
4. Rancher UI → Service Discovery：删掉 data-server-nodeport
   【关键】只有删 Service 对象，kube-proxy 才会撤掉 30000 的 iptables 规则，端口才真正释放；
   仅 scale 0 规则仍在，swarm 发端口会冲突/分流。
5. OpsGaurd 更新 swarm 服务，加上 ports: [{target: 20081, published: 30000}]
   → 等容器就绪【中断结束，实测窗口约 1~2 分钟】
6. 验证：
   - curl http://10.60.171.230:30000/<健康路径> 通
   - nginxwebui 对应入口（200xx 端口）功能正常
   - r-nacos 控制台实例数回落到 2（新的两个）
7. 观察 ≥30 分钟无异常 → 迁移下一个服务
```

**回退（任意步骤出问题）**：OpsGaurd 删/停 swarm 服务 → Rancher UI 重建 nodeport Service（或先从同集群其他服务的 YAML 复制）→ Workload scale 回 2 → 1 分钟内恢复原状。

---

## 六、K8s/RKE 整体拆除（全部服务迁移完、观察 1~2 天后）

### 6.1 首选：Rancher UI 删集群
Rancher → 集群 → Delete。Rancher 会下掉 cattle-agent 并触发节点清理（对 RKE 集群的清理不彻底，仍需 §6.2 收尾）。

### 6.2 节点手工清理清单（b-1/b-2/b-3 各执行）

```bash
# 1. 删全部 K8s 容器（此时业务早已迁走，剩下的都是系统组件）
docker rm -f $(docker ps -aq --filter name=k8s_) \
  kubelet kube-proxy kube-apiserver kube-scheduler kube-controller-manager

# 2. 清 iptables（KUBE-*/CALI-* 链非常多，最干净的办法是直接重启机器；
#    不能重启则：）
iptables -t nat -F KUBE-SERVICES   # 及各 KUBE-* 链，逐条清；calico 同理
# 3. 删 CNI 残留网卡与目录
ip link del cni0 ; ip link del flannel.1    # cali* 网卡随容器删除已消失
rm -rf /etc/cni/net.d /var/lib/cni /run/calico /opt/cni
# 4. 删 K8s 数据目录
rm -rf /etc/kubernetes /var/lib/kubelet /var/log/kube-audit
# 5.（可选）重启 docker 与机器，确认 80/443/6443/10250/10257/10259 全部释放
ss -ltnp
```

### 6.3 etcd 节点（231）

```bash
# 先完成 P1 核查（231 上有 AI 等裸跑服务，别误伤）
docker rm -f etcd
rm -rf /var/lib/etcd
# 231 上其余容器一概不动
```

### 6.4 Rancher（253）

K8s 没了 Rancher 即失业。建议先 `docker stop rancher_v2517-rancher_server-1_old` 观察一周，无回退需求后删除。253 的 Harbor/nacos/ES/mongo 等全部保留。

### 6.5 80/443 释放后的用途规划

- r-nacos 控制台可改用 80（改三处：run-rnacos.sh 端口映射、rnacos.env 的 REDIRECT_URI、OpsGaurd IdP client 白名单——见《注册配置中心-r-nacos-方案.md》附录 C：REDIRECT_URI 必须指前端登录页路径 `/rnacos/p/login`，改端口时同步改 OpsGaurd IdP 白名单）；
- 或规划统一门户。此为可选项，不在本方案强制范围。

---

## 七、风险与对策

| # | 风险 | 说明 | 对策 |
|---|---|---|---|
| R1 | 无 kubectl/kubeconfig | 节点上没有 K8s 客户端，API 匿名 401 | 所有 K8s 侧操作走 Rancher UI（253）；切忌只 docker stop Pod 容器（会被重建） |
| R2 | 端口冲突 | scale 0 后 NodePort iptables 规则仍在，swarm 同端口会被抢 | SOP 第 4 步**删 Service 对象**才释放端口；先无端口预热把窗口压到 1~2 分钟 |
| R3 | neo4j-server 无数据卷 | 数据若在容器可写层，迁走即丢 | 迁移前确认它是纯代理（真库在 253 `neo4j-neo4j-1`）还是有本地数据；有数据先 dump |
| R4 | minio 下落不明 | K8s 只有 Service 无 Pod，data-server 引用它 | P2 前置检查定位（疑似 231 裸跑或已废弃）；废弃则随 K8s 一并清理 |
| R5 | /data/dbte 节点绑定 | data-server 读宿主 DBTE 密透目录 | P3 确认分布；必要时 placement 钉 230 或先行同步目录 |
| R6 | 231 混跑 | etcd 与 AI 等裸服务同机 | 迁移期不动 231；拆 etcd 只删 etcd 容器与 /var/lib/etcd |
| R7 | 镜像拉取鉴权 | swarm 服务拉 Harbor 需凭据 | 各节点 /root/.docker/config.json 已有凭据；**预拉取 + imagePullPolicy: missing** 彻底绕开；或改用内嵌 registry 中继（`10.60.171.232:6060`，经 gRPC 隧道，**无需 Harbor 凭据**，见 P4b/《镜像隧道中继方案.md》） |
| R8 | 双编排并存期资源 | b-1/2/3 同时跑两套控制面 | 迁移期短暂并存无碍（业务 Pod 此消彼长）；全部迁完立即拆 K8s 释放 |
| R9 | 命名空间混用 | 新老实例同 ns=test、同在 r-nacos 一个中心，短暂并存 | 预热阶段消费方可能打到新实例——新实例同镜像同配置，行为一致；这也是预热验证的一部分 |

## 八、验收清单

- [x] 11 个业务服务全部跑在 Swarm（含 mapcache/gateway 补迁），**r-nacos 控制台**（151:20005/rnacos/，ns=**prod**）实例数与各服务副本数一致（见 §10）
- [x] nginxwebui 各 200xx 入口功能回归通过（重点：20004/20014/20017 业务主入口、30003/ws 长连接）；nginx 配置零改动（Swarm 路由网格同端口接管）
- [x] OpsGaurd 管理台可见全部新服务；worker 1.2.5 全 5 节点在线（healthy 计数仅含 manager 节点任务，见 §10.2-7）
- [x] 5 节点均无 k8s_/kubelet/kube-proxy/kube-apiserver 容器，ss 无 6443/10250/10257/10259
- [x] 231 上 etcd 容器与 /var/lib/etcd 已删，231 裸跑服务（AI/kafka/minio 等）不受影响
- [x] 80/443/8443 已释放（ingress-nginx hostPort 随拆除消失）
- [x] Rancher 两台已停止（容器保留待观察）；iptables 无 KUBE-/CALI- 残留链（未重启节点，iptables-restore 过滤法）
- [x] 回退演练以 prod/test 双命名空间并存 5 天替代（老实例全程保留至拆除，未发生回退需求）

## 九、工作量估计

| 阶段 | 内容 | 估时 |
|---|---|---|
| 前置检查 | P1~P6 | 0.5 天 |
| 灰度迁移 | 8 个服务 × SOP（含观察） | 1~2 天 |
| 拆除清理 | Rancher 删集群 + 三节点清理 + 231 etcd | 0.5 天（不含观察期） |
| 回归验收 | 清单执行 + 回退演练 | 0.5 天 |

合计约 **2.5~3.5 天**，建议核心服务（data/admin/monitor）的切换安排在业务低峰窗口。

---

## 十、执行记录（正式迁移与 K8s 拆除，2026-08-11 ~ 08-16 完成）

> 本节为实测记录，与上文 SOP 的差异以本节为准。前置：r-nacos 迁移与镜像中继（gRPC 隧道双向化）已就绪，见《注册配置中心-r-nacos-方案.md》《镜像隧道中继方案.md》。

### 10.1 时间线

| 日期 | 动作 | 结果 |
|---|---|---|
| 08-11 | r-nacos prod 命名空间导入 10 份配置（8×DEFAULT_GROUP1 + external-online/plans 2×DEFAULT_GROUP）；10 个业务镜像（v2026081101）经 172.28.50.176:8080 直推内嵌仓库；经 OpsGaurd 部署 10 服务到 Swarm（prod ns） | 全部注册 prod、健康 |
| 08-12 | **正式切换**（Rancher 已死）：停 5 节点 kube-proxy + 清 KUBE-NODEPORTS → 发布 30000-30007 → mapcache 补部署（30005）；worker 升级 1.2.5 全 5 节点 | nginx 上游 30000-30010 全通 |
| 08-16 | **K8s 整体拆除**（230/249/232 控制面+Pod、231 etcd、253/231 rancher 停止）+ 残留清理（node-exporter/alloy/build-api/login-check/旧 worker） | K8s 归零，业务无影响 |

### 10.2 与 SOP 的关键差异（实测修正）

1. **Rancher 死亡改道**：etcd 卡死导致 Rancher UI 不可用，且节点上无 admin kubeconfig（仅 kube-proxy/kubelet 受限凭据），无法删 Service 释放 NodePort → 改为**停各节点 kube-proxy（restart=no）+ `iptables -t nat -F KUBE-NODEPORTS`**，端口即刻归 Swarm。
2. **nginx 零改动**：nginxwebui 全部业务反代指向 `10.60.171.230:3000X`；Swarm ingress 路由网格使**任一节点:发布端口自动负载均衡到全部副本**（跨节点），既无需改 nginx，也天然满足"不落单副本"。多副本分布实测：data(232,230) monitor(230,231,249) plotting(231,230,232) admin(232,253)。
3. **mapcache 补迁**：原 10 服务清单遗漏 mapcache（nginx 30005 入口）；镜像取自 232 现存 `mapcache-server:v202408312`（经运行容器 image ID tag），env 仅 mongo(253:20016)+profile nx，无 nacos。
4. **admin/data 必须 glibc 基础镜像**：DBTE 密态驱动 native 库 `libeyibc4jlite.so` 依赖 glibc libstdc++/libgcc_s（老 ABI 符号），alpine(musl) 加载必失败（UnsatisfiedLinkError: initEYIBC）。在 230 用旧镜像（adoptopenjdk glibc）为基座替换新 jar 重建，tag `v2026081101-glibc`，经 6060 中继 push。
5. **healthcheck 三类坑**：① 业务 Dockerfile CMD 把 `-Xmx256m -Xms256m` 写成单参数 → config `command` 覆盖修复；② glibc 镜像无 `nc` → `bash -c 'exec 3<>/dev/tcp/127.0.0.1/PORT'`；③ admin 启动 3-5 分钟 > 默认宽限 → startPeriod 300s + retries 10。
6. **30007 ingress 规则丢失**：端口发布后 dockerd 未下发 DOCKER-INGRESS 规则（spec 有、规则无）→ `docker service update --force admin-server` 补齐。
7. **worker 全节点化**：global worker 此前仅 232 在跑（其余节点缺镜像被拒）；镜像改引 `10.60.171.232:6060/library/opsguard-worker:1.2.5` 后 5/5 收敛。/v2 /idp-proxy 仅 manager 角色 worker 挂载（设计如此）。**已知限制**：OpsGaurd healthy 计数只含 manager worker 本机可见容器（`orchestrator/lifecycle.go:387` 本地 docker inspect），跨节点 healthy 汇聚为待改进项。
8. **隧道带宽优化前后**：优化前实测 ~35KB/s（75MB blob 4m52s 拉了 10.58MB）；裸链路 iperf3 实测 3.91Gbps，瓶颈在隧道代码；优化后 75.6MB blob 0.635s（≈119MB/s）。

### 10.3 K8s 拆除执行（§6 落地）

拆除前置验证：① 5 节点审计确认关键基础设施（DM 253:20011、redis 253:22030、neo4j 253:20001、kafka 231:20013、minio 231:20015、mongo、AI）**均为裸容器**非 K8s Pod；② r-nacos test 命名空间 9 服务订阅者=0；③ 前门抽样无回归（20017 根路径 502 为存量问题：后端 249:20001 本无监听，与迁移无关）。

每节点执行（日志留档 `/root/k8s_teardown.log`）：
```bash
docker update --restart=no kubelet && docker stop kubelet          # 先停 kubelet 防 Pod 重建
for c in kube-apiserver kube-scheduler kube-controller-manager kube-proxy; do docker update --restart=no $c; docker stop $c; done
docker ps -aq --filter name=k8s_ | xargs -r docker rm -f           # Pod 容器（cattle/ingress/旧业务）
iptables-save | grep -vE 'KUBE-|CALI-' | iptables-restore          # 只删 K8s 规则，保留 DOCKER-*/Swarm
ip link del cni0; ip link del flannel.1
rm -rf /etc/cni/net.d /var/lib/cni /run/calico /opt/cni /etc/kubernetes /var/lib/kubelet /var/log/kube-audit
```
- 231 追加：`docker rm -f etcd && rm -rf /var/lib/etcd`（AI/kafka/minio 一概未动）；`docker stop rancher-v2613`
- 253 仅 `docker stop rancher_v2517-rancher_server-1_old`（无其他 K8s 组件）
- 验证口径：k8s_/kube 容器 NONE、KUBE/CALI iptables 行数 0、6443/10250/10257/10259/8443/2379 FREE、Swarm 容器数不变

### 10.4 残留清理（08-16）

- 容器：`node-exporter`/`alloy`（230）、`build-api`（253）、`login-check`（232，其 30010 端口映射与 gateway 发布端口重合，删除后 gateway 独占）、9 个退出态旧版 worker 任务容器（全节点）
- 镜像：opsguard-worker:1.2.4、alloy/node-exporter/login-check/build-api（死仓 253:20005 引用）及 dangling
- 内嵌仓库：删除 relay 测试遗留 `library/build-api-test:v1`、`library/push-test:v1`
- 日志留档各节点 `/root/cleanup_done.log`

### 10.5 最终状态（2026-08-16）

**Swarm 11 服务**（网络 opsguard_default，注册 r-nacos prod 10.60.171.232:8848）：

| 服务 | 副本 | 端口 | 镜像 | 备注 |
|---|---|---|---|---|
| data-server | 2 | 30000→20081 | v2026081101-glibc | 挂载 /data/dbte（全节点都有） |
| external-server | 1 | 30001→8082 | v2026081101 | |
| plotting-server | 3 | 30002→20083 | v2026081101 | |
| notice-server | 1 | 30003→8083 | v2026081101 | 含 /ws 长连接 |
| monitor-server | 4 | 30004→8081 | v2026081101 | |
| mapcache-server | 1 | 30005→18084 | v202408312 | 老 glibc 镜像，hc=/dev/tcp |
| plans-server | 1 | 30006→8080 | v2026081101 | |
| admin-server | 2 | 30007→8080 | v2026081101-glibc | 启动 3-5 分钟，startPeriod=300s |
| neo4j-server | 1 | 30008→8080 | v2026081101 | |
| external-online-server | 1 | 30009→8083 | v2026081101 | |
| gateway-server | 1 | 30010→8080 | v2026081101 | 新增入口 |

另：opsguard_worker global 5/5（1.2.5，08-16 快照；后续 08-18/08-20 已升至 1.2.6→1.2.7，见 `deploy/offline/README.md` 记录 13/15，manager 角色承担隧道中继）；maxkb4j-app/pgvector、rnacos、nginxwebui、mongo、DM/redis/neo4j/kafka/minio/AI 等裸容器不受影响。

**验证口径**：`230:30000-30010` 全应答；r-nacos prod 10 服务全健康（test ns 归零）；前门 20004/20014/20030/20003/20002/20013/20020 与基线一致。

### 10.6 遗留事项

- [ ] rancher 容器两台（231 rancher-v2613、253 rancher_v2517-rancher_server-1_old）已停止未删除，观察后删
- [ ] 253 `/data/harbor` 死数据清理（镜像已迁内嵌仓库）
- [x] OpsGaurd healthy 跨节点汇聚（已实现 2026-08-17，git f55a0f7，方案见《healthy跨节点汇聚方案.md》，deploy README 记录 13 已验证）
- [ ] 20017 根路径 502（后端 249:20001 不存在，存量问题，与业务方确认是否有用）
- [ ] 80/443（原 ingress-nginx hostPort）已释放，可按 §6.5 规划改用途
