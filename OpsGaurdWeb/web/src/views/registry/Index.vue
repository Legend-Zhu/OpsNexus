<template>
  <el-card shadow="never">
    <template #header>
      <div class="card-header">
        <span>镜像仓库（内嵌 Registry）</span>
        <div v-if="info">
          <el-tag v-if="info.docker_available" size="small" type="success">docker 可用</el-tag>
          <el-tooltip v-else content="管理端未检测到 docker CLI，仅构建不可用；推送/拉取不受影响">
            <el-tag size="small" type="warning">构建不可用</el-tag>
          </el-tooltip>
          <el-tag v-if="info.auth_enabled" size="small" type="success" class="ml">已启用认证</el-tag>
          <el-tag v-else size="small" type="danger" class="ml">未启用认证</el-tag>
        </div>
      </div>
    </template>

    <el-alert v-if="!enabled && !loading" type="warning" :closable="false" class="mb"
      title="镜像仓库未启用" description="在 server.yaml 配置 registry.enabled: true 后重启管理端。" />

    <template v-if="info">
      <!-- 使用指引 -->
      <el-collapse class="mb">
        <el-collapse-item name="guide">
          <template #title>
            <span>使用指引（内网 HTTP + 双网段统一寻址，<b>节点必须配置</b>）</span>
          </template>
          <div class="guide">
            <h5>1. 每个集群节点一次性配置（内网无 HTTPS，必须）</h5>
            <pre class="code"># /etc/docker/daemon.json —— 配主机名，双网段通用
{ "insecure-registries": ["{{ registryAddr }}"] }

# /etc/hosts —— 把主机名解析到本网段的管理端地址
&lt;管理端本网段IP&gt;  {{ info.hostname }}
# systemctl reload docker</pre>
            <h5>2. 登录与推拉（10 段内网 / 172 政务外网命令相同）</h5>
            <pre class="code">docker login {{ registryAddr }}          # 账号在 server.yaml registry.users
