# OpsGaurd 场景操作手册

> 按**真实运维任务**组织：一个场景 = 一件从头到尾做完的事。每个场景独立成篇——什么时机做、前置是什么、逐步怎么点、怎么验证、坑在哪，可直达查阅。
> 架构与设计原理见《OpsGaurd-系统技术总览.md》，本文只讲操作。
>
> 版本快照：2026-08-21，管理端（server）1.2.12 / Worker 1.2.7。「」内为页面按钮/字段原文，`→` 为菜单/页面路径，关键处附 `文件:行号` 便于回溯。

## 场景目录

| # | 场景 | 谁来做 | 频率 |
| --- | --- | --- | --- |
| 1 | 新业务从零上线（上传镜像 → 建项目 → 接入集群 → 部署含监控） | 平台管理员 / 实施人员 | 每个新业务一次 |
| 2 | 发布新版本与服务生命周期管理（更新/扩缩容/重启/下线） | 业务运维 | 日常 |
| 3 | 纳管 swarm 之外的中间件与宿主机服务 | 业务运维 | 按需 |
| 4 | 镜像仓库日常治理（CLI 直推/删除/保留策略） | 平台管理员 | 日常 |
| 5 | 让告警通知到人（通知渠道 + 策略） | 平台管理员 | 一次性 + 变更 |
| 6 | 值班巡屏与告警处置 | 值班人员 | 每日 |
| 7 | 周期智能巡检与 AI 报告投递 | 平台管理员配置，自动执行 | 一次性 + 周期 |
| 8 | 接入 AI 模型并启用排查网关 | 平台管理员 | 一次性 |
| 9 | AI 异常排查 | 值班/业务运维 | 按需 |
| 10 | MLOps 运营（提示词定制/模型治理/费用预算） | 平台管理员 | 周期 |
| 11 | 账号与登录管理（改密/建用户/SSO） | 平台管理员 + 用户自助 | 日常 |
| 12 | 向集群内系统签发 OIDC 身份（IdP） | 平台管理员 | 每个接入系统一次 |

场景间依赖：1 → 2/3/6/9 依赖其产物；5 是 6/7 收到通知的前提；8 是 7（AI 报告）/9/10 的前提；12 依赖管理端 `idp.enabled`。

## 使用约定

- 角色：**admin**（管理员）与 **viewer**（只读）两级。admin 限定：用户管理、身份提供者（IdP clients）、AI 网关配置写、MLOps 写操作（提示词/单价/模型启停/绑定/预算）。其余（项目/集群/部署/告警/巡检/通知/密钥）登录用户即可操作。详见附录 B。
- 登录：本地账号密码或企业 SSO（见场景 11）；首次部署播种默认账号 `admin / opsguard-admin`。
- 平台网络方向：一切通信由**管理端 → 集群 manager Worker** 单向发起，集群无需反向访问管理端。

---

# A. 业务交付

## 场景 1：新业务从零上线

**任务**：把一个新业务从构建产物变成集群里带监控运行的服务。完整走完 镜像 → 项目 → 集群 → 部署 四步。

**前置**：管理端已部署可登录；目标集群已装 Docker 并完成 `docker swarm init`/`join`；管理端到集群 manager 的 Worker **gRPC 端口（默认 9080）与 HTTP 端口（默认 8080，`/mcp`+`/healthz`）**都网络可达（端口可自定义，接入时按实际端口填写）；各机时钟同步。

### 1.1 上传镜像

**路径**：「镜像仓库」。先看页头状态：`docker 可用`（绿，才能页面构建）、`已启用认证`。若出现「镜像仓库未启用」黄色告警条（el-alert warning 样式）→ server 配置 `registry.enabled: true` 后重启。

在「上传构建」卡片：

| 字段 | 填写 |
| --- | --- |
| 构建包 | `.zip`，内含 Dockerfile（任意子目录自动定位，构建上下文 = Dockerfile 所在目录），≤500MB |
| 项目名 | 镜像命名空间，选已有或输入新名（如 `ops`） |
| 镜像名 | 如 `myapp`（不含前缀） |
| Tag | 如 `v1.0.0` |

命名仅允许小写字母/数字/`_ . -`。点「开始构建」后异步执行，进度条状态依次 PENDING → EXTRACTING → BUILDING → PUSHING → CLEANING → SUCCESS（整体超时 30 分钟），失败看日志框。

