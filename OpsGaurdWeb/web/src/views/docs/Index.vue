<template>
  <div class="docs">
    <!-- 左侧目录 -->
    <aside class="toc">
      <el-card shadow="never" class="toc-card">
        <template #header>
          <span class="card-header-text">目录</span>
        </template>
        <div class="toc-list">
          <button
            v-for="s in sections"
            :key="s.id"
            class="toc-item"
            :class="{ active: activeId === s.id }"
            @click="jump(s.id)"
          >
            {{ s.label }}
          </button>
        </div>
      </el-card>
    </aside>

    <!-- 正文 -->
    <div class="content">
      <el-card id="intro" shadow="never" class="section">
        <template #header>
          <span class="card-header-text">平台简介</span>
        </template>
        <p>
          OpsGaurd 是一个智能运维平台:在一个统一的管理界面里纳管多个相互隔离的容器集群,
          把日常运维中的部署发布、监控告警、周期巡检、异常排查、镜像管理与 AI 费用运营收拢到一处完成。
        </p>
        <ul>
          <li><b>业务团队</b>:在页面上完成服务从构建到上线的全过程;发布新版本、调整容量、查看日志都不需要登服务器。</li>
          <li><b>值班人员</b>:集中查看所有集群的健康状态与告警,配合 AI 排查快速定位问题。</li>
          <li><b>平台管理员</b>:负责管理端部署、集群接入、通知渠道、账号权限等一次性配置,之后各角色自助使用。</li>
        </ul>
        <p class="note">
          平台由两部分组成:「管理端」部署在管理机上,提供页面与全部平台能力;
          「Worker」部署在每个被纳管集群的每台节点上,负责执行编排、采集监控、供 AI 调用。
          通信只走「管理端 → Worker」单向,被管集群不需要对管理端开放反向访问。
        </p>
      </el-card>

      <el-card id="start" shadow="never" class="section">
        <template #header>
          <span class="card-header-text">快速上手</span>
        </template>
        <ol>
          <li>
            <b>登录</b>:支持本地账号密码与企业统一登录(SSO)。首次部署会提供默认管理员账号
            (admin / opsguard-admin),请在首次登录后立即通过页面底部的「修改密码」完成改密。
          </li>
          <li>
            <b>认识总览</b>:登录后进入「总览」,这里汇总在线集群、未处理告警、项目数与巡检异常,
            以及集群健康表和最近告警,适合作为每日巡屏的首页。
          </li>
          <li>
            <b>完成第一件任务</b>:按下方「常用场景」的第一个场景(新业务从零上线)完整走一遍,
            即可熟悉平台的主干操作。
          </li>
        </ol>
        <p class="note">管理端尚未部署?看「部署管理端」;有新集群要纳管?看「接入 Worker」。</p>
      </el-card>

      <el-card id="deploy-server" shadow="never" class="section">
        <template #header>
          <span class="card-header-text">部署管理端</span>
        </template>
        <p>
          管理端以单个容器运行,内含管理页面与全部平台服务;数据保存在宿主机挂载目录里,
          升级时只替换容器、数据不动。目标环境通常无外网,标准做法是
          <b>「有网侧构建镜像 → 导出 tar → 上传到管理机离线载入」</b>。
        </p>

        <h3>前置条件</h3>
        <ul>
          <li>一台 Linux 服务器作为管理机(建议独立部署,不与业务混部)。</li>
          <li>已安装 Docker。离线环境使用部署包内的 docker 静态包 + 安装脚本(见 runbook)。</li>
          <li>准备管理端配置文件 config.yaml:监听端口(默认 8080)、认证密钥(token_secret)、数据目录等。</li>
        </ul>

        <h3>部署步骤</h3>
        <ol>
          <li>
            在有外网的一侧构建镜像并导出(构建上下文为 OpsGaurdWeb 目录,产物自带前端页面):
            <pre class="code">cd OpsGaurdWeb
