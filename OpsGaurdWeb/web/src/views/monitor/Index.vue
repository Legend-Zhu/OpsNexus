<template>
  <el-card shadow="never">
    <template #header>
      <div class="card-header">
        <span>集群监控</span>
        <div class="header-right">
          <el-select v-model="cluster" placeholder="选择集群" style="width: 220px" @change="fetchMetrics">
            <el-option v-for="c in clusters" :key="c.name" :label="c.name" :value="c.name" />
          </el-select>
          <el-button :icon="Refresh" circle @click="fetchMetrics" />
        </div>
      </div>
    </template>

    <template v-if="cluster">
      <h4>节点：{{ stats?.node || '—' }}</h4>
      <el-table v-loading="loading" :data="stats?.containers ?? []" empty-text="该集群无运行容器">
        <el-table-column label="容器 ID" prop="containerId" min-width="150" show-overflow-tooltip />
        <el-table-column label="服务" prop="service" min-width="120">
          <template #default="{ row }">{{ row.service || '—' }}</template>
        </el-table-column>
        <el-table-column label="CPU %" width="110">
          <template #default="{ row }">
            <el-progress :percentage="Math.min(100, row.cpuPercent)" :stroke-width="8" />
          </template>
        </el-table-column>
        <el-table-column label="内存 %" width="110">
          <template #default="{ row }">
            <el-progress
              :percentage="Math.min(100, row.memPercent)"
              :stroke-width="8"
              :status="row.memPercent > 85 ? 'exception' : undefined"
            />
          </template>
        </el-table-column>
        <el-table-column label="内存使用" width="130">
          <template #default="{ row }">{{ fmtBytes(row.memUsageBytes) }} / {{ fmtBytes(row.memLimitBytes) }}</template>
        </el-table-column>
      </el-table>
    </template>
    <el-empty v-else description="请选择集群查看节点资源" />
  </el-card>
</template>

<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { Refresh } from '@element-plus/icons-vue'
import { clusterApi, monitorApi } from '@/api'
import type { ClusterSummary, NodeStats } from '@/types'

const clusters = ref<ClusterSummary[]>([])
const cluster = ref('')
const stats = ref<NodeStats | null>(null)
const loading = ref(false)

function fmtBytes(n?: number) {
  if (!n) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let v = n
  let i = 0
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return `${v.toFixed(1)} ${units[i]}`
}

async function fetchClusters() {
  const resp = await clusterApi.list()
  clusters.value = resp.items ?? []
  if (!cluster.value && clusters.value.length) {
    cluster.value = clusters.value[0].name
    await fetchMetrics()
  }
}

async function fetchMetrics() {
  if (!cluster.value) return
  loading.value = true
  try {
    stats.value = await monitorApi.metrics(cluster.value)
  } finally {
    loading.value = false
  }
}

onMounted(fetchClusters)
</script>

<style scoped>
.card-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
}
.header-right {
  display: flex;
  gap: 8px;
}
</style>
