<template>
  <div class="page">
    <!-- 页头：标题 + 日期 + 系统健康胶囊 -->
    <header class="page-head">
      <div class="head-left">
        <h2>运营总览</h2>
        <span class="head-date">{{ dateText }}</span>
      </div>
      <div v-if="statsLoaded" class="status-pill" :class="pill.cls">
        <span class="dot" />
        <span>{{ pill.text }}</span>
      </div>
    </header>

    <div class="kpis">
      <div class="kpi">
        <div class="kpi-top">
          <span class="kpi-icon"><el-icon><Platform /></el-icon></span>
          <span class="kpi-label">在线集群</span>
        </div>
        <span class="kpi-value kpi-num">{{ onlineClusters }}</span>
        <span class="kpi-sub">共 {{ clusters.length }} 个集群</span>
      </div>
      <div class="kpi">
        <div class="kpi-top">
          <span class="kpi-icon warn"><el-icon><Bell /></el-icon></span>
          <span class="kpi-label">未处理告警</span>
        </div>
        <span class="kpi-value kpi-num warn">{{ activeAlerts }}</span>
        <span class="kpi-sub">需处理</span>
      </div>
      <div class="kpi">
        <div class="kpi-top">
          <span class="kpi-icon"><el-icon><FolderOpened /></el-icon></span>
          <span class="kpi-label">项目</span>
        </div>
        <span class="kpi-value kpi-num">{{ projectCount }}</span>
        <span class="kpi-sub">管理分组</span>
      </div>
      <div class="kpi">
        <div class="kpi-top">
          <span class="kpi-icon"><el-icon><Timer /></el-icon></span>
          <span class="kpi-label">巡检异常</span>
        </div>
        <span class="kpi-value kpi-num">{{ patrolAnomalies }}</span>
        <span class="kpi-sub">最近一次执行</span>
      </div>
    </div>

    <!-- 模型用量概览（MLOps 运营层未启用时整块隐藏） -->
    <section v-if="usageVisible" class="card usage">
      <header class="card-head">
        <h3>模型用量概览</h3>
        <div class="head-actions">
          <div class="seg">
            <button
              v-for="m in METRICS"
              :key="m.key"
              type="button"
              :class="{ active: metric === m.key }"
              @click="metric = m.key"
            >
              {{ m.label }}
            </button>
          </div>
          <el-button link type="primary" @click="$router.push('/mlops')">用量费用 →</el-button>
        </div>
      </header>

      <div class="usage-stats">
        <div class="ustat">
          <span class="ustat-label">今日调用</span>
          <span class="ustat-value">{{ todayTotals.calls.toLocaleString() }}</span>
          <span class="ustat-sub">成功率 {{ todaySuccessRate }} · 失败/取消 {{ todayTotals.error_calls + todayTotals.canceled_calls }}</span>
        </div>
        <div class="ustat">
          <span class="ustat-label">今日 Tokens</span>
          <span class="ustat-value">{{ fmtCompact(todayTotals.total_tokens) }}</span>
          <span class="ustat-sub">输入 {{ fmtCompact(todayTotals.prompt_tokens) }} · 输出 {{ fmtCompact(todayTotals.completion_tokens) }}</span>
        </div>
        <div class="ustat">
          <span class="ustat-label">今日费用</span>
          <span class="ustat-value">{{ fmtMoney(todayTotals.cost_minor) }}</span>
          <span class="ustat-sub">未计价调用 {{ todayTotals.unpriced_calls }}</span>
        </div>
        <div class="ustat">
          <span class="ustat-label">本月费用</span>
          <span class="ustat-value">{{ fmtMoney(monthTotals.cost_minor) }}</span>
          <span class="ustat-sub">本月调用 {{ monthTotals.calls.toLocaleString() }} · Tokens {{ fmtCompact(monthTotals.total_tokens) }}</span>
        </div>
      </div>

      <div ref="chartEl" class="chart">
        <template v-if="hasTrend">
          <div class="chart-plot">
            <svg :viewBox="`0 0 ${CHART_W} ${CHART_H}`" preserveAspectRatio="none" class="chart-svg" aria-hidden="true">
              <line
                :x1="PAD_X" :x2="CHART_W - PAD_X"
                :y1="PAD_T + plotH" :y2="PAD_T + plotH"
                class="baseline"
              />
              <rect
                v-for="b in trendBars"
                :key="b.day"
                class="bar"
                :x="b.x" :y="b.y" :width="b.w" :height="b.h" rx="2"
                @mousemove="onBarMove($event, b.row)"
                @mouseleave="tip = null"
              />
            </svg>
            <span class="chart-peak">峰值 {{ fmtCompact(trendMax) }}{{ metricUnit }}</span>
          </div>
          <div class="chart-ticks">
            <span
              v-for="(b, i) in trendBars"
              v-show="b.showTick"
              :key="b.day"
              class="tick"
              :style="tickStyle(i)"
            >{{ b.day.slice(5) }}</span>
          </div>
          <div
            v-if="tip"
            class="chart-tip"
            :style="{ left: `${tip.x}px`, top: `${tip.y}px` }"
          >
            <div class="tip-day mono">{{ tip.row.day }}</div>
            <div class="tip-row"><span>调用</span><b>{{ tip.row.totals.calls.toLocaleString() }} 次</b></div>
            <div class="tip-row"><span>Tokens</span><b>{{ fmtCompact(tip.row.totals.total_tokens) }}</b></div>
            <div class="tip-row"><span>费用</span><b>{{ fmtMoney(tip.row.totals.cost_minor) }}</b></div>
          </div>
        </template>
        <div v-else class="chart-empty">近 30 天暂无模型调用</div>
      </div>

      <footer class="usage-foot">
        <span>采集队列 {{ collector.queue_len }}/{{ collector.queue_cap }} · 丢弃 {{ collector.dropped }} · 明细保留 {{ collector.retain_days }} 天</span>
        <span>统计口径：业务时区 {{ overview?.timezone }} · 趋势为近 30 天</span>
      </footer>
    </section>

    <div class="sections">
      <section class="card">
        <header class="card-head">
          <h3>集群健康</h3>
          <el-button link type="primary" @click="$router.push('/clusters')">全部集群 →</el-button>
        </header>
        <div class="row-list">
          <div
            v-for="c in clusters"
            :key="c.name"
            class="cluster-row"
            @click="$router.push(`/clusters/${c.name}`)"
          >
            <span class="st-dot" :class="c.status === 'online' ? 'ok' : 'bad'" />
            <div class="row-main">
              <div class="row-line">
                <span class="row-title">{{ c.name }}</span>
                <span class="row-note mono">{{ c.worker_url }}</span>
                <span class="row-time">探测于 {{ fmtAgo(c.last_seen) }}</span>
                <el-tag size="small" :type="c.status === 'online' ? 'success' : 'danger'" effect="plain" round>
                  {{ c.status === 'online' ? '在线' : '离线' }}
                </el-tag>
              </div>
              <div v-if="c.err" class="row-err mono">{{ c.err }}</div>
            </div>
          </div>
          <div v-if="!clusters.length" class="muted">暂无集群</div>
        </div>
      </section>

      <section class="card">
        <header class="card-head">
          <h3>智能巡检</h3>
          <el-button link type="primary" @click="$router.push('/patrol')">巡检中心 →</el-button>
        </header>
        <div class="row-list">
          <div
            v-for="row in patrolRows"
            :key="row.patrol.id"
            class="cluster-row"
            @click="$router.push('/patrol')"
          >
            <span class="st-dot" :class="[patrolDotCls(row.run), { pulse: row.run?.status === 'running' }]" />
            <div class="row-main">
              <div class="row-line">
                <span class="row-title">{{ row.patrol.name }}</span>
                <span class="row-note" :class="{ 'note-bad': runNoteBad(row.run) }">{{ runNote(row.run) }}</span>
                <span class="row-time">{{ fmtAgo(row.run?.started_at) }}</span>
                <el-tag size="small" :type="runTag(row.run).type" effect="plain" round>{{ runTag(row.run).text }}</el-tag>
              </div>
              <div v-if="row.run?.status === 'failed' && row.run.error" class="row-err mono">{{ row.run.error }}</div>
            </div>
          </div>
          <div v-if="!patrolRows.length" class="muted">暂无巡检任务</div>
        </div>
      </section>

      <section class="card">
        <header class="card-head">
          <h3>最近告警</h3>
          <el-button link type="primary" @click="$router.push('/alerts')">告警中心 →</el-button>
        </header>
        <div class="row-list">
          <div
            v-for="a in recentAlerts"
            :key="a.id"
            class="alert-row"
            :class="{ done: a.status === 'recovered' }"
            @click="$router.push('/alerts')"
          >
            <span class="st-dot" :class="a.level === 'error' ? 'bad' : a.level === 'warn' ? 'warn' : 'dim'" />
            <div class="row-main">
              <div class="alert-title">{{ a.title }}</div>
              <div class="row-sub-line">
                <span class="mono">{{ a.cluster }}/{{ a.service }}</span>
                <span v-if="a.count > 1">触发 {{ a.count }} 次</span>
                <span>{{ fmtAgo(a.last_ts) }}</span>
              </div>
            </div>
            <span class="alert-status" :class="a.status === 'active' ? 'bad' : a.status === 'acked' ? 'warn' : 'ok'">
              <i />{{ a.status === 'active' ? '未处理' : a.status === 'acked' ? '已认领' : '已恢复' }}
            </span>
          </div>
          <div v-if="!recentAlerts.length" class="muted">暂无告警</div>
        </div>
      </section>

      <section class="card">
        <header class="card-head">
          <h3>AI 异常排查</h3>
          <el-button link type="primary" @click="$router.push('/troubleshoot')">排查记录 →</el-button>
        </header>
        <div class="row-list">
          <div
            v-for="inv in investigations"
            :key="inv.id"
            class="cluster-row"
            @click="$router.push('/troubleshoot')"
          >
            <span class="st-dot accent" />
            <div class="row-main">
              <div class="alert-title">{{ inv.title }}</div>
              <div class="row-sub-line">
                <span v-if="inv.cluster" class="mono">{{ inv.cluster }}</span>
                <span v-if="inv.model" class="mono">{{ inv.model }}</span>
                <span>{{ fmtAgo(inv.updated_at || inv.created_at) }}</span>
              </div>
            </div>
            <span class="alert-status" :class="inv.conclusion ? 'ok' : 'info'">
              <i />{{ inv.conclusion ? '已出结论' : '排查中' }}
            </span>
          </div>
          <div v-if="!investigations.length" class="muted">暂无排查会话</div>
        </div>
      </section>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { Bell, FolderOpened, Platform, Timer } from '@element-plus/icons-vue'
