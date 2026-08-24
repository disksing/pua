# PUA macOS 桌面壳

桌面壳使用 Wails v3 和系统 WKWebView 显示 `pua serve` 页面。桌面壳与 PUA
后端和 AgentHub 是两个独立受管进程：关闭窗口只隐藏窗口并保留服务；使用 Cmd+Q 时，没有活动
Turn 会优雅停止两个服务，有活动 Turn 时只能选择停止并退出或取消。
外部启动的 PUA 永远不会由 App 停止。

首次运行时，随 App 提供的 `Contents/Resources/pua` 和 `agenthub` 会按 SHA-256 安装到各自的
版本目录，例如：

```text
~/Library/Application Support/PUA/backend/versions/<sha256>/pua
```

`backend/current.json` 和 `agenthub/current.json` 分别指向当前组件版本，两个 state 文件分别记录
由桌面壳启动的进程身份。App 包内组件只是全新安装的 baseline；已有 current 版本不会因为安装
较旧 App 而降级。组件通过 Service Manager 独立更新，PUA 更新只重启 PUA，AgentHub 更新才停止
两个进程。外部启动的服务不参与更新或生命周期操作。
如果默认 serve config lock 指向一个健康的现有 PUA，桌面壳只连接它，不把外部进程标记为
可管理版本。新的空配置不会把 App 的工作目录误加为 Workspace，用户可以在 Web UI 中添加真实
Workspace。默认地址固定为 `127.0.0.1:4936`，以保持浏览器 origin 和 localStorage 稳定。

同一版本还会原子安装到 `~/.pua/bin/pua`，作为用户和 Agent 共用的稳定 CLI 入口。桌面壳会在
当前用户的 `~/.zprofile`（登录 shell 为 bash 时使用 `~/.bash_profile`）维护一个带明确起止标记的
PATH 区块，并在启动受管后端时立即把 `~/.pua/bin` 前置到其 PATH。因此新 Agent Session 无需等待
用户重新登录即可找到 `pua`；已经打开的终端需要重新打开，或重新加载对应 profile。

在 macOS 上构建本机开发 App：

```bash
scripts/build-desktop
open "bin/PUA Dev.app"
```

脚本会构建带完整 Web 资源的 `pua` 与 `agenthub`，嵌入并链接校验过的真实 Sparkle framework，
再创建 `bin/PUA Dev.app` 并使用 ad-hoc 签名。开发 App 使用独立 bundle ID、Application Support
目录、CLI 目录和端口 14936/14646，不会连接或覆盖正式版数据。默认不配置 feed，也不会自动检查
正式版更新；要端到端测试开发 App 的 Sparkle 更新，可同时设置 `PUA_DEV_SPARKLE_FEED_URL` 和
`PUA_DEV_SPARKLE_PUBLIC_KEY`。正式发布由 release 脚本生成 Developer ID 签名、公证的 Universal DMG。

开发和隔离测试可使用以下环境变量：

- `PUA_DESKTOP_BACKEND`：指定 bootstrap `pua` 可执行文件；它仍会复制到版本化目录。
- `PUA_DESKTOP_AGENTHUB`：指定 bootstrap `agenthub` 可执行文件。
- `PUA_DESKTOP_AGENTHUB_HOME`：覆盖 App 启动的 AgentHub 数据目录。
- `PUA_DESKTOP_HOME`：覆盖桌面壳 Application Support 目录。
- `PUA_DESKTOP_ADDRESS`：覆盖监听地址，测试时应使用独立端口。
- `PUA_SERVE_CONFIG`：覆盖 PUA serve config，测试时应指向临时目录。
- `PUA_DESKTOP_CLI_PATH`：覆盖稳定 CLI 安装路径，仅供隔离测试。
- `PUA_DESKTOP_SHELL_PROFILE`：覆盖 PATH 托管区块写入的 shell profile，仅供隔离测试。

Service Manager 在 App 自己的本地 Asset Server 中运行，即使 PUA Server 停止也能使用。组件更新
使用 Ed25519 签名的 channel manifest、HTTPS、长度/SHA-256 和 Developer ID 多重校验；PUA 可单独
重启更新，AgentHub 更新会等待活动 Turn 清零。PUA.app 本身由 Sparkle 和独立 appcast 更新。
