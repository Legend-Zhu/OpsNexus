<template>
  <el-card shadow="never">
    <template #header>
      <div class="card-header">
        <span>系统设置</span>
      </div>
    </template>

    <el-tabs v-model="tab">
      <!-- 用户（用户管理 admin only；修改密码为自助操作，所有登录用户可用） -->
      <el-tab-pane v-if="isAdmin" label="用户" name="users">
        <el-table :data="users" empty-text="暂无用户">
          <el-table-column prop="username" label="用户名" min-width="140" />
          <el-table-column prop="role" label="角色" width="100">
            <template #default="{ row }">
              <el-tag size="small" :type="row.role === 'admin' ? 'danger' : 'info'">{{ row.role }}</el-tag>
            </template>
          </el-table-column>
          <el-table-column prop="enabled" label="启用" width="80">
            <template #default="{ row }">{{ row.enabled ? '是' : '否' }}</template>
          </el-table-column>
          <el-table-column label="创建时间" width="170">
            <template #default="{ row }">{{ new Date(row.created_at).toLocaleString() }}</template>
          </el-table-column>
          <el-table-column label="操作" width="110">
            <template #default="{ row }">
              <el-button link type="primary" @click="openReset(row)">重置密码</el-button>
            </template>
          </el-table-column>
        </el-table>
        <div class="user-actions">
          <el-button type="primary" :icon="Plus" @click="userVisible = true">新增用户</el-button>
          <el-button :icon="Key" @click="pwdVisible = true">修改密码</el-button>
        </div>
      </el-tab-pane>

      <!-- SSO -->
      <el-tab-pane label="SSO" name="sso">
        <el-alert type="info" :closable="false" class="mb"
          title="企业 SSO（OIDC 授权码流程）"
          description="通过 server.yaml 的 auth.sso.oidc 配置（issuer / client_id / client_secret / redirect_url / frontend_url）对接企业 SSO 网关。未配置时使用本地用户登录。" />
        <el-descriptions :column="1" border size="small">
          <el-descriptions-item label="SSO 模式">
            <el-tag size="small" :type="ssoStatus?.sso?.enabled ? 'success' : 'info'" effect="plain">
              {{ ssoStatus?.sso?.enabled ? '已启用（OIDC）' : '未配置' }}
            </el-tag>
          </el-descriptions-item>
          <el-descriptions-item v-if="ssoStatus?.sso?.enabled" label="Issuer" class-name="mono">
            {{ ssoStatus.sso.issuer }}
          </el-descriptions-item>
          <el-descriptions-item label="本地账号">
            <el-tag size="small" :type="ssoStatus?.local ? 'success' : 'info'" effect="plain">
              {{ ssoStatus?.local ? '已启用' : '未启用' }}
            </el-tag>
          </el-descriptions-item>
          <el-descriptions-item v-if="ssoStatus?.sso?.enabled" label="落地页">
            <span class="mono">{{ ssoStatus.sso.frontendUrl }}</span>
          </el-descriptions-item>
        </el-descriptions>
      </el-tab-pane>

      <!-- 身份提供者（OpsGaurd 作为 OIDC IdP，供其他系统接入） -->
      <el-tab-pane label="身份提供者" name="idp">
        <IdpClients />
      </el-tab-pane>

      <!-- AI 排查网关（启用 + 从模型池选默认模型 + 工具/Agent/MCP；
           模型池配置在「MLOps → 模型接入」） -->
      <el-tab-pane label="AI 排查网关" name="ainexus">
        <AINexusGateway />
      </el-tab-pane>

      <!-- 密钥（巡检 flow 拨测账号等 ${secret:} 引用） -->
      <el-tab-pane label="密钥" name="secrets">
        <Secrets />
      </el-tab-pane>
    </el-tabs>

    <!-- 新增用户对话框 -->
    <el-dialog v-model="userVisible" title="新增用户" width="400px">
      <el-form label-width="80px">
        <el-form-item label="用户名">
          <el-input v-model="userForm.username" />
        </el-form-item>
        <el-form-item label="密码">
          <el-input v-model="userForm.password" type="password" show-password />
        </el-form-item>
        <el-form-item label="角色">
          <el-select v-model="userForm.role" style="width: 100%">
            <el-option label="管理员" value="admin" />
            <el-option label="只读" value="viewer" />
          </el-select>
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="userVisible = false">取消</el-button>
        <el-button type="primary" :loading="savingUser" @click="saveUser">创建</el-button>
      </template>
    </el-dialog>

    <!-- 重置密码对话框（admin，免旧密码；对 SSO 用户重置即设置本地口令） -->
    <el-dialog v-model="resetVisible" :title="`重置密码：${resetTarget?.username ?? ''}`" width="420px" @closed="resetForm.confirm = ''">
      <el-form label-width="90px">
        <el-form-item label="新密码">
          <el-input v-model="resetForm.password" type="password" show-password />
        </el-form-item>
        <el-form-item label="确认新密码">
          <el-input v-model="resetForm.confirm" type="password" show-password />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="resetVisible = false">取消</el-button>
        <el-button type="primary" :loading="resetting" @click="saveReset">重置</el-button>
      </template>
    </el-dialog>

    <!-- 修改自己的密码（自助） -->
    <ChangePasswordDialog v-model:visible="pwdVisible" />
  </el-card>
