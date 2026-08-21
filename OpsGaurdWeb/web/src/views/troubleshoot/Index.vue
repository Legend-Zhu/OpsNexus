<template>
  <el-card shadow="never" class="ts-card">
    <template #header>
      <div class="card-header">
        <span>异常排查（AiNexus）</span>
        <div>
          <el-tag v-if="gatewayActive" size="small" type="success">已整合 · 进程内</el-tag>
          <el-tag v-else size="small" type="info">网关未启用</el-tag>
        </div>
      </div>
    </template>

    <!-- 网关未启用引导 -->
    <el-alert
      v-if="!gatewayActive && !configLoading"
      type="warning"
      :closable="false"
      class="mb"
      title="AiNexus 网关未启用"
      description="请在 MLOps → 模型接入 配置模型，再到 系统设置 → AI 排查网关 启用网关并选择默认模型，保存后无需重启立即生效。"
    >
      <el-button size="small" type="primary" @click="router.push('/mlops')">配置模型</el-button>
      <el-button size="small" @click="router.push('/system')">启用网关</el-button>
    </el-alert>

    <!-- 会话工具栏：告警可选（不选 = 自由提问） -->
    <div class="toolbar">
      <el-select
        v-model="alertId"
        placeholder="关联告警（可选，不选则自由提问）"
        filterable
        clearable
        style="width: 380px"
        :disabled="running"
        @change="onAlertChange"
      >
        <el-option
          v-for="a in alerts"
          :key="a.id"
          :label="`[${a.cluster}/${a.service || '-'}] ${a.title} (${a.status})`"
          :value="a.id"
        />
      </el-select>
      <el-switch v-model="useMCP" active-text="MCP 采证" :disabled="running" />
      <el-button size="small" @click="openHistory">历史排查</el-button>
      <el-button size="small" :icon="RefreshLeft" :disabled="!messages.length" @click="resetSession">新会话</el-button>
      <el-button
        v-if="alertId && !messages.length"
        type="primary"
        :icon="Search"
        :loading="running"
        :disabled="!gatewayActive"
        @click="send('')"
      >
        开始排查
      </el-button>
    </div>

    <!-- 对话区 -->
    <div v-if="messages.length" ref="chatBoxRef" class="chat-box">
      <div v-for="(m, i) in messages" :key="i" class="msg" :class="m.role">
        <div class="msg-role">{{ m.role === 'user' ? '我' : 'AI 排查' }}</div>
        <!-- 工具调用（assistant 轮次内） -->
        <div v-if="m.tools?.length" class="tool-list">
          <el-collapse>
            <el-collapse-item v-for="(t, j) in m.tools" :key="j" :name="j">
              <template #title>
                <span class="tool-chip" :class="{ err: t.isError }">⚙ {{ t.name }}</span>
              </template>
              <pre class="tool-result">{{ t.result || '（无输出）' }}</pre>
            </el-collapse-item>
          </el-collapse>
        </div>
        <div class="msg-content" v-html="renderText(m.content + (m.streaming ? ' ▌' : ''))" />
      </div>
    </div>
    <el-empty
      v-else
      :description="alertId ? '点击「开始排查」，AI 将基于事件/日志/审计证据分析该告警' : '选择告警开始排查，或直接输入问题自由提问'"
    />

    <!-- 输入区 -->
    <div class="input-bar">
      <el-input
        v-model="input"
        type="textarea"
        :autosize="{ minRows: 1, maxRows: 4 }"
        :placeholder="messages.length ? '继续追问…（Enter 发送，Shift+Enter 换行）' : alertId ? '也可以先输入补充说明再发送' : '输入你的问题，例如：prod 集群的 java 进程还在吗？'"
        :disabled="running || !gatewayActive"
        @keydown.enter.exact.prevent="send(input)"
      />
      <el-button type="primary" :loading="running" :disabled="!canSend" @click="send(input)">发送</el-button>
    </div>
    <div v-if="status" class="status">{{ status }}</div>

    <!-- 历史排查（选中告警时按告警过滤，否则列出全部会话含自由提问） -->
    <el-dialog v-model="historyVisible" :title="alertId ? '历史排查（当前告警）' : '历史排查（全部会话）'" width="720px">
      <el-table :data="historyList" size="small" v-loading="historyLoading">
        <el-table-column prop="title" label="标题" min-width="200" show-overflow-tooltip />
        <el-table-column label="类型" width="120">
          <template #default="{ row }">
            <el-tag v-if="row.alert_id" size="small" type="warning">告警排查</el-tag>
            <el-tag v-else size="small" type="info">自由提问</el-tag>
          </template>
        </el-table-column>
        <el-table-column label="时间" width="170">
          <template #default="{ row }">{{ new Date(row.updated_at).toLocaleString() }}</template>
        </el-table-column>
        <el-table-column label="操作" width="80">
          <template #default="{ row }">
            <el-button link type="primary" @click="viewHistory(row.id)">查看</el-button>
          </template>
        </el-table-column>
      </el-table>
      <el-empty v-if="!historyLoading && !historyList.length" description="暂无历史排查" />
    </el-dialog>

    <!-- 历史会话只读查看 -->
    <el-dialog v-model="historyViewVisible" :title="historyView?.title ?? ''" width="760px">
      <div v-if="historyView" class="chat-box readonly">
        <div v-for="(m, i) in historyView.messages" :key="i" class="msg" :class="m.role">
          <div class="msg-role">{{ m.role === 'user' ? '我' : 'AI 排查' }}</div>
          <div v-if="m.tools?.length" class="tool-list">
            <span v-for="(t, j) in m.tools" :key="j" class="tool-chip">⚙ {{ t.name }}</span>
          </div>
          <div class="msg-content" v-html="renderText(m.content)" />
        </div>
      </div>
    </el-dialog>
  </el-card>