import { alertApi, clusterApi, investigationApi, mlopsApi, patrolApi, projectApi } from '@/api'
import type { Alert, ClusterSummary, Investigation, MLOpsCostTotals, MLOpsCostTrendRow, MLOpsCostsOverview, Patrol, PatrolRun } from '@/types'

// ---------- 系统总览（集群 / 告警 / 项目 / 巡检） ----------

const clusters = ref<ClusterSummary[]>([])
const alerts = ref<Alert[]>([])

// KPI 展示值（数字滚动从 0 递增）
const onlineClusters = ref(0)
const activeAlerts = ref(0)
const projectCount = ref(0)
const patrolAnomalies = ref(0)
const statsLoaded = ref(false)

const activeAlertsTotal = computed(() => alerts.value.filter((a) => a.status !== 'recovered').length)
const recentAlerts = computed(() => alerts.value.slice(0, 8))

// 相对时间：探测/告警时刻以“多久之前”呈现，替代绝对时间戳的宽度压力
function fmtAgo(t?: string): string {
  if (!t) return '—'
  const d = new Date(t)
  if (Number.isNaN(d.getTime())) return '—'
  const s = Math.max(0, (Date.now() - d.getTime()) / 1000)
  if (s < 60) return '刚刚'
  if (s < 3600) return `${Math.floor(s / 60)} 分钟前`
  if (s < 86400) return `${Math.floor(s / 3600)} 小时前`
  if (s < 86400 * 30) return `${Math.floor(s / 86400)} 天前`
  return d.toLocaleDateString()
}