**验证**：「镜像列表」展开仓库看到该 Tag；点「复制拉取命令」备用——去掉 `docker pull ` 前缀就是稍后的 `image` 值。

### 1.2 新建项目

**路径**：「项目」→「新建项目」。字段：`名称`（必填、全局唯一，如 `核心交易域`）、`描述`（可选）。「保存」即可，项目在下一步接入集群时被引用。

### 1.3 接入集群

分两段：**目标集群部署 Worker（每个集群一次）**，然后**页面注册**。

**① 部署 Worker**（完整 runbook 见 `deploy/offline/README.md`）：

1. 各节点放置 `/etc/opsguard/agent-config.yaml`（模板 `Worker/deploy/agent-config.yaml.example`，**全节点逐字节一致**；生产必配 `auth.enabled: true` + `auth.tokens: <名字>: "<secret>"`），并 `mkdir -p /var/lib/opsguard`。
2. 各节点 `docker load` Worker 镜像 tar。
3. manager 节点 `docker stack deploy -c stack.yml opsguard`（swarm global 模式，每节点一个 Worker）。
4. 验证：`curl http://<manager-IP>:<HTTP端口>/healthz` 返回 ok；`docker service logs opsguard_worker` 中 manager 节点出现 `worker role=manager`；防火墙放通管理端 → manager 的 **gRPC 端口（默认 9080）与 HTTP 端口（默认 8080）**（生产常自定义，如 6061/6060，接入时两个端口都会探测）。

**② 集群节点接入镜像仓库**（每个集群节点一次，为部署拉镜像铺路）：

| 模式 | 适用 | 节点配置（/etc/docker/daemon.json） | 部署时 image 写法 |
| --- | --- | --- | --- |
| **隧道中继（推荐，隔离集群）** | 节点连不上管理端 | `{"insecure-registries": ["<manager-IP>:<Worker HTTP端口>"]}`，全节点统一（含 manager 本机），改完 `systemctl restart docker` | `<manager-IP>:<Worker HTTP端口>/<项目名>/<镜像名>:<tag>`，如 `10.60.171.232:6060/ops/myapp:v1.0.0` |
| 直连 | 节点可达管理端网段 | `insecure-registries` 配 `<registry.hostname>:<管理端端口>`，`/etc/hosts` 把主机名解析到本网段管理端 IP | `<registry.hostname>:<管理端端口>/<项目名>/<镜像名>:<tag>`（永远用统一主机名） |

中继模式前提：管理端 `idp.enabled: true`（隧道随 IdP 建立）+ Worker 配置 `OPSGUARD_TUNNEL_BASE=http://<manager-IP>:<HTTP端口>`（离线 stack 模板已带）。中继模式无需 docker login，basic auth 由管理端 relay 账号代持。

**③ 页面注册**：「集群」→「接入集群」：

| 字段 | 填写 |
| --- | --- |
| 集群名称 | 字母/数字开头，可含 `. _ -`，≤63 字符；**创建后不可改名** |
| 所属项目 | 选 1.2 建的项目（可选） |
| Manager 地址 | `http://<manager-IP>:<gRPC端口>`（**是 gRPC 端口**，MCP 端点自动推导，勿填 HTTP 端口） |
| Token | 与 Worker `auth.tokens` 的 secret 一致；Worker 未开鉴权可留空 |
| 描述 | 可选 |

「接入」时管理端立即探测（5 秒超时，要求对端是 swarm manager）；失败弹 502 含原因，成功后列表状态「在线」。

### 1.4 部署服务（含监控）

**路径**：「集群」→ 点集群名进详情页 → 头部「部署」。

对话框只有三项：`集群`（只读）、`类别`（服务 / 中间件，决定出现在哪个 Tab）、`配置`——**一整份 YAML**（`service:` + 可选 `monitoring:`），必填仅 `name` 和 `image`。常用完整示例：

