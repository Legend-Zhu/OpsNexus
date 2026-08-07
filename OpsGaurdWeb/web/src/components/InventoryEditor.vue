<template>
  <div class="inv-editor">
    <div class="inv-mode-bar">
      <el-radio-group v-model="boundMode" size="small">
        <el-radio-button value="form">表单</el-radio-button>
        <el-radio-button value="yaml">YAML</el-radio-button>
      </el-radio-group>
      <el-button size="small" :icon="Plus" @click="addItem">添加条目</el-button>
      <el-button size="small" :icon="MagicStick" @click="insertSample">插入示例</el-button>
    </div>

    <!-- 表单模式：条目列表 -->
    <template v-if="mode === 'form'">
      <div v-for="(it, i) in items" :key="i" class="inv-item" :class="{ 'inv-item-error': itemErrors[i] }">
        <div class="inv-item-head">
          <span class="inv-item-index">{{ i + 1 }}</span>
          <el-button link type="danger" :icon="Delete" @click="removeItem(i)">删除</el-button>
        </div>
        <div class="inv-item-grid">
          <el-form-item label="名称" required :error="itemErrors[i]?.name">
            <el-input v-model="it.name" placeholder="如 r-nacos" size="small" />
          </el-form-item>
          <el-form-item label="类型" required :error="itemErrors[i]?.type">
            <el-select v-model="it.type" size="small" style="width: 100%">
              <el-option label="standalone-container（docker run 容器）" value="standalone-container" />
              <el-option label="host-service（宿主机端口服务）" value="host-service" />
            </el-select>
          </el-form-item>
          <el-form-item label="ref" required :error="itemErrors[i]?.ref">
            <el-input
              v-model="it.ref"
              size="small"
              :placeholder="it.type === 'host-service' ? '如 10.0.0.10:3000' : '容器名，如 r-nacos'"
            />
          </el-form-item>
          <el-form-item
            label="node"
            :required="it.type === 'standalone-container'"
            :error="itemErrors[i]?.node"
          >
            <el-input v-model="it.node" size="small" placeholder="节点 hostname（如 node-01）" />
          </el-form-item>
          <el-form-item label="分类" :error="itemErrors[i]?.category">
            <el-select
              v-model="it.category"
              size="small"
              allow-create
              filterable
              default-first-option
              placeholder="middleware / business / infra…"
              style="width: 100%"
            >
              <el-option label="middleware（中间件）" value="middleware" />
              <el-option label="business（业务）" value="business" />
              <el-option label="infra（基础设施）" value="infra" />
              <el-option label="service（服务）" value="service" />
            </el-select>
          </el-form-item>
          <el-form-item label="描述">
            <el-input v-model="it.desc" size="small" placeholder="可选" />
          </el-form-item>
        </div>
        <div v-if="it.type === 'standalone-container'" class="inv-tip">
          standalone-container 需同时填写容器所在节点的 hostname（node），用于定位并查询该容器
        </div>
        <div v-else class="inv-tip">host-service 的 ref 为 "host:port"，探活从 node 节点（留空 = 从 manager）发起</div>
      </div>
      <el-empty v-if="!items.length" description="尚未声明任何纳管对象，点「添加条目」或「插入示例」开始" :image-size="60" />
    </template>

    <!-- YAML 模式 -->
    <template v-else>
      <el-input
        v-model="yamlText"
        type="textarea"
        :rows="14"
        class="mono"
        placeholder="items:&#10;  - name: r-nacos&#10;    type: standalone-container&#10;    ref: r-nacos&#10;    node: node-01&#10;    category: middleware"
        @blur="syncYamlToForm"
      />
      <div class="inv-tip">YAML 结构：items 数组，每项含 name / type / ref / node（standalone 必填）/ category / desc；切换回表单时会解析并校验</div>
    </template>
  </div>
</template>

<script setup lang="ts">
import { computed, nextTick, ref, watch } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Delete, MagicStick, Plus } from '@element-plus/icons-vue'
import { load as yamlLoad, dump as yamlDump } from 'js-yaml'
import type { InventoryConfig, InventoryItem } from '@/types'

const props = defineProps<{
  /** 当前清单（打开编辑器时的快照；null = 空清单） */
  config: InventoryConfig | null
}>()
const emit = defineEmits<{
  /** 组件内校验通过（含空清单二次确认）后提交 */
  save: [config: InventoryConfig]
}>()

const mode = ref<'form' | 'yaml'>('form')
const items = ref<InventoryItem[]>([])
const yamlText = ref('')
/** 行级错误：{ [index]: { name?, type?, ref?, node?, category? } } */
const itemErrors = ref<Record<number, Record<string, string>>>({})

const VALID_TYPES = new Set(['standalone-container', 'host-service'])

// 打开/切换集群时重置编辑器内容。组件由父组件用 :key 强制重建（每次打开
// 都是全新实例），这里只需做初始加载；空清单时自动预填示例条目引导填写。
watch(
  () => props.config,
  (cfg) => {
    const list = cloneItems(cfg?.items ?? [])
    if (!list.length) {
      list.push(...cloneItems(SAMPLE))
      // 预填提示延后到渲染后弹出（避免与父组件打开流程的提示冲突）
      void nextTick(() => {
        ElMessage.info('清单为空——已预填示例条目（r-nacos / grafana）供参考，请按实际修改后保存')
      })
    }
    items.value = list
    yamlText.value = dumpItems(list)
    itemErrors.value = {}
  },
  { immediate: true },
)

function cloneItems(list: InventoryItem[]): InventoryItem[] {
  return list.map((it) => ({ ...it }))
}

