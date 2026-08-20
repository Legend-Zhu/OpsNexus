<template>
  <div class="prompts">
    <el-alert type="info" :closable="false" class="mb"
      title="提示词场景化管理"
      description="内置场景与代码默认逐字节一致；修改保存新版本并激活后立即生效（无需重启），可预览、回滚任意版本。未自定义/回滚 v1 时线上行为与代码内置完全一致。" />

    <!-- 场景列表 -->
    <el-table :data="prompts" size="small" v-loading="loading" empty-text="暂无提示词"
      highlight-current-row @current-change="onSelect">
      <el-table-column prop="name" label="场景" min-width="180" />
      <el-table-column prop="scenario" label="key" min-width="160">
        <template #default="{ row }"><span class="mono">{{ row.scenario }}</span></template>
      </el-table-column>
      <el-table-column label="类型" width="90">
        <template #default="{ row }">
          <el-tag size="small" :type="row.builtin ? 'info' : 'success'" effect="plain">
            {{ row.builtin ? '内置' : '自定义' }}
          </el-tag>
        </template>
      </el-table-column>
      <el-table-column label="状态" width="140">
        <template #default="{ row }">
          <span v-if="!row.builtin">v{{ row.active_version }} 生效</span>
          <span v-else-if="row.materialized">v{{ row.active_version }} 生效</span>
          <span v-else class="muted">未自定义（内置默认）</span>
        </template>
      </el-table-column>
      <el-table-column label="更新时间" width="180">
        <template #default="{ row }">{{ fmtTime(row.updated_at) }}</template>
      </el-table-column>
    </el-table>
    <el-button v-if="isAdmin" class="mt" size="small" :icon="Plus" @click="openCreate">新建自定义提示词</el-button>

    <!-- 详情：编辑 + 预览 + 版本历史 -->
    <div v-if="detail" class="detail">
      <div class="detail-head">
        <h3>{{ detail.name }} <span class="mono muted">({{ detail.scenario }})</span></h3>
        <el-space>
          <el-tag v-if="detail.builtin" size="small" type="info" effect="plain">内置场景 · v1 为代码默认</el-tag>
          <el-button v-if="isAdmin && detail.builtin && detail.materialized" size="small" @click="reset">还原默认</el-button>
          <el-button v-if="isAdmin && !detail.builtin" size="small" type="danger" @click="removeCustom">删除</el-button>
        </el-space>
      </div>

      <!-- 变量说明 -->
      <div v-if="activeVars.length" class="vars">
        <span class="vars-title">模板变量：</span>
        <el-tag v-for="v in activeVars" :key="v.name" size="small" :type="v.required ? 'warning' : 'info'" effect="plain" class="var-tag">
          {{ v.name }}{{ v.required ? ' *' : '' }}<span v-if="v.note" class="muted"> · {{ v.note }}</span>
        </el-tag>
      </div>

      <!-- 编辑器（基于 active 版本拷贝，保存为新版本；非 admin 只读浏览） -->
      <el-divider content-position="left">
        编辑新版本（Go text/template 语法）{{ isAdmin ? '' : '（只读，写操作需管理员）' }}
      </el-divider>
      <template v-if="isAdmin">
        <div v-for="(m, i) in editor.messages" :key="i" class="msg-row">
          <el-select v-model="m.role" size="small" style="width: 110px">
            <el-option label="system" value="system" />
            <el-option label="user" value="user" />
          </el-select>
          <el-input v-model="m.template" type="textarea" :rows="Math.min(12, Math.max(3, m.template.split('\n').length))"
            class="mono msg-input" placeholder="模板正文，变量用 {{ '{{.Alert.Title}}' }} 形式引用" />
          <el-button v-if="editor.messages.length > 1" link type="danger" :icon="Delete" @click="editor.messages.splice(i, 1)" />
        </div>
        <el-button size="small" :icon="Plus" @click="editor.messages.push({ role: 'user', template: '' })">添加消息</el-button>
        <div class="save-bar">
          <el-input v-model="editor.note" placeholder="版本备注（可选）" style="width: 320px" />
          <el-checkbox v-model="editor.activate">保存后立即激活</el-checkbox>
          <el-button type="primary" :loading="saving" @click="saveVersion">保存新版本 (v{{ detail.versions.length + 1 }})</el-button>
        </div>
      </template>

      <!-- 预览 -->
      <el-divider content-position="left">渲染预览（data 为 JSON，按场景数据结构）</el-divider>
      <div class="preview">
        <el-input v-model="previewData" type="textarea" :rows="4" class="mono" placeholder='{"alert": {...}}' />
        <el-button :loading="rendering" @click="render">渲染预览</el-button>
      </div>
      <div v-if="rendered" class="rendered">
        <div v-for="(m, i) in rendered" :key="i" class="rendered-msg">
          <el-tag size="small" effect="plain">{{ m.role }}</el-tag>
          <pre class="mono">{{ m.content }}</pre>
        </div>
      </div>

      <!-- 版本历史 -->
      <el-divider content-position="left">版本历史</el-divider>
      <el-table :data="[...detail.versions].reverse()" size="small">
        <el-table-column prop="version" label="版本" width="70">
          <template #default="{ row }">
            <el-tag v-if="row.version === detail.active_version" size="small" type="success">v{{ row.version }} 生效</el-tag>
            <span v-else>v{{ row.version }}</span>
          </template>
        </el-table-column>
        <el-table-column prop="note" label="备注" min-width="160" />
        <el-table-column prop="created_by" label="操作人" width="100" />
        <el-table-column label="时间" width="170">
          <template #default="{ row }">{{ fmtTime(row.created_at) }}</template>
        </el-table-column>
        <el-table-column label="操作" width="90">
          <template #default="{ row }">
            <el-button v-if="isAdmin && row.version !== detail.active_version" link type="primary" @click="activate(row.version)">
              {{ row.version === 1 && detail.builtin ? '还原默认' : '切换' }}
            </el-button>
          </template>
        </el-table-column>
      </el-table>
    </div>

    <!-- 新建自定义 -->
    <el-dialog v-model="createVisible" title="新建自定义提示词" width="560px">
      <el-form label-width="90px">
        <el-form-item label="名称" required>
          <el-input v-model="createForm.name" placeholder="如 实验提示词" />
        </el-form-item>
        <el-form-item label="首条消息" required>
          <el-select v-model="createForm.role" style="width: 110px; margin-right: 8px">
            <el-option label="system" value="system" />
            <el-option label="user" value="user" />
          </el-select>
          <el-input v-model="createForm.template" type="textarea" :rows="5" class="mono"
            placeholder="模板正文，变量用 {{ '{{.Name}}' }} 形式引用（JSON data 提供值）" />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="createVisible = false">取消</el-button>
        <el-button type="primary" :loading="saving" @click="create">创建</el-button>
      </template>
    </el-dialog>
  </div>
