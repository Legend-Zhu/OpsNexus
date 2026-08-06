<template>
  <el-card shadow="never">
    <template #header>
      <div class="card-header">
        <span>系统设置</span>
      </div>
    </template>

    <el-tabs v-model="tab">
      <!-- 告警规则（管理 Worker monitoring config，决策⑦） -->
      <el-tab-pane label="告警规则" name="rules">
        <el-table :data="rules" empty-text="暂无告警规则">
          <el-table-column prop="cluster" label="集群" width="110" />
          <el-table-column prop="service" label="服务" width="120" />
          <el-table-column label="端口探测" min-width="120">
            <template #default="{ row }">
              <el-tag v-for="p in row.monitoring?.portChecks ?? []" :key="p.port" size="small" class="mr">
                :{{ p.port }}
              </el-tag>
              <span v-if="!row.monitoring?.portChecks?.length" class="muted">—</span>
            </template>
          </el-table-column>
          <el-table-column label="HTTP 检查" min-width="120">
            <template #default="{ row }">
              <el-tag v-for="h in row.monitoring?.httpChecks ?? []" :key="h.url" size="small" class="mr">
                {{ h.method ?? 'GET' }} {{ h.url }}
              </el-tag>
              <span v-if="!row.monitoring?.httpChecks?.length" class="muted">—</span>
            </template>
          </el-table-column>
          <el-table-column label="资源阈值" min-width="130">
            <template #default="{ row }">
              <el-tag v-for="r in row.monitoring?.resourceThresholds ?? []" :key="r.metric" size="small" type="warning" class="mr">
                {{ r.metric }}>{{ r.threshold }}%
              </el-tag>
              <span v-if="!row.monitoring?.resourceThresholds?.length" class="muted">—</span>
            </template>
          </el-table-column>
          <el-table-column label="操作" width="170" fixed="right">
            <template #default="{ row }">
              <el-button link type="primary" @click="openRule(row)">编辑</el-button>
              <el-button link type="success" :loading="applying === `${row.cluster}/${row.service}`" @click="applyRule(row)">下发</el-button>
              <el-button link type="danger" @click="removeRule(row)">删除</el-button>
            </template>
          </el-table-column>
        </el-table>
        <el-button class="mt" type="primary" :icon="Plus" @click="openRule()">新增规则</el-button>
      </el-tab-pane>

      <!-- 用户 -->
      <el-tab-pane label="用户" name="users">
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
        </el-table>
        <el-button class="mt" type="primary" :icon="Plus" @click="userVisible = true">新增用户</el-button>
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

      <!-- 模型配置（模型池：provider + 模型清单，唯一配置模型的地方） -->
      <el-tab-pane label="模型配置" name="model-config">
        <ModelConfig />
      </el-tab-pane>

      <!-- AI 排查网关（启用 + 从模型池选默认模型 + 工具/Agent/MCP） -->
      <el-tab-pane label="AI 排查网关" name="ainexus">
        <AINexusGateway />
      </el-tab-pane>

      <!-- 巡检报告（生成后的渠道投递策略） -->
      <el-tab-pane label="巡检报告" name="patrol-report">
        <PatrolReport />
      </el-tab-pane>

      <!-- 密钥（巡检 flow 拨测账号等 ${secret:} 引用） -->
      <el-tab-pane label="密钥" name="secrets">
        <Secrets />
      </el-tab-pane>
    </el-tabs>

    <!-- 规则对话框 -->
    <el-dialog v-model="ruleVisible" title="编辑告警规则" width="620px">
      <el-form label-width="90px">
        <el-form-item label="集群">
          <el-select v-model="ruleForm.cluster" style="width: 100%" filterable>
            <el-option v-for="c in clusters" :key="c.name" :label="c.name" :value="c.name" />
          </el-select>
        </el-form-item>
        <el-form-item label="服务">
          <el-input v-model="ruleForm.service" placeholder="服务名，如 web" />
        </el-form-item>
        <el-form-item label="CPU 阈值 %">
          <el-input-number v-model="ruleForm.cpuThreshold" :min="0" :max="100" />
          <span class="muted ml">0 = 不检查</span>
        </el-form-item>
        <el-form-item label="内存阈值 %">
          <el-input-number v-model="ruleForm.memThreshold" :min="0" :max="100" />
          <span class="muted ml">0 = 不检查</span>
        </el-form-item>
        <el-form-item label="端口探测">
          <el-input v-model="ruleForm.ports" placeholder="逗号分隔，如 8080,9090（空 = 不检查）" />
        </el-form-item>
        <el-form-item label="HTTP 检查">
          <el-input v-model="ruleForm.httpUrl" placeholder="如 http://service:8080/health（空 = 不检查）" />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="ruleVisible = false">取消</el-button>
        <el-button type="primary" :loading="savingRule" @click="saveRule">保存</el-button>
      </template>
    </el-dialog>

    <!-- 用户对话框 -->
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
  </el-card>
