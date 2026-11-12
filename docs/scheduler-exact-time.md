# 精确时间调度

自然语言 `condition` 仍由 Scheduler Agent 判断。新增的 `exactTime` 由 PUA Server 直接生成目标资源的 mailbox 消息；仅有精确时间项时不会启动 Scheduler Agent。精确时间项的 `condition` 可留空，或作为说明文字，不作为附加触发条件。

## 创建与编辑

```sh
pua scheduler add --description="一次提醒" --target=project1.task1 \
  --at=2026-11-12T09:00 --timezone=Asia/Shanghai
pua scheduler add --description="每日检查" --target=project1.task1 \
  --daily=09:00 --timezone=Asia/Shanghai
pua scheduler update --id=schedule-... --daily=10:00
pua scheduler update --id=schedule-... --condition=
pua scheduler update --id=schedule-... --expires=
pua scheduler update --id=schedule-... --condition="条件满足时" --no-exact-time
```

`--at` 和 `--daily` 互斥。`--at`、`--expires` 接受带 offset 的 RFC3339 或本地 `YYYY-MM-DDTHH:MM[:SS]`。本地输入按照 `--timezone` 解释；带 offset 的输入定义绝对时刻，不受时区字段二次偏移。

时区使用 IANA 名称，例如 `Asia/Shanghai`，建议显式指定。CLI 未指定时保存服务主机的本地时区；不能解析名称的平台会保留 `Local`，该值仍随主机时区变化。Web 表单默认使用浏览器时区并显式保存。只要显式使用 IANA 时区，操作系统默认时区改变不会改变调度。

重复型第一版仅支持每日一个时刻。每日调度不补发创建或改时之前的历史；Server 停机后补发已生效范围内最近一次漏掉的时刻，更早的漏掉时刻合并跳过。`expiresAt` 为包含边界，重启时仍可补发截止时间之前的最后一次漏发。

夏令时跳过的本地时刻不触发，当天不平移到其他时间；本地时刻重复出现时仅选择较早的一次。一次性本地输入遇到不存在的时刻报错；重复时刻可用显式 offset 选择绝对时间。

## 持久化与恢复

`scheduler/scheduler.json` 中旧调度项保持原样；新增字段示例：

```json
{
  "exactTime": {
    "timeOfDay": "09:00",
    "timeZone": "Asia/Shanghai",
    "startedAt": "2026-11-11T13:00:00Z",
    "lastFiredAt": "2026-11-12T01:00:00Z"
  }
}
```

`startedAt` 定义当前时间规则的生效起点及消息版本。`lastFiredAt` 表示已被目标持久 mailbox 接受的 occurrence，不表示目标业务执行完成。触发时先原子写入 `pending`（应触发时刻、实际记录时刻、描述、目标、语言），再幂等接受目标消息，最后推进游标。一次性项成功接受后删除。文件写入使用现有临时文件、fsync、rename 机制。

`pending`、`startedAt`、`lastFiredAt` 均由系统维护，创建和更新接口拒绝客户端写入这些字段。重试复用冻结的消息 ID 与正文，因此在接受消息后、游标保存前崩溃，跨日恢复也不会重建另一个补发消息。目标消息终态和业务执行去重沿用普通 mailbox 协议，目标 Agent 仍应按 schedule ID 和 occurrence 避免重复业务操作。

仅修改描述或再次提交相同时间配置保留游标及待投递消息。修改时间规则或 target 会重新生效并清除旧游标；移除项会取消尚未接受的触发，但已经进入目标 mailbox 的消息不能撤回。

## 接口与运行

现有 scheduler POST/PUT 接口接受可选 `exactTime` 对象，字段为 `at`、`timeOfDay`、`timeZone`、`expiresAt`。PUT 省略该字段保留原配置，传 `{}` 移除时间类型；移除后必须有自然语言 condition。GET、CLI show/list 返回生效配置和游标。CLI 支持只改 timezone 或 expires，API 的 exactTime 按整体替换。

Server 在现有 reconcile 定时器中取精确调度与自然语言 tick 的最早 deadline，并用现有 30 秒 fallback 检查遗漏事件及 CLI 文件变更；不另建常驻进程、系统 cron、逐项 goroutine 或 LLM 轮询。到期接受消息后立即请求现有 mailbox 投递协调。

“30 秒”是服务正常运行时的检查兜底粒度，不保证目标 Agent 在 30 秒内开始或完成工作。服务停机、主机休眠、目标忙碌及投递异常可延迟执行；源端失败保留 pending 供重试，目标投递异常按 mailbox 现有策略处理。

兼容旧 schemaVersion 1 的自然语言项，不自动转换现有调度或刷新生产指引。旧版本使用严格 JSON 读取，不能直接读取含新字段的文件；回退旧二进制前应先通过新版本移除精确时间项或恢复经过核对的配置备份。
