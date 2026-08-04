<template>
  <el-container class="layout">
    <el-aside width="220px" class="aside">
      <div class="logo">OpsGaurd</div>
      <el-menu
        :default-active="$route.path"
        router
        background-color="#001529"
        text-color="rgba(255,255,255,0.7)"
        active-text-color="#409EFF"
      >
        <el-menu-item index="/dashboard">
          <el-icon><Odometer /></el-icon>
          <span>仪表盘</span>
        </el-menu-item>
        <el-menu-item index="/clusters">
          <el-icon><Platform /></el-icon>
          <span>集群管理</span>
        </el-menu-item>
        <el-menu-item index="/workloads">
          <el-icon><Box /></el-icon>
          <span>工作负载</span>
        </el-menu-item>
        <el-menu-item index="/monitor">
          <el-icon><Monitor /></el-icon>
          <span>集群监控</span>
        </el-menu-item>
        <el-menu-item index="/alerts">
          <el-icon><Bell /></el-icon>
          <span>告警中心</span>
        </el-menu-item>
        <el-menu-item index="/troubleshoot">
          <el-icon><MagicStick /></el-icon>
          <span>异常排查</span>
        </el-menu-item>
      </el-menu>
    </el-aside>

    <el-container>
      <el-header class="header">
        <span class="title">{{ $route.meta.title ?? 'OpsGaurd' }}</span>
        <el-space>
          <el-tag size="small" type="info">AiNexus</el-tag>
          <el-dropdown>
            <span class="user">管理员</span>
            <template #dropdown>
              <el-dropdown-menu>
                <el-dropdown-item>退出登录</el-dropdown-item>
              </el-dropdown-menu>
            </template>
          </el-dropdown>
        </el-space>
      </el-header>

      <el-main class="main">
        <router-view />
      </el-main>
    </el-container>
  </el-container>
</template>

<script setup lang="ts">
import { Bell, Box, MagicStick, Monitor, Odometer, Platform } from '@element-plus/icons-vue'
// 主布局：左侧导航 + 顶部栏 + 内容区。
</script>

<style scoped>
.layout {
  height: 100vh;
}
.aside {
  background: #001529;
}
.logo {
  height: 56px;
  line-height: 56px;
  text-align: center;
  color: #fff;
  font-size: 18px;
  font-weight: 600;
}
.header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  border-bottom: 1px solid #e5e7eb;
  background: #fff;
}
.title {
  font-size: 16px;
  font-weight: 500;
}
.user {
  cursor: pointer;
}
.main {
  background: #f5f7fa;
}
</style>
