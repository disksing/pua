# AgentHub

[English](README.md)

AgentHub 是一个本地 Agent 启动器与 Session 中枢。一个 Go daemon 统一管理本机 Codex、Kimi、Pi/Grok 和 OpenCode，Web UI 与 CLI 都通过同一套 HTTP API 和 SSE 事件流工作。

## 能力

- 默认仅监听 loopback，可显式配置局域网/通配/IPv6 地址；无账号、token 或 API 鉴权。
- 独立的 provider 和 agent 配置。
- 创建 Session 时始终显式选择一个 Agent，没有隐式路由或回退。
- 真实 Provider Adapter：
  - Codex app-server
  - Kimi / OpenCode ACP v1
  - Pi JSONL RPC，包括 Kimi K3 与 Grok 等模型
- Session 创建、聊天、steer、interrupt、stop、resume、archive 和 approval。
- daemon 重启后按需恢复 provider 原生 session/thread。
- 同源 Web UI：Session 列表、实时聊天、状态、审批、停止，以及包含四个内置 Provider 可用性探测、结构化 Agent 表单和按 Provider 加载的模型下拉框的设置界面。
- 浮动伙伴：展示由 OnWatch 提供的 Provider 配额，以及由 AgentHub 自身事件聚合出的实时 Session 活动，可选 Web Audio 提示音。
- Provider 模型枚举：每个内置 Provider 都能通过各自的官方接口报告当前可用模型，统一为一个只读 API。
- CLI：一次性运行、交互聊天、attach、事件查询和 Session 管理。
- 每个 Session 保存唯一事实来源 `events.jsonl`，以及可重建的 `session.json` 和紧凑 `turns.jsonl` 投影；Approval 仍只以规范 Event 为准。

## 构建与启动

源码现已合并到 PUA 仓库，与 PUA 共用 Go module 和发布构建。需要 Go 1.26+ 和 Node.js。在仓库根目录执行：

```bash
scripts/build
bin/agenthub serve
```

打开 <http://127.0.0.1:4646/agenthub/>。独立服务的根路径会重定向到这里；API 固定位于 `/agenthub/v1`，API 文档位于 `/agenthub/api.md`。binary 已嵌入 Web UI，运行时不需要外部前端文件。

PUA 默认会把同一个 AgentHub application 内嵌到 `http://127.0.0.1:4936/agenthub`。只有在 AgentHub 需要脱离 PUA 运行，或 PUA 显式使用 `--agenthub-mode=external` 时才需要独立 binary。两种形态都继续使用 `~/.agenthub`，不能同时持有其 daemon lock。

### 监听地址

默认只监听 loopback，`agenthub serve` 等价于 `agenthub serve --addr 127.0.0.1:4646`。需要让局域网内其他设备访问时，可以用 `--addr host:port` 显式选择本机地址：

```bash
agenthub serve --addr 192.168.2.150:4646   # 具体局域网 IPv4
agenthub serve --addr 0.0.0.0:4646         # 所有 IPv4 接口（通配）
agenthub serve --addr '[::]:4646'          # 所有 IPv6 接口（通配）
agenthub serve --addr '[::1]:4646'         # 仅 IPv6 loopback
agenthub serve --addr myhost.local:4646    # 解析到本机接口的主机名/域名
```

主机名必须能解析到本机网络接口或 loopback。无法解析的主机名、非本机接口的地址、错误的格式和非法端口都会在启动时直接报错，不会静默回退到其他地址。IPv6 地址必须用方括号括起来。

> **安全警告**：AgentHub 没有账号、token 或 API 鉴权。监听非 loopback 或通配地址时，任何能访问该地址的设备都可以完全控制 daemon（运行 Agent、修改 Session 和配置）。只在可信网络中使用，不要直接暴露到公网。启动日志会打印同样的警告。

非 loopback 监听时，本机 CLI 仍通过 `server.json` 中的 loopback endpoint 自动发现 daemon。浏览器写请求仍要求 `Origin` 与请求 `Host` 一致，且 `Host` 必须是本机接口地址或本机主机名（防止 DNS rebinding），不接受任意 Origin。

### 反向代理

