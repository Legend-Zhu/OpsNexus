<template>
  <div>
    <el-alert type="info" :closable="false" class="mb"
      title="密钥供巡检 flow 检查等场景引用（YAML 中写 ${secret:名称}，执行时服务端解析）"
      description="密钥值只在写入时提交，列表/接口永不回显；覆盖同名即更新。" />

    <el-table :data="secrets" size="small" v-loading="loading" empty-text="暂无密钥">
      <el-table-column prop="name" label="名称" min-width="180">
        <template #default="{ row }"><span class="mono">{{ row.name }}</span></template>
      </el-table-column>
      <el-table-column label="值" min-width="120">
        <template #default><span class="muted">••••••••</span></template>
      </el-table-column>
      <el-table-column label="更新时间" width="180">
        <template #default="{ row }">{{ new Date(row.updated_at).toLocaleString() }}</template>
      </el-table-column>
      <el-table-column label="操作" width="90">
        <template #default="{ row }">
          <el-button link type="danger" @click="remove(row)">删除</el-button>
        </template>
      </el-table-column>
    </el-table>
    <el-button class="mt" type="primary" :icon="Plus" @click="openCreate">新增密钥</el-button>

    <el-dialog v-model="visible" :title="editing ? '更新密钥' : '新增密钥'" width="420px">
      <el-form label-width="80px">
        <el-form-item label="名称" required>
          <el-input v-model="form.name" :disabled="editing" placeholder="如 patrol-login（字母数字._-）" />
        </el-form-item>
        <el-form-item label="值" required>
          <el-input v-model="form.value" type="password" show-password :placeholder="editing ? '输入新值覆盖' : '密钥值'" />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="visible = false">取消</el-button>
        <el-button type="primary" :loading="saving" @click="save">保存</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Plus } from '@element-plus/icons-vue'
import { secretApi } from '@/api'
import type { Secret } from '@/types'

const secrets = ref<Secret[]>([])
const loading = ref(false)
const visible = ref(false)
const editing = ref(false)
const saving = ref(false)
const form = reactive({ name: '', value: '' })

async function load() {
  loading.value = true
  try {
    const resp = await secretApi.list()
    secrets.value = resp.items ?? []
  } catch {
    secrets.value = []
  } finally {
    loading.value = false
  }
}

function openCreate() {
  editing.value = false
  Object.assign(form, { name: '', value: '' })
  visible.value = true
}

async function save() {
  if (!/^[a-zA-Z0-9_.-]+$/.test(form.name)) {
    ElMessage.warning('名称仅允许字母、数字、_ . -')
    return
  }
  if (!form.value) {
    ElMessage.warning('请输入密钥值')
    return
  }
  saving.value = true
  try {
    await secretApi.save(form.name, form.value)
    ElMessage.success('已保存')
    visible.value = false
    await load()
  } catch {
    /* 错误已提示 */
  } finally {
    saving.value = false
  }
}

async function remove(row: Secret) {
  await ElMessageBox.confirm(`删除密钥「${row.name}」？引用它的巡检将执行失败。`, '删除密钥', { type: 'warning' })
  await secretApi.remove(row.name)
  ElMessage.success('已删除')
  await load()
}

onMounted(load)
</script>

<style scoped>
.mb {
  margin-bottom: 12px;
}
.mt {
  margin-top: 12px;
}
.mono {
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  font-size: 12px;
}
.muted {
  color: var(--el-text-color-placeholder);
}
</style>
