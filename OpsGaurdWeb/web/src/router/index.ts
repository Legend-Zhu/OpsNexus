import { createRouter, createWebHistory, type RouteRecordRaw } from 'vue-router'
import MainLayout from '@/layout/MainLayout.vue'

/**
 * 路由骨架：
 *  - /dashboard      仪表盘（多集群总览）
 *  - /clusters       集群管理（类 Rancher：集群列表/详情/工作负载/事件）
 *  - /monitor        集群监控（节点/容器资源，经 Worker /local/stats）
 *  - /alerts         告警中心（Worker webhook 汇聚）
 *  - /patrol         智能巡检（YAML 流程 + 内置调度 + AI 报告）
 *  - /troubleshoot   异常排查（AiNexus 集成）
 * 后续业务迭代时按模块拆 views 子路由。
 */
const routes: RouteRecordRaw[] = [
  {
    path: '/',
    component: MainLayout,
    redirect: '/dashboard',
    children: [
      {
        path: 'dashboard',
        name: 'dashboard',
        component: () => import('@/views/dashboard/Index.vue'),
        meta: { title: '仪表盘' },
      },
      {
        path: 'clusters',
        name: 'clusters',
        component: () => import('@/views/clusters/Index.vue'),
        meta: { title: '集群管理' },
      },
      {
        path: 'workloads',
        name: 'workloads',
        component: () => import('@/views/workloads/Index.vue'),
        meta: { title: '工作负载' },
      },
      {
        path: 'monitor',
        name: 'monitor',
        component: () => import('@/views/monitor/Index.vue'),
        meta: { title: '集群监控' },
      },
      {
        path: 'alerts',
        name: 'alerts',
        component: () => import('@/views/alerts/Index.vue'),
        meta: { title: '告警中心' },
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

export default router