当 daemon 位于终止 TLS 的反向代理（Caddy、nginx 等）之后时，浏览器写请求携带的是公网 https `Origin`，与 daemon 自身 origin 不一致，会被拒绝并返回 `403 origin_rejected`。需要显式信任该公网 origin（参数可重复）：

```bash
agenthub serve --addr 0.0.0.0:4646 --allow-origin https://agenthub.example.com:8443
```

浏览器禁止伪造 `Origin` 头，因此 allowlist 只放行你显式配置的 origin，其他跨域写请求仍会被拒绝。建议保持代理将 `Host` 改写为 upstream 地址（Caddy `reverse_proxy` 的默认行为），这样防 DNS rebinding 的 Host 检查会继续通过。

开发时可分别运行：

```bash
go run ./cmd/agenthub serve
cd ../web && npm run dev -- --config vite.agenthub.config.ts
```

Vite 会把 `/agenthub/v1` 代理到默认 daemon 端口。

## CLI

CLI 提供分层帮助：`agenthub help` 输出总览和概念导读（Provider、Agent、Session、Turn、Approval、事件），`agenthub help <command>`（或 `agenthub help session <subcommand>`）输出单条命令的用法、选项、默认值和示例，`agenthub <command> --help` 效果相同。未知命令和错误参数会以非零状态退出，并提示对应的帮助入口。

```bash
agenthub help
agenthub help session approve
agenthub serve --help

agenthub status
agenthub agents

agenthub run --agent "Kimi K3" --cwd . "检查测试失败原因"
agenthub run --agent "Codex" --cwd . "实现这个功能并运行测试"

agenthub chat --agent "Codex GPT" --cwd .
agenthub session create --agent "Kimi K3" --title "bug hunt"
agenthub session attach <session-id>
agenthub session list
agenthub session list --archived
agenthub session show <session-id>
agenthub session events <session-id>
agenthub session resume <session-id>
agenthub session interrupt <session-id>
agenthub session stop <session-id>
agenthub session approve --decision accept <session-id> <approval-id>
agenthub session archive <session-id>
```

交互聊天支持 `/interrupt`、`/stop` 和 `/quit`。CLI 自动从 daemon 的 `server.json` 发现 endpoint；也可设置 `AGENTHUB_ENDPOINT`。

## 配置

默认配置文件是：

```text
$HOME/.agenthub/config.json
```

首次启动时，如果配置不存在，AgentHub 会直接生成自己的最小默认配置，不读取或迁移其他程序的配置。配置结构：

```json
{
  "version": 1,
  "agentProviders": [
    { "id": "codex", "name": "Codex app-server", "type": "codex" },
    { "id": "kimi", "name": "Kimi Code", "type": "kimi" },
    { "id": "pi", "name": "Pi Coding Agent", "type": "pi" }
  ],
  "agents": [
    {
      "name": "Kimi K3",
      "providerId": "pi",
      "options": { "mode": "build", "model": "kimi-coding/k3" }
    }
  ],
  "onWatch": {
    "enabled": false,
    "serverUrl": "http://127.0.0.1:9211",
    "authMode": "trusted_proxy",
    "username": "admin",
    "password": "",
    "refreshIntervalSeconds": 60
  }
}
```

浮动伙伴可以拖到视口内任意位置；归一化位置保存在浏览器本地存储中，刷新后仍会恢复，并会在视口缩放后保持可见。打开时，它会根据当前位置选择向上/向下展开及向左/向右对齐，确保卡片留在屏幕内。展开卡片还可以从外侧角调整宽高；保存的尺寸会按当前视口自动夹紧，内部控制项、波形、配额分栏和滚动布局会响应窄、矮或较宽的尺寸。标题栏可在新标签页打开 `/agenthub/beeper`；独立监控页以深色内容填满整个视口，竖屏时 Provider 配额保持单列，并继续提供设置入口。Activity 的显示、蜂鸣、音量、和弦进行、单和弦和完成音效均保存在当前浏览器来源的 `localStorage` 中，不会发送给 daemon 或写入 AgentHub 配置。Activity 波形由 AgentHub 全局 Activity SSE 流驱动：每个 Session 在每个一秒活动帧中只产生一个波峰，不受底层 event 数量影响；同一帧内多个 Session 的波峰与活动蜂鸣共用四个固定的 250ms subdivision，从右侧进入后平滑向左滚动。声音和视觉波峰统一延后 300ms；每个 Session ID 稳定绑定四套已调波形之一，单个波峰前后各占 200ms。九秒时间带由两张 Canvas 交替承载，基础微动和新增波峰只在视口外绘制，像素进入可见区后只随整条时间带平移，直至离开。1 至 4 个 Session 使用确定性密度节奏型，更多和弦音在轮换的 subdivision 上同时播放，而不会继续缩短间隔。活跃 Session 标签一行一个，每次有新活动时变亮，并在 10 秒内恢复初始颜色。Turn 成功结束后，该行以黄色保留 5 分钟；失败或取消时则以红色保留，新 Turn 会恢复活跃样式。结束 Session 的音高只继续占用 10 秒，随后即可由其他 Session 复用。Activity 设置可以保持单个和弦，也可以切换到内置卡农 C 大调进行（C–G–Am–Em–F–C–F–G）；每个和弦独立随机持续 1～6 个一秒活动帧，边界处所有 Session 同时切换到新和弦的离散音高，不做滑音或错峰迁移。完成提示音使用内置的 6 个 Codex Beeper MP3。

