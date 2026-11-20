# PUA Web 服务

`internal/serve` 提供静态 Web UI、Workspace HTTP API，以及以资源 generation 为边界的 AgentHub 对话、输入、审批、停止、恢复和对账。Workspace 文件操作直接调用 `internal/app`，不会启动 `pua` 子进程。

Generation 生命周期的 canonical facts、operation 优先级、网络 effect 与 guarded commit 边界见 [`generation_lifecycle.md`](generation_lifecycle.md)。planner 是纯决策层；resource controller 串行化同一稳定 resource 的调度；store adapter 负责 durable receipt/current/retired 事实；AgentHub client 只负责网络副作用。统一恢复顺序为 `facts → plan → effect → guarded commit → replan`，旧 generation 的过期结果不得覆盖新 current。

## AgentHub 运行形态

`pua serve` 默认使用 `--agenthub-mode=embedded`：PUA 与 AgentHub 共用一个进程、一个 listener 和网络暴露边界。PUA Web/API 使用 `/` 与 `/api/...`；AgentHub Web、API 和文档固定位于 `/agenthub/`、`/agenthub/v1/...` 和 `/agenthub/api.md`。PUA 仍通过该 HTTP contract 调用 AgentHub，不直接依赖 Provider/runtime。

需要分离部署时，先运行 `agenthub serve`，再显式启动：

```sh
pua serve --agenthub-mode=external \
  --agenthub-endpoint=http://127.0.0.1:4646/agenthub
```

external endpoint 必须是以 `/agenthub` 结尾的规范 base URL。两种形态只改变 host/port，不改变 path。external 模式不取得 AgentHub daemon lock，也不在 PUA 退出时停止外置服务。

## 配置与所有权

持久化服务配置使用 schema version 6，包含 Workspace、当前 AgentHub endpoint、PUA instance ID 和 Profile 路由。embedded 模式会把从 PUA listener 派生的 endpoint 写回配置；该值仅用于展示与运行时连接，不用于推断下次启动的模式。每个 Workspace 可保存一个内置图标键；缺失或未知值回退为 PUA 默认图标。Settings 的 `User` 页签把用户名保存在当前浏览器的 `localStorage` 中，不进入服务配置或 Workspace 数据。

服务启动形态由 CLI flag 决定，不会根据持久化 endpoint 隐式切换。可用环境变量：

```text
PUA_SERVE_CONFIG    serve configuration file path
```

未设置配置环境变量时，配置默认保存于 `~/.pua/serve.json`。

每个被管理的 Workspace 同时只能由一个 `pua serve` 进程持有。服务启动时为配置中的每个 Workspace 获取 `<workspace>/.pua/serve.lock` 的 OS advisory 独占锁，并在整个生命周期内保持文件描述符打开。锁冲突会让启动整体失败并释放本轮已取得的锁。

## Workspace API

项目、任务、日志、归档、文件预览、Wiki 和 diff 路由都以显式 Workspace ID 为作用域。`POST .../tasks` 创建普通任务，接受标题和可选简述或完整 Markdown。`POST .../tasks/preview` 返回最终标题、Markdown、slug 和 Agent 绑定。

Server 确认 AgentHub 连接和 Workspace 所有权后，在对外就绪前清理旧任务模板。清理删除开放及归档 Project 的 `templates/` 及 Task 元数据的 `template` 字段，保留已有规格和其他数据。清理可重复运行。升级前由各 Project 将任务类型、创建时机和格式同步至 `spec.md`。

Scheduler API 同样委托 `internal/app`，提供 `GET/POST .../scheduler`、`PUT/DELETE .../scheduler/{scheduleId}` 与 `PUT .../scheduler/settings`。Server 不解析自然语言 condition；Web 界面使用这些接口维护调度项、独立绑定和唤醒间隔。

## Resource generation and AgentHub facts

每个 Workspace、Scheduler、Project、Task 都持久化显式 `{kind: profile|agent, name}` 绑定，不做父级继承。资源聊天在首条消息到达时懒创建代际；PUA 使用 Workspace 稳定 instance ID、资源 ID、代际编号/ID、绑定与 Profile revision 组成 AgentHub source metadata，并用代际 ID 幂等建会。PUA 向 AgentHub 发送 schema v2 消息：顶层 `text` 是 PUA 已完整组装、应原样交给 Provider 的 prompt，`payload` 则以 `pua.resource-message.v1` 保存原始正文、role、sender、Workspace instance、type 与 causation。AgentHub 不解释 payload；PUA 在历史、时间线和重试恢复时自行解码。浏览器用户输入先保存到独立聊天，再生成交流通道的内部待办；用户名表示来源，不能被解释为额外授权。旧 AgentHub `message.input` 中的顶层 role/sender 继续作为只读兼容数据处理。

