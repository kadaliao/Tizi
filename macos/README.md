# Tizi 桌面版（macOS）

SwiftUI 菜单栏应用加主窗口。界面只负责展示和交互；连接、配置生成、DNS 接管和恢复都由 `tizi` 自己完成，桌面版不重新实现任何核心逻辑。

主窗口有两种模式，选择记在 `defaults` 的 `InterfaceMode`（`simple` / `advanced`），默认简洁模式：

- **简洁模式**（`SimpleView.swift`）：给没有网络基础的用户。首次引导粘贴机场订阅地址（也可以编辑节点文件、导入内网 `.ovpn`，或到高级模式开启 Tailscale），之后只有「连接 / 断开」。缺少 Mihomo 核心时连接会先自动安装；界面不出现 Mihomo、TUN、策略组等术语，常见问题收在「遇到问题」菜单里（重新连接、刷新微信直连、查看诊断），需要处理的状态（DNS 未恢复、其他 VPN 冲突、有新版本）以一行提示加一个按钮给出。菜单栏也只保留连接相关的项，外加「检查更新…」。
- **高级模式**（`MainView.swift` 及各 `*Page.swift`）：与 TUI 的 7 个页面一一对应。从简洁模式右下角「高级模式」、侧栏底部「切换到简洁模式」或 `⌘0` 切换；`⌘1`–`⌘7` 直接跳到对应页面。

视觉基础在 `Theme.swift`：品牌玉绿（与应用图标一致）、梯子标志 `LadderMark`（未连接灰、连接中横档自下而上点亮、已连接绿），以及按路线分的配色：外网代理蓝、直连灰、内网橙、Claude 紫、Tailscale 青、拦截红。状态页的路线分布、策略组、连接表、规则和日志都用同一套颜色，新增界面时沿用 `RoutePill`、`RouteMix`、`panel()` 这些组件。

## 架构

```
TiziDesktop.app（SwiftUI）
  │  stdin/stdout：JSON 行协议
  ▼
tizi _gui-engine          ← 外层：创建伪终端，转发信号
  └─ tizi _gui-engine --in-session   ← 新会话首进程，伪终端为控制终端
       ├─ internal/engine：进程内调用 manager / controller / customrules / core …
       └─ _run-logged-core、_recover-dns-after-exit（与 TUI 启动的完全相同）
```

- **与 TUI 共存**：两个界面读写同一份 `~/.config/tizi/`（settings.json、PID 记录、DNS 恢复点、custom-rules.txt）。可以同时打开；一边启动的 Mihomo，另一边能看到并停止。引擎在 settings.json 被 TUI 或命令行改写后自动重新读取。TUI 只在启动时读取设置，两边同时改设置时以后保存的为准。
- **引擎程序**：发布构建的 .app 在 `Contents/Helpers/tizi` 自带通用引擎；每次启动时，若 `~/.local/bin/tizi` 不存在或版本比自带的旧，就把它复制过去并执行 `init`（显式设置了 `EnginePath` 或 `TIZI_BIN` 时跳过）。这样 dmg 里的 .app 不依赖 `install.sh`，之后的更新仍走原来的逻辑。运行时按顺序查找 `defaults` 中的 `EnginePath`、环境变量 `TIZI_BIN`、`~/.local/bin/tizi`、`/opt/homebrew/bin`、`/usr/local/bin`。
- **分发与更新**：Release 附带 `Tizi-<版本>.dmg`（同一个 .app 加 Applications 快捷方式和首次打开说明，可以直接下载安装）和 `TiziDesktop-macos.zip`（通用二进制，ad-hoc 签名），`install.sh` 在 macOS 上把它装到 `~/Applications`；Homebrew 的 cask `tizi-desktop` 把同一个 zip 装到 `/Applications`，装完去掉隔离属性。`appupdate.Install` 更新命令行时，会把 `~/Applications` 或 `/Applications` 里已安装的桌面版一起更新：下载、校验 SHA-256、在同目录暂存后整包替换，失败时保留原桌面版，只给出提示。从桌面版里更新时，被替换的就是正在运行的那一份，它会等自己退出后重新打开，并在 Mihomo 运行时用新版本重载；从仓库 `bin/` 运行的开发构建不会被覆盖，更新后提示重启引擎。在终端版或命令行里更新时，运行中的桌面版会从快照轮询中发现自己的 bundle 版本（`TiziBuildVersion`）变了并重新打开自己；只有引擎程序的修改时间变了就重启引擎。两者都等进行中的操作结束，VPN 保持运行。引擎启动时和之后每 6 小时检查一次新版本；Go 在 macOS 上的计时器睡眠时不走，这 6 小时只算醒着的时间，所以桌面版还会在 Mac 唤醒 20 秒后（失败再试两次）和打开菜单栏菜单时检查。这两种自动检查给方法 `checkUpdate` 带上 `maxAge`（秒），按墙上时间算，这段时间内成功查过就沿用结果。「检查更新…」不带 `maxAge`，总是重新查询并弹窗给出结果。同时发起的检查共用一次查询，快照的 `update.checkedAt` 是上次成功查询的时间。
- **Claude 访问监测**：引擎检测 Claude 的线路（`internal/claudeguard`），平时每 30 秒一次；快照发现 Mihomo 启停、或经 Controller 的 `/logs` 实时日志看到 Claude 的失败和直连时立即检测。结论放在快照的 `claude` 字段；`level` 为 `alert` 时桌面版弹出置顶的大窗口（`ClaudeAlert.swift`），同一个 `key` 只弹一次，恢复正常后重新计。引擎运行期间维持配置目录下的 `claude-guard-ui` 心跳文件，后台的 `_run-logged-core` 看到它就不再弹 macOS 系统提醒，引擎退出时删除它。方法 `claudeCheck` 立即重新检测，`claudeProxyFile` 返回 `claude-proxy.yaml` 的路径（不存在时先写入说明），`proxiesFile` 同样返回节点文件 `proxies.yaml` 的路径，`setClaudeBlock` 安装或卸载直连保护（pf 防火墙，需要管理员授权，见 `internal/claudeblock`）。住宅节点地址在快照里隐去了密码；`settings.claudeNode` 给出地址的各项（协议、主机、端口、用户名，密码只有 `hasPassword`），界面据此预填表单。`updateSettings` 的 `claudeNode` 参数按项保存，不传 `password` 时沿用已保存的密码。粘贴整段地址时界面调用 `parseClaudeAddress`，由引擎识别各种写法（`internal/claude/address.go`）。
- **协议**：见 `internal/engine/protocol.go`。请求 `{"id","method","params"}`，响应 `{"id","result"|"error"}`，事件 `{"event":"snapshot"|"install","data"}`。快照每 2 秒推送一次，只包含 remote 列表，不包含证书、私钥、口令、订阅地址、Tailscale Auth Key 或 Controller 密钥（订阅地址和 Auth Key 只给出脱敏后的样子）。局域网代理认证只给出用户名和是否开启（`proxyAuth`），密码用方法 `proxyCredentials` 按需读取；`updateSettings` 的 `proxyAuth` 为 true 时没有密码会自动生成，`proxyPassword` 传空字符串表示重新生成。字段不兼容变化时递增 `ProtocolVersion`，并同步修改 `Models.swift` 的 `supportedProtocol`。
- **管理员授权**：核心代码依赖 sudo 的凭据缓存（先授权一次，之后多处 `sudo -n`）。sudo 默认按「终端 + 会话」缓存，而 GUI 进程没有终端，因此引擎先把自己放进一个带伪终端的新会话（`internal/engine/session_darwin.go`），再用 `sudo -A` 调用应用内的 `TiziAskPass` 弹出密码框。密码只经过密码框进程和 sudo，不经过界面或引擎；授权后行为与在 Terminal 里运行 TUI 相同。
- **退出**：与 TUI 一致，VPN 运行时询问「停止并退出」或「保持运行退出」；有操作进行中时先等它完成。界面或引擎意外退出不会停止 Mihomo；界面消失后，引擎会关掉还没处理的密码框，密码框本身也会在 2 分钟无人处理或 sudo 退出后自动取消。