docker build -t opsguard-server:1.2.x -f deploy/Dockerfile .
docker save -o opsguard-server-1.2.x.tar opsguard-server:1.2.x</pre>
          </li>
          <li>把镜像 tar 与 config.yaml 上传到管理机,惯例放在 /opt/opsguard/ 下(镜像在 images/、配置在 server/config.yaml)。</li>
          <li>管理机载入镜像:
            <pre class="code">docker load -i opsguard-server-1.2.x.tar</pre>
          </li>
          <li>运行容器(数据目录、配置文件、docker 套接字三个挂载是固定写法):
            <pre class="code">docker run -d --name opsguard-server --restart unless-stopped -p 8080:8080 \
  -v /opt/opsguard/server/data:/app/data \
  -v /opt/opsguard/server/config.yaml:/app/configs/config.yaml:ro \
  -v /var/run/docker.sock:/var/run/docker.sock opsguard-server:1.2.x</pre>
          </li>
          <li>
            验证:<code>curl http://127.0.0.1:8080/healthz</code> 返回 200;
            浏览器打开 <code>http://管理机IP:8080</code>,用默认账号登录后立即改密。
          </li>
        </ol>

        <h3>升级与回滚</h3>
        <ul>
          <li><b>升级顺序必须先所有集群的 Worker、后管理端</b>,反了会导致事件通道持续重连报错。</li>
          <li>数据在 /opt/opsguard/server/data 卷内,与容器无关:停旧容器、用新镜像原样 run 即完成升级。</li>
          <li>保留上一版镜像 tar;出问题时停掉新容器、用旧镜像原样 run 即回滚。</li>
        </ul>
      </el-card>

      <el-card id="deploy-worker" shadow="never" class="section">
        <template #header>
          <span class="card-header-text">接入 Worker(纳管新集群)</span>
        </template>
        <p>
          Worker 是装在被纳管集群<b>每台节点</b>上的节点代理,负责执行编排操作、采集监控事件、
          供 AI 排查调用。以 swarm 全局服务方式运行,每节点自动各起一个;集群侧装好后,
          在管理端页面登记即可完成纳管。
        </p>

        <h3>前置检查</h3>
        <ul>
          <li>
            <b>网络</b>:防火墙放行「管理端 → 集群 manager 节点」的
            <b>gRPC 端口(默认 9080)与 HTTP 端口(默认 8080)</b> 两个端口,
            均可自定义(生产常用 6061/6060);Worker 方向不需要任何放行。
          </li>
          <li><b>时钟</b>:所有节点与管理端 NTP 对时,时钟漂移会导致证书校验失败。</li>
          <li><b>Swarm</b>:目标集群已完成组网——manager 上 <code>docker node ls</code> 全部 Ready。</li>
        </ul>

        <h3>集群侧安装(每台节点)</h3>
        <ol>
          <li>
            分发离线包到统一目录:docker 静态包与安装脚本(offlines/)、
            Worker 镜像 tar(images/)、agent-config.yaml(etc/opsguard/)。
            配置模板见仓库 Worker/deploy/agent-config.yaml.example。
          </li>
          <li>
            安装 Docker:manager 节点用 install-docker.sh(装完自动 swarm init);
            node 节点用 install-docker-noinit.sh。
            核对 daemon.json 的 <code>insecure-registries</code>(镜像拉取源):
            隔离集群走隧道中继时填 <b>manager 上 Worker 的 HTTP 地址</b>(如 managerIP:6060),
            全集群各节点统一指同一地址,保证镜像引用一致;改完重启 docker 生效。
          </li>
          <li>
            放置 /etc/opsguard/agent-config.yaml(<b>全节点内容逐字一致</b>),核心三段:
            <pre class="code">worker:
  role: auto                    # 按本机 swarm 角色自动推导
  listen: ":6060"               # HTTP(/healthz + AI 采证入口)
  grpcListen: ":6061"           # gRPC,管理端连这里
  dataDir: "/var/lib/opsguard"  # 事件数据目录,须挂卷持久化