</template>

<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { RefreshLeft, Search } from '@element-plus/icons-vue'
import { useRoute, useRouter } from 'vue-router'
import { ainexusApi, alertApi, investigationApi } from '@/api'
import type { Alert, Investigation } from '@/types'

interface ToolEvent {
  name: string
  result?: string
  isError?: boolean
}
interface ChatMsg {
  role: 'user' | 'assistant'
  content: string
  tools?: ToolEvent[]
  streaming?: boolean
}

const router = useRouter()
const route = useRoute()
const alerts = ref<Alert[]>([])
const alertId = ref('')
const useMCP = ref(true)

const gatewayActive = ref(false)
const configLoading = ref(true)

const messages = ref<ChatMsg[]>([])
const input = ref('')
const running = ref(false)
const status = ref('')
const chatBoxRef = ref<HTMLElement>()

// 会话状态：alertId 在首轮固定（sessionAlertId），apiHistory 是发给网关的
// 客户端消息（告警会话的证据前缀由服务端注入，不含在这里）；savedInvId 为
// 落库记录 id（首轮完成 POST，后续轮 PUT 更新）。
const sessionAlertId = ref('')
const apiHistory = { current: [] as { role: string; content: string }[] }
const savedInvId = ref('')

const historyVisible = ref(false)
const historyLoading = ref(false)
const historyList = ref<Investigation[]>([])
const historyViewVisible = ref(false)
const historyView = ref<{ title: string; messages: ChatMsg[] } | null>(null)

let abort: AbortController | null = null

const currentAlert = computed(() => alerts.value.find((a) => a.id === sessionAlertId.value))
const canSend = computed(() => gatewayActive.value && !running.value && (!!input.value.trim() || (!!alertId.value && !messages.value.length)))

function renderText(s: string) {
  return s
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .split('\n')
    .map((l) => `<div class="line">${l || '&nbsp;'}</div>`)
    .join('')
}

function scrollChat() {
  void nextTick(() => {
    const el = chatBoxRef.value
    if (el) el.scrollTop = el.scrollHeight
  })
}

async function loadAlerts() {
  try {
    const resp = await alertApi.list()
    alerts.value = resp.items ?? []
  } catch {
    alerts.value = []
  }
}

