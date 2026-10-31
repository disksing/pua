# PUA CLI adapter

`internal/pua` owns the `pua` command surface: argument parsing, current-directory selection, error wording, and stdout formatting.

Workspace discovery and resource selector normalization are exposed by `internal/app`; all resource creation, reads, archive operations, Resource History access, repository binding, Workspace snapshots, initialization, and migration execute through an `app.Workspace`. The CLI package must not add a second filesystem model, resource schema, migration path, or business mutation fallback.

The only process-global behavior kept here is intentional CLI adaptation, such as reading the current working directory or stdin and writing command output. `internal/app` remains explicitly rooted and returns typed values without user-facing output.

创建命令直接调用 `internal/app`，不保存创建者元数据。资源 generation 注入 `PUA_WORKSPACE_ROOT`、`PUA_WORKSPACE_INSTANCE_ID` 和 `PUA_RESOURCE_ID`，用于校验 Agent 消息的来源。创建资源不会联系 PUA Server、发送初始消息或创建 generation；Agent 使用 `pua message send` 单独发送首条输入，并携带稳定的 Workspace instance ID。Agent 消息是单向的，需要回复时须显式发送新消息。