Provider 封装一个本地 Agent 运行时或协议；Agent 引用一个 Provider 并保存具体启动参数。Agent 没有独立的 id：它的 `name` 必填（最长 80 个字符），在去除首尾空白后大小写不敏感地唯一，并且是唯一引用键——配置、API、CLI 和 Session 记录都使用它。每个 Session 都用显式 Agent 名称创建（`POST /v1/sessions` 要求 `agentName`，CLI 要求 `--agent`）；名称匹配不区分大小写，Session 记录的是配置中的规范拼写。未知或缺失的 Agent 会直接返回明确错误，不会被路由到其他 Agent。

重命名 Agent 是安全的：当一次配置保存把某个名称替换为唯一一个其余字段完全相同的 Agent 时，daemon 会向每个引用旧名称的活动 Session 追加 `session.agent` 事件，使这些 Session 跟随重命名。歧义重命名（存在多个字段相同的候选）会被拒绝并给出可操作错误；删除 Agent 或重命名到不存在唯一目标时，旧 Session 会以清晰的“unknown agent”错误失败，而不会猜测到其他 Agent。

推荐使用 Web UI 的 **Settings** 界面管理配置。**Providers** 区块刻意保持极简：列出四个内置 Provider（Codex、Kimi、Grok/Pi、OpenCode）及其可用性，不提供用户控制的启用/停用开关，也不提供 Provider 增删——只要可执行文件能解析成功，Provider 就可用。启动后 daemon 会自动从 PATH 和常用安装目录探测可执行文件；找到则显示为可用并展示解析出的路径，每行提供内联编辑器修改可执行文件路径，确认时校验路径有效性、无法解析的路径会被拒绝保存（`PUT /v1/config/providers/{id}`）；清空路径则恢复自动探测。**Agents** 区块保留结构化、带校验的表单。所有修改都通过 daemon API 提交，daemon 仍是配置文件的唯一写入者，无需手动编辑 JSON。

**General** 区块配置由 daemon 管理的 OnWatch 集成；**Activity** 区块的活动、声音和逐条 quota 可见性偏好只保存在当前浏览器来源的本地存储中，不通过 `GET` 或 `PUT /v1/config` 同步。设置页会列出 `/v1/quota` 当前返回的 quota 条目；隐藏某条后，展开的 Beeper 列表和折叠态 quota 轮播都会过滤该条。余额型 quota 行（例如 DeepSeek 余额）还可以为每个 Provider 配置余额总额：剩余占比按 当前余额/总额 重新计算（默认 `100`）。Provider 配额只由 daemon 从 OnWatch 拉取，经归一化和按配置间隔缓存后通过 `GET /v1/quota` 暴露；Basic Auth 密码保存在本机权限为 `0600` 的配置文件中，但所有 API 响应都会将其抹除。Session 活动只来自 AgentHub 自身持久化追加且用户可感知的 Turn 事件，并在 `GET /v1/activity/events` 中按 Session 聚合为一秒一帧。消息、思考、工具、审批、Turn 错误与 Turn 终态计为活动；Session/进程生命周期记账、消息投递记账、原始 Provider 通知、后台 metadata 和 stderr 不计为活动，避免 daemon 重启或空闲 Provider 维护动作让旧 Session 重新显示为活跃。浏览器只保持一条全局 EventSource，并通过 Web Audio 合成可选的运行蜂鸣。每个活跃 Session 按第 5、4、6、3、7 八度带的顺序领取所选大三和弦或小三和弦中的第一个可用音高，优先使用较低八度后再到最高音位；该音高保持稳定，直到当前 Turn 结束 10 秒后或 Session 退出活跃窗口，前 15 个并发 Session 使用互不重复的音位。结束态行继续保留 5 分钟：成功为黄色，失败或取消为红色；新 Turn 会清除旧结束态并重新领取音高。播放量化到四个固定的 250ms subdivision，并使用确定性节奏型和轮换；超过四个 Session 后，和弦音以归一化音量共享 subdivision，不再产生不规则的一秒分数间隔。用户选择的本地 MP3 完成提示音与活动和弦相互独立。此功能不会扫描 Codex 或其他 Provider 的原生 Session 文件。