```yaml
service:
  name: myapp                      # 小写字母/数字开头，可含 _ . -，≤63
  image: 10.60.171.232:6060/ops/myapp:v1.0.0   # 写法见 1.3 ②；中继模式无需 registryAuth
  replicas: 2
  labels: { category: service }    # 与「类别」一致：service / middleware
  ports:
    - { target: 8080, published: 18080, mode: host }  # host=任务节点直接监听；ingress(默认)=任意节点可访问
  env:
    - TZ=Asia/Shanghai
  mounts:
    - { type: bind, source: /data/app, target: /data }
  resources:
    limits: { cpu: "1.0", memory: 512Mi }
  healthcheck:                     # 配置后部署收敛会等容器健康
    test: ["CMD-SHELL", "curl -sf http://localhost:8080/health || exit 1"]
    interval: 10s
    timeout: 3s
    retries: 3
  update: { parallelism: 1, delay: 10s, failureAction: pause }
monitoring:                        # 部署后探针立即自动注册
  enabled: true
  portChecks:                      # TCP 探测，连续失败 → port_down(error)
    - { port: "18080", interval: 10s, timeout: 3s, retries: 2 }
  httpChecks:                      # 连续 2 次失败 → http_unhealthy(error)；localhost 自动重写为任务节点 IP
    - { url: "http://localhost:8080/health", expectedStatus: [200], interval: 15s, timeout: 5s }
  logChecks:                       # 日志正则；action=alert 告警 / restart 自动重启
    - { pattern: "ERROR|Exception|panic", level: error, action: alert }
  resourceThresholds:              # 15s 采样多副本均值；超限 resource_over(warn)、回落自动恢复
    - { metric: cpu, threshold: 80, action: alert }
    - { metric: memory, threshold: 85, action: alert }
```

更多字段（command/args、placement、secrets/configs、imagePullPolicy、restart、securityOpt 等）看对话框内「配置字段说明」折叠面板或 `docs/Worker-设计方案.md` §4.2。

点「部署」后异步收敛（上限 5 分钟，页面每 1.5s 轮询）：终态 `healthy`（副本齐且健康）/ `partial`（部分在跑）/ `failed`（一个都没跑，多为镜像拉取失败或端口冲突）。约 90 秒未收敛会提示去服务列表刷新。

### 1.5 上线验证

1. 「服务」Tab 出现该服务，状态列副本比 `2/2`；
2. 「监控」列显示规则；「告警规则」Tab 出现该服务的规则（带 swarm 标签）；
3. 集群内 `curl http://<节点>:18080/` 业务可用；
4. 故意停掉业务（或等一次真实异常），「事件」Tab 出现 `port_down` 等事件，配置了场景 5 后能收到通知。

**本场景高频坑**：镜像地址写法与节点 daemon.json 不匹配（failed `no replicas running`，节点上手工 `docker pull` 复现）；中继 pull 报 502 `registry tunnel unavailable` = 隧道未建立（核对 `OPSGUARD_TUNNEL_BASE`、管理端 IdP、地址是否 manager）；接入 502 `not a swarm manager` = 地址填到了 node 节点。

---

## 场景 2：发布新版本与服务生命周期管理

**任务**：业务已上线（场景 1 完成），要发新版本、调容量、重启或下线。

**路径**：集群详情页「服务」Tab，行内操作：

| 操作 | 步骤与要点 |
| --- | --- |
| **发布新版本** | 先按场景 1.1 构建新 Tag（或场景 4 直推）→ 行「编辑」→ 修改 YAML 中的 `image` Tag → 「保存」。编辑是**整体替换**语义：预填最近一次配置快照；⚠️ 保存的 YAML 不带 `monitoring:` 块会把已生效的监控清掉（无快照时页面自动从告警规则恢复并黄字提示） |
| 滚动更新 | 由 `service.update` 参数控制（parallelism/delay/failureAction），不配则默认逐个、失败暂停 |
| **扩缩容** | 行「缩放」→ 输入副本数（0-999）→「应用」；global 模式服务无此按钮。部署 YAML 里 replicas 必须 > 0，缩到 0 只能走缩放 |
| **重启** | 行「重启」→ 确认（强制重建任务） |
| **看日志** | 行「详情」→ 聚合日志，支持「跟随最新/停止跟随」，非跟随取最近 300 行、跟随上限 2000 行，stderr 红色 |
| **下线** | 行「移除」→ 确认（不可恢复，同时注销该服务全部监控探针） |

**验证**：每次写操作都产生异步 operation（同场景 1.4 的收敛状态），失败弹窗带具体错误；「事件」Tab 可回溯。

---

## 场景 3：纳管 swarm 之外的中间件与宿主机服务

**任务**：有些服务不是 swarm 部署的（宿主机 `docker run` 的容器、直接跑在宿主机上的端口服务如 MySQL/Redis），也要纳入监控告警。

**路径**：集群详情页头部「纳管配置」→ 打开清单编辑器（表单 / YAML 双模式），逐条声明：