// 切换告警 = 新会话（有对话时先确认）
async function onAlertChange(v: string) {
  if (!messages.value.length) {
    return
  }
  try {
    await ElMessageBox.confirm('切换告警将开始新会话（当前会话已自动保存）。', '新会话', { type: 'info' })
    doReset()
  } catch {
    alertId.value = sessionAlertId.value // 取消则还原选择
  }
  void v
}

function doReset() {
  abort?.abort()
  messages.value = []
  apiHistory.current = []
  savedInvId.value = ''
  sessionAlertId.value = ''
  status.value = ''
  input.value = ''
}

function resetSession() {
  doReset()
}

async function send(typed: string) {
  const text = typed.trim()
  const firstTurn = messages.value.length === 0
  if (firstTurn) {
    sessionAlertId.value = alertId.value
  }
  const withAlert = !!sessionAlertId.value
  // 告警会话首轮且未输入：服务端证据消息本身即是提问，无需客户端消息
  const sendUserMsg = text ? [{ role: 'user', content: text }] : []
  if (!withAlert && !text) {
    ElMessage.warning('请输入问题')
    return
  }

  const displayText = text || `请排查该告警：${currentAlert.value?.title ?? ''}`
  messages.value.push({ role: 'user', content: displayText })
  const assistant: ChatMsg = { role: 'assistant', content: '', streaming: true }
  messages.value.push(assistant)
  input.value = ''
  running.value = true
  status.value = '正在分析…'
  scrollChat()

  abort?.abort()
  abort = new AbortController()
  try {
    const resp = await fetch(ainexusApi.chatUrl(), {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', ...authHeaders() },
      body: JSON.stringify({
        stream: true,
        use_mcp: useMCP.value,
        alert_id: sessionAlertId.value || undefined,
        messages: [...apiHistory.current, ...sendUserMsg],
      }),
      signal: abort.signal,
    })
    if (!resp.ok || !resp.body) {
      const body = await resp.text().catch(() => '')
      assistant.content = `排查失败（${resp.status}）：${body}`
      assistant.streaming = false
      status.value = '排查失败'
      return
    }
    await consumeSSE(resp.body, assistant)
    status.value = '已完成'
  } catch (e) {
    if ((e as Error).name !== 'AbortError') {
      ElMessage.error(`请求中断：${(e as Error).message}`)
      status.value = '中断'
    }
  } finally {
    assistant.streaming = false
    running.value = false
  }

  // 推进 API 历史 + 自动落库
  apiHistory.current.push(...sendUserMsg, { role: 'assistant', content: assistant.content })
  await saveSession()
}

async function consumeSSE(body: ReadableStream<Uint8Array>, assistant: ChatMsg) {
  const reader = body.getReader()
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
      if (!line.startsWith('data:')) continue
      const payload = line.slice(5).trim()
      if (!payload || payload === '[DONE]') continue
      try {
        const evt = JSON.parse(payload)
        const delta = evt?.choices?.[0]?.delta
        if (delta?.content) {
          assistant.content += delta.content
          scrollChat()
        }
        const tc = delta?.tool_calls?.[0]
        if (tc?.function?.name) {
          assistant.tools = assistant.tools ?? []
          assistant.tools.push({ name: tc.function.name })
          status.value = `调用工具 ${tc.function.name}…`
        }
        if (tc?.tool_result) {
          const t = assistant.tools?.find((x) => x.name === tc.tool_result.name && x.result === undefined)
          if (t) {
            t.result = tc.tool_result.content
            t.isError = tc.tool_result.is_error
          }
          status.value = '正在分析…'
        }
      } catch {
        /* 忽略非 chunk 事件 */
      }
    }
  }
}

// 自动落库：首轮 POST 创建，后续轮 PUT 更新；失败不阻断对话
async function saveSession() {
  const lastAssistant = [...messages.value].reverse().find((m) => m.role === 'assistant' && m.content)
  const payload = {
    alert_id: sessionAlertId.value || undefined,
    cluster: currentAlert.value?.cluster,
    title: sessionAlertId.value
      ? `排查：${currentAlert.value?.title ?? sessionAlertId.value}`
      : (messages.value[0]?.content ?? '自由提问').slice(0, 40),
    messages: JSON.stringify(
      messages.value.map((m) => ({
        role: m.role,
        content: m.content,
        tools: m.tools?.map((t) => ({ name: t.name })),
      })),
    ),
    conclusion: lastAssistant?.content.slice(0, 2000),
  }
  try {
    if (!savedInvId.value) {
      const inv = await investigationApi.save(payload)
      savedInvId.value = inv.id
    } else {
      await investigationApi.update(savedInvId.value, payload)
    }
  } catch {
    /* 落库失败不影响排查 */
  }
}

