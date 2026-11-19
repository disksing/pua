# Turn 预告与 Chat 状态提醒

PUA 的正式输入与提示相互独立。固定受众 Turn 运行期间，新业务输入继续排队，预告不消费原消息、不改变其送达记录或回复受众。预告缺失不能单独证明业务消息丢失。

## Provider 投递方式

- Codex 在 AgentHub 与会话均声明 scoped steer 时，沿既有 `expectedTurnId` 预告路径投递。
- Pi 支持普通 steer，使用独立稳定消息 ID 和 `prompt + streamingBehavior=steer`，不声明 scoped 能力，也不携带 `expectedTurnId`。活动期间尽量插入当前轮；恰逢终态时允许创建独立内部提示轮。
- 无 steer 能力的 Provider 保留业务排队，下一正式 opener 或状态查询提供最新快照。

Pi 原生 `steer` 在空闲时只入队而不启动运行，不能代替本方案用于尽快提醒。读取状态后再发送也不构成原子投递保证。

## 独立提示轮与正式输入

`turn-work` 文件中的 `advisories` 保存提示 ID、原消息 ID、仅供审计的 `originWorkId`、generation/session/canonical Turn、投递状态、终态、错误和 `historyRef`。它们不属于业务 mailbox，不参与 waiting、read 或 handled/ack。

服务端先落盘保留稳定输入，再发送给 Hub。响应不明时从 canonical `message.input` 恢复归属；无法确认时保留 `unknown` 并阻止后续正式输入。重试使用原 ID 和原内容，不重新广播新 ID；已确认的终态提示不重发。模型侧仍遵守 Hub 的至少一次交付语义。

语义事件投影保留 scoped steer 的 `expectedTurnId`。兼容旧 Hub 时，仅在该字段从语义事件中缺失、且持久提示携带非空期望 Turn 的情况下，读取同一事件游标的 event-detail 原始输入；核对事件 ID、类型、Session、Turn、稳定消息 ID 及原始输入内容后恢复归属，不把缺失值当作相等。显式空值、不同期望 Turn、正文或 payload 冲突、原始事件无法读取时继续阻止正式投递。scoped 提示还必须归属其期望的原 Turn。

已持久化的 unknown 提示沿正常协调恢复，不需要删除提示或改写回执。确认其原 Turn 已终态后，提示收敛为 completed/failed，后续正式消息继续按 FIFO 各自投递；恢复过程不重发已接受的提示。此行为需运行包含修复的 PUA，或在原始事件完整时运行包含投影修复的 Hub，源码交付本身不会改变运行中的实例。

可以提前选择下一个正式输入，但实际提交必须等待提示所归属的 Turn 终态。独立提示轮不会接收普通正式输入作为 steer；原消息随后以自己的 opener/work/受众独立投递。明确的 interrupt 继续使用原中断流程。

提示独立成轮时没有可控制的正式 work。Agent 应结束该轮，等待正式输入。只有在正式 Turn 里才能切换、结束或取消恢复。提示轮结束后业务 Task 继续等待正式投递。

## 提示语言与指引兼容

正式开轮、队列预告和恢复提示使用接收 Workspace 的语言（en / zh-CN）。重试已发起的投递时沿用首次的文本格式。

Agent 发送的协作消息统一进入队列。正式开轮时，空队列不展示提示。队列预告默认请接收方继续当前工作。同一发送资源和 Workspace 实例在采样时已排队的普通结果，最多八条合并提醒一次，每条仍独立投递或确认。用户消息保留完整原文，单独预告。

内部轮可提前处理结果时，预告给出已选 ID 的读取命令。CLI 在完整读取、获得收据后提示如何确认，由 Agent 在工作完成后执行。允许切换的正式根工作收到用户预告时附带绑定 work 和消息 ID 的 `turn switch` 命令。CLI 也允许省略 `--work`，通过 `currentControl` 选择正式工作，提交时由服务端再次校验。Pi 在轮次边界可能收到独立提示轮，应结束提示轮等待正式输入。队列采样细节保存在结构化状态和收据中。

## Chat 开关与状态查询

Chat 开关保存递增 revision 与最新状态；开启新周期仍递增 epoch。重复 open/close 不生成新提醒。只有核实存在活动正式轮才允许边界提示；内部和用户可见正式轮均覆盖。临时无法查询 Hub 时保留待核实提示，恢复后必须确认原正式轮仍活动，才能发送。

已结束但尚未结算的 work 不算活动轮。空闲开关不启动 Agent；下轮非用户 opener 和 `pua turn status` 展示最新快照。尚未发送的快速多次开关合并到最新 revision。只有提示轮活动时不连续制造提示轮。

`pua turn status` 增加：

| 字段 | 含义 |
| --- | --- |
| `chat` | open、epoch、revision、observedAt 与稳定 parent ID |
| `currentControl` | 当前 generation/session/Turn、匹配 work ID、kind、可用控制与拒绝原因 |
| `advisories` | 提示归属、状态、终态及可用于 `pua history turn show --ref=...` 的 `historyRef` |

`currentControl.workId` 只有在 generation、Session、活动 canonical Turn 与未结正式 work 真实匹配时提供。未知归属或提示轮不授予 work 控制；服务端再次校验。status 不返回新的提示正文；PUA History 可以查看提示轮的原始执行记录和提示元数据。

Chat open 与本轮可见性独立。要先回复排队的用户消息，用 `pua turn switch --message=<id>`。内部正式工作要主动联系用户，用 `pua turn switch --to=chat --context="要沟通的事"`。接受后结束本轮，在新的可见 Turn 回复用户。回复轮成功结束后自动恢复原受众，需要记录未决操作时可附 `--checkpoint`。Chat 关闭时向明确稳定上层发送必要业务摘要，委派回报义务保留。

开关采样不持有全局 Chat 锁进行 Hub 查询，使用短查询预算；暂停或归档后不发送尚未被 Hub 接受的新提示。提示及状态恢复沿资源 controller 和独立 Turn 输出核对循环运行，不要求生产重启或部署才能执行测试。

## 验证

`internal/serve/turn_advisory_test.go` 使用临时 Workspace 与回环 fake Hub，覆盖 Pi 活动/边界投递、未知响应恢复、提示轮 History、正式输入串行、无 work 控制、Codex/Pi 两种正式受众开关、快速变化合并、空闲不唤醒和业务状态保持。既有 Turn、Chat epoch、续接、输出与指引刷新测试继续适用。

`agenthub/internal/api/server_advisory_input_test.go` 从真实 Store 经 HTTP 分页与 event-detail 验证 scoped/普通输入投影。`internal/serve/turn_advisory_canonical_test.go` 覆盖旧投影的严格原始事件核对、身份/内容冲突、原始事件暂不可用，以及 scoped unknown 在 manager 重启、活动/已终态 Turn 下的收敛、FIFO 和不重发；真实冲突在重启后仍保留栅栏。
