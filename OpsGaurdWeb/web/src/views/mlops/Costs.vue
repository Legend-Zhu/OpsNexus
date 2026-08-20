<template>
  <div class="costs">
    <el-alert type="info" :closable="false" class="mb"
      title="按底层 provider 调用计量（Agent 工具轮/压缩各计一条）"
      description="费用为估算：明细入账时锁定当时单价，改价不回溯历史；usage_present=false 表示上游未返回用量；priced=false 表示未配单价（≠免费）。金额单位为元（CNY）。" />

    <!-- 今日/本月摘要 -->
    <el-row :gutter="12" class="mb" v-loading="loadingOverview">
      <el-col :span="6" v-for="card in summaryCards" :key="card.label">
        <el-card shadow="never" class="stat-card">
          <div class="stat-label">{{ card.label }}</div>
          <div class="stat-value">{{ card.value }}</div>
          <div class="stat-sub">{{ card.sub }}</div>
        </el-card>
      </el-col>
    </el-row>

    <!-- 月度预算（P3） -->
    <h4 class="panel-title">
      月度预算（超限只告警不拦截；档位经通知渠道各发一次）
      <el-button size="small" class="ml8" type="primary" :icon="Plus" @click="openBudget">设置预算</el-button>
    </h4>
    <el-table :data="budgets" size="small" class="mb" empty-text="未设置预算（在全部启用通知渠道上无预算告警）">
      <el-table-column prop="month" label="月份" width="100" />
      <el-table-column label="预算(元)" width="110" align="right">
        <template #default="{ row }">{{ fmtMinor(row.limit_minor) }}</template>
      </el-table-column>
      <el-table-column label="已计价(元)" width="110" align="right">
        <template #default="{ row }">{{ fmtMinor(row.spend_minor) }}</template>
      </el-table-column>
      <el-table-column label="使用率" min-width="180">
        <template #default="{ row }">
          <el-progress :percentage="Math.min(100, Math.round(row.usage_ratio * 100))" :stroke-width="10"
            :status="row.usage_ratio >= 1 ? 'exception' : row.usage_ratio >= row.warn_at ? 'warning' : undefined" />
          <span class="muted size12">{{ (row.usage_ratio * 100).toFixed(1) }}% · 未计价 {{ row.unpriced_calls }} 次 · 已通知档位 {{ row.notified.map((n: number) => n * 100 + '%').join('/') || '—' }}</span>
        </template>
      </el-table-column>
      <el-table-column label="预警档位" width="90" align="right">
        <template #default="{ row }">{{ row.warn_at * 100 }}%</template>
      </el-table-column>
      <el-table-column label="操作" width="80">
        <template #default="{ row }">
          <el-button link type="danger" @click="removeBudget(row)">删除</el-button>
        </template>
      </el-table-column>
    </el-table>

    <!-- 按模型 / 按场景（本月） -->
    <el-row :gutter="12" class="mb">
      <el-col :span="12">
        <h4 class="panel-title">按模型（本月）</h4>
        <el-table :data="overview?.by_model ?? []" size="small" empty-text="本月暂无调用">
          <el-table-column label="provider / model" min-width="200">
            <template #default="{ row }">
              <span class="mono">{{ row.provider }} / {{ row.model }}</span>
            </template>
          </el-table-column>
          <el-table-column label="调用" width="70">
            <template #default="{ row }">{{ row.totals.calls }}</template>
          </el-table-column>
          <el-table-column label="tokens" width="110">
            <template #default="{ row }">{{ row.totals.total_tokens.toLocaleString() }}</template>
          </el-table-column>
          <el-table-column label="费用(元)" width="100" align="right">
            <template #default="{ row }">{{ fmtMinor(row.totals.cost_minor) }}</template>
          </el-table-column>
        </el-table>
      </el-col>
      <el-col :span="12">
        <h4 class="panel-title">按场景（本月）</h4>
        <el-table :data="overview?.by_scenario ?? []" size="small" empty-text="本月暂无调用">
          <el-table-column label="scenario" min-width="140">
            <template #default="{ row }">
              <el-tag size="small" effect="plain">{{ row.scenario }}</el-tag>
            </template>
          </el-table-column>
          <el-table-column label="operation" width="90">
            <template #default="{ row }">{{ row.totals.operations }}</template>
          </el-table-column>
          <el-table-column label="调用" width="70">
            <template #default="{ row }">{{ row.totals.calls }}</template>
          </el-table-column>
          <el-table-column label="失败/取消" width="90">
            <template #default="{ row }">
              <span :class="{ 'err': row.totals.error_calls + row.totals.canceled_calls > 0 }">
                {{ row.totals.error_calls }}/{{ row.totals.canceled_calls }}
              </span>
            </template>
          </el-table-column>
          <el-table-column label="未计量" width="70">
            <template #default="{ row }">
              <span :class="{ 'warn': row.totals.unmetered_calls > 0 }">{{ row.totals.unmetered_calls }}</span>
            </template>
          </el-table-column>
          <el-table-column label="tokens" width="110">
            <template #default="{ row }">{{ row.totals.total_tokens.toLocaleString() }}</template>
          </el-table-column>
        </el-table>
      </el-col>
    </el-row>

    <!-- 日趋势 -->
    <h4 class="panel-title">
      日趋势
      <el-date-picker v-model="trendRange" type="daterange" size="small" value-format="YYYY-MM-DD"
        :clearable="false" :disabled-date="(d: Date) => d.getTime() > Date.now()" style="margin-left: 12px; width: 240px" />
      <el-button size="small" class="ml8" :loading="loadingTrend" @click="loadTrend">查询</el-button>
    </h4>
    <el-table :data="trend" size="small" class="mb" v-loading="loadingTrend" empty-text="暂无数据" max-height="320">
      <el-table-column prop="day" label="日期" width="120" />
      <el-table-column label="费用占比" min-width="160">
        <template #default="{ row }">
          <el-progress :percentage="costPercent(row.totals.cost_minor)" :show-text="false"
            :stroke-width="8" color="#409eff" />
        </template>
      </el-table-column>
      <el-table-column label="费用(元)" width="100" align="right">
        <template #default="{ row }">{{ fmtMinor(row.totals.cost_minor) }}</template>
      </el-table-column>
      <el-table-column label="operation" width="90">
        <template #default="{ row }">{{ row.totals.operations }}</template>
      </el-table-column>
      <el-table-column label="调用" width="70">
        <template #default="{ row }">{{ row.totals.calls }}</template>
      </el-table-column>
      <el-table-column label="tokens" width="120">
        <template #default="{ row }">{{ row.totals.total_tokens.toLocaleString() }}</template>
      </el-table-column>
      <el-table-column label="失败/未计量" width="100">
        <template #default="{ row }">{{ row.totals.error_calls + row.totals.canceled_calls }} / {{ row.totals.unmetered_calls }}</template>
      </el-table-column>
    </el-table>

    <!-- 单价管理 -->
    <h4 class="panel-title">
      模型单价（元/百万 token）
      <el-button size="small" class="ml8" type="primary" :icon="Plus" @click="openPricing">配置单价</el-button>
    </h4>
    <el-table :data="pricings" size="small" class="mb" empty-text="未配置单价（调用只计 token，不计费用）">
      <el-table-column label="provider / model" min-width="220">
        <template #default="{ row }"><span class="mono">{{ row.provider }} / {{ row.model }}</span></template>
      </el-table-column>
      <el-table-column prop="price_in_per_m" label="输入" width="100" align="right" />
      <el-table-column prop="price_out_per_m" label="输出" width="100" align="right" />
      <el-table-column label="更新" width="170">
        <template #default="{ row }">{{ row.updated_at }}<span v-if="row.updated_by" class="muted"> · {{ row.updated_by }}</span></template>
      </el-table-column>
      <el-table-column prop="note" label="备注" min-width="120" show-overflow-tooltip />
      <el-table-column label="操作" width="120">
        <template #default="{ row }">
          <el-button link type="primary" @click="editPricing(row)">编辑</el-button>
          <el-button link type="danger" @click="removePricing(row)">删除</el-button>
        </template>
      </el-table-column>
    </el-table>

    <!-- 明细 -->
    <h4 class="panel-title">
      调用明细
      <span class="muted size12">（默认最近 90 天；点击 operation_id 下钻该次业务请求的全部调用）</span>
    </h4>
    <div class="filters">
      <el-date-picker v-model="detailRange" type="daterange" size="small" value-format="YYYY-MM-DD"
        :clearable="false" style="width: 240px" />
      <el-select v-model="filters.scenario" size="small" clearable placeholder="场景" style="width: 150px">
        <el-option v-for="s in scenarios" :key="s" :label="s" :value="s" />
      </el-select>
      <el-select v-model="filters.status" size="small" clearable placeholder="状态" style="width: 140px">
        <el-option label="success" value="success" />
        <el-option label="provider_error" value="provider_error" />
        <el-option label="canceled" value="canceled" />
      </el-select>
      <el-input v-model="filters.model" size="small" clearable placeholder="模型名" style="width: 180px" />
      <el-button size="small" type="primary" :loading="loadingDetail" @click="loadDetail(1)">查询</el-button>
    </div>
    <el-table :data="details" size="small" v-loading="loadingDetail" empty-text="暂无明细">
      <el-table-column label="时间" width="165">
        <template #default="{ row }">{{ fmtTime(row.started_at) }}</template>
      </el-table-column>
      <el-table-column label="provider/model" min-width="170">
        <template #default="{ row }"><span class="mono">{{ row.provider }}/{{ row.model }}</span></template>
      </el-table-column>
      <el-table-column label="场景" width="110">
        <template #default="{ row }">
          <el-tag size="small" effect="plain" :type="row.scenario === 'compress' ? 'warning' : 'info'">{{ row.scenario }}</el-tag>
          <span v-if="row.round" class="muted"> r{{ row.round }}</span>
        </template>
      </el-table-column>
      <el-table-column label="状态" width="120">
        <template #default="{ row }">
          <el-tag size="small" :type="statusTag(row.status)">{{ row.status }}</el-tag>
          <el-tooltip v-if="row.error" :content="row.error"><span class="muted">?</span></el-tooltip>
        </template>
      </el-table-column>
      <el-table-column label="tokens(p/c)" width="110" align="right">
        <template #default="{ row }">
          <span v-if="row.usage_present">{{ row.prompt_tokens }}/{{ row.completion_tokens }}</span>
          <el-tag v-else size="small" type="warning" effect="plain">缺失</el-tag>
        </template>
      </el-table-column>
      <el-table-column label="费用" width="90" align="right">
        <template #default="{ row }">
          <span v-if="row.priced">{{ fmtMinor(row.cost_minor) }}</span>
          <span v-else class="muted">未计价</span>
        </template>
      </el-table-column>
      <el-table-column label="耗时" width="80" align="right">
        <template #default="{ row }">{{ row.latency_ms >= 1000 ? (row.latency_ms / 1000).toFixed(1) + 's' : row.latency_ms + 'ms' }}</template>
      </el-table-column>
      <el-table-column label="operation" min-width="160">
        <template #default="{ row }">
          <el-button link type="primary" class="mono" @click="drillOperation(row.operation_id)">{{ row.operation_id }}</el-button>
        </template>
      </el-table-column>
    </el-table>
    <div class="pager">
      <el-pagination size="small" layout="total, prev, pager, next" :total="detailTotal"
        :page-size="pageSize" :current-page="page" @current-change="loadDetail" />
      <span v-if="detailTruncated" class="warn size12">结果超过扫描上限，已截断——请缩小时间范围</span>
    </div>

    <!-- operation 下钻 -->
    <el-dialog v-model="opVisible" :title="`operation ${opId} 的全部调用`" width="900px">
      <el-table :data="opItems" size="small" empty-text="无记录">
        <el-table-column label="时间" width="165">
          <template #default="{ row }">{{ fmtTime(row.started_at) }}</template>
        </el-table-column>
        <el-table-column label="provider/model" min-width="160">
          <template #default="{ row }"><span class="mono">{{ row.provider }}/{{ row.model }}</span></template>
        </el-table-column>
        <el-table-column label="场景" width="110">
          <template #default="{ row }">{{ row.scenario }}<span v-if="row.round" class="muted"> r{{ row.round }}</span></template>
        </el-table-column>
        <el-table-column prop="status" label="状态" width="120" />
        <el-table-column label="tokens" width="100" align="right">
          <template #default="{ row }">{{ row.usage_present ? row.total_tokens : '缺失' }}</template>
        </el-table-column>
        <el-table-column label="费用" width="90" align="right">
          <template #default="{ row }">{{ row.priced ? fmtMinor(row.cost_minor) : '未计价' }}</template>
        </el-table-column>
        <el-table-column label="入口" min-width="180">
          <template #default="{ row }"><span class="mono size12">{{ row.entry_point || '—' }}</span></template>
        </el-table-column>
      </el-table>
    </el-dialog>

    <!-- 单价编辑 -->
    <el-dialog v-model="pricingVisible" :title="pricingForm._edit ? '编辑单价' : '配置单价'" width="480px">
      <el-form label-width="120px">
        <el-form-item label="provider" required>
          <el-input v-model="pricingForm.provider" :disabled="pricingForm._edit" placeholder="如 openai" />
        </el-form-item>
        <el-form-item label="model" required>
          <el-input v-model="pricingForm.model" :disabled="pricingForm._edit" placeholder="如 gpt-4o（可含 /）" />
        </el-form-item>
        <el-form-item label="输入 元/百万token" required>
          <el-input v-model="pricingForm.price_in" placeholder="如 2.5；免费填 0" />
        </el-form-item>
        <el-form-item label="输出 元/百万token" required>
          <el-input v-model="pricingForm.price_out" placeholder="如 10" />
        </el-form-item>
        <el-form-item label="备注">
          <el-input v-model="pricingForm.note" placeholder="可选" />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="pricingVisible = false">取消</el-button>
        <el-button type="primary" :loading="savingPricing" @click="savePricing">保存</el-button>
      </template>
    </el-dialog>

    <!-- 预算编辑 -->
    <el-dialog v-model="budgetVisible" title="设置月度预算" width="460px">
      <el-form label-width="130px">
        <el-form-item label="月份" required>
          <el-date-picker v-model="budgetForm.month" type="month" value-format="YYYY-MM" :clearable="false" style="width: 160px" />
        </el-form-item>
        <el-form-item label="预算上限(元)" required>
          <el-input v-model="budgetForm.limit" placeholder="如 100；须为正数" />
        </el-form-item>
        <el-form-item label="预警档位" required>
          <el-input-number v-model="budgetForm.warnPercent" :min="1" :max="99" style="width: 160px" />
          <span class="muted size12" style="margin-left: 8px">%（达档位与 100% 各通知一次）</span>
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="budgetVisible = false">取消</el-button>
        <el-button type="primary" :loading="savingBudget" @click="saveBudget">保存</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Plus } from '@element-plus/icons-vue'