// 系统健康胶囊：无离线集群且无待处理告警即正常
const pill = computed(() => {
  const offline = clusters.value.filter((c) => c.status !== 'online').length
  const pending = activeAlertsTotal.value
  if (offline === 0 && pending === 0) return { cls: 'ok', text: '系统运行正常' }
  const parts: string[] = []
  if (offline > 0) parts.push(`${offline} 个集群离线`)
  if (pending > 0) parts.push(`${pending} 条告警待处理`)
  return { cls: offline > 0 ? 'bad' : 'warn', text: parts.join(' · ') }
})

// 日期时钟（30s 刷新）
const now = ref(new Date())
let clockTimer: number | undefined
const dateText = computed(() => {
  const d = now.value
  const date = d.toLocaleDateString('zh-CN', { year: 'numeric', month: 'long', day: 'numeric', weekday: 'long' })
  const time = d.toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit', hour12: false })
  return `${date} ${time}`
})

// 数字滚动：从 0 递增到目标值（一次，700ms ease-out）
function animateTo(goal: number, display: { value: number }, dur = 700) {
  const start = performance.now()
  const step = (now: number) => {
    const t = Math.min(1, (now - start) / dur)
    const eased = 1 - Math.pow(1 - t, 3)
    display.value = Math.round(goal * eased)
    if (t < 1) requestAnimationFrame(step)
    else display.value = goal
  }
  requestAnimationFrame(step)
}

