//go:build !linux

package tracescope

import "os/exec"

// setParentDeathSignal 在非 Linux 平台无对应机制，孤儿进程依靠
// TRACESCOPE_PARENT_PID 轮询自行退出。
func setParentDeathSignal(*exec.Cmd) {}