docker pull {{ registryAddr }}/&lt;name&gt;:&lt;tag&gt;
docker tag  myapp:v1 {{ registryAddr }}/myapp:v1
docker push {{ registryAddr }}/myapp:v1</pre>
            <h5>3. 部署到集群时 image 写法</h5>
            <pre class="code">service:
  image: {{ registryAddr }}/&lt;name&gt;:&lt;tag&gt;   # 永远用统一主机名，不写网段 IP</pre>
          </div>
        </el-collapse-item>
      </el-collapse>

      <!-- 上传构建 -->
      <el-card shadow="never" class="sub-card">
        <template #header>
          <span>上传构建（zip 内含 Dockerfile 与编译产物；Dockerfile 自动定位，无需指定）</span>
        </template>
        <el-form inline class="build-form">
          <el-form-item label="构建包">
            <el-upload :auto-upload="false" accept=".zip" :show-file-list="false" :on-change="onPick">
              <el-button :icon="FolderOpened">选择 zip</el-button>
            </el-upload>
            <template v-if="zipFile">
              <span class="file-name mono">{{ zipFile.name }}（{{ fmtSize(zipFile.size) }}）</span>
              <el-button link type="danger" size="small" @click="zipFile = null">清除</el-button>
            </template>
          </el-form-item>
          <el-form-item label="项目名">
            <el-autocomplete v-model="buildForm.project" :fetch-suggestions="queryProjects" clearable
              placeholder="选择已有命名空间或输入新名" style="width: 200px" />
          </el-form-item>
          <el-form-item label="镜像名">
            <el-input v-model="buildForm.name" placeholder="如 myapp（不含项目前缀）" style="width: 170px" />
          </el-form-item>
          <el-form-item label="Tag">
            <el-input v-model="buildForm.tag" placeholder="如 v1.0.0" style="width: 120px" />
          </el-form-item>
          <el-form-item>
            <el-button type="primary" :loading="building" :disabled="!canBuild" @click="submitBuild">
              开始构建
            </el-button>
          </el-form-item>
        </el-form>
        <div v-if="fullImageRef" class="ref-preview">
          目标镜像：<span class="mono">{{ fullImageRef }}</span>
          <span class="muted">（项目名/镜像名仅允许小写字母、数字、_ . -）</span>
        </div>

        <div v-if="uploading" class="upload-status">
          <span>上传中</span>
          <el-progress :percentage="uploadProgress" :indeterminate="uploadProgress === 0" style="width: 320px" />
          <span class="mono dim">{{ uploadProgress }}%</span>
        </div>

        <!-- 当前构建进度 -->
        <template v-if="currentTask && !uploading">
          <div class="build-status">
            <el-progress :percentage="currentTask.progress < 0 ? 100 : currentTask.progress"
              :status="currentTask.status === 'FAILED' ? 'exception' : currentTask.status === 'SUCCESS' ? 'success' : undefined"
              style="width: 320px" />
            <el-tag size="small" :type="currentTask.status === 'FAILED' ? 'danger' : currentTask.status === 'SUCCESS' ? 'success' : 'warning'">
              {{ currentTask.status }}
            </el-tag>
            <span v-if="currentTask.image" class="mono dim">{{ currentTask.image }}</span>
          </div>
          <pre ref="logBoxRef" class="log-box">{{ (currentTask.logs ?? []).join('\n') }}</pre>
        </template>
      </el-card>

      <!-- 最近构建 -->
      <el-card v-if="builds.length" shadow="never" class="sub-card">
        <template #header><span>最近构建</span></template>
        <el-table :data="builds" size="small">
          <el-table-column label="镜像" min-width="220">
            <template #default="{ row }"><span class="mono">{{ row.image || `${row.name}:${row.tag}` }}</span></template>
          </el-table-column>
          <el-table-column label="状态" width="110">
            <template #default="{ row }">
              <el-tag size="small" :type="row.status === 'FAILED' ? 'danger' : row.status === 'SUCCESS' ? 'success' : 'warning'">{{ row.status }}</el-tag>
            </template>
          </el-table-column>
          <el-table-column label="发起时间" width="170">
            <template #default="{ row }">{{ new Date(row.created_at).toLocaleString() }}</template>
          </el-table-column>
          <el-table-column label="错误" min-width="160">
            <template #default="{ row }"><span class="err-text">{{ row.error || '—' }}</span></template>
          </el-table-column>
          <el-table-column label="操作" width="80">
            <template #default="{ row }"><el-button link type="primary" @click="watchTask(row.id)">日志</el-button></template>
          </el-table-column>
        </el-table>
      </el-card>

      <!-- 镜像列表 -->
      <el-card shadow="never" class="sub-card">
        <template #header>
          <div class="card-header">
            <span>镜像列表（每仓库保留最近 {{ info.retention_per_repo || '∞' }} 个 tag）</span>
            <el-button size="small" :icon="Refresh" @click="loadImages">刷新</el-button>
          </div>
        </template>
        <el-table :data="images" size="small" v-loading="imagesLoading" empty-text="暂无镜像">
          <el-table-column type="expand">
            <template #default="{ row }">
              <el-table :data="row.tags || []" size="small" class="tag-table">
                <el-table-column label="Tag" prop="tag" width="140" />
                <el-table-column label="大小" width="110">
                  <template #default="{ row: t }">{{ fmtSize(t.size) }}</template>
                </el-table-column>
                <el-table-column label="更新时间" width="180">
                  <template #default="{ row: t }">{{ new Date(t.updated).toLocaleString() }}</template>
                </el-table-column>
                <el-table-column label="Digest" min-width="200">
                  <template #default="{ row: t }"><span class="mono dim">{{ t.digest.slice(0, 19) }}…</span></template>
                </el-table-column>
                <el-table-column label="操作" width="170">
                  <template #default="{ row: t }">
                    <el-button link type="primary" @click="copyPull(row.name, t.tag)">复制拉取命令</el-button>
                    <el-button link type="danger" @click="removeTag(row.name, t.tag)">删除</el-button>
                  </template>
                </el-table-column>
              </el-table>
            </template>
          </el-table-column>
          <el-table-column label="仓库" prop="name" min-width="200">
            <template #default="{ row }"><span class="mono">{{ row.name }}</span></template>
          </el-table-column>
          <el-table-column label="Tag 数" width="90" align="center">
            <template #default="{ row }">{{ row.tags?.length ?? 0 }}</template>
          </el-table-column>
          <el-table-column label="操作" width="110" align="center">
            <template #default="{ row }">
              <el-button link type="danger" @click="removeRepo(row)">删除仓库</el-button>
            </template>
          </el-table-column>
        </el-table>
      </el-card>
    </template>
  </el-card>