Provider 的可执行文件无法解析时，其 Agent 会被标记为不可用（`GET /v1/agents` 中 `available: false` 并附原因），不会出现在新建 Session 的选择中。不可用不会中断已经在运行的 Session，既有 Session 历史仍可查看。

### Per-session 启动环境

`POST /v1/sessions` 接受可选的字符串映射 `launchEnvironment`。它会覆盖 daemon 环境中的同名变量，并只传给该 Session 的 Provider 进程；Codex 还会在 `thread/start` 和 `thread/resume` 时把每项映射为 `shell_environment_policy.set.<KEY>`，ACP 和 Pi 使用合并后的进程环境。该字段存放在持久化的 `session.created` 事件中，因此 event replay、daemon 重启和 Provider resume 后仍然有效，并发 Session 之间不会串值；没有该字段的旧 Session 继续直接继承 daemon 环境。

`launchEnvironment` 会明确写入 Session 的 `events.jsonl` 和可重建的 `session.json`，也会由 Session API 返回。不要放入任何不希望持久化的凭据或其他 secret。Session 文件继续使用 `0600` 权限，但文件权限不能替代专门的 secret 存储。

### Agent 环境变量

Agent 配置还可以携带一个可选的字符串映射 `environment`，可以在 Web UI 的 **Agents** 面板或 `config.json` 中编辑。daemon 为该 Agent 启动 Provider 进程时，会先把 Agent 环境合并到 daemon 环境之上，再把 Session 的 `launchEnvironment` 合并到最上层，因此优先级为 `daemon < Agent < Session launchEnvironment`（Session 值覆盖 Agent 默认值）。Codex 会拿到合并后的进程环境，并在 `thread/start` 和 `thread/resume` 时把每项映射为 `shell_environment_policy.set.<KEY>`；ACP 和 Pi 使用合并后的进程环境。

与持久化的 per-session `launchEnvironment` 不同，Agent 环境是“活”配置：Session 只记录 Agent 名称，因此启动或恢复 Session 时会重新读取 Agent 当前的环境变量，适合存放该 Agent 所有 Session 共用的默认值。这些值保存在 `config.json`（`0600`）中并由 `GET /v1/config` 返回，不要在其中存放凭据。

### 模型枚举

每个内置 Provider 都可以通过各自的官方接口报告本机当前可用的模型——不创建 Provider Session，也不写入 Provider 配置：

- Codex：app-server 的 `model/list` 请求（按账号过滤，含展示名、默认标记和隐藏模型过滤）。
- Kimi：`kimi provider list --json`（已配置的模型注册表，含展示名）。
- Pi：`--no-session` 模式下的 RPC `get_available_models` 命令（覆盖所有已配置上游；会标记 Pi 默认使用的模型）。
- OpenCode：`opencode models --verbose`（已配置 Provider 及 OpenCode Zen 免费模型，含展示名）。

`GET /v1/providers/{id}/models` 把四方统一为 `{ "provider": {...}, "models": [{ "id", "label", "default" }] }`，其中 `id` 就是可直接写入 Agent `model` 选项的值。结果按 ID 去重、保持 Provider 原有顺序，带短期缓存（成功 5 分钟、失败 15 秒）与并发去重，并在每次配置变更（整体保存或 Provider 路径修改）时失效。失败按类别区分，便于客户端分别展示：`404 unknown_provider`、`503 provider_unavailable`（CLI 缺失或无法启动）、`504 provider_timeout`、`502 provider_error`（上游或解析失败）；空列表是成功的 `200` 并返回 `"models": []`。该端点是只读的：不创建 Provider Session，也不修改配置。

