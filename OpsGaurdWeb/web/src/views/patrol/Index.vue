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
        <el-form-item label="流程 YAML" prop="yaml">
          <el-input v-model="form.yaml" type="textarea" :rows="14" class="mono"
            placeholder="name: nightly
checks:
  - type: resource
    cluster: dev
    service: web
    cpu_threshold: 85
    mem_threshold: 90
  - type: health
    cluster: dev
    service: api
    min_replicas: 2
report:
  model: deepseek-chat" />
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
import { patrolApi } from '@/api'
import type { Patrol, PatrolReport, PatrolRun } from '@/types'

const loading = ref(false)
const patrols = ref<Patrol[]>([])
const saving = ref(false)
const runningId = ref('')

const formVisible = ref(false)
const editing = ref(false)
const formRef = ref<FormInstance>()
const form = reactive({ id: '', name: '', description: '', cron: '', enabled: true, yaml: '' })

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
  Object.assign(form, { id: '', name: '', description: '', cron: '0 2 * * *', enabled: true, yaml: '' })
  formVisible.value = true
}

function openEdit(row: Patrol) {
  editing.value = true
  Object.assign(form, { id: row.id, name: row.name, description: row.description ?? '', cron: row.cron, enabled: row.enabled, yaml: row.yaml })
  formVisible.value = true
}

async function save() {
  await formRef.value?.validate()
  saving.value = true
  try {
    if (editing.value) {
      await patrolApi.update(form.id, { ...form })
      ElMessage.success('已更新')
    } else {
      await patrolApi.create({ ...form })
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

onMounted(fetchPatrols)
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
