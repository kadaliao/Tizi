# Tizi

填好机场订阅，点「连接」：国内网站直连，海外网站走代理，不用自己写规则。基于 [Mihomo](https://github.com/MetaCubeX/mihomo)，支持 macOS 和 Ubuntu。

https://github.com/user-attachments/assets/5ab1f5ab-4146-4431-ba94-cc4b98bb2c7e

## 功能

- **订阅即用**：粘贴机场订阅，或写自己的节点；节点按延迟自动选择，国内外自动分流
- **内网（可选）**：导入 OpenVPN `.ovpn`，只有内网流量走 VPN，其余照常分流
- **Tailscale（可选）**：不用装客户端，用 Auth Key 加入 tailnet，访问自己的设备
- **Claude 线路保护**：Claude 固定走代理、不会直连，线路异常时立即提醒
- **三种用法**：macOS 桌面版（一个「连接」按钮）、终端版（适合服务器）、命令行

## 安装

打开「终端」，粘贴这行命令并回车：

```sh
curl -fsSL https://raw.githubusercontent.com/kadaliao/Tizi/main/install.sh | sh
```

Mac 上会同时装好桌面版。也可以从 [Releases](https://github.com/kadaliao/Tizi/releases) 下载 `.dmg` 直接安装。当前版本是 `v1.2.0`。

## 快速开始

1. 打开 Tizi（Mac：`open ~/Applications/TiziDesktop.app`；终端：`~/.local/bin/tizi`）。
2. 粘贴机场订阅地址。没有订阅时，也可以写节点文件、导入 `.ovpn` 或开启 Tailscale。
3. 点「连接」，输入这台电脑的登录密码。看到「已连接」就好了。

> 订阅地址、节点密码、`.ovpn` 和 Tailscale Auth Key 都是个人凭据，不要发给别人。

## 文档

- [安装与使用](docs/getting-started.md)：桌面版、终端版的详细步骤
- [配置线路](docs/configuration.md)：订阅、节点文件、内网、Tailscale
- [Claude 线路保护](docs/claude.md)：住宅节点、访问监测、直连保护
- [命令行](docs/cli.md) · [配置文件与自定义规则](docs/files-and-rules.md) · [Ubuntu 服务器](docs/linux-server.md)
- [常见问题](docs/faq.md) · [工作原理](docs/how-it-works.md) · [开发与发布](docs/development.md)

## 参与贡献

欢迎提 Issue 和 PR，开始前请看 [CONTRIBUTING.md](CONTRIBUTING.md)。发现安全问题请按 [SECURITY.md](SECURITY.md) 私下报告。

## 许可证

[MIT](LICENSE)。Mihomo 核心在首次连接时从其官方 Release 下载，不随 Tizi 分发。
