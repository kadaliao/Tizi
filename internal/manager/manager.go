package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kadaliao/tizi/internal/claude"
	"github.com/kadaliao/tizi/internal/claudeblock"
	"github.com/kadaliao/tizi/internal/claudeguard"
	"github.com/kadaliao/tizi/internal/config"
	"github.com/kadaliao/tizi/internal/controller"
	"github.com/kadaliao/tizi/internal/core"
	"github.com/kadaliao/tizi/internal/generator"
	"github.com/kadaliao/tizi/internal/ovpn"
	"github.com/kadaliao/tizi/internal/system"
)

var ErrVPNConflict = errors.New("检测到其他 OpenVPN 进程正在运行")

type Manager struct {
	Paths config.Paths
}

type Status struct {
	Running bool
	// Exited 表示 PID 记录还在但进程已经不在：Mihomo 不是经由 Stop 停下的
	// （崩溃、被结束，或者机器重启）。Stop 和下一次 Start 会清掉这个状态。
	Exited      bool
	PID         int
	Privileged  bool
	Command     string
	CorePath    string
	CoreVersion string
	StartToken  string
	Executable  string
}

type pidRecord struct {
	PID        int    `json:"pid"`
	Privileged bool   `json:"privileged,omitempty"`
	StartToken string `json:"start_token"`
	Executable string `json:"executable"`
	ConfigFile string `json:"config_file"`
}

// LoadProfile 读取设置里的 .ovpn；没有导入时返回 nil, nil。
func LoadProfile(settings config.Settings) (*ovpn.Profile, error) {
	if strings.TrimSpace(settings.OVPNPath) == "" {
		return nil, nil
	}
	profile, err := ovpn.ParseFile(settings.OVPNPath)
	if err != nil {
		return nil, err
	}
	return &profile, nil
}

// Generate 生成 Mihomo 配置，返回用到的 .ovpn（没有导入时为 nil）。
func (m Manager) Generate(settings config.Settings) (*ovpn.Profile, error) {
	if err := generator.CheckReady(m.Paths.ConfigDir, settings); err != nil {
		return nil, err
	}
	profile, err := LoadProfile(settings)
	if err != nil {
		return nil, err
	}
	if err := generator.Generate(profile, settings, m.Paths.Generated); err != nil {
		return nil, err
	}
	return profile, nil
}

// GenerateShare 生成给手机扫码导入的配置，返回文件路径。
func (m Manager) GenerateShare(settings config.Settings) (string, error) {
	if err := generator.CheckReady(m.Paths.ConfigDir, settings); err != nil {
		return "", err
	}
	profile, err := LoadProfile(settings)
	if err != nil {
		return "", err
	}
	if err := generator.GenerateShare(profile, settings, m.Paths.ShareConfig); err != nil {
		return "", err
	}
	return m.Paths.ShareConfig, nil
}

func dnsDomains(profile *ovpn.Profile, settings config.Settings) system.DNSDomains {
	return system.DNSDomains{
		Private: generator.ResolverDomains(profile, settings),
		Search:  generator.EffectiveIntranet(profile, settings).Domains,
	}
}

func (m Manager) Validate(settings config.Settings) error {
	if _, err := m.Generate(settings); err != nil {
		return err
	}
	if err := core.EnsureGeoIP(context.Background(), m.Paths, nil); err != nil {
		return err
	}
	return core.Validate(core.Find(m.Paths), m.Paths.ConfigDir, m.Paths.Generated)
}