import { mlopsApi } from '@/api'
import type { MLOpsBudgetView, MLOpsCostTrendRow, MLOpsCostsOverview, MLOpsPricing, MLOpsUsageDetail } from '@/types'

const scenarios = ['chat', 'investigate', 'native_chat', 'patrol_report', 'compress', 'health']
const pageSize = 20

const overview = ref<MLOpsCostsOverview | null>(null)
const loadingOverview = ref(false)
const trend = ref<MLOpsCostTrendRow[]>([])
const loadingTrend = ref(false)
const details = ref<MLOpsUsageDetail[]>([])
const detailTotal = ref(0)
const detailTruncated = ref(false)
const loadingDetail = ref(false)
const page = ref(1)
const pricings = ref<MLOpsPricing[]>([])
const savingPricing = ref(false)

const opVisible = ref(false)
const opId = ref('')
const opItems = ref<MLOpsUsageDetail[]>([])

const pricingVisible = ref(false)
const pricingForm = reactive({ provider: '', model: '', price_in: '', price_out: '', note: '', _edit: false })

const budgets = ref<MLOpsBudgetView[]>([])
const budgetVisible = ref(false)
const savingBudget = ref(false)
const budgetForm = reactive({
  month: `${new Date().getFullYear()}-${String(new Date().getMonth() + 1).padStart(2, '0')}`,
  limit: '',
  warnPercent: 80,
})

