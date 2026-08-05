<template>
  <el-card shadow="never">
    <template #header>
      <div class="card-header">
        <span>智能巡检</span>
        <div class="header-right">
          <el-button type="primary" :icon="Plus" @click="openCreate">新建流程</el-button>
        </div>
      </div>
    </template>

    <!-- 流程列表 -->
    <el-table v-loading="loading" :data="patrols" empty-text="暂无巡检流程">
      <el-table-column label="名称" prop="name" min-width="140">
        <template #default="{ row }">
          <el-link type="primary" @click="openDetail(row)">{{ row.name }}</el-link>
        </template>
      </el-table-column>
      <el-table-column label="描述" prop="description" min-width="160" show-overflow-tooltip>
        <template #default="{ row }">{{ row.description || '—' }}</template>
      </el-table-column>
      <el-table-column label="调度" prop="cron" width="110" />
      <el-table-column label="状态" width="90">
        <template #default="{ row }">
          <el-tag size="small" :type="row.enabled ? 'success' : 'info'">
            {{ row.enabled ? '启用' : '停用' }}
          </el-tag>
        </template>
      </el-table-column>
      <el-table-column label="更新时间" width="170">
        <template #default="{ row }">{{ new Date(row.updated_at).toLocaleString() }}</template>
      </el-table-column>
      <el-table-column label="操作" width="230" fixed="right">
        <template #default="{ row }">
          <el-button link type="primary" @click="openDetail(row)">执行记录</el-button>
          <el-button link type="primary" :loading="runningId === row.id" @click="runNow(row)">立即执行</el-button>
          <el-button link type="warning" @click="openEdit(row)">编辑</el-button>
          <el-button link type="danger" @click="remove(row)">删除</el-button>
        </template>
      </el-table-column>
    </el-table>

    <!-- 新建/编辑对话框 -->
    <el-dialog v-model="formVisible" :title="editing ? '编辑流程' : '新建流程'" width="680px">
      <el-form ref="formRef" :model="form" :rules="rules" label-width="90px">
        <el-form-item label="名称" prop="name">
          <el-input v-model="form.name" placeholder="如 nightly-check" />
        </el-form-item>
        <el-form-item label="描述">
          <el-input v-model="form.description" placeholder="可选" />
        </el-form-item>
        <el-form-item label="Cron" prop="cron">
          <el-input v-model="form.cron" placeholder="5 字段，如 0 2 * * *（每天 02:00）" />
        </el-form-item>
        <el-form-item label="启用">
          <el-switch v-model="form.enabled" />
        </el-form-item>
        <el-form-item label="报告模型">
          <el-select v-model="form.model" clearable filterable placeholder="默认（AI 网关首个可用模型）" style="width: 100%">
            <el-option v-for="m in models" :key="m.name" :label="modelLabel(m)" :value="m.name" />
          </el-select>
          <span class="muted ml">来自「系统设置 → 模型配置」的模型池</span>
        </el-form-item>
        <el-form-item label="流程 YAML" prop="yaml">
          <el-input v-model="form.yaml" type="textarea" :rows="14" class="mono"
            placeholder="name: nightly
checks:
  - type: resource      # 服务容器资源阈值
    cluster: dev
    service: web
    cpu_threshold: 85
    mem_threshold: 90
  - type: health        # 服务副本健康
    cluster: dev
    service: api
    min_replicas: 2
  - type: port          # 从节点探测任意 host:port（node 空=全部 ready 节点）
    cluster: dev
    host: 10.0.0.11     # 探宿主机中间件用节点 IP，勿用 127.0.0.1
    port: 3306
  - type: http          # 从节点探测任意 URL（expected_status 空=2xx）
    cluster: dev
    url: http://10.0.0.11:8080/healthz
  - type: process       # 宿主机进程（filter 匹配名称/命令行，少于 min_count=1 即异常）
    cluster: dev
    filter: java
