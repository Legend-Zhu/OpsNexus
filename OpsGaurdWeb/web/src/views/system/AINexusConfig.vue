<template>
  <div>
    <!-- 状态行 -->
    <div class="status-bar">
      <el-switch v-model="form.enabled" active-text="启用网关" />
      <el-tag size="small" :type="active ? 'success' : 'info'" effect="plain">
        {{ active ? '网关已加载' : '未启用' }}
      </el-tag>
      <span class="muted ml">保存后立即热重载，无需重启进程</span>
    </div>

    <!-- 模型提供商 -->
    <el-divider content-position="left">模型提供商</el-divider>
    <div v-for="(p, i) in form.providers" :key="i" class="provider-box">
      <div class="provider-head">
        <span class="provider-title">Provider {{ i + 1 }}</span>
        <span>
          <el-button link type="primary" :loading="testing === i" @click="testProvider(i)">测试连接</el-button>
          <el-button link type="danger" @click="form.providers.splice(i, 1)">移除</el-button>
        </span>
      </div>
      <el-form label-width="90px" class="provider-form">
        <el-form-item label="名称">
          <el-input v-model="p.name" placeholder="如 deepseek" />
        </el-form-item>
        <el-form-item label="类型">
          <el-select v-model="p.type" style="width: 220px">
            <el-option label="OpenAI 兼容" value="openai_compatible" />
            <el-option label="Anthropic 兼容" value="anthropic_compatible" />
          </el-select>
        </el-form-item>
        <el-form-item label="Base URL">
          <el-input v-model="p.base_url" placeholder="如 https://api.deepseek.com/v1" class="mono" />
        </el-form-item>
        <el-form-item label="API Key">
          <el-input
            v-model="p.api_key"
            type="password"
            show-password
            :placeholder="p.api_key_set ? '已设置（留空 = 保持）' : '必填'"
            class="mono"
          />
        </el-form-item>
        <el-form-item label="模型">
          <div class="models-wrap">
            <div v-for="(m, mi) in p.models" :key="mi" class="model-row">
              <el-input v-model="m.name" placeholder="模型名，如 deepseek-chat" class="mono model-name" />
              <el-input v-model="m.display_name" placeholder="显示名（可选）" class="model-display" />
              <el-input-number v-model="m.max_tokens" :min="0" placeholder="max_tokens" controls-position="right" />
              <el-input-number
                v-model="m.temperature"
                :min="0"
                :max="2"
                :step="0.1"
                controls-position="right"
              />
              <el-button link type="danger" :icon="Delete" @click="p.models.splice(mi, 1)" />
            </div>
            <el-button size="small" :icon="Plus" @click="p.models.push({ name: '' })">添加模型</el-button>
          </div>
        </el-form-item>
      </el-form>
    </div>
    <el-button class="mb" size="small" :icon="Plus" @click="addProvider">添加 Provider</el-button>

    <!-- 内置工具 -->
    <el-divider content-position="left">内置工具</el-divider>
    <div class="tools-grid">
      <div class="tool-box">
        <el-switch v-model="form.tools.command.enabled" active-text="命令执行" />
        <div class="tool-fields">
          <el-input v-model="form.tools.command.allowed_commands_str" placeholder="允许的命令（逗号分隔，空 = 全部禁止）" />
          <el-input v-model="form.tools.command.timeout" placeholder="超时，如 30s" class="mono" />
          <el-input v-model="form.tools.command.work_dir" placeholder="工作目录，如 /tmp" class="mono" />
        </div>
      </div>
      <div class="tool-box">
        <el-switch v-model="form.tools.http_request.enabled" active-text="HTTP 请求" />
        <div class="tool-fields">
          <el-input v-model="form.tools.http_request.timeout" placeholder="超时，如 15s" class="mono" />
        </div>
      </div>
      <div class="tool-box">
        <el-switch v-model="form.tools.file_read.enabled" active-text="文件读取" />
        <div class="tool-fields">
          <el-input-number v-model="form.tools.file_read.max_size" :min="0" placeholder="最大字节" controls-position="right" />
        </div>
      </div>
    </div>

    <!-- Agent 参数 -->
    <el-divider content-position="left">Agent 参数</el-divider>
    <div class="agent-grid">
      <el-form label-width="150px" class="agent-form">
        <el-form-item label="工具轮次上限">
          <el-input-number v-model="form.agent.max_tool_rounds" :min="1" controls-position="right" />
        </el-form-item>
        <el-form-item label="并行工具调用">
          <el-switch v-model="form.agent.parallel_tool_calls" />
        </el-form-item>
        <el-form-item label="上下文预算 (token)">
          <el-input-number v-model="form.agent.max_context_tokens" :min="0" :step="10000" controls-position="right" />
        </el-form-item>
        <el-form-item label="保留工具轮次">
          <el-input-number v-model="form.agent.keep_tool_rounds" :min="0" controls-position="right" />
        </el-form-item>
      </el-form>
    </div>

    <!-- MCP Servers -->
    <el-divider content-position="left">MCP Servers（集群 Worker 采证）</el-divider>
    <div v-for="(m, i) in form.mcp_servers" :key="i" class="provider-box">
      <div class="provider-head">
        <span class="provider-title">MCP {{ i + 1 }}：{{ m.name || '未命名' }}</span>
        <el-button link type="danger" @click="form.mcp_servers.splice(i, 1)">移除</el-button>
      </div>
      <el-form label-width="90px" class="provider-form">
        <el-form-item label="名称">
          <el-input v-model="m.name" placeholder="如 dev-cluster" class="mono" />
        </el-form-item>
        <el-form-item label="传输">
          <el-select v-model="m.transport" style="width: 200px">
            <el-option label="streamable-http" value="streamable-http" />
            <el-option label="sse" value="sse" />
            <el-option label="stdio" value="stdio" />
          </el-select>
        </el-form-item>
        <el-form-item v-if="m.transport !== 'stdio'" label="URL">
          <el-input v-model="m.url" placeholder="如 http://worker:8080/mcp" class="mono" />
        </el-form-item>
        <el-form-item v-else label="命令">
          <el-input v-model="m.command" placeholder="可执行文件" class="mono" />
        </el-form-item>
        <el-form-item v-if="m.transport === 'stdio'" label="参数">
          <el-input v-model="m.args_str" placeholder="空格分隔的参数" class="mono" />
        </el-form-item>
        <el-form-item label="Headers">
          <div class="kv-wrap">
            <div v-for="(row, ri) in m.headers_rows" :key="ri" class="kv-row">
              <el-input v-model="row.key" placeholder="头名，如 Authorization" class="mono kv-key" />
              <el-input
                v-model="row.value"
                placeholder="值（留空 = 保持）"
                type="password"
                show-password
                class="mono kv-val"
              />
              <el-button link type="danger" :icon="Delete" @click="m.headers_rows.splice(ri, 1)" />
            </div>
            <el-button size="small" :icon="Plus" @click="m.headers_rows.push({ key: '', value: '' })">
              添加 Header
            </el-button>
          </div>
        </el-form-item>
        <el-form-item label="Env">
          <div class="kv-wrap">
            <div v-for="(row, ri) in m.env_rows" :key="ri" class="kv-row">
              <el-input v-model="row.key" placeholder="变量名" class="mono kv-key" />
              <el-input v-model="row.value" placeholder="值（留空 = 保持）" class="mono kv-val" />
              <el-button link type="danger" :icon="Delete" @click="m.env_rows.splice(ri, 1)" />
            </div>
            <el-button size="small" :icon="Plus" @click="m.env_rows.push({ key: '', value: '' })">
              添加 Env
            </el-button>
          </div>
        </el-form-item>
      </el-form>
    </div>
    <el-button size="small" :icon="Plus" @click="addMCPServer">添加 MCP Server</el-button>

    <!-- 保存 -->
    <div class="save-bar">
      <el-button type="primary" :loading="saving" @click="save">保存并热重载</el-button>
      <el-button :loading="loading" @click="load">重置</el-button>
    </div>
  </div>
