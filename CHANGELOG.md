# Changelog

本文件记录 Tizi 每个版本的主要变更，格式参考 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，版本号遵循 [语义化版本](https://semver.org/lang/zh-CN/)。

## [Unreleased]

### 新增

- 文档拆分到 `docs/`，README 精简为概览；新增贡献指南、安全策略、行为准则、Issue / PR 模板和 GitHub Actions CI。

## [1.0.0] - 2026-11-07

第一个公开版本。

### 新增

- 线路：机场订阅（Clash / Mihomo 格式或 Base64 分享链接，由 Mihomo 下载并每天更新）和节点文件 `proxies.yaml`（分享链接或 Mihomo 节点格式），节点进「自动选择」和「外网代理」。
- 内网（可选）：导入 OpenVPN `.ovpn`，内网域名、网段、DNS 默认从 `.ovpn` 的 `route`、`dhcp-option` 读取，也可以自己填写；内网 DNS 经隧道查询；可以把内网里的 HTTP 出口作为「外网代理」的选项；支持 `tls-crypt`、`tls-crypt-v2`、`tls-auth`。
- Tailscale（可选）：使用 Mihomo 内置的 Tailscale 节点加入 tailnet，`100.64.0.0/10` 和 MagicDNS（`*.ts.net`）走 Tailscale；支持 Headscale 和出口节点。
- Claude：固定走没有直连选项的「Claude」线路；住宅节点可以经「前置代理」链式连接；线路检测、置顶报警和 macOS 直连保护。
- 系统：TUN 接管、DNS 接管与恢复、连接期间关闭公网 IPv6、隧道自动恢复、微信图片直连修复、诊断。
- 界面：macOS 桌面版（简洁模式和高级模式、菜单栏）、终端版、命令行；一键更新；Clash Mi 扫码导入；TraceScope 网络可视化看板。

[Unreleased]: https://github.com/kadaliao/Tizi/compare/v1.0.0...HEAD
[1.0.0]: https://github.com/kadaliao/Tizi/releases/tag/v1.0.0