# report.model 由上方「报告模型」下拉自动写入（也可手写覆盖）" />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="formVisible = false">取消</el-button>
        <el-button type="primary" :loading="saving" @click="save">保存</el-button>
      </template>
    </el-dialog>

    <!-- 执行记录/报告抽屉 -->
    <el-drawer v-model="detailVisible" :title="`${current?.name ?? ''} · 执行记录`" size="55%">
      <template v-if="current">
        <h4>流程定义</h4>
        <pre class="yaml-box">{{ current.yaml }}</pre>

        <h4>执行记录</h4>
        <el-table :data="runs" size="small">
          <el-table-column prop="id" label="ID" width="110" />
          <el-table-column prop="status" label="状态" width="90">
            <template #default="{ row }">
              <el-tag size="small" :type="runStatusTag(row.status)">{{ row.status }}</el-tag>
            </template>
          </el-table-column>
          <el-table-column label="开始" width="160">
            <template #default="{ row }">{{ new Date(row.started_at).toLocaleString() }}</template>
          </el-table-column>
          <el-table-column label="异常" width="70" align="center">
            <template #default="{ row }">{{ anomalyCount(row) }}</template>
          </el-table-column>
          <el-table-column label="操作" width="80">
            <template #default="{ row }">
              <el-button link type="primary" @click="viewRun(row)">详情</el-button>
            </template>
          </el-table-column>
        </el-table>
      </template>
    </el-drawer>

    <!-- 单次执行详情（检查明细 + 报告） -->
    <el-drawer v-model="runVisible" title="执行详情" size="55%">
      <template v-if="currentRun">
        <h4>检查明细</h4>
        <el-table :data="currentRun.anomalies ?? []" size="small">
          <el-table-column prop="check" label="检查项" min-width="130" />
          <el-table-column prop="cluster" label="集群" width="100" />
          <el-table-column prop="service" label="服务" width="100" />
          <el-table-column label="结果" width="70">
            <template #default="{ row }">
              <el-tag size="small" :type="row.ok ? 'success' : 'danger'">{{ row.ok ? '正常' : '异常' }}</el-tag>
            </template>
          </el-table-column>
          <el-table-column prop="message" label="说明" min-width="160" show-overflow-tooltip />
        </el-table>

        <h4>AI 报告</h4>
        <pre class="report-box">{{ currentReport?.ai_summary || '（无报告）' }}</pre>
      </template>
    </el-drawer>
  </el-card>
</template>

<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox, type FormInstance, type FormRules } from 'element-plus'
import { Plus } from '@element-plus/icons-vue'
import { ainexusApi, patrolApi } from '@/api'
import type { AINexusModelInfo, Patrol, PatrolReport, PatrolRun } from '@/types'

const loading = ref(false)
const patrols = ref<Patrol[]>([])
const saving = ref(false)
const runningId = ref('')

// 报告模型：统一取自 AI 排查网关配置（/v1/ainexus/models）；网关未启用时为空
// 数组，巡检降级为无 AI 报告，不影响检查。
const models = ref<AINexusModelInfo[]>([])

const formVisible = ref(false)
const editing = ref(false)
const formRef = ref<FormInstance>()
const form = reactive({ id: '', name: '', description: '', cron: '', enabled: true, yaml: '', model: '' })

const rules: FormRules = {
  name: [{ required: true, message: '请输入名称', trigger: 'blur' }],
  cron: [{ required: true, message: '请输入 cron 表达式', trigger: 'blur' }],
  yaml: [{ required: true, message: '请输入流程 YAML', trigger: 'blur' }],
}

const detailVisible = ref(false)
const current = ref<Patrol | null>(null)
const runs = ref<PatrolRun[]>([])

const runVisible = ref(false)
const currentRun = ref<PatrolRun | null>(null)
const currentReport = ref<PatrolReport | null>(null)

function runStatusTag(s: string) {
  return s === 'success' ? 'success' : s === 'running' ? 'warning' : 'danger'
}

function anomalyCount(run: PatrolRun) {
  return (run.anomalies ?? []).filter((a) => !a.ok).length
}

async function fetchPatrols() {
  loading.value = true
  try {
    const resp = await patrolApi.list()
    patrols.value = resp.items ?? []
  } finally {
    loading.value = false
  }
}

function openCreate() {
  editing.value = false
  Object.assign(form, { id: '', name: '', description: '', cron: '0 2 * * *', enabled: true, yaml: '', model: '' })
  formVisible.value = true
}

function openEdit(row: Patrol) {
  editing.value = true
  Object.assign(form, {
    id: row.id,
    name: row.name,
    description: row.description ?? '',
    cron: row.cron,
    enabled: row.enabled,
    yaml: row.yaml,
    model: readYamlReportModel(row.yaml),
  })
  formVisible.value = true
}

async function save() {
  await formRef.value?.validate()
  saving.value = true
  try {
    const body = { ...form, yaml: setYamlReportModel(form.yaml, form.model) }
    if (editing.value) {
      await patrolApi.update(form.id, body)
      ElMessage.success('已更新')
    } else {
      await patrolApi.create(body)
      ElMessage.success('已创建')
    }
    formVisible.value = false
    await fetchPatrols()
  } catch {
    // 错误已由 http.ts 统一提示（校验失败 400 等）
  } finally {
    saving.value = false
  }
}