</template>

<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { Delete, Plus } from '@element-plus/icons-vue'
import { ainexusApi } from '@/api'
import type { AINexusConfig, AINexusMCPServer, AINexusModelSpec, AINexusProvider } from '@/types'

interface KVRow {
  key: string
  value: string
}

/** 表单用的 MCP Server（headers/env 展开为键值行） */
interface MCPServerForm extends Omit<AINexusMCPServer, 'headers' | 'env'> {
  args_str: string
  headers_rows: KVRow[]
  env_rows: KVRow[]
}

/** 表单模型（agent 字段必填，方便 el-input-number 绑定） */
interface AINexusForm {
  enabled: boolean
  providers: AINexusProvider[]
  tools: {
    command: { enabled: boolean; allowed_commands_str: string; timeout: string; work_dir: string }
    http_request: { enabled: boolean; timeout: string }
    file_read: { enabled: boolean; max_size: number }
  }
  agent: { max_tool_rounds: number; parallel_tool_calls: boolean; max_context_tokens: number; keep_tool_rounds: number }
  mcp_servers: MCPServerForm[]
}

const loading = ref(false)
const saving = ref(false)
const testing = ref(-1)
const active = ref(false)

const form = reactive<AINexusForm>({
  enabled: false,
  providers: [] as AINexusProvider[],
  tools: {
    command: { enabled: false, allowed_commands_str: '', timeout: '30s', work_dir: '/tmp' },
    http_request: { enabled: false, timeout: '15s' },
    file_read: { enabled: false, max_size: 1048576 },
  },
  agent: { max_tool_rounds: 200, parallel_tool_calls: true, max_context_tokens: 800000, keep_tool_rounds: 5 },
  mcp_servers: [] as MCPServerForm[],
})