func (m Manager) Start(settings config.Settings) (pid int, err error) {
	if err := m.Paths.Ensure(); err != nil {
		return 0, err
	}
	lock, err := acquireStartLock(m.Paths.PIDFile + ".lock")
	if err != nil {
		return 0, err
	}
	defer releaseStartLock(lock)
	status := m.Status()
	if status.Running {
		return status.PID, fmt.Errorf("Mihomo 已由本工具启动，PID %d", status.PID)
	}
	if _, err := m.RecoverDNS(); err != nil {
		return 0, fmt.Errorf("修复启动前的 DNS: %w", err)
	}
	vpn := system.InspectVPN()
	if settings.TunEnabled && vpn.HasActiveOpenVPN() && !settings.AllowVPNConflict {
		return 0, ErrVPNConflict
	}
	profile, err := m.Generate(settings)
	if err != nil {
		return 0, err
	}
	binary := core.Find(m.Paths)
	if binary == "" {
		return 0, fmt.Errorf("未找到 Mihomo，请先安装核心")
	}
	if err := core.EnsureGeoIP(context.Background(), m.Paths, nil); err != nil {
		return 0, err
	}
	for _, port := range []int{settings.MixedPort, settings.ControllerPort, settings.DNSPort} {
		if owner := system.PortOwner(port); owner != "" {
			return 0, fmt.Errorf("端口 %d 已被 %s 占用；本工具不会自动结束该进程", port, owner)
		}
	}
	if err := core.Validate(binary, m.Paths.ConfigDir, m.Paths.Generated); err != nil {
		return 0, err
	}
	if settings.TunEnabled {
		if err := system.ValidateTUNPlatform(); err != nil {
			return 0, err
		}
		if err := system.AuthorizeAdministrator(); err != nil {
			return 0, err
		}
	}
	dnsPrepared := false
	if settings.TunEnabled && runtime.GOOS == "darwin" {
		if err := system.PrepareDNS(m.dnsStatePath(), settings.DNSPort, dnsDomains(profile, settings)); err != nil {
			return 0, fmt.Errorf("准备系统 DNS: %w", err)
		}
		dnsPrepared = true
	}
	defer func() {
		if err == nil || !dnsPrepared {
			return
		}
		if _, restoreErr := system.RestoreDNS(m.dnsStatePath()); restoreErr != nil {
			err = errors.Join(err, fmt.Errorf("回滚系统 DNS: %w", restoreErr))
		}
	}()
	useSudo := settings.TunEnabled && system.TUNRequiresAdministrator()
	logFile, err := openLogFile(m.Paths.LogFile)
	if err != nil {
		return 0, fmt.Errorf("打开日志: %w", err)
	}
	if err := os.Chmod(m.Paths.LogFile, 0o600); err != nil {
		_ = logFile.Close()
		return 0, fmt.Errorf("设置日志权限: %w", err)
	}
	if err := logFile.Close(); err != nil {
		return 0, fmt.Errorf("关闭日志: %w", err)
	}
	launcher, err := os.Executable()
	if err != nil {
		return 0, fmt.Errorf("定位启动程序: %w", err)
	}
	cmd := exec.Command(launcher,
		"_run-logged-core",
		binary,
		m.Paths.ConfigDir,
		m.Paths.Generated,
		m.Paths.LogFile,
		strconv.FormatBool(useSudo),
	)
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("启动 Mihomo: %w", err)
	}
	pid = cmd.Process.Pid
	// 由本进程回收启动器。桌面版引擎和 TUI 常驻，不回收的话启动器退出后
	// 一直是僵尸进程，ps 里仍能看到它，下面「启动后立即退出」的检查也会漏掉。
	go func() { _ = cmd.Wait() }()
	time.Sleep(700 * time.Millisecond)
	if !processAlive(pid) {
		return 0, fmt.Errorf("Mihomo 启动后立即退出，请查看日志 %s", m.Paths.LogFile)
	}
	if settings.TunEnabled && runtime.GOOS == "linux" {
		if err := system.PrepareDNS(m.dnsStatePath(), settings.DNSPort, dnsDomains(profile, settings)); err != nil {
			_ = terminate(pid, "")
			return 0, fmt.Errorf("准备系统 DNS: %w", err)
		}
		dnsPrepared = true
	}
	record := pidRecord{
		PID:        pid,
		Privileged: useSudo,
		StartToken: system.ProcessStartToken(pid),
		Executable: system.ProcessExecutable(pid),
		ConfigFile: filepath.Clean(m.Paths.Generated),
	}
	if record.StartToken == "" || record.Executable == "" {
		_ = terminate(pid, "")
		return 0, fmt.Errorf("无法确认 Mihomo 进程身份，已取消启动")
	}
	if err := writePID(m.Paths.PIDFile, record); err != nil {
		_ = terminate(pid, record.StartToken)
		return 0, err
	}
	dnsPrepared = false
	return pid, nil
}

