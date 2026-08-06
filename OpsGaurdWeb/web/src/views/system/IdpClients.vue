<template>
  <el-alert type="info" :closable="false" class="mb"
    title="身份提供者（OpsGaurd 作为 OIDC IdP）"
    description="其他系统（企业内业务应用、Worker/MCP 工具）可跳转 /api/v1/idp/authorize 到本系统完成认证。在此注册 client（依赖方），对接方按 OIDC 授权码 + PKCE 接入（discovery: /.well-known/openid-configuration）。需 server.yaml 配 idp.enabled 启用。" />

  <el-table :data="clients" empty-text="尚未注册 client（或 IdP 未启用）" v-loading="loading">
    <el-table-column prop="name" label="名称" min-width="130" />
    <el-table-column prop="id" label="Client ID" min-width="180">
      <template #default="{ row }"><span class="mono">{{ row.id }}</span></template>
    </el-table-column>
    <el-table-column label="回调地址" min-width="220">
      <template #default="{ row }">
        <div v-for="u in row.redirect_uris" :key="u" class="mono small">{{ u }}</div>
      </template>
    </el-table-column>
    <el-table-column label="类型" width="90">
      <template #default="{ row }">
        <el-tag size="small" :type="row.public ? 'warning' : 'success'">
          {{ row.public ? '公共(PKCE)' : '机密' }}
        </el-tag>
      </template>
    </el-table-column>
    <el-table-column label="Scopes" min-width="120">
      <template #default="{ row }">
        <el-tag v-for="s in row.scopes ?? []" :key="s" size="small" class="mr">{{ s }}</el-tag>
      </template>
    </el-table-column>
    <el-table-column label="创建时间" width="160">
      <template #default="{ row }">{{ new Date(row.created_at).toLocaleString() }}</template>
    </el-table-column>
    <el-table-column label="操作" width="200" fixed="right">
      <template #default="{ row }">
        <el-button link type="primary" @click="openEdit(row)">编辑</el-button>
        <el-button v-if="!row.public" link type="warning" @click="rotateSecret(row)">轮换密钥</el-button>
        <el-button link type="danger" @click="removeClient(row)">删除</el-button>
      </template>
    </el-table-column>
  </el-table>
  <el-button class="mt" type="primary" :icon="Plus" @click="openCreate">注册 Client</el-button>

  <!-- 创建/编辑对话框 -->
  <el-dialog v-model="formVisible" :title="editing ? '编辑 Client' : '注册 Client'" width="560px">
    <el-form label-width="110px" :model="form">
      <el-form-item label="名称" required>
        <el-input v-model="form.name" placeholder="如 biz-app / mcp-worker" :disabled="!!editing" />
      </el-form-item>
      <el-form-item label="类型">
        <el-switch v-model="form.public" active-text="公共（PKCE，无 secret）" inactive-text="机密（client_secret）"
          :disabled="!!editing" />
        <div class="muted small">公共客户端用于无法安全保存密钥的场景（MCP 工具、SPA）；机密客户端用于后端服务。</div>
      </el-form-item>
      <el-form-item label="回调地址" required>
        <div v-for="(_, i) in form.redirect_uris" :key="i" class="uri-row">
          <el-input v-model="form.redirect_uris[i]" placeholder="https://your-app.example.com/cb" class="uri-input" />
          <el-button v-if="form.redirect_uris.length > 1" link type="danger" @click="form.redirect_uris.splice(i, 1)">移除</el-button>
        </div>
        <el-button link type="primary" :icon="Plus" @click="form.redirect_uris.push('')">添加回调地址</el-button>
      </el-form-item>
      <el-form-item label="允许 Scope">
        <el-select v-model="form.scopes" multiple placeholder="留空 = openid profile email" class="block">
          <el-option v-for="s in SCOPE_OPTIONS" :key="s" :label="s" :value="s" />
        </el-select>
      </el-form-item>
      <el-form-item label="Token TTL">
        <el-input v-model="form.token_ttl" placeholder="如 1h；留空用全局默认" />
      </el-form-item>
    </el-form>
    <template #footer>
      <el-button @click="formVisible = false">取消</el-button>
      <el-button type="primary" :loading="saving" @click="save">保存</el-button>
    </template>
  </el-dialog>

  <!-- 创建/轮换后的密钥一次性展示 -->
  <el-dialog v-model="secretVisible" title="Client Secret（仅此一次可见）" width="520px">
    <el-alert type="warning" :closable="false" title="请立即复制并妥善保管，关闭后无法再次查看。" class="mb" />
    <el-input :model-value="shownSecret" readonly>
      <template #append>
        <el-button @click="copySecret">复制</el-button>
      </template>
    </el-input>
    <template #footer>
      <el-button type="primary" @click="secretVisible = false">我已保存</el-button>
    </template>
  </el-dialog>
