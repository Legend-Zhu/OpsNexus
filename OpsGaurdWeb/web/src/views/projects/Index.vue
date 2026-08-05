<template>
  <div class="page">
    <div class="page-head">
      <div>
        <h2 class="page-title">项目</h2>
        <p class="page-sub">管理层级：项目 → 集群 → 节点 → 容器 / 进程 / 中间件</p>
      </div>
      <el-button type="primary" :icon="Plus" @click="openCreate">新建项目</el-button>
    </div>

    <div v-loading="loading" class="proj-grid">
      <div v-for="v in views" :key="v.project.id" class="proj-card">
        <div class="proj-top">
          <span class="proj-name">{{ v.project.name }}</span>
          <el-tag size="small" effect="plain">{{ v.cluster_count }} 集群</el-tag>
        </div>
        <p v-if="v.project.desc" class="proj-desc">{{ v.project.desc }}</p>
        <p v-else class="proj-desc muted">—</p>

        <div v-if="v.clusters?.length" class="proj-clusters">
          <router-link
            v-for="c in v.clusters"
            :key="c.name"
            :to="`/clusters/${c.name}`"
            class="proj-cluster"
          >
            <span class="dot" :class="c.status" />
            {{ c.name }}
          </router-link>
        </div>
        <p v-else class="og-dim">尚未接入集群</p>

        <div class="proj-foot">
          <span class="og-dim">{{ new Date(v.project.created_at).toLocaleDateString() }}</span>
          <div>
            <el-button link type="primary" @click="openEdit(v.project)">编辑</el-button>
            <el-button link type="danger" @click="remove(v.project)">删除</el-button>
          </div>
        </div>
      </div>
      <el-empty v-if="!loading && !views.length" description="暂无项目，点击右上角「新建项目」" />
    </div>

    <el-dialog v-model="dialogVisible" :title="editing ? '编辑项目' : '新建项目'" width="420px">
      <el-form ref="formRef" :model="form" :rules="rules" label-width="80px">
        <el-form-item label="名称" prop="name">
          <el-input v-model="form.name" placeholder="如 核心交易域" />
        </el-form-item>
        <el-form-item label="描述">
          <el-input v-model="form.desc" type="textarea" :rows="2" placeholder="可选" />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="dialogVisible = false">取消</el-button>
        <el-button type="primary" :loading="saving" @click="save">保存</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox, type FormInstance, type FormRules } from 'element-plus'
import { Plus } from '@element-plus/icons-vue'
import { projectApi } from '@/api'
import type { Project, ProjectView } from '@/types'

const loading = ref(false)
const views = ref<ProjectView[]>([])

const dialogVisible = ref(false)
const editing = ref(false)
const saving = ref(false)
const formRef = ref<FormInstance>()
const form = reactive({ id: '', name: '', desc: '' })

const rules: FormRules = {
  name: [{ required: true, message: '请输入项目名称', trigger: 'blur' }],
}

async function fetchProjects() {
  loading.value = true
  try {
    const resp = await projectApi.list()
    views.value = resp.items ?? []
  } finally {
    loading.value = false
  }
}

function openCreate() {
  editing.value = false
  Object.assign(form, { id: '', name: '', desc: '' })
  dialogVisible.value = true
}

function openEdit(p: Project) {
  editing.value = true
  Object.assign(form, { id: p.id, name: p.name, desc: p.desc ?? '' })
  dialogVisible.value = true
}

async function save() {
  await formRef.value?.validate()
  saving.value = true
  try {
    if (editing.value) {
      await projectApi.update(form.id, { name: form.name, desc: form.desc })
      ElMessage.success('已更新')
    } else {
      await projectApi.create({ name: form.name, desc: form.desc })
      ElMessage.success('已创建')
    }
    dialogVisible.value = false
    await fetchProjects()
  } catch {
    // 错误已由 http.ts 提示
  } finally {
    saving.value = false
  }
}

async function remove(p: Project) {
  await ElMessageBox.confirm(`删除项目「${p.name}」？其下集群将解除归属（不删除集群）。`, '删除项目', {
    type: 'warning',
  })
  await projectApi.remove(p.id)
  ElMessage.success('已删除')
  await fetchProjects()
}

onMounted(fetchProjects)
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
.proj-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(300px, 1fr));
  gap: 14px;
}
.proj-card {
  background: var(--og-bg-surface);
  border: 1px solid var(--el-border-color-light);
  border-radius: 12px;
  padding: 16px;
  transition: border-color 0.15s, transform 0.15s;
}
.proj-card:hover {
  border-color: var(--og-accent);
  transform: translateY(-2px);
}
.proj-top {
  display: flex;
  align-items: center;
  justify-content: space-between;
}
.proj-name {
  font-size: 15px;
  font-weight: 650;
}
.proj-desc {
  margin: 6px 0 12px;
  font-size: 12px;
  color: var(--el-text-color-regular);
  min-height: 18px;
}
.proj-clusters {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
  margin-bottom: 12px;
}
.proj-cluster {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  padding: 2px 9px;
  border-radius: 99px;
  background: var(--el-fill-color);
  font-size: 12px;
  color: var(--el-text-color-regular);
  text-decoration: none;
}
.proj-cluster:hover {
  color: var(--og-accent-strong);
}
.dot {
  width: 7px;
  height: 7px;
  border-radius: 50%;
}
.dot.online {
  background: #34d399;
}
.dot.offline {
  background: #f87171;
}
.dot.unknown {
  background: #94a3b8;
}
.proj-foot {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding-top: 10px;
  border-top: 1px solid var(--el-border-color-lighter);
}
.muted {
  color: var(--og-text-dim);
}
</style>
