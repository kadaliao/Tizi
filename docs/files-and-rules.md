# 配置文件与自定义规则

## 配置目录

运行数据保存在 `~/.config/tizi/`（设置了 `XDG_CONFIG_HOME` 时在它下面）：

| 文件 | 内容 |
| --- | --- |
| `settings.json` | 用户设置，桌面版和终端版共用；包含订阅地址和 Tailscale Auth Key |
| `config.yaml` | 生成的 Mihomo 配置，包含节点密码、个人证书和私钥 |
| `proxies.yaml` | 可选：节点文件，见 [配置线路](configuration.md#节点文件) |
| `custom-rules.txt` | 自定义分流规则 |
| `claude-proxy.yaml` | 可选：Mihomo 节点格式的 Claude 住宅节点 |
| `providers/`、`tailscale/` | Mihomo 下载的订阅缓存和 Tailscale 登录状态 |
| `mihomo.log` | 日志，最多 5 MiB，另保留两份历史 |
| `dns-state.json` | 连接前保存的 DNS 和 IPv6 状态，断开时用来还原 |
| `geoip.metadb` | 判断 IP 所在地区的数据 |

Mihomo 核心装在 `~/.local/share/tizi/bin/mihomo`。这些文件都包含个人凭据或网络状态，不要提交到代码仓库或发给别人。

## 自定义规则

`custom-rules.txt` 每行一条 Mihomo 规则，`#` 开头的整行和行尾的 ` # 说明` 是注释：

```text
DOMAIN-SUFFIX,example.com,外网代理
DOMAIN,api.example.com,DIRECT
DOMAIN-SUFFIX,corp.example.com,内网
IP-CIDR,203.0.113.0/24,DIRECT,no-resolve
```

- 可用线路有 `外网代理`、`默认出口`、`Claude`、`DIRECT`、`REJECT`；导入 `.ovpn` 后还有 `内网`，开启 Tailscale 后还有 `Tailscale`。
- 自定义规则排在内置规则之后、兜底规则之前，可以覆盖内置的固定域名规则；Claude 的域名除外，它们固定走 `Claude` 线路。
- 保存或生成配置时会校验，出错会指出行号；安装和更新不会覆盖这个文件。
- 修改后运行 `tizi reload` 生效。也可以在桌面版或终端版的「规则」页直接测试某个网址走哪条线路，并保存域名规则。
