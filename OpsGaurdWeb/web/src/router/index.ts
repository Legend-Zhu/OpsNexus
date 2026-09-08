import { createRouter, createWebHistory, type RouteRecordRaw } from 'vue-router'
import MainLayout from '@/layout/MainLayout.vue'
import { getToken } from '@/api/http'

/**
 * 路由：层级化 IA（项目 → 集群 → 节点/容器/进程）
 *  - /dashboard      总览（在线集群 / 未处理告警 / 巡检异常 / 项目数）
 *  - /projects       项目（管理层级第一层）
 *  - /clusters       集群列表
 *  - /clusters/:name 集群详情（节点 / 容器与服务 / 中间件 / 监控 / 事件）
 *  - /alerts /patrol /troubleshoot /notify /system
 *  - /login          登录（本地账号 + SSO）
 *  - /sso/callback   SSO 回调落地页（读 #token= 后跳首页）
 * 守卫：无 token 一律重定向 /login；已登录访问 /login 跳回 /dashboard。
 */
const routes: RouteRecordRaw[] = [
  {
    path: '/login',
    name: 'login',
    component: () => import('@/views/login/Index.vue'),
    meta: { title: '登录' },
  },
  {
    path: '/sso/callback',
    name: 'sso-callback',
    component: () => import('@/views/sso/Callback.vue'),
    meta: { title: 'SSO 回调' },
  },
  {
    path: '/',
    component: MainLayout,
    redirect: '/dashboard',
    children: [
      {
        path: 'dashboard',
        name: 'dashboard',
        component: () => import('@/views/dashboard/Index.vue'),
        meta: { title: '总览' },
      },
      {
        path: 'projects',
        name: 'projects',
        component: () => import('@/views/projects/Index.vue'),
        meta: { title: '项目' },
      },
      {
        path: 'clusters',
        name: 'clusters',
        component: () => import('@/views/clusters/Index.vue'),
        meta: { title: '集群' },
      },
      {
        path: 'clusters/:name',
        name: 'cluster-detail',
        component: () => import('@/views/clusters/Detail.vue'),
        meta: { title: '集群详情' },
      },
      {
        path: 'alerts',
        name: 'alerts',
        component: () => import('@/views/alerts/Index.vue'),
        meta: { title: '告警中心' },
      },
      {
        path: 'registry',
        name: 'registry',
        component: () => import('@/views/registry/Index.vue'),
        meta: { title: '镜像仓库' },
      },
      {
        path: 'patrol',
        name: 'patrol',
        component: () => import('@/views/patrol/Index.vue'),
        meta: { title: '智能巡检' },
      },
      {
        path: 'troubleshoot',
        name: 'troubleshoot',
        component: () => import('@/views/troubleshoot/Index.vue'),
        meta: { title: '异常排查' },
      },
      {
        path: 'mlops',
        name: 'mlops',
        component: () => import('@/views/mlops/Index.vue'),
        meta: { title: 'MLOps' },
      },
      {
        path: 'notify',
        name: 'notify',
        component: () => import('@/views/notify/Index.vue'),
        meta: { title: '通知中心' },
      },
      {
        path: 'system',
        name: 'system',
        component: () => import('@/views/system/Index.vue'),
        meta: { title: '系统设置' },
      },
      {
        path: 'docs',
        name: 'docs',
        component: () => import('@/views/docs/Index.vue'),
        meta: { title: '文档' },
      },
      // 工作负载 / 监控已收编进集群详情（/clusters/:name）
      { path: 'workloads', redirect: '/clusters' },
      { path: 'monitor', redirect: '/clusters' },
    ],
  },
  {
    path: '/:pathMatch(.*)*',
    name: 'not-found',
    component: () => import('@/views/NotFound.vue'),
  },
]

const router = createRouter({
  history: createWebHistory(),
  routes,
})

// 公开路径（无需登录）
const PUBLIC_PATHS = ['/login', '/sso/callback']

router.beforeEach((to) => {
  const authed = Boolean(getToken())
  if (authed && to.path === '/login') {
    return { path: '/dashboard' }
  }
  if (!authed && !PUBLIC_PATHS.some((p) => to.path.startsWith(p))) {
    return { path: '/login', query: { redirect: to.fullPath } }
  }
  return true
})

router.afterEach((to) => {
  const base = 'OpsGaurd'
  document.title = to.meta.title ? `${to.meta.title as string} · ${base}` : base
})

export default router
