<template>
  <el-card shadow="never">
    <template #header>
      <div class="card-header">
        <span>工作负载</span>
        <div class="header-right">
          <el-select
            v-model="cluster"
            placeholder="选择集群"
            style="width: 220px"
            filterable
            :loading="clustersLoading"
            @change="fetchWorkloads"
          >
            <el-option
              v-for="c in clusters"
              :key="c.name"
              :label="`${c.name}${c.status === 'online' ? '' : '（离线）'}`"
              :value="c.name"
              :disabled="c.status !== 'online'"
            />
          </el-select>
          <el-button type="primary" :icon="Plus" :disabled="!cluster" @click="openDeploy">部署服务</el-button>
        </div>
      </div>
    </template>

    <!-- 服务列表 -->
    <el-table v-loading="loading" :data="workloads" empty-text="该集群暂无服务，点击「部署服务」创建">
      <el-table-column label="名称" prop="name" min-width="140">
        <template #default="{ row }">
          <el-link type="primary" @click="openDetail(row)">{{ row.name }}</el-link>
        </template>
      </el-table-column>
      <el-table-column label="镜像" prop="image" min-width="200" show-overflow-tooltip />
      <el-table-column label="模式" width="100">
        <template #default="{ row }">
          <el-tag size="small" :type="row.mode === 'global' ? 'warning' : 'info'">
            {{ row.mode ?? '—' }}
          </el-tag>
        </template>
      </el-table-column>
      <el-table-column label="副本" prop="replica" width="90" />
      <el-table-column label="端口" min-width="150">
        <template #default="{ row }">
          <span v-for="p in row.ports ?? []" :key="`${p.publishedPort}:${p.targetPort}`" class="port-chip">
            {{ p.publishedPort }}→{{ p.targetPort }}/{{ p.protocol ?? 'tcp' }}
          </span>
          <span v-if="!row.ports?.length">—</span>
        </template>
      </el-table-column>
      <el-table-column label="操作" width="220" fixed="right">
        <template #default="{ row }">
          <el-button link type="primary" @click="openDetail(row)">详情</el-button>
          <el-button link type="primary" @click="openScale(row)">缩放</el-button>
          <el-button link type="warning" @click="restart(row)">重启</el-button>
          <el-button link type="danger" @click="remove(row)">移除</el-button>
        </template>
      </el-table-column>
    </el-table>

    <!-- 部署对话框 -->
    <el-dialog v-model="deployVisible" title="部署服务" width="640px">
      <el-form label-width="80px">
        <el-form-item label="集群">
          <el-tag>{{ cluster }}</el-tag>
        </el-form-item>
        <el-form-item label="配置" required>
          <el-input
            v-model="deployConfig"
            type="textarea"
            :rows="14"
            placeholder="Worker 服务配置（YAML/JSON），示例：
name: web
image: nginx:alpine
replicas: 2
ports:
  - target: 80
    published: 8080"
          />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="deployVisible = false">取消</el-button>
        <el-button type="primary" :loading="deploying" @click="deploy">部署</el-button>
      </template>
    </el-dialog>

    <!-- 缩放对话框 -->
    <el-dialog v-model="scaleVisible" title="缩放副本" width="360px">
      <el-form label-width="80px">
        <el-form-item label="服务">
          <el-tag>{{ current?.name }}</el-tag>
        </el-form-item>
        <el-form-item label="副本数" required>
          <el-input-number v-model="scaleReplicas" :min="0" :max="999" />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="scaleVisible = false">取消</el-button>
        <el-button type="primary" :loading="scaling" @click="scale">应用</el-button>
      </template>
    </el-dialog>

    <!-- 详情抽屉（任务 + 日志） -->
    <el-drawer v-model="detailVisible" :title="`${current?.name ?? ''} 详情`" size="55%">
      <template v-if="detail">
        <el-descriptions :column="2" border size="small">
          <el-descriptions-item label="镜像">{{ detail.image || '—' }}</el-descriptions-item>
          <el-descriptions-item label="模式">{{ detail.mode || '—' }}</el-descriptions-item>
          <el-descriptions-item label="副本">{{ detail.replica || '—' }}</el-descriptions-item>
          <el-descriptions-item label="健康">{{ detail.healthy }} 个</el-descriptions-item>
        </el-descriptions>

        <h4>任务</h4>
        <el-table :data="detail.tasks" size="small">
          <el-table-column prop="id" label="任务 ID" min-width="150" show-overflow-tooltip />
          <el-table-column prop="slot" label="Slot" width="60" />
          <el-table-column prop="state" label="状态" width="100">
            <template #default="{ row }">
              <el-tag size="small" :type="row.state === 'running' ? 'success' : 'info'">{{ row.state }}</el-tag>
            </template>
          </el-table-column>
          <el-table-column prop="containerId" label="容器" min-width="150" show-overflow-tooltip>
            <template #default="{ row }">{{ row.containerId || '—' }}</template>
          </el-table-column>
          <el-table-column prop="err" label="错误" min-width="120" show-overflow-tooltip>
            <template #default="{ row }">{{ row.err || '—' }}</template>
          </el-table-column>
        </el-table>

        <div class="log-header">
          <h4>日志</h4>
          <el-button size="small" @click="toggleLogFollow">
            {{ logFollow ? '停止跟随' : '跟随最新' }}
          </el-button>
          <el-button size="small" @click="loadLogs(false)">刷新</el-button>
        </div>
        <div ref="logBoxRef" class="log-box">
          <div v-for="(l, i) in logLines" :key="i" :class="['log-line', l.stream]">
            <span class="log-ts">{{ l.ts }}</span> {{ l.line }}
          </div>
          <el-empty v-if="!logLines.length" description="暂无日志" :image-size="40" />
        </div>
      </template>
    </el-drawer>
  </el-card>