</template>

<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { FolderOpened, Refresh } from '@element-plus/icons-vue'
import { registryApi } from '@/api'
import type { BuildTask, RegistryInfo, RegistryRepo } from '@/types'

const loading = ref(true)
const info = ref<RegistryInfo | null>(null)
const enabled = computed(() => !!info.value)
const registryAddr = computed(() => (info.value ? `${info.value.hostname}:${info.value.port}` : ''))

const zipFile = ref<File | null>(null)
const buildForm = reactive({ project: '', name: '', tag: '' })
const building = ref(false)
const uploading = ref(false)
const uploadProgress = ref(0)
const currentTask = ref<BuildTask | null>(null)
const builds = ref<BuildTask[]>([])
const images = ref<RegistryRepo[]>([])
const imagesLoading = ref(false)
const logBoxRef = ref<HTMLElement>()

// 项目名 = 镜像仓库的命名空间(repo 首段,如 ops/myapp 的 ops),从现有镜像
// 列表提取,也允许输入新名——与管理端「项目」体系无关。
// 用 autocomplete 而非 select+allow-create:后者点击外部失焦会丢弃未回车的
// 新名,体验上变成"只能选已有"
const projectOptions = computed(() => {
  const set = new Set<string>()
  for (const r of images.value) {
    const idx = r.name.indexOf('/')
    if (idx > 0) set.add(r.name.slice(0, idx))
  }
  return [...set].sort()
})

// 建议项必须是 { value } 对象——纯字符串经 item[valueKey] 取值渲染为空白行
function queryProjects(qs: string, cb: (items: { value: string }[]) => void) {
  const q = qs.trim()
  cb(projectOptions.value.filter((p) => !q || p.includes(q)).map((p) => ({ value: p })))
}

// 镜像路径段：小写字母/数字/_.-(registry 仓库名约束;后端同规则校验)
const nameSegOk = (s: string) => /^[a-z0-9_.\-]+$/.test(s)
const nameValid = computed(() => nameSegOk(buildForm.project.trim()) && nameSegOk(buildForm.name.trim()))
const fullImageRef = computed(() => {
  if (!buildForm.project.trim() || !buildForm.name.trim()) return ''
  const tag = buildForm.tag.trim() || 'latest'
  return `${registryAddr.value}/${buildForm.project.trim()}/${buildForm.name.trim()}:${tag}`
})
const canBuild = computed(
  () => !!info.value?.docker_available && !!zipFile.value && nameValid.value && !!buildForm.tag.trim() && !building.value,
)

let pollTimer: ReturnType<typeof setInterval> | null = null

function onPick(upload: { raw?: File }) {
  zipFile.value = upload.raw ?? null
}

async function loadInfo() {
  loading.value = true
  try {
    info.value = await registryApi.info()
  } catch {
    info.value = null
  } finally {
    loading.value = false
  }
}

async function loadBuilds() {
  try {
    const resp = await registryApi.builds()
    builds.value = resp.items ?? []
  } catch {
    /* 忽略 */
  }
}

async function loadImages() {
  imagesLoading.value = true
  try {
    const resp = await registryApi.images()
    images.value = resp.items ?? []
  } catch {
    images.value = []
  } finally {
    imagesLoading.value = false
  }
}

async function submitBuild() {
  if (!zipFile.value) return
  if (!nameValid.value) {
    ElMessage.warning('项目名/镜像名仅允许小写字母、数字、_ . -（如 ops、myapp）')
    return
  }
  const form = new FormData()
  form.append('file', zipFile.value)
  form.append('name', `${buildForm.project.trim()}/${buildForm.name.trim()}`)
  form.append('tag', buildForm.tag.trim())
  building.value = true
  uploading.value = true
  uploadProgress.value = 0
  try {
    const task = await registryApi.submitBuild(form, (event) => {
      if (!event.total) return
      uploadProgress.value = Math.min(100, Math.max(0, Math.round((event.loaded / event.total) * 100)))
    })
    currentTask.value = task
    startPoll(task.id)
    ElMessage.success('构建任务已提交')
  } catch {
    /* 错误已由 http.ts 提示 */
  } finally {
    uploading.value = false
    building.value = false
  }
}

