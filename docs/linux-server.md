# Ubuntu 服务器

需要 Ubuntu 22.04 / 24.04 x86_64 和系统自带的 `systemd-resolved`。

SSH 登录后运行 `~/.local/bin/tizi`（用 Homebrew 装的直接运行 `tizi`），用法和桌面一样（见 [安装与使用](getting-started.md#终端版)）；退出时选「保持运行退出」，连接会继续在后台运行。服务器自己和 Docker 容器的出站流量按规则分流，SSH 等入站连接不受影响。

- **服务器下载不了安装包或 Mihomo**：在本机下载 Release 里的 `tizi-linux-amd64.tar.gz`，`scp` 到服务器的 `~/.local/bin/`。下载 Mihomo 太慢时借本机代理：`ssh -R 27891:127.0.0.1:7890 服务器` 登录，再运行 `TIZI_HTTP_PROXY=http://127.0.0.1:27891 tizi install-core`。
- **给其他机器用**：开「局域网代理」时一定同时设置「代理用户名」和「代理密码」（终端版「配置」页，或桌面版「配置 → 网络」的「要求用户名和密码」），否则任何人都能用这个端口。也可以用命令行：`tizi reload --allow-lan --proxy-user 用户名 --proxy-auth`，没有密码时会自动生成并打印出来。本机程序连接代理不需要认证。认证信息以明文经过网络，只在必要时开启。
- **云主机**：腾讯云的软件源、NTP 和 metadata（`*.tencentyun.com`、`169.254.0.0/16`）固定直连，并交给网卡原来的 DNS 解析。
- **提示 `systemd-resolved 不可用`**：多半是 dbus 重启过，运行 `sudo systemctl restart systemd-resolved` 后重试。运行 `tizi diagnose` 可以看到还缺什么。
