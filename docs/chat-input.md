# Chat 输入独立转交

HTTP 和 Telegram 用户输入先保存到原有 Chat 文件及其 outbox；落盘成功后立即请求对应资源的独立转交。Server 为这条通道维护单独的唤醒循环，不需要等待会话列表、冷审计、快速会话轮询或其他资源的 controller 完成。

## 转交与执行

- 转交阶段仅读取本地资源、用户注册及 Chat 状态，调用既有 mailbox 接受接口；没有 AgentHub 网络操作，也不进入目标资源的 controller。
- mailbox 接受成功后，即使 Chat 的 accepted 标记写回失败，也请求唤醒目标资源；稳定 mailbox ID 和原有 causation 保证重放幂等。
- mailbox 唤醒进入原有资源 controller，调用原有 mailbox 协调逻辑。controller 按 Workspace 实例和资源隔离，其他 Workspace 或资源的慢作业不会占用该资源的 controller。
- 每个 Workspace 路径、资源、阶段最多有一个排队或正在执行的作业。期间的新请求合并成后续核对；失败默认在完成后 2 秒重试，新请求可提前唤醒。
- mailbox 协调沿用固定 Turn 受众、FIFO、预告、yield/恢复、归档和不确定投递规则。输入进入 mailbox 不意味着 provider 已接受，更不意味着当前 Turn 会被抢占。

Server 启动时独立扫描持久 Chat outbox 和对应资源的待处理 mailbox。它覆盖“outbox 已保存”“mailbox 已接受但 accepted 尚未写回”及“accepted 已写回但尚未唤醒”的崩溃窗口；单个损坏记录会报告错误并继续其余记录。旧版 Chat 迁移和周期性全量协调仍保留兜底。

关闭、重新开启及资源归档仍检查原有 epoch 与有效性；重开不会转投旧周期输入。已经接受的执行工作继续按原有规则结算。旧版数据结构、稳定消息 ID 和 causation 均未改变。

关停时取消独立循环及排队作业，已执行的 mailbox 请求受生命周期 context 和 30 秒执行预算约束。后台工作跟踪等待 controller 真正完成；该预算不含 controller 排队时间。

## 诊断记录

日志仅包含必要标识、时间和错误码，不包含正文、Chat 凭据、请求 query 或上游错误正文：

| 日志 | 用途 |
| --- | --- |
| `Chat input accepted` | 对齐 Workspace、目标资源、outbox 输入 ID、稳定 mailbox ID、原输入保存时间、首次 mailbox 接受时间及本次本地接受耗时 |
| `Chat input reconcile` | 分别记录 transfer/mailbox 的请求入队、实际执行开始、排队毫秒、执行毫秒及错误码；执行成功为 `none` |
| `AgentHub request` | 记录失败、至少 1 秒的请求，以及带输入 ID 的投递；包含 method、无 query 的 path、消息 ID、开始时间、总耗时、HTTP 状态及错误码 |

重放时 `accepted_at` 保留首次接受时间；`accept_ms` 是本次幂等接受调用耗时。请求成功仅代表 Hub HTTP 接口返回成功，provider 接受仍需关联 Hub 的输入与 provider 事件。首次新建会话时也可能发生既有的会话列表查询，这属于目标执行路径，不属于 outbox 恢复或本地转交。

排查时以稳定消息 ID 对齐 Chat 保存、mailbox 接受、controller 等待、Hub 请求和 provider 接受；结合 [输出同步](chat-output.md) 对齐回复完成、观察与发布。固定受众下前序 Turn 占用和本资源 controller 等待需分别报告。独立通道不提供生产端到端时限；本地磁盘、短时全局 Chat 存储锁、同资源既有工作及 Hub/provider 响应仍可增加等待。