在 Web 设置中，Agent 的 **Model** 字段是由该端点加载的下拉框，不再是自由文本输入：先选择 Provider，再选择模型。空的“Provider default”选项表示不设置 `model` 选项。已保存但当前列表中不存在的模型会保留为明确的“saved, not currently listed”选项，直到用户主动更换；加载、重试、空列表和 Provider 不可用状态都会内联展示。

### 已移除的旧格式

Session 现在只使用显式 Agent 名称作为身份。Agent Profile、tag 路由、`defaultChatAgentId`、Agent `id` 字段，以及 `POST /v1/sessions` 的 `agentId` 字段均不再接受。daemon 不会重写旧配置、创建 id 映射 sidecar，也不会回放使用旧身份字段的事件 payload。升级到此版本前，请先把旧配置和 Session 数据完成一次性转换或备份。

命令发现顺序为：provider 的 `command`、`AGENTHUB_*_CLI`、`PATH`。支持：

- `AGENTHUB_CODEX_CLI`
- `AGENTHUB_OPENCODE_CLI`
- `AGENTHUB_KIMI_CLI`
- `AGENTHUB_PI_CLI`
`AGENTHUB_HOME=/path` 可把配置、数据和 runtime 状态全部隔离到一个目录，配置文件此时位于 `/path/config/config.json`，适合测试。该布局由用户显式选择，daemon 按原样读取。

## API

daemon 在 **`GET /agenthub/api.md`** 提供完整的 Markdown API 参考（`text/markdown; charset=utf-8`）：覆盖全部公共端点的参数、请求与响应体、错误码、curl 示例和 SSE 事件约定。文档内嵌在二进制中，无需前端构建即可访问，并由自动化测试保证与真实注册路由同步。获取方式：

```bash
curl -s http://127.0.0.1:4646/agenthub/api.md
```

规范客户端 endpoint 为 `http://host:port/agenthub`；下面列出的 API 路径都相对于该 endpoint。例如 `GET /v1/status` 的完整路径是 `GET /agenthub/v1/status`。

主要端点：

```text
GET    /v1/health
GET    /v1/status
GET    /v1/config
PUT    /v1/config
POST   /v1/onwatch/test
GET    /v1/quota
GET    /v1/activity/events
GET    /v1/agents
GET    /v1/providers/{id}/models

POST   /v1/sessions
GET    /v1/sessions
GET    /v1/sessions/{id}
DELETE /v1/sessions/{id}
POST   /v1/sessions/{id}/messages
POST   /v1/sessions/{id}/resume
POST   /v1/sessions/{id}/interrupt
POST   /v1/sessions/{id}/stop
POST   /v1/sessions/{id}/approvals/{approvalId}
GET    /v1/sessions/{id}/events
GET    /v1/sessions/{id}/event/{sourceEventId}
GET    /v1/sessions/{id}/turns
GET    /v1/sessions/{id}/turns/{turnId}
```

复数事件端点在普通请求下返回 provider-neutral `agenthub.semantic-events.v1` frame 的分页 JSON，包含 exclusive `after` cursor、`nextAfter`、`hasMore` 和最新 durable cursor；带 `Accept: text/event-stream` 或 `?stream=true` 时通过 SSE 返回相同 frame，并通过 `Last-Event-ID` 使用相同的 exclusive cursor 语义。daemon 先建立订阅并捕获 high-water mark，再分页重放完整 durable backlog，最后切换到 live；subscriber overflow 会关闭流，客户端可从最后一个连续 id 重连并从 `events.jsonl` 恢复，daemon 重启后同样有效。只有单数 `/event/{sourceEventId}` 会为排障返回精确 raw source Event。

