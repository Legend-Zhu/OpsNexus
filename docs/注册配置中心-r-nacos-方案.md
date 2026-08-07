# 注册中心 / 配置中心轻量替代方案（r-nacos）

> 版本：v0.1（草案）
> 日期：2026-08-06
> 状态：调研完成，待试点验证

## 一、背景与目标

平台上集群部署的业务服务目前使用 **Nacos** 做服务注册发现与配置中心。痛点：**Nacos server 资源占用过大**（JVM，动辄 1GB+ 内存），与 OpsGaurd"轻量一体化"的定位相悖。

**硬约束：不改业务程序。** 业务服务已集成 Nacos 客户端 SDK（注册发现 + 配置），不允许更换 SDK 或改造代码，最多接受调整部署配置（`server-addr` 指向）。

**目标**：用资源占用低一个数量级的服务端替换 Nacos server，业务侧零感知完成迁移，并将注册/配置中心沉淀为 OpsGaurd 平台的一等内置组件。

## 二、决策与选型

"不改程序"意味着替代服务端必须讲 **Nacos 私有协议**（1.x HTTP OpenAPI / 2.x gRPC）。Consul、etcd 等换协议的方案在此约束下全部排除；自行实现 Nacos 协议代理层工作量大、兼容性风险高，不推荐。

| 方案 | 协议兼容 | 资源占用 | 配置中心 | 结论 |
|---|---|---|---|---|
| **r-nacos**（Rust，nacos-group 官方生态） | ✅ 1.x HTTP + 2.x gRPC | 单二进制 10MB+，运行内存 ~15MB（5k 配置 + 450 实例实测） | ✅ 完整（历史记录/导入导出） | **选定** |
| Nacos 瘦身（JVM 调参 + Derby + 关模块） | 原生 | 可压至 ~500MB | 原生 | 兜底，非方向 |
| Consul / etcd | ❌ 需改程序换 SDK | 低 | 弱/无 | 排除 |
| 自研 Nacos 协议代理 | 理论上 | — | — | 排除（成本/风险） |

**结论：选 r-nacos。** nacos-group 组织下的 Apache-2.0 项目，Rust 重写 Nacos server，协议级兼容，业务零改动平迁。

## 三、r-nacos 关键事实

> 来源：github.com/nacos-group/r-nacos（Star 1.6k，提交 1100+，持续活跃）。

- **协议兼容**：同时兼容 1.x HTTP OpenAPI 与 2.x gRPC 客户端协议。已验证 SDK：
  - Java：HTTP 协议 ≥ 1.4.x；gRPC 协议 ≥ 2.1.x
  - Go：nacos-sdk-go/v2 ≥ v2.2.5
  - Rust：nacos-sdk 0.3.3+ / nacos_rust_client 0.3.0+
- **资源占用**：包体积 10MB 出头，无 JVM 依赖；空闲 CPU < 0.5%、内存 < 5MB；官方演示环境（近 5000 配置 + 450 服务实例）内存约 **15MB**。Docker 镜像 alpine 版解压后 34MB。
- **性能**（官方数据）：配置写 1.76 万 tps / 读 8 万 qps（集群 n×8 万）；实例注册（HTTP）4.8 万 qps；gRPC 心跳 8 万+。
- **集群与持久化**：**Raft + 节点本地存储**，不依赖 MySQL（类 etcd）；单机即单节点集群，可随时扩容加入新节点；备份 = 拷贝 `RNACOS_CONFIG_DB_DIR`（nacos_db 目录）。
- **配置中心**：基础功能完整、历史记录、导入导出（**文件格式与 Nacos 兼容**，迁移直接导出/导入）。
- **注册中心**：基础功能完整；2.x gRPC 实例变更实时通知支持。
- **控制台**：独立端口 10848（0.4.0+），含用户管理/角色权限（管理员/开发者/访客）、LDAP、OAuth2.0、登录失败锁定；8848 老控制台已废弃。
- **其他**：提供备份接口（需配 32 位以上 `RNACOS_BACKUP_TOKEN`）；自带 MCP Server 能力（可将注册的 HTTP 接口转为 MCP 服务，与平台 Agent 方向契合，后续可挖掘）。

## 四、已知边界（迁移前自查）

