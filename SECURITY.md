# 安全策略

## 支持的版本

只为最新的发布版本修复安全问题。请先用 `tizi update` 更新到最新版，确认问题仍然存在。

## 报告漏洞

请不要公开提 Issue。在仓库页面点 **Security → Report a vulnerability**（[GitHub Security Advisories](https://github.com/kadaliao/Tizi/security/advisories/new)）私下报告，写清楚：

- 受影响的版本和系统
- 问题描述和影响
- 复现步骤或概念验证

我们会尽快确认，并在修复发布后公开致谢（如果你愿意）。

## 需要特别注意的地方

- `~/.config/tizi/settings.json` 保存订阅地址和 Tailscale Auth Key；`config.yaml` 保存节点密码、OpenVPN 证书和私钥；`tailscale/` 保存 tailnet 登录状态。`settings.json` 和 `config.yaml` 的权限是 `0600`，配置目录是 `0700`。
- 连接时 Tizi 会用管理员权限启动 Mihomo（TUN）、修改系统 DNS 和 IPv6 设置；macOS 上的直连保护会安装 pf 规则和一个 LaunchDaemon。以 root 身份运行的部分会读取用户目录里的状态文件，这条路径上的问题都按安全问题处理。
- Mihomo Controller 只监听 `127.0.0.1`，使用随机密钥。开启「局域网代理」时请务必设置用户名和密码。
- Mihomo 核心和 GeoIP 数据从 GitHub 下载，并校验 Release 提供的 SHA-256。
