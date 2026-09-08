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
        <el-menu-item index="/registry">
          <el-icon><Box /></el-icon>
          <span>镜像仓库</span>
        </el-menu-item>
        <el-menu-item index="/patrol">
          <el-icon><Calendar /></el-icon>
          <span>智能巡检</span>
        </el-menu-item>
        <el-menu-item index="/troubleshoot">
          <el-icon><MagicStick /></el-icon>
          <span>异常排查</span>
        </el-menu-item>
        <el-menu-item index="/mlops">
          <el-icon><Notebook /></el-icon>
          <span>MLOps</span>
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
          <router-link to="/docs" class="docs-link" title="使用文档">
            <el-icon><Document /></el-icon>
            <span>使用文档</span>
          </router-link>
          <el-dropdown trigger="click" @command="onTheme">
            <span class="theme-btn" title="切换主题">
              <el-icon><Brush /></el-icon>
            </span>
            <template #dropdown>
              <el-dropdown-menu>
                <el-dropdown-item
                  v-for="t in THEMES"
                  :key="t.key"
                  :command="t.key"
                >
                  <span class="theme-dot" :style="{ background: t.swatch }" />
                  <span class="theme-label">{{ t.label }}</span>
                  <el-icon v-if="t.key === currentTheme" class="theme-check"><Check /></el-icon>
                </el-dropdown-item>
              </el-dropdown-menu>
            </template>
          </el-dropdown>
          <el-dropdown trigger="click" @command="onCommand">
            <span class="user">
              <span class="avatar">{{ avatarChar }}</span>
              <span class="user-name">{{ me?.username ?? '—' }}</span>
              <span v-if="me?.role" class="user-role">{{ me.role }}</span>
            </span>
            <template #dropdown>
              <el-dropdown-menu>
                <el-dropdown-item command="password">修改密码</el-dropdown-item>
                <el-dropdown-item command="logout" divided>退出登录</el-dropdown-item>
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

      <!-- 自助修改密码（所有登录用户） -->
      <ChangePasswordDialog v-model:visible="pwdVisible" />
    </el-container>
  </el-container>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import {
  Bell, Box, Calendar, Document, FolderOpened, MagicStick, Notebook, Odometer, Platform, Promotion, Setting,
} from '@element-plus/icons-vue'
import { authApi } from '@/api'
import { clearToken } from '@/api/http'
import ChangePasswordDialog from '@/components/ChangePasswordDialog.vue'
import { THEMES, applyTheme, currentTheme } from '@/theme'
import type { Me } from '@/types'

const route = useRoute()
const router = useRouter()
const me = ref<Me | null>(null)
const pwdVisible = ref(false)

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
  if (cmd === 'password') {
    pwdVisible.value = true
    return
  }
  if (cmd === 'logout') {
    clearToken()
    router.replace('/login')
  }
}

function onTheme(key: string) {
  applyTheme(key)
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
  background: var(--og-bg-sidebar);
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
  background: linear-gradient(135deg, var(--og-accent), var(--og-accent-deep));
  color: var(--og-on-accent);
  font-weight: 800;
  font-size: 13px;
}
.logo-name {
  color: var(--el-text-color-primary);
  font-size: 17px;
  font-weight: 700;
  letter-spacing: -0.02em;
}
.nav {
  flex: 1;
  border-right: none;
  padding: 6px 8px;
  --el-menu-item-height: 44px;
  --el-menu-text-color: var(--og-menu-text);
  --el-menu-active-color: var(--og-accent-strong);
  --el-menu-hover-text-color: var(--el-text-color-primary);
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
.docs-link {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  font-size: 13px;
  color: var(--og-text-dim);
  text-decoration: none;
  padding: 4px 10px;
  border-radius: 8px;
  transition: background 0.15s, color 0.15s;
}
.docs-link:hover {
  background: var(--el-fill-color-light);
  color: var(--el-text-color-primary);
}
.theme-btn {
  display: grid;
  place-items: center;
  width: 32px;
  height: 32px;
  border-radius: 8px;
  cursor: pointer;
  color: var(--el-text-color-secondary);
  transition: background 0.15s, color 0.15s;
}
.theme-btn:hover {
  background: var(--el-fill-color-light);
  color: var(--el-text-color-primary);
}
.theme-dot {
  display: inline-block;
  width: 14px;
  height: 14px;
  margin-right: 8px;
  border-radius: 50%;
  border: 1px solid var(--el-border-color);
  vertical-align: -2px;
}
.theme-label {
  font-size: 13px;
}
.theme-check {
  margin-left: 12px;
  color: var(--og-accent);
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
