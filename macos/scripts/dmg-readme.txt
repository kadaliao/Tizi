Tizi 桌面版 安装说明

1. 把「TiziDesktop」拖到右边的「Applications」文件夹。
2. 在「应用程序」里打开「Tizi」。第一次会被 macOS 拦截
   （提示“无法验证开发者”），这是正常的，按下面处理一次即可：
   - 打开「系统设置 → 隐私与安全性」，拉到最下面，
     点「仍要打开」，再输入开机密码确认。
   - 或者在「终端」执行一次：
     xattr -dr com.apple.quarantine /Applications/TiziDesktop.app
3. 粘贴你的机场订阅地址（也可以导入节点文件、内网 .ovpn，或开启 Tailscale），
   点「连接」。连接时会弹出开机密码框，用来授权修改网络设置。

首次连接时会自动下载 Mihomo 核心，这一步需要能访问 github.com。
