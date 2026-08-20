<template>
  <div class="models">
    <el-alert type="info" :closable="false" class="mb"
      title="模型运营视图：provider/model 本体在本页「模型接入」维护，此处只做启停/绑定/健康"
      description="启停经运行时热重载立即生效（构建校验成功才持久化，失败不影响旧网关）；禁用的模型不再接收请求，默认模型禁用时按配置顺序稳定回退；健康测试会发送一次真实最小请求（计量计入 health 场景）。" />

    <!-- 网关状态 -->
    <el-descriptions :column="4" size="small" border class="mb" v-if="view">
      <el-descriptions-item label="网关">
        <el-tag size="small" :type="view.gateway_active ? 'success' : 'info'">
          {{ view.gateway_active ? '运行中' : view.gateway_enabled ? '未加载' : '未启用' }}
        </el-tag>
      </el-descriptions-item>
      <el-descriptions-item label="配置默认模型">
        <span class="mono">{{ view.default_model || '（首个启用模型）' }}</span>
      </el-descriptions-item>
      <el-descriptions-item label="实际生效默认">
        <span class="mono">{{ view.effective_default || '—' }}</span>
      </el-descriptions-item>
      <el-descriptions-item label="模型数">
        {{ view.items.filter((m) => m.enabled).length }} 启用 / {{ view.items.length }} 配置
      </el-descriptions-item>
    </el-descriptions>

    <!-- 模型池 -->
    <el-table :data="view?.items ?? []" size="small" v-loading="loading" empty-text="未配置模型（在本页「模型接入」tab 添加）">
      <el-table-column label="provider / model" min-width="230">
        <template #default="{ row }">
          <span class="mono">{{ row.provider }} / {{ row.model }}</span>
          <span v-if="row.display_name" class="muted">（{{ row.display_name }}）</span>
        </template>
      </el-table-column>
      <el-table-column label="状态" width="150">
        <template #default="{ row }">
          <el-tag v-if="row.routed" size="small" type="success" effect="plain">路由中</el-tag>
          <el-tag v-else-if="row.enabled" size="small" type="warning" effect="plain">已启用未路由</el-tag>
          <el-tag v-else size="small" type="danger" effect="plain">已禁用</el-tag>
          <el-tag v-if="row.is_default" size="small" type="primary" effect="plain" class="ml4">默认</el-tag>
        </template>
      </el-table-column>
      <el-table-column label="本月调用" width="90" align="right">
        <template #default="{ row }">{{ row.month_calls }}</template>
      </el-table-column>
      <el-table-column label="本月费用(元)" width="110" align="right">
        <template #default="{ row }">
          <span v-if="row.priced">{{ fmtMinor(row.month_cost_minor) }}</span>
          <el-tooltip v-else content="未配单价，只计 token 不计费用（在费用 tab 配置）">
            <span class="muted">未计价</span>
          </el-tooltip>
        </template>
      </el-table-column>
      <el-table-column label="健康" min-width="200">
        <template #default="{ row }">
          <span v-if="healthOf(row)" :class="healthOf(row)!.ok ? 'ok' : 'err'">
            {{ healthOf(row)!.ok ? '正常' : '异常' }} · {{ healthOf(row)!.latency_ms }}ms · {{ fmtTime(healthOf(row)!.tested_at) }}
          </span>
          <span v-else class="muted">未测试</span>
        </template>
      </el-table-column>
      <el-table-column label="操作" width="220" fixed="right">
        <template #default="{ row }">
          <template v-if="isAdmin">
            <el-button v-if="!row.enabled" link type="primary" :loading="toggling === row.model" @click="toggle(row, true)">启用</el-button>
            <el-button v-else link type="danger" :loading="toggling === row.model" @click="toggle(row, false)">禁用</el-button>
            <el-button link type="primary" :loading="testing === row.model" @click="testHealth(row)">健康测试</el-button>
          </template>
          <span v-else class="muted">只读（写操作需管理员）</span>
        </template>
      </el-table-column>
    </el-table>

    <!-- 场景绑定 -->
    <h4 class="panel-title">场景模型绑定（请求未指定模型时生效；绑定不可用时回退默认模型）</h4>
    <el-table :data="bindingRows" size="small" empty-text="无绑定">
      <el-table-column label="场景" width="160">
        <template #default="{ row }">
          <el-tag size="small" effect="plain">{{ row.scenario }}</el-tag>
        </template>
      </el-table-column>
      <el-table-column label="绑定模型" min-width="240">
        <template #default="{ row }">
          <el-select v-model="row.model" size="small" clearable placeholder="未绑定（用默认模型）" style="width: 260px"
            :disabled="!isAdmin" @change="saveBinding(row)">
            <el-option v-for="m in enabledModels" :key="m" :label="m" :value="m" />
          </el-select>
        </template>
      </el-table-column>
      <el-table-column label="更新" min-width="160">
        <template #default="{ row }">{{ row.updated_at ? fmtTime(row.updated_at) : '—' }}</template>
      </el-table-column>
    </el-table>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { mlopsApi } from '@/api'