Provider 消息封装跟随目标 Workspace 的 `en` / `zh-CN` 内容语言。新 Turn 的精简开场固定来源、对话对象、Chat 可见性和 work ID；公共规则写入托管 AGENTS.md，首次投递前更新其 managed block 并保留自定义内容。用户 Chat Turn 的普通阶段文本与 final 自动同步；内部 Agent、system 和 Scheduler Turn 保持内部可见，Agent 消息仍是单向的。旧冻结输入继续使用原封装与显式 publish。语言、受众和输出周期冻结在 mailbox provider context，重试不会改变。

资源代际按资源拆分保存在 `<workspace>/<control-dir>/runtime/resources/<resource-key>/`：`current.json` 是唯一可变 current，`generations/` 保存不可变 retired manifest；resource key 由稳定 Workspace instance ID 与规范化 resource ID 编码而成，与 Workspace 路径无关。`generation-store.json` 是版本化迁移 marker，旧的 `<control-dir>/runtime/generations.json` 与 `<control-dir>/gui-agent/runs.json` 会保留为 rollback evidence，缺少 `generationId` 的记录只进入 cold history，不进入 lifecycle reconcile。同一资源目录还包含原子提交的 `hot.json`、`receipts.json`、`outbox.json`、`scheduler.json` 和 `commit.json`：hot 只保留 `queued`/`delivering`/`interrupting`、结果不明、待重试、未收敛 notification/outbox 或 Scheduler Turn 终态观察所需的完整消息；普通终态进入不含正文的最小 receipt。receipt 固定最多 2,048 条且保留七天，另有同样有界的过期 ID 索引；过期索引内的查询返回 `message_receipt_expired`/HTTP 410，索引再次过期后才返回 `message_not_found`。`.message-locations/` 只是可重建的查找加速索引，资源文档才是事实源。HTTP 只有在资源文档临时文件完成 write + fsync + rename 且提交目录 fsync 后才返回 accepted。旧的全局 mailbox 文件和 migration marker 不再读取或改写；当前版本只使用资源目录中的 mailbox bundle。

三种模式共享同一 reconciler：新版固定受众 Turn 中的业务消息（包括同一用户的新消息）统一排队，`downgradeReason=fixed_turn_audience`；只有独立预告可以提前插入。旧 Turn 的 `steer` 保留原能力降级行为；`enqueue` 只在 ready 边界作为新 Turn 投递；`interrupt` 先记录被中断的稳定 Turn ID，只重试同一 Turn 的中断，确认其 terminal 后再开启新 Turn。已经处于 delivering/interrupting 的结果不明项最先收敛；其余项按 interrupt、steer、enqueue 优先级处理，同一类保持接受顺序，因此显式 steer/interrupt 可以越过早先等待的 enqueue。AgentHub 成功承担至少一次投递责任后状态才变为 delivered；这不表示 Turn 已完成。绑定替换不再搬运消息：steer 成功后留在旧 Turn，enqueue 等新 generation，interrupt 终止旧 Turn 后再随 replacement 收敛。归档资源拒收新消息；尚未开始发送的项进入 `undeliverable`，已开始发送但无法确认结果的项进入 `delivery_unknown`，两种终态都可按 message ID 查询。

### 工作对象状态与消息 API

公共资源入口仍以 Server 已拥有的 Workspace ID 为作用域，但目标只使用稳定资源 ID `workspace`、`projectN` 或 `projectN.taskN`，不要求 run ID 或 AgentHub Session ID；generation/Turn 只通过返回的稳定 ID/ref 关联：

```text
GET  /api/workspaces/{workspaceId}/resources/{resourceId}/status
POST /api/workspaces/{workspaceId}/resources/{resourceId}/messages
GET  /api/workspaces/{workspaceId}/messages/{messageId}
POST /api/workspaces/{workspaceId}/messages/{messageId}/steer
GET  /api/workspaces/{workspaceId}/resources/{resourceId}/history/turns
GET  /api/workspaces/{workspaceId}/resources/{resourceId}/history/turns/by-id?generationId={generationId}&turnId={turnId}
GET  /api/workspaces/{workspaceId}/resources/{resourceId}/history/turns/{turnRef}
GET  /api/workspaces/{workspaceId}/resources/{resourceId}/history/events/{eventRef}
GET  /api/workspaces/{workspaceId}/resources/{resourceId}/events
GET  /api/workspaces/{workspaceId}/resources/{resourceId}/stream
POST /api/workspaces/{workspaceId}/resources/{resourceId}/approval
POST /api/workspaces/{workspaceId}/resources/{resourceId}/turn/end
POST /api/workspaces/{workspaceId}/resources/{resourceId}/generation/end
PUT  /api/workspaces/{workspaceId}/generation-policy
PUT  /api/workspaces/{workspaceId}/stall-watchdog-policy
POST /api/workspaces/{workspaceId}/resources/{resourceId}/uploads
PUT  /api/workspaces/{workspaceId}/resources/{resourceId}/read
GET  /api/workspaces/{workspaceId}/users
POST /api/workspaces/{workspaceId}/users
PUT  /api/workspaces/{workspaceId}/users/{name}
DELETE /api/workspaces/{workspaceId}/users/{name}
```