// ---------- 模型用量概览（复用 /v1/mlops/costs/*，零后端改动） ----------

// 运营层探测：mlops.enabled=false 时 /v1/mlops/* 未注册，请求落到 SPA 回退
// 返回 HTML，get() 解析为 undefined —— 据此隐藏整个区块（与 MLOps 页一致）。
const overview = ref<MLOpsCostsOverview | null>(null)
const trend = ref<MLOpsCostTrendRow[]>([])
const usageLoaded = ref(false)
const usageVisible = computed(() => usageLoaded.value && overview.value != null)

const todayTotals = computed<MLOpsCostTotals>(
  () => overview.value?.today_totals ?? ({} as MLOpsCostTotals),
)
const monthTotals = computed<MLOpsCostTotals>(
  () => overview.value?.month_totals ?? ({} as MLOpsCostTotals),
)
const collector = computed(
  () => overview.value?.collector ?? { queue_len: 0, queue_cap: 0, dropped: 0, deduped: 0, failed: 0, retain_days: 0 },
)
const todaySuccessRate = computed(() => {
  const t = todayTotals.value
  return t.calls > 0 ? `${Math.round((t.success_calls / t.calls) * 100)}%` : '—'
})

// 微元 → 金额字符串（最多 4 位小数，随币种带符号）
function fmtMoney(minor: number): string {
  const v = minor / 1e6
  const s = v.toLocaleString(undefined, { maximumFractionDigits: v < 100 ? 4 : 2 })
  return overview.value?.currency === 'CNY' ? `¥${s}` : `${s} ${overview.value?.currency ?? ''}`.trimEnd()
}

// 大数紧凑展示（中文习惯：万 / 亿）
function fmtCompact(n: number): string {
  const trim = (v: number) => v.toFixed(1).replace(/\.0$/, '')
  if (n >= 1e8) return `${trim(n / 1e8)}亿`
  if (n >= 1e4) return `${trim(n / 1e4)}万`
  if (n >= 1e3) return `${trim(n / 1e3)}k`
  return String(n)
}

// 趋势图指标切换：调用 / Tokens / 费用
type MetricKey = 'calls' | 'tokens' | 'cost'
const METRICS: { key: MetricKey; label: string }[] = [
  { key: 'calls', label: '调用' },
  { key: 'tokens', label: 'Tokens' },
  { key: 'cost', label: '费用' },
]
const metric = ref<MetricKey>('calls')
function metricValue(t: MLOpsCostTotals): number {
  if (metric.value === 'calls') return t.calls
  if (metric.value === 'tokens') return t.total_tokens
  return t.cost_minor
}
const metricUnit = computed(() => (metric.value === 'calls' ? ' 次' : metric.value === 'tokens' ? ' tokens' : ''))

// 手写 SVG 柱状图（无图表库依赖）：viewBox 逻辑坐标，CSS 定高 +
// preserveAspectRatio="none" 拉伸填充（SVG 内只放形状，文字刻度在 HTML 层防拉伸）
const CHART_W = 760
const CHART_H = 150
const PAD_T = 6
const PAD_B = 2
const PAD_X = 2
const GAP = 4
const plotH = CHART_H - PAD_T - PAD_B

