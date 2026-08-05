<template>
  <div class="login-page">
    <!-- 背景：细网格 + 强调色光晕（纯 CSS，无图片） -->
    <div class="bg-grid" />
    <div class="bg-glow" />

    <div class="panel">
      <header class="brand">
        <span class="brand-mark">OG</span>
        <div>
          <h1 class="brand-name">OpsGaurd</h1>
          <p class="brand-tagline">集群运维 · 智能巡检 · AI 排查</p>
        </div>
      </header>

      <el-form v-if="localEnabled" ref="formRef" :model="form" :rules="rules" size="large" @keyup.enter="submit">
        <el-form-item prop="username">
          <el-input v-model="form.username" placeholder="用户名" autocomplete="username" :prefix-icon="User" />
        </el-form-item>
        <el-form-item prop="password">
          <el-input
            v-model="form.password"
            type="password"
            placeholder="密码"
            autocomplete="current-password"
            show-password
            :prefix-icon="Lock"
          />
        </el-form-item>
        <el-button class="submit" type="primary" :loading="loading" @click="submit">
          进入控制台
        </el-button>
      </el-form>

      <div v-else class="no-local">
        <p class="og-dim">本地账号未启用</p>
      </div>

      <el-divider v-if="localEnabled && ssoEnabled" class="divider">或</el-divider>

      <el-button
        v-if="ssoEnabled"
        class="submit sso"
        :icon="Connection"
        @click="goSSO"
      >
        企业 SSO 登录
      </el-button>

      <p v-if="hint" class="hint">{{ hint }}</p>
    </div>
  </div>
</template>

<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { Connection, Lock, User } from '@element-plus/icons-vue'
import { ElMessage, type FormInstance, type FormRules } from 'element-plus'
import { authApi } from '@/api'
import { setToken } from '@/api/http'

const router = useRouter()
const route = useRoute()

const formRef = ref<FormInstance>()
const form = reactive({ username: '', password: '' })
const loading = ref(false)
const localEnabled = ref(true)
const ssoEnabled = ref(false)
const hint = ref('')

const rules: FormRules = {
  username: [{ required: true, message: '请输入用户名', trigger: 'blur' }],
  password: [{ required: true, message: '请输入密码', trigger: 'blur' }],
}

async function loadSSOStatus() {
  try {
    const st = await authApi.ssoStatus()
    localEnabled.value = st.local
    ssoEnabled.value = st.sso?.enabled ?? false
  } catch {
    // 后端不可达时保留默认（本地表单可见）
  }
}

async function submit() {
  await formRef.value?.validate()
  loading.value = true
  try {
    const { token } = await authApi.login(form.username, form.password)
    setToken(token)
    ElMessage.success(`欢迎回来，${form.username}`)
    const redirect = (route.query.redirect as string) || '/dashboard'
    router.replace(redirect)
  } catch {
    // 错误已由 http.ts 提示
  } finally {
    loading.value = false
  }
}

function goSSO() {
  window.location.href = authApi.ssoLoginUrl()
}

onMounted(() => {
  void loadSSOStatus()
  // 提示默认账号（后端 bootstrap 播种）
  hint.value = '本地默认账号 admin / opsguard-admin'
})
</script>

<style scoped>
.login-page {
  position: relative;
  height: 100vh;
  display: flex;
  align-items: center;
  justify-content: center;
  overflow: hidden;
  background: radial-gradient(1200px 600px at 70% 20%, rgba(20, 184, 166, 0.08), transparent 60%),
    var(--og-bg-base);
}

.bg-grid {
  position: absolute;
  inset: 0;
  background-image: linear-gradient(rgba(148, 163, 184, 0.07) 1px, transparent 1px),
    linear-gradient(90deg, rgba(148, 163, 184, 0.07) 1px, transparent 1px);
  background-size: 44px 44px;
  mask-image: radial-gradient(ellipse at center, #000 30%, transparent 75%);
}
.bg-glow {
  position: absolute;
  width: 520px;
  height: 520px;
  left: 50%;
  top: 50%;
  transform: translate(-50%, -50%);
  background: radial-gradient(circle, rgba(20, 184, 166, 0.12), transparent 65%);
  filter: blur(40px);
}

.panel {
  position: relative;
  width: 380px;
  padding: 40px 36px 32px;
  background: rgba(18, 22, 31, 0.86);
  border: 1px solid var(--el-border-color-light);
  border-radius: 16px;
  backdrop-filter: blur(14px);
  box-shadow: 0 24px 60px rgba(0, 0, 0, 0.5);
  animation: rise 0.5s cubic-bezier(0.16, 1, 0.3, 1) both;
}

@keyframes rise {
  from {
    opacity: 0;
    transform: translateY(14px);
  }
  to {
    opacity: 1;
    transform: translateY(0);
  }
}

.brand {
  display: flex;
  align-items: center;
  gap: 14px;
  margin-bottom: 28px;
}
.brand-mark {
  display: grid;
  place-items: center;
  width: 46px;
  height: 46px;
  border-radius: 12px;
  background: linear-gradient(135deg, var(--og-accent), #0e7490);
  color: #04120f;
  font-weight: 800;
  font-size: 17px;
  letter-spacing: 0.02em;
}
.brand-name {
  margin: 0;
  font-size: 22px;
  font-weight: 700;
  letter-spacing: -0.02em;
}
.brand-tagline {
  margin: 2px 0 0;
  color: var(--og-text-dim);
  font-size: 12px;
}

.submit {
  width: 100%;
  margin-top: 4px;
}
.sso {
  --el-button-bg-color: transparent;
  --el-button-border-color: var(--el-border-color);
  --el-button-text-color: var(--el-text-color-regular);
}
.divider {
  margin: 20px 0 16px;
}
.hint {
  margin-top: 18px;
  text-align: center;
  color: var(--og-text-dim);
  font-size: 12px;
}
.no-local {
  text-align: center;
  padding: 10px 0;
}
</style>