手动结束当前 Task Turn 时，Server 会在 AgentHub Interrupt 成功后把持久化 Task state 设为 `paused`，并在同一个资源控制器临界区取消尚未送达的 steer；这样 terminal event 不会把用户刚停止的 Task 自动续推。Workspace、Project 和 Scheduler 的 End Turn 不写 Task state。响应中的 `taskState` / `taskStateError` 分别报告自动暂停结果和异常。

`POST .../generation/end?generationId={currentGenerationId}` 只在没有活动 Turn/approval 时接受。它以 generation ID 防止过期页面误操作，随后沿统一 lifecycle 完成 Session Stop、stopped 确认、Archive 和 generation retire；不会立即创建空 successor，下一条 mailbox 消息会按资源当前绑定懒创建新 generation。

`GET /api/workspaces/{workspaceId}/tree` 返回层级资源。浏览器的 Running 筛选读取各节点的 `runtime.activeTurn`，Unread 筛选读取当前用户的 `chat.unreadCount`。筛选保留命中 Task 所在的 Project 和虚拟目录。`activity.running` 继续服务桌面活跃 Turn 计数的兼容路径。

资源树的 `latestTurnNumber` 表示最新已完成 Turn。旧客户端的 `PUT .../read` 和 `PUT .../chat/runtime/read` 继续验证身份及 `throughTurnNumber` 的已完成边界，成功时返回 `{}`。树行展示自身的 `chat.open` 图标及 `chat.unreadCount`，Chat 关闭后保留历史与未读数字。共享 Chat 通过 `POST .../chat/read` 按消息 ID 推进各用户自己的游标。图标和计数通过只读轮询与操作后刷新同步，执行事件通过 SSE 和状态同步刷新。

共享 Chat 分页响应的 `queuedMessages` 投影当前仍待执行的用户消息，顺序来自持久投递记录，独立于 `messages` 的游标范围。历史分段的 `generation.turnTotals` 来自 AgentHub 的整代统计，包含已启动 Turn 数、已知累计执行毫秒数和可选的活动 Turn 起始时间。旧 AgentHub 可省略整代统计。缺失历史起止边界时省略执行毫秒数。

共享 Chat 消息可包含 `inputMessageIds`，用于关联用户消息的邮箱输入；历史投影的自动回复包含 `turnId` 和 `startEventId`。执行历史摘要的 `triggerMessageId` 来自 canonical Turn，`triggerType` 为已知的触发类型，`triggerSourceResourceId` 来自已知的触发因果关系。续接的历史摘要和状态接口的等待项可包含可选 `resumeSource`，提供用于显示的原工作角色、类型和发送者。旧记录只返回仍能核对到的字段。展开 Turn 时可按消息 ID 或事件位置省略同一时间线已展示的共享消息。

个人 UI 和资源状态保存在 `<control-dir>/users/{name}/ui-state.json`，公共 Turn 编号高水位保存在 `<control-dir>/resource-state.json`。用户级请求通过 `X-PUA-User` 指定用户。`ui-state` 保存侧栏虚拟目录和顺序（`folders` 与 `folderOrder`），`taskOrder` 中混合记录项目根级的 Task 和目录 ID。虚拟目录只挂在 Project 下。归档 Project/Task 时删除相关阅读状态和目录引用。

用户名只允许 ASCII 字母、数字、下划线和减号，最长 80 个字符；`workspace`、`scheduler`、`project`、`task` 以及 `projectN`、`taskN` 这类与稳定资源 ID 相同或近似的名字（不区分大小写）被保留，不能注册为用户名，避免 `pua message send --to=` 目标产生歧义。`POST .../users` 显式注册用户，`PUT .../users/{name}` 接收 `{ "preference": "..." }`，删除用户会级联删除该用户目录，但不会改写历史消息中的 sender。用户只是 Workspace 范围的身份标记，不构成认证或权限边界。

用户聊天由 `<control-dir>/chats/{chatId}.json` 保存，与 AgentHub 事件独立。每个资源有一条 Workspace 用户共享的长期聊天，用户消息标明发送者，各用户的未读游标独立；输入直接进入资源执行 Agent 的 mailbox。独立交流运行时已停用，旧 `dialogue-{chatId}` 与 `/chat/runtime/...` 仍按原用户定位历史审计。