auth:
  enabled: true                 # 生产必须开启,否则端口裸奔
  tokens:
    opsguard-server: "用 openssl rand -hex 24 生成的长随机串"</pre>
            token 名会作为审计记录里的操作者显示;注册集群时页面填的 Token 必须与这里的值完全一致。
          </li>
          <li>
            建数据目录并导入镜像(swarm 不会自动创建挂载源目录,漏了会导致任务起不来):
            <pre class="code">mkdir -p /var/lib/opsguard
docker load -i opsguard-worker-1.2.x.tar</pre>
          </li>
        </ol>

        <h3>初始化 / 加入 Swarm</h3>
        <p>
          Worker 的角色由本机 swarm 角色自动推导:manager 节点承担编排与管理入口,
          普通节点只做本地执行——<b>swarm 拓扑就是 Worker 的部署拓扑</b>。
          manager 节点装 Docker 时已自动初始化的跳过第 1 步。
        </p>
        <ol>
          <li>
            manager 节点初始化(地址填节点间互通的网卡 IP):
            <pre class="code">docker swarm init --advertise-addr managerIP</pre>
          </li>
          <li>
            manager 上取加入令牌:
            <pre class="code">docker swarm join-token worker    # 普通节点加入用
docker swarm join-token manager   # 追加 manager(HA)用</pre>
          </li>
          <li>
            各 node 节点加入(2377 为集群管理端口):
            <pre class="code">docker swarm join managerIP:2377 --token 加入令牌</pre>
          </li>
          <li>
            (可选)追加 manager 实现高可用:用 manager 令牌执行同样的 join 命令,
            或事后 docker node promote 提升;多 manager 时编排写操作自动由 leader 处理,无需额外配置。
          </li>
        </ol>
        <ul class="points">
          <li>swarm 自身要求的<b>节点间</b>放行:2377/tcp(集群管理)、7946/tcp+udp(节点发现)、4789/udp(overlay 网络);与管理端到 manager 的双端口放行是两回事,都要有。</li>
          <li>验证:manager 上 <code>docker node ls</code>,所有节点 Ready、预期节点带 Leader 标记。</li>
          <li>已有 Docker 的环境跳过安装脚本,但需确认节点已加入目标集群:<code>docker info</code> 查看 swarm 段(未加入时加入命令同上)。</li>
        </ul>

        <h3>部署 Worker 服务(manager 节点执行一次)</h3>
        <pre class="code">docker stack deploy -c stack.yml opsguard</pre>
        <p class="note">stack 模板见仓库 Worker/deploy/stack.yml,核心是 global 模式 + 挂载 docker.sock / 配置 / 数据目录,端口用 host 模式发布。</p>

        <h3>集群侧自检</h3>
        <pre class="code">docker stack services opsguard                        # 副本应为 x/x
docker service logs opsguard_worker --tail 20 | grep version   # 版本正确
curl -s http://localhost:6060/healthz                 # 每台节点都应返回 ok</pre>

        <h3>管理端页面接入</h3>
        <ol>
          <li>
            在管理端所在主机预检连通(gRPC 与 HTTP 两个端口都要通,任一不通先解决防火墙):
            <pre class="code">nc -zv managerIP 6061   # gRPC(编排/监控/探活)
