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

可以提前选择下一个正式输入，但实际提交必须等待提示所归属的 Turn 终态。独立提示轮不会接收普通正式输入作为 steer；原消息随后以自己的 opener/work/受众独立投递。明确的 interrupt 继续使用原中断流程。

提示独立成轮时没有合法当前正式 work。Agent 应结束该轮，等待正式输入，不能沿用原 work ID 调用 yield、notify、finish 或 cancel-resume。提示轮的完成不创建业务 work 或隐式续接，也不据此完成业务 Task。

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

Chat open 与本轮可见性独立。内部正式工作需要用户获知时，通过 `pua turn notify` 请求独立可见通知，接受后结束本轮，需要恢复时附 checkpoint。Chat 关闭时向明确稳定上层发送必要业务摘要，委派回报义务保留。快照不恢复已撤销资格；关闭重开不补发旧 epoch 输出，也不跨资源转发失效用户回复。

开关采样不持有全局 Chat 锁进行 Hub 查询，使用短查询预算；暂停或归档后不发送尚未被 Hub 接受的新提示。提示及状态恢复沿资源 controller 和独立 Turn 输出核对循环运行，不要求生产重启或部署才能执行测试。

## 验证

`internal/serve/turn_advisory_test.go` 使用临时 Workspace 与回环 fake Hub，覆盖 Pi 活动/边界投递、未知响应恢复、提示轮 History、正式输入串行、无 work 控制、Codex/Pi 两种正式受众开关、快速变化合并、空闲不唤醒和业务状态保持。既有 Turn、Chat epoch、续接、输出与指引刷新测试继续适用。