## 构建与运行

需要 Xcode（Swift 5.9+），最低运行系统 macOS 13。`make release` 同样必须在装有 Xcode 的 Mac 上执行，以便打包桌面版。

```sh
make install        # 安装带 _gui-engine 的 tizi 到 ~/.local/bin
make install-gui    # 构建通用二进制 .app（arm64 + x86_64）并安装到 ~/Applications
open ~/Applications/TiziDesktop.app
```

只构建不安装：`make gui`，产物是 `bin/TiziDesktop.app`（ad-hoc 签名）。本机调试可以只编当前架构：`ARCHS=arm64 make gui`。

开发时直接运行调试版，并指定引擎：

```sh
go build -o bin/tizi-dev ./cmd/tizi
cd macos && swift build
TIZI_BIN=$PWD/../bin/tizi-dev .build/debug/TiziDesktop
```

调试版支持把每个页面渲染成 PNG（不需要录屏权限，正式版不包含这段代码）。页面名 `simple` 是简洁模式，其余是高级模式的页面；侧栏的毛玻璃背景在截图里会显示为空白或黑边，属于截图方式的限制。配合 `XDG_CONFIG_HOME`、`XDG_DATA_HOME` 和 `HOME` 指向临时目录，可以在不影响本机 VPN 的情况下渲染首次设置等状态：

```sh
TIZI_SNAPSHOT_DIR=/tmp/shots TIZI_SNAPSHOT_PAGES=simple,status,connections \
TIZI_SNAPSHOT_QUIT=1 TIZI_BIN=... .build/debug/TiziDesktop
```

另外三个变量方便检查长页面和两种外观：`TIZI_SNAPSHOT_APPEARANCE=light|dark` 固定外观，`TIZI_SNAPSHOT_HEIGHT=<点数>` 拉高窗口（受屏幕高度限制），`TIZI_SNAPSHOT_SCROLL=0…1` 把页面里最长的滚动区域滚到相应位置。

也可以不启动界面、直接和引擎对话，排查协议问题。`--askpass` 必填；只读调试时传 `/usr/bin/false`，需要授权的操作会直接以授权失败返回：

```sh
printf '%s\n' '{"id":1,"method":"hello"}' '{"id":2,"method":"logs","params":{"limit":3}}' |
  bin/tizi-dev _gui-engine --askpass /usr/bin/false
```

## 尚未覆盖

- TraceScope 看板没有入口，仍通过 `tizi tracescope` 启动。
- 应用只做了 ad-hoc 签名，没有公证；从浏览器下载的副本会被 Gatekeeper 拦截，本地构建、`curl` 下载或 Homebrew 安装的不受影响。