</template>
<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Delete, Plus } from '@element-plus/icons-vue'
import { mlopsApi } from '@/api'
import { isAdmin, loadAdminFlag } from '@/composables/admin'
import type { MLOpsPrompt, MLOpsPromptMessage, MLOpsRenderedMessage } from '@/types'

const prompts = ref<MLOpsPrompt[]>([])
const detail = ref<MLOpsPrompt | null>(null)
const loading = ref(false)
const saving = ref(false)
const rendering = ref(false)

const editor = reactive({ messages: [] as MLOpsPromptMessage[], note: '', activate: true })
const previewData = ref('{}')
const rendered = ref<MLOpsRenderedMessage[] | null>(null)

const createVisible = ref(false)
const createForm = reactive({ name: '', role: 'user' as 'system' | 'user', template: '' })

const activeVars = computed(() => detail.value?.versions.find((v) => v.version === detail.value?.active_version)?.variables ?? [])

async function load(selectId?: string) {
  loading.value = true
  try {
    const resp = await mlopsApi.prompts()
    prompts.value = resp.items ?? []
    if (selectId) await select(selectId)
    else if (detail.value) await select(detail.value.id)
  } catch {
    prompts.value = []
  } finally {
    loading.value = false
  }
}

function onSelect(row: MLOpsPrompt | null) {
  if (row) void select(row.id)
}

async function select(id: string) {
  try {
    detail.value = await mlopsApi.prompt(id)
    const active = detail.value.versions.find((v) => v.version === detail.value!.active_version)
    editor.messages = (active?.messages ?? []).map((m) => ({ role: m.role, template: m.template }))
    editor.note = ''
    rendered.value = null
    previewData.value = defaultPreviewData(detail.value.scenario)
  } catch {
    /* 错误已提示 */
  }
}

// 场景默认预览数据（与后端数据结构一致）
function defaultPreviewData(scenario: string): string {
  switch (scenario) {
    case 'investigate_system':
      return JSON.stringify({ use_mcp: true }, null, 2)
    case 'investigate_user':
      return JSON.stringify({
        alert: { cluster: 'dev', service: 'web', type: 'port_down', level: 'error', title: '端口不可测：8080 down', count: 3, first_ts: '2026-08-20T10:00:00Z', last_ts: '2026-08-20T10:05:00Z' },
        events: '[{"id":"e1","msg":"8080 down"}]',
        audit: '',
        logs: '[t1/stdout] started\n[t2/stderr] bind: address already in use',
        use_mcp: false,
      }, null, 2)
    case 'compress_system':
      return JSON.stringify({ max_words: 80, content: '[用户] 排查 web\n[助手] 调用工具: probe\n' }, null, 2)
    default:
      return '{}'
  }
}