</template>

<script setup lang="ts">
import { nextTick, onBeforeUnmount, onMounted, ref } from 'vue'
import { ElMessage, ElMessageBox, ElNotification } from 'element-plus'
import { Plus } from '@element-plus/icons-vue'
import { clusterApi, workloadApi } from '@/api'
import type { ClusterSummary, LogLine, Operation, Workload, WorkloadDetail } from '@/types'

const clusters = ref<ClusterSummary[]>([])
const clustersLoading = ref(false)
const cluster = ref('')
const workloads = ref<Workload[]>([])
const loading = ref(false)

const deployVisible = ref(false)
const deployConfig = ref('')
const deploying = ref(false)

const scaleVisible = ref(false)
const scaleReplicas = ref(1)
const scaling = ref(false)

const detailVisible = ref(false)
const detail = ref<WorkloadDetail | null>(null)
const current = ref<Workload | null>(null)

const logLines = ref<LogLine[]>([])
const logFollow = ref(false)
const logBoxRef = ref<HTMLElement>()

let logAbort: AbortController | null = null

async function fetchClusters() {
  clustersLoading.value = true
  try {
    const resp = await clusterApi.list()
    clusters.value = resp.items ?? []
    if (!cluster.value && clusters.value.length) {
      cluster.value = clusters.value[0].name
      await fetchWorkloads()
    }
  } finally {
    clustersLoading.value = false
  }
}

async function fetchWorkloads() {
  if (!cluster.value) return
  loading.value = true
  try {
    const resp = await workloadApi.list(cluster.value)
    workloads.value = resp.items ?? []
  } finally {
    loading.value = false
  }
}

// --- 部署 ---
function openDeploy() {
  deployConfig.value = ''
  deployVisible.value = true
}

async function deploy() {
  if (!deployConfig.value.trim()) {
    ElMessage.warning('请填写服务配置')
    return
  }
  deploying.value = true
  try {
    const op = await workloadApi.deploy(cluster.value, { config: deployConfig.value })
    deployVisible.value = false
    ElNotification.success({ title: '部署已提交', message: `操作 ${op.id}` })
    pollOperation(op)
  } finally {
    deploying.value = false
  }
}

// --- 缩放 ---
function openScale(row: Workload) {
  current.value = row
  scaleReplicas.value = row.desired ?? 1
  scaleVisible.value = true
}

async function scale() {
  if (!current.value) return
  scaling.value = true
  try {
    const op = await workloadApi.scale(cluster.value, current.value.name, scaleReplicas.value)
    scaleVisible.value = false
    ElNotification.success({ title: '缩放已提交', message: `操作 ${op.id}` })
    pollOperation(op)
  } finally {
    scaling.value = false
  }
}

// --- 重启 / 移除 ---
async function restart(row: Workload) {
  await ElMessageBox.confirm(`确定重启服务「${row.name}」？`, '重启服务', { type: 'warning' })
  const op = await workloadApi.restart(cluster.value, row.name)
  ElNotification.success({ title: '重启已提交', message: `操作 ${op.id}` })
  pollOperation(op)
}