function fmtDay(d: Date): string {
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`
}
function daysAgo(n: number): string {
  return fmtDay(new Date(Date.now() - n * 86400_000))
}

const trendRange = ref<[string, string]>([daysAgo(29), fmtDay(new Date())])
const detailRange = ref<[string, string]>([daysAgo(89), fmtDay(new Date())])
const filters = reactive({ scenario: '', status: '', model: '' })

const summaryCards = computed(() => {
  const t = overview.value?.today_totals
  const m = overview.value?.month_totals
  return [
    { label: '今日调用', value: String(t?.calls ?? '—'), sub: `operation ${t?.operations ?? 0} · 失败/取消 ${(t?.error_calls ?? 0) + (t?.canceled_calls ?? 0)}` },
    { label: '今日费用(元)', value: t ? fmtMinor(t.cost_minor) : '—', sub: `未计价 ${t?.unpriced_calls ?? 0} · 未计量 ${t?.unmetered_calls ?? 0}` },
    { label: '本月调用', value: String(m?.calls ?? '—'), sub: `operation ${m?.operations ?? 0} · tokens ${(m?.total_tokens ?? 0).toLocaleString()}` },
    { label: '本月费用(元)', value: m ? fmtMinor(m.cost_minor) : '—', sub: `collector 队列 ${overview.value?.collector.queue_len ?? 0}/${overview.value?.collector.queue_cap ?? 0} · 丢弃 ${overview.value?.collector.dropped ?? 0}` },
  ]
})

// 微元 → 元字符串（最多 6 位小数，去尾零）
function fmtMinor(minor: number): string {
  const v = minor / 1e6
  return v.toLocaleString(undefined, { maximumFractionDigits: 6 })
}

const maxTrendCost = computed(() => Math.max(1, ...trend.value.map((r) => r.totals.cost_minor)))
function costPercent(minor: number): number {
  return Math.round((minor / maxTrendCost.value) * 100)
}

function statusTag(status: string): 'success' | 'danger' | 'info' {
  if (status === 'success') return 'success'
  if (status === 'canceled') return 'info'
  return 'danger'
}

function fmtTime(t?: string): string {
  if (!t) return '—'
  const d = new Date(t)
  return Number.isNaN(d.getTime()) ? '—' : d.toLocaleString()
}

async function loadOverview() {
  loadingOverview.value = true
  try {
    overview.value = await mlopsApi.costsOverview()
  } catch {
    overview.value = null
  } finally {
    loadingOverview.value = false
  }
}

async function loadTrend() {
  loadingTrend.value = true
  try {
    const resp = await mlopsApi.costsTrend({ from: trendRange.value?.[0], to: trendRange.value?.[1] })
    trend.value = resp.items ?? []
  } catch {
    trend.value = []
  } finally {
    loadingTrend.value = false
  }
}

async function loadDetail(p = 1) {
  page.value = p
  loadingDetail.value = true
  try {
    const resp = await mlopsApi.costsDetail({
      from: detailRange.value?.[0],
      to: detailRange.value?.[1],
      scenario: filters.scenario || undefined,
      status: filters.status || undefined,
      model: filters.model || undefined,
      limit: pageSize,
      offset: (p - 1) * pageSize,
    })
    details.value = resp.items ?? []
    detailTotal.value = resp.total ?? 0
    detailTruncated.value = !!resp.truncated
  } catch {
    details.value = []
    detailTotal.value = 0
  } finally {
    loadingDetail.value = false
  }
}

async function loadPricings() {
  try {
    const resp = await mlopsApi.pricingList()
    pricings.value = resp.items ?? []
  } catch {
    pricings.value = []
  }
}

async function drillOperation(id: string) {
  opId.value = id
  opVisible.value = true
  try {
    const resp = await mlopsApi.costsOperation(id)
    opItems.value = resp.items ?? []
  } catch {
    opItems.value = []
  }
}

function openPricing() {
  Object.assign(pricingForm, { provider: '', model: '', price_in: '', price_out: '', note: '', _edit: false })
  pricingVisible.value = true
}

function editPricing(row: MLOpsPricing) {
  Object.assign(pricingForm, {
    provider: row.provider, model: row.model,
    price_in: row.price_in_per_m, price_out: row.price_out_per_m,
    note: row.note ?? '', _edit: true,
  })
  pricingVisible.value = true
}

async function savePricing() {
  if (!pricingForm.provider.trim() || !pricingForm.model.trim()) {
    ElMessage.warning('请填写 provider 和 model')
    return
  }
  if (!/^\d{1,10}(\.\d{1,6})?$/.test(pricingForm.price_in.trim()) || !/^\d{1,10}(\.\d{1,6})?$/.test(pricingForm.price_out.trim())) {
    ElMessage.warning('单价格式：非负数字，最多 6 位小数（免费填 0）')
    return
  }
  savingPricing.value = true
  try {
    await mlopsApi.pricingSave({
      provider: pricingForm.provider.trim(),
      model: pricingForm.model.trim(),
      price_in_per_m: pricingForm.price_in.trim(),
      price_out_per_m: pricingForm.price_out.trim(),
      note: pricingForm.note.trim() || undefined,
    })
    ElMessage.success('单价已保存（只影响之后的调用）')
    pricingVisible.value = false
    await loadPricings()
  } catch {
    /* 错误已提示 */
  } finally {
    savingPricing.value = false
  }
}

async function removePricing(row: MLOpsPricing) {
  await ElMessageBox.confirm(`删除「${row.provider}/${row.model}」的单价？之后该模型调用只计 token 不计费用。`, '删除单价', { type: 'warning' })
  await mlopsApi.pricingDelete(row.provider, row.model)
  ElMessage.success('已删除')
  await loadPricings()
}

// ---- 预算 ----

async function loadBudgets() {
  try {
    budgets.value = (await mlopsApi.budgets()).items ?? []
  } catch {
    budgets.value = []
  }
}

function openBudget() {
  budgetForm.month = `${new Date().getFullYear()}-${String(new Date().getMonth() + 1).padStart(2, '0')}`
  budgetForm.limit = ''
  budgetVisible.value = true
}

async function saveBudget() {
  const limit = Number(budgetForm.limit)
  if (!Number.isFinite(limit) || limit <= 0) {
    ElMessage.warning('预算上限须为正数（元）')
    return
  }
  savingBudget.value = true
  try {
    await mlopsApi.saveBudget({
      month: budgetForm.month,
      limit_minor: Math.round(limit * 1e6),
      warn_at: budgetForm.warnPercent / 100,
    })
    ElMessage.success('预算已保存（覆盖会重置通知档位）')
    budgetVisible.value = false
    await loadBudgets()
  } catch {
    /* 已提示 */
  } finally {
    savingBudget.value = false
  }
}

async function removeBudget(row: MLOpsBudgetView) {
  await ElMessageBox.confirm(`删除 ${row.month} 的预算？`, '删除预算', { type: 'warning' })
  await mlopsApi.deleteBudget(row.month)
  ElMessage.success('已删除')
  await loadBudgets()
}

onMounted(() => {
  void loadOverview()
  void loadTrend()
  void loadDetail(1)
  void loadPricings()
  void loadBudgets()
})
</script>

<style scoped>
.mb {
  margin-bottom: 12px;
}
.ml8 {
  margin-left: 8px;
}
.mono {
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  font-size: 12px;
}
.muted {
  color: var(--el-text-color-placeholder);
}
.size12 {
  font-size: 12px;
}
.err {
  color: var(--el-color-danger);
}
.warn {
  color: var(--el-color-warning);
}
.panel-title {
  margin: 16px 0 8px;
  display: flex;
  align-items: center;
}
.stat-card :deep(.el-card__body) {
  padding: 12px 16px;
}
.stat-label {
  font-size: 12px;
  color: var(--el-text-color-secondary);
}
.stat-value {
  font-size: 22px;
  font-weight: 600;
  line-height: 1.4;
}
.stat-sub {
  font-size: 12px;
  color: var(--el-text-color-placeholder);
}
.filters {
  display: flex;
  gap: 8px;
  margin-bottom: 8px;
  flex-wrap: wrap;
}
.pager {
  margin-top: 8px;
  display: flex;
  align-items: center;
  gap: 12px;
}
</style>
