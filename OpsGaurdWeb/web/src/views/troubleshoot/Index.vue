<template>
  <el-card shadow="never">
    <template #header>
      <div class="card-header">
        <span>异常排查（AiNexus）</span>
        <el-tag size="small" type="success">已整合 · 进程内</el-tag>
      </div>
    </template>

    <div class="toolbar">
      <el-select v-model="alertId" placeholder="选择要排查的告警" filterable style="width: 340px">
        <el-option
          v-for="a in alerts"
          :key="a.id"
          :label="`[${a.cluster}/${a.service}] ${a.title} (${a.status})`"
          :value="a.id"
        />
      </el-select>
      <el-select v-model="model" placeholder="模型（默认首个可用）" clearable style="width: 200px">
        <el-option v-for="m in models" :key="m.name" :label="m.name" :value="m.name" />
      </el-select>
      <el-switch v-model="useMCP" active-text="启用 MCP 采证" />
      <el-button type="primary" :icon="Search" :loading="running" :disabled="!alertId" @click="investigate">
        开始排查
      </el-button>
    </div>

    <!-- 排查过程/结论 -->
    <div v-if="running || result" class="result-box">
      <div class="result-title">排查结果</div>
      <el-collapse>
        <el-collapse-item title="排查详情" name="detail">
          <div ref="streamBoxRef" class="stream-box" v-html="renderedStream" />
        </el-collapse-item>
      </el-collapse>
      <div v-if="status" class="status">{{ status }}</div>
    </div>
    <el-empty v-else description="选择一条告警，AI 将基于事件/日志/审计证据进行根因排查" />
  </el-card>
</template>

<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { Search } from '@element-plus/icons-vue'
import { ainexusApi, alertApi } from '@/api'
import type { AINexusModelInfo, Alert } from '@/types'

const alerts = ref<Alert[]>([])
const alertId = ref('')
const models = ref<AINexusModelInfo[]>([])
const model = ref('')
const useMCP = ref(true)

const running = ref(false)
const result = ref('')
const status = ref('')
const streamBoxRef = ref<HTMLElement>()

let abort: AbortController | null = null

// 展示为终端风格文本（转义 HTML 防注入）
const renderedStream = computed(() => {
  return (result.value || '')
    .split('\n')
    .map((l) => `<div class="line">${escapeHtml(l) || '&nbsp;'}</div>`)
    .join('')
})

function escapeHtml(s: string) {
  return s
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
}

function scrollStream() {
  void nextTick(() => {
    const el = streamBoxRef.value
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

async function loadModels() {
  try {
    const resp = await ainexusApi.models()
    models.value = resp.models ?? []
  } catch {
    models.value = []
  }
}

async function investigate() {
  if (!alertId.value) {
    ElMessage.warning('请选择告警')
    return
  }
  abort?.abort()
  abort = new AbortController()
  running.value = true
  result.value = ''
  status.value = '正在采集证据并分析…'

  try {
    const resp = await fetch(ainexusApi.investigateUrl(), {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', ...authHeaders() },
      body: JSON.stringify({ alert_id: alertId.value, model: model.value || undefined, use_mcp: useMCP.value }),
      signal: abort.signal,
    })
    if (!resp.ok || !resp.body) {
      const body = await resp.text().catch(() => '')
      ElMessage.error(`排查失败（${resp.status}）：${body}`)
      status.value = '排查失败'
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
            const evt = JSON.parse(payload)
            const delta = evt?.choices?.[0]?.delta
            if (delta?.content) {
              result.value += delta.content
              scrollStream()
            }
            if (delta?.tool_calls?.length) {
              const tc = delta.tool_calls[0]
              if (tc.function?.name) result.value += `\n⚙ 调用工具：${tc.function.name}\n`
            }
            if (evt?.choices?.[0]?.finish_reason) {
              status.value = '排查完成'
            }
          } catch {
            /* 忽略非 chunk 事件 */
          }
        }
      }
    }
    status.value = '排查完成'
  } catch (e) {
    if ((e as Error).name !== 'AbortError') {
      ElMessage.error(`排查中断：${(e as Error).message}`)
      status.value = '排查中断'
    }
  } finally {
    running.value = false
  }
}

function authHeaders(): Record<string, string> {
  const token = localStorage.getItem('opsguard_token')
  return token ? { Authorization: `Bearer ${token}` } : {}
}

onMounted(async () => {
  await Promise.all([loadAlerts(), loadModels()])
})
onBeforeUnmount(() => abort?.abort())
</script>

<style scoped>
.card-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
}
.toolbar {
  display: flex;
  align-items: center;
  gap: 10px;
  margin-bottom: 16px;
  flex-wrap: wrap;
}
.result-box {
  border: 1px solid var(--el-border-color-lighter);
  border-radius: 6px;
  padding: 8px 12px;
}
.result-title {
  font-weight: 600;
  margin-bottom: 6px;
}
.stream-box {
  height: 380px;
  overflow: auto;
  background: #0d1117;
  color: #e6edf3;
  border-radius: 6px;
  padding: 10px;
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  font-size: 13px;
  line-height: 1.6;
  white-space: pre-wrap;
  word-break: break-all;
}
.status {
  margin-top: 8px;
  color: var(--el-text-color-secondary);
  font-size: 12px;
}
</style>
