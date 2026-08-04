<template>
  <el-card shadow="never">
    <template #header>
      <div class="card-header">
        <span>集群管理（类 Rancher）</span>
        <el-button type="primary" :icon="Plus">接入集群</el-button>
      </div>
    </template>
    <el-empty description="集群列表 — 骨架占位（多集群接入/健康状态/进入管理）" />
  </el-card>
</template>

<script setup lang="ts">
import { Plus } from '@element-plus/icons-vue'
// 集群管理：接入集群（Worker 端点 + token）、列表/健康、进入集群详情。
// 骨架占位，业务待实现。
</script>

<style scoped>
.card-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
}
</style>