`GET .../resources/{id}/chat` 读取资源共享聊天，`POST .../chat/open|close|messages|read` 管理聊天。`GET .../chats` 查询聊天列表和当前用户未读数。公开消息来自用户输入和可见 Turn 的普通 Agent 文本。输入与 outbox 原子保存，后台按固定 mailbox ID 投递。关闭保留历史并撤销回复资格，资源继续执行。重开时增加 epoch。Workspace 聊天始终开启。

公开聊天支持按消息 ID 分页：`GET .../chat?limit=40` 返回最新一页，`before` 或 `after` 分别返回指定消息之前或之后的一页（不含该消息）。`limit` 范围为 1–100，两个游标不能同时使用；分页结果中的 `hasOlder`、`hasNewer` 表示是否还有前后页面，`unreadCount` 始终针对整个聊天。未指定 `limit` 时保留完整历史响应以兼容现有客户端；聊天写操作也可指定 `limit` 来限制响应消息数。当前磁盘存储仍是整份 JSON，分页减少网络传输和浏览器渲染，不消除服务端读取及解析完整文件的开销。

可见 Turn 使用 `pua chat show --work=<work-id>` 读取共享历史，通过普通输出自动发布。旧 `--reply-to`、显式发布和 Notes 已退役。已迁移消息的 ID、来源、已读游标和旧 Notes 留在现有记录。旧每用户 Chat 文件继续供只读历史访问，新共享 Chat 直接初始化。独立交流运行时继续隔离，旧执行事件可通过 `/chat/runtime/history/...` 查阅。

共享记录保留原迁移确定的消息顺序、映射后的 ID 及原发送者。用户各自的未读状态继续沿用共享记录内的游标。共享 outbox 继续按原 mailbox ID 去重，并用 Chat 周期约束输入的投递资格。原笔记、旧 outbox 和历史事件仍可从原始文件、已有备份和旧身份审计。回退前保存最新的共享记录与 Telegram 状态。

Workspace、Project、Task 的详情页提供独立的 Spec 和 Memory 页签。`PUT .../resources/{id}/spec?path=...` 只保存该资源的 `spec.md`，要求同源编辑请求和已选择的 Workspace 用户。`PUT .../resources/{id}/documents?path=...` 保存 `memory.md` 及该资源允许的模板和产物。两个入口分别校验内容哈希、资源归属和普通文件路径。归档资源只读。用户名选择属于本地产品上下文，认证和同 UID 文件权限的边界见迁移说明。

对话事项在 Memory 中维护，保留来源消息和 History 引用。工作状态和通知状态分别记录，自动同步成功后标记已发布。预览和原文接口将已迁移的旧资源说明路径映射到 Memory，旧路径只读。[文档维护与回退](../../docs/resource-documents-migration.md)

资源说明本身不触发工作，长时间等待使用 Scheduler 的真实条件。旧聊天 timer 在迁移和 reconcile 中清零，不恢复旧自动轮询。浏览器 SSE 与状态读取只刷新展示，不驱动模型。

旧 Inbox 路由统一返回 410，原始文件留在磁盘。历史 API、CLI 和已有引用继续使用。旧交流事件读取使用 `/chat/runtime/history/...`，交流流使用 `/chat/runtime/stream?user=...` 选择注册用户。其他旧交流读取入口使用 `X-PUA-User`。

`PUT .../generation-policy` 更新 `workspace.json` 中统一覆盖 Workspace、Project、Task 和 Scheduler 的自动轮换策略。策略有两个独立开关：usage-based rotation 默认在 20 个已结束 Turn 或累计 120 分钟 Turn wall-clock 的 OR 阈值后轮换，Turn 之间的 idle 不计时；inactivity-based rotation 默认在 AgentHub Session 最后一次语义 Turn 活动距今达到 1440 分钟后轮换。两者都只在下一条 queued input 即将开始新 Turn 时检查，活动 Turn 的 steer 不检查。旧 `enabled` 配置迁移时同时控制两个新开关，设置页可分别关闭并保留阈值。

`PUT .../stall-watchdog-policy` 更新 `workspace.json` 中对所有资源统一生效的活动 Turn 停滞看门狗，正文为 `{ "enabled": true, "timeoutMinutes": 30 }`。缺少该配置的现有 Workspace 与新 Workspace 默认开启、超时 30 分钟；只有 `running` 且没有 pending approval 的 Turn 会被监控。AgentHub 只把 Turn 内的 message/reasoning/tool/approval/provider error/terminal 事件计为有效活动，普通 Session 更新时间、metadata 和 stderr 不会续期。超时后 PUA 持久化去重状态，Stop 同一 Session，再通过 mailbox 系统恢复消息 Resume 同一 Session；每条连续恢复链最多自动尝试一次，避免 Stop/Resume 无限循环。设置页可关闭看门狗或调整超时时间。