async function load() {
  loading.value = true
  try {
    const cfg = await ainexusApi.config()
    active.value = cfg.active
    form.enabled = cfg.enabled
    form.providers = cfg.providers.map((p) => ({
      ...p,
      api_key: '', // 仅标记已设置；编辑时留空 = 保持
      models: p.models.map((m) => ({ ...m })),
    }))
    form.tools.command = {
      enabled: cfg.tools?.command?.enabled ?? false,
      allowed_commands_str: (cfg.tools?.command?.allowed_commands ?? []).join(','),
      timeout: cfg.tools?.command?.timeout || '30s',
      work_dir: cfg.tools?.command?.work_dir || '/tmp',
    }
    form.tools.http_request = { enabled: cfg.tools?.http_request?.enabled ?? false, timeout: cfg.tools?.http_request?.timeout || '15s' }
    form.tools.file_read = { enabled: cfg.tools?.file_read?.enabled ?? false, max_size: cfg.tools?.file_read?.max_size ?? 1048576 }
    form.agent = {
      max_tool_rounds: cfg.agent?.max_tool_rounds ?? 200,
      parallel_tool_calls: cfg.agent?.parallel_tool_calls ?? true,
      max_context_tokens: cfg.agent?.max_context_tokens ?? 800000,
      keep_tool_rounds: cfg.agent?.keep_tool_rounds ?? 5,
    }
    form.mcp_servers = (cfg.mcp_servers ?? []).map((m) => ({
      name: m.name,
      transport: m.transport,
      url: m.url,
      command: m.command,
      args: m.args,
      args_str: (m.args ?? []).join(' '),
      // GET 只返回 key 名（值脱敏）；编辑时留空 = 保持已保存值
      headers_rows: (m.headers_keys ?? []).map((k) => ({ key: k, value: '' })),
      env_rows: (m.env_keys ?? []).map((k) => ({ key: k, value: '' })),
    }))
  } catch {
    // 错误已由 http.ts 提示
  } finally {
    loading.value = false
  }
}

function addProvider() {
  form.providers.push({ name: '', type: 'openai_compatible', base_url: '', api_key: '', models: [{ name: '' }] })
}

function addMCPServer() {
  form.mcp_servers.push({ name: '', transport: 'streamable-http', args_str: '', headers_rows: [], env_rows: [] })
}

/** 键值行 → map（空 key 的行丢弃；空 value 保留 = 沿用旧值） */
function kvToMap(rows: KVRow[]): Record<string, string> {
  const out: Record<string, string> = {}
  for (const r of rows) {
    if (!r.key.trim()) continue
    out[r.key.trim()] = r.value
  }
  return out
}

