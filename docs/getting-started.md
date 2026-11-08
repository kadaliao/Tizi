# 安装与使用

## 准备

- 一台 Mac（macOS 13 或更新），或 Ubuntu 22.04 / 24.04 x86_64（桌面版或服务器版）
- 至少一种线路：机场订阅地址、自己的节点、内网 `.ovpn` 文件或 Tailscale 账号

> 订阅地址、节点密码、`.ovpn` 和 Tailscale Auth Key 都是你的个人凭据，不要发给别人，也不要上传到任何地方。

## 安装

打开「终端」（Mac 上按 `Command + 空格`，输入“终端”），粘贴下面这行命令，按回车：

```sh
curl -fsSL https://raw.githubusercontent.com/kadaliao/Tizi/main/install.sh | sh
```

看到“安装完成”就好了。Mac 上会同时装好桌面版。

也可以从 [Releases](https://github.com/kadaliao/Tizi/releases) 下载 `Tizi-vX.Y.Z.dmg`，打开后把「TiziDesktop」拖进「Applications」。应用没有 Apple 公证，首次打开要按磁盘映像里的说明，在「系统设置 → 隐私与安全性」点一次「仍要打开」。

首次连接会自动下载 Mihomo 核心，需要能访问 github.com。

### 用 Homebrew 安装

已经装了 [Homebrew](https://brew.sh) 的话：

```sh
brew install --cask kadaliao/tap/tizi-desktop   # Mac 桌面版
brew install kadaliao/tap/tizi                  # 只装终端版和命令行（Mac 或 Ubuntu x86_64）
```

- 桌面版装在 `/Applications/TiziDesktop.app`，第一次打开时会把自带的终端版装到 `~/.local/bin/tizi`，和用安装命令装的一样。安装时已经去掉 macOS 的隔离标记，不用去「隐私与安全性」点「仍要打开」。以前用安装命令装过桌面版的，先删掉 `~/Applications/TiziDesktop.app`，免得留两份。
- 只装终端版时，`tizi` 已经在 `PATH` 里，直接运行即可。
- 卸载：先断开连接，再运行 `brew uninstall --cask tizi-desktop` 或 `brew uninstall tizi`。设置（`~/.config/tizi`）和桌面版装的 `~/.local/bin/tizi` 会留着；卸载桌面版时加 `--zap`，会把它们连同 Mihomo 核心一起删掉。

## Mac 桌面版

1. 运行 `open ~/Applications/TiziDesktop.app`，以后也可以在访达的「个人文件夹 → 应用程序」里双击打开。用 dmg 或 Homebrew 装的在系统的「应用程序」文件夹里（`open /Applications/TiziDesktop.app`）。
2. 第一次打开时粘贴机场订阅地址。没有订阅时，可以点「编辑节点文件…」写自己的节点，或导入内网 `.ovpn`；Tailscale 在「高级模式 → 配置」里开启。
3. 点「连接」，弹出密码框时输入这台 Mac 的登录密码。看到「已连接」就可以用了。

简洁模式只有一个「连接」按钮；点右下角「高级模式」可以看实时连接、给节点测速和切换线路、改规则、看日志和诊断。菜单栏图标也可以随时连接或断开。关掉窗口不会断开；退出程序时会问你「停止并退出」还是「保持运行退出」。

## 终端版

适合 Ubuntu、SSH 登录的服务器，或习惯命令行的 Mac 用户。运行 `~/.local/bin/tizi`（用 Homebrew 装的直接运行 `tizi`），用方向键移动，`Enter` 确认：

1. 按 `2` 进入「配置」页，在「订阅地址」上按 `Enter` 粘贴订阅地址（直接粘贴也可以）。导入 `.ovpn` 时把文件拖进终端，或选中「内网 OVPN」后按 `s` 扫描。
2. 按 `1` 回到「状态」页，第一次先选「安装 Mihomo 核心」，再选「启动 Mihomo」。
3. 按提示输入登录密码（输入时屏幕不显示字符），看到「运行中」就连上了。

终端版有和桌面版高级模式一样的 7 个页面：状态、配置、策略、规则、连接、日志、诊断，功能也一致。「状态」页可以重载 Mihomo（改了设置或规则后让它生效）、刷新微信直连、检测 Claude 线路；「策略」页按 `t` 测速，节点后面显示延迟；上次没有正常断开时，还会出现「恢复系统 DNS」。在「规则」页保存规则后，选「重载 Mihomo 使规则生效」。按 `?` 查看全部按键。退出时选「停止并退出」会断开连接，选「保持运行退出」让它继续在后台运行，之后用 `tizi stop` 断开。

桌面版和终端版共用设置和运行状态，可以同时打开：一边连接，另一边能看到并断开。

## 更新

有新版本时桌面版会提示，点「更新」即可；也可以点菜单栏图标里的「检查更新…」，或运行 `tizi update`。桌面版和终端版会一起更新。

用 Homebrew 装的终端版（`brew install kadaliao/tap/tizi`）用 `brew upgrade tizi` 更新，`tizi update` 会提示你这样做。Homebrew 装的桌面版照常在应用里更新；`brew upgrade` 默认跳过它，加 `--greedy` 才会用 Homebrew 更新。
