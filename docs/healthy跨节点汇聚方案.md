# healthy 跨节点汇聚方案

> 背景：`Orchestrator.Inspect` 只用本地 Docker Engine 查容器健康，非本机 task 永远查不到 → 跨节点服务 `healthy` 计数失真、部署操作误报 Partial。本方案改动全部收敛在 Worker 模块内，proto / server / 前端零改动。

## 1. 问题

`Orchestrator.Inspect`（`Worker/internal/orchestrator/lifecycle.go:387`）对每个 running task 用**本地** Docker Engine 调 `ContainerInspect` 查健康。非本机 task 的容器在远端节点上，本地必然 404、错误被吞（`err == nil` 判断失败），该 task 永远不计入 `Healthy`。后果有两处：

1. **服务详情页**（gRPC `GetService` → MCP / 前端）：跨节点服务 `healthy` 永远小于 `running`。
2. **同一 bug 存在于 `computeReady`**（`lifecycle.go:422`），它驱动 `pollReadiness` 的收敛判定（`rs.healthy == rs.running`，`lifecycle.go:455`）——多节点 + 带 healthcheck 的服务，部署/更新操作永远等不到全 healthy，超时后误报 `OpStatusPartial`（"convergence timeout: N/M running"）。这是比 UI 显示更疼的实际 bug。

现状调用路径（全部落在本地）：

```
Inspect / computeReady
  → o.cli.ContainerInspect(ctx, cid)        # 本地 Docker Engine GET /containers/{id}/json
  → 非本机容器 404 → 错误被吞 → 永不计入 healthy
```

## 2. 现成基础设施（无需新建，直接复用）

| 能力 | 位置 | 说明 |
|---|---|---|
| task→node 关联 | `docker.Task.NodeID`（`Worker/internal/docker/types.go:229`） | swarm task 自带节点归属 |
| 本机节点 ID | `Orchestrator.SelfNodeID`（`orchestrator/proxy.go:447`） | 经 engine `/info` 的 `Swarm.NodeID`，不依赖 hostname |
| 节点地址表 | `Orchestrator.nodeAddrs`（`proxy.go:421`） | nodeID → addr，仅 ready 节点 |
| 节点 worker 客户端 | `NodeClientByAddr`（`proxy.go:442`） | HTTP :8080，自动带 bearer token |
| 节点侧 container inspect | `docker.Client.ContainerInspect`（`docker/stats.go:44`） | 精简类型 `{ID, Name, State{Status, Health{Status}}}`，够用 |
| 本机/远程分流先例 | `mcp/metrics.go:292`（exec 工具） | 已验证同款模式：`task.NodeID == selfID` 走本地，否则走节点 worker |

唯一缺口：**nodeagent 的 `/api/v1/local/*` 没有 container inspect 类端点**。

## 3. 方案：4 个改动点，全部在 Worker 模块

### 3.1 nodeagent 新增批量健康端点（节点 worker 侧）

新文件 `Worker/internal/nodeagent/containers_health.go`：

- 路由：`POST /api/v1/local/containers/health`（注册进 `api.go` 的 `Routes()` map）
- 请求：`{"ids": ["<containerID>", ...]}`
- 响应：`{"node": "<hostname>", "health": {"<cid>": "healthy" | "unhealthy" | "starting"}}`
  - 值直接取 `ci.State.Health.Status`；`Health == nil` 或容器不存在/查询失败 → 不出现在 map 里（调用方把缺失一律按不健康计）
- 实现：对 ids 并发 fan-out `a.cli.ContainerInspect`，预分配索引切片免锁写（抄 `LocalStats` 第一遍快照的模式，`nodeagent/stats.go:71-87`）；本地 socket 调用很便宜，不做缓存、不占 `statsCache`

选批量 POST 而非逐容器 GET 的理由：manager 对每个远端节点一次往返（一个服务往往在同一节点有多个 task），且与现有 `containers/restart`、`exec` 的 POST+JSON 风格一致。

### 3.2 `NodeClient` 新增 `ContainerHealth`（`orchestrator/proxy.go`）

```go
// ContainerHealthResp mirrors nodeagent.containersHealthResp (POST
// /api/v1/local/containers/health).
type ContainerHealthResp struct {
	Node   string            `json:"node"`
	Health map[string]string `json:"health"` // containerID -> healthy|unhealthy|starting
}

// ContainerHealth batch-queries container health on the node. Missing ids
// are simply absent from the map (treated as not-healthy by the caller).
func (n *NodeClient) ContainerHealth(ctx context.Context, ids []string) (map[string]string, error)
```

复用现成的 `postJSON`；类型定义与现有 `PortCheckResult` 等 mirror 类型同一风格。

### 3.3 `Orchestrator` 新增共享汇聚方法 `countHealthy`（`lifecycle.go`）

```go
// countHealthy 统计 tasks 中健康容器的数量：本机 task 走本地 engine，
// 远端 task 按节点分组、经该节点 worker 批量查询（fan-out + 每节点独立
// 超时，慢/不可达节点不阻塞其他节点，也不阻塞整体调用）。
// 入参 tasks 须已过滤为 DesiredState=running && Status.State=running && ContainerID != ""。
func (o *Orchestrator) countHealthy(ctx context.Context, tasks []docker.Task) int
```

