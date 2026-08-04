<template>
  <el-card shadow="never">
    <template #header>
      <div class="card-header">
        <span>工作负载</span>
        <el-select placeholder="选择集群" style="width: 200px">
          <el-option label="开发集群" value="dev" />
        </el-select>
      </div>
    </template>
    <el-empty description="工作负载列表 — 骨架占位（服务/副本/健康，经 Worker 编排）" />
  </el-card>
</template>

<script setup lang="ts">
// 工作负载：服务列表/详情/缩放/回滚，代理到所选集群的 Worker。
// 骨架占位，业务待实现。
</script>

<style scoped>
.card-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
}
</style>
