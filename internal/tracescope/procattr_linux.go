//go:build linux

package tracescope

import (
	"os/exec"
	"syscall"
)

// setParentDeathSignal 让看板服务在父进程退出时收到 SIGTERM（Linux 专有），
// 与 TRACESCOPE_PARENT_PID 轮询互为兜底。
func setParentDeathSignal(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Pdeathsig = syscall.SIGTERM
}