发送正文示例：

```json
{
  "text": "Review the current implementation.",
  "mode": "steer",
  "role": "agent",
  "sender": { "id": "project1.task1", "name": "project1.task1" },
  "senderWorkspaceInstanceId": "ws-0123456789abcdef"
}
```

发送响应包含 `messageId`、`resourceId`、正文（仍在 hot 中时）、`receipt` 标记、`requestedMode`、`actualMode`、`downgradeReason`、对外消息状态、接受/提升时间、可再次 GET 的 `reference`、当前 generation/Turn 关联以及可选的 `lastErrorCode`/`lastError`。历史结果与投递失败通知可能包含 `type`、`causation` 和源消息上的 `notification` receipt。内部 `queued` 对外映射为 `waiting`。`GET .../messages/{messageId}` 对保留的冷 receipt 返回无正文但带 `receipt: true` 的诊断结果；超过 retention 返回稳定 `message_receipt_expired`（HTTP 410），不把已存在的消息伪装成从未存在。状态响应的公共状态只会是 `idle`、`working`、`attention_required`、`unavailable` 或 `archived`；消息等待数与 `waitingMessages` 单列，不会把 Task 标成 queued。状态不再展示 creator；显式绑定、当前 generation/replacement、Turn/steer capability 和最近错误仍可诊断。`POST .../steer` 保留同一个 waiting mailbox 项；旧 Turn 支持 steer 时立即插入，新版固定受众 Turn 则保持排队并发送独立预告，不消费原输入。稳定错误 code 还包括 `message_not_waiting`、`steer_unavailable` 和 `message_receipt_expired`。provenance 只是来源元数据，不构成认证、授权或指令优先级。

批量队列接口以 `mailbox_batch_v1` capability 暴露：`GET .../resources/{resourceId}/messages` 只列出 waiting Agent 消息元数据；`POST .../messages/read` 读取显式 ID 的完整正文但不消费；`POST .../messages/ack` 在活动内部 Turn 中把显式 ID 原子确认为 `handled` 终态并保存完整正文、摘要、work/generation 与 History 审计。首批仅允许没有 causation、没有投递尝试、非 interrupt、非续接依赖、且带来源 Workspace instance ID 的稳定资源 Agent 输入。读取会按当前 work 持久化 receipt claim；ack 可以自动复用 claim，也可显式提供同 ID 顺序的 receipts 作恢复。相同 `operationId` 与相同请求幂等返回已提交 operation，冲突请求被拒绝；`GET .../message-operations/{operationId}` 用于未知结果恢复。opener 与独立预览附带冻结的 queue snapshot：waiting 总数、同稳定来源 waiting、批量可确认数、投递中/中断中/结果未知数，并标明是否排除当前 opener 或包含 preview 原消息。

跨页读取会按 work 累计有效 claim，而不是让后一页覆盖前一页；重启后 claim 会按当前 generation、正文和 eligibility 重新校验。read/ack 除 Session 当前状态外还查询 canonical Turn，Session 投影仍显示 running 但 Turn 已 closed 时拒绝写入。CLI 在发送 ack 前把 operationId、work、IDs、receipts 和 summary 冻结到受控目录；响应或 operation 查询都失败时保留原 operation，并提示用 `pua message ack --resume=<operation>` 或以 `pua message operation show` 核对，重试不会生成新的随机 operation。

跨 Turn 恢复时，先用 `pua message operation show --id=<operation-id>` 核对服务端结果。已提交的操作可通过相同请求幂等取回结果，即使原 Turn 已结束。服务端明确返回操作不存在且原 work 仍活动时，可用 `pua message ack --resume=<operation-id>` 重试原请求。若服务端明确返回操作不存在且原 work 已结束，应留存旧 operation ID 和查询结果。在新的活动内部 Turn 中，重新核对消息状态、读取仍在等待且有资格确认的正文，并确认所需工作已完成。确认时显式传入新的 `--operation=<new-id>`，避免相同 IDs 和摘要匹配到旧的本地冻结请求。查询失败且结果仍未知时，先核实旧操作结果。

带 `Files` 附加文档的事务使用字符串版本 `mailbox-batch-v2`，并将资源文档版本提升到 2。旧版 Server 按整数解析事务版本会直接拒绝该 journal，不会忽略 `Files` 后删除提交标记；新版仍可读取无附加文档的 v1 存储。回退到旧版前应停止写入并确认没有未恢复的 v2 journal；旧版会拒绝 v2 metadata，而不是静默降级。事务恢复会校验每个 `Files` 项的临时或最终文件，缺失时不删除 journal。

