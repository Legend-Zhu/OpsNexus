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
          <el-form-item
            :label="it.type === 'host-service' ? 'IP 地址' : '容器名'"
            required
            :error="itemErrors[i]?.ref"
          >
            <el-input
              v-model="it.ref"
              size="small"
              :placeholder="it.type === 'host-service' ? '如 10.0.0.10' : '容器名，如 r-nacos'"
            />
          </el-form-item>
          <el-form-item
            label="node"
            :required="it.type === 'standalone-container'"
            :error="itemErrors[i]?.node"
          >
            <el-input v-model="it.node" size="small" placeholder="节点 hostname（如 node-01）" />
          </el-form-item>
          <el-form-item label="端口" :error="itemErrors[i]?.ports">
            <el-select
              v-model="it.ports"
              size="small"
              multiple
              filterable
              allow-create
              default-first-option
              placeholder="如 8848、9848、9849（可多端口）"
              style="width: 100%"
            >
              <el-option v-for="p in it.ports ?? []" :key="p" :label="p" :value="p" />
            </el-select>
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
        <div v-else class="inv-tip">host-service 填 IP 地址 + 端口（可多个），探活从 node 节点（留空 = 从 manager）发起</div>
        <el-collapse class="inv-mon">
          <el-collapse-item name="mon">
            <template #title>
              <span class="inv-mon-title">监控配置</span>
              <el-tag v-if="it.monitoring?.enabled" size="small" type="success" effect="plain">已启用</el-tag>
              <span v-else class="inv-mon-hint">可选：端口/HTTP/容器存活探测，保存后由 server 周期执行</span>
            </template>
            <el-input
              v-model="it.__monDraft"
              type="textarea"
              :rows="5"
              class="mono"
              placeholder="monitoring YAML（enabled 须为 true 才会执行）；可点「插入示例」填入示例后修改"
            />
            <div class="cfg-actions">
              <el-button link type="primary" size="small" :icon="MagicStick" @click="fillMonSample(it)">插入示例</el-button>
            </div>
            <div class="inv-tip">
              monitoring YAML（enabled 须为 true 才会执行）；纳管对象支持 portChecks / httpChecks / 容器存活，
              资源阈值与日志检查仅 swarm 服务由 Worker 执行
            </div>
            <div v-if="itemErrors[i]?.monitoring" class="inv-mon-error">{{ itemErrors[i].monitoring }}</div>
          </el-collapse-item>
        </el-collapse>
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
        placeholder="items 数组 YAML——切换到 YAML 时会自动带出当前清单，可直接修改后保存"
        @blur="syncYamlToForm"
      />
      <div class="inv-tip">YAML 结构：items 数组，每项含 name / type / ref / node（standalone 必填）/ ports / category / desc / monitoring（可选，schema 同告警规则）；切换回表单时会解析并校验</div>
    </template>
  </div>
</template>

<script setup lang="ts">
import { computed, nextTick, ref, watch } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Delete, MagicStick, Plus } from '@element-plus/icons-vue'
import { load as yamlLoad, dump as yamlDump } from 'js-yaml'
import type { InventoryConfig, InventoryItem, Monitoring } from '@/types'

/** 表单条目：InventoryItem + 监控配置草稿（__monDraft，textarea 绑定用；
 * 保存时解析回 monitoring 字段，提交前剥离） */
type FormItem = InventoryItem & { __monDraft?: string }

/** 监控草稿示例（「插入示例」填入，可编辑/复制；schema 同告警规则。
 *  不默认预填——插入即启用监控，需用户主动触发） */
const MON_SAMPLE = `enabled: true                     # 须为 true 才会执行探测
portChecks:                       # TCP 端口探测，连续失败发 port_down
  - { port: "8848", interval: 30s }
httpChecks:                       # HTTP 健康检查，异常发 http_unhealthy
  - { url: "http://10.0.0.1:8848/nacos/v1/console/health/readiness", expectedStatus: [200], interval: 60s }`

/** 填入监控示例（覆盖当前草稿；不保存可通过清空草稿丢弃） */
function fillMonSample(it: FormItem) {
  it.__monDraft = MON_SAMPLE
}

const props = defineProps<{
  /** 当前清单（打开编辑器时的快照；null = 空清单） */
  config: InventoryConfig | null
}>()
const emit = defineEmits<{
  /** 组件内校验通过（含空清单二次确认）后提交 */
  save: [config: InventoryConfig]
}>()

const mode = ref<'form' | 'yaml'>('form')
const items = ref<FormItem[]>([])
const yamlText = ref('')
/** 行级错误：{ [index]: { name?, type?, ref?, node?, category?, monitoring? } } */
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

function cloneItems(list: InventoryItem[]): FormItem[] {
  return list.map((it) => {
    const copy: FormItem = { ...it, monitoring: it.monitoring ? { ...it.monitoring } : undefined }
    initMonDraft(copy)
    return copy
  })
}

/** 监控草稿初始化：已有 monitoring → dump 成 YAML 供 textarea 编辑 */
function initMonDraft(it: FormItem) {
  it.__monDraft = it.monitoring ? yamlDump(it.monitoring, { indent: 2, lineWidth: 120 }).trimEnd() : ''
}

function dumpItems(list: FormItem[]): string {
  // 空 ports 数组不落 YAML（避免无意义的 `ports: []`）；__monDraft 为表单
  // 草稿不导出（YAML 模式以 monitoring 字段为准）
  const clean = list.map((it) => {
    const copy = { ...it }
    delete copy.__monDraft
    if (!copy.ports?.length) delete copy.ports
    if (!copy.monitoring) delete copy.monitoring
    return copy
  })
  return yamlDump({ items: clean }, { indent: 2, lineWidth: 120 })
}

