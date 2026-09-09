<template>
  <div class="page">
    <div class="page-head">
      <div>
        <h2 class="page-title">集群</h2>
        <p class="page-sub">纳管的 Docker Swarm 集群 · 每个集群经其 Manager Worker 统一编排、监控与对外提供数据</p>
      </div>
      <el-button type="primary" :icon="Plus" :loading="adding" @click="openDialog">接入集群</el-button>
    </div>

    <div class="toolbar">
      <el-select
        v-model="filterProjectId"
        class="project-filter"
        clearable
        filterable
        placeholder="按项目筛选（输入名称模糊搜索）"
        :filter-method="onProjectFilter"
        @visible-change="(v: boolean) => { if (!v) projectQuery = '' }"
        @change="syncFilterQuery"
      >
        <el-option v-for="p in projectOptions" :key="p.id" :label="p.name" :value="p.id" />
      </el-select>
      <span class="filter-summary">
        共 {{ filteredClusters.length }} 个集群<template v-if="filterProjectId"> / 全部 {{ clusters.length }}</template>
      </span>
    </div>

    <el-table ref="tableRef" v-loading="loading" :data="filteredClusters" :empty-text="emptyText">
      <el-table-column label="名称" :width="colW.name">
        <template #default="{ row }">
          <el-link type="primary" @click="$router.push(`/clusters/${row.name}`)">
            <span class="cluster-name">{{ row.name }}</span>
          </el-link>
          <span v-if="row.desc" class="cluster-desc">{{ row.desc }}</span>
        </template>
      </el-table-column>
      <el-table-column label="项目" :width="colW.project">
        <template #default="{ row }">
          <el-tag v-if="projectName(row)" size="small" effect="plain">{{ projectName(row) }}</el-tag>
          <span v-else class="muted">—</span>
        </template>
      </el-table-column>
      <el-table-column label="状态" :width="colW.status">
        <template #default="{ row }">
          <el-tooltip :disabled="!row.err" :content="row.err" placement="top">
            <el-tag :type="statusTag(row.status)" size="small" effect="dark">{{ statusText(row.status) }}</el-tag>
          </el-tooltip>
        </template>
      </el-table-column>
      <el-table-column label="Manager 端点" :width="colW.endpoint" show-overflow-tooltip>
        <template #default="{ row }">
          <div class="mono">{{ row.worker_url }}</div>
          <div v-if="row.worker_http_url" class="mono og-dim endp-http">{{ row.worker_http_url }}</div>
        </template>
      </el-table-column>
      <el-table-column label="最近探测" :width="colW.lastSeen">
        <template #default="{ row }">{{ formatTime(row.last_seen) }}</template>
      </el-table-column>
      <el-table-column label="操作" :width="colW.actions" fixed="right">
        <template #default="{ row }">
          <el-button link type="primary" @click="$router.push(`/clusters/${row.name}`)">详情</el-button>
          <el-button link type="primary" @click="openEdit(row)">编辑</el-button>
          <el-button link type="danger" :disabled="row.status !== 'offline'" @click="removeCluster(row)">
            移除
          </el-button>
        </template>
      </el-table-column>
    </el-table>

    <!-- 接入/编辑对话框 -->
    <el-dialog v-model="dialogVisible" :title="editing ? '编辑集群' : '接入集群'" width="640px">
      <el-form ref="formRef" :model="form" :rules="rules" label-width="100px">
        <el-form-item label="集群名称" prop="name">
          <el-input v-model="form.name" placeholder="如 dev-cluster" :disabled="editing" />
        </el-form-item>
        <el-form-item label="所属项目">
          <el-select v-model="form.project_id" clearable placeholder="可选" style="width: 100%">
            <el-option v-for="p in projects" :key="p.id" :label="p.name" :value="p.id" />
          </el-select>
        </el-form-item>
        <el-form-item label="gRPC 地址" prop="worker_url">
          <el-input v-model="form.worker_url" placeholder="http://&lt;管理节点IP&gt;:9080" />
          <div class="form-tip">Worker 的 gRPC 管理端口（默认 9080，可按部署自定义）：服务编排、监控与健康探测走此端口</div>
        </el-form-item>
        <el-form-item label="HTTP 地址" prop="worker_http_url">
          <el-input v-model="form.worker_http_url" placeholder="http://&lt;管理节点IP&gt;:8080" />
          <div class="form-tip">Worker 的 HTTP 端口（默认 8080，可自定义，与 gRPC 是两个独立端口）：MCP 端点自动取 {HTTP 地址}/mcp，接入时校验 /healthz</div>
        </el-form-item>
        <el-form-item label="Token">
          <el-input v-model="form.token" type="password" show-password :placeholder="editing && !editingHasToken ? '尚未配置（Worker 未开启鉴权）' : editing ? '已配置，留空不修改' : 'Worker Bearer token（可选）'" />
          <div v-if="editing" class="form-tip">
            <span v-if="editingHasToken" style="color: #67c23a">✓ 已配置 token</span>
            <span v-else style="color: #e6a23c">⚠ 未配置 token——Worker 未开启鉴权，任何能访问 gRPC 端口的人均可操作集群</span>
          </div>
          <div v-else class="form-tip">Worker 开启鉴权（auth.enabled）时填写；未配置则 Worker 不校验请求方身份</div>
        </el-form-item>
        <el-form-item label="描述">
          <el-input v-model="form.desc" placeholder="可选" />
        </el-form-item>
        <div class="form-tip">接入后可到集群详情页点「+纳管」接入外部对象（standalone 容器 / 宿主机服务），并在服务列表中逐条管理</div>
      </el-form>
      <template #footer>
        <el-button @click="dialogVisible = false">取消</el-button>
        <el-button type="primary" :loading="adding" @click="submitCluster">{{ editing ? '保存' : '接入' }}</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage, ElMessageBox, type FormInstance, type FormRules } from 'element-plus'