curl 示例：

```bash
curl -sS http://127.0.0.1:4936/api/workspaces/WORKSPACE_ID/resources/project1.task2/status
curl -sS -X POST -H 'Content-Type: application/json' \
  -d '{"text":"Please review this.","mode":"enqueue","role":"agent","sender":{"id":"project1.task1"},"senderWorkspaceInstanceId":"ws-0123456789abcdef"}' \
  http://127.0.0.1:4936/api/workspaces/WORKSPACE_ID/resources/project1.task2/messages
curl -sS http://127.0.0.1:4936/api/workspaces/WORKSPACE_ID/messages/MESSAGE_ID
curl -sS -X POST http://127.0.0.1:4936/api/workspaces/WORKSPACE_ID/messages/MESSAGE_ID/steer
```

资源绑定的显式修改仍按资源收敛；Profile 映射修改只保存配置，不遍历资源、不预写 replacement。mailbox 在每个新 Turn 真正启动前重新解析当前绑定：Agent 未变化时沿用并刷新当前 generation，Agent 变化时才开始 Stop、确认 stopped、Archive 和按需创建新 generation。活动 Turn 的 steer 不开启新 Turn，继续使用当前 generation 的 Agent；enqueue、interrupt 后的新 Turn、Scheduler tick 和跨资源通知都走同一 mailbox 边界。删除仍被引用的自定义 Profile 不会改写资源显式绑定；解析按资源类型默认、再按全局 `default` 回退，同时在 generation 暴露 `agentConfigError` 和实际 `resolvedProfile`。

PUA 定期从 AgentHub 拉取 Session 状态并以同一 desired-state reconciler 更新 durable generation 记录和已存在的 runtime；轮询不再扫描全部资源以重算 Profile 绑定。Task 或 Project 归档首先以一个可恢复的顶层目录移动提交事实；它不因活动 Turn、queued/hot mailbox 或 Stop/Archive 的未知失败而阻断。Project 子树中的所有 generation 随后由 resource planner/reconciler 异步执行 Stop、确认 `stopped`、Archive；未知响应、服务重启和中间状态均由持久事实与重复 reconcile 恢复。

Task 工作流状态在 mailbox 只接受未投递时不切换；资源控制器只在真实投递边界将 Task 设为 `in_progress`，并在新工作链中消费上一 Turn 的终止标记。Turn 终止收口只在没有更新 queued 消息或活动 Turn 时生成自动续推；`in_progress` 会获得继续推进提醒，`waiting` 则要求 `scheduler.json` 至少有一条 target 精确指向当前 Task 的调度项，否则生成注册唤醒 condition 的系统提醒。两类收口共用持久化 marker、确定性消息 ID 和最多 3 次的工作链恢复预算。

同一周期 reconciler 还读取每个 Workspace 的 `scheduler.json` 并生成稳定、enqueue-only 的 `scheduler_tick` mailbox 消息；该消息显式以 `requestedMode=enqueue`、`actualMode=enqueue`、`ModeFrozen=true` 接受，不能被生成消息公共 helper 改成 `steer`。空列表不会生成消息；配置变化在 Scheduler 忙碌时最多保留一个 waiting tick。间隔基准只接受由 Server tick 触发且 canonical 状态为 `completed` 的 Turn 结束时间，普通用户 Turn 不会重置计时；失败 tick 和无法恢复历史的 tick 使用恢复原因重新唤醒。资源级 `scheduler.json` checkpoint 保留最近 tick 的稳定 ID、generation/Session/Turn、配置 digest 和 delivery/Turn terminal 边界，即使 tick receipt 已 compact，Server 重启也不会重复或丢失恢复判断。

资源 generation 的创建、AgentHub 绑定和生命周期由资源级 API 与 reconciler 负责；不再存在独立的 PUA Session store 或写入 API。资源级 Session Lock 已删除；资源聊天由单一当前代际串行化，Web 界面不再提供 Session 新建、切换、恢复或关闭控件。内部 lifecycle controller 仍保留 AgentHub Session 的 Stop/Resume effect；这不是用户级 Session API，也不建立旁路恢复状态机。

