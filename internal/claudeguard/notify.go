package claudeguard

import (
	"os/exec"
	"runtime"
	"sync"
)

// desktopBundleID 是桌面版的 bundle id，系统弹窗的「打开Tizi」用它。
const desktopBundleID = "com.kadaliao.tizi.desktop"

// alertScript 显示一个需要手动关闭的警告框。标题和正文经 argv 传入，
// 不拼进脚本，不需要转义。只用 display alert 和 open，不向其他应用发送
// Apple Event，不会触发「自动化」权限请求。
const alertScript = `on run argv
	beep
	set reply to display alert (item 1 of argv) message (item 2 of argv) as critical buttons {"打开Tizi", "知道了"} default button "知道了"
	if button returned of reply is "打开Tizi" then
		do shell script "/usr/bin/open -b ` + desktopBundleID + ` >/dev/null 2>&1 || true"
	end if
end run`

// Notifier 在没有打开桌面版时用系统弹窗提醒，只在 macOS 上有效。同一时间
// 只保留一个弹窗：新告警替换还没关掉的旧告警。
type Notifier struct {
	mu      sync.Mutex
	current *exec.Cmd
}

// Show 显示告警，不等待用户关闭。
func (n *Notifier) Show(title, message string) {
	if runtime.GOOS != "darwin" {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.current != nil && n.current.Process != nil {
		_ = n.current.Process.Kill()
	}
	cmd := exec.Command("/usr/bin/osascript", "-e", alertScript, title, message)
	if err := cmd.Start(); err != nil {
		n.current = nil
		return
	}
	n.current = cmd
	go func() {
		_ = cmd.Wait()
		n.mu.Lock()
		if n.current == cmd {
			n.current = nil
		}
		n.mu.Unlock()
	}()
}