| 字段 | 填写 |
| --- | --- |
| 名称 | 必填且唯一，如 `r-nacos` |
| 类型 | `standalone-container`（docker run 容器）/ `host-service`（宿主机端口服务） |
| 引用 ref | standalone 填容器名；host-service 填 IP 或 `host:port` |
| 所在节点 | standalone 必填（节点 hostname）；host-service 不需要 |
| 端口 | 如 `8848,9848`，可多个 |
| 分类 | middleware（中间件）/ business（业务）/ infra（基础设施）/ service（服务）——middleware 会额外出现在「中间件」Tab |
| 监控 | 内嵌端口探测 / HTTP 检查配置 |

「保存」**整体替换**当前清单。声明后：

- 管理端每 30s 周期探测：standalone 查容器存活（非 running → `container_down`）+ 端口探测；host-service 做端口/HTTP 探测（从清单指定节点发起）；
- 只在**状态翻转**时产生事件/恢复事件，进告警中心；
- 清单条目与「告警规则」Tab 双向同步（带监控的条目自动出现同名规则）；资源/日志探针对纳管对象只存不执行；
- 纳管行在「服务」Tab 带「纳管」标签，仅有「重启」操作（仅 standalone）。

---

## 场景 4：镜像仓库日常治理

**任务**：不经过页面 zip 构建直接推镜像；清理 tag；了解保留策略与账号体系。

- **docker CLI 直推**（节点/开发机可直连管理端时）：`docker login <registry地址>`（账号在 server 配置 `registry.users`）→ `docker tag` / `docker push`。地址以「镜像仓库」页「使用指引」面板显示值为准。
- **删除 tag**：「镜像列表」展开 → 行「删除」→ 确认。只删 tag 关联，manifest 与 blob 由 GC 异步回收。
- **保留策略**：每仓库自动保留最近 N 个 tag（`registry.retention_per_repo`，常见配置 10），构建成功后立即裁剪——旧 tag 消失是预期行为。
- **账号体系**（均在 server 配置，无页面管理，改后重启）：`registry.users` = 外部推拉 basic auth；`registry.builder` = 页面构建推送用；`registry.relay` = 中继模式代持凭据（未配时回退 users 首条）。

---

# B. 监控告警与值班

## 场景 5：让告警通知到人（一次性配置）

**任务**：告警默认只在页面展示，要配好渠道和策略才能推送到飞书/短信/webhook。

**路径**：「通知中心」，三个 Tab：

1. **渠道**→「新增渠道」：`名称`、`类型`（飞书 / 短信 / 通用 Webhook）、`经互联网代理` 开关（内网无外网时开启并填 `代理地址`）、对应类型的 `Webhook URL` / `飞书 Webhook` / `签名（代理侧）`、`启用`。约束：飞书直连必须给 webhook 地址；短信必须走代理（凭据在代理侧）；webhook 必须给 URL。
2. **策略**：按级别（`error` / `warn` / `info`）选择 `渠道`（多选）与 `接收人`（多选，手机号/用户标识）。**某级别没配策略 = 该级别告警不发通知**。
3. **发送记录**：每次发送的时间/渠道/内容/结果（success/failed + 错误）——通知没收到先查这里。

**通知节奏**（了解即可，无需配置）：告警首次出现或恢复后复发立即通知；持续未恢复在累计 10 / 100 / 1000… 次时追加一次「已累计 N 次」；恢复时发恢复通知。

**验证**：临时把某服务监控阈值调到必触发（如 CPU 阈值 1%），等一个采样周期确认渠道收到，再改回。

## 场景 6：值班巡屏与告警处置

**任务**：值班人员每天的例行检查与告警处置动作。

**巡屏**：「总览」页（每次进入刷新一次，不自动轮询）——

- 4 个数字卡：在线集群 / 未处理告警 / 项目 / 巡检异常；
- 集群健康表：离线集群红标 + 最近错误，点名称下钻；
- 最近告警 8 条（级别/集群服务/标题/状态）。

**告警处置**：「告警中心」→ 顶部按集群/状态筛选（`全部 / 未处理 / 已认领 / 已恢复`）→ 行操作：

- **排查**：跳「异常排查」页带告警上下文，走场景 9 的 AI 排查；
- **认领**（仅未处理）：标记有人在跟；
- **恢复**（未处理/已认领均可）：人工关闭；同类型异常再次出现会重新激活并再通知。