逻辑：

1. `selfID, err := o.SelfNodeID(ctx)`；失败则全部按本机处理（保持现状语义，单节点 swarm 下这本来就是全部路径）。
2. 按 `t.NodeID` 分组：`NodeID == ""` 或 `== selfID` → 本机组（循环 `o.cli.ContainerInspect`，即原路径）；其余 → 远端组。
3. 存在远端组时调一次 `o.nodeAddrs(ctx)`（**只调一次**，不是每节点一次）。
4. 每个远端节点一个 goroutine + `context.WithTimeout(ctx, healthyNodeTimeout)`（常量 `5s`，对齐 `StreamNodeStats` 的 per-node timeout 模式；inspect 是远端节点的本地 socket 调用，5s 绰绰有余），调 `NodeClientByAddr(addr).ContainerHealth(cids)`。
5. `sync.WaitGroup` + mutex 收集各节点 map，最后统一数 `== "healthy"` 的容器。
6. **降级语义**：节点无地址（down / not ready）、超时、HTTP 失败（含旧版 worker 没有该端点返回 404）→ 该节点 task 全部不计健康，与今天行为一致，只记一条 warn 日志；`countHealthy` 本身永不返回 error。

### 3.4 两个调用点接入

- **`Inspect`**（`lifecycle.go:379-393`）：主循环只数 `Desired` / `Running` 和"无 healthcheck 直判健康"的分支（`serviceHasHealth` 为 false 时 `d.Healthy++` 保持不变）；带 healthcheck 且 `cid != ""` 的 task 收集进切片，循环后 `d.Healthy += o.countHealthy(ctx, candidates)`。
- **`computeReady`**（`lifecycle.go:420-425`）：同样改为收集 + `rs.healthy += o.countHealthy(...)`。这会顺带修掉"多节点服务部署必误报 Partial"的 bug——同一文件、同一共享方法，属于"改动集中在 Inspect"的自然延伸；若只想先修 UI 侧可去掉这一处，但建议一起修。

改造后的调用路径：

```
Inspect / computeReady
  ├─ 本机 task → o.cli.ContainerInspect                    # 原路径不变
  └─ 远端 task → 按 NodeID 分组（每节点一次批量调用，并行）
        → NodeClientByAddr(addr).ContainerHealth(cids)     # HTTP :8080 + bearer
        → POST /api/v1/local/containers/health             # 节点 worker
        → a.cli.ContainerInspect × N（并发，本地 socket）
```

## 4. 兼容性与部署

- **proto / server / 前端零改动**：gRPC `GetService` 返回的 `Healthy` 字段语义不变，数值从此才是对的。
- **非破坏性滚动升级**：旧版节点 worker 没有新端点 → manager 收到 404 → 该节点按不健康计（= 现状）；manager / worker 滚动升级到新版后自动生效。worker-only 变更，部署时只升 worker 镜像。
- **性能**：远端每节点一次 HTTP 往返、5s 超时上限、节点间并行；`computeReady` 在 poll 循环里调用，最坏让一轮 poll 多 ~5s，整体仍受 `readyTimeout` 管控。单节点 swarm（本机全覆盖）时零额外开销。

## 5. 测试（沿用包内既有 fake / httptest 风格）

1. `nodeagent`：fake `docker.Client` + httptest 断言 health handler——healthy / unhealthy / `Health == nil` / 容器缺失 / 空 ids 各场景。
2. `orchestrator/proxy_test.go`：httptest 模拟节点 worker，测 `NodeClient.ContainerHealth` 编解码与错误路径。
3. `orchestrator` 汇聚测试：`fakeDocker`（已存在于 `nodes_test.go`，需补 `ContainerInspect` 桩）+ httptest 节点服务，覆盖：本机+远端混合计数、节点不可达计 0、单节点 swarm 全本机路径。

## 6. 变更文件清单

| 文件 | 变更 |
|---|---|
| `Worker/internal/nodeagent/containers_health.go` | 新增：批量健康 handler |
| `Worker/internal/nodeagent/api.go` | `Routes()` 注册新端点 |
| `Worker/internal/nodeagent/containers_health_test.go` | 新增：handler 测试 |
| `Worker/internal/orchestrator/proxy.go` | 新增 `ContainerHealthResp` + `NodeClient.ContainerHealth` |
| `Worker/internal/orchestrator/proxy_test.go` | 新增 client 测试 |
| `Worker/internal/orchestrator/lifecycle.go` | 新增 `countHealthy`；`Inspect` / `computeReady` 接入 |
| `Worker/internal/orchestrator/lifecycle_test.go` | 新增汇聚测试、补 `fakeDocker` 桩 |

验证：`cd Worker && go build ./... && go vet ./... && go test ./internal/nodeagent/ ./internal/orchestrator/`。

## 7. 范围外（后续可做，本次不动）

- 前端 / MCP 暴露 per-task 健康明细（现在只有计数）。
- 扩展 `docker.ContainerInspect` 类型（Mounts / NetworkSettings / Labels）做容器详情抽屉。
