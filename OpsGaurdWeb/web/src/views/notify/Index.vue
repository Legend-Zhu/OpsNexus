<template>
  <el-card shadow="never">
    <template #header>
      <div class="card-header">
        <span>通知中心</span>
        <div class="header-right">
          <el-button type="primary" :icon="Plus" @click="openChannel">新增渠道</el-button>
        </div>
      </div>
    </template>

    <el-tabs v-model="tab">
      <!-- 渠道 -->
      <el-tab-pane label="渠道" name="channels">
        <el-table :data="channels" empty-text="暂无渠道">
          <el-table-column prop="name" label="名称" min-width="120" />
          <el-table-column prop="type" label="类型" width="90">
            <template #default="{ row }">
              <el-tag size="small" type="info">{{ row.type }}</el-tag>
            </template>
          </el-table-column>
          <el-table-column label="网络" width="110">
            <template #default="{ row }">
              <el-tag size="small" :type="row.via_proxy ? 'warning' : 'success'">
                {{ row.via_proxy ? '经互联网代理' : '直连' }}
              </el-tag>
            </template>
          </el-table-column>
          <el-table-column label="状态" width="90">
            <template #default="{ row }">
              <el-switch :model-value="row.enabled" size="small" @change="(v: string | number | boolean) => toggleChannel(row, v)" />
            </template>
          </el-table-column>
          <el-table-column label="配置" min-width="200" show-overflow-tooltip>
            <template #default="{ row }">{{ describeConfig(row) }}</template>
          </el-table-column>
          <el-table-column label="操作" width="140" fixed="right">
            <template #default="{ row }">
              <el-button link type="primary" @click="editChannel(row)">编辑</el-button>
              <el-button link type="danger" @click="removeChannel(row)">删除</el-button>
            </template>
          </el-table-column>
        </el-table>
      </el-tab-pane>

      <!-- 策略 -->
      <el-tab-pane label="策略" name="policies">
        <el-table :data="policies" empty-text="暂无策略（告警不会发通知）">
          <el-table-column prop="level" label="级别" width="100">
            <template #default="{ row }">
              <el-tag size="small" :type="levelTag(row.level)">{{ row.level }}</el-tag>
            </template>
          </el-table-column>
          <el-table-column label="渠道" min-width="200">
            <template #default="{ row }">
              <el-tag v-for="cid in row.channel_ids" :key="cid" size="small" class="mr">
                {{ channelName(cid) }}
              </el-tag>
              <span v-if="!row.channel_ids?.length" class="muted">未配置</span>
            </template>
          </el-table-column>
          <el-table-column label="接收人" min-width="140">
            <template #default="{ row }">{{ (row.receivers ?? []).join(', ') || '—' }}</template>
          </el-table-column>
          <el-table-column label="操作" width="100">
            <template #default="{ row }">
              <el-button link type="primary" @click="openPolicy(row)">编辑</el-button>
            </template>
          </el-table-column>
        </el-table>
      </el-tab-pane>

      <!-- 发送记录 -->
      <el-tab-pane label="发送记录" name="records">
        <el-table :data="records" empty-text="暂无发送记录">
          <el-table-column label="时间" width="170">
            <template #default="{ row }">{{ new Date(row.ts).toLocaleString() }}</template>
          </el-table-column>
          <el-table-column prop="target" label="渠道" width="140" />
          <el-table-column prop="title" label="内容" min-width="220" show-overflow-tooltip />
          <el-table-column label="状态" width="90">
            <template #default="{ row }">
              <el-tag size="small" :type="row.status === 'success' ? 'success' : 'danger'">{{ row.status }}</el-tag>
            </template>
          </el-table-column>
          <el-table-column prop="error" label="错误" min-width="160" show-overflow-tooltip>
            <template #default="{ row }">{{ row.error || '—' }}</template>
          </el-table-column>
        </el-table>
      </el-tab-pane>
    </el-tabs>

    <!-- 渠道对话框 -->
    <el-dialog v-model="channelVisible" :title="editingChannel ? '编辑渠道' : '新增渠道'" width="520px">
      <el-form label-width="110px">
        <el-form-item label="名称">
          <el-input v-model="channelForm.name" />
        </el-form-item>
        <el-form-item label="类型">
          <el-select v-model="channelForm.type" style="width: 100%">
            <el-option label="飞书" value="feishu" />
            <el-option label="短信" value="sms" />
            <el-option label="通用 Webhook" value="webhook" />
          </el-select>
        </el-form-item>
        <el-form-item label="经互联网代理">
          <el-switch v-model="channelForm.via_proxy" />
        </el-form-item>
        <el-form-item v-if="channelForm.via_proxy" label="代理地址">
          <el-input v-model="channelForm.proxy_url" placeholder="http://proxy-host:8080/notify" />
        </el-form-item>
        <el-form-item v-if="channelForm.type === 'webhook' && !channelForm.via_proxy" label="Webhook URL">
          <el-input v-model="channelForm.config.url" placeholder="https://…/hook" />
        </el-form-item>
        <el-form-item v-if="channelForm.type === 'feishu' && !channelForm.via_proxy" label="飞书 Webhook">
          <el-input v-model="channelForm.config.webhook_url" placeholder="https://open.feishu.cn/open-apis/bot/v2/hook/…" />
        </el-form-item>
        <el-form-item v-if="channelForm.type === 'sms'" label="签名（代理侧）">
          <el-input v-model="channelForm.config.sign" placeholder="短信签名（凭据在代理侧）" />
        </el-form-item>
        <el-form-item label="启用">
          <el-switch v-model="channelForm.enabled" />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="channelVisible = false">取消</el-button>
        <el-button type="primary" :loading="saving" @click="saveChannel">保存</el-button>
      </template>
    </el-dialog>

    <!-- 策略对话框 -->
    <el-dialog v-model="policyVisible" title="编辑策略" width="460px">
      <el-form label-width="80px">
        <el-form-item label="级别">
          <el-tag :type="levelTag(policyForm.level)">{{ policyForm.level }}</el-tag>
        </el-form-item>
        <el-form-item label="渠道">
          <el-select v-model="policyForm.channel_ids" multiple style="width: 100%" placeholder="选择发送渠道">
            <el-option v-for="c in enabledChannels" :key="c.id" :label="c.name" :value="c.id" />
          </el-select>
        </el-form-item>
        <el-form-item label="接收人">
          <el-select v-model="policyForm.receivers" multiple allow-create filterable default-first-option
            style="width: 100%" placeholder="手机号/用户标识（可输入）" />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="policyVisible = false">取消</el-button>
        <el-button type="primary" @click="savePolicy">保存</el-button>
      </template>
    </el-dialog>
  </el-card>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Plus } from '@element-plus/icons-vue'