状态流转 `未处理(active) → 已认领(acked) → 已恢复(recovered)`。「次数/首次/最近」列看持续性；「已排查 ×N」标记看过 AI 排查历史。要回溯原始事件：集群详情页「事件」Tab（时间/服务/级别/内容，游标分页「加载更多」）。

## 场景 7：周期智能巡检与 AI 报告投递

**任务**：配置定时巡检（cron 自动跑检查项），生成 AI 总结报告并可投递到通知渠道；异常项自动转告警。

**前置**：目标集群已接入（场景 1.3）；要 AI 报告需先完成场景 8（未配模型时巡检照常跑，只是报告退化为纯文本摘要，模型下拉为空即此情况）。

**路径**：「智能巡检」→「新建流程」：

| 字段 | 填写 |
| --- | --- |
| 名称 / 描述 | 如 `nightly-check` |
| Cron | 5 字段标准表达式，默认 `0 2 * * *`（每天 02:00） |
| 启用 | 停用的流程不参与调度，只能手动「立即执行」 |
| 报告模型 | 下拉来自「MLOps → 模型接入」模型池；保存时以此下拉为准回写 YAML |
| 流程 YAML | 见下；卡住就点「下载完整模板」（含全部 6 种检查类型注释） |

**6 种检查类型**（每个 check 的 `cluster` 一律必填）：

| type | 必填 | 关键可选 |
| --- | --- | --- |
| `resource` 服务容器资源 | `service` | `cpu_threshold` / `mem_threshold`（百分比；未找到容器也算异常） |
| `health` 服务副本健康 | `service` | `min_replicas`（默认 1；running≠desired 也异常） |
| `port` TCP 探测 | `host`、`port` | `node`（**空 = 全部 ready 节点逐个探测**）、`timeout`（默认 3s，上限 10s） |
| `http` HTTP 探测 | `url` | `expected_status[]`（空=任意 2xx）、`expected_body`（正则）、`node` |
| `process` 宿主机进程 | `filter`（名称/命令行子串） | `min_count`（默认 1）、`node` |
| `flow` 多步 HTTP 事务 | `name`、`steps[]` | `vars`（支持 `${secret:名称}` 引用密钥）、每步 `extract`（`$.json.path` 或 `re:正则`） |

flow 示例（登录链路拨测，凭据走密钥不落 YAML）：

```yaml
name: nightly-check
checks:
  - type: health
    cluster: azbx-cluster
    service: myapp
  - type: flow
    cluster: azbx-cluster
    name: 登录可用性
    vars: { user: monitor-bot, pass: "${secret:patrol-login}" }   # 密钥在 系统设置→密钥 维护
    steps:
      - { name: login, method: POST, url: http://10.0.0.11:8080/api/login,
          body: '{"username":"{{user}}","password":"{{pass}}"}',
          extract: { token: "$.data.token" } }
      - { name: verify, url: http://10.0.0.11:8080/api/me,
          headers: { Authorization: "Bearer {{token}}" } }
report:
  model: deepseek-chat        # 与表单「报告模型」同步
```

保存后点「立即执行」验证一轮（1.5s 后刷新执行记录），「执行记录」→「详情」看检查明细（每项 正常/异常）与 AI 报告。

**报告投递**：「智能巡检」→「报告投递」→ `发送时机`（`每次生成后都发送` / `仅有异常时发送` / `不发送`，默认不发送）+ `投递渠道`（多选，来自通知中心；先去场景 5 建渠道）。**全局配置，所有流程共享**，保存后下次执行生效。

**异常转告警**：检查项失败自动产生 `patrol_failed`（warn 级）告警进告警中心；与上一轮对比，仅新增/复发才通知（持续失败只累加次数）；本轮恢复自动关告警。

**高频坑**：port/http 探测从 Worker 容器网络发起，探宿主机服务**必须写节点 IP，不能写 127.0.0.1**；`${secret:}` 引用的密钥不存在直接判失败（先去「系统设置 → 密钥」建）；run 状态恒为 `success`，检查失败体现在「异常」列，不算 run 失败；集群不可用会产出一条「集群不可用」异常而不是报错中断。

---

# C. AI 能力

## 场景 8：接入 AI 模型并启用排查网关（一次性初始化）

**任务**：给平台接上大模型，使 AI 排查、巡检 AI 报告、MLOps 计量可用。

**前置**：拿到模型服务的 Base URL 与 API Key（OpenAI 兼容或 Anthropic 兼容端点均可，GLM/DeepSeek/Ollama/vLLM 都行）；admin 账号。

