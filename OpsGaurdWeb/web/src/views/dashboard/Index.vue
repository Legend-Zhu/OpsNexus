<template>
  <div class="page">
    <div class="kpis">
      <div class="kpi">
        <span class="kpi-label">在线集群</span>
        <span class="kpi-value kpi-num">{{ onlineClusters }}</span>
        <span class="kpi-sub">共 {{ clusters.length }} 个集群</span>
      </div>
      <div class="kpi">
        <span class="kpi-label">未处理告警</span>
        <span class="kpi-value kpi-num warn">{{ activeAlerts }}</span>
        <span class="kpi-sub">需处理</span>
      </div>
      <div class="kpi">
        <span class="kpi-label">项目</span>
        <span class="kpi-value kpi-num">{{ projectCount }}</span>
        <span class="kpi-sub">管理分组</span>
      </div>
      <div class="kpi">
        <span class="kpi-label">巡检异常</span>
        <span class="kpi-value kpi-num">{{ patrolAnomalies }}</span>
        <span class="kpi-sub">最近一次执行</span>
      </div>
    </div>

    <div class="sections">
      <section class="card">
        <header class="card-head">
          <h3>集群健康</h3>
          <el-button link type="primary" @click="$router.push('/clusters')">全部集群 →</el-button>
        </header>
        <el-table :data="clusters" size="small" empty-text="暂无集群">
          <el-table-column label="集群" prop="name" min-width="140">
            <template #default="{ row }">
              <el-link type="primary" @click="$router.push(`/clusters/${row.name}`)">{{ row.name }}</el-link>
            </template>
          </el-table-column>
          <el-table-column label="状态" width="100">
            <template #default="{ row }">
              <el-tag size="small" :type="row.status === 'online' ? 'success' : 'danger'" effect="dark">
                {{ row.status === 'online' ? '在线' : '离线' }}
              </el-tag>
            </template>
          </el-table-column>
          <el-table-column label="Worker 端点" prop="worker_url" min-width="180" show-overflow-tooltip class-name="mono" />
          <el-table-column label="最近探测" width="160">
            <template #default="{ row }">{{ new Date(row.last_seen).toLocaleString() }}</template>
          </el-table-column>
          <el-table-column label="最近错误" min-width="180" show-overflow-tooltip>
            <template #default="{ row }">
              <span v-if="row.err" class="err">{{ row.err }}</span>
              <span v-else>—</span>
            </template>
          </el-table-column>
        </el-table>
      </section>

      <section class="card">
        <header class="card-head">
          <h3>最近告警</h3>
          <el-button link type="primary" @click="$router.push('/alerts')">告警中心 →</el-button>
        </header>
        <el-table :data="recentAlerts" size="small" empty-text="暂无告警">
          <el-table-column label="级别" width="70">
            <template #default="{ row }">
              <el-tag size="small" :type="row.level === 'error' ? 'danger' : row.level === 'warn' ? 'warning' : 'info'">
                {{ row.level }}
              </el-tag>
            </template>
          </el-table-column>
          <el-table-column label="集群/服务" min-width="150">
            <template #default="{ row }">{{ row.cluster }}/{{ row.service }}</template>
          </el-table-column>
          <el-table-column label="标题" prop="title" min-width="180" show-overflow-tooltip />
          <el-table-column label="状态" width="80">
            <template #default="{ row }">
              <el-tag size="small" effect="plain" :type="row.status === 'active' ? 'danger' : row.status === 'acked' ? 'warning' : 'success'">
                {{ row.status === 'active' ? '未处理' : row.status === 'acked' ? '已认领' : '已恢复' }}
              </el-tag>
            </template>
          </el-table-column>
        </el-table>
      </section>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { alertApi, clusterApi, patrolApi, projectApi } from '@/api'
import type { Alert, ClusterSummary } from '@/types'

const clusters = ref<ClusterSummary[]>([])
const alerts = ref<Alert[]>([])

// KPI 展示值（数字滚动从 0 递增）
const onlineClusters = ref(0)
const activeAlerts = ref(0)
const projectCount = ref(0)
const patrolAnomalies = ref(0)

const activeAlertsTotal = computed(() => alerts.value.filter((a) => a.status !== 'recovered').length)
const recentAlerts = computed(() => alerts.value.slice(0, 8))

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

async function loadAll() {
  try {
    const [c, a] = await Promise.all([clusterApi.list(), alertApi.list()])
    clusters.value = c.items ?? []
    alerts.value = a.items ?? []
  } catch {
    // 部分失败不阻断总览
  }
  let projects = 0
  try {
    const pl = await projectApi.list()
    projects = (pl.items ?? []).length
  } catch {
    projects = 0
  }
  try {
    const pl = await patrolApi.list()
    const first = (pl.items ?? [])[0]
    if (first) {
      const runs = await patrolApi.runs(first.id, 1)
      const last = (runs.items ?? [])[0]
      patrolAnomalies.value = (last?.anomalies ?? []).filter((x) => !x.ok).length
    }
  } catch {
    patrolAnomalies.value = 0
  }
  // 数据就绪后启动滚动（先定格目标值再动画）
  const goals = {
    online: clusters.value.filter((c) => c.status === 'online').length,
    alerts: activeAlertsTotal.value,
    projects,
    anomalies: patrolAnomalies.value,
  }
  animateTo(goals.online, onlineClusters)
  animateTo(goals.alerts, activeAlerts)
  animateTo(goals.projects, projectCount)
  animateTo(goals.anomalies, patrolAnomalies)
}

onMounted(loadAll)
</script>

<style scoped>
.kpis {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(200px, 1fr));
  gap: 14px;
  margin-bottom: 18px;
}
.kpi {
  background: var(--og-bg-surface);
  border: 1px solid var(--el-border-color-light);
  border-radius: 12px;
  padding: 16px 18px;
  display: flex;
  flex-direction: column;
  gap: 2px;
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
  color: #fbbf24;
}
.kpi-sub {
  font-size: 11px;
  color: var(--og-text-dim);
}
.sections {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 14px;
}
@media (max-width: 1100px) {
  .sections {
    grid-template-columns: 1fr;
  }
}
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
}
.card-head h3 {
  margin: 0;
  font-size: 14px;
  font-weight: 650;
}
.err {
  color: #f87171;
  font-size: 12px;
}
</style>