ready 的 current generation 在连续空闲 30 分钟且没有活动 Turn/approval、待处理 mailbox 投递或生命周期收敛时由 PUA 自动休眠。空闲边界持久化在 generation 记录中，reconciler 会重新核对精确 AgentHub Session，在资源/Turn 互斥边界内执行 Stop；确认 durable `stopped` 后仍保留同一 current generation/Session，公共 runtime 标记为 `idle-suspended`。后台协调器只对活跃或生命周期待收敛的 runtime 保持 2 秒 Session 投影；全部稳定时退避到 10 秒。全量 current generation、归档与遗漏恢复每 30 秒冷审计一次，idle 与 Scheduler 使用持久化 deadline 提前唤醒；mailbox 正常投递和投递通知由写入/完成事件立即唤醒，另分别保留 10 秒与 30 秒恢复检查。可恢复的 suspended/stopped current generation 仍允许浏览器保持只读事件流，避免同一 Session Resume 后丢失实时更新；永久关闭的浏览器 EventSource 会在后续状态变化时重新建立。之后的 user、agent、system 或 Scheduler 消息保留在 mailbox，按同一 planner 规划幂等 Resume，确认原 Session ready 后再投递；PUA 或 AgentHub 重启后观察到的 `requested`/`daemon_recovery`/idle stopped 统一走这条按需路径。没有消息时保持 stopped，不批量启动 provider。binding/profile 变化、资源归档、Session archived/missing、身份/source 不匹配、AgentHub 明确报告 provider/native resume 不可恢复，或 Workspace Generation 自动轮换策略达到阈值时，才进入 Stop/Archive/retire 并按需创建新 generation；临时 Resume 失败保留 mailbox 与 receipt，等待下一次 receipt/replan。Provider 启动失败不同于临时 Resume 失败：结构化 `provider_start_failed` 或精确 Session 的 durable `stopped` + `stopReason=startup_error` 会终止当前 generation，并让尚未投递的 Task 输入消耗工作链恢复预算；连续 3 次后消息转为 `undeliverable`、Task 转为 `error`，不会无限 Resume。自动轮换限制旧 generation 是否还能开始下一个 Turn：poller 在 Turn 终态只完成 completion 与 Task 状态收口，不主动轮换；mailbox 在 queued input 即将从 inactive Session 开始新 Turn 时，按需从 AgentHub 的规范 closed Turns 投影刷新 usage，并直接使用 Session 的规范 `lastActivityAt` 判断 inactivity。usage 达到阈值后持久化 `turn_limit`，否则 inactivity 达到阈值时持久化 `inactivity_limit`；两者都保留 queued input 并由 replacement generation 接管。stopped generation 会在 Resume 前完成检查，活动 Turn 的 steer 不触发检查。没有下一条消息时，超预算或长期不活跃的 generation 可以保持 ready 或正常进入 `idle-suspended`。普通轮询和 Server 重启不会制造主动轮换，也不会丢失已持久化的 replacement intent。

资源历史接口以版本化 base64url opaque reference/cursor 绑定 Workspace instance、资源和 generation。列表跨 generation 反向分页，保留创建时标题、绑定与解析结果；缺失、损坏或暂时不可读的 AgentHub 历史形成显式 gap，单个 gap 不阻断更旧历史。浏览器先加载 Turn 摘要，由视口按需请求详情；只有当前 generation 的开放 Turn 通过资源级 `events` 读取 `agenthub.semantic-events.v1` frame 并接入 SSE，terminal 后替换为紧凑 Turn。跨 generation 的复合 key、滚动锚点、未读与草稿由 PUA 管理。

PUA Web 在自身源码中维护 semantic Event 到连续 `activity`（思考与工具调用）及其他可见条目的展示投影，只依赖 AgentHub 的稳定公共协议，不再 vendoring AgentHub timeline package。AgentHub 在读取旧日志时负责把 Provider raw Event 归一化为新协议；PUA 不解析 Provider payload，也不兼容新旧 AgentHub 协议组合。精确 Event 排障通过单数 `/event/{sourceEventId}` 获取 raw source Event。AgentHub source app 与恢复诊断事件使用协议标识 `pua` / `pua.notice`。上传直接写入目标资源的 `artifacts/upload/`，不会为了上传创建 generation，未发送路径仍留在资源级草稿中。

资源 generation 向 AgentHub Session 注入 `PUA_WORKSPACE_ROOT`、`PUA_WORKSPACE_INSTANCE_ID` 和 `PUA_RESOURCE_ID`，供本地 CLI 验证 Agent sender provenance。Agent 消息在所有投递模式下都是单向的；需要回复时显式发送新消息。发送 API 已移除 `subscribeResult`，旧请求包含该字段时返回 `invalid_request`。`undeliverable`/终态 `delivery_unknown` 仍可生成独立的 `delivery_terminal_notice`，通过可恢复 outbox 投递并保留 receipt。旧结果订阅不再触发回传，遗留 outbox 操作终止并保留 receipt；已进入目标 mailbox 的旧结果在未投递时取消，已开始投递的项标为 `delivery_unknown`。旧的订阅字段仍可能出现在历史 receipt 中，仅用于审计。AgentHub 已接受的消息无法撤回；回退到旧 Server 可能重新启用其仍能读取的旧订阅。

