<template>
  <div class="mlops">
    <el-tabs v-model="tab">
      <!-- 模型接入：模型池（provider + 模型清单）唯一配置入口。
           属网关基础设施（/ainexus/config 恒注册），不受 mlops.enabled 开关影响。 -->
      <el-tab-pane label="模型接入" name="model-config" lazy>
        <model-config />
      </el-tab-pane>

      <!-- 运营层（提示词/模型/费用）：mlops.enabled=true 才有后端路由 -->
      <template v-if="opsEnabled">
        <el-tab-pane label="提示词" name="prompts">
          <prompts-panel />
        </el-tab-pane>
        <el-tab-pane label="模型" name="models" lazy>
          <models-panel />
        </el-tab-pane>
        <el-tab-pane label="用量费用" name="costs" lazy>
          <costs-panel />
        </el-tab-pane>
      </template>
    </el-tabs>

    <!-- mlops.enabled=false：只保留模型接入（探测完成前不提示，避免闪烁） -->
    <el-alert
      v-if="opsLoaded && !opsEnabled"
      type="info"
      :closable="false"
      class="ops-hint"
      title="MLOps 运营层未启用"
      description="提示词 / 模型运营 / 用量费用需要 server.yaml 配置 mlops.enabled=true 后重启生效。模型接入为网关基础能力，始终可用。" />
  </div>
</template>

<script setup lang="ts">
import { onMounted, ref, defineAsyncComponent } from 'vue'
import { mlopsApi } from '@/api'

// 模型/费用面板较重（运营数据/趋势明细），lazy 按需加载
const ModelsPanel = defineAsyncComponent(() => import('./Models.vue'))
const CostsPanel = defineAsyncComponent(() => import('./Costs.vue'))
const PromptsPanel = defineAsyncComponent(() => import('./Prompts.vue'))
const ModelConfig = defineAsyncComponent(() => import('./ModelConfig.vue'))

const tab = ref('model-config')

// 运营 tab 可用性：mlops.enabled=false 时 /v1/mlops/* 未注册，请求落到
// SPA 回退（返回 HTML 而非 JSON），get() 解析结果为 undefined —— 据此裁剪。
const opsEnabled = ref(true)
const opsLoaded = ref(false)

onMounted(async () => {
  try {
    const view = await mlopsApi.models()
    opsEnabled.value = view != null
  } catch {
    opsEnabled.value = false
  } finally {
    opsLoaded.value = true
  }
})
</script>

<style scoped>
.mlops :deep(.el-tabs__header) {
  margin-bottom: 4px;
}
.ops-hint {
  margin-top: 8px;
}
</style>
