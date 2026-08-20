// 当前用户是否管理员（MLOps 等管理端写操作的前端可见性收敛）。
// 后端 admin 守卫始终是权威校验，这里只做 UI 体验：非 admin 隐藏写入口。
// 未启用认证（/auth/me 不可用）时保持 true（与后端"无认证即放开"一致）。
import { ref } from 'vue'
import { authApi } from '@/api'

export const isAdmin = ref(true)
let loaded = false

export async function loadAdminFlag(): Promise<void> {
  if (loaded) return
  loaded = true
  try {
    const me = await authApi.me()
    isAdmin.value = me.role === 'admin'
  } catch {
    isAdmin.value = true // 认证未启用
  }
}