import { isAdmin, loadAdminFlag } from '@/composables/admin'
import type { MLOpsBinding, MLOpsHealthResult, MLOpsModelItem, MLOpsModelsView } from '@/types'

const SCENARIOS = ['chat', 'investigate', 'native_chat', 'patrol_report']

const view = ref<MLOpsModelsView | null>(null)
const healths = ref<MLOpsHealthResult[]>([])
const loading = ref(false)
const toggling = ref('')
const testing = ref('')

const bindingRows = reactive(
  SCENARIOS.map((scenario) => ({ scenario, model: '', updated_at: '', updated_by: '' })),
)

const enabledModels = computed(() => (view.value?.items ?? []).filter((m) => m.enabled).map((m) => m.model))

function healthOf(row: MLOpsModelItem): MLOpsHealthResult | undefined {
  return healths.value.find((h) => h.provider === row.provider && h.model === row.model)
}

function fmtMinor(minor: number): string {
  return (minor / 1e6).toLocaleString(undefined, { maximumFractionDigits: 6 })
}

function fmtTime(t?: string): string {
  if (!t) return '—'
  const d = new Date(t)
  return Number.isNaN(d.getTime()) ? '—' : d.toLocaleString()
}

async function load() {
  loading.value = true
  try {
    view.value = await mlopsApi.models()
    healths.value = (await mlopsApi.modelHealth()).items ?? []
    const items: MLOpsBinding[] = (await mlopsApi.bindings()).items ?? []
    for (const row of bindingRows) {
      const hit = items.find((b) => b.scenario === row.scenario)
      row.model = hit?.model ?? ''
      row.updated_at = hit?.updated_at ?? ''
      row.updated_by = hit?.updated_by ?? ''
    }
  } catch {
    view.value = null
  } finally {
    loading.value = false
  }
}

async function toggle(row: MLOpsModelItem, enable: boolean) {
  if (!enable) {
    await ElMessageBox.confirm(
      `禁用「${row.provider}/${row.model}」？进行中的请求不受影响，新请求不再路由到它；${row.is_default ? '它是默认模型，禁用后按配置顺序回退。' : ''}`,
      '禁用模型',
      { type: 'warning' },
    )
  }
  toggling.value = row.model
  try {
    if (enable) await mlopsApi.enableModel({ provider: row.provider, model: row.model })
    else await mlopsApi.disableModel({ provider: row.provider, model: row.model })
    ElMessage.success(enable ? '已启用（热重载生效）' : '已禁用（热重载生效）')
    await load()
  } catch {
    /* 409（最后一个模型）等已提示 */
  } finally {
    toggling.value = ''
  }
}

async function testHealth(row: MLOpsModelItem) {
  await ElMessageBox.confirm(
    `对「${row.model}」发送一次真实最小请求测试连通性？（会产生极少量 token 消耗，计入 health 场景）`,
    '健康测试',
    { type: 'warning' },
  )
  testing.value = row.model
  try {
    const resp = await mlopsApi.testModelHealth({ provider: row.provider, model: row.model })
    if (resp.ok) ElMessage.success(`${row.model} 连通正常（${resp.latency_ms}ms）`)
    else ElMessage.error(`${row.model} 测试失败：${resp.error ?? '未知错误'}`)
    healths.value = (await mlopsApi.modelHealth()).items ?? []
  } catch {
    /* 已提示 */
  } finally {
    testing.value = ''
  }
}

async function saveBinding(row: { scenario: string; model: string }) {
  try {
    if (row.model) {
      await mlopsApi.saveBinding(row.scenario, row.model)
      ElMessage.success(`已绑定 ${row.scenario} → ${row.model}`)
    } else {
      await mlopsApi.deleteBinding(row.scenario)
      ElMessage.success(`已解除 ${row.scenario} 绑定`)
    }
    await load()
  } catch {
    await load()
  }
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
.ml4 {
  margin-left: 4px;
}
.mono {
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  font-size: 12px;
}
.muted {
  color: var(--el-text-color-placeholder);
}
.ok {
  color: var(--el-color-success);
}
.err {
  color: var(--el-color-danger);
}
.panel-title {
  margin: 20px 0 8px;
}
</style>