| 项 | 影响 | 应对 |
|---|---|---|
| ❌ 配置灰度发布、tag 隔离、tag 高级查询 | 使用了 Nacos 灰度发布的服务 | **迁移前盘点，有则评估改造或该服务暂缓** |
| ❌ 配置/实例的监听记录查询 | 排障辅助手段缺失 | 可接受 |
| ❌ 1.x UDP 实例变更实时通知 | 仅影响 1.x 老客户端，靠轮询兜底（时效略降） | 优先确认客户端版本；2.x gRPC 通知不受影响 |
| ⚠️ gRPC 实例注册性能未实际压测（官方为理论值） | 大规模实例集中注册 | 试点期实测验证 |
| ⚠️ Linux 默认下载包为 musl 版，性能弱于 gnu 版 | 生产性能 | 生产用 gnu 版或官方 Docker 镜像 |

**试点前必查清单**：

1. 业务服务的 nacos-client 版本分布（1.x HTTP 还是 2.x gRPC——两者都兼容，但需确认 ≥ 1.4.x / ≥ 2.1.x）。
2. 是否使用配置灰度发布 / tag 隔离（用了则该服务单独评估）。
3. 是否启用 Nacos 鉴权（r-nacos 支持 username/password 认证，需同步账号配置）。
4. 是否依赖冷门 OpenAPI（控制台人工操作类接口不影响业务，仅影响运维习惯）。

## 五、迁移路径（业务零停机）

```
 Nacos ──┐                    ┌── Nacos（缩小保留，观察期）
         │ 双中心并存（灰度）  │                              ┌── 下线 Nacos
         ├── 业务分批切换 ───▶│  全部切换完成，观察 1~2 周 ──▶│
         │                    └── r-nacos（Raft 3 节点）      └── r-nacos
 r-nacos ┘
```

1. **并跑**：r-nacos 上线（先单机验证，生产按 Raft 3 节点），与现有 Nacos 并行，互不影响。
2. **配置搬迁**：Nacos 控制台导出配置 → r-nacos 控制台导入（文件格式兼容）；抽样比对关键配置内容。
3. **注册灰度**：注册数据**不搬迁**——业务服务 `server-addr` 指向 r-nacos 后滚动重启，实例自然重新注册。
   - 按服务分批切换，每批观察注册/心跳/实例通知正常后再切下一批。
   - 存在跨服务互调的，同批切换或放最后一批，避免跨中心不可见。
4. **验证**（见 §八），重点：配置热更新、实例上下线通知时效、鉴权。
5. **观察 1~2 周**，无异常后下线 Nacos。

**回退**：任意时刻把 `server-addr` 改回 Nacos 即完成单服务回退，无数据损失。

## 六、与 OpsGaurd 平台集成

r-nacos 应沉淀为**平台一等内置组件**，而非用户手工部署的中间件：

### 6.1 部署形态

- 单二进制打成小镜像（alpine 基底，解压后 34MB），上传至平台**内嵌 registry**（复用已上线的 OCI /v2 仓库与传包构建能力）。
- 通过 Worker 编排通道以 stack 部署到 Swarm，部署描述复用 `config.Service` 契约。
- 参考 stack（3 节点 Raft）：

```yaml
version: "3.8"
services:
  rnacos:
    image: <内嵌registry>/rnacos:v0.8.x
    environment:
      RNACOS_RAFT_NODE_ID: "{{.Task.Slot}}"        # 1/2/3 按副本序号
      RNACOS_RAFT_NODE_ADDR: "0.0.0.0:7848"
      RNACOS_RAFT_AUTO_INIT: "1"                   # 首节点自初始化，其余 join
      RNACOS_HTTP_PORT: "8848"                     # 客户端协议端口（兼容 Nacos）
      RNACOS_CONSOLE_PORT: "10848"                 # 新控制台
      RNACOS_CONFIG_DB_DIR: /data/nacos_db
      RNACOS_BACKUP_TOKEN: <32位以上随机串>
    volumes:
      - rnacos-data:/data
    networks:
      - ops-net
    deploy:
      replicas: 3
      placement:
        constraints: [node.role == manager]
      # 注意：Swarm VIP 模式下 9848(gRPC) 长连接在部分场景有连通性问题，
      # 客户端走 2.x gRPC 时建议 endpoint_mode: dnsrr，由客户端直连各实例。
volumes:
  rnacos-data:
networks:
  ops-net:
    external: true
```

### 6.2 业务接入（平台注入）