function startPoll(id: string) {
  stopPoll()
  pollTimer = setInterval(() => void pollTask(id), 1500)
}

function stopPoll() {
  if (pollTimer) {
    clearInterval(pollTimer)
    pollTimer = null
  }
}

async function pollTask(id: string) {
  try {
    const task = await registryApi.build(id)
    currentTask.value = task
    void nextTick(() => {
      const el = logBoxRef.value
      if (el) el.scrollTop = el.scrollHeight
    })
    if (task.status === 'SUCCESS' || task.status === 'FAILED') {
      stopPoll()
      void loadImages()
      void loadBuilds()
    }
  } catch {
    stopPoll()
  }
}

async function watchTask(id: string) {
  try {
    currentTask.value = await registryApi.build(id)
  } catch {
    /* 已提示 */
  }
}

async function copyPull(name: string, tag: string) {
  const cmd = `docker pull ${registryAddr.value}/${name}:${tag}`
  try {
    await navigator.clipboard.writeText(cmd)
    ElMessage.success('已复制')
  } catch {
    ElMessage.info(cmd)
  }
}

async function removeTag(name: string, tag: string) {
  try {
    await ElMessageBox.confirm(`删除镜像 ${name}:${tag}？（manifest 本体由 GC 清理）`, '删除镜像', { type: 'warning' })
  } catch {
    return // 用户取消
  }
  await registryApi.deleteTag(name, tag)
  ElMessage.success('已删除')
  await loadImages()
}

async function removeRepo(row: RegistryRepo) {
  try {
    await ElMessageBox.confirm(
      `删除仓库「${row.name}」及其全部 ${row.tags?.length ?? 0} 个 tag？此操作不可恢复（blob 由 GC 清理）。`,
      '删除仓库',
      { type: 'error' },
    )
  } catch {
    return // 用户取消
  }
  await registryApi.deleteRepo(row.name)
  ElMessage.success('已删除')
  await loadImages()
}

function fmtSize(n: number) {
  if (n >= 1 << 30) return (n / (1 << 30)).toFixed(2) + ' GB'
  if (n >= 1 << 20) return (n / (1 << 20)).toFixed(1) + ' MB'
  return (n / 1024).toFixed(0) + ' KB'
}

onMounted(async () => {
  await loadInfo()
  if (info.value) {
    void loadImages()
    void loadBuilds()
  }
})
onBeforeUnmount(stopPoll)
</script>

<style scoped>
.card-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
}
.mb {
  margin-bottom: 14px;
}
.ml {
  margin-left: 8px;
}
.sub-card {
  margin-bottom: 14px;
}
.guide h5 {
  margin: 10px 0 4px;
  font-size: 13px;
}
.code {
  background: var(--og-bg-code);
  color: var(--og-text-code);
  border-radius: 6px;
  padding: 10px 12px;
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  font-size: 12px;
  line-height: 1.7;
  white-space: pre-wrap;
}
.build-form :deep(.el-form-item__content) {
  display: flex;
  align-items: center;
  gap: 8px;
}
.file-name {
  font-size: 12px;
  color: var(--el-text-color-secondary);
}
.ref-preview {
  margin: 2px 0 6px;
  font-size: 12px;
  color: var(--el-text-color-regular);
}
.muted {
  color: var(--el-text-color-placeholder);
  font-size: 12px;
}
.upload-status,
.build-status {
  display: flex;
  align-items: center;
  gap: 12px;
  margin: 10px 0;
}
.log-box {
  height: 240px;
  overflow: auto;
  background: var(--og-bg-code);
  color: var(--og-text-code);
  border-radius: 6px;
  padding: 10px;
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  font-size: 12px;
  line-height: 1.6;
  white-space: pre-wrap;
  word-break: break-all;
}
.mono {
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  font-size: 12px;
}
.dim {
  color: var(--el-text-color-secondary);
}
.err-text {
  color: var(--el-color-danger);
  font-size: 12px;
}
.tag-table {
  margin: 4px 12px;
}
</style>