1. **接入模型**：「MLOps → 模型接入」（admin）→「添加 Provider」→ 填 `名称`、`类型`（`OpenAI 兼容` / `Anthropic 兼容`）、`Base URL`、`API Key` →「添加模型」填 模型名（如 `deepseek-chat`）/ 显示名（可选）/ max_tokens / temperature → 点「测试连接」验证连通（返回延迟/错误）→「保存模型配置」。保存即**热重载**，无需重启；API Key 留空 = 保持旧值。
2. **启用网关**：「系统设置 → AI 排查网关」（admin）→ 打开「启用网关」→ 选「默认模型」（空 = 模型池首个可用）→「保存并热重载」。此页还可配：内置工具（命令执行/HTTP 请求/文件读取，默认关）、Agent 参数（工具轮次上限默认 200、上下文预算默认 800000 token）、自定义 MCP Servers；集群 MCP 是**自动**连接的（网关加载时连每个已注册集群的 Worker `/mcp`，页面只读列出）。
3. **验证**：`MLOps → 模型`Tab 网关状态栏显示「运行中」、模型「路由中」；对新模型做「健康测试」（发一次真实最小请求，消耗极少量 token，计入 health 场景）。

顺序必须**先模型池后启用网关**——网关启用时至少要有一个 provider。巡检的「报告模型」下拉、排查页的可用性都从此而来。

## 场景 9：AI 异常排查

**任务**：出告警了（或凭直觉有问题），用 AI 对集群采证、定位根因、给出处置建议。

**前置**：场景 8 完成（否则「异常排查」页顶部黄色横幅「网关未启用」，输入框禁用）；要用 MCP 直操作集群，目标集群需在线且 Worker token 配置正确（MCP 连不上会静默降级为纯证据注入模式，不报错）。

**两个入口**：

- **告警驱动**（推荐）：告警中心行「排查」自动跳转并预选告警；或在「异常排查」页「关联告警」下拉选 `[集群/服务] 标题 (状态)`；
- **自由提问**：不选告警，直接在输入框提问（Enter 发送，Shift+Enter 换行），如「azbx 集群为什么昨晚 2 点有重启」。

**操作**：保持「MCP 采证」开启 → 点「开始排查」（告警会话首轮可不输文字——服务端会自动注入证据：近 20 条事件 + 10 条审计 + 50 行日志）→ 观察流式回答：

- 回答中出现的工具 chip（如 `get_service_logs`、`list_nodes`）是 AI 在真实调用集群 Worker，点开看采证结果，红色 = 该次调用失败；
- 同一会话可继续追问（每轮自动重新前置告警证据）；「新会话」或切换告警重开。

**会话留痕**：每轮自动落库；告警行显示「已排查 ×N」；选告警 →「历史排查」→「查看」**只读回放**（不能续聊）。

**安全须知**：AI 可执行危险操作（删除服务、缩容到 0、容器内/宿主机执行命令），服务端护栏是要求模型显式传 `confirm=true`，exec 类动作全部写入 Worker 审计日志；宿主机命令还受 Worker `commandPolicy` 黑白名单与 `allowHostExec` 开关约束（生产默认关闭）。

**常见失败**：400 `model X not found` = 模型名拼错或被禁用；400 `model X is disabled by mlops operations` = 被 MLOps 停用；AI 不调任何工具 = MCP 未连上（查集群在线状态与 token）或「MCP 采证」被关；回答含 `[Error: …]` = 上游模型调用失败。

## 场景 10：MLOps 运营（提示词定制 / 模型治理 / 费用预算）

**任务**：admin 对 AI 能力做持续运营：改提示词、模型启停与场景绑定、算费用、控预算。前置：server 配置 `mlops.enabled: true`（否则只剩「模型接入」Tab，无计量）。

**① 提示词（Prompt Hub）**：「MLOps → 提示词」。内置 5 个场景模板：

| 场景 key | 用途 |
| --- | --- |
| `investigate_system` / `investigate_user` | AI 排查的系统提示词与证据消息模板 |
| `compress_system` | 长会话摘要压缩 |
| `patrol_system` | 巡检 AI 报告 |
| `custom` | 自定义（仅管理，不接入线上） |

选中场景 → 编辑消息行（role + 模板正文，Go 模板语法如 `{{.Alert.Title}}`）→ 填版本备注 → 「保存后立即激活」→「保存新版本 (vN+1)」。可预览渲染、回滚任意版本、「还原默认」（v1 = 代码内置默认）。改动即时生效无需重启；线上渲染失败自动回退内置默认不阻断业务。

