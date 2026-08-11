# 自然灾害服务迁移前置：服务间调用统一整改计划

> 版本：v0.1（草案）
> 日期：2026-08-11
> 状态：待评审
> 依赖：`08-服务间调用分析报告.md`（zrzh-project/doc/业务流程梳理/，2026-08-11 生成）

## 一、背景与结论

**为什么现在不能迁移**：`prdl-ycyj-servers` 内部服务间调用**大面积绕过服务发现**（8 对以上硬编码直连：`localhost`/内网 IP + 端口、K8s NodePort、`feign.app.*` 伪服务发现）。这些调用在 K8s 单节点部署时"碰巧可通"，但迁到 Docker Swarm 后：

| 现状写法 | Swarm 迁移后的后果 |
|---|---|
| `http://localhost:8084` 直连 | 容器各自独立网络，localhost 指向自身 → **全部失效** |
| `http://10.60.171.230:30004`（K8s NodePort） | 随 K8s 拆除 NodePort 消失 → **失效** |
| `feign.app.*` 硬编码地址（127.0.0.1:8083 等） | 地址与真实环境脱节 + 多副本/迁移失效 |
| 网关 `uri: http://localhost:8082`（lb:// 被注释） | 网关无法路由到新实例 |

**结论**：先完成服务间调用统一（内部调用一律走 Nacos 服务发现），在现网（K8s）验证通过后，再执行《K8s替换方案.md》的迁移。**整改是迁移的前置阻塞项。**

## 二、整改目标与原则

| 项 | 内容 |
|---|---|
| **目标态** | 本项目**内部**服务间调用 100% 走 Nacos 服务发现（`@FeignClient(name=...)` 无 url / 网关 `lb://`）；**外部**系统调用收敛到配置中心统一管理 |
| **原则 1** | 不改业务行为：只改调用方式，不改接口契约 |
| **原则 2** | 先整改、后迁移：整改在现网（K8s）验证 ≥1 周无回归，才启动 Swarm 迁移 |
| **原则 3** | 逐服务灰度：按被调用方依赖关系排序，先改"被调方先行注册"，再切"调用方" |
| **原则 4** | 外部系统调用（plan-service/user-center/fire-api/数仓/AI 等）**不阻塞迁移**，但一并收敛到配置中心 |

## 三、整改范围分级

### 3.1 迁移阻塞项（本项目内部调用，必须整改）

| # | 调用方 → 目标 | 现状 | 整改动作 | 代码位置（来自分析报告） |
|---|---|---|---|---|
| 1 | monitor → prdl-external-server | `feign.app.prdl-external-server` = `192.168.200.80:8082` | 删配置，Feign 走服务发现 | `ExternalFeignUrlSupport.java:17-24` |
| 2 | monitor → prdl-notice-server | `feign.app.prdl-notice-server` = `127.0.0.1:8083` | 删配置，Feign 走服务发现 | `NoticeFeignUrlSupport.java:17-24` |
| 3 | external → prdl-notice-server | `feign.app.prdl-notice-server` = `localhost:8083` | 删配置，Feign 走服务发现 | 同上 |
| 4 | data-server → prdl-monitor-server | HttpUtil 直连 `localhost:8084` ×3（pushWarning ×2、pushJudgmentWarning） | **恢复被注释的 `externalWarningService.pushWarning()` Feign**，删直连 | `DataSyncServiceImpl.java:855/1006/423` |
| 5 | data-server → prdl-monitor-server | HttpUtil 直连 `10.60.171.230:30004/external/getFireLevel` | 改走 Feign（`ExternalWarningService`） | `DataSyncServiceImpl.java:1384` |
| 6 | external-server → prdl-monitor-server | `${prdl-push-warning-server}` = `localhost:8084` ×2 | 改走 Feign | `FireWarningHandle.java:46`、`Schedule.java:72` |
| 7 | plans-server → prdl-external-server | `${external_url}` 默认 `localhost:8082` | 改走 Feign | `ExternalController.java:46` |
| 8 | prdl-gateway-server | 路由 `uri: http://localhost:8082`（lb:// 被注释） | 恢复 `uri: lb://prdl-external-server`（**注意注册名为 `prdl-external-server`，不是 `external-server`**） | `bootstrap-local.yaml:15-23` |
| 9 | admin-server → prdl-external-server | 未配 `feign.app.*`，空串回退走服务发现（依赖巧合） | 保留（删除伪发现机制后自然走服务发现），回归确认 | — |

**整改通用动作**：
1. 废弃 `ExternalFeignUrlSupport` / `NoticeFeignUrlSupport` 两个伪服务发现类及其 `feign.app.*` 配置；
2. `@FeignClient` 仅保留 `name`（`prdl-external-server`、`prdl-notice-server`、`prdl-monitor-server`），去掉显式 url；
3. 确认 Feign 依赖含 `spring-cloud-starter-openfeign` + `spring-cloud-starter-loadbalancer`（Swarm 多副本下需要负载均衡）；
4. 服务间内部调用**统一走现有 API 模块**（`prdl-monitor-server-api`）或 feign-modules（去 url）。

### 3.2 非阻塞项（外部系统，收敛配置中心即可）

| 项 | 现状 | 动作 |
|---|---|---|
| admin/data/plotting → user-center-service | `UserCenterFeign`（3 份重复）+ `${userCenterApi}` | 地址收敛到 Nacos 配置中心；合并 3 份重复 Feign 为共享模块 |
| admin/neo4j → plan-service（外部） | `${planSvcApi}` = `172.28.49.151:20000` | 收敛到配置中心 |
| plotting → notice-service（外部） | `${noticeApi}` = `localhost:8083` | 收敛到配置中心（注意：此处是外部 notice，与内部 prdl-notice-server 区分） |
| monitor → fire-api | `${fire-api.url}` | 收敛到配置中心 |
| 4.5 写死 IP 5 处（数仓/图层/AI/文件/管理端） | 散落 Java 代码 | 全部改为配置项 |

