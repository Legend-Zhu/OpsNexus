<template>
  <div class="patrol-report">
    <el-alert type="info" :closable="false" class="mb"
      title="巡检报告渠道投递"
      description="每次巡检执行结束生成报告后，按此策略投递到通知渠道（渠道在「通知中心」维护）。巡检发现的异常同时会转为告警（告警中心可见，按 warn 级策略通知）。" />

    <el-form label-width="110px" style="max-width: 560px">
      <el-form-item label="发送时机">
        <el-radio-group v-model="mode">
          <el-radio value="always">每次生成后都发送</el-radio>
          <el-radio value="anomaly">仅有异常时发送</el-radio>
          <el-radio value="off">不发送</el-radio>
        </el-radio-group>
      </el-form-item>
      <el-form-item label="投递渠道">
        <el-select v-model="channelIds" multiple clearable placeholder="选择渠道（可多选）" style="width: 100%">
          <el-option v-for="c in channels" :key="c.id" :label="`${c.name}（${c.type}）`" :value="c.id" :disabled="!c.enabled" />
        </el-select>
        <div class="muted">无可用渠道？先到「通知中心 → 渠道」创建。</div>
      </el-form-item>
      <el-form-item>
        <el-button type="primary" :loading="saving" @click="save">保存</el-button>
      </el-form-item>
    </el-form>
  </div>
</template>

<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { notifyApi, settingsApi } from '@/api'
import type { NotifyChannel } from '@/types'

const mode = ref<'always' | 'anomaly' | 'off'>('off')
const channelIds = ref<string[]>([])
const channels = ref<NotifyChannel[]>([])
const saving = ref(false)

async function load() {
  try {
    const cfg = await settingsApi.patrolReport()
    mode.value = cfg.mode ?? 'off'
    channelIds.value = cfg.channel_ids ?? []
  } catch {
    /* 未配置时保持默认 */
  }
  try {
    const resp = await notifyApi.channels()
    channels.value = resp.items ?? []
  } catch {
    channels.value = []
  }
}

async function save() {
  if (mode.value !== 'off' && channelIds.value.length === 0) {
    ElMessage.warning('发送模式开启时需至少选择一个渠道')
    return
  }
  saving.value = true
  try {
    await settingsApi.updatePatrolReport({ mode: mode.value, channel_ids: channelIds.value })
    ElMessage.success('已保存，下次巡检执行生效')
  } catch {
    // 错误已由 http.ts 提示
  } finally {
    saving.value = false
  }
}

onMounted(load)
</script>

<style scoped>
.mb {
  margin-bottom: 14px;
}
.muted {
  color: var(--el-text-color-placeholder);
  font-size: 12px;
}
</style>
