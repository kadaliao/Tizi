package system

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// inspectCommandTimeout 限制 ps、lsof、route 这类只读检查命令的运行时间。
// macOS 上 lsof 在套接字很多时可能长时间不返回，诊断页和状态轮询不应因此
// 无限等待。
const inspectCommandTimeout = 5 * time.Second

// inspectOutput 在超时保护下运行只读检查命令并返回标准输出。
func inspectOutput(name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), inspectCommandTimeout)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).Output()
}

type Process struct {
	PID       int
	ParentPID int
	Command   string
}

type VPNState struct {
	Tunnelblick []Process
	OpenVPN     []Process
	Mihomo      []Process
	// Tailscale 是 Tailscale 客户端的进程（tailscaled 或 macOS 的网络扩展）。
	Tailscale  []Process
	UTUNs      []string
	parentPIDs map[int]int
}

func InspectVPN() VPNState {
	processes := listProcesses()
	state := VPNState{parentPIDs: make(map[int]int, len(processes))}
	for _, process := range processes {
		state.parentPIDs[process.PID] = process.ParentPID
		lower := strings.ToLower(process.Command)
		base := strings.ToLower(filepath.Base(process.Command))
		switch {
		case strings.Contains(lower, "tunnelblick") && strings.HasPrefix(base, "openvpn"):
			state.Tunnelblick = append(state.Tunnelblick, process)
		case strings.HasPrefix(base, "openvpn"):
			state.OpenVPN = append(state.OpenVPN, process)
		case base == "mihomo" || strings.HasPrefix(base, "mihomo-"):
			state.Mihomo = append(state.Mihomo, process)
		case base == "tailscaled" || base == "ipnextension" || strings.Contains(lower, "io.tailscale.ipn"):
			state.Tailscale = append(state.Tailscale, process)
		}
	}
	state.UTUNs = listUTUNs()
	return state
}

func (s VPNState) HasActiveOpenVPN() bool {
	return len(s.Tunnelblick) > 0 || len(s.OpenVPN) > 0
}

// BelongsToProcessTree reports whether pid is rootPID or one of its descendants
// in the process snapshot used to inspect the VPN state.
func (s VPNState) BelongsToProcessTree(pid, rootPID int) bool {
	if pid <= 0 || rootPID <= 0 {
		return false
	}
	seen := make(map[int]struct{})
	for pid > 0 {
		if pid == rootPID {
			return true
		}
		if _, exists := seen[pid]; exists {
			return false
		}
		seen[pid] = struct{}{}
		parentPID, exists := s.parentPIDs[pid]
		if !exists {
			return false
		}
		pid = parentPID
	}
	return false
}

func ProcessCommand(pid int) string {
	output, err := inspectOutput("ps", "-p", strconv.Itoa(pid), "-o", "command=")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

func ProcessStartToken(pid int) string {
	output, err := inspectOutput("ps", "-p", strconv.Itoa(pid), "-o", "lstart=")
	if err != nil {
		return ""
	}
	return strings.Join(strings.Fields(string(output)), " ")
}

// ProcessState 是 ProcessStateOf 的结论。
type ProcessState int

const (
	// ProcessUnknown 表示这次没查到（例如 ps 失败或超时），不能据此判断进程已退出。
	ProcessUnknown ProcessState = iota
	ProcessRunning
	// ProcessGone 表示进程已经退出（包括还没被父进程回收的僵尸进程），
	// 或者这个 PID 已经换成了别的进程。
	ProcessGone
)

// ProcessStateOf 判断 pid 是否仍是启动时间为 startToken（ProcessStartToken
// 的结果）的那个进程。
func ProcessStateOf(pid int, startToken string) ProcessState {
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return ProcessGone
	}
	output, err := inspectOutput("ps", "-p", strconv.Itoa(pid), "-o", "stat=", "-o", "lstart=")
	if err != nil {
		return ProcessUnknown
	}
	return parseProcessState(string(output), startToken)
}

// parseProcessState 解析 `ps -o stat= -o lstart=` 的输出，例如
// "Ss   Wed Oct 28 00:08:05 2026"；僵尸进程的状态以 Z 开头。
func parseProcessState(output, startToken string) ProcessState {
	fields := strings.Fields(output)
	if len(fields) < 2 {
		return ProcessUnknown
	}
	if strings.HasPrefix(fields[0], "Z") || strings.Join(fields[1:], " ") != startToken {
		return ProcessGone
	}
	return ProcessRunning
}

