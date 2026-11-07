package tracescope

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"syscall"
	"time"
)

// DefaultPort 是看板页面与 API 共用的默认端口。
const DefaultPort = 8788

// Options 控制 tracescope 子命令的行为。
type Options struct {
	// ConfigPath 指向生成的 Mihomo 配置（paths.Generated）；存在时通过
	// MIHOMO_CONFIG 传给看板服务，使其能直接读取 controller 地址与密钥。
	ConfigPath string
	// Port 是看板页面与 API 共用的端口，默认 DefaultPort。
	Port int
	// Open 表示服务就绪后自动打开浏览器。
	Open bool
	// VPNActive 表示受管 Mihomo 正在运行（仅影响提示文案）。
	VPNActive bool
	Stdout    io.Writer
	Stderr    io.Writer
}

// Run 准备并启动 TraceScope 看板，直到 ctx 被取消或服务退出。
// 首次运行会自动解包内嵌源码、安装依赖并构建前端。
func Run(ctx context.Context, opts Options) error {
	if opts.Port <= 0 {
		opts.Port = DefaultPort
	}
	if opts.Stdout == nil {
		opts.Stdout = io.Discard
	}
	if opts.Stderr == nil {
		opts.Stderr = os.Stderr
	}
	dir, err := ensureWeb(opts.Stdout)
	if err != nil {
		return err
	}
	if err := ensureNode(opts.Stdout); err != nil {
		return err
	}
	if err := ensureDeps(ctx, dir, opts.Stdout); err != nil {
		return err
	}
	if err := ensureBuild(ctx, dir, opts.Stdout); err != nil {
		return err
	}
	return serve(ctx, dir, opts)
}

// serve 启动看板服务进程，等待 /health 就绪后打开浏览器，并管理进程生命周期。
func serve(ctx context.Context, dir string, opts Options) error {
	port := opts.Port
	if err := checkPortFree(port); err != nil {
		return err
	}
	cmd := exec.Command("node", "server/index.mjs")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"TRACESCOPE_PORT="+strconv.Itoa(port),
		// 看板服务据此定期检查父进程是否存活，父进程被强杀时自行退出，避免孤儿进程占用端口。
		"TRACESCOPE_PARENT_PID="+strconv.Itoa(os.Getpid()),
	)
	setParentDeathSignal(cmd)
	if opts.ConfigPath != "" {
		if _, err := os.Stat(opts.ConfigPath); err == nil {
			cmd.Env = append(cmd.Env, "MIHOMO_CONFIG="+opts.ConfigPath)
		}
	}
	cmd.Stdout = opts.Stdout
	cmd.Stderr = opts.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动看板服务: %w", err)
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	// stop 优雅停止子进程：先 SIGTERM，3 秒后仍未退出则 SIGKILL。
	stop := func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-waitCh:
		case <-time.After(3 * time.Second):
			_ = cmd.Process.Kill()
			<-waitCh
		}
	}

	healthURL := fmt.Sprintf("http://127.0.0.1:%d/health", port)
	if err := waitHealthy(ctx, healthURL, waitCh, 15*time.Second); err != nil {
		stop()
		return err
	}
	pageURL := fmt.Sprintf("http://127.0.0.1:%d/", port)
	fmt.Fprintf(opts.Stdout, "TraceScope 看板已启动: %s（按 Ctrl+C 停止）\n", pageURL)
	if !opts.VPNActive {
		fmt.Fprintln(opts.Stdout, "提示: Mihomo 未运行，看板显示本机现有连接（lsof 降级采集）；先执行 tizi start 可获得完整实时线路。")
	}
	if opts.Open {
		if err := openBrowser(pageURL); err != nil {
			fmt.Fprintf(opts.Stderr, "未能自动打开浏览器，请手动访问 %s\n", pageURL)
		}
	}
	select {
	case <-ctx.Done():
		stop()
		return nil
	case err := <-waitCh:
		if err != nil {
			return fmt.Errorf("看板服务异常退出: %w", err)
		}
		return errors.New("看板服务已退出")
	}
}

// waitHealthy 轮询 /health，直到就绪、超时、ctx 取消或服务进程提前退出。
func waitHealthy(ctx context.Context, healthURL string, waitCh <-chan error, timeout time.Duration) error {
	client := &http.Client{Timeout: 1 * time.Second}
	deadline := time.Now().Add(timeout)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-waitCh:
			if err == nil {
				err = errors.New("服务进程已退出")
			}
			return fmt.Errorf("看板服务提前退出: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("看板服务在 %s 内未就绪", timeout)
		}
		resp, err := client.Get(healthURL)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// checkPortFree 预先检查端口是否可用，给出更友好的提示。
func checkPortFree(port int) error {
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return fmt.Errorf("端口 %d 已被占用，无法启动看板；可用 --port 或 TRACESCOPE_PORT 指定其他端口", port)
	}
	_ = listener.Close()
	return nil
}

// openBrowser 用系统默认浏览器打开 URL（fire-and-forget）。
func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	default:
		return fmt.Errorf("不支持自动打开浏览器")
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait() // xdg-open 可能驻留，不等待其退出。
	return nil
}