</template>

<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { Key, Plus } from '@element-plus/icons-vue'
import { authApi } from '@/api'
import { isAdmin, loadAdminFlag } from '@/composables/admin'
import type { SSOStatus, User } from '@/types'
import AINexusGateway from './AINexusGateway.vue'
import Secrets from './Secrets.vue'
import IdpClients from './IdpClients.vue'
import ChangePasswordDialog from '@/components/ChangePasswordDialog.vue'

const tab = ref('users')
const users = ref<User[]>([])
const ssoStatus = ref<SSOStatus | null>(null)

const userVisible = ref(false)
const savingUser = ref(false)
const userForm = reactive({ username: '', password: '', role: 'viewer' })

const pwdVisible = ref(false)

const resetVisible = ref(false)
const resetting = ref(false)
const resetTarget = ref<User | null>(null)
const resetForm = reactive({ password: '', confirm: '' })

async function fetchAll() {
  try {
    ssoStatus.value = await authApi.ssoStatus()
  } catch {
    ssoStatus.value = null
  }
  // 用户列表 admin only（后端守卫；viewer 直接跳过请求避免 403 弹错）
  if (isAdmin.value) {
    const u = await authApi.users()
    users.value = u.items ?? []
  }
}

async function saveUser() {
  savingUser.value = true
  try {
    await authApi.createUser({ username: userForm.username, password: userForm.password, role: userForm.role })
    ElMessage.success('已创建')
    userVisible.value = false
    Object.assign(userForm, { username: '', password: '', role: 'viewer' })
    const u = await authApi.users()
    users.value = u.items ?? []
  } catch {
    // 错误已提示
  } finally {
    savingUser.value = false
  }
}

function openReset(row: User) {
  resetTarget.value = row
  Object.assign(resetForm, { password: '', confirm: '' })
  resetVisible.value = true
}

async function saveReset() {
  if (!resetTarget.value) return
  if (!resetForm.password) {
    ElMessage.warning('请输入新密码')
    return
  }
  if (resetForm.password !== resetForm.confirm) {
    ElMessage.warning('两次输入的新密码不一致')
    return
  }
  resetting.value = true
  try {
    await authApi.resetPassword(resetTarget.value.username, { new_password: resetForm.password })
    ElMessage.success(`已重置 ${resetTarget.value.username} 的密码`)
    resetVisible.value = false
  } catch {
    // 错误已由 http.ts 提示
  } finally {
    resetting.value = false
  }
}

onMounted(async () => {
  await loadAdminFlag()
  if (!isAdmin.value) tab.value = 'sso'
  await fetchAll()
})
</script>

<style scoped>
.card-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
}
.user-actions {
  display: flex;
  gap: 8px;
  margin-top: 12px;
}
.mb {
  margin-bottom: 12px;
}
</style>