const trendMax = computed(() => Math.max(1, ...trend.value.map((r) => metricValue(r.totals))))
const trendBars = computed(() => {
  const n = trend.value.length
  if (!n) return []
  const barW = (CHART_W - PAD_X * 2 - GAP * (n - 1)) / n
  return trend.value.map((r, i) => {
    const v = metricValue(r.totals)
    const h = v <= 0 ? 0 : Math.max(2, Math.round((v / trendMax.value) * plotH))
    return {
      day: r.day,
      row: r,
      x: PAD_X + i * (barW + GAP),
      y: PAD_T + plotH - h,
      w: barW,
      h,
      showTick: i % 5 === 0 || i === n - 1,
    }
  })
})
const hasTrend = computed(() => trendBars.value.some((b) => b.h > 0))

// 刻度标签定位（HTML 层）：首尾贴边避免裁切，其余居中于柱
function tickStyle(i: number): Record<string, string> {
  const bars = trendBars.value
  const b = bars[i]
  if (!b) return {}
  if (i === 0) return { left: '0%', transform: 'none' }
  if (i === bars.length - 1) return { left: '100%', transform: 'translateX(-100%)' }
  return { left: `${((b.x + b.w / 2) / CHART_W) * 100}%` }
}

const chartEl = ref<HTMLDivElement | null>(null)
const tip = ref<{ x: number; y: number; row: MLOpsCostTrendRow } | null>(null)
function onBarMove(e: MouseEvent, row: MLOpsCostTrendRow) {
  const el = chartEl.value
  if (!el) return
  const rect = el.getBoundingClientRect()
  // 悬浮框跟随鼠标，左右各留 70px 防溢出
  const x = Math.min(Math.max(e.clientX - rect.left, 70), rect.width - 70)
  tip.value = { x, y: e.clientY - rect.top, row }
}

// ---------- 智能巡检 / AI 排查面板 ----------

// 每个巡检任务的最近一次执行（任务数多时取前 6，避免面板过长）
const patrolRows = ref<{ patrol: Patrol; run?: PatrolRun }[]>([])

function anomalyCount(run?: PatrolRun): number {
  return (run?.anomalies ?? []).filter((x) => !x.ok).length
}
function patrolDotCls(run?: PatrolRun): string {
  if (!run) return 'dim'
  if (run.status === 'success') return 'ok'
  if (run.status === 'failed') return 'bad'
  return 'warn'
}
function runNote(run?: PatrolRun): string {
  if (!run) return '尚未执行'
  if (run.status === 'running') return '巡检进行中'
  if (run.status === 'failed') return '执行中断'
  const n = anomalyCount(run)
  return n > 0 ? `异常 ${n} 项` : '全部通过'
}
function runNoteBad(run?: PatrolRun): boolean {
  return run?.status === 'failed' || anomalyCount(run) > 0
}
function runTag(run?: PatrolRun): { text: string; type: 'success' | 'danger' | 'warning' | 'info' } {
  if (!run) return { text: '未执行', type: 'info' }
  if (run.status === 'success') return { text: '成功', type: 'success' }
  if (run.status === 'failed') return { text: '失败', type: 'danger' }
  return { text: '运行中', type: 'warning' }
}

async function loadPatrols() {
  try {
    const pl = await patrolApi.list()
    const tasks = (pl.items ?? []).slice(0, 6)
    patrolRows.value = await Promise.all(
      tasks.map(async (p) => {
        try {
          const runs = await patrolApi.runs(p.id, 1)
          return { patrol: p, run: (runs.items ?? [])[0] }
        } catch {
          return { patrol: p }
        }
      }),
    )
  } catch {
    patrolRows.value = []
  }
  // KPI「巡检异常」沿用原口径：第一个有执行记录的任务的异常数
  const firstWithRun = patrolRows.value.find((r) => r.run)
  const goal = firstWithRun ? anomalyCount(firstWithRun.run) : 0
  animateTo(goal, patrolAnomalies)
}

// 最近 AI 排查会话
const investigations = ref<Investigation[]>([])