import { notifyApi } from '@/api'
import type { NotifyChannel, NotifyPolicy, NotifyRecord } from '@/types'

const tab = ref('channels')
const channels = ref<NotifyChannel[]>([])
const policies = ref<NotifyPolicy[]>([])
const records = ref<NotifyRecord[]>([])
const saving = ref(false)

const channelVisible = ref(false)
const editingChannel = ref(false)
const channelForm = reactive({
  id: '', name: '', type: 'feishu', via_proxy: true, proxy_url: '', enabled: true,
  config: {} as Record<string, string>,
})

const policyVisible = ref(false)
const policyForm = reactive<{ level: string; channel_ids: string[]; receivers: string[] }>({
  level: '', channel_ids: [], receivers: [],
})

const enabledChannels = computed(() => channels.value.filter((c) => c.enabled))

function levelTag(l: string) {
  return l === 'error' ? 'danger' : l === 'warn' ? 'warning' : 'info'
}
function describeConfig(ch: NotifyChannel) {
  const parts: string[] = []
  if (ch.type === 'webhook' && ch.config.url) parts.push(String(ch.config.url))
  if (ch.type === 'feishu' && ch.config.webhook_url) parts.push(String(ch.config.webhook_url))
  if (ch.type === 'sms' && ch.config.sign) parts.push(`签名:${ch.config.sign}`)
  if (ch.via_proxy) parts.push(`代理:${ch.proxy_url ?? '—'}`)
  return parts.join(' · ') || '—'
}
function channelName(id: string) {
  return channels.value.find((c) => c.id === id)?.name ?? id
}

async function fetchAll() {
  const [ch, po, rec] = await Promise.all([notifyApi.channels(), notifyApi.policies(), notifyApi.records(50)])
  channels.value = ch.items ?? []
  policies.value = po.items ?? []
  records.value = rec.items ?? []
}

function openChannel() {
  editingChannel.value = false
  Object.assign(channelForm, { id: '', name: '', type: 'feishu', via_proxy: true, proxy_url: '', enabled: true, config: {} })
  channelVisible.value = true
}

function editChannel(row: NotifyChannel) {
  editingChannel.value = true
  Object.assign(channelForm, {
    id: row.id, name: row.name, type: row.type, via_proxy: row.via_proxy,
    proxy_url: row.proxy_url ?? '', enabled: row.enabled,
    config: { ...(row.config as Record<string, string>) },
  })
  channelVisible.value = true
}

async function saveChannel() {
  saving.value = true
  try {
    const body = {
      name: channelForm.name,
      type: channelForm.type as NotifyChannel['type'],
      config: channelForm.config,
      via_proxy: channelForm.via_proxy,
      proxy_url: channelForm.proxy_url,
      enabled: channelForm.enabled,
    }
    if (editingChannel.value) {
      await notifyApi.updateChannel(channelForm.id, body)
      ElMessage.success('已更新')
    } else {
      await notifyApi.createChannel(body)
      ElMessage.success('已创建')
    }
    channelVisible.value = false
    await fetchAll()
  } catch {
    // 错误已由 http.ts 提示
  } finally {
    saving.value = false
  }
}

async function toggleChannel(row: NotifyChannel, v: string | number | boolean) {
  await notifyApi.updateChannel(row.id, { ...row, enabled: Boolean(v) })
  await fetchAll()
}

async function removeChannel(row: NotifyChannel) {
  await ElMessageBox.confirm(`删除渠道「${row.name}」？`, '删除渠道', { type: 'warning' })
  await notifyApi.removeChannel(row.id)
  ElMessage.success('已删除')
  await fetchAll()
}

function openPolicy(row: NotifyPolicy) {
  Object.assign(policyForm, { level: row.level, channel_ids: [...(row.channel_ids ?? [])], receivers: [...(row.receivers ?? [])] })
  policyVisible.value = true
}

async function savePolicy() {
  await notifyApi.upsertPolicy(policyForm.level, { channel_ids: policyForm.channel_ids, receivers: policyForm.receivers })
  ElMessage.success('已保存')
  policyVisible.value = false
  await fetchAll()
}

onMounted(fetchAll)
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
.mr {
  margin-right: 4px;
}
.muted {
  color: var(--el-text-color-placeholder);
}
</style>