持久 schema 升级是无损的：一次性版本化迁移会删除 Workspace/Project/Task 中旧的 `creator`/`createdBy` 字段，已完成历史不会重新批量通知。旧 `<control-dir>/runtime/mailbox.json`、mailbox migration marker 和 generation `pendingMessages` 已退出在线兼容范围；当前版本忽略这些残留文件和字段，不主动清理生产数据。`<control-dir>/initializing.json` 表示可重试但尚未完成的 Workspace 初始化，正常打开会拒绝该半成品并提示重新执行 `pua init`。发布前可备份 Workspace；代码回滚不要求改写资源 JSON，回滚前应暂停跨 Workspace 通知。

Telegram 接入位于 `telegram_*.go`：每 Workspace 独立配置、私聊话题与资源 Chat 映射、一次性用户配对、持久输入游标和公开回复投递。使用方法及恢复边界见 [Telegram 接入说明](../../docs/telegram.md)。

## 固定受众与工作续接

`GET/POST .../resources/{resourceId}/turn-work` 提供持久工作查询与控制。`GET ?chat=true&work=<id>` 读取该可见工作所属开启周期的最近共享消息。控制必须引用开场 work ID，并匹配当前 generation、Session 和 Turn；拒绝未决审批、过期 work、重复但不同的控制请求。

- `pua turn switch --message=<queued-id>` 在当前 Turn 结束后投递所选原消息。`pua turn switch --to=chat --context="要沟通的事"` 从内部 Turn 安排新的可见 Turn。两种切换都默认恢复，可加 `--checkpoint="下一步或未决操作"`。仅支持一层挂起。
- B 成功结束后，服务端用稳定 mailbox ID 创建 A2；保留 A 的受众和 Chat epoch，开场携带 A 的检查点及 A、B 的 History 查询引用。A2 完成后收口原工作。
- `pua turn finish --work=<id> --reason=cancelled`：安全收尾后直接结束，不建立续接；原排队消息仍正式处理，获得之前取消的 History。普通结束同样不自动建立续接，不解析 final 猜测意图。
- `pua turn cancel-resume --work=<id> --reason="用户取消"`：B 明确取消挂起的 A。手动停止、Task paused 和资源归档同样取消待恢复安排；失败或结果不明的 B 使 A blocked。
- 旧版 `turn yield` 仍要求检查点。旧版 `turn notify` 仅在附带检查点时恢复原工作。

工作状态保存在 `<control-dir>/turn-work/{resource-chat-id}.json`，所有写入由资源 controller 串行，采用原子持久提交。它只保存续接与输出收据，AgentHub canonical Turn 历史仍是执行事实源。新输入的工作记录先于投递落盘；重启后从 mailbox 和 Turn 历史核对。选定输入、恢复输入与通知使用稳定 ID；正在投递/中断的项和显式 interrupt 保持优先。

预告另有稳定 ID，schema v2 `expectedTurnId` 限定当前 Turn，不改变原 mailbox receipt。需要 AgentHub `messages.turn-scoped-steer` 及 Session `inputCapabilities.turnScopedSteer`；第一版只有 Codex app-server 提供此保证，Pi/ACP 保持排队。每轮核对最多处理八条新预告。预告仅是尽力通知，拒绝或丢失不影响正式业务输入；绝不回退为新 Turn。

去重范围覆盖资源内所有 work：已预告的排队项不因 Turn 结束、续接或重启而重新预告。发送前保存完整预告请求和所属 Turn，结果不明时仅允许在原 Turn 用相同 ID 与请求重试；原 Turn 结束后不向新 Turn 重发。升级时保留旧 work 中成功的 `previews` 记录。预告 payload 使用 `pua.resource-message.v1` 的 `system` 身份，原发送人只出现在预告正文；正式自动 Chat 输入使用 `user` 身份。历史投影兼容旧 `pua.queue-preview.v1` 与错误标为 system 的 `user_chat_input`，不改写 canonical payload 或投递 receipt。

活动同步走快速核对（默认活动间隔 2 秒），冷核对负责发现和重启恢复。`message.assistant.completed` 标记完整文本块；后续 item 或 Turn terminal 也形成边界。Codex 与 Pi 输出完整块边界，ACP 在后续 item/terminal 时同步。Chat 消息 ID 由 Session、Turn 和块的起始事件 ID 确定；重复读取、同游标增量合并和崩溃重试不重复发布。关闭或重开后保留执行历史及 LastError，不向新 epoch 转投。

第一版是协作式安全让出，不强杀正在运行的工具，也不恢复工具进程或模型内部状态。长工具会延迟响应；恢复时 Agent 必须先核对外部操作结果。工作记录目前保留完整续接审计，后续可增加终态压缩；Chat 仍使用既有整文件持久化。