async function saveVersion() {
  if (!detail.value) return
  if (!editor.messages.length || editor.messages.some((m) => !m.template.trim())) {
    ElMessage.warning('请至少填写一条非空消息')
    return
  }
  saving.value = true
  try {
    await mlopsApi.savePrompt(detail.value.id, {
      expected_active_version: detail.value.active_version,
      activate: editor.activate,
      version: { messages: editor.messages.map((m) => ({ role: m.role, template: m.template })), note: editor.note || undefined },
    })
    ElMessage.success('新版本已保存' + (editor.activate ? '并激活' : '（未激活）'))
    await load(detail.value.id)
  } catch {
    /* 409/400 已提示 */
  } finally {
    saving.value = false
  }
}

async function render() {
  if (!detail.value) return
  let data: unknown
  try {
    data = JSON.parse(previewData.value || '{}')
  } catch {
    ElMessage.warning('data 不是合法 JSON')
    return
  }
  rendering.value = true
  try {
    const resp = await mlopsApi.renderPrompt(detail.value.id, { version: 0, data })
    rendered.value = resp.messages ?? []
  } catch {
    /* 错误已提示 */
  } finally {
    rendering.value = false
  }
}

async function activate(version: number) {
  if (!detail.value) return
  await ElMessageBox.confirm(`切换到 v${version}？保存/切换立即生效。`, '激活版本', { type: 'warning' })
  try {
    await mlopsApi.activatePrompt(detail.value.id, { version, expected_active_version: detail.value.active_version })
    ElMessage.success(`已切换到 v${version}`)
    await load(detail.value.id)
  } catch {
    /* 错误已提示 */
  }
}

async function reset() {
  if (!detail.value) return
  await ElMessageBox.confirm('恢复代码默认？用户版本保留在历史中，可随时再切换。', '还原默认', { type: 'warning' })
  try {
    await mlopsApi.resetPrompt(detail.value.id)
    ElMessage.success('已恢复代码默认')
    await load(detail.value.id)
  } catch {
    /* 错误已提示 */
  }
}

async function removeCustom() {
  if (!detail.value) return
  await ElMessageBox.confirm(`删除自定义提示词「${detail.value.name}」？`, '删除', { type: 'warning' })
  await mlopsApi.deletePrompt(detail.value.id)
  ElMessage.success('已删除')
  detail.value = null
  await load()
}

function openCreate() {
  Object.assign(createForm, { name: '', role: 'user', template: '' })
  createVisible.value = true
}

async function create() {
  if (!createForm.name.trim() || !createForm.template.trim()) {
    ElMessage.warning('请填写名称与模板')
    return
  }
  saving.value = true
  try {
    const p = await mlopsApi.createPrompt({
      name: createForm.name,
      version: { messages: [{ role: createForm.role, template: createForm.template }] },
    })
    ElMessage.success('已创建')
    createVisible.value = false
    await load(p.id)
  } catch {
    /* 错误已提示 */
  } finally {
    saving.value = false
  }
}

function fmtTime(t?: string): string {
  if (!t) return '—'
  const d = new Date(t)
  return Number.isNaN(d.getTime()) ? '—' : d.toLocaleString()
}

onMounted(() => {
  void loadAdminFlag()
  load()
})
</script>

<style scoped>
.mb {
  margin-bottom: 12px;
}
.mt {
  margin-top: 12px;
}
.mono {
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  font-size: 12px;
}
.muted {
  color: var(--el-text-color-placeholder);
}
.detail {
  margin-top: 20px;
}
.detail-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
}
.detail-head h3 {
  margin: 0;
}
.vars {
  margin-top: 8px;
  line-height: 26px;
}
.vars-title {
  font-size: 13px;
  color: var(--el-text-color-secondary);
}
.var-tag {
  margin-right: 6px;
}
.msg-row {
  display: flex;
  gap: 8px;
  margin-bottom: 8px;
  align-items: flex-start;
}
.msg-input {
  flex: 1;
}
.save-bar {
  display: flex;
  gap: 12px;
  align-items: center;
  margin-top: 8px;
}
.preview {
  display: flex;
  gap: 8px;
  align-items: flex-start;
}
.preview .el-input {
  flex: 1;
}
.rendered {
  margin-top: 8px;
}
.rendered-msg {
  margin-bottom: 8px;
}
.rendered-msg pre {
  margin: 4px 0 0;
  padding: 8px;
  background: var(--el-fill-color-light);
  border-radius: 4px;
  white-space: pre-wrap;
  word-break: break-all;
  max-height: 320px;
  overflow: auto;
}
</style>