async function loadInvestigations() {
  try {
    const resp = await investigationApi.list()
    investigations.value = (resp.items ?? [])
      .slice()
      .sort((a, b) => (b.updated_at || b.created_at).localeCompare(a.updated_at || a.created_at))
      .slice(0, 8)
  } catch {
    investigations.value = []
  }
}

async function loadUsage() {
  const fmtDay = (d: Date) => `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`
  try {
    const [ov, tr] = await Promise.all([
      mlopsApi.costsOverview(),
      mlopsApi.costsTrend({ from: fmtDay(new Date(Date.now() - 29 * 86400_000)), to: fmtDay(new Date()) }),
    ])
    overview.value = ov ?? null
    trend.value = tr?.items ?? []
  } catch {
    overview.value = null
  } finally {
    usageLoaded.value = true
  }
}

// ---------- 装配 ----------

async function loadAll() {
  try {
    const [c, a] = await Promise.all([clusterApi.list(), alertApi.list()])
    clusters.value = c.items ?? []
    alerts.value = a.items ?? []
  } catch {
    // 部分失败不阻断总览
  } finally {
    statsLoaded.value = true
  }
  let projects = 0
  try {
    const pl = await projectApi.list()
    projects = (pl.items ?? []).length
  } catch {
    projects = 0
  }
  // 数据就绪后启动滚动（先定格目标值再动画）；巡检异常由 loadPatrols 独立动画
  const goals = {
    online: clusters.value.filter((c) => c.status === 'online').length,
    alerts: activeAlertsTotal.value,
    projects,
  }
  animateTo(goals.online, onlineClusters)
  animateTo(goals.alerts, activeAlerts)
  animateTo(goals.projects, projectCount)
}

onMounted(() => {
  loadAll()
  loadUsage()
  loadPatrols()
  loadInvestigations()
  clockTimer = window.setInterval(() => (now.value = new Date()), 30_000)
})
onBeforeUnmount(() => window.clearInterval(clockTimer))
</script>

<style scoped>
/* ---- 页头 ---- */
.page-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  margin-bottom: 16px;
}
.head-left {
  display: flex;
  align-items: baseline;
  gap: 12px;
  flex-wrap: wrap;
}
.head-left h2 {
  margin: 0;
  font-size: 18px;
  font-weight: 700;
  letter-spacing: -0.01em;
}
.head-date {
  font-size: 12px;
  color: var(--og-text-dim);
  font-variant-numeric: tabular-nums;
}
.status-pill {
  display: inline-flex;
  align-items: center;
  gap: 7px;
  padding: 5px 12px;
  border-radius: 99px;
  font-size: 12px;
  border: 1px solid color-mix(in srgb, currentColor 28%, transparent);
  background: color-mix(in srgb, currentColor 10%, transparent);
}
.status-pill .dot {
  width: 7px;
  height: 7px;
  border-radius: 50%;
  background: currentColor;
  box-shadow: 0 0 0 3px color-mix(in srgb, currentColor 18%, transparent);
}
.status-pill.ok {
  color: var(--el-color-success);
}
.status-pill.warn {
  color: var(--el-color-warning);
}
.status-pill.bad {
  color: var(--el-color-danger);
}

/* ---- KPI ---- */
.kpis {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(200px, 1fr));
  gap: 14px;
  margin-bottom: 14px;
}
.kpi {
  background: var(--og-bg-surface);
  border: 1px solid var(--el-border-color-light);
  border-radius: 12px;
  padding: 14px 18px 12px;
  display: flex;
  flex-direction: column;
  gap: 2px;
  transition: border-color 0.15s, transform 0.15s;
}
.kpi:hover {
  border-color: color-mix(in srgb, var(--og-accent) 35%, transparent);
  transform: translateY(-1px);
}
.kpi-top {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 6px;
}
.kpi-icon {
  display: grid;
  place-items: center;
  width: 28px;
  height: 28px;
  border-radius: 8px;
  background: var(--og-accent-soft);
  color: var(--og-accent-strong);
  font-size: 15px;
}
.kpi-icon.warn {
  background: color-mix(in srgb, var(--el-color-warning) 14%, transparent);
  color: var(--el-color-warning);
}
.kpi-label {
  font-size: 12px;
  color: var(--og-text-dim);
}
.kpi-num {
  font-size: 30px;
  line-height: 1.2;
  color: var(--og-accent-strong);
  font-variant-numeric: tabular-nums;
}
.kpi-num.warn {
  color: var(--el-color-warning);
}
.kpi-sub {
  font-size: 11px;
  color: var(--og-text-dim);
}

