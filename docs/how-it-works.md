# 工作原理

Tizi 把你的订阅、节点、`.ovpn` 和 Tailscale 设置生成一份 [Mihomo](https://github.com/MetaCubeX/mihomo) 配置，以受管进程启动 Mihomo，并通过 TUN 接管系统流量。

## 分流顺序

规则按顺序匹配：

1. Apple 推送服务直连；内网域名和网段走「内网」；`100.64.0.0/10` 和 `*.ts.net` 走「Tailscale」。
2. 微信、飞书等国内服务按域名固定直连，不依赖 DNS 解析结果。
3. Claude 与 Anthropic 的域名走「Claude」（住宅节点或代理节点，不会直连）。
4. 你的自定义规则。
5. YouTube、Gemini 生成媒体等固定域名走「外网代理」。
6. 私有地址和中国大陆 IP 直连，其余走「默认出口」（默认是「外网代理」）。

策略组：

| 策略组 | 内容 |
| --- | --- |
| 自动选择 | 订阅和节点文件里的节点，按延迟选最快的 |
| 外网代理 | 自动选择、各个节点、内网 HTTP 出口、Tailscale 出口节点，最后是 DIRECT |
| 默认出口 | 外网代理或 DIRECT，兜底规则使用 |
| 内网 | `.ovpn` 里的各个 remote（导入 `.ovpn` 后才有） |
| Claude | 住宅节点和代理节点，没有 DIRECT |
| 前置代理 | 住宅节点链式连接时的前置节点，没有 DIRECT |
| Tailscale | 内置 Tailscale 节点（开启后才有） |

## DNS

运行期间 Tizi 接管 DNS：

- macOS：保存并在断开时恢复原网络服务的 DNS，为内网域名和 MagicDNS 写 `/etc/resolver` 文件，把内网域名加入搜索域。
- Ubuntu：通过 `systemd-resolved` 给 `tizi` 网卡配置 DNS 路由，并在停止、崩溃或下次启动时撤销。

内网域名经隧道交给内网 DNS 解析，`*.ts.net` 交给 Tailscale 解析，其余由 Mihomo 按规则解析。

## IPv6

TUN 只接管 IPv4，所以连接期间还会关闭物理网卡的公网 IPv6，否则双栈的海外网站会走 IPv6 直连，绕过代理并暴露真实地址：

- macOS 把每个有线、无线网络服务改为「仅本地链路」IPv6（保留 `fe80::` 本地发现）。
- Ubuntu 对物理网卡设置 `disable_ipv6=1`。

断开、崩溃恢复时按 `dns-state.json` 里的记录还原，原来手动配置或已关闭的 IPv6 不会被改动。只有连接后网络设置才会改变。

## 隧道恢复

连接期间会监测网络变化和睡眠唤醒，清理失效的连接。导入了 `.ovpn` 时，还会定期检查内网隧道（需要内网检查地址或内网 HTTP 出口），失败时切换到下一个 remote 或重建隧道。