func ProcessExecutable(pid int) string {
	output, err := inspectOutput("ps", "-p", strconv.Itoa(pid), "-o", "comm=")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

func PortOwner(port int) string {
	owner := PortOwnerProcess(port)
	if owner.PID == 0 {
		return ""
	}
	name := owner.Command
	if name == "" {
		name = "未知进程"
	}
	return fmt.Sprintf("%s (PID %d)", name, owner.PID)
}

func PortOwnerProcess(port int) Process {
	if owner := portOwner("-iTCP:"+strconv.Itoa(port), "-sTCP:LISTEN"); owner.PID != 0 {
		return owner
	}
	if owner := portOwner("-iUDP:" + strconv.Itoa(port)); owner.PID != 0 {
		return owner
	}
	if runtime.GOOS == "linux" {
		if owner := ssPortOwner("tcp", port); owner.PID != 0 {
			return owner
		}
		return ssPortOwner("udp", port)
	}
	return Process{}
}

func portOwner(filters ...string) Process {
	args := append([]string{"-nP"}, filters...)
	args = append(args, "-Fpct")
	output, err := inspectOutput("lsof", args...)
	if err != nil || len(output) == 0 {
		return Process{}
	}
	var pid, command string
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		line := scanner.Text()
		if len(line) < 2 {
			continue
		}
		switch line[0] {
		case 'p':
			if pid != "" {
				value, _ := strconv.Atoi(pid)
				return Process{PID: value, Command: command}
			}
			pid = line[1:]
		case 'c':
			command = line[1:]
		}
	}
	if pid != "" {
		value, _ := strconv.Atoi(pid)
		return Process{PID: value, Command: command}
	}
	return Process{}
}

func CanUseSudoWithoutPrompt() bool {
	if !TUNRequiresAdministrator() {
		return true
	}
	cmd := exec.Command("sudo", "-n", "true")
	return cmd.Run() == nil
}

// askPassProgram 非空时，管理员授权改由 sudo -A 调用这个图形密码框程序，
// 而不是在当前终端读取密码。只有 GUI 后台引擎在启动时设置它。
var askPassProgram string

// AskPassPrompt 是图形密码框显示的说明，经 sudo -p 传给 askpass 程序。
// sudo 会展开 % 转义，文案中不能出现 %。
const AskPassPrompt = "Tizi 需要管理员权限来修改系统网络设置（TUN、DNS 或防火墙），请输入当前用户的登录密码。"

// UseAskPass 让之后的管理员授权通过 program 弹出图形密码框。它在没有可交互
// 终端的 GUI 引擎启动时调用一次；凭据仍由 sudo 按终端会话缓存，因此核心中
// 既有的 sudo -n 调用无需改动。
func UseAskPass(program string) {
	askPassProgram = program
}

// CancelAuthorization 结束当前进程经 sudo 弹出、仍在等待输入的图形密码框，
// 让对应的 sudo 以授权失败返回，返回结束的数量。GUI 退出时由引擎调用。
func CancelAuthorization() int {
	if askPassProgram == "" {
		return 0
	}
	processes := listProcesses()
	state := VPNState{parentPIDs: make(map[int]int, len(processes))}
	for _, process := range processes {
		state.parentPIDs[process.PID] = process.ParentPID
	}
	cancelled := 0
	for _, process := range processes {
		if process.Command != askPassProgram || !state.BelongsToProcessTree(process.PID, os.Getpid()) {
			continue
		}
		if syscall.Kill(process.PID, syscall.SIGTERM) == nil {
			cancelled++
		}
	}
	return cancelled
}

// AdministratorAuthorizationCommand returns the interactive command used to
// refresh sudo credentials. TUI callers can run it with tea.ExecProcess so the
// terminal is restored before sudo reads the user's password.
func AdministratorAuthorizationCommand() *exec.Cmd {
	if askPassProgram != "" {
		cmd := exec.Command("sudo", "-A", "-p", AskPassPrompt, "-v")
		cmd.Env = append(os.Environ(), "SUDO_ASKPASS="+askPassProgram)
		return cmd
	}
	return exec.Command("sudo", "-v")
}

// AuthorizeAdministrator requests administrator access in the current
// terminal. It is used by foreground CLI operations; the TUI runs the same
// command through Bubble Tea's interactive process support. With an askpass
// program configured it shows a password dialog instead.
func AuthorizeAdministrator() error {
	if !TUNRequiresAdministrator() || CanUseSudoWithoutPrompt() {
		return nil
	}
	cmd := AdministratorAuthorizationCommand()
	if askPassProgram != "" {
		output, err := cmd.CombinedOutput()
		if err != nil {
			if detail := strings.TrimSpace(string(output)); detail != "" {
				return fmt.Errorf("管理员授权未完成: %s", detail)
			}
			return fmt.Errorf("管理员授权未完成: %w", err)
		}
		return nil
	}
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("管理员授权未完成: %w", err)
	}
	return nil
}