**② 模型治理**：「MLOps → 模型」。模型池表格：行「启用/禁用」（热重载立即生效，禁用不断进行中请求；最后一个启用模型不允许禁用）、「健康测试」；下方「场景绑定」表给 4 个场景（`chat` / `investigate` / `native_chat` / `patrol_report`）指定模型——绑定只影响之后的新请求，绑定模型不可路由时回退网关默认。

**③ 费用与预算**：「MLOps → 用量费用」。口径：按**底层 provider 调用**计量（Agent 每轮工具调用/压缩各计一条），金额为估算、入账时锁定当时单价（改价不回溯）。

- **模型单价**：`provider` + `model` + `输入/输出 元/百万token`（最多 6 位小数，免费填 0）。没配单价的调用显示「未计价」（≠免费）。
- **月度预算**：「设置」→ `月份`（yyyy-mm）、`预算上限(元)`、`预警档位`（1-99%）。达档位与 100% **各通知一次**，发往全部启用渠道，超限**只告警不拦截**请求；重新保存预算会重置通知档位。
- 报表：今日/本月摘要、按模型、按场景、日趋势（最多 92 天）、调用明细（默认近 90 天，可按场景/状态/模型筛选，点 `operation_id` 下钻一次业务请求的全部底层调用）。

---

# D. 平台安全

## 场景 11：账号与登录管理

**任务**：初始账号加固、给同事开账号、对接企业 SSO。

- **首登改密（所有用户自助）**：「系统设置 → 用户」Tab 底部「修改密码」→ 旧密码/新密码/确认。默认账号 `admin / opsguard-admin` 首次登录后必须改。SSO 账号无本地密码，不能自助改（提示找管理员重置）。
- **新建用户 / 重置密码（admin，「用户」Tab 仅 admin 可见）**：「新增用户」→ `用户名` / `密码` / `角色`（`管理员` / `只读`，默认只读）；「重置密码」免旧密码直接设新值（对 SSO 用户重置 = 给他设本地口令）。注意：**没有删除用户和禁用用户的入口**（存储层支持 enabled，但无管理界面），开账号前想清楚。
- **SSO**：「系统设置 → SSO」Tab 是**只读展示**，实际配置在 server 的 `auth.sso.oidc`（issuer / client_id / client_secret / redirect_url / frontend_url），改后重启。启用后登录页出现「企业 SSO 登录」按钮：跳企业 IdP → 回调 → token 放 URL hash 自动登入；SSO 新用户默认 viewer 角色，**第一个** SSO 用户自动提升 admin。

## 场景 12：向集群内系统签发 OIDC 身份（IdP）

**任务**：让集群内的系统（如 r-nacos）或企业内应用用 OpsGaurd 的账号体系做登录（OpsGaurd 作为 OIDC IdP），隔离集群经 Worker 反向隧道接入、免开防火墙。

**前置**：server 配置 `idp.enabled: true`（同时会启用认证与隧道）；admin 账号。

1. **注册 Client**：「系统设置 → 身份提供者」→「注册 Client」：`名称`；`类型`——`公共（PKCE，无 secret，适合 MCP 工具/SPA）` 或 `机密（client_secret，适合后端服务）`，创建后类型锁定；`回调地址`（至少一个）；`允许 Scope`（默认 openid profile email）；`Token TTL`（可空）。机密类型创建成功弹 **Client Secret 一次性弹窗**（仅此一次可见）；「轮换密钥」旧密钥立即失效。
2. **对接方接入**：授权码 + PKCE；discovery 地址 `/.well-known/openid-configuration`。
   - 网络可达管理端的系统：issuer 直接填管理端地址；
   - **隔离集群内系统**：issuer 填 `http://<OPSGUARD_TUNNEL_BASE>/idp-proxy`（Worker 本地 `/idp-proxy/` 经反向隧道转发到管理端，discovery 文档自动改写端点）。验证：`curl http://<worker:端口>/idp-proxy/.well-known/openid-configuration` 返回改写后的发现文档。
3. 单点体验：管理端登录后浏览器已持有 IdP 会话 cookie，对接系统跳授权可免密。

详细对接参数与实测记录见 `docs/IdP-接入指南.md`。

---

## 附录 A：按症状索引的 FAQ