// StopOtherOpenVPN ends the OpenVPN processes currently detected on this host.
// It intentionally does not terminate VPN client applications themselves.
func (m Manager) StopOtherOpenVPN() (int, error) {
	vpn := system.InspectVPN()
	processes := append(vpn.Tunnelblick, vpn.OpenVPN...)
	if len(processes) == 0 {
		return 0, nil
	}
	if err := system.AuthorizeAdministrator(); err != nil {
		return 0, err
	}
	for _, process := range processes {
		startToken := system.ProcessStartToken(process.PID)
		executable := system.ProcessExecutable(process.PID)
		if startToken == "" || !isOpenVPNExecutable(executable) {
			return 0, fmt.Errorf("OpenVPN PID %d 的身份已变化，拒绝结束", process.PID)
		}
		if err := terminateProcess(process.PID, startToken, "其他 OpenVPN", defaultStopTimeout); err != nil {
			return 0, err
		}
	}
	if system.InspectVPN().HasActiveOpenVPN() {
		return 0, fmt.Errorf("OpenVPN 进程未完全退出，请先停止 VPN 客户端后重试")
	}
	return len(processes), nil
}

func (m Manager) Stop() error {
	if err := m.Paths.Ensure(); err != nil {
		return err
	}
	lock, err := acquireStartLock(m.Paths.PIDFile + ".lock")
	if err != nil {
		return fmt.Errorf("当前正在启动 Mihomo，请稍后再停止")
	}
	defer releaseStartLock(lock)
	status := m.Status()
	if !status.Running {
		_ = os.Remove(m.Paths.PIDFile)
		_, err := m.RecoverDNS()
		return err
	}
	if status.Privileged {
		if err := system.AuthorizeAdministrator(); err != nil {
			return err
		}
	}
	cleanGenerated := filepath.Clean(m.Paths.Generated)
	if !strings.Contains(status.Command, cleanGenerated) {
		return fmt.Errorf("PID %d 不再是本工具启动的 Mihomo，拒绝结束: %s", status.PID, status.Command)
	}
	if err := terminateWithin(status.PID, status.StartToken, managerStopTimeout); err != nil {
		return err
	}
	_ = os.Remove(m.Paths.PIDFile)
	if _, err := system.RecoverDNS(m.dnsStatePath()); err != nil {
		return fmt.Errorf("Mihomo 已停止，但恢复系统 DNS 失败: %w", err)
	}
	return nil
}

// Reload 用当前 settings 重新生成配置并重启受管 Mihomo，让新代码、自定义规则
// 或新安装的 APP 版本立即生效。Mihomo 尚未运行时会直接启动。
func (m Manager) Reload(settings config.Settings) (int, error) {
	if m.Status().Running {
		if err := m.Stop(); err != nil {
			return 0, err
		}
	}
	return m.Start(settings)
}

