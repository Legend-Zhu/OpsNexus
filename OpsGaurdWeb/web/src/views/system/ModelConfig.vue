<template>
  <div>
    <!-- 模型池：Provider + 模型清单（唯一配置模型的地方） -->
    <el-alert type="info" :closable="false" class="mb"
      title="模型池统一配置"
      description="智能巡检、异常排查、AI 排查网关的模型都来自这里。新增 Provider 后可在「AI 排查网关」tab 选择默认模型；模型的运营启停（禁用不断流）在「MLOps → 模型」页面。" />

    <div v-for="(p, i) in form.providers" :key="i" class="provider-box">
      <div class="provider-head">
        <span class="provider-title">Provider {{ i + 1 }}</span>
        <span>
          <el-button link type="primary" :loading="testing === i" @click="testProvider(i)">测试连接</el-button>
          <el-button link type="danger" @click="form.providers.splice(i, 1)">移除</el-button>
        </span>
      </div>
      <el-form label-width="90px" class="provider-form">
        <el-form-item label="名称">
          <el-input v-model="p.name" placeholder="如 deepseek" />
        </el-form-item>
        <el-form-item label="类型">
          <el-select v-model="p.type" style="width: 220px">
            <el-option label="OpenAI 兼容" value="openai_compatible" />
            <el-option label="Anthropic 兼容" value="anthropic_compatible" />
          </el-select>
        </el-form-item>
        <el-form-item label="Base URL">
          <el-input v-model="p.base_url" placeholder="如 https://api.deepseek.com/v1" class="mono" />
        </el-form-item>
        <el-form-item label="API Key">
          <el-input
            v-model="p.api_key"
            type="password"
            show-password
            :placeholder="p.api_key_set ? '已设置（留空 = 保持）' : '必填'"
            class="mono"
          />
        </el-form-item>
        <el-form-item label="模型">
          <div class="models-wrap">
            <div v-for="(m, mi) in p.models" :key="mi" class="model-row">
              <el-input v-model="m.name" placeholder="模型名，如 deepseek-chat" class="mono model-name" />
              <el-input v-model="m.display_name" placeholder="显示名（可选）" class="model-display" />
              <el-input-number v-model="m.max_tokens" :min="0" placeholder="max_tokens" controls-position="right" />
              <el-input-number v-model="m.temperature" :min="0" :max="2" :step="0.1" controls-position="right" />
              <el-button link type="danger" :icon="Delete" @click="p.models.splice(mi, 1)" />
            </div>
            <el-button size="small" :icon="Plus" @click="p.models.push({ name: '' })">添加模型</el-button>
          </div>
        </el-form-item>
      </el-form>
    </div>
    <el-button class="mb" size="small" :icon="Plus" @click="addProvider">添加 Provider</el-button>

    <!-- 保存 -->
    <div class="save-bar">
      <el-button type="primary" :loading="saving" @click="save">保存模型配置</el-button>
      <el-button :loading="loading" @click="load">重置</el-button>
    </div>
  </div>
</template>

<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { Delete, Plus } from '@element-plus/icons-vue'
import { ainexusApi } from '@/api'
import type { AINexusModelSpec, AINexusProvider } from '@/types'

const loading = ref(false)
const saving = ref(false)
const testing = ref(-1)

/** 表单模型（provider 可编辑；api_key 留空 = 沿用已保存值） */
const form = reactive<{ providers: AINexusProvider[] }>({ providers: [] as AINexusProvider[] })

async function load() {
  loading.value = true
  try {
    const cfg = await ainexusApi.config()
    form.providers = cfg.providers.map((p) => ({
      ...p,
      api_key: '', // 仅标记已设置；编辑时留空 = 保持
      models: p.models.map((m) => ({ ...m })),
    }))
  } catch {
    // 错误已由 http.ts 提示
  } finally {
    loading.value = false
  }
}

function addProvider() {
  form.providers.push({ name: '', type: 'openai_compatible', base_url: '', api_key: '', models: [{ name: '' }] })
}

async function save() {
  saving.value = true
  try {
    // 只改模型池（providers），其余字段（启用/默认模型/工具/Agent/MCP）保留
    const cfg = await ainexusApi.config()
    cfg.providers = form.providers.map((p) => ({
      name: p.name,
      type: p.type,
      base_url: p.base_url,
      api_key: p.api_key ?? '',
      models: p.models.map((m: AINexusModelSpec) => ({ ...m })),
    }))
    await ainexusApi.updateConfig(cfg)
    ElMessage.success('模型配置已保存（网关与智能巡检模型下拉即时生效）')
    await load()
  } catch {
    // 错误已由 http.ts 提示
  } finally {
    saving.value = false
  }
}

async function testProvider(i: number) {
  const p = form.providers[i]
  const model = p.models[0]?.name
  if (!p.base_url || !model) {
    ElMessage.warning('请先填写 Base URL 与至少一个模型名')
    return
  }
  if (!p.api_key && !p.api_key_set) {
    ElMessage.warning('请填写 API Key')
    return
  }
  testing.value = i
  try {
    const resp = await ainexusApi.testConfig({
      name: p.name,
      type: p.type,
      base_url: p.base_url,
      api_key: p.api_key || undefined,
      model,
    })
    if (resp.ok) {
      ElMessage.success(`连接成功（${resp.latency_ms}ms，模型 ${resp.model}）`)
    } else {
      ElMessage.error(`连接失败：${resp.error ?? '未知错误'}`)
    }
  } catch {
    // 错误已由 http.ts 提示
  } finally {
    testing.value = -1
  }
}

onMounted(load)
</script>

<style scoped>
.provider-box {
  border: 1px solid var(--el-border-color-lighter);
  border-radius: 6px;
  padding: 8px 12px 0;
  margin-bottom: 12px;
}
.provider-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: 6px;
}
.provider-title {
  font-weight: 600;
}
.provider-form {
  max-width: 720px;
}
.models-wrap {
  width: 100%;
}
.model-row {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 8px;
  flex-wrap: wrap;
}
.model-name {
  width: 220px;
}
.model-display {
  width: 160px;
}
.mb {
  margin-bottom: 12px;
}
.save-bar {
  margin-top: 16px;
}
.mono {
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
}
</style>