/** 空格分隔参数 → 数组 */
function splitArgs(s: string): string[] {
  return s.split(/\s+/).filter(Boolean)
}

function toPayload(): Partial<AINexusConfig> {
  return {
    enabled: form.enabled,
    providers: form.providers.map((p) => ({
      name: p.name,
      type: p.type,
      base_url: p.base_url,
      api_key: p.api_key ?? '',
      models: p.models.map((m: AINexusModelSpec) => ({ ...m })),
    })),
    tools: {
      command: {
        enabled: form.tools.command.enabled,
        allowed_commands: form.tools.command.allowed_commands_str.split(',').map((s) => s.trim()).filter(Boolean),
        timeout: form.tools.command.timeout || '30s',
        work_dir: form.tools.command.work_dir,
      },
      http_request: { enabled: form.tools.http_request.enabled, timeout: form.tools.http_request.timeout || '15s' },
      file_read: { enabled: form.tools.file_read.enabled, max_size: form.tools.file_read.max_size },
    },
    agent: { ...form.agent },
    mcp_servers: form.mcp_servers.map((m) => ({
      name: m.name,
      transport: m.transport,
      url: m.transport === 'stdio' ? '' : m.url,
      command: m.transport === 'stdio' ? m.command : '',
      args: splitArgs(m.args_str),
      headers: kvToMap(m.headers_rows),
      env: kvToMap(m.env_rows),
    })),
  }
}

async function save() {
  saving.value = true
  try {
    const cfg = await ainexusApi.updateConfig(toPayload())
    active.value = cfg.active
    ElMessage.success('已保存并热重载，无需重启')
    await load()
  } catch {
    // 错误已由 http.ts 提示
  } finally {
    saving.value = false
  }
}

async function testProvider(i: number) {
  const p = form.providers[i]
  const model = p.models[0]?.name
  if (!p.base_url || !model) {
    ElMessage.warning('请先填写 Base URL 与至少一个模型名')
    return
  }
  if (!p.api_key && !p.api_key_set) {
    ElMessage.warning('请填写 API Key')
    return
  }
  testing.value = i
  try {
    const resp = await ainexusApi.testConfig({
      name: p.name,
      type: p.type,
      base_url: p.base_url,
      api_key: p.api_key || undefined,
      model,
    })
    if (resp.ok) {
      ElMessage.success(`连接成功（${resp.latency_ms}ms，模型 ${resp.model}）`)
    } else {
      ElMessage.error(`连接失败：${resp.error ?? '未知错误'}`)
    }
  } catch {
    // 错误已由 http.ts 提示
  } finally {
    testing.value = -1
  }
}

onMounted(load)
</script>

<style scoped>
.status-bar {
  display: flex;
  align-items: center;
  gap: 10px;
  margin-bottom: 4px;
}
.provider-box {
  border: 1px solid var(--el-border-color-lighter);
  border-radius: 6px;
  padding: 8px 12px 0;
  margin-bottom: 12px;
}
.provider-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: 6px;
}
.provider-title {
  font-weight: 600;
}
.provider-form {
  max-width: 720px;
}
.models-wrap {
  width: 100%;
}
.model-row {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 8px;
  flex-wrap: wrap;
}
.model-name {
  width: 220px;
}
.model-display {
  width: 160px;
}
.mb {
  margin-bottom: 12px;
}
.tools-grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(320px, 1fr));
  gap: 12px;
  margin-bottom: 12px;
}
.tool-box {
  border: 1px solid var(--el-border-color-lighter);
  border-radius: 6px;
  padding: 10px 12px;
}
.tool-fields {
  margin-top: 10px;
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.agent-grid {
  margin-bottom: 12px;
}
.agent-form {
  max-width: 480px;
}
.kv-wrap {
  width: 100%;
}
.kv-row {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 6px;
  width: 100%;
}
.kv-key {
  width: 220px;
}
.kv-val {
  flex: 1;
}
.save-bar {
  margin-top: 16px;
}
.ml {
  margin-left: 8px;
}
.muted {
  color: var(--el-text-color-placeholder);
  font-size: 12px;
}
.mono {
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
}
</style>