/* ---- 通用卡片 ---- */
.card {
  background: var(--og-bg-surface);
  border: 1px solid var(--el-border-color-light);
  border-radius: 12px;
  padding: 14px 16px;
}
.card-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: 10px;
  gap: 10px;
  flex-wrap: wrap;
}
.card-head h3 {
  margin: 0;
  font-size: 14px;
  font-weight: 650;
}
.muted {
  color: var(--og-text-dim);
  font-size: 12px;
  padding: 10px 0;
}

/* ---- 模型用量概览 ---- */
.usage {
  margin-bottom: 14px;
}
.head-actions {
  display: flex;
  align-items: center;
  gap: 12px;
}
.seg {
  display: inline-flex;
  padding: 2px;
  gap: 2px;
  border: 1px solid var(--el-border-color-lighter);
  border-radius: 8px;
  background: var(--og-bg-raised);
}
.seg button {
  border: none;
  background: transparent;
  color: var(--og-text-dim);
  font-size: 12px;
  padding: 4px 12px;
  border-radius: 6px;
  cursor: pointer;
  transition: color 0.12s, background 0.12s;
}
.seg button:hover {
  color: var(--el-text-color-primary);
}
.seg button.active {
  background: var(--og-accent-soft);
  color: var(--og-accent-strong);
  font-weight: 600;
}
.usage-stats {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(180px, 1fr));
  gap: 12px;
  padding: 8px 0 12px;
}
.ustat {
  display: flex;
  flex-direction: column;
  gap: 1px;
}
.ustat-label {
  font-size: 12px;
  color: var(--og-text-dim);
}
.ustat-value {
  font-size: 20px;
  font-weight: 650;
  letter-spacing: -0.02em;
  color: var(--el-text-color-primary);
  font-variant-numeric: tabular-nums;
}
.ustat-sub {
  font-size: 11px;
  color: var(--og-text-dim);
}

/* 趋势图：定高拉伸，宽度随卡片、高度不随宽度膨胀 */
.chart {
  position: relative;
  border-top: 1px solid var(--el-border-color-extra-light);
  padding-top: 10px;
}
.chart-plot {
  position: relative;
  height: 160px;
}
.chart-svg {
  display: block;
  width: 100%;
  height: 100%;
}
.chart-peak {
  position: absolute;
  top: 0;
  left: 2px;
  font-size: 11px;
  color: var(--og-text-dim);
}
.baseline {
  stroke: var(--el-border-color-lighter);
  stroke-width: 1;
}
.bar {
  fill: color-mix(in srgb, var(--og-accent) 72%, transparent);
  cursor: pointer;
  transition: fill 0.12s;
}
.bar:hover {
  fill: var(--og-accent-strong);
}
.chart-ticks {
  position: relative;
  height: 16px;
  margin-top: 3px;
}
.tick {
  position: absolute;
  top: 0;
  transform: translateX(-50%);
  font-size: 10px;
  color: var(--og-text-dim);
  font-family: var(--og-mono);
}
.chart-empty {
  display: grid;
  place-items: center;
  height: 150px;
  color: var(--og-text-dim);
  font-size: 12px;
}
.chart-tip {
  position: absolute;
  transform: translate(-50%, calc(-100% - 10px));
  min-width: 132px;
  background: var(--og-bg-raised);
  border: 1px solid var(--el-border-color-light);
  border-radius: 8px;
  padding: 8px 10px;
  font-size: 12px;
  box-shadow: var(--el-box-shadow);
  pointer-events: none;
  z-index: 5;
}
.tip-day {
  color: var(--og-text-dim);
  margin-bottom: 4px;
  font-size: 11px;
}
.tip-row {
  display: flex;
  justify-content: space-between;
  gap: 14px;
  line-height: 1.7;
}
.tip-row span {
  color: var(--og-text-dim);
}
.tip-row b {
  font-weight: 600;
  font-variant-numeric: tabular-nums;
}