| 症状 | 场景 | 原因 → 处理 |
| --- | --- | --- |
| 「镜像仓库未启用」黄条 | 1 | `registry.enabled` 未开 → 改 true 重启 |
| 「构建不可用」黄标 | 1 | 管理端缺 docker CLI → 安装，或走场景 4 的 CLI 直推 |
| 构建失败 `zip 内未找到 Dockerfile` / PUSHING 401 | 1/4 | 包内容不对 / `registry.builder` 账号缺配 → 对应修复 |
| 接入集群 502 `not a swarm manager` / 连接失败 | 1 | 地址填到 node 节点 / 端口填成 HTTP 端口、防火墙未通 → 填 manager 的 gRPC 地址并放通 |
| 部署 failed `no replicas running` | 1/2 | 多为镜像拉不动或端口冲突 → 节点手工 `docker pull` 复现；核对 daemon.json 与 image 写法 |
| 节点 pull 报 502 `registry tunnel unavailable` | 1 | 隧道未建立：`OPSGUARD_TUNNEL_BASE` 未配 / 管理端未启 IdP / 地址非 manager Worker → 逐项核对 |
| 编辑服务后监控消失 | 2 | 整体替换语义，YAML 没带 `monitoring:` → 补上，或经「告警规则」Tab 重新「下发」 |
| 改了告警规则没生效 | 1/3 | 只「保存」没「下发」 → 规则行点「下发」 |
| 告警产生了但没人收到 | 5/6 | 该级别未配策略，或渠道发送失败 → 「策略」补配；「发送记录」查失败原因 |
| 巡检探测宿主机服务总失败 | 7 | url/host 写了 127.0.0.1 → 改节点 IP（探测从 Worker 容器网络发起） |
| 巡检 `${secret:}` 报不存在 | 7 | 密钥未建 → 「系统设置 → 密钥」先建（名称仅字母数字`._-`，值永不回显） |
| 巡检报告是纯文本没有 AI 总结 | 7/8 | 网关未启用或无可用模型（报告模型下拉为空）→ 完成场景 8 |
| 排查页黄条「网关未启用」/ 503 | 8/9 | 网关未启用或模型池为空 → 先模型接入再启用网关 |
| AI 排查不调用任何集群工具 | 9 | 集群离线 / token 不符致 MCP 静默降级，或「MCP 采证」被关 |
| 400 `model X is disabled by mlops operations` | 9/10 | 模型被 MLOps 停用 → 「模型」Tab 重新启用或换模型 |
| SSO 用户改不了密码 | 11 | SSO 账号无本地口令 → 管理员「重置密码」 |
| IdP Client 接入后无法认证 | 12 | secret 轮换后旧密钥失效 / client 被删 → 重新下发凭据 |

## 附录 B：权限速查

| 操作 | viewer（只读） | admin |
| --- | --- | --- |
| 看所有页面/报表/日志 | ✅ | ✅ |
| 项目/集群/纳管清单/部署/扩缩容/重启/删除 | ✅ 可写 | ✅ |
| 告警认领/恢复、排查会话、巡检增删改/执行、告警规则、通知渠道与策略、密钥、镜像构建与删除 | ✅ 可写 | ✅ |
| 用户管理（列表/新建/重置密码，Tab 仅 admin 可见） | ❌ | ✅ |
| IdP Clients（注册/轮换/删除） | ❌ | ✅ |
| AI 网关配置写 / 模型池保存（含测试连接） | ❌ | ✅ |
| MLOps 写：提示词版本、单价、模型启停/健康测试、场景绑定、预算、审计查询 | ❌ | ✅ |

后端守卫是权威（403 `forbidden: insufficient role`），前端仅做按钮收敛。

## 附录 C：相关文档

| 文档 | 内容 |
| --- | --- |
| `docs/OpsGaurd-系统技术总览.md` | 架构、通信、存储、安全全景 |
| `docs/Worker-设计方案.md` §4.2 | service / monitoring 配置完整契约 |
| `docs/镜像隧道中继方案.md` | 隔离集群镜像中继设计与节点接入 SOP |
| `docs/集群纳管清单方案.md` | 纳管清单与周期探测设计 |
| `docs/IdP-接入指南.md` | OpsGaurd 作为 OIDC IdP 的对接参数与实测记录 |
| `docs/MLOps-方案.md` | MLOps 分层设计 |
| `deploy/offline/README.md` | 集群离线安装 docker / 部署 Worker 完整 runbook |