- 业务服务部署时，平台自动注入注册中心地址（env `NACOS_ADDR` / JVM 参数）并挂载到同一 overlay 网络，业务零配置接入。
- 地址注入逻辑并入现有编排下发链路（`config.Service.Env`）。

### 6.3 巡检联动

把注册中心健康纳入 patrol 巡检闭环（复用已有 check/flow 能力）：

| 巡检项 | 类型 |
|---|---|
| r-nacos 进程/端口存活（8848/10848/7848） | portCheck |
| Raft 集群状态（节点数、leader 存在） | httpCheck（OpenAPI） |
| 关键服务实例数低于预期 | httpCheck + alertrule |
| 实例不健康比例超阈值 | httpCheck + alertrule |

### 6.4 页面集成

server 调 r-nacos 的 Nacos OpenAPI（协议兼容 ⇒ API 兼容）拉取服务/实例清单，在 cluster/项目视图中展示（实例数、健康状态、元数据）；控制台入口（10848）可嵌入平台导航。

### 6.5 控制台 SSO 对接 OpsGaurd IdP（gRPC 隧道，免反向防火墙）

r-nacos 控制台支持 OAuth2 登录（`OAUTH2_*` 环境变量）。对接 OpsGaurd IdP 后，用户用 OpsGaurd 账号（admin 等）单点登录 r-nacos 控制台，无需在 r-nacos 重复建账号。

**网络背景**：disaster 集群（`10.60.171.x`）与管理端（`10.60.189.6`）网段隔离，防火墙只开 `189.6→171.232:6060-6064` 单向，反向不通。直接让 r-nacos 访问管理端 IdP 需开反向防火墙——成本高。OpsGaurd 提供 **gRPC 反向隧道**（复用管理端→Worker 长连）绕开此约束，详见 `docs/IdP-接入指南.md` 第七章。

**部署分两阶段**：

**阶段一：r-nacos 本地账号跑通**（不依赖 IdP）。镜像离线导入 + 单副本部署（见下方部署模板），用 r-nacos 内置账号登录，立即可用。

**阶段二：对接 IdP**。前置：管理端启用 IdP（`idp.enabled`，issuer 填公网地址如 `http://172.28.50.176:8080`）；Worker 配隧道 env（`OPSGUARD_TUNNEL_BASE=http://10.60.171.232:8080`、`OPSGUARD_IDP_PUBLIC_ISSUER=http://172.28.50.176:8080`）。然后在管理台「身份提供者」注册 client（`rnacos-console`，机密，回调 `http://10.60.171.232:10848/<r-nacos回调路径>`），最后更新 r-nacos 服务的 env 取消注释：

```yaml
env:
  # ... 其余不变
  - "OAUTH2_ENABLE=true"
  - "OAUTH2_ISSUER=http://10.60.171.232:6060/idp-proxy"   # Worker 本地隧道入口（worker http 端口 6060）
  - "OAUTH2_CLIENT_ID=cli-rnacos"
  - "OAUTH2_CLIENT_SECRET=<注册得到的一次性 secret>"
```

r-nacos 拉 discovery 时拿到改写后的端点：`authorize`→公网管理端（浏览器跳），`token`/`jwks`/`userinfo`→Worker 隧道地址（服务端走）。登录流程自洽，全程不开反向防火墙。

**部署模板（实测版，2026-08-07 disaster 集群验证）**：原 §6.1 的 stack yaml 是 docker-compose 语法，不能直接用。下方是实测可用的部署方式。注意三个实测要点：
1. **镜像名是 `qingpan/rnacos`**（不是 rustack），最新 `stable` tag（v0.8.6）
2. **必须 `--security-opt seccomp=unconfined`**：r-nacos 的 Rust tokio runtime 起线程会被 docker 默认 seccomp 拦截（panic exit 101）。OpsGaurd 的 `config.Service` 暂未暴露 security_opt，故试点用 `docker run`（非 swarm service）部署。
3. **控制台端口是 10848**（v0.8.x，非旧版 10010）；工作目录 `/io`，数据卷挂 `/io`。

```bash
# 232 上（disaster swarm leader）。镜像先离线 docker load。
docker volume create rnacos-data
docker run -d --name rnacos --restart=always \
  --security-opt seccomp=unconfined \
  -e RNACOS_CONFIG_DB_DIR=/io/nacos_db \
  -e RNACOS_RAFT_NODE_ID=1 \
  -e RNACOS_RAFT_NODE_ADDR=0.0.0.0:7848 \
  -e RNACOS_RAFT_AUTO_INIT=1 \
  -e RNACOS_BACKUP_TOKEN=<32位随机串> \
  -v rnacos-data:/io \
  -p 8848:8848 -p 10848:10848 -p 9848:9848 -p 7848:7848 \
  qingpan/rnacos:stable
```