</template>

<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Plus } from '@element-plus/icons-vue'
import { alertRuleApi, authApi, clusterApi } from '@/api'
import type { AlertRule, ClusterSummary, SSOStatus, User } from '@/types'
import AINexusGateway from './AINexusGateway.vue'
import ModelConfig from './ModelConfig.vue'
import PatrolReport from './PatrolReport.vue'
import Secrets from './Secrets.vue'

const tab = ref('rules')
const rules = ref<AlertRule[]>([])
const users = ref<User[]>([])
const clusters = ref<ClusterSummary[]>([])
const ssoStatus = ref<SSOStatus | null>(null)
const applying = ref('')

const ruleVisible = ref(false)
const savingRule = ref(false)
const ruleForm = reactive({
  cluster: '', service: '', cpuThreshold: 0, memThreshold: 0, ports: '', httpUrl: '',
})

const userVisible = ref(false)
const savingUser = ref(false)
const userForm = reactive({ username: '', password: '', role: 'viewer' })

async function fetchAll() {
  const [r, u, c] = await Promise.all([alertRuleApi.list(), authApi.users(), clusterApi.list()])
  rules.value = r.items ?? []
  users.value = u.items ?? []
  clusters.value = c.items ?? []
  try {
    ssoStatus.value = await authApi.ssoStatus()
  } catch {
    ssoStatus.value = null
  }
}

function openRule(row?: AlertRule) {
  Object.assign(ruleForm, {
    cluster: row?.cluster ?? '', service: row?.service ?? '',
    cpuThreshold: row?.monitoring?.resourceThresholds?.find((x) => x.metric === 'cpu')?.threshold ?? 0,
    memThreshold: row?.monitoring?.resourceThresholds?.find((x) => x.metric === 'memory')?.threshold ?? 0,
    ports: (row?.monitoring?.portChecks ?? []).map((p) => p.port).join(','),
    httpUrl: row?.monitoring?.httpChecks?.[0]?.url ?? '',
  })
  ruleVisible.value = true
}

function buildMonitoring() {
  const monitoring: AlertRule['monitoring'] = {}
  const thresholds = []
  if (ruleForm.cpuThreshold > 0) thresholds.push({ metric: 'cpu' as const, threshold: ruleForm.cpuThreshold })
  if (ruleForm.memThreshold > 0) thresholds.push({ metric: 'memory' as const, threshold: ruleForm.memThreshold })
  if (thresholds.length) monitoring.resourceThresholds = thresholds
  const ports = ruleForm.ports.split(',').map((s) => s.trim()).filter(Boolean)
  if (ports.length) monitoring.portChecks = ports.map((port) => ({ port }))
  if (ruleForm.httpUrl.trim()) {
    monitoring.httpChecks = [{ url: ruleForm.httpUrl.trim(), expectedStatus: [200] }]
  }
  return monitoring
}

async function saveRule() {
  savingRule.value = true
  try {
    const body = { cluster: ruleForm.cluster, service: ruleForm.service, monitoring: buildMonitoring() }
    await alertRuleApi.upsert(body)
    ElMessage.success('已保存（下发需点「下发」按钮推送到 Worker）')
    ruleVisible.value = false
    await fetchAll()
  } catch {
    // 错误已由 http.ts 提示
  } finally {
    savingRule.value = false
  }
}

async function applyRule(row: AlertRule) {
  applying.value = `${row.cluster}/${row.service}`
  try {
    await alertRuleApi.apply(row)
    ElMessage.success('已下发到 Worker')
  } catch {
    // 错误已提示
  } finally {
    applying.value = ''
  }
}

async function removeRule(row: AlertRule) {
  await ElMessageBox.confirm(`删除规则 ${row.cluster}/${row.service}？`, '删除规则', { type: 'warning' })
  await alertRuleApi.remove(row.cluster, row.service)
  ElMessage.success('已删除')
  await fetchAll()
}

async function saveUser() {
  savingUser.value = true
  try {
    await authApi.createUser({ username: userForm.username, password: userForm.password, role: userForm.role })
    ElMessage.success('已创建')
    userVisible.value = false
    Object.assign(userForm, { username: '', password: '', role: 'viewer' })
    await fetchAll()
  } catch {
    // 错误已提示
  } finally {
    savingUser.value = false
  }
}

onMounted(fetchAll)
</script>

<style scoped>
.card-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
}
.mr {
  margin-right: 4px;
}
.mt {
  margin-top: 12px;
}
.mb {
  margin-bottom: 12px;
}
.ml {
  margin-left: 8px;
}
.muted {
  color: var(--el-text-color-placeholder);
  font-size: 12px;
}
</style>