nc -zv managerIP 6060   # HTTP(健康校验 + AI 采证入口)</pre>
          </li>
          <li>
            「集群 → 接入集群」填表:
            <b>集群名称</b>(创建后不可改名)、<b>所属项目</b>(可选)、
            <b>gRPC 地址</b>(http://managerIP:gRPC端口,注意不是 HTTP 端口)、
            <b>HTTP 地址</b>(http://managerIP:HTTP端口,AI 采证端点自动取其 /mcp)、
            <b>Token</b>(与 agent-config 里的 secret 完全一致)、描述(可选)。
          </li>
          <li>
            提交时管理端立即探测(5 秒超时):gRPC 确认对端是 swarm manager、HTTP 校验健康接口。
            失败返回 502 并带原因(按下方清单排查);成功后列表状态「在线」,
            详情页能看到各节点与实时 CPU/内存指标。
          </li>
        </ol>
        <ul class="points">
          <li>接入失败最常见原因:gRPC/HTTP 两个地址端口填反;防火墙未放行;token 两边不一致;填到的节点不是 manager。</li>
          <li>已注册集群要换 token:先改各节点配置滚动重启,再在管理端更新集群的 token,短暂断流事件不丢、恢复后自动续传。</li>
        </ul>
      </el-card>

      <el-card id="modules" shadow="never" class="section">
        <template #header>
          <span class="card-header-text">功能导览</span>
        </template>
        <el-table :data="modules" size="small">
          <el-table-column prop="page" label="页面" width="110" />
          <el-table-column prop="purpose" label="用途" min-width="220" />
          <el-table-column prop="actions" label="常见操作" min-width="300" />
        </el-table>
        <p class="note">
          集群详情页是日常操作的主界面:顶部「部署」「+纳管」入口 +
          节点 / 服务 / 中间件 / 事件 / 审计 / 告警规则等页签;
          MLOps 含模型接入 / 提示词 / 模型 / 用量费用四个页签;
          系统设置含用户 / SSO / 身份提供者 / AI 排查网关 / 密钥。
        </p>
      </el-card>

      <el-card id="scenes" shadow="never" class="section">
        <template #header>
          <span class="card-header-text">常用场景</span>
        </template>

        <div v-for="sc in scenes" :key="sc.title" class="scene">
          <h3>{{ sc.title }}</h3>
          <p v-if="sc.note" class="note">{{ sc.note }}</p>
          <ol v-if="sc.steps?.length">
            <li v-for="(step, i) in sc.steps" :key="i">{{ step }}</li>
          </ol>
          <ul v-if="sc.points?.length" class="points">
            <li v-for="(pt, i) in sc.points" :key="i">{{ pt }}</li>
          </ul>
        </div>
      </el-card>

      <el-card id="roles" shadow="never" class="section">
        <template #header>
          <span class="card-header-text">角色与权限</span>
        </template>
        <p>平台分「管理员」与「只读」两级角色:</p>
        <el-table :data="roleRows" size="small">
          <el-table-column prop="area" label="能力" min-width="240" />
          <el-table-column label="只读" width="90" align="center">
            <template #default="{ row }">
              <el-tag size="small" :type="row.viewer ? 'success' : 'info'">{{ row.viewer ? '可用' : '限定' }}</el-tag>
            </template>
          </el-table-column>
          <el-table-column label="管理员" width="90" align="center">
            <template #default>
              <el-tag size="small" type="success">可用</el-tag>
            </template>
          </el-table-column>
        </el-table>
        <ul class="points">
          <li>没有删除或停用用户的入口,开通账号前请确认账号需求。</li>
          <li>SSO 登录的账号没有本地密码,无法自助修改密码,请联系管理员重置。</li>
        </ul>
      </el-card>

      <el-card id="faq" shadow="never" class="section">
        <template #header>
          <span class="card-header-text">常见问题</span>
        </template>
        <div v-for="(qa, i) in faqs" :key="i" class="qa">
          <p class="q">{{ qa.q }}</p>
          <p class="a">{{ qa.a }}</p>
        </div>
      </el-card>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref } from 'vue'

// 文档中心:面向使用者的操作指引(内容取自项目文档与部署手册)。

const sections = [
  { id: 'intro', label: '平台简介' },
  { id: 'start', label: '快速上手' },
  { id: 'deploy-server', label: '部署管理端' },
  { id: 'deploy-worker', label: '接入 Worker' },
  { id: 'modules', label: '功能导览' },
  { id: 'scenes', label: '常用场景' },
  { id: 'roles', label: '角色与权限' },
  { id: 'faq', label: '常见问题' },
]

const activeId = ref('intro')

function jump(id: string) {
  activeId.value = id
  document.getElementById(id)?.scrollIntoView({ behavior: 'smooth', block: 'start' })
}

const modules = [
  { page: '总览', purpose: '全局健康与告警速览', actions: '关键指标数字卡、集群健康表(离线集群红标)、最近告警;每次进入刷新' },
  { page: '项目', purpose: '按业务维度组织集群', actions: '新建项目(名称全局唯一)、维护描述;接入集群时归属到项目' },
  { page: '集群', purpose: '接入与管理容器集群', actions: '接入集群;详情页含节点(CPU/内存实时)、服务部署/编辑/缩放/重启/日志、中间件、事件回溯、审计、告警规则、+纳管(单条接入/编辑/删除)' },
  { page: '告警中心', purpose: '集中处理告警', actions: '按集群/状态筛选,认领 / 恢复,跳转 AI 排查,查看历史排查记录' },
  { page: '镜像仓库', purpose: '存储与分发业务镜像', actions: '上传构建包自动构建(需含构建描述文件)、查看镜像版本、复制拉取命令、删除标签' },
  { page: '智能巡检', purpose: '定时自动检查并产出 AI 报告', actions: '新建巡检流程(6 类检查项)、立即执行、查看明细与 AI 报告、配置报告投递' },
  { page: '异常排查', purpose: 'AI 辅助定位问题', actions: '关联告警提问或自由提问、查看 AI 采证过程与结论、继续追问、回放历史会话' },
  { page: 'MLOps', purpose: 'AI 模型与费用运营', actions: '模型接入(Provider+模型池)、提示词版本管理、模型启停与场景绑定、用量费用报表与月度预算' },
  { page: '通知中心', purpose: '让告警通知到人', actions: '维护渠道(飞书/短信/Webhook)与级别策略、查看每次发送记录与失败原因' },
  { page: '系统设置', purpose: '账号与平台配置', actions: '修改密码、用户管理、SSO、身份提供者(向其他系统签发登录身份)、AI 排查网关、密钥' },
]

interface Scene {
  title: string
  note?: string
  steps?: string[]
  points?: string[]
}

const scenes: Scene[] = [
  {
    title: '1. 新业务从零上线',
    steps: [
      '在「镜像仓库」上传构建包(zip 内含构建描述文件,大小上限 500MB),填项目名/镜像名/版本号,点「开始构建」;构建为异步执行,失败看日志框。',
      '在「项目」新建项目,作为业务的归属容器。',
      '在「集群」接入目标集群(集群侧的一次性安装见「接入 Worker」章节)。',
      '进入集群详情页点「部署」,选择类别(服务/中间件),填写服务配置:必填仅名称与镜像地址(写法要与节点镜像源配置一致),可同时声明端口、环境变量、挂载、资源上限、健康检查;监控配置与部署一起提交,含端口探测、HTTP 检查、日志关键字、CPU/内存阈值四类探针。',
      '提交后异步收敛(上限 5 分钟,页面自动轮询):副本比达到 x/x 为健康;部分运行为 partial;一个都没起为 failed。',
    ],
    points: [
      '上线验证:服务列表副本数齐全;告警规则页签出现该服务的规则;业务端口可访问;故意制造一次异常能产生事件/告警。',
      '部署 failed 多为镜像拉取失败或端口冲突:在节点上手工拉一次镜像复现,核对镜像地址写法与节点镜像源配置一致。',
    ],
  },
  {
    title: '2. 发布新版本与日常变更',
    steps: [
      '在「镜像仓库」构建新版本号。',
      '集群详情页对应服务行点「编辑」,更新镜像版本后保存;滚动更新节奏由配置里的 update 参数控制,不配则默认逐个替换、失败暂停。',
      '容量调整用「缩放」(输入目标副本数);异常时用「重启」强制重建;下线用「移除」(不可恢复,同时注销该服务全部监控)。',
      '「详情」里看聚合日志,支持跟随最新/停止跟随,错误输出标红。',
    ],
    points: [
      '编辑是整体替换:保存时请保留原有监控配置,否则已生效的监控会被清掉(页面会黄字提示并自动尝试恢复)。',
      '每次变更都是异步操作,失败弹窗带具体原因;「事件」页签可完整回溯。',
    ],
  },
  {
    title: '3. 纳管集群之外的中间件与服务',
    note: '不是平台部署的(宿主机 docker run 的容器、直接跑在宿主机上的 MySQL/Redis 等端口服务),也能纳入监控告警。',
    steps: [
      '集群详情页点「+纳管」逐条接入:名称(唯一)、类型(docker 容器 / 宿主机端口服务)、引用(容器名或 IP:端口)、所在节点(容器类型必填)、端口、分类(中间件/业务/基础设施/服务)、监控配置(端口探测/HTTP 检查)。已有条目在「服务 / 中间件」列表行上直接详情 / 编辑 / 删除。',
    ],
    points: [
      '平台周期探测,只在状态翻转(异常/恢复)时产生事件进告警中心。',
      '带监控的纳管条目会自动出现在「告警规则」页签;纳管对象在服务列表带「纳管」标签,支持详情 / 编辑 / 删除,standalone 容器还可重启。',
    ],
  },
  {
    title: '4. 让告警通知到人(管理员一次性配置)',
    steps: [
      '「通知中心 → 渠道」新增渠道:类型支持飞书 / 短信 / 通用 Webhook;内网无外网时打开「经互联网代理」开关并填代理地址。',
      '「通知中心 → 策略」按告警级别(error/warn/info)分别选择渠道与接收人。',
    ],
    points: [
      '某一级别未配置策略,该级别的告警就不会推送通知。',
      '通知节奏:告警首次出现立即通知;持续未恢复在累计 10/100/1000… 次时追加提醒;恢复时发恢复通知。',
      '通知没送达,先到「发送记录」查每次发送的结果与失败原因。',
      '验证方法:临时把某服务阈值调到必触发(如 CPU 1%),等一个采样周期确认渠道收到,再改回。',
    ],
  },
  {
    title: '5. 值班巡屏与告警处置',
    steps: [
      '每日从「总览」开始:看四个数字卡(在线集群/未处理告警/项目/巡检异常)、集群健康表(离线集群红标并可下钻)与最近告警。',
      '「告警中心」按集群和状态筛选(全部/未处理/已认领/已恢复),处理前先「认领」表示有人在跟,处理完成后「恢复」关闭。',
      '需要定位根因时点「排查」,带着告警上下文进入 AI 异常排查。',
    ],
    points: [
      '同类型异常恢复后再次出现会重新激活告警并再次通知;「次数/首次/最近」列看持续性。',
      '原始事件在集群详情页「事件」页签回溯,支持翻页加载。',
    ],
  },
  {
    title: '6. 周期智能巡检',
    steps: [
      '「智能巡检 → 新建流程」:填名称、执行时间(cron 表达式,默认每天凌晨 2 点)、选择报告模型(可选)。',
      '流程内容为检查项列表,共 6 类:容器资源占用、服务副本健康、TCP 端口探测、HTTP 探测(可校验状态码/响应体)、宿主机进程存在性、多步 HTTP 事务拨测(如登录链路,凭据可引用密钥)。',
      '保存后点「立即执行」验证一轮,执行明细里看每项 正常/异常 与 AI 总结报告。',
    ],
    points: [
      '「报告投递」可设每次生成后发送 / 仅有异常时发送 / 不发送,投递渠道来自通知中心;全局配置,所有流程共享。',
      '检查项失败自动产生告警进告警中心;与上一轮对比仅新增/复发才通知,恢复自动关闭。',
      '探测宿主机服务时目标地址必须写节点 IP,不能写本机回环地址(探测从集群内部发起)。',
      '引用的密钥不存在会直接判失败:先到「系统设置 → 密钥」创建。',
    ],
  },
  {
    title: '7. AI 异常排查',
    steps: [
      '前置:管理员已完成模型接入并启用排查网关(否则页面顶部黄条提示、输入框禁用)。',
      '从告警中心点「排查」自动带入告警上下文;或在「异常排查」页直接提问(回车发送,Shift+回车换行)。',
      '保持「MCP 采证」开启后点「开始排查」:关联告警的会话首轮会自动注入最近事件、审计与日志作为证据。',
      '观察回答中出现的工具调用片(即 AI 在真实采集集群证据),可点开看采证结果;同一会话可继续追问。',
    ],
    points: [
      '每轮会话自动留痕:告警行显示「已排查 ×N」,历史会话可只读回放。',
      'AI 具备真实操作集群的能力(含删除/缩容/命令执行),危险动作需显式确认并全程审计;宿主机命令默认关闭。',
      'AI 全程不调工具:多为集群离线或 token 不一致导致采证通道未建立,或「MCP 采证」被关。',
      '报模型不存在/已停用:核对模型名,或到「MLOps → 模型」重新启用。',
    ],
  },
  {
    title: '8. 账号与单点登录',
    steps: [
      '所有用户:在「系统设置 → 用户」页底部自助修改密码;默认账号首次登录后必须改。',
      '管理员:在「系统设置 → 用户」新建账号(角色默认只读)、重置密码(免旧密码)。',
      '企业 SSO:由管理员在服务端配置后,登录页出现「企业 SSO 登录」按钮;SSO 新用户默认只读角色。',
      '向其他系统签发登录身份:管理员在「系统设置 → 身份提供者」注册应用(公共/机密两类),对接方按授权码方式接入;管理端登录后跳转对接系统可免密。',
    ],
  },
]

const roleRows = [
  { area: '查看所有页面、报表、日志', viewer: true },
  { area: '项目 / 集群 / 部署 / 扩缩容 / 重启 / 下线', viewer: true },
  { area: '告警处置、巡检、告警规则、通知渠道与策略、密钥、镜像构建与删除', viewer: true },
  { area: '用户管理(新建账号、重置密码)', viewer: false },
  { area: '身份提供者(向其他系统签发登录身份)', viewer: false },
  { area: 'AI 排查网关配置、模型接入', viewer: false },
  { area: 'MLOps 运营(提示词、单价、模型启停、预算)', viewer: false },
]

const faqs = [
  { q: '接入集群时返回 502 探测失败?', a: '按顺序检查:两个地址的端口是否填反;管理端到集群 manager 的 gRPC 与 HTTP 端口是否都放通;Token 是否与 Worker 配置一致;填的是否为 swarm manager 节点。见「接入 Worker」章节。' },
  { q: '部署服务一直是 failed(没有副本在跑)?', a: '多为镜像拉取失败或端口冲突:在节点上手工拉一次镜像复现;核对镜像地址写法与节点 insecure-registries 配置一致;检查端口是否被占用。' },
  { q: '节点拉镜像报 502 / 中继不可用?', a: '镜像中继通道未建立:确认 Worker 的隧道基址环境变量指向 manager 的 HTTP 地址、管理端身份服务已开启、镜像源地址填的是 manager 的 Worker HTTP 地址。' },
  { q: '编辑服务后,监控规则不见了?', a: '服务编辑是整体替换,保存时需要带上原有监控配置;也可以在集群详情页「告警规则」中重新下发。' },
  { q: '修改了告警规则却没有生效?', a: '规则保存后需要在规则列表点「下发」,才会推送到集群执行。' },
  { q: '告警产生了,但没有人收到通知?', a: '到「通知中心」检查:对应告警级别是否配置了策略;再到「发送记录」查看每次发送的结果与失败原因。' },
  { q: '巡检报告是纯文本,没有 AI 总结?', a: '平台尚未接入 AI 模型或排查网关未启用。请管理员先在「MLOps → 模型接入」添加模型,再到「系统设置 → AI 排查网关」启用。' },
  { q: '巡检探测宿主机上的服务总是失败?', a: '探测从集群内部发起,目标地址请填写节点 IP,不要写本机回环地址。' },
  { q: '巡检引用的密钥报不存在?', a: '先在「系统设置 → 密钥」创建同名密钥(名称仅限字母数字与 . _ -,创建后值不再回显),再重新执行。' },
  { q: '节点进程页只看到 Worker 自己的进程?', a: 'swarm 服务形态下 Worker 看不到宿主机全量进程(平台限制);需要宿主机进程能力时,把 Worker 改用 docker run 方式部署(见 Worker 部署手册)。' },
  { q: 'SSO 登录的账号无法自助修改密码?', a: 'SSO 账号没有本地密码,请联系管理员重置。' },
  { q: '镜像列表里的旧版本标签消失了?', a: '镜像仓库默认为每个镜像保留最近若干个版本,超出保留数的旧标签会自动清理,属于预期行为。' },
]
</script>

<style scoped>
.docs {
  display: flex;
  gap: 16px;
  align-items: flex-start;
  max-width: 1200px;
  margin: 0 auto;
}
.toc {
  position: sticky;
  top: 0;
  width: 168px;
  flex-shrink: 0;
}
.toc-list {
  display: flex;
  flex-direction: column;
  gap: 2px;
}
.toc-item {
  border: none;
  background: transparent;
  text-align: left;
  padding: 7px 10px;
  border-radius: 6px;
  font-size: 13px;
  color: var(--el-text-color-secondary);
  cursor: pointer;
  transition: background 0.15s, color 0.15s;
}
.toc-item:hover {
  background: var(--el-fill-color-light);
  color: var(--el-text-color-primary);
}
.toc-item.active {
  background: var(--og-accent-soft);
  color: var(--og-accent-strong);
  font-weight: 600;
}
.content {
  flex: 1;
  min-width: 0;
  display: flex;
  flex-direction: column;
  gap: 16px;
}
.section {
  scroll-margin-top: 12px;
}
.card-header-text {
  font-size: 15px;
  font-weight: 600;
}
h3 {
  margin: 18px 0 4px;
  font-size: 13.5px;
  font-weight: 600;
  color: var(--el-text-color-primary);
}
p,
li {
  font-size: 13.5px;
  line-height: 1.8;
  color: var(--el-text-color-regular);
}
p {
  margin: 6px 0;
}
ul,
ol {
  margin: 6px 0;
  padding-left: 22px;
}
li > ul,
li > ol {
  margin: 2px 0;
}
.note {
  color: var(--og-text-dim);
}
.code {
  margin: 6px 0;
  padding: 10px 14px;
  border-radius: 8px;
  background: var(--og-bg-code);
  color: var(--og-text-code);
  font-family: var(--og-mono);
  font-size: 12.5px;
  line-height: 1.7;
  overflow-x: auto;
  white-space: pre;
}
code {
  font-family: var(--og-mono);
  font-size: 12.5px;
  background: var(--el-fill-color);
  padding: 1px 6px;
  border-radius: 4px;
}
.scene {
  padding: 10px 0;
}
.scene + .scene {
  border-top: 1px solid var(--el-border-color-lighter);
}
.scene h3 {
  margin: 4px 0 2px;
}
.points {
  margin-top: 2px;
}
.points li {
  color: var(--og-text-dim);
}
.qa {
  padding: 8px 0;
}
.qa + .qa {
  border-top: 1px dashed var(--el-border-color-lighter);
}
.qa .q {
  margin: 2px 0;
  font-weight: 600;
  color: var(--el-text-color-primary);
}
.qa .a {
  margin: 2px 0;
}

/* 窄屏:目录收纳到顶部 */
@media (max-width: 900px) {
  .docs {
    flex-direction: column;
  }
  .toc {
    position: static;
    width: 100%;
  }
  .toc-list {
    flex-direction: row;
    flex-wrap: wrap;
  }
}
</style>