async function openHistory() {
  historyVisible.value = true
  historyLoading.value = true
  try {
    // 选中告警时按告警过滤；否则列出全部会话（含自由提问）
    const resp = await investigationApi.list(alertId.value || undefined)
    historyList.value = resp.items ?? []
  } catch {
    historyList.value = []
  } finally {
    historyLoading.value = false
  }
}

async function viewHistory(id: string) {
  try {
    const inv = await investigationApi.get(id)
    historyView.value = { title: inv.title, messages: JSON.parse(inv.messages || '[]') }
    historyViewVisible.value = true
  } catch {
    ElMessage.error('加载历史排查失败')
  }
}

function authHeaders(): Record<string, string> {
  const token = localStorage.getItem('opsguard_token')
  return token ? { Authorization: `Bearer ${token}` } : {}
}

onMounted(async () => {
  // 探测网关是否启用（配置可热重载），未启用时展示引导横幅
  configLoading.value = true
  try {
    const cfg = await ainexusApi.config()
    gatewayActive.value = cfg.active
  } catch {
    gatewayActive.value = false
  } finally {
    configLoading.value = false
  }
  await loadAlerts()
  // 从告警页「排查」跳转进来时预选该告警
  const q = route.query.alert
  if (typeof q === 'string' && q && alerts.value.some((a) => a.id === q)) {
    alertId.value = q
  }
})
onBeforeUnmount(() => abort?.abort())
</script>

<style scoped>
.card-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
}
.mb {
  margin-bottom: 14px;
}
.toolbar {
  display: flex;
  align-items: center;
  gap: 10px;
  margin-bottom: 14px;
  flex-wrap: wrap;
}
.chat-box {
  max-height: 52vh;
  overflow: auto;
  border: 1px solid var(--el-border-color-lighter);
  border-radius: 6px;
  padding: 12px;
  margin-bottom: 12px;
  display: flex;
  flex-direction: column;
  gap: 12px;
}
.chat-box.readonly {
  max-height: 60vh;
}
.msg {
  max-width: 92%;
}
.msg.user {
  align-self: flex-end;
  text-align: right;
}
.msg.assistant {
  align-self: flex-start;
  width: 92%;
}
.msg-role {
  font-size: 12px;
  color: var(--el-text-color-secondary);
  margin-bottom: 4px;
}
.msg-content {
  border-radius: 8px;
  padding: 8px 12px;
  font-size: 13px;
  line-height: 1.65;
  white-space: pre-wrap;
  word-break: break-word;
  text-align: left;
}
.msg.user .msg-content {
  background: var(--el-color-primary-dark-2);
  color: #fff;
  display: inline-block;
}
.msg.assistant .msg-content {
  background: var(--og-bg-code);
  color: var(--og-text-code);
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
}
.tool-list {
  margin-bottom: 6px;
}
.tool-chip {
  display: inline-block;
  font-size: 12px;
  padding: 1px 8px;
  border-radius: 10px;
  background: var(--el-color-info-light-7);
  color: var(--el-text-color-regular);
  margin-right: 6px;
}
.tool-chip.err {
  background: var(--el-color-danger-light-8);
  color: var(--el-color-danger);
}
.tool-result {
  max-height: 200px;
  overflow: auto;
  font-size: 12px;
  white-space: pre-wrap;
  word-break: break-all;
  margin: 4px 0;
}
.input-bar {
  display: flex;
  gap: 10px;
  align-items: flex-end;
}
.status {
  margin-top: 8px;
  color: var(--el-text-color-secondary);
  font-size: 12px;
}
</style>