import { Plus } from '@element-plus/icons-vue'
import { clusterApi, projectApi } from '@/api'
import type { AddClusterPayload, ClusterSummary, Project } from '@/types'

const route = useRoute()
const router = useRouter()

const loading = ref(false)
const adding = ref(false)
const clusters = ref<ClusterSummary[]>([])
const projects = ref<Project[]>([])

// 按项目筛选：支持项目名称模糊搜索；与路由 ?project_id= 双向同步（项目页跳转落点）
const filterProjectId = ref(typeof route.query.project_id === 'string' ? route.query.project_id : '')
const projectQuery = ref('')

const projectOptions = computed(() => {
  const q = projectQuery.value.trim().toLowerCase()
  if (!q) return projects.value
  return projects.value.filter((p) => fuzzyMatch(p.name, q))
})

const filteredClusters = computed(() => {
  if (!filterProjectId.value) return clusters.value
  return clusters.value.filter((c) => c.project_id === filterProjectId.value)
})

const emptyText = computed(() => {
  if (!clusters.value.length) return '暂无集群，点击右上角「接入集群」'
  const p = projects.value.find((x) => x.id === filterProjectId.value)
  return p ? `项目「${p.name}」下暂无集群` : '暂无匹配的集群'
})

/** 模糊匹配：子串命中，或查询字符按顺序全部出现在名称中（如“灾监”命中“自然灾害…监测…”） */
function fuzzyMatch(text: string, query: string) {
  const t = text.toLowerCase()
  const q = query.toLowerCase().trim()
  if (!q) return true
  if (t.includes(q)) return true
  let i = 0
  for (const ch of t) {
    if (ch === q[i]) i++
    if (i === q.length) return true
  }
  return false
}

function onProjectFilter(query: string) {
  projectQuery.value = query
}

function syncFilterQuery() {
  router.replace({ query: { ...route.query, project_id: filterProjectId.value || undefined } })
}

watch(
  () => route.query.project_id,
  (v) => {
    filterProjectId.value = typeof v === 'string' ? v : ''
  },
)

// 深链指向已删除/不存在的项目时自动清空筛选，避免停留在空列表
watch(projects, (list) => {
  if (filterProjectId.value && !list.some((p) => p.id === filterProjectId.value)) {
    filterProjectId.value = ''
  }
})

