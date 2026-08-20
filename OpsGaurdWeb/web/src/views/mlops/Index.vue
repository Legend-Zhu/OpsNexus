<template>
  <div class="mlops">
    <el-tabs v-model="tab">
      <el-tab-pane label="提示词" name="prompts">
        <prompts-panel />
      </el-tab-pane>
      <el-tab-pane label="模型" name="models" lazy>
        <models-panel />
      </el-tab-pane>
      <el-tab-pane label="用量费用" name="costs" lazy>
        <costs-panel />
      </el-tab-pane>
    </el-tabs>
  </div>
</template>

<script setup lang="ts">
import { ref, defineAsyncComponent } from 'vue'

// 模型/费用面板较重（运营数据/趋势明细），lazy 按需加载
const ModelsPanel = defineAsyncComponent(() => import('./Models.vue'))
const CostsPanel = defineAsyncComponent(() => import('./Costs.vue'))
const PromptsPanel = defineAsyncComponent(() => import('./Prompts.vue'))

const tab = ref('prompts')
</script>

<style scoped>
.mlops :deep(.el-tabs__header) {
  margin-bottom: 4px;
}
</style>