端口约定（v0.8.6 启动日志确认）：`8848` OpenAPI、`10848` 控制台、`9848` Nacos2 gRPC、`7848` Raft。健康检查：`curl http://localhost:8848/nacos/v1/ns/operator/metrics` 返回 200。

**配置外置（配置变更免重建容器）**：r-nacos 应用层配置（基础 + OAuth2）外置到宿主机 `/opt/rnacos/rnacos.env`，容器以 `--env-file /opt/rnacos.env` 只读挂载。改任意配置（OAuth2 回调、端口、密钥等）只需：
```bash
vim /opt/rnacos/rnacos.env        # 改配置
docker restart rnacos             # 生效，无需重建
```
启动脚本固定为 `/opt/rnacos/run-rnacos.sh`（含 seccomp、卷、端口映射、`--env-file`）。只有改 **docker 端口映射**（`-p`）才需重跑该脚本重建容器。

**改控制台端口（如 10848 → 新端口）的操作**：涉及三处，必须同步改：
1. docker 端口映射：改 `/opt/rnacos/run-rnacos.sh` 里的 `-p 10848:10848`（改 published 端口，宿主侧）→ 重跑脚本重建容器。
2. r-nacos OAuth2 回调：改 `/opt/rnacos/rnacos.env` 的 `RNACOS_OAUTH2_REDIRECT_URI`（端口部分）→ `docker restart rnacos`。
3. OpsGaurd IdP client 白名单：管理台「系统设置 → 身份提供者」编辑 `rnacos-console` 的 redirect_uri 端口（或 API `PUT /api/v1/idp/clients/<id>`）。redirect_uri 须与 REDIRECT_URI 精确一致，否则 authorize 报 `invalid redirect_uri`。

> 原 OpsGaurd `config.Service` 部署模板（含 healthcheck/monitoring/resources）见下，**但需先在 config.Service 增加 securityOpt 字段支持**（待实现），否则 r-nacos 会 panic：

compose → OpsGaurd 字段对照（避免踩坑）：

| compose（§6.1 原文） | OpsGaurd config.Service |
|---|---|
| `deploy.replicas: 3` | `replicas: 1`（顶层，试点先单副本） |
| `deploy.placement.constraints: [node.role==manager]` | `placement.constraints: [node.role==manager]` |
| `deploy.endpoint_mode: dnsrr` | **不支持**（OpsGaurd 未暴露 endpoint_mode）；单副本+host 模式规避 |
| `environment: KEY: VAL` | `env: ["KEY=VAL"]`（字符串数组） |
| `volumes: rnacos-data:/data` | `mounts: [{type: volume, source: rnacos-data, target: /data}]` |
| `networks: [ops-net]` | `networks: [ops-net]`（需先建 overlay 网络） |

## 七、兜底方案

若试点撞到协议兼容边角（极小概率，如冷门 OpenAPI 行为差异）：**Nacos 瘦身并跑续命**——单机模式 + 内嵌 Derby + JVM `-Xms256m -Xmx512m` + 关闭非必要模块，可压至 ~500MB。此为减配续命，不是方向；兼容问题应优先向 r-nacos 社区反馈修复。

## 八、试点验证清单与行动项

**试点验证**（拿 1 个非核心服务，预计 1~2 天出结论）：

- [ ] 服务注册/反注册正常，控制台可见
- [ ] 实例上下线通知时效（对端感知延迟秒级内）
- [ ] 配置读取、**配置热更新推送**（改配置观察业务无重启生效）
- [ ] 鉴权开启后业务正常连接（如使用）
- [ ] 客户端 2.x gRPC 走 dnsrr 模式连通性（Swarm 网络下）
- [ ] 资源占用实测记录（对比 Nacos 基线）

**后续行动项**：

1. 盘点业务服务 nacos-client 版本与高级特性使用情况（§四自查清单）
2. r-nacos 镜像构建 + 上传内嵌 registry + stack 模板入库
3. 平台侧：地址注入改造、巡检项配置、OpenAPI 对接页面（可分迭代）
4. 试点 → 灰度 → 全量 → 下线 Nacos（按 §五节奏）
