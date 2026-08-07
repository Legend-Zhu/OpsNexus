<template>
  <div class="page">
    <div class="page-head">
      <div>
        <h2 class="page-title">集群</h2>
        <p class="page-sub">纳管的 Docker Swarm 集群 · 每个集群经其 Manager Worker 统一编排、监控与对外提供数据</p>
      </div>
      <el-button type="primary" :icon="Plus" :loading="adding" @click="openDialog">接入集群</el-button>
    </div>

    <el-table v-loading="loading" :data="clusters" empty-text="暂无集群，点击右上角「接入集群」">
      <el-table-column label="名称" min-width="150">
        <template #default="{ row }">
          <el-link type="primary" @click="$router.push(`/clusters/${row.name}`)">
            <span class="cluster-name">{{ row.name }}</span>
          </el-link>
          <span v-if="row.desc" class="cluster-desc">{{ row.desc }}</span>
        </template>
      </el-table-column>
      <el-table-column label="项目" width="130">
        <template #default="{ row }">
          <el-tag v-if="projectName(row)" size="small" effect="plain">{{ projectName(row) }}</el-tag>
          <span v-else class="muted">—</span>
        </template>
      </el-table-column>
      <el-table-column label="状态" width="100">
        <template #default="{ row }">
          <el-tooltip :disabled="!row.err" :content="row.err" placement="top">
            <el-tag :type="statusTag(row.status)" size="small" effect="dark">{{ statusText(row.status) }}</el-tag>
          </el-tooltip>
        </template>
      </el-table-column>
      <el-table-column label="Manager 端点" prop="worker_url" min-width="200" show-overflow-tooltip />
      <el-table-column label="最近探测" width="170">
        <template #default="{ row }">{{ formatTime(row.last_seen) }}</template>
      </el-table-column>
      <el-table-column label="操作" width="160" fixed="right">
        <template #default="{ row }">
          <el-button link type="primary" @click="$router.push(`/clusters/${row.name}`)">详情</el-button>
          <el-button link type="primary" @click="openEdit(row)">编辑</el-button>
          <el-button link type="danger" :disabled="row.status !== 'offline'" @click="removeCluster(row)">
            移除
          </el-button>
        </template>
      </el-table-column>
    </el-table>

    <!-- 接入/编辑对话框 -->
    <el-dialog v-model="dialogVisible" :title="editing ? '编辑集群' : '接入集群'" width="640px">
      <el-form ref="formRef" :model="form" :rules="rules" label-width="100px">
        <el-form-item label="集群名称" prop="name">
          <el-input v-model="form.name" placeholder="如 dev-cluster" :disabled="editing" />
        </el-form-item>
        <el-form-item label="所属项目">
          <el-select v-model="form.project_id" clearable placeholder="可选" style="width: 100%">
            <el-option v-for="p in projects" :key="p.id" :label="p.name" :value="p.id" />
          </el-select>
        </el-form-item>
        <el-form-item label="Manager 地址" prop="worker_url">
          <el-input v-model="form.worker_url" placeholder="http://&lt;管理节点IP&gt;:9080" />
          <div class="form-tip">Worker 的 gRPC 管理端口（默认 9080）；MCP 端点自动取 HTTP 端口 /mcp</div>
        </el-form-item>
        <el-form-item label="Token">
          <el-input v-model="form.token" type="password" show-password :placeholder="editing && !editingHasToken ? '尚未配置（Worker 未开启鉴权）' : editing ? '已配置，留空不修改' : 'Worker Bearer token（可选）'" />
          <div v-if="editing" class="form-tip">
            <span v-if="editingHasToken" style="color: #67c23a">✓ 已配置 token</span>
            <span v-else style="color: #e6a23c">⚠ 未配置 token——Worker 未开启鉴权，任何能访问 gRPC 端口的人均可操作集群</span>
          </div>
          <div v-else class="form-tip">Worker 开启鉴权（auth.enabled）时填写；未配置则 Worker 不校验请求方身份</div>
        </el-form-item>
        <el-form-item label="描述">
          <el-input v-model="form.desc" placeholder="可选" />
        </el-form-item>
        <div class="form-tip">接入后可到集群详情页「纳管配置」声明外部纳管对象（standalone 容器 / 宿主机服务）</div>
      </el-form>
      <template #footer>
        <el-button @click="dialogVisible = false">取消</el-button>
        <el-button type="primary" :loading="adding" @click="submitCluster">{{ editing ? '保存' : '接入' }}</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox, type FormInstance, type FormRules } from 'element-plus'