// --- YAML report.model 同步（下拉与 YAML 双向，不引入 yaml 库，逐行处理） ---

// readYamlReportModel 读取 report: 块下的 model 值（未配置返回空串）。
function readYamlReportModel(yaml: string): string {
  let inReport = false
  for (const line of yaml.split('\n')) {
    const trimmed = line.trim()
    if (/^report\s*:/.test(trimmed)) {
      inReport = true
      continue
    }
    if (inReport && !/^\s+/.test(line)) break // 离开 report 块
    if (inReport) {
      const m = trimmed.match(/^model\s*:\s*(\S+)\s*$/)
      if (m) return m[1]
    }
  }
  return ''
}

// setYamlReportModel 把 model 写回 YAML 的 report 块：空 model = 移除原
// model 行；无 report 块则追加；已有 report 块则在块首插入。
function setYamlReportModel(yaml: string, model: string): string {
  const lines = yaml.split('\n')
  const out: string[] = []
  let inReport = false
  let reportIdx = -1 // report: 行在 out 中的位置
  for (const line of lines) {
    const trimmed = line.trim()
    const isReportKey = /^report\s*:/.test(trimmed)
    if (isReportKey) {
      inReport = true
      reportIdx = out.length
    } else if (inReport && !/^\s+/.test(line)) {
      inReport = false
    }
    // report 块内的 model 行：移除（下方按需重插）
    if (inReport && !isReportKey && /^model\s*:/.test(trimmed)) {
      continue
    }
    out.push(line)
  }
  if (model) {
    const modelLine = `  model: ${model}`
    if (reportIdx >= 0) {
      out.splice(reportIdx + 1, 0, modelLine)
    } else {
      out.push('report:', modelLine)
    }
  }
  return out.join('\n')
}

function modelLabel(m: AINexusModelInfo) {
  return `${m.name}（${m.provider}）`
}

async function runNow(row: Patrol) {
  runningId.value = row.id
  try {
    const run = await patrolApi.run(row.id)
    ElMessage.success(`执行已开始：${run.id}`)
    // 轮询一次获取结果（同步执行，基本立即可见）
    setTimeout(async () => {
      await loadRuns(row)
      runningId.value = ''
    }, 1500)
  } catch {
    runningId.value = ''
  }
}

async function remove(row: Patrol) {
  await ElMessageBox.confirm(`确定删除流程「${row.name}」？执行记录将保留。`, '删除流程', { type: 'warning' })
  await patrolApi.remove(row.id)
  ElMessage.success('已删除')
  await fetchPatrols()
}

async function openDetail(row: Patrol) {
  current.value = row
  detailVisible.value = true
  await loadRuns(row)
}

async function loadRuns(row: Patrol) {
  const resp = await patrolApi.runs(row.id, 20)
  runs.value = resp.items ?? []
}

async function viewRun(run: PatrolRun) {
  currentRun.value = run
  currentReport.value = null
  runVisible.value = true
  if (run.report_id) {
    const reps = await patrolApi.reports(current.value?.id ?? '', 1)
    const found = reps.items?.find((r) => r.id === run.report_id)
    currentReport.value = found ?? null
  }
}

onMounted(async () => {
  await fetchPatrols()
  // 模型统一取自「系统设置 → AI 排查网关」的配置（GET /v1/ainexus/config，
  // 网关未启用也正常返回 200；此时模型列表为空，巡检降级为无 AI 报告）。
  // 不用 /v1/ainexus/models——网关未启用时它返回 503，会被全局拦截器弹错。
  try {
    const cfg = await ainexusApi.config()
    models.value = (cfg.providers ?? []).flatMap((p) =>
      (p.models ?? []).map((m) => ({ name: m.name, provider: p.name, type: p.type })),
    )
  } catch {
    models.value = []
  }
})
</script>

<style scoped>
.card-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
}
.ml {
  margin-left: 8px;
}
.muted {
  color: var(--el-text-color-placeholder);
  font-size: 12px;
}
.header-right {
  display: flex;
  gap: 8px;
}
.mono :deep(textarea) {
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  font-size: 12px;
}
.yaml-box {
  background: #0d1117;
  color: #e6edf3;
  border-radius: 6px;
  padding: 10px;
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  font-size: 12px;
  max-height: 240px;
  overflow: auto;
  white-space: pre-wrap;
}
.report-box {
  background: #f6f8fa;
  border: 1px solid var(--el-border-color-lighter);
  border-radius: 6px;
  padding: 10px;
  font-size: 13px;
  white-space: pre-wrap;
  line-height: 1.7;
}
</style>
