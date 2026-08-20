<template>
  <div class="mlops">
    <el-tabs v-model="tab">
      <el-tab-pane label="提示词" name="prompts">
        <prompts-panel />
      </el-tab-pane>
      <el-tab-pane label="用量费用" name="costs" lazy>
        <costs-panel />
      </el-tab-pane>
    </el-tabs>
  </div>
</template>

<script setup lang="ts">
import { ref, defineAsyncComponent } from 'vue'

// 费用面板较重（趋势/明细/价格），lazy 按需加载
const CostsPanel = defineAsyncComponent(() => import('./Costs.vue'))
const PromptsPanel = defineAsyncComponent(() => import('./Prompts.vue'))

const tab = ref('prompts')
</script>

<style scoped>
.mlops :deep(.el-tabs__header) {
  margin-bottom: 4px;
}
</style>