</template>

<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Plus } from '@element-plus/icons-vue'
import { idpApi } from '@/api'
import type { IdpClient } from '@/types'

const SCOPE_OPTIONS = ['openid', 'profile', 'email']

const clients = ref<IdpClient[]>([])
const loading = ref(false)
const formVisible = ref(false)
const editing = ref<IdpClient | null>(null)
const saving = ref(false)
const secretVisible = ref(false)
const shownSecret = ref('')

const form = reactive<{
  name: string
  public: boolean
  redirect_uris: string[]
  scopes: string[]
  token_ttl: string
}>({
  name: '',
  public: false,
  redirect_uris: [''],
  scopes: [],
  token_ttl: '',
})

async function load() {
  loading.value = true
  try {
    const res = await idpApi.listClients()
    clients.value = res.items ?? []
  } catch (e: any) {
    // IdP 未启用时后端返回 503；静默提示
    ElMessage.warning(e?.response?.data?.message ?? '无法加载 client（IdP 可能未启用）')
    clients.value = []
  } finally {
    loading.value = false
  }
}

function openCreate() {
  editing.value = null
  form.name = ''
  form.public = false
  form.redirect_uris = ['']
  form.scopes = []
  form.token_ttl = ''
  formVisible.value = true
}

function openEdit(c: IdpClient) {
  editing.value = c
  form.name = c.name
  form.public = c.public
  form.redirect_uris = [...c.redirect_uris]
  form.scopes = [...(c.scopes ?? [])]
  form.token_ttl = c.token_ttl ?? ''
  formVisible.value = true
}

async function save() {
  if (!form.name.trim()) {
    ElMessage.warning('请填写名称')
    return
  }
  const uris = form.redirect_uris.map((u) => u.trim()).filter(Boolean)
  if (uris.length === 0) {
    ElMessage.warning('至少一个回调地址')
    return
  }
  saving.value = true
  try {
    if (editing.value) {
      const updated = await idpApi.updateClient(editing.value.id, {
        name: form.name,
        redirect_uris: uris,
        scopes: form.scopes,
        token_ttl: form.token_ttl || undefined,
      })
      const idx = clients.value.findIndex((c) => c.id === updated.id)
      if (idx >= 0) clients.value[idx] = updated
      ElMessage.success('已更新')
    } else {
      const res = await idpApi.createClient({
        name: form.name,
        public: form.public,
        redirect_uris: uris,
        scopes: form.scopes,
        token_ttl: form.token_ttl || undefined,
      })
      clients.value.push(res.client)
      if (!form.public && res.secret) {
        shownSecret.value = res.secret
        secretVisible.value = true
      }
      ElMessage.success('已创建')
    }
    formVisible.value = false
  } catch (e: any) {
    ElMessage.error(e?.response?.data?.message ?? '保存失败')
  } finally {
    saving.value = false
  }
}

async function rotateSecret(c: IdpClient) {
  try {
    await ElMessageBox.confirm(`确认轮换 client「${c.name}」的密钥？旧密钥立即失效。`, '轮换密钥', { type: 'warning' })
  } catch {
    return
  }
  try {
    const res = await idpApi.rotateSecret(c.id)
    shownSecret.value = res.secret
    secretVisible.value = true
    ElMessage.success('密钥已轮换')
  } catch (e: any) {
    ElMessage.error(e?.response?.data?.message ?? '轮换失败')
  }
}

async function removeClient(c: IdpClient) {
  try {
    await ElMessageBox.confirm(`确认删除 client「${c.name}」？依赖它的系统将无法再认证。`, '删除', { type: 'warning' })
  } catch {
    return
  }
  try {
    await idpApi.deleteClient(c.id)
    clients.value = clients.value.filter((x) => x.id !== c.id)
    ElMessage.success('已删除')
  } catch (e: any) {
    ElMessage.error(e?.response?.data?.message ?? '删除失败')
  }
}

function copySecret() {
  navigator.clipboard?.writeText(shownSecret.value).then(
    () => ElMessage.success('已复制'),
    () => ElMessage.warning('复制失败，请手动选中复制'),
  )
}

onMounted(load)
</script>

<style scoped>
.mono {
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
  font-size: 0.85em;
}
.small {
  font-size: 12px;
}
.muted {
  color: var(--el-text-color-secondary);
}
.uri-row {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 6px;
}
.uri-input {
  flex: 1;
}
.block {
  width: 100%;
}
.mr {
  margin-right: 4px;
}
.mb {
  margin-bottom: 12px;
}
.mt {
  margin-top: 12px;
}
</style>
