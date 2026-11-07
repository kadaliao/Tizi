//go:build darwin

package engine

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"unsafe"
)

// sessionTTYFD 是伪终端从设备在子进程中的描述符编号（ExtraFiles[0]）。
const sessionTTYFD = 3

// RunInTerminalSession 在新会话中重新执行当前程序（参数为 args），并把一个
// 伪终端设为该会话的控制终端，然后等待它退出。
//
// sudo 默认按「终端 + 会话」缓存管理员凭据。GUI 启动的进程没有终端，
// 凭据会退化为按父进程缓存，引擎授权后，它启动的日志进程和 DNS 守护进程
// 里的 sudo -n 仍会失败。放进同一个伪终端会话后，授权一次即可在凭据有效期内
// 被整个进程树复用，与在 Terminal 中运行 TUI 的行为一致，核心代码无需区分。
func RunInTerminalSession(args []string) error {
	master, slave, err := openPTY()
	if err != nil {
		return fmt.Errorf("创建伪终端: %w", err)
	}
	defer master.Close()
	executable, err := os.Executable()
	if err != nil {
		_ = slave.Close()
		return fmt.Errorf("定位引擎程序: %w", err)
	}
	cmd := exec.Command(executable, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.ExtraFiles = []*os.File{slave}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: sessionTTYFD}
	startErr := cmd.Start()
	_ = slave.Close()
	if startErr != nil {
		return fmt.Errorf("启动引擎会话: %w", startErr)
	}
	// sudo 可能向终端写入提示或错误；必须持续读走，否则缓冲写满后 sudo 阻塞。
	// 这些内容只用于排障，转到标准错误由 GUI 记录。
	go func() { _, _ = io.Copy(os.Stderr, master) }()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(signals)
	go func() {
		for sig := range signals {
			_ = cmd.Process.Signal(sig)
		}
	}()
	err = cmd.Wait()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return fmt.Errorf("引擎退出: %s", exitErr.ProcessState)
	}
	return err
}

// PrepareSessionChild 在会话内的引擎进程启动时调用：保留控制终端，但不让
// 伪终端描述符泄漏给之后启动的 Mihomo 等子进程。
func PrepareSessionChild() {
	syscall.CloseOnExec(sessionTTYFD)
}

func openPTY() (*os.File, *os.File, error) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, err
	}
	fd := master.Fd()
	if err := ioctl(fd, syscall.TIOCPTYGRANT, 0); err != nil {
		_ = master.Close()
		return nil, nil, fmt.Errorf("grantpt: %w", err)
	}
	if err := ioctl(fd, syscall.TIOCPTYUNLK, 0); err != nil {
		_ = master.Close()
		return nil, nil, fmt.Errorf("unlockpt: %w", err)
	}
	var name [128]byte
	// unsafe.Pointer 到 uintptr 的转换必须写在 Syscall 调用表达式里，
	// 编译器才会在调用期间保持指针有效。
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCPTYGNAME, uintptr(unsafe.Pointer(&name[0]))); errno != 0 {
		_ = master.Close()
		return nil, nil, fmt.Errorf("ptsname: %w", errno)
	}
	end := bytes.IndexByte(name[:], 0)
	if end <= 0 {
		_ = master.Close()
		return nil, nil, fmt.Errorf("ptsname 返回空名称")
	}
	slave, err := os.OpenFile(string(name[:end]), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		_ = master.Close()
		return nil, nil, err
	}
	return master, slave, nil
}

// ioctl 只用于不带指针参数的请求。
func ioctl(fd, request, argument uintptr) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, request, argument); errno != 0 {
		return errno
	}
	return nil
}
