<template>
  <div v-if="!info?.enabled">
    <el-alert type="info" :closable="false" title="管理端 MCP Server 未启用"
      description="在 server 的 config.yaml 中配置 mcp.enabled: true 与 mcp.tokens（至少一个命名 token）后重启，即可让外部 AI 助手（ZCode / Claude / Cursor…）经 /mcp 用自然语言操作平台。" />
  </div>

  <template v-else>
    <!-- 接入信息 -->
    <el-alert type="success" :closable="false" class="mb"
      title="管理端 MCP Server 已启用（外部 AI 助手经 /mcp 用自然语言操作平台）" />

    <el-descriptions :column="1" border size="small" class="mb">
      <el-descriptions-item label="端点">
        <span class="mono">{{ endpointUrl }}</span>
        <el-button link type="primary" @click="copy(endpointUrl)">复制</el-button>
      </el-descriptions-item>
      <el-descriptions-item label="接入模板（ZCode / Claude / Cursor）">
        <span class="mono small">{{ clientConfig }}</span>
        <el-button link type="primary" @click="copy(clientConfig)">复制</el-button>
        <div class="muted small">把 secret 换成下方任一 token 的密钥；scope=read 的 token 只能查询与排查，不能变更。</div>
      </el-descriptions-item>
    </el-descriptions>

    <!-- Token 管理 -->
    <div class="section-title">Token</div>
    <el-table :data="tokens" empty-text="暂无 token" v-loading="loadingTokens" size="small">
      <el-table-column prop="name" label="名称" min-width="120" />
      <el-table-column label="Scope" width="90">
        <template #default="{ row }">
          <el-tag size="small" :type="row.scope === 'write' ? 'danger' : 'info'">{{ row.scope }}</el-tag>
        </template>
      </el-table-column>
      <el-table-column label="来源" width="90">
        <template #default="{ row }">
          <el-tag size="small" :type="row.static ? 'warning' : 'success'" effect="plain">
            {{ row.static ? 'config' : '页面' }}
          </el-tag>
        </template>
      </el-table-column>
      <el-table-column label="启用" width="80">
        <template #default="{ row }">
          <el-switch :model-value="row.enabled" @change="(v: any) => toggleEnabled(row, !!v)" />
        </template>
      </el-table-column>
      <el-table-column label="最近使用" width="170">
        <template #default="{ row }">{{ fmtTime(row.last_used) }}</template>
      </el-table-column>
      <el-table-column label="创建时间" width="170">
        <template #default="{ row }">{{ fmtTime(row.created_at) }}</template>
      </el-table-column>
      <el-table-column label="操作" width="90" fixed="right">
        <template #default="{ row }">
          <el-button v-if="!row.static" link type="danger" @click="removeToken(row)">删除</el-button>
          <span v-else class="muted small">改 config</span>
        </template>
      </el-table-column>
    </el-table>
    <el-button class="mt" type="primary" :icon="Plus" @click="createVisible = true">新建 Token</el-button>

    <!-- 调用情况 -->
    <div class="section-title">调用情况（近 {{ usageDays }} 天）</div>
    <el-table :data="usage.rows" empty-text="暂无调用" size="small" max-height="260">
      <el-table-column prop="actor" label="Token" min-width="120" />
      <el-table-column prop="tool" label="工具" min-width="180" />
      <el-table-column prop="calls" label="调用次数" width="100" sortable />
    </el-table>

    <div class="section-title">最近写操作审计</div>
    <el-table :data="audit" empty-text="暂无记录（只读调用不落审计）" size="small" v-loading="loadingAudit" max-height="300">
      <el-table-column label="时间" width="170">
        <template #default="{ row }">{{ fmtTime(row.ts) }}</template>
      </el-table-column>
      <el-table-column prop="actor" label="Token" min-width="110" />
      <el-table-column prop="tool" label="工具" min-width="150" />
      <el-table-column prop="args" label="参数摘要" min-width="220" show-overflow-tooltip />
      <el-table-column label="结果" width="80">
        <template #default="{ row }">
          <el-tag size="small" :type="row.ok ? 'success' : 'danger'">{{ row.ok ? '成功' : '失败' }}</el-tag>
        </template>
      </el-table-column>
      <el-table-column prop="error" label="错误" min-width="160" show-overflow-tooltip />
      <el-table-column label="耗时" width="90">
        <template #default="{ row }">{{ row.cost_ms }}ms</template>
      </el-table-column>
    </el-table>

    <!-- 新建 token -->
    <el-dialog v-model="createVisible" title="新建 MCP Token" width="440px">
      <el-form label-width="80px">
        <el-form-item label="名称" required>
          <el-input v-model="createForm.name" placeholder="小写字母开头，如 zcode / patrol-bot" />
        </el-form-item>
        <el-form-item label="权限">
          <el-select v-model="createForm.scope" style="width: 100%">
            <el-option label="read（只读：查询/观测/排查）" value="read" />
            <el-option label="write（含全部写操作/部署/构建）" value="write" />
            <el-option label="exec（含命令执行，最高危；还需服务端打开 exec_enabled）" value="exec" />
          </el-select>
          <div class="muted small">破坏性操作（删集群/删服务/缩容到 0 等）还需助手上传 confirm=true，双重防护。</div>
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="createVisible = false">取消</el-button>
        <el-button type="primary" :loading="creating" @click="createToken">创建</el-button>
      </template>
    </el-dialog>

    <!-- secret 一次性展示 -->
    <el-dialog v-model="secretVisible" title="Token Secret（仅此一次可见）" width="520px">
      <el-alert type="warning" :closable="false" title="请立即复制并妥善保管，关闭后无法再次查看。" class="mb" />
      <el-input :model-value="shownSecret" readonly>
        <template #append>
          <el-button @click="copy(shownSecret)">复制</el-button>
        </template>
      </el-input>
      <template #footer>
        <el-button type="primary" @click="secretVisible = false">我已保存</el-button>
      </template>
    </el-dialog>
  </template>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Plus } from '@element-plus/icons-vue'