// RecoverDNS repairs state left by an interrupted TUN run. It never changes DNS
// while this manager's Mihomo process is still alive. A leftover snapshot next
// to a Mihomo process this manager does not own (an orphan of a killed
// launcher, or another client) is reported instead of silently skipped, since
// the system DNS stays hijacked until that process exits.
func (m Manager) RecoverDNS() (bool, error) {
	if m.Status().Running {
		return false, nil
	}
	_, statErr := os.Stat(m.dnsStatePath())
	hasSnapshot := statErr == nil
	if others := system.InspectVPN().Mihomo; len(others) > 0 {
		if !hasSnapshot {
			return false, nil
		}
		pids := make([]string, 0, len(others))
		for _, process := range others {
			pids = append(pids, strconv.Itoa(process.PID))
		}
		return false, fmt.Errorf("系统 DNS 仍被接管，但存在本工具未管理的 Mihomo 进程（PID %s）；请先结束该进程，再执行 tizi stop 恢复 DNS", strings.Join(pids, ", "))
	}
	if hasSnapshot {
		if err := system.AuthorizeAdministrator(); err != nil {
			return false, err
		}
	}
	return system.RecoverDNS(m.dnsStatePath())
}

// RecoverLeftoverDNS 在持有启动锁时执行 RecoverDNS，供界面上的「恢复 DNS」
// 使用：另一个进程（例如 TUI）正在启动或停止时直接拒绝，不在它保存 DNS
// 快照与写入 PID 之间把 DNS 改回去。
func (m Manager) RecoverLeftoverDNS() (bool, error) {
	if err := m.Paths.Ensure(); err != nil {
		return false, err
	}
	lock, err := acquireStartLock(m.Paths.PIDFile + ".lock")
	if err != nil {
		return false, fmt.Errorf("另一个启动或停止操作正在进行，请稍后再试")
	}
	defer releaseStartLock(lock)
	return m.RecoverDNS()
}

// LeftoverDNS 报告有没有上次运行遗留、尚未恢复的系统 DNS 设置：受管 Mihomo
// 没在运行，DNS 恢复点却还在。启动时会先保存恢复点再写入 PID，调用方要让
// 这个状态持续几秒再提示。
func (m Manager) LeftoverDNS(running bool) bool {
	path := m.dnsStatePath()
	if running || path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

func (m Manager) dnsStatePath() string {
	if m.Paths.DNSState != "" {
		return m.Paths.DNSState
	}
	if m.Paths.ConfigDir == "" {
		return ""
	}
	return filepath.Join(m.Paths.ConfigDir, "dns-state.json")
}

func (m Manager) Status() Status {
	binary := core.Find(m.Paths)
	status := Status{CorePath: binary, CoreVersion: cachedCoreVersion(binary)}
	data, err := os.ReadFile(m.Paths.PIDFile)
	if err != nil {
		return status
	}
	var record pidRecord
	if err := json.Unmarshal(data, &record); err != nil || record.PID <= 0 {
		return status
	}
	command := system.ProcessCommand(record.PID)
	startToken := system.ProcessStartToken(record.PID)
	executable := system.ProcessExecutable(record.PID)
	if command == "" || startToken == "" || startToken != record.StartToken || executable != record.Executable {
		status.Exited = true
		return status
	}
	if record.ConfigFile != filepath.Clean(m.Paths.Generated) || !strings.Contains(command, record.ConfigFile) {
		status.Exited = true
		return status
	}
	status.Running = true
	status.PID = record.PID
	status.Privileged = record.Privileged || loggedCoreUsesSudo(command)
	status.Command = command
	status.StartToken = startToken
	status.Executable = executable
	return status
}

// coreVersionCache 记住每个 Mihomo 二进制（按路径、大小和修改时间）的版本
// 字符串。TUI 每 2 秒调用一次 Status，没有缓存时每次都要额外启动一个
// `mihomo -v` 子进程。
var coreVersionCache struct {
	sync.Mutex
	entries map[coreVersionKey]string
}

type coreVersionKey struct {
	path    string
	size    int64
	modTime int64
}

func cachedCoreVersion(binary string) string {
	if binary == "" {
		return core.Version(binary)
	}
	info, err := os.Stat(binary)
	if err != nil {
		return core.Version(binary)
	}
	key := coreVersionKey{path: binary, size: info.Size(), modTime: info.ModTime().UnixNano()}
	coreVersionCache.Lock()
	defer coreVersionCache.Unlock()
	if version, ok := coreVersionCache.entries[key]; ok {
		return version
	}
	version := core.Version(binary)
	if version == "" {
		return version
	}
	if coreVersionCache.entries == nil {
		coreVersionCache.entries = make(map[coreVersionKey]string)
	}
	coreVersionCache.entries[key] = version
	return version
}

func writePID(path string, record pidRecord) error {
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("序列化 PID 记录: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("写入 PID 文件: %w", err)
	}
	return os.Chmod(path, 0o600)
}

func openLogFile(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err == nil || !errors.Is(err, os.ErrPermission) {
		return file, err
	}
	info, statErr := os.Lstat(path)
	if statErr != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("拒绝替换不可写的非普通日志文件")
	}
	backup := fmt.Sprintf("%s.unreadable-%d", path, time.Now().UnixNano())
	if renameErr := os.Rename(path, backup); renameErr != nil {
		return nil, fmt.Errorf("保留不可写日志 %s: %w", path, renameErr)
	}
	file, err = os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return file, nil
}

