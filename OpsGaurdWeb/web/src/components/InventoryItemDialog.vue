<template>
  <div class="inv-item-editor">
    <el-form label-width="86px" label-position="right" @submit.prevent>
      <el-form-item label="类型" required :error="errors.type">
        <el-radio-group v-model="form.type" @change="onTypeChange">
          <el-radio-button value="standalone-container">standalone 容器</el-radio-button>
          <el-radio-button value="host-service">宿主机服务</el-radio-button>
        </el-radio-group>
      </el-form-item>
      <el-form-item label="名称" required :error="errors.name">
        <el-input v-model="form.name" placeholder="唯一名，如 r-nacos" maxlength="64" />
      </el-form-item>
      <el-form-item :label="form.type === 'host-service' ? 'IP 地址' : '容器名'" required :error="errors.ref">
        <el-input
          v-model="form.ref"
          :placeholder="form.type === 'host-service' ? '如 10.0.0.10' : 'docker ps 里的容器名，如 r-nacos'"
        />
      </el-form-item>
      <el-form-item label="所在节点" :required="form.type === 'standalone-container'" :error="errors.node">
        <el-select
          v-model="form.node"
          filterable
          allow-create
          default-first-option
          clearable
          :placeholder="form.type === 'host-service' ? '探活发起节点（留空 = manager 本机）' : '容器所在节点 hostname'"
          style="width: 100%"
        >
          <el-option v-for="n in nodeOptions" :key="n" :label="n" :value="n" />
        </el-select>
      </el-form-item>
      <el-form-item label="端口" :error="errors.ports">
        <el-select
          v-model="form.ports"
          multiple
          filterable
          allow-create
          default-first-option
          placeholder="如 8848、9848（可多个，回车添加）"
          style="width: 100%"
        >
          <el-option v-for="p in form.ports" :key="p" :label="p" :value="p" />
        </el-select>
      </el-form-item>
      <el-form-item label="分类" :error="errors.category">
        <el-select
          v-model="form.category"
          filterable
          allow-create
          default-first-option
          clearable
          placeholder="middleware / business / infra / service"
          style="width: 100%"
        >
          <el-option label="middleware（中间件）" value="middleware" />
          <el-option label="business（业务）" value="business" />
          <el-option label="infra（基础设施）" value="infra" />
          <el-option label="service（服务）" value="service" />
        </el-select>
      </el-form-item>
      <el-form-item label="描述">
        <el-input v-model="form.desc" placeholder="可选" maxlength="200" />
      </el-form-item>
    </el-form>
    <div class="inv-tip">
      {{ typeTip }}
    </div>

    <el-collapse v-model="monOpen" class="inv-mon">
      <el-collapse-item name="mon">
        <template #title>
          <span class="inv-mon-title">监控配置</span>
          <el-tag v-if="form.monitoring?.enabled" size="small" type="success" effect="plain">已启用</el-tag>
          <span v-else class="inv-mon-hint">可选：端口/HTTP 探测，保存后由 server 周期执行</span>
        </template>
        <el-input
          v-model="monDraft"
          type="textarea"
          :rows="6"
          class="mono"
          placeholder="monitoring YAML（enabled 须为 true 才会执行）；可点「插入示例」填入示例后修改；清空 = 不监控"
        />
        <div class="cfg-actions">
          <el-button link type="primary" size="small" :icon="MagicStick" @click="fillMonSample">插入示例</el-button>
        </div>
        <div class="inv-tip">
          monitoring YAML（enabled 须为 true 才会执行）；纳管对象支持 portChecks / httpChecks / 容器存活，
          资源阈值与日志检查仅 swarm 服务由 Worker 执行
        </div>
        <div v-if="errors.monitoring" class="inv-mon-error">{{ errors.monitoring }}</div>
      </el-collapse-item>
    </el-collapse>
  </div>
</template>

<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import { MagicStick } from '@element-plus/icons-vue'
import { load as yamlLoad, dump as yamlDump } from 'js-yaml'
import type { InventoryItem, Monitoring } from '@/types'

/**
 * 单条纳管对象的表单编辑器（新增/编辑共用），由父组件放进 el-dialog：
 * - add：空表单起步（standalone 类型默认选中）
 * - edit：由 item 快照回填；改名允许，重名校验排除自身
 * 监控配置沿用 YAML 草稿（textarea）交互；保存经 doSave() 校验后 emit。
 */
const props = defineProps<{
  /** add | edit */
  mode: 'add' | 'edit'
  /** 编辑时的条目快照（add 时忽略） */
  item?: InventoryItem | null
  /** 当前清单里已占用的名称（重名校验用；编辑时自动排除自身） */
  existingNames?: string[]
  /** 节点下拉选项（集群节点 hostname 列表） */
  nodeOptions?: string[]
}>()

const emit = defineEmits<{
  /** 校验通过后提交完整条目 */
  save: [item: InventoryItem]
}>()

// 表单态：node / desc 固定为 string（编辑快照里的可选字段回填为空串）
type InventoryItemForm = InventoryItem & { node: string; desc: string }