import { Plus } from '@element-plus/icons-vue'
import { clusterApi, projectApi } from '@/api'
import type { AddClusterPayload, ClusterSummary, Project } from '@/types'

const loading = ref(false)
const adding = ref(false)
const clusters = ref<ClusterSummary[]>([])
const projects = ref<Project[]>([])

const dialogVisible = ref(false)
const editing = ref(false)
const editingHasToken = ref(false)
const formRef = ref<FormInstance>()
const form = reactive<AddClusterPayload>({ name: '', project_id: '', worker_url: '', token: '', desc: '' })

const rules: FormRules = {
  name: [
    { required: true, message: '请输入集群名称', trigger: 'blur' },
    {
      pattern: /^[a-zA-Z0-9][a-zA-Z0-9._-]{0,62}$/,
      message: '字母/数字开头，可含 . _ -，最长 63 字符',
      trigger: 'blur',
    },
  ],
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
function projectName(row: ClusterSummary) {
  return projects.value.find((p) => p.id === row.project_id)?.name
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

async function fetchProjects() {
  try {
    const resp = await projectApi.list()
    projects.value = (resp.items ?? []).map((v) => v.project)
  } catch {
    projects.value = []
  }
}

function openDialog() {
  editing.value = false
  Object.assign(form, { name: '', project_id: '', worker_url: '', token: '', desc: '' })
  dialogVisible.value = true
}

function openEdit(row: ClusterSummary) {
  editing.value = true
  editingHasToken.value = !!row.has_token
  Object.assign(form, {
    name: row.name,
    project_id: row.project_id ?? '',
    worker_url: row.worker_url,
    token: '',
    desc: row.desc ?? '',
  })
  dialogVisible.value = true
}

async function submitCluster() {
  await formRef.value?.validate()
  adding.value = true
  try {
    if (editing.value) {
      await clusterApi.update(form.name, form)
      ElMessage.success('集群已更新')
    } else {
      await clusterApi.add(form)
      ElMessage.success('集群接入成功')
    }
    dialogVisible.value = false
    Object.assign(form, { name: '', project_id: '', worker_url: '', token: '', desc: '' })
    await fetchClusters()
  } catch {
    // 错误提示已由 http.ts 统一处理（探测失败 502 等）
  } finally {
    adding.value = false
  }
}

async function removeCluster(row: ClusterSummary) {
  try {
    await ElMessageBox.confirm(`确定移除集群「${row.name}」？此操作仅删除管理端注册记录。`, '移除集群', {
      type: 'warning',
    })
  } catch {
    return // 用户取消
  }
  await clusterApi.remove(row.name)
  ElMessage.success('已移除')
  await fetchClusters()
}

onMounted(async () => {
  await Promise.all([fetchClusters(), fetchProjects()])
})
</script>

<style scoped>
.page-head {
  display: flex;
  align-items: flex-end;
  justify-content: space-between;
  margin-bottom: 16px;
}
.page-title {
  margin: 0;
  font-size: 20px;
  font-weight: 700;
  letter-spacing: -0.02em;
}
.page-sub {
  margin: 4px 0 0;
  color: var(--og-text-dim);
  font-size: 12px;
}
.cluster-name {
  font-weight: 600;
}
.cluster-desc {
  margin-left: 8px;
  color: var(--og-text-dim);
  font-size: 12px;
}
.muted {
  color: var(--og-text-dim);
}
.form-tip {
  color: var(--og-text-dim);
  font-size: 12px;
  line-height: 1.6;
}
</style>
