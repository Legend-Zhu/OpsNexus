<template>
  <el-card shadow="never">
    <template #header>
      <div class="card-header">
        <span>告警中心</span>
        <div class="header-right">
          <el-select v-model="cluster" placeholder="全部集群" clearable style="width: 180px" @change="fetchAlerts">
            <el-option v-for="c in clusters" :key="c.name" :label="c.name" :value="c.name" />
          </el-select>
          <el-radio-group v-model="status" @change="fetchAlerts">
            <el-radio-button value="">全部</el-radio-button>
            <el-radio-button value="active">未处理</el-radio-button>
            <el-radio-button value="acked">已认领</el-radio-button>
            <el-radio-button value="recovered">已恢复</el-radio-button>
          </el-radio-group>
          <el-button :icon="Refresh" circle @click="fetchAlerts" />
        </div>
      </div>
    </template>

    <el-table v-loading="loading" :data="alerts" empty-text="暂无告警">
      <el-table-column label="级别" width="80">
        <template #default="{ row }">
          <el-tag size="small" :type="levelTag(row.level)">{{ row.level }}</el-tag>
        </template>
      </el-table-column>
      <el-table-column label="集群" prop="cluster" width="110" />
      <el-table-column label="服务" prop="service" width="120" />
      <el-table-column label="类型" width="140">
        <template #default="{ row }">
          <el-tag size="small" type="info">{{ typeText(row.type) }}</el-tag>
        </template>
      </el-table-column>
      <el-table-column label="标题" min-width="220" show-overflow-tooltip>
        <template #default="{ row }">
          <span>{{ row.title }}</span>
          <el-tag v-if="row.investigations" size="small" type="success" effect="plain" class="ml">
            已排查{{ row.investigations > 1 ? `×${row.investigations}` : '' }}
          </el-tag>
        </template>
      </el-table-column>
      <el-table-column label="次数" prop="count" width="70" align="center" />
      <el-table-column label="首次" width="160">
        <template #default="{ row }">{{ formatTime(row.first_ts) }}</template>
      </el-table-column>
      <el-table-column label="最近" width="160">
        <template #default="{ row }">{{ formatTime(row.last_ts) }}</template>
      </el-table-column>
      <el-table-column label="状态" width="90">
        <template #default="{ row }">
          <el-tag size="small" :type="statusTag(row.status)">{{ statusText(row.status) }}</el-tag>
        </template>
      </el-table-column>
      <el-table-column label="操作" width="200" fixed="right">
        <template #default="{ row }">
          <el-button link type="warning" @click="goTroubleshoot(row)">排查</el-button>
          <template v-if="row.status === 'active'">
            <el-button link type="primary" @click="ack(row)">认领</el-button>
            <el-button link type="success" @click="recover(row)">恢复</el-button>
          </template>
          <template v-else-if="row.status === 'acked'">
            <el-button link type="success" @click="recover(row)">恢复</el-button>
          </template>
        </template>
      </el-table-column>
    </el-table>
  </el-card>
</template>

<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { Refresh } from '@element-plus/icons-vue'
import { useRouter } from 'vue-router'
import { alertApi, clusterApi } from '@/api'
import type { Alert, ClusterSummary } from '@/types'

const router = useRouter()

const clusters = ref<ClusterSummary[]>([])
const cluster = ref('')
const status = ref('')
const alerts = ref<Alert[]>([])
const loading = ref(false)

function levelTag(l: string) {
  return l === 'error' ? 'danger' : l === 'warn' ? 'warning' : 'info'
}
function statusTag(s: string) {
  return s === 'active' ? 'danger' : s === 'acked' ? 'warning' : 'success'
}
function statusText(s: string) {
  return s === 'active' ? '未处理' : s === 'acked' ? '已认领' : '已恢复'
}
function typeText(t: string) {
  const map: Record<string, string> = {
    port_down: '端口不可达',
    http_unhealthy: 'HTTP 异常',
    log_match: '日志匹配',
    resource_over: '资源超限',
    resource_recovered: '资源恢复',
    patrol_failed: '巡检异常',
  }
  return map[t] ?? t
}
function formatTime(ts?: string) {
  if (!ts) return '—'
  return new Date(ts).toLocaleString()
}

async function fetchClusters() {
  const resp = await clusterApi.list()
  clusters.value = resp.items ?? []
}

async function fetchAlerts() {
  loading.value = true
  try {
    const params: { cluster?: string; status?: string } = {}
    if (cluster.value) params.cluster = cluster.value
    if (status.value) params.status = status.value
    const resp = await alertApi.list(params)
    alerts.value = resp.items ?? []
  } finally {
    loading.value = false
  }
}

async function ack(row: Alert) {
  await alertApi.ack(row.id)
  ElMessage.success('已认领')
  await fetchAlerts()
}

async function recover(row: Alert) {
  await alertApi.recover(row.id)
  ElMessage.success('已恢复')
  await fetchAlerts()
}

// 跳转对话式排查页并预选该告警
function goTroubleshoot(row: Alert) {
  router.push({ path: '/troubleshoot', query: { alert: row.id } })
}

onMounted(async () => {
  await fetchClusters()
  await fetchAlerts()
})
</script>

<style scoped>
.card-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
}
.header-right {
  display: flex;
  align-items: center;
  gap: 10px;
}
.muted {
  color: var(--el-text-color-placeholder);
}
</style>