import { mcpApi } from '@/api'
import type { MCPAuditItem, MCPTokenView, MCPUsageSummary } from '@/types'

const info = ref<{ enabled: boolean; tokens?: string[] } | null>(null)
const tokens = ref<MCPTokenView[]>([])
const audit = ref<MCPAuditItem[]>([])
const usage = ref<MCPUsageSummary>({ days: 7, rows: [] })
const usageDays = 7

const loadingTokens = ref(false)
const loadingAudit = ref(false)
const createVisible = ref(false)
const creating = ref(false)
const secretVisible = ref(false)
const shownSecret = ref('')
const createForm = reactive({ name: '', scope: 'read' })

const endpointUrl = computed(() => `${window.location.origin}/mcp`)
const clientConfig = computed(
  () =>
    `{"mcpServers":{"opsguard":{"type":"streamableHttp","url":"${endpointUrl.value}","headers":{"Authorization":"Bearer <secret>"}}}}`,
)

function fmtTime(t?: string) {
  return t ? new Date(t).toLocaleString() : '—'
}

function copy(text: string) {
  navigator.clipboard?.writeText(text).then(
    () => ElMessage.success('已复制'),
    () => ElMessage.warning('复制失败，请手动选中复制'),
  )
}

async function loadTokens() {
  loadingTokens.value = true
  try {
    tokens.value = (await mcpApi.tokens()).items ?? []
  } catch {
    tokens.value = []
  } finally {
    loadingTokens.value = false
  }
}

async function loadAudit() {
  loadingAudit.value = true
  try {
    audit.value = (await mcpApi.audit({ limit: 30 })).items ?? []
  } catch {
    audit.value = []
  } finally {
    loadingAudit.value = false
  }
}

async function createToken() {
  if (!createForm.name.trim()) {
    ElMessage.warning('请填写名称')
    return
  }
  creating.value = true
  try {
    const res = await mcpApi.createToken({ name: createForm.name.trim(), scope: createForm.scope })
    createVisible.value = false
    shownSecret.value = res.secret
    secretVisible.value = true
    ElMessage.success('已创建')
    await loadTokens()
  } catch (e: any) {
    ElMessage.error(e?.response?.data?.message ?? '创建失败')
  } finally {
    creating.value = false
  }
}

async function toggleEnabled(row: MCPTokenView, enabled: boolean) {
  try {
    await mcpApi.setTokenEnabled(row.name, enabled)
    row.enabled = enabled
    ElMessage.success(enabled ? `已启用 ${row.name}` : `已停用 ${row.name}（立即生效）`)
  } catch (e: any) {
    ElMessage.error(e?.response?.data?.message ?? '操作失败')
  }
}

async function removeToken(row: MCPTokenView) {
  try {
    await ElMessageBox.confirm(
      `确认删除 token「${row.name}」？使用它的助手将立即失去访问能力。`,
      '删除 Token',
      { type: 'warning' },
    )
  } catch {
    return
  }
  try {
    await mcpApi.deleteToken(row.name)
    tokens.value = tokens.value.filter((x) => x.name !== row.name)
    ElMessage.success('已删除')
  } catch (e: any) {
    ElMessage.error(e?.response?.data?.message ?? '删除失败')
  }
}

onMounted(async () => {
  try {
    info.value = await mcpApi.info()
  } catch {
    info.value = { enabled: false }
    return
  }
  if (info.value?.enabled) {
    await Promise.all([loadTokens(), loadAudit(), mcpApi.usage(usageDays).then((u) => (usage.value = u)).catch(() => {})])
  }
})
</script>

<style scoped>
.mono {
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
  font-size: 0.85em;
  word-break: break-all;
}
.small {
  font-size: 12px;
}
.muted {
  color: var(--el-text-color-secondary);
}
.mb {
  margin-bottom: 12px;
}
.mt {
  margin-top: 12px;
}
.section-title {
  font-weight: 600;
  margin: 18px 0 8px;
}
</style>