## 四、实施阶段与里程碑

```
M0 基线锁定 ──▶ M1 代码整改 ──▶ M2 现网验证(K8s) ──▶ M3 迁移(Swarm) ──▶ M4 迁移后回归
(1天)           (2~3天)          (≥1周观察)          (衔接K8s替换方案)     (1天)
```

### M0 基线锁定（1 天）
- [ ] 代码冻结：`prdl-ycyj-servers` 打基线 tag；
- [ ] 现网（K8s）确认 10 个业务服务当前注册实例数、端口、NodePort 全表（已核对，见 K8s替换方案 §1.2）；
- [ ] 从报告 §09-接口和定时任务 提取各服务核心接口清单，作为回归用例集；
- [ ] 确认服务注册名（Nacos）与代码 @FeignClient name 一致（重点：`prdl-external-server` vs 注释里的 `external-server`）。

### M1 代码整改（2~3 天）
按 §3.1 清单逐条改，**先改被调方（确认注册名与端口映射），再改调用方**：

- [ ] 第 1~3 条：删 `feign.app.*` 配置 + 废弃两个 UrlSupport 类（1 天，含编译+单测）；
- [ ] 第 4~7 条：恢复/新增 Feign 调用，删除 HttpUtil 直连（1 天）；
- [ ] 第 8 条：网关路由恢复 `lb://`（0.5 天）；
- [ ] 第 9 条：回归确认 admin 隐式发现不再依赖巧合；
- [ ] 编译全绿、单元测试通过、构建出新镜像（沿用现有 Harbor `nx/` 或内嵌仓库中继）。

### M2 现网验证（K8s 内，≥1 周观察）
- [ ] 新镜像滚动部署到现网 K8s（Rancher UI 或 kubectl 等效操作）；
- [ ] 回归核心接口（M0 清单），重点：monitor→external/notice、external→monitor、data→monitor、plans→external 的调用链；
- [ ] r-nacos 控制台（ns=test）确认各服务实例注册正常、`feign.app.*` 配置清除后无 404；
- [ ] 网关路由经 `lb://` 正常分发（多副本下负载均衡生效）；
- [ ] 观察 ≥1 周，业务无回归告警 → 冻结新基线。

### M3 迁移执行
按《K8s替换方案.md》§四批次与 SOP 执行（此时内部调用已全走服务发现，Swarm 下 `NACOS_ADDR=10.60.171.232:8848` 直连 r-nacos 即自动完成服务发现，无需再处理 localhost 直连）。

### M4 迁移后回归（1 天）
- [ ] 全部服务 Swarm 化后，r-nacos 实例数核对；
- [ ] 内部调用链回归（同 M2 用例集）；
- [ ] 前门（nginxwebui 200xx 入口）回归。

## 五、验证方法与验收标准

| 验证点 | 方法 | 通过标准 |
|---|---|---|
| 服务发现生效 | r-nacos 控制台看实例注册 + 调用方日志无直连地址 | 无 `feign.app.*`、无 localhost 直连调用日志 |
| 调用链功能 | M0 提取的核心接口回归 | 全部通过，与整改前一致 |
| 负载均衡 | 网关/Feign 调用在 ≥2 副本下轮询 | 后端各实例均有流量 |
| 无配置漂移 | grep 代码库：`localhost:8084`、`127.0.0.1:8083`、`feign.app`、`lb://` 注释 | 内部调用 0 处直连残留 |

## 六、风险与回退

| 风险 | 说明 | 对策 |
|---|---|---|
| 服务注册名不一致 | Feign name 与实际注册名不符 → 发现失败 | M0 先核注册名清单；`lb://prdl-external-server`（非 external-server） |
| 整改引入行为差异 | 直连→发现的 URL 路径/参数差异 | 回归用例集先行，差异项回退该服务到旧镜像，单独处理 |
| 外部系统地址 | 收敛到配置中心后需 Nacos 配一份 | 不阻塞迁移，M2 观察期内并行收敛 |
| LoadBalancer 依赖缺失 | 删 url 后无 lb 组件 → 启动报错 | 确认 spring-cloud-starter-loadbalancer 在依赖中 |
| **回退** | 任意服务整改回归失败 | 该服务回滚旧镜像（K8s Deployment 回滚），其余继续；M1 按服务原子提交 |

## 七、工作量与排期

| 阶段 | 内容 | 估时 | 依赖 |
|---|---|---|---|
| M0 | 基线锁定 + 用例集 | 1 天 | 代码库访问 |
| M1 | 代码整改 + 构建 | 2~3 天 | M0 |
| M2 | 现网灰度验证 | ≥1 周 | M1 |
| M3 | Swarm 迁移 | 2.5~3.5 天 | M2 + 中继 push 通道 |
| M4 | 迁移后回归 | 1 天 | M3 |

合计约 **2 周**（M2 观察期占大头）。建议 M1 整改期间代码冻结其他功能开发，避免冲突。

## 八、与既有文档的关系

- 前置依据：《08-服务间调用分析报告.md》（本计划所有整改清单的事实来源）
- 迁移执行：《K8s替换方案.md》——本计划 M3 直接衔接其 §四批次与 §五 SOP
- 镜像通道：《镜像隧道中继方案.md》——M1 新镜像的推送/拉取通道
- 注册中心：《注册配置中心-r-nacos-方案.md》——服务发现落点（r-nacos）
