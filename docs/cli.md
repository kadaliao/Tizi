# 命令行

把 `~/.local/bin` 加入 `PATH` 后可以直接使用 `tizi`（用 Homebrew 装的已经在 `PATH` 里）：

```sh
tizi                              # 启动终端版界面
tizi start                        # 连接；--no-tun 只提供本地代理，--proxy-china 国内流量也走代理
tizi stop
tizi reload                       # 重新生成配置并重启，让设置和规则修改生效
tizi status
tizi diagnose                     # 只读诊断本机网络和依赖
tizi update                       # 用 Homebrew 装的改用 brew upgrade tizi
tizi install-core                 # 加 --channel alpha 安装滚动 Alpha 构建
tizi generate                     # 只生成 Mihomo 配置
tizi validate                     # 生成并用 Mihomo 校验
tizi recover-wechat               # 只刷新微信直连，不重启
tizi claude-check                 # 检测 Claude 的线路和出口 IP
tizi claude-block on|off|status   # Claude 直连保护（macOS）
tizi clash-qr                     # 手机扫码导入
tizi dashboard                    # 打开 Mihomo Dashboard
tizi tracescope                   # 网络可视化看板
tizi version
```

## 连接参数

`generate`、`validate`、`start`、`reload`、`clash-qr` 接受下面的参数，并把它们保存到设置里：

```sh
tizi start --subscription 'https://example.com/api/v1/client/subscribe?token=...'
tizi start --ovpn ~/Downloads/office.ovpn --intranet-domains corp.example.com --intranet-dns 10.0.0.53
tizi start --tailscale --tailscale-auth-key tskey-auth-... --tailscale-exit-node home-server
tizi start --ovpn ''              # 移除内网
```

| 参数 | 作用 |
| --- | --- |
| `--subscription URL` | 机场订阅地址（传 `''` 清除） |
| `--ovpn PATH` | 内网 `.ovpn` 文件（传 `''` 移除） |
| `--remote HOST[,HOST...]` | 只用指定的 OpenVPN remote |
| `--intranet-domains`、`--intranet-cidrs`、`--intranet-dns` | 内网域名、网段、DNS，逗号分隔 |
| `--intranet-http HOST:PORT` | 内网 HTTP 出口 |
| `--tailscale` / `--no-tailscale` | 开启或关闭内置 Tailscale |
| `--tailscale-auth-key KEY`、`--tailscale-exit-node NODE` | Tailscale Auth Key、出口节点 |
| `--tun` / `--no-tun` | 接管或不接管系统流量 |
| `--allow-lan`、`--proxy-user NAME`、`--proxy-auth` | 局域网代理，要求用户名和密码（没有密码时自动生成） |
| `--direct-china` / `--proxy-china` | 国内流量直连或走代理 |

完整列表见 `tizi help`。

## 环境变量

| 变量 | 作用 |
| --- | --- |
| `TIZI_HTTP_PROXY` | 下载 Mihomo 核心和 GeoIP 时使用的 HTTP 代理 |
| `GITHUB_TOKEN` | 检查更新和下载核心时访问 GitHub API，提高配额 |
| `TRACESCOPE_PORT`、`TRACESCOPE_MAX_ROUTES` | TraceScope 看板的端口和最多线路数 |