资源编排客户端可在创建请求中提供 `idempotencyKey`，并在 `source.metadata`
保存 Workspace 实例、资源和代际标识；完全相同的请求在 daemon 重启后仍返回
原 Session。入站消息可提供稳定 `messageId`，并发或重启重试只会持久化一次规范输入。AgentHub 在返回可出队的成功响应前接管至少一次投递责任；Provider 失败、响应不明或 daemon 重启时继续重试。如果 Provider 已接收而确认 Event 尚未落盘，恢复可能产生少量重复投递。
Session 的 `inputCapabilities.steer` 明确说明当前 Provider 是否支持活动 Turn
输入；不支持时调用方应排队。`turns` 端点返回以稳定 event id 为引用的紧凑
Turn 索引，Session 归档后仍可读取。

### 不透明消息 Payload

schema v2 入站消息只包含面向 Provider 的 `text` 和可选、由调用方定义的
JSON `payload`。AgentHub 不解释 payload，只负责持久化并原样返回；`text`
则逐字节转发给 Provider。`steer` 与 `messageId` 仍是 AgentHub 的投递控制
字段，应用侧的来源、关联和展示信息都应放入 payload。

未提供 `schemaVersion` 的消息继续走旧协议：AgentHub 仍接受 `role`、
`sender`、`replyTo` 和 `correlationId`，并按旧规则拼接 Provider prompt。
已有 `message.input`、`message.user` 与 `message.user.steer` 数据无需迁移，
仍可读取和重放。

示例：

```json
{"schemaVersion":2,"text":"Message from agent \"Review Agent\":\nWorker 已完成扫描。","payload":{"schema":"my-app.message.v1","text":"Worker 已完成扫描。","role":"agent","sender":{"name":"Review Agent"}},"messageId":"msg-42"}
{"text":"旧客户端消息","role":"agent","sender":{"name":"Old Client"}}
```

### Semantic Events

`GET /v1/sessions/{id}/events` 向所有 client 提供 provider-neutral 的
`agenthub.semantic-events.v1` 协议。AgentHub Web 在自身源码中维护展示投影，
只依赖公共协议，不再发布或要求共享 timeline package。既有 Provider raw
Event 在读取时归一化；新的工具活动以 canonical `tool.call` source Event
持久化。只有精确 raw 排障才使用单数
`GET /v1/sessions/{id}/event/{sourceEventId}`。

### 归档 Session

`DELETE /v1/sessions/{id}` 归档一个 Session：daemon 先追加一条持久化的 `session.archived` 事件，再把整个 Session 目录移动到 Session Store 的 `Archive/` 子目录（`sessions/Archive/<session-id>/`）。不删除任何数据——`session.json`、`events.jsonl` 和其他文件一起移动。

- 仅严格处于 `stopped` 且没有未完成 Turn 或待处理审批的 Session 可以归档。端点不会强制停止 Session；请先调用 `POST /v1/sessions/{id}/stop`。
- 状态码语义：`200` 归档成功（重复归档幂等成功）；`404` Session 不存在；`409 session_active` Session 仍有活动工作；`409 session_archive_conflict` 归档目标被占用；`500 session_archive_failed` 文件系统错误（数据保持完整，重试或 daemon 重启会补完移动）。
- `GET /v1/sessions` 默认不返回归档 Session；用 `?includeArchived=true` 包含它们，或用 `?archived=true` 只列出归档 Session。归档后 `GET /v1/sessions/{id}` 和事件端点仍然可读。
- 归档 Session 只读：`messages`、`resume`、`interrupt`、`stop` 和审批写入都返回 `409 session_archived`。不支持取消归档。

CLI 对应命令是 `agenthub session archive <id>`、`agenthub session list --all` 和 `agenthub session list --archived`；Web UI 提供带应用内确认框的 Archive 操作和“Archived Sessions”视图。

### Provider 启动失败

创建 Session 会同步启动 Provider：握手请求（`initialize`、`session/new` / `thread/start` 及其 resume/load 变体）必须在 2 分钟启动超时内应答。无法应答的 Provider——例如进程因操作系统挂起隐私授权弹窗而卡在读取 Session 工作目录上——会让创建请求快速失败而不是一直挂起：

- API 返回 `502 provider_start_failed`，携带 Provider 的真实错误；超时时附带可操作的提示（在 macOS 上指向“系统设置 > 隐私与安全性”弹窗，例如“下载”文件夹或完全磁盘访问权限）。Web 新建 Session 窗口会显示该信息。
- 失败 Session 保留用于诊断：记录 `provider.error`、失败闭合 open turn，并在确认进程退出后收敛到带 `stopReason: "startup_error"` 的 `stopped`；之后可查看、resume、归档或保留。