function dumpItems(list: InventoryItem[]): string {
  return yamlDump({ items: list }, { indent: 2, lineWidth: 120 })
}

function addItem() {
  items.value.push({
    name: '',
    type: 'standalone-container',
    ref: '',
    node: '',
    category: '',
    desc: '',
  })
}

function removeItem(i: number) {
  items.value.splice(i, 1)
  delete itemErrors.value[i]
}

// 示例模板：一个 standalone 容器（r-nacos）+ 一个宿主机端口服务
const SAMPLE: InventoryItem[] = [
  {
    name: 'r-nacos',
    type: 'standalone-container',
    ref: 'r-nacos',
    node: 'node-01',
    category: 'middleware',
    desc: '注册中心（docker run 部署）',
  },
  {
    name: 'grafana',
    type: 'host-service',
    ref: '10.0.0.10:3000',
    node: '',
    category: 'middleware',
    desc: '监控面板（宿主机端口探活）',
  },
]

function insertSample() {
  let added = 0
  for (const s of SAMPLE) {
    if (items.value.some((it) => it.name === s.name)) continue
    items.value.push({ ...s })
    added++
  }
  if (added > 0) {
    ElMessage.success(`已插入 ${added} 条示例，按需修改后保存`)
  } else {
    ElMessage.info('示例条目已存在，无需重复插入')
  }
}

// ---- 校验 ----
function validateItems(list: InventoryItem[]): string | null {
  const seen = new Set<string>()
  for (let i = 0; i < list.length; i++) {
    const it = list[i]
    const errs: Record<string, string> = {}
    if (!it.name?.trim()) errs.name = '必填'
    else if (seen.has(it.name.trim())) errs.name = `与第 ${list.findIndex((x) => x.name === it.name) + 1} 条重名`
    else seen.add(it.name.trim())

    if (!VALID_TYPES.has(it.type)) errs.type = '类型非法'
    if (!it.ref?.trim()) {
      errs.ref = '必填'
    } else if (it.type === 'host-service' && !it.ref.includes(':')) {
      errs.ref = 'host-service 需为 host:port'
    }
    if (it.type === 'standalone-container' && !it.node?.trim()) errs.node = 'standalone 容器必填 node'
    if (Object.keys(errs).length) itemErrors.value[i] = errs
    else delete itemErrors.value[i]
  }
  const hasErr = Object.keys(itemErrors.value).length > 0
  return hasErr ? '存在校验错误，请修正后保存' : null
}

// ---- 模式切换（双向同步） ----
function switchToYaml() {
  yamlText.value = dumpItems(items.value)
  mode.value = 'yaml'
}

function parseYamlList(): InventoryItem[] | null {
  try {
    const obj = yamlLoad(yamlText.value) as { items?: unknown }
    const list = Array.isArray(obj?.items) ? (obj.items as InventoryItem[]) : []
    return list.map((it) => ({
      name: String(it.name ?? ''),
      type: String(it.type ?? '') as InventoryItem['type'],
      ref: String(it.ref ?? ''),
      node: String(it.node ?? ''),
      category: String(it.category ?? ''),
      desc: String(it.desc ?? ''),
    }))
  } catch (e: any) {
    ElMessage.error('YAML 解析失败: ' + (e?.message ?? String(e)))
    return null
  }
}

function syncYamlToForm() {
  const list = parseYamlList()
  if (!list) return
  itemErrors.value = {}
  items.value = list
  mode.value = 'form'
}

// 模板里 radio 切换时同步（双向绑定直接改 mode；这里拦截切换做同步）
const boundMode = computed({
  get: () => mode.value,
  set: (v) => {
    if (v === mode.value) return
    if (v === 'yaml') switchToYaml()
    else syncYamlToForm()
  },
})

// ---- 保存 ----
async function doSave() {
  if (mode.value === 'yaml') {
    // YAML 模式：先解析再校验（成功即同步回表单）
    const list = parseYamlList()
    if (!list) return
    items.value = list
    itemErrors.value = {}
  }
  const err = validateItems(items.value)
  if (err) {
    ElMessage.error(err)
    return
  }
  // 防误清空：原清单非空而新清单为空时二次确认
  const hadItems = (props.config?.items?.length ?? 0) > 0
  if (items.value.length === 0 && hadItems) {
    try {
      await ElMessageBox.confirm('新清单为空，将清空当前全部纳管对象声明，确定？', '清空纳管清单', {
        type: 'warning',
      })
    } catch {
      return
    }
  }
  emit('save', { items: items.value })
}

defineExpose({ doSave })
</script>

<style scoped>
.inv-mode-bar {
  display: flex;
  align-items: center;
  gap: 10px;
  margin-bottom: 12px;
}
.inv-item {
  border: 1px solid var(--el-border-color-light);
  border-radius: 10px;
  padding: 12px 14px 4px;
  margin-bottom: 12px;
  background: var(--og-bg-surface);
}
.inv-item-error {
  border-color: var(--el-color-danger-light-5);
}
.inv-item-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: 6px;
}
.inv-item-index {
  font-size: 12px;
  color: var(--og-text-dim);
  font-weight: 600;
}
.inv-item-grid {
  display: grid;
  grid-template-columns: 1fr 1fr;
  column-gap: 16px;
}
.inv-item-grid :deep(.el-form-item) {
  margin-bottom: 12px;
}
.inv-tip {
  font-size: 12px;
  color: var(--og-text-dim);
  line-height: 1.6;
  margin: 2px 0 10px;
}
</style>
