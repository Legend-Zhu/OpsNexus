<template>
  <el-container class="layout">
    <el-aside width="224px" class="aside">
      <router-link to="/dashboard" class="logo">
        <span class="logo-mark">OG</span>
        <span class="logo-name">OpsGaurd</span>
      </router-link>
      <el-menu
        :default-active="activeMenu"
        router
        class="nav"
        background-color="transparent"
        text-color="rgba(230,237,243,0.62)"
        active-text-color="#2dd4bf"
      >
        <el-menu-item index="/dashboard">
          <el-icon><Odometer /></el-icon>
          <span>总览</span>
        </el-menu-item>
        <el-menu-item index="/projects">
          <el-icon><FolderOpened /></el-icon>
          <span>项目</span>
        </el-menu-item>
        <el-menu-item index="/clusters">
          <el-icon><Platform /></el-icon>
          <span>集群</span>
        </el-menu-item>
        <el-menu-item index="/alerts">
          <el-icon><Bell /></el-icon>
          <span>告警中心</span>
        </el-menu-item>
        <el-menu-item index="/patrol">
          <el-icon><Calendar /></el-icon>
          <span>智能巡检</span>
        </el-menu-item>
        <el-menu-item index="/troubleshoot">
          <el-icon><MagicStick /></el-icon>
          <span>异常排查</span>
        </el-menu-item>
        <el-menu-item index="/notify">
          <el-icon><Promotion /></el-icon>
          <span>通知中心</span>
        </el-menu-item>
        <el-menu-item index="/system">
          <el-icon><Setting /></el-icon>
          <span>系统设置</span>
        </el-menu-item>
      </el-menu>
    </el-aside>

    <el-container>
      <el-header class="header">
        <span class="title">{{ $route.meta.title ?? 'OpsGaurd' }}</span>
        <el-space :size="16">
          <el-tag size="small" effect="plain" class="env-tag">管理面</el-tag>
          <el-dropdown trigger="click" @command="onCommand">
            <span class="user">
              <span class="avatar">{{ avatarChar }}</span>
              <span class="user-name">{{ me?.username ?? '—' }}</span>
              <span v-if="me?.role" class="user-role">{{ me.role }}</span>
            </span>
            <template #dropdown>
              <el-dropdown-menu>
                <el-dropdown-item command="logout">退出登录</el-dropdown-item>
              </el-dropdown-menu>
            </template>
          </el-dropdown>
        </el-space>
      </el-header>

      <el-main class="main">
        <router-view v-slot="{ Component }">
          <transition name="fade" mode="out-in">
            <component :is="Component" />
          </transition>
        </router-view>
      </el-main>
    </el-container>
  </el-container>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import {
  Bell, Calendar, FolderOpened, MagicStick, Odometer, Platform, Promotion, Setting,
} from '@element-plus/icons-vue'
import { authApi } from '@/api'
import { clearToken } from '@/api/http'
import type { Me } from '@/types'

const route = useRoute()
const router = useRouter()
const me = ref<Me | null>(null)

// 集群详情页高亮「集群」菜单
const activeMenu = computed(() => {
  if (route.path.startsWith('/clusters')) return '/clusters'
  return route.path
})

const avatarChar = computed(() => (me.value?.username?.slice(0, 1) ?? '?').toUpperCase())

async function loadMe() {
  try {
    me.value = await authApi.me()
  } catch {
    // 401 已由 http.ts 处理（清 token 跳登录）
  }
}

function onCommand(cmd: string) {
  if (cmd === 'logout') {
    clearToken()
    router.replace('/login')
  }
}

onMounted(loadMe)
</script>

<style scoped>
.layout {
  height: 100vh;
}
.aside {
  display: flex;
  flex-direction: column;
  background: #0d1117;
  border-right: 1px solid var(--el-border-color-lighter);
}
.logo {
  display: flex;
  align-items: center;
  gap: 10px;
  height: 60px;
  padding: 0 18px;
  text-decoration: none;
}
.logo-mark {
  display: grid;
  place-items: center;
  width: 32px;
  height: 32px;
  border-radius: 9px;
  background: linear-gradient(135deg, var(--og-accent), #0e7490);
  color: #04120f;
  font-weight: 800;
  font-size: 13px;
}
.logo-name {
  color: #e6edf3;
  font-size: 17px;
  font-weight: 700;
  letter-spacing: -0.02em;
}
.nav {
  flex: 1;
  border-right: none;
  padding: 6px 8px;
  --el-menu-item-height: 44px;
}
.nav :deep(.el-menu-item) {
  border-radius: 8px;
  margin-bottom: 2px;
}
.nav :deep(.el-menu-item.is-active) {
  background: var(--og-accent-soft);
}
.header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  height: 60px;
  border-bottom: 1px solid var(--el-border-color-lighter);
  background: var(--og-bg-surface);
}
.title {
  font-size: 15px;
  font-weight: 600;
}
.env-tag {
  color: var(--og-text-dim);
}
.user {
  display: flex;
  align-items: center;
  gap: 8px;
  cursor: pointer;
  padding: 4px 10px;
  border-radius: 8px;
  transition: background 0.15s;
}
.user:hover {
  background: var(--el-fill-color-light);
}
.avatar {
  display: grid;
  place-items: center;
  width: 28px;
  height: 28px;
  border-radius: 8px;
  background: var(--og-accent-soft);
  color: var(--og-accent-strong);
  font-weight: 700;
  font-size: 13px;
}
.user-name {
  font-size: 13px;
}
.user-role {
  font-size: 11px;
  color: var(--og-text-dim);
  background: var(--el-fill-color);
  padding: 1px 7px;
  border-radius: 99px;
}
.main {
  background: var(--og-bg-page);
  padding: 20px;
}
</style>