// —— 列宽自适应：按表头与单元格实际内容测量宽度（canvas measureText），
//    名称/端点两列在容器剩余空间内弹性分摊，保证表格恰好铺满不出现横向滚动 ——
const FONT_STACK = `'Helvetica Neue', Helvetica, 'PingFang SC', 'Hiragino Sans GB', 'Microsoft YaHei', Arial, sans-serif`
const CELL_PAD = 24 // el-table 单元格左右内边距 12px × 2
const TAG_PAD = 20 // el-tag small 左右 padding 9px × 2 + 边框
const BUFFER = 10
const measureCtx = document.createElement('canvas').getContext('2d')
const tableRef = ref()
const tableWidth = ref(0)
let tableResizeObserver: ResizeObserver | undefined

function textWidth(text: string, px: number, weight = 400) {
  if (!measureCtx || !text) return 0
  measureCtx.font = `${weight} ${px}px ${FONT_STACK}`
  return measureCtx.measureText(text).width
}

const colW = computed(() => {
  const cs = clusters.value
  const longest = (f: (c: ClusterSummary) => number) => Math.max(0, ...cs.map(f))
  const col = (label: string, content: number, min: number, max: number) =>
    Math.min(max, Math.max(min, Math.ceil(Math.max(textWidth(label, 14), content) + CELL_PAD + BUFFER)))
  const statusTextW =
    Math.max(textWidth('在线', 12), textWidth('离线', 12), textWidth('未知', 12)) + TAG_PAD

  const project = col('项目', longest((c) => (projectName(c) ? textWidth(projectName(c)!, 12) + TAG_PAD : 0)), 90, 320)
  const status = col('状态', statusTextW, 80, 140)
  const lastSeen = col('最近探测', longest((c) => textWidth(formatTime(c.last_seen), 14)), 150, 220)
  const actions = Math.max(150, Math.ceil(3 * textWidth('详情', 14) + 2 * 12 + 3 * 10 + CELL_PAD))
  const nameNeed = col('名称', longest((c) => textWidth(c.name, 14, 600) + (c.desc ? 8 + textWidth(c.desc, 12) : 0)), 120, 520)
  const endpointNeed = col(
    'Manager 端点',
    longest((c) => Math.max(textWidth(c.worker_url ?? '', 14), textWidth(c.worker_http_url ?? '', 12))),
    160,
    420,
  )

  // 容器减去固定内容列后，剩余宽度按两列的内容占比分摊（不足时按比例压缩）
  const avail = Math.max(320, (tableWidth.value || 1040) - project - status - lastSeen - actions)
  const need = nameNeed + endpointNeed
  const nameW = Math.max(90, Math.floor((avail * nameNeed) / need))
  return {
    name: nameW,
    project,
    status,
    endpoint: Math.max(90, avail - nameW),
    lastSeen,
    actions,
  }
})

onMounted(() => {
  const el = tableRef.value?.$el as HTMLElement | undefined
  if (el && typeof ResizeObserver !== 'undefined') {
    tableResizeObserver = new ResizeObserver((entries) => {
      tableWidth.value = entries[0]?.contentRect.width ?? el.clientWidth
    })
    tableResizeObserver.observe(el)
    tableWidth.value = el.clientWidth
  }
})

onBeforeUnmount(() => tableResizeObserver?.disconnect())

const dialogVisible = ref(false)
const editing = ref(false)
const editingHasToken = ref(false)
const formRef = ref<FormInstance>()
const form = reactive<AddClusterPayload>({
  name: '',
  project_id: '',
  worker_url: '',
  worker_http_url: '',
  token: '',
  desc: '',
})

const rules: FormRules = {
  name: [
    { required: true, message: '请输入集群名称', trigger: 'blur' },
    {
      pattern: /^[a-zA-Z0-9][a-zA-Z0-9._-]{0,62}$/,
      message: '字母/数字开头，可含 . _ -，最长 63 字符',
      trigger: 'blur',
    },
  ],
  worker_url: [{ required: true, message: '请输入 Worker gRPC 地址', trigger: 'blur' }],
  worker_http_url: [{ required: true, message: '请输入 Worker HTTP 地址', trigger: 'blur' }],
}

/** 从 gRPC 地址推导 HTTP 地址默认值：同主机 + 8080 端口（Worker 默认端口约定） */
function deriveHTTP(url: string) {
  try {
    const u = new URL(url.trim())
    if (!u.hostname) return ''
    return `${u.protocol}//${u.hostname}:8080`
  } catch {
    return ''
  }
}

