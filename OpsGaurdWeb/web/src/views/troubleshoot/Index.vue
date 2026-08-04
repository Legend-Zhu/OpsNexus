<template>
  <el-card shadow="never">
    <template #header>
      <div class="card-header">
        <span>异常排查（AiNexus）</span>
        <el-tag size="small" type="success">已整合</el-tag>
      </div>
    </template>
    <el-empty description="AiNexus 对话排查 — 骨架占位（模型选择 + SSE 流式对话 + 注入集群诊断上下文）" />
  </el-card>
</template>

<script setup lang="ts">
// 异常排查：经管理端代理到 AiNexus 服务（SSE 透传），
// 可把所选集群的事件/日志/审计注入上下文供 Agent 排查。
// 骨架占位，业务待实现。
</script>

<style scoped>
.card-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
}
</style>
