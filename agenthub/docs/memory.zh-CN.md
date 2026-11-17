# 事件历史的内存与测量

## 历史读取和写入

`events.jsonl` 仍是事实来源。完整事件缓存按估算大小采用单会话 4 MiB、整个 Store 32 MiB 的保留预算；超过预算或会话停止、归档后，只保留追加和流式 delta 合并需要的最后一条事件。预算包含事件对象、字符串及 payload 的估算，不是 Go 堆或进程 RSS 上限。单条尾事件、索引、请求输出和并发临时分配还会占用内存。

大历史首次使用时，逐行校验事件连续性，并建立每 128 条事件一个字节位置的稀疏索引。后续正向、反向分页和 SSE 补读从临近位置读取，内存随页大小分配。追加持久化后更新索引；delta 改写尾行时保持起点、调整结束位置，保留原事件 ID。索引记录的文件身份、长度或修改时间变化时，重新扫描；不一致的日志游标会返回错误。

索引也保存各 Turn 的原始字节范围，以及输入、投递和终态事件的位置。已有日志通过首次扫描补齐这些位置，新事件在同一会话锁内维护。构建一个 Turn 时读取对应范围，并过滤夹杂的其他 Turn；恢复消息时只读取相关事实。全量 Turn 投影重建和不可信会话快照恢复采用逐行归约，避免先保留全部原始 Event。长 Turn 的原始事件查询继续分页，现有 Turn 展示内容和 canonical input 返回值保持完整。

当前偏移索引保存在进程内，重启后按需重建，未向 `turns.jsonl` 增加持久化 offset 字段。它覆盖无 Turn 的事件和任意事件游标，也提供按 Turn 读取的定位。首次扫描仍需线性读取该会话的日志，并持有该会话锁；其他会话和 Store 的订阅锁不受这次扫描占用。持久化 Turn offset 可以减少重启后的再次扫描，属于后续优化。

## 诊断计数

`GET /v1/status` 的 `sessionStore.diagnostics` 包含：

- `cachedEventPayloadBytes`：当前保留 Event payload 的字节数；释放缓存时减少。
- `cachedEventBytes`：保留 Event 的对象、字符串和 payload 估算。
- `eventHistorySessionBudget`、`eventHistoryStoreBudget`：完整历史缓存保留预算。
- `eventIndexBuilds`、`eventIndexBuildNanos`：成功构建的索引数量与构建耗时。
- `historyLoads`、`historyLoadNanos`：完整历史读取或恢复扫描的次数与耗时；普通索引分页独立统计。
- 既有 Turn 投影缓存和重建计数，用来识别展示内容构建产生的工作量。

这些数值不包含 provider 子进程。macOS 的 RSS、Go 存活堆、Go 已取得的堆地址空间和 provider RSS 是不同指标，必须分别记录。不能将某个缓存 gauge 的下降解释成整个系统相同比例的内存下降。

## 可重复测量

固定待比较提交、Go 版本、机器、负载和并发度，先构建测试二进制，再运行合成负载，避免把编译器内存计入 Hub。测试仅创建独立临时 Store，不读取生产 Session 或认证信息：

```sh
go test -c -o /tmp/agenthub-session-memory.test ./agenthub/internal/session
/tmp/agenthub-session-memory.test -test.run '^$' \
  -test.bench '^BenchmarkStoreMemoryLifecycle$' -test.benchtime=1x -test.count=3 -test.v
AGENTHUB_MEMORY_SCENARIO=single-long /tmp/agenthub-session-memory.test \
  -test.run '^$' -test.bench '^BenchmarkStoreMemoryLifecycle$' -test.benchtime=1x -test.v
```

默认负载为 8 会话，每会话 4096 条、每条 4 KiB 的合成事件；`single-long` 使用单会话 32768 条事件。阶段日志记录空闲、首次读取、128 次热分页、128 次持久追加、停止和归档。`MEM` 日志在强制 GC 并归还空闲页后记录存活堆、RSS 和 goroutine；`LATENCY` 记录实际操作时间及追加的 p50、p95、吞吐。整个 benchmark 的 `ns/op` 含 fixture 准备，不应用作单请求延迟。

将 `AGENTHUB_MEMORY_PROFILES` 指向已有的独立目录，可保存各阶段 heap profile，再使用 `go tool pprof` 分析。真实 daemon 压测应另建配置、数据目录、HOME、Workspace 和 loopback 端口，清除当前 PUA/Forge/AgentHub 环境变量，使用确定性 fake provider，并按 PID 分开记录 Hub 和子进程。测试结束后停止自己启动的进程。

## 收益边界

优化消除的是全部原始事件永久驻留，以及完整文件缓冲与解码结果共存的峰值。热历史分页需要再次读取、解码该页，其延迟可能高于完整历史已经驻留的旧实现。第一次索引扫描和历史规模成正比。

完整 Turn 展示结果、canonical input 集合、单条巨大事件和并发返回页仍可能很大；既有 Turn 投影缓存预算独立生效。这里不调整 GC 参数，不清理持久历史，不减少消息或历史内容，不改造 `turns.jsonl` 自身的完整展示内容存储，也不限制真实 provider 自己的内存。