.usage-foot {
  display: flex;
  justify-content: space-between;
  gap: 10px;
  flex-wrap: wrap;
  margin-top: 12px;
  padding-top: 10px;
  border-top: 1px solid var(--el-border-color-extra-light);
  color: var(--og-text-dim);
  font-size: 11px;
}

/* ---- 底部面板：列表行布局（el-table 宽列会把 1fr 栏撑爆出横向滚动） ---- */
/* 2×2 对齐网格：同行等高，列表区 flex 撑满卡片，矮的一侧不留卡外空白带 */
.sections {
  display: grid;
  grid-template-columns: minmax(0, 1fr) minmax(0, 1fr);
  gap: 14px;
}
.sections .card {
  display: flex;
  flex-direction: column;
}
.sections .row-list {
  flex: 1;
}
.row-list {
  max-height: 424px;
  overflow-y: auto;
}
.cluster-row,
.alert-row {
  display: flex;
  align-items: flex-start;
  gap: 10px;
  padding: 9px 8px;
  border-radius: 8px;
  cursor: pointer;
  transition: background 0.12s;
}
.cluster-row:hover,
.alert-row:hover {
  background: color-mix(in srgb, var(--og-accent) 6%, transparent);
}
.st-dot {
  flex: none;
  width: 8px;
  height: 8px;
  margin-top: 6px;
  border-radius: 50%;
}
.st-dot.ok {
  background: var(--el-color-success);
  box-shadow: 0 0 0 3px color-mix(in srgb, var(--el-color-success) 18%, transparent);
}
.st-dot.bad {
  background: var(--el-color-danger);
  box-shadow: 0 0 0 3px color-mix(in srgb, var(--el-color-danger) 18%, transparent);
}
.st-dot.warn {
  background: var(--el-color-warning);
  box-shadow: 0 0 0 3px color-mix(in srgb, var(--el-color-warning) 18%, transparent);
}
.st-dot.dim {
  background: var(--og-text-dim);
  box-shadow: 0 0 0 3px color-mix(in srgb, var(--og-text-dim) 15%, transparent);
}
.st-dot.accent {
  background: var(--og-accent);
  box-shadow: 0 0 0 3px var(--og-accent-soft);
}
/* 运行中状态：呼吸闪烁 */
.st-dot.pulse {
  animation: og-pulse 1.6s ease-in-out infinite;
}
@keyframes og-pulse {
  50% {
    opacity: 0.3;
  }
}
.row-main {
  flex: 1;
  min-width: 0;
}
.row-line {
  display: flex;
  align-items: center;
  gap: 10px;
  min-width: 0;
}
.row-title {
  font-size: 13px;
  font-weight: 600;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.row-note {
  flex: 1;
  min-width: 0;
  font-size: 12px;
  color: var(--og-text-dim);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.row-time {
  flex: none;
  font-size: 11px;
  color: var(--og-text-dim);
  font-variant-numeric: tabular-nums;
  white-space: nowrap;
}
.row-err {
  margin-top: 3px;
  font-size: 11px;
  color: var(--el-color-danger);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.alert-row.done .alert-title,
.alert-row.done .row-sub-line {
  color: var(--og-text-dim);
}
.alert-title {
  font-size: 13px;
  font-weight: 500;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.row-sub-line {
  display: flex;
  gap: 10px;
  margin-top: 2px;
  font-size: 11px;
  color: var(--og-text-dim);
  overflow: hidden;
  white-space: nowrap;
}
.alert-status {
  flex: none;
  display: inline-flex;
  align-items: center;
  gap: 5px;
  margin-top: 2px;
  font-size: 12px;
  white-space: nowrap;
}
.alert-status i {
  width: 6px;
  height: 6px;
  border-radius: 50%;
  background: currentColor;
}
.alert-status.bad {
  color: var(--el-color-danger);
}
.alert-status.warn {
  color: var(--el-color-warning);
}
.alert-status.ok {
  color: var(--el-color-success);
}
.alert-status.info {
  color: var(--og-text-dim);
}
.note-bad {
  color: var(--el-color-danger);
}
@media (max-width: 1100px) {
  .sections {
    grid-template-columns: minmax(0, 1fr);
  }
}
</style>
