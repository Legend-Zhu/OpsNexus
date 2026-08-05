<template>
  <div class="cb">
    <div class="spinner" />
    <p v-if="error" class="err">{{ error }}</p>
    <p v-else class="og-dim">SSO 登录成功，正在进入控制台…</p>
  </div>
</template>

<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { setToken } from '@/api/http'

// 后端回调成功后 302 到 FrontendURL，token 放 URL hash：/#token=… 或 /#error=…
const router = useRouter()
const error = ref('')

onMounted(() => {
  const hash = window.location.hash // 形如 #token=xxx 或 #error=xxx
  const params = new URLSearchParams(hash.replace(/^#/, ''))
  const token = params.get('token')
  const err = params.get('error')
  if (token) {
    setToken(decodeURIComponent(token))
    // 清除 hash，避免 token 残留在地址栏
    window.history.replaceState(null, '', '/sso/callback')
    router.replace('/dashboard')
    return
  }
  error.value = err ? `SSO 登录失败：${err}` : 'SSO 回调缺少 token'
})
</script>

<style scoped>
.cb {
  height: 100vh;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 16px;
  background: var(--og-bg-base);
}
.spinner {
  width: 34px;
  height: 34px;
  border-radius: 50%;
  border: 3px solid var(--el-border-color);
  border-top-color: var(--og-accent);
  animation: spin 0.8s linear infinite;
}
@keyframes spin {
  to {
    transform: rotate(360deg);
  }
}
.err {
  color: #f87171;
  font-size: 14px;
}
</style>