async function remove(row: Workload) {
  await ElMessageBox.confirm(`确定移除服务「${row.name}」？此操作不可恢复。`, '移除服务', { type: 'error' })
  const op = await workloadApi.remove(cluster.value, row.name)
  ElNotification.success({ title: '移除已提交', message: `操作 ${op.id}` })
  await fetchWorkloads()
}

// --- 操作轮询（直到终态） ---
const TERMINAL = new Set(['healthy', 'done', 'failed', 'partial', 'canceled'])

async function pollOperation(op: Operation) {
  for (let i = 0; i < 60; i++) {
    await new Promise((r) => setTimeout(r, 1500))
    try {
      const cur = await workloadApi.operation(cluster.value, op.id)
      if (TERMINAL.has(cur.status)) {
        if (cur.status === 'healthy' || cur.status === 'done') {
          ElNotification.success({ title: `操作 ${op.type} 完成`, message: cur.error || '成功' })
        } else {
          ElNotification.error({ title: `操作 ${op.type} ${cur.status}`, message: cur.error || '未完全成功' })
        }
        await fetchWorkloads()
        return
      }
    } catch {
      return // 轮询失败即静默退出
    }
  }
}

// --- 详情 + 日志 ---
async function openDetail(row: Workload) {
  current.value = row
  detailVisible.value = true
  logLines.value = []
  try {
    detail.value = await workloadApi.get(cluster.value, row.name)
  } catch {
    detail.value = null
  }
  await loadLogs(false)
}

async function loadLogs(follow: boolean) {
  if (!current.value) return
  logAbort?.abort()
  logAbort = new AbortController()
  if (!follow) logLines.value = []

  try {
    const resp = await fetch(workloadApi.logsUrl(cluster.value, current.value.name, follow), {
      signal: logAbort.signal,
      headers: authHeaders(),
    })
    if (!resp.ok || !resp.body) {
      ElMessage.error(`日志拉取失败：${resp.status}`)
      return
    }
    const reader = resp.body.getReader()
    const decoder = new TextDecoder()
    let buf = ''
    // eslint-disable-next-line no-constant-condition
    while (true) {
      const { done, value } = await reader.read()
      if (done) break
      buf += decoder.decode(value, { stream: true })
      let idx: number
      while ((idx = buf.indexOf('\n')) >= 0) {
        const line = buf.slice(0, idx).trim()
        buf = buf.slice(idx + 1)
        if (line.startsWith('data:')) {
          const payload = line.slice(5).trim()
          if (!payload || payload === '[DONE]') continue
          try {
            logLines.value.push(JSON.parse(payload) as LogLine)
          } catch {
            /* 忽略无法解析的行 */
          }
        }
      }
      if (logLines.value.length > 2000) {
        logLines.value.splice(0, logLines.value.length - 2000)
      }
      scrollLogToBottom()
    }
  } catch (e) {
    if ((e as Error).name !== 'AbortError') {
      ElMessage.error(`日志流中断：${(e as Error).message}`)
    }
  }
}

async function toggleLogFollow() {
  logFollow.value = !logFollow.value
  if (logFollow.value) {
    await loadLogs(true)
  } else {
    logAbort?.abort()
  }
}

function scrollLogToBottom() {
  void nextTick(() => {
    const el = logBoxRef.value
    if (el) el.scrollTop = el.scrollHeight
  })
}

function authHeaders(): Record<string, string> {
  const token = localStorage.getItem('opsguard_token')
  return token ? { Authorization: `Bearer ${token}` } : {}
}

onMounted(fetchClusters)
onBeforeUnmount(() => logAbort?.abort())
</script>

<style scoped>
.card-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
}
.header-right {
  display: flex;
  gap: 8px;
}
.port-chip {
  display: inline-block;
  margin-right: 6px;
  padding: 1px 6px;
  background: var(--el-fill-color-light);
  border-radius: 4px;
  font-size: 12px;
}
.log-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-top: 8px;
}
.log-box {
  height: 320px;
  overflow: auto;
  background: #0d1117;
  color: #e6edf3;
  border-radius: 6px;
  padding: 10px;
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  font-size: 12px;
  line-height: 1.6;
}
.log-line.stderr {
  color: #ff7b72;
}
.log-ts {
  color: #8b949e;
  margin-right: 8px;
}
</style>