const form = reactive<InventoryItemForm>({
  name: '',
  type: 'standalone-container',
  ref: '',
  node: '',
  ports: [],
  category: '',
  desc: '',
})
const monDraft = ref('')
const errors = reactive<Record<string, string>>({})
const monOpen = ref<string[]>([])

// 打开时回填（父组件用 :key 重建实例，这里只需初始化一次）
watch(
  () => props.item,
  (it) => {
    if (!it) return
    Object.assign(form, {
      name: it.name,
      type: it.type,
      ref: it.ref,
      node: it.node ?? '',
      ports: [...(it.ports ?? [])],
      category: it.category ?? '',
      desc: it.desc ?? '',
      monitoring: it.monitoring ? { ...it.monitoring } : undefined,
    })
    if (it.monitoring) {
      monDraft.value = yamlDump(it.monitoring, { indent: 2, lineWidth: 120 }).trimEnd()
      monOpen.value = ['mon']
    }
  },
  { immediate: true },
)

const typeTip = computed(() =>
  form.type === 'standalone-container'
    ? 'standalone 容器：OpsGaurd 之外 docker run 起的容器。需填容器名 + 所在节点 hostname（用于定位查询），端口可选（未填取容器实况）。'
    : '宿主机服务：systemd / 裸进程 / 未容器化端口。填 IP + 端口做探活，探活从所选节点（留空 = manager 本机）发起。',
)

function onTypeChange() {
  // 类型切换后 ref 语义不同（容器名 ↔ IP），旧值大概率无意义，清空引导重填
  form.ref = ''
}

const MON_SAMPLE = `enabled: true                     # 须为 true 才会执行探测
portChecks:                       # TCP 端口探测，连续失败发 port_down
  - { port: "8848", interval: 30s }
httpChecks:                       # HTTP 健康检查，异常发 http_unhealthy
  - { url: "http://10.0.0.1:8848/nacos/v1/console/health/readiness", expectedStatus: [200], interval: 60s }`

function fillMonSample() {
  monDraft.value = MON_SAMPLE
  if (!monOpen.value.includes('mon')) monOpen.value = ['mon']
}

function validate(): boolean {
  Object.keys(errors).forEach((k) => delete errors[k])
  const name = form.name.trim()
  if (!name) errors.name = '必填'
  else if ((props.existingNames ?? []).some((n) => n === name && !(props.mode === 'edit' && props.item?.name === name))) {
    errors.name = '名称已被其他纳管对象占用'
  }
  if (!form.ref.trim()) errors.ref = '必填'
  if (form.type === 'standalone-container' && !form.node.trim()) errors.node = 'standalone 容器必填所在节点'
  const badPort = (form.ports ?? []).find((p) => !/^\d{1,5}$/.test(p) || Number(p) < 1 || Number(p) > 65535)
  if (badPort) errors.ports = `端口 ${badPort} 非法（1-65535）`
  if (form.type === 'host-service' && !form.ref.includes(':') && !(form.ports ?? []).length) {
    errors.ref = 'host-service 需填 IP 并至少配一个端口'
  }
  // 监控草稿：非空 → 解析回 monitoring；置空 → 清除
  const draft = monDraft.value.trim()
  if (draft) {
    try {
      const parsed = yamlLoad(draft)
      if (parsed == null || typeof parsed !== 'object' || Array.isArray(parsed)) {
        errors.monitoring = 'monitoring 须为 YAML 对象'
      } else {
        form.monitoring = parsed as Monitoring
        if (!form.monitoring.enabled) errors.monitoring = 'enabled 须为 true（否则保存后不会执行探测）'
      }
    } catch (e: any) {
      errors.monitoring = 'YAML 解析失败：' + (e?.message ?? String(e))
    }
  } else {
    delete form.monitoring
  }
  return Object.keys(errors).length === 0
}

/** 对话框「保存」按钮入口：校验通过后 emit save */
function doSave() {
  if (!validate()) return
  emit('save', {
    ...form,
    name: form.name.trim(),
    ref: form.ref.trim(),
    node: form.node.trim(),
    desc: form.desc.trim(),
    ports: (form.ports ?? []).filter(Boolean),
  })
}

defineExpose({ doSave })
</script>

<style scoped>
.inv-tip {
  font-size: 12px;
  color: var(--og-text-dim);
  line-height: 1.6;
  margin: 2px 0 10px;
}
.inv-mon {
  margin-bottom: 12px;
}
.cfg-actions {
  display: flex;
  align-items: center;
  gap: 8px;
  margin: 2px 0 6px;
}
.inv-mon-title {
  font-size: 13px;
  margin-right: 8px;
}
.inv-mon-hint {
  font-size: 12px;
  color: var(--og-text-dim);
  font-weight: normal;
}
.inv-mon-error {
  font-size: 12px;
  color: var(--el-color-danger);
  margin: 4px 0 10px;
}
.inv-mon :deep(.el-collapse-item__header) {
  height: 36px;
}
</style>
