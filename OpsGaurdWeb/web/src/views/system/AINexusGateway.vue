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

    <!-- 默认模型（从模型池选择，模型池在「MLOps → 模型接入」管理） -->
    <el-divider content-position="left">默认模型</el-divider>
    <el-form label-width="90px" class="default-model-form">
      <el-form-item label="默认模型">
        <el-select v-model="form.default_model" clearable filterable placeholder="默认（模型池首个可用）" style="width: 100%">
          <el-option v-for="m in modelOptions" :key="m.name" :label="modelLabel(m)" :value="m.name" />
        </el-select>
        <span class="muted ml">异常排查直接使用该模型（排查页无需再选）；模型池见「MLOps → 模型接入」</span>
      </el-form-item>
    </el-form>

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

    <!-- 集群 MCP（自动连接，只读） -->
    <el-divider content-position="left">集群 MCP（自动）</el-divider>
    <el-alert type="info" :closable="false" class="mb"
      title="各集群 Manager Worker 的 MCP 已自动连接"
      description="网关加载/热重载时自动连接每个接入集群（有 mcp_url）的 /mcp，供 AI 排查与巡检采证，无需在此配置。" />
    <div v-if="autoMcps.length" class="provider-box">
      <div v-for="(m, i) in autoMcps" :key="i" class="auto-mcp-row">
        <el-tag size="small" type="success" effect="plain">集群 manager</el-tag>
        <span class="mono auto-mcp-name">{{ m.name.replace(/^cluster:/, '') }}</span>
        <span class="mono og-dim">{{ m.url }}</span>
      </div>
    </div>
    <p v-else class="og-dim mb">尚未接入带 mcp_url 的集群</p>

    <!-- 自定义 MCP Servers -->
    <el-divider content-position="left">自定义 MCP Servers</el-divider>
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
              <el-input v-model="row.value" placeholder="值（留空 = 保持）" type="password" show-password class="mono kv-val" />
              <el-button link type="danger" :icon="Delete" @click="m.headers_rows.splice(ri, 1)" />
            </div>
            <el-button size="small" :icon="Plus" @click="m.headers_rows.push({ key: '', value: '' })">添加 Header</el-button>
          </div>
        </el-form-item>
        <el-form-item label="Env">
          <div class="kv-wrap">
            <div v-for="(row, ri) in m.env_rows" :key="ri" class="kv-row">
              <el-input v-model="row.key" placeholder="变量名" class="mono kv-key" />
              <el-input v-model="row.value" placeholder="值（留空 = 保持）" class="mono kv-val" />
              <el-button link type="danger" :icon="Delete" @click="m.env_rows.splice(ri, 1)" />
            </div>
            <el-button size="small" :icon="Plus" @click="m.env_rows.push({ key: '', value: '' })">添加 Env</el-button>
          </div>
        </el-form-item>
      </el-form>
    </div>
    <el-button size="small" :icon="Plus" @click="addMCPServer">添加 MCP Server</el-button>

    <!-- 保存 -->
    <div class="save-bar">
      <el-button type="primary" :loading="saving" :disabled="!isAdmin" @click="save">保存并热重载</el-button>
      <el-button :loading="loading" @click="load">重置</el-button>
      <span v-if="!isAdmin" class="muted ml">只读（写操作需管理员）</span>
    </div>
  </div>
</template>

<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { Delete, Plus } from '@element-plus/icons-vue'
import { ainexusApi } from '@/api'
import { isAdmin, loadAdminFlag } from '@/composables/admin'
import type { AINexusMCPServer, AINexusModelInfo } from '@/types'

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

const loading = ref(false)
const saving = ref(false)
const active = ref(false)
// 集群 manager 的自动 MCP（cluster:true，服务端自动连接，页面只读）
const autoMcps = ref<AINexusMCPServer[]>([])
// 模型池（来自 GET config 的 providers，选项数据源）
const modelOptions = ref<AINexusModelInfo[]>([])

const form = reactive<{
  enabled: boolean
  default_model: string
  tools: {
    command: { enabled: boolean; allowed_commands_str: string; timeout: string; work_dir: string }
    http_request: { enabled: boolean; timeout: string }
    file_read: { enabled: boolean; max_size: number }
  }
  agent: { max_tool_rounds: number; parallel_tool_calls: boolean; max_context_tokens: number; keep_tool_rounds: number }
  mcp_servers: MCPServerForm[]
}>({
  enabled: false,
  default_model: '',
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
    form.default_model = cfg.default_model ?? ''
    modelOptions.value = (cfg.providers ?? []).flatMap((p) =>
      (p.models ?? []).map((m) => ({ name: m.name, provider: p.name, type: p.type })),
    )
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
    // 集群 manager 的自动 MCP 拆出（只读展示），其余为用户自定义（可编辑）
    autoMcps.value = (cfg.mcp_servers ?? []).filter((m) => m.cluster)
    form.mcp_servers = (cfg.mcp_servers ?? [])
      .filter((m) => !m.cluster)
      .map((m) => ({
        name: m.name,
        transport: m.transport,
        url: m.url,
        command: m.command,
        args: m.args,
        args_str: (m.args ?? []).join(' '),
        headers_rows: (m.headers_keys ?? []).map((k) => ({ key: k, value: '' })),
        env_rows: (m.env_keys ?? []).map((k) => ({ key: k, value: '' })),
      }))
  } catch {
    // 错误已由 http.ts 提示
  } finally {
    loading.value = false
  }
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

function splitArgs(s: string): string[] {
  return s.split(/\s+/).filter(Boolean)
}

function modelLabel(m: AINexusModelInfo) {
  return `${m.name}（${m.provider}）`
}

async function save() {
  saving.value = true
  try {
    // 只改网关设置（启用/默认模型/工具/Agent/MCP），模型池（providers）保留
    const cfg = await ainexusApi.config()
    cfg.enabled = form.enabled
    cfg.default_model = form.default_model || undefined
    cfg.tools = {
      command: {
        enabled: form.tools.command.enabled,
        allowed_commands: form.tools.command.allowed_commands_str.split(',').map((s) => s.trim()).filter(Boolean),
        timeout: form.tools.command.timeout || '30s',
        work_dir: form.tools.command.work_dir,
      },
      http_request: { enabled: form.tools.http_request.enabled, timeout: form.tools.http_request.timeout || '15s' },
      file_read: { enabled: form.tools.file_read.enabled, max_size: form.tools.file_read.max_size },
    }
    cfg.agent = { ...form.agent }
    cfg.mcp_servers = form.mcp_servers.map((m) => ({
      name: m.name,
      transport: m.transport,
      url: m.transport === 'stdio' ? '' : m.url,
      command: m.transport === 'stdio' ? m.command : '',
      args: splitArgs(m.args_str),
      headers: kvToMap(m.headers_rows),
      env: kvToMap(m.env_rows),
    }))
    const resp = await ainexusApi.updateConfig(cfg)
    active.value = resp.active
    ElMessage.success('已保存并热重载，无需重启')
    await load()
  } catch {
    // 错误已由 http.ts 提示
  } finally {
    saving.value = false
  }
}

onMounted(() => {
  void loadAdminFlag()
  load()
})
</script>

<style scoped>
.status-bar {
  display: flex;
  align-items: center;
  gap: 10px;
  margin-bottom: 4px;
}
.default-model-form {
  max-width: 480px;
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
.provider-form {
  max-width: 720px;
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
.agent-form {
  max-width: 480px;
}
.auto-mcp-row {
  display: flex;
  align-items: center;
  gap: 10px;
  padding-bottom: 8px;
}
.auto-mcp-name {
  font-weight: 600;
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
