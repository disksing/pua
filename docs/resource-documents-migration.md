# 资源文档维护与旧数据回退

Workspace、Project、Task 的创建输入写入 `spec.md`，Agent 的上下文和对话事项写入 `memory.md`。Scheduler 使用 `scheduler.md`。`pua migrate` 刷新托管指引和补齐缺失的当前文档，`--language` 用于切换语言。已有正文及其中的迁移来源块原样保留。

## 已迁移的数据

`pua doctor` 用 `markdown_file_missing` 提示文档缺失，用 `markdown_file_invalid` 提示文档路径不合法。若报告 `legacy_document_pending`，先在副本检查旧文档、暂存文件和现有 Memory 来源块，再按需手动合并。旧正文导入流程可从 Git 历史查阅。

Web 预览和原文下载将旧 `workspace.md`、`project.md` 和 `task.md` 路径只读映射至当前 Memory。命令行应直接读取 `memory.md`。

## 授权与回退

普通文档保存入口接受 Memory 及允许的产物路径。Spec 的单独保存入口验证同源请求、所选 Workspace 用户、归属和基准哈希。Agent 按托管指引核对可追溯的用户指示或批准。所选用户名源于本地界面，文件系统仍沿用同 UID 权限。应用写锁只协调 PUA 支持的写入路径。

若需要回退，暂停新写入并单独保存迁移后的 Spec、Memory、暂存稿及控制数据。恢复配套的旧程序和迁移前备份。迁移后新增的规格变更、事项和上下文先在副本中逐项合并至恢复的旧说明，再验证历史和事项。使用旧程序直接打开已迁移目录可能重新建立旧路径。在隔离副本完成验证后再按部署流程切换。