func TCPReachable(address string, timeout time.Duration) (time.Duration, error) {
	start := time.Now()
	conn, err := net.DialTimeout("tcp", address, timeout)
	if err != nil {
		return 0, err
	}
	_ = conn.Close()
	return time.Since(start), nil
}

// RouteUnavailable 和 RouteUnsupported 是 DefaultRoute 的哨兵返回值，
// 供调用方区分真实路由和读取失败。
const (
	RouteUnavailable = "无法读取"
	RouteUnsupported = "当前平台不支持"
)

func DefaultRoute() string {
	if runtime.GOOS == "linux" {
		output, err := inspectOutput("ip", "-4", "route", "show", "default")
		if err != nil {
			return RouteUnavailable
		}
		return parseLinuxDefaultRoute(string(output))
	}
	if runtime.GOOS != "darwin" {
		return RouteUnsupported
	}
	output, err := inspectOutput("route", "-n", "get", "default")
	if err != nil {
		return RouteUnavailable
	}
	var gateway, iface string
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "gateway:") {
			gateway = strings.TrimSpace(strings.TrimPrefix(line, "gateway:"))
		}
		if strings.HasPrefix(line, "interface:") {
			iface = strings.TrimSpace(strings.TrimPrefix(line, "interface:"))
		}
	}
	if iface == "" {
		return RouteUnavailable
	}
	return fmt.Sprintf("%s via %s", iface, gateway)
}

func listProcesses() []Process {
	output, err := inspectOutput("ps", "-axo", "pid=,ppid=,comm=")
	if err != nil {
		return nil
	}
	var result []Process
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pid, pidErr := strconv.Atoi(fields[0])
		parentPID, parentErr := strconv.Atoi(fields[1])
		if pidErr != nil || parentErr != nil {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, fields[0]))
		command := strings.TrimSpace(strings.TrimPrefix(line, fields[1]))
		result = append(result, Process{PID: pid, ParentPID: parentPID, Command: command})
	}
	return result
}

func listUTUNs() []string {
	if runtime.GOOS == "linux" {
		matches, _ := filepath.Glob("/sys/class/net/*/tun_flags")
		interfaces := make([]string, 0, len(matches))
		for _, match := range matches {
			interfaces = append(interfaces, filepath.Base(filepath.Dir(match)))
		}
		sort.Strings(interfaces)
		return interfaces
	}
	if runtime.GOOS != "darwin" {
		return nil
	}
	output, err := inspectOutput("ifconfig", "-l")
	if err != nil {
		return nil
	}
	var interfaces []string
	for _, name := range strings.Fields(string(output)) {
		if strings.HasPrefix(name, "utun") {
			interfaces = append(interfaces, name)
		}
	}
	sort.Strings(interfaces)
	return interfaces
}

func TUNRequiresAdministrator() bool {
	return runtime.GOOS == "darwin" || runtime.GOOS == "linux"
}

func ssPortOwner(network string, port int) Process {
	flag := "-ltnp"
	if network == "udp" {
		flag = "-lunp"
	}
	output, err := inspectOutput("ss", "-H", flag, "sport", "=", ":"+strconv.Itoa(port))
	if err != nil {
		return Process{}
	}
	return parseSSOwner(string(output))
}

func parseSSOwner(output string) Process {
	for _, line := range strings.Split(output, "\n") {
		pidAt := strings.Index(line, "pid=")
		if pidAt < 0 {
			continue
		}
		pidText := line[pidAt+len("pid="):]
		if end := strings.IndexAny(pidText, ",) "); end >= 0 {
			pidText = pidText[:end]
		}
		pid, err := strconv.Atoi(pidText)
		if err != nil || pid <= 0 {
			continue
		}
		command := ""
		if userAt := strings.Index(line, `users:(("`); userAt >= 0 {
			command = line[userAt+len(`users:(("`):]
			if end := strings.Index(command, `"`); end >= 0 {
				command = command[:end]
			}
		}
		return Process{PID: pid, Command: command}
	}
	return Process{}
}

func parseLinuxDefaultRoute(output string) string {
	fields := strings.Fields(output)
	var gateway, iface string
	for index, field := range fields {
		if field == "via" && index+1 < len(fields) {
			gateway = fields[index+1]
		}
		if field == "dev" && index+1 < len(fields) {
			iface = fields[index+1]
		}
	}
	if iface == "" {
		return RouteUnavailable
	}
	if gateway == "" {
		return iface
	}
	return fmt.Sprintf("%s via %s", iface, gateway)
}