活动 Provider Turn 不设置固定的总时长上限。ACP `session/prompt` 与 Pi
`prompt`/`steer` 会等待 Provider 的真实终态，即使推理或工具执行超过 15 分钟；
用户仍可通过 interrupt 或 stop 主动结束，Provider 退出或 daemon 关闭也会解除等待。
启动握手继续保留 2 分钟上限，普通控制请求则继续使用单独的有界超时。

### Strict stopped 生命周期与崩溃恢复

`stopped` 是唯一可信的 Provider 资源释放边界。stop 请求先写入
`stopping`；只有 adapter Wait 路径与进程组探测确认 Provider 及其子进程
不能再写工作目录后，调用才返回并追加
`session.state {"state":"stopped","reason":"requested"}`。

所有退出路径使用同一终态收敛器：正常退出使用 `completed`；崩溃先记录
`provider.error`、关闭 approval 与 open turn，再使用 `provider_error`；
启动失败使用 `startup_error`；显式 stop 和 daemon 优雅关闭使用
`requested`。daemon 被 SIGKILL 后，新 daemon 使用持久化的
`provider.process.started` 证据终止遗留进程组，确定性取消 pending
approval 与已投递的 open turn，最后使用 `daemon_recovery`。已持久接受但尚未确认交给 Provider 的输入不会被伪装成已完成；它保留在可恢复投递路径，并在 Provider 恢复后继续尝试。

## 数据与安全

所有用户持久数据统一存放在 `$HOME/.agenthub`：

```text
~/.agenthub/
├── config.json                 （provider 与 agent 配置）
├── sessions/<session-id>/
│     session.json
│     events.jsonl
├── sessions/Archive/<session-id>/   （归档 Session，文件相同）
├── logs/                       （以服务方式安装后的 stdout/stderr 日志）
├── server.json                 （临时 daemon 端点发现）
└── server.lock                 （临时单 daemon 锁）
```

`events.jsonl` 是唯一事实来源，`session.json` 与紧凑的 `turns.jsonl` 都是可重建投影。写入先 append + fsync 规范 Event，再更新投影；当前写入中断造成的最后半行可在启动或查询时修复。归档只是同一 Store 内的目录移动：如果 daemon 在追加归档事件和移动目录之间停止，启动时会补完移动，使物理位置始终与事件日志一致。目录默认 `0700`，敏感文件默认 `0600`。

`agenthub status`（以及 `GET /v1/status`）会报告当前生效的配置、Session Store、归档与日志路径，便于升级后确认布局。

### 数据布局

daemon 只读取统一的 `~/.agenthub` 布局。旧版本可能把 Session 存放在操作系统用户数据目录（例如 macOS 的 `~/Library/Application Support/agenthub`），把日志存放在 `~/Library/Logs/AgentHub`；这些路径不再自动读取或迁移。升级前请完成一次性、可验证的复制或导出，并保留备份；daemon 不会合并多个数据根，也不会替用户选择保留哪一侧。

无鉴权模式只适合本机和可信网络：默认仅监听 loopback，不发送 CORS 许可，拒绝跨 origin 的浏览器写请求，并校验请求 Host 必须指向本机地址。

## 验证

在干净的 PUA checkout 中可以直接编译和运行后端测试，不需要先构建前端。
PUA 与 AgentHub 共用 `../web` 下的 Svelte/TypeScript 工程，同时保留各自的 Vite 入口、嵌入资源与 binary。仓库根目录的 `scripts/build` 会构建两个应用入口，并在生成发布 binary 时强制校验入口文件。

```bash
go test -race ./...
go test -race -count=1 -tags=integration ./integration
go vet ./...
cd ../web
npm ci
npm run check
npm run build:agenthub
npm run test:sites
```

实现还经过本机真实联调：Codex app-server、Kimi ACP、Pi/Kimi K3、Pi/Grok、Codex 原生 thread 重启恢复，以及 Kimi 创建并写入工作区文件。

## 许可证

本项目采用 [BSD 3-Clause License](LICENSE)（New BSD License / Revised BSD License）发布。