// 填 gRPC 地址时自动补全 HTTP 地址（仅覆盖空值或仍是自动推导值，不抢用户手输）
const lastAutoHttp = ref('')
watch(
  () => form.worker_url,
  (v) => {
    const d = deriveHTTP(v)
    if (!d) return
    if (!form.worker_http_url || form.worker_http_url === lastAutoHttp.value) {
      form.worker_http_url = d
      lastAutoHttp.value = d
    }
  },
)

function statusText(s: string) {
  return s === 'online' ? '在线' : s === 'offline' ? '离线' : '未知'
}
function statusTag(s: string) {
  return s === 'online' ? 'success' : s === 'offline' ? 'danger' : 'info'
}
function formatTime(ts?: string) {
  if (!ts) return '—'
  return new Date(ts).toLocaleString()
}
function projectName(row: ClusterSummary) {
  return projects.value.find((p) => p.id === row.project_id)?.name
}

async function fetchClusters() {
  loading.value = true
  try {
    const resp = await clusterApi.list()
    clusters.value = resp.items ?? []
  } finally {
    loading.value = false
  }
}

async function fetchProjects() {
  try {
    const resp = await projectApi.list()
    projects.value = (resp.items ?? []).map((v) => v.project)
  } catch {
    projects.value = []
  }
}

function openDialog() {
  editing.value = false
  lastAutoHttp.value = ''
  Object.assign(form, { name: '', project_id: '', worker_url: '', worker_http_url: '', token: '', desc: '' })
  dialogVisible.value = true
}

function openEdit(row: ClusterSummary) {
  editing.value = true
  editingHasToken.value = !!row.has_token
  // 旧记录无 worker_http_url：按默认端口约定（同主机 :8080）预填，可改
  const httpURL = row.worker_http_url ?? deriveHTTP(row.worker_url)
  lastAutoHttp.value = httpURL
  Object.assign(form, {
    name: row.name,
    project_id: row.project_id ?? '',
    worker_url: row.worker_url,
    worker_http_url: httpURL,
    token: '',
    desc: row.desc ?? '',
  })
  dialogVisible.value = true
}

async function submitCluster() {
  await formRef.value?.validate()
  adding.value = true
  try {
    if (editing.value) {
      await clusterApi.update(form.name, form)
      ElMessage.success('集群已更新')
    } else {
      await clusterApi.add(form)
      ElMessage.success('集群接入成功')
    }
    dialogVisible.value = false
    lastAutoHttp.value = ''
    Object.assign(form, { name: '', project_id: '', worker_url: '', worker_http_url: '', token: '', desc: '' })
    await fetchClusters()
  } catch {
    // 错误提示已由 http.ts 统一处理（探测失败 502 等）
  } finally {
    adding.value = false
  }
}

async function removeCluster(row: ClusterSummary) {
  try {
    await ElMessageBox.confirm(`确定移除集群「${row.name}」？此操作仅删除管理端注册记录。`, '移除集群', {
      type: 'warning',
    })
  } catch {
    return // 用户取消
  }
  await clusterApi.remove(row.name)
  ElMessage.success('已移除')
  await fetchClusters()
}

onMounted(async () => {
  await Promise.all([fetchClusters(), fetchProjects()])
})
</script>

<style scoped>
.page-head {
  display: flex;
  align-items: flex-end;
  justify-content: space-between;
  margin-bottom: 16px;
}
.page-title {
  margin: 0;
  font-size: 20px;
  font-weight: 700;
  letter-spacing: -0.02em;
}
.page-sub {
  margin: 4px 0 0;
  color: var(--og-text-dim);
  font-size: 12px;
}
.toolbar {
  display: flex;
  align-items: center;
  gap: 12px;
  margin-bottom: 14px;
}
.project-filter {
  width: 280px;
}
.filter-summary {
  color: var(--og-text-dim);
  font-size: 12px;
}
.cluster-name {
  font-weight: 600;
}
.cluster-desc {
  margin-left: 8px;
  color: var(--og-text-dim);
  font-size: 12px;
}
.muted {
  color: var(--og-text-dim);
}
.endp-http {
  font-size: 12px;
  line-height: 1.5;
}
.form-tip {
  color: var(--og-text-dim);
  font-size: 12px;
  line-height: 1.6;
}
</style>