func processAlive(pid int) bool {
	return system.ProcessCommand(pid) != ""
}

const defaultStopTimeout = 2 * time.Second

func terminate(pid int, startToken string) error {
	return terminateWithin(pid, startToken, defaultStopTimeout)
}

func terminateWithin(pid int, startToken string, wait time.Duration) error {
	return terminateProcess(pid, startToken, "受管 Mihomo", wait)
}

func terminateProcess(pid int, startToken, name string, wait time.Duration) error {
	if startToken != "" && system.ProcessStartToken(pid) != startToken {
		return fmt.Errorf("PID %d 的启动时间已变化，拒绝结束", pid)
	}
	if err := signalProcess(pid, startToken, name, "-TERM"); err != nil {
		return err
	}
	deadline := time.Now().Add(wait)
	for {
		if !processAlive(pid) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s PID %d 未在 %s 内退出", name, pid, wait.Round(time.Second))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// killProcess 向仍未退出的受管进程发送 SIGKILL，仅由启动器在 SIGTERM 超时后
// 使用；此时进程身份已在 terminateProcess 中核对过。
func killProcess(pid int) error {
	if !processAlive(pid) {
		return nil
	}
	if err := signalProcess(pid, "", "受管 Mihomo", "-KILL"); err != nil {
		return err
	}
	for range 20 {
		if !processAlive(pid) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("受管 Mihomo PID %d 强制结束后仍存在", pid)
}

func signalProcess(pid int, startToken, name, signal string) error {
	cmd := exec.Command("kill", signal, strconv.Itoa(pid))
	output, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	if !system.TUNRequiresAdministrator() || !system.CanUseSudoWithoutPrompt() {
		return fmt.Errorf("结束%s PID %d: %s", name, pid, strings.TrimSpace(string(output)))
	}
	if startToken != "" && system.ProcessStartToken(pid) != startToken {
		return fmt.Errorf("PID %d 的启动时间已变化，拒绝以管理员权限结束", pid)
	}
	cmd = exec.Command("sudo", "-n", "kill", signal, strconv.Itoa(pid))
	if sudoOutput, sudoErr := cmd.CombinedOutput(); sudoErr != nil {
		return fmt.Errorf("结束%s PID %d: %s", name, pid, strings.TrimSpace(string(sudoOutput)))
	}
	return nil
}

func isOpenVPNExecutable(executable string) bool {
	return strings.HasPrefix(strings.ToLower(filepath.Base(executable)), "openvpn")
}

// loggedCoreUsesSudo recognizes PID records written before the privileged flag
// was added. The internal launcher always writes useSudo as its final argument.
func loggedCoreUsesSudo(command string) bool {
	fields := strings.Fields(command)
	return strings.Contains(command, "_run-logged-core") && len(fields) > 0 && fields[len(fields)-1] == "true"
}

func acquireStartLock(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("打开启动锁: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("另一个启动操作正在进行")
	}
	return file, nil
}

// dnsRecoveryLockWait 是会话退出后的 DNS 恢复等待启动锁的上限。Start 持锁
// 的时间主要是校验配置与启动核心，几秒内会释放。
const dnsRecoveryLockWait = 30 * time.Second

// recoverDNSAfterSessionExit 恢复 sessionPID 这个启动器遗留的 DNS 状态。
//
// 启动器自己退出后的恢复（root 守护进程，或 Mihomo 自行退出）与 Reload 里新
// 一轮 Start 会并发：旧会话的恢复动作若在新会话保存快照、安装 resolver 期间
// 才按旧清单执行，会把新会话刚写好的 resolver 文件删掉，系统 DNS 就回到
// 路由器，内网域名全部解析不了。这里先拿到启动锁，让恢复不会与 Start / Stop
// 交叠；拿到锁后如果 PID 文件已属于另一个仍在运行的会话，DNS 状态归它所有，
// 不再动。
func recoverDNSAfterSessionExit(statePath string, sessionPID int) error {
	pidFile := filepath.Join(filepath.Dir(statePath), "mihomo.pid")
	lock, err := waitStartLock(pidFile+".lock", dnsRecoveryLockWait)
	if err != nil {
		return err
	}
	defer releaseStartLock(lock)
	if successorRunning(pidFile, sessionPID) {
		return nil
	}
	_, err = system.RecoverDNS(statePath)
	return err
}

// successorRunning 报告 PID 文件是否记录着另一个仍在运行的启动器。
func successorRunning(pidFile string, sessionPID int) bool {
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return false
	}
	var record pidRecord
	if err := json.Unmarshal(data, &record); err != nil || record.PID <= 0 || record.PID == sessionPID {
		return false
	}
	startToken := system.ProcessStartToken(record.PID)
	return startToken != "" && startToken == record.StartToken
}

func waitStartLock(path string, wait time.Duration) (*os.File, error) {
	deadline := time.Now().Add(wait)
	for {
		lock, err := acquireStartLock(path)
		if err == nil {
			return lock, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("等待启动锁超时: %w", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func releaseStartLock(file *os.File) {
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	_ = file.Close()
}

// ClaudeEnv 汇总检测 Claude 线路需要的运行状态。Mihomo 运行时以生成的
// config.yaml 为准：设置改了但还没重载时，端口和 TUN 可能与设置不同。
func (m Manager) ClaudeEnv(settings config.Settings) claudeguard.Env {
	status := m.Status()
	env := claudeguard.Env{
		Enabled:    claudeguard.Enabled(m.Paths.ConfigDir, settings),
		Running:    status.Running,
		Exited:     status.Exited,
		TunEnabled: settings.TunEnabled,
		MixedPort:  settings.MixedPort,
		Prober:     controller.New(settings.ControllerPort, settings.ControllerSecret),
	}
	if status.Running {
		if runtime, err := claudeguard.ReadRuntime(m.Paths.Generated); err == nil {
			env.TunEnabled, env.MixedPort, env.Prober = runtime.TunEnabled, runtime.MixedPort, runtime.Client
			env.Applied = claude.Running(runtime.Residential)
		}
		if env.Enabled {
			env.Offline = system.DefaultRoute() == system.RouteUnavailable
		}
	}
	if env.Enabled {
		env.Node, _ = claude.Load(m.Paths.ConfigDir, settings)
		env.Blocked, env.BlockError = claudeBlockState()
	}
	return env
}

// claudeBlockState 报告直连保护是否安装，以及自检发现的问题。断网时无法
// 自检，不算失效。
func claudeBlockState() (bool, string) {
	if !claudeblock.Installed() {
		return false, ""
	}
	if err := claudeblock.SelfTest(); err != nil && !errors.Is(err, claudeblock.ErrSelfTestOffline) {
		return true, err.Error()
	}
	return true, ""
}
