<template>
  <el-card shadow="never">
    <template #header>
      <div class="card-header">
        <span>集群管理（类 Rancher）</span>
        <el-button type="primary" :icon="Plus" :loading="adding" @click="openDialog">接入集群</el-button>
      </div>
    </template>

    <!-- 集群列表 -->
    <el-table v-loading="loading" :data="clusters" empty-text="暂无集群，点击右上角「接入集群」">
      <el-table-column label="名称" prop="name" min-width="140">
        <template #default="{ row }">
          <span class="cluster-name">{{ row.name }}</span>
        </template>
      </el-table-column>
      <el-table-column label="状态" width="120">
        <template #default="{ row }">
          <el-tag :type="statusTag(row.status)" size="small">{{ statusText(row.status) }}</el-tag>
        </template>
      </el-table-column>
      <el-table-column label="Worker 端点" prop="worker_url" min-width="200" show-overflow-tooltip />
      <el-table-column label="MCP 端点" prop="mcp_url" min-width="200" show-overflow-tooltip>
        <template #default="{ row }">{{ row.mcp_url || '—' }}</template>
      </el-table-column>
      <el-table-column label="描述" prop="desc" min-width="120" show-overflow-tooltip>
        <template #default="{ row }">{{ row.desc || '—' }}</template>
      </el-table-column>
      <el-table-column label="最近探测" width="170">
        <template #default="{ row }">{{ formatTime(row.last_seen) }}</template>
      </el-table-column>
      <el-table-column label="操作" width="120" fixed="right">
        <template #default="{ row }">
          <el-button link type="danger" :disabled="row.status !== 'offline'" @click="removeCluster(row)">
            移除
          </el-button>
        </template>
      </el-table-column>
    </el-table>

    <!-- 接入对话框 -->
    <el-dialog v-model="dialogVisible" title="接入集群" width="520px">
      <el-form ref="formRef" :model="form" :rules="rules" label-width="100px">
        <el-form-item label="集群名称" prop="name">
          <el-input v-model="form.name" placeholder="如 dev-cluster" />
        </el-form-item>
        <el-form-item label="Worker 地址" prop="worker_url">
          <el-input v-model="form.worker_url" placeholder="http://10.60.189.30:8080" />
        </el-form-item>
        <el-form-item label="MCP 地址">
          <el-input v-model="form.mcp_url" placeholder="http://10.60.189.30:8080/mcp（可选）" />
        </el-form-item>
        <el-form-item label="Token">
          <el-input v-model="form.token" type="password" show-password placeholder="Worker Bearer token（可选）" />
        </el-form-item>
        <el-form-item label="描述">
          <el-input v-model="form.desc" placeholder="可选" />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="dialogVisible = false">取消</el-button>
        <el-button type="primary" :loading="adding" @click="addCluster">接入</el-button>
      </template>
    </el-dialog>
  </el-card>
</template>

<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox, type FormInstance, type FormRules } from 'element-plus'
import { Plus } from '@element-plus/icons-vue'
import { clusterApi } from '@/api'
import type { AddClusterPayload, ClusterSummary } from '@/types'

const loading = ref(false)
const adding = ref(false)
const clusters = ref<ClusterSummary[]>([])

const dialogVisible = ref(false)
const formRef = ref<FormInstance>()
const form = reactive<AddClusterPayload>({ name: '', worker_url: '', mcp_url: '', token: '', desc: '' })

const rules: FormRules = {
  name: [{ required: true, message: '请输入集群名称', trigger: 'blur' }],
  worker_url: [{ required: true, message: '请输入 Worker 地址', trigger: 'blur' }],
}

function statusText(s: string) {
  return s === 'online' ? '在线' : s === 'offline' ? '离线' : '未知'
}
function statusTag(s: string) {
  return s === 'online' ? 'success' : s === 'offline' ? 'danger' : 'info'
}
function formatTime(ts?: string) {
  if (!ts) return '—'
  return new Date(ts).toLocaleString()
}

async function fetchClusters() {
  loading.value = true
  try {
    const resp = await clusterApi.list()
    clusters.value = resp.items ?? []
  } finally {
    loading.value = false
  }
}

function openDialog() {
  dialogVisible.value = true
}

async function addCluster() {
  await formRef.value?.validate()
  adding.value = true
  try {
    await clusterApi.add(form)
    ElMessage.success('集群接入成功')
    dialogVisible.value = false
    Object.assign(form, { name: '', worker_url: '', mcp_url: '', token: '', desc: '' })
    await fetchClusters()
  } catch {
    // 错误提示已由 http.ts 统一处理（探测失败 502 等）
  } finally {
    adding.value = false
  }
}

async function removeCluster(row: ClusterSummary) {
  await ElMessageBox.confirm(`确定移除集群「${row.name}」？此操作仅删除管理端注册记录。`, '移除集群', {
    type: 'warning',
  })
  await clusterApi.remove(row.name)
  ElMessage.success('已移除')
  await fetchClusters()
}

onMounted(fetchClusters)
</script>

<style scoped>
.card-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
}
.cluster-name {
  font-weight: 600;
}
</style>