function addItem() {
  const it: FormItem = {
    name: '',
    type: 'standalone-container',
    ref: '',
    node: '',
    ports: [],
    category: '',
    desc: '',
  }
  initMonDraft(it)
  items.value.push(it)
}

function removeItem(i: number) {
  items.value.splice(i, 1)
  delete itemErrors.value[i]
}

// 示例模板：一个 standalone 容器（r-nacos，多端口 + 端口/HTTP 探测）+
// 一个宿主机端口服务（grafana，端口探测）。monitoring schema 同告警规则，
// enabled: true 保存后由 server 侧周期探测执行。
const SAMPLE: InventoryItem[] = [
  {
    name: 'r-nacos',
    type: 'standalone-container',
    ref: 'r-nacos',
    node: 'node-01',
    ports: ['8848', '9848', '9849'],
    category: 'middleware',
    desc: '注册中心（docker run 部署）',
    monitoring: {
      enabled: true,
      portChecks: [
        { port: '8848', interval: '30s' },
        { port: '9848', interval: '30s' },
      ],
      httpChecks: [
        { url: 'http://10.0.0.1:8848/nacos/v1/console/health/readiness', expectedStatus: [200], interval: '60s' },
      ],
    },
  },
  {
    name: 'grafana',
    type: 'host-service',
    ref: '10.0.0.10',
    ports: ['3000'],
    node: '',
    category: 'middleware',
    desc: '监控面板（宿主机端口探活）',
    monitoring: {
      enabled: true,
      portChecks: [{ port: '3000', interval: '30s' }],
    },
  },
]

function insertSample() {
  let added = 0
  for (const s of SAMPLE) {
    if (items.value.some((it) => it.name === s.name)) continue
    const copy: FormItem = { ...s, monitoring: s.monitoring ? { ...s.monitoring } : undefined }
    initMonDraft(copy)
    items.value.push(copy)
    added++
  }
  if (added > 0) {
    ElMessage.success(`已插入 ${added} 条示例，按需修改后保存`)
  } else {
    ElMessage.info('示例条目已存在，无需重复插入')
  }
}

// ---- 校验 ----
function validateItems(list: FormItem[]): string | null {
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
    } else if (it.type === 'host-service' && !it.ref.includes(':') && !(it.ports ?? []).length) {
      errs.ref = 'host-service 需填 IP 并在「端口」里至少配一个端口'
    }
    if (it.type === 'standalone-container' && !it.node?.trim()) errs.node = 'standalone 容器必填 node'
    // 端口格式：数字 1-65535
    const badPort = (it.ports ?? []).find((p) => !/^\d{1,5}$/.test(p) || Number(p) < 1 || Number(p) > 65535)
    if (badPort) errs.ports = `端口 ${badPort} 非法（1-65535）`
    // 监控草稿：非空 → 解析回 monitoring（对象才行）；置空 → 清除 monitoring
    const draft = (it.__monDraft ?? '').trim()
    if (draft) {
      try {
        const parsed = yamlLoad(draft)
        if (parsed == null || typeof parsed !== 'object' || Array.isArray(parsed)) {
          errs.monitoring = 'monitoring 须为 YAML 对象'
        } else {
          it.monitoring = parsed as Monitoring
          if (!it.monitoring.enabled) errs.monitoring = 'enabled 须为 true（否则保存后不会执行探测）'
        }
      } catch (e: any) {
        errs.monitoring = 'YAML 解析失败：' + (e?.message ?? String(e))
      }
    } else {
      delete it.monitoring
    }
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

function parseYamlList(): FormItem[] | null {
  try {
    const obj = yamlLoad(yamlText.value) as { items?: unknown }
    const list = Array.isArray(obj?.items) ? (obj.items as InventoryItem[]) : []
    return list.map((it) => {
      const copy: FormItem = {
        name: String(it.name ?? ''),
        type: String(it.type ?? '') as InventoryItem['type'],
        ref: String(it.ref ?? ''),
        node: String(it.node ?? ''),
        ports: normalizePorts(it.ports),
        category: String(it.category ?? ''),
        desc: String(it.desc ?? ''),
        // monitoring 全字段保留（此前版本静默丢弃，YAML 往返会丢监控配置）
        monitoring:
          it.monitoring && typeof it.monitoring === 'object' && !Array.isArray(it.monitoring)
            ? (it.monitoring as Monitoring)
            : undefined,
      }
      initMonDraft(copy)
      return copy
    })
  } catch (e: any) {
    ElMessage.error('YAML 解析失败: ' + (e?.message ?? String(e)))
    return null
  }
}

// 端口字段归一化：接受 string[]、逗号分隔串或单端口数字
function normalizePorts(ports: unknown): string[] {
  if (Array.isArray(ports)) {
    return ports.map((p) => String(p).trim()).filter(Boolean)
  }
  if (typeof ports === 'string' || typeof ports === 'number') {
    return String(ports)
      .split(/[,，\s]+/)
      .map((p) => p.trim())
      .filter(Boolean)
  }
  return []
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
  // 提交前剥离表单草稿字段（__monDraft 不属于 InventoryItem 契约）
  const payload: InventoryItem[] = items.value.map((it) => {
    const copy = { ...it }
    delete copy.__monDraft
    return copy
  })
  emit('save', { items: payload })
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
