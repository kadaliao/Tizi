package manager

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kadaliao/tizi/internal/claude"
	"github.com/kadaliao/tizi/internal/claudeguard"
	"github.com/kadaliao/tizi/internal/config"
	"github.com/kadaliao/tizi/internal/controller"
	"github.com/kadaliao/tizi/internal/system"
	"gopkg.in/yaml.v3"
)

const (
	maxLogSize          int64 = 5 << 20
	logBackupCount            = 2
	routePollInterval         = 2 * time.Second
	routeStableSamples        = 2
	tencentRefreshDelay       = 3 * time.Second

	// wakeGap 是判定「睡眠唤醒」的轮询间隔跳变阈值。正常 tick 约 2 秒，
	// 超过该值说明进程在系统睡眠期间被挂起，刚被唤醒。
	wakeGap = 10 * time.Second
	// wakeSettleDelay 是唤醒后等待默认路由与网络稳定再健康检查的时间。
	wakeSettleDelay = 3 * time.Second
	// vpnHealthTimeout 是唤醒后 VPN 健康检查的单次短超时。刻意短于
	// controller HTTP client 的 2s 硬超时，让 Mihomo 探测阈值先生效。
	vpnHealthTimeout = 1500 * time.Millisecond
	// maxWakeRecoveryRetries 是唤醒恢复失败后的最大重试次数，避免路由
	// 长期无效时无限循环。
	maxWakeRecoveryRetries = 3
	// vpnHealthCheckInterval 是周期性 VPN 健康检查间隔。OpenVPN 隧道会因
	// reneg（密钥重协商）或服务端会话轮换而周期性断开，断开若发生在非
	// 睡眠、非网络切换的时段，现有的 wake/路由监控都覆盖不到；这里每隔
	// 一段时间主动探测当前节点能否访问内网，失败时触发恢复。
	vpnHealthCheckInterval = 30 * time.Second
	// vpnHealthFailThreshold 是健康检查连续失败的次数阈值，达到后才执行
	// 恢复，避免单次探测抖动误伤正常工作的隧道。
	vpnHealthFailThreshold = 2
	// maxHealthRecoveryBackoff 限制连续恢复之间的退避倍数：每次恢复后
	// 若隧道仍不健康，下一次恢复所需的连续失败次数翻倍，最多为阈值的
	// 该倍数（按默认参数约 16 分钟一次），避免 VPN 长期不可达时每分钟
	// 都清空全部连接并轮换 remote。任一次探测成功即重置退避。
	maxHealthRecoveryBackoff = 16
	// coreStopTimeout 是启动器等待 Mihomo 响应 SIGTERM 的时间，超时后升级为
	// SIGKILL；managerStopTimeout 是 Stop 等待启动器整体退出的时间，必须
	// 严格长于前者，否则 TUN 拆除稍慢就会被误报为停止失败。
	coreStopTimeout    = 4 * time.Second
	managerStopTimeout = 8 * time.Second

	// vpnGroupName 与 openVPNProxyPrefix 对应 generator 生成的代理组名与
	// OpenVPN 节点命名，用于定位当前节点并在多个 remote 间轮转切换。
	vpnGroupName       = "内网"
	openVPNProxyPrefix = "openvpn-"
)

type rotatingLogWriter struct {
	path    string
	maxSize int64
	backups int
	file    *os.File
	size    int64
	mu      sync.Mutex
}

func newRotatingLogWriter(path string, maxSize int64, backups int) (*rotatingLogWriter, error) {
	writer := &rotatingLogWriter{path: path, maxSize: maxSize, backups: backups}
	if err := writer.open(); err != nil {
		return nil, err
	}
	if writer.size > writer.maxSize {
		if err := writer.file.Close(); err != nil {
			return nil, err
		}
		if err := os.Remove(writer.path); err != nil {
			return nil, err
		}
		if err := writer.open(); err != nil {
			return nil, err
		}
	} else if writer.size == writer.maxSize {
		if err := writer.rotate(); err != nil {
			_ = writer.file.Close()
			return nil, err
		}
	}
	return writer, nil
}

func (w *rotatingLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	originalLen := len(p)
	if int64(len(p)) > w.maxSize {
		p = p[int64(len(p))-w.maxSize:]
	}
	if w.size+int64(len(p)) > w.maxSize {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	if err == nil && n == len(p) {
		return originalLen, nil
	}
	return n, err
}

func (w *rotatingLogWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.file.Close()
}

func (w *rotatingLogWriter) open() error {
	file, err := openLogFile(w.path)
	if err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return err
	}
	w.file = file
	w.size = info.Size()
	return nil
}

func (w *rotatingLogWriter) rotate() error {
	if err := w.file.Close(); err != nil {
		return err
	}
	if w.backups == 0 {
		if err := os.Remove(w.path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return w.open()
	}
	for index := w.backups; index >= 1; index-- {
		destination := fmt.Sprintf("%s.%d", w.path, index)
		if err := os.Remove(destination); err != nil && !os.IsNotExist(err) {
			return err
		}
		if index == 1 {
			if err := os.Rename(w.path, destination); err != nil && !os.IsNotExist(err) {
				return err
			}
			continue
		}
		source := fmt.Sprintf("%s.%d", w.path, index-1)
		if err := os.Rename(source, destination); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return w.open()
}

// RunLoggedCore owns Mihomo's output after the command that requested Start exits.
func RunLoggedCore(binary, configDir, generated, logPath string, useSudo bool) error {
	logWriter, err := newRotatingLogWriter(logPath, maxLogSize, logBackupCount)
	if err != nil {
		return fmt.Errorf("打开日志: %w", err)
	}
	defer logWriter.Close()

	if useSudo {
		if err := startDNSRecoveryGuard(filepath.Join(configDir, "dns-state.json")); err != nil {
			return fmt.Errorf("启动 DNS 恢复守护进程: %w", err)
		}
	}

	args := []string{"-d", configDir, "-f", generated}
	var cmd *exec.Cmd
	if useSudo {
		cmd = exec.Command("sudo", append([]string{"-n", binary}, args...)...)
	} else {
		cmd = exec.Command(binary, args...)
	}
	// Mihomo 的连接日志同时交给 Claude 访问监测：看到 Claude 的失败或直连
	// 立即检测，不等下一次定期检测。
	claudeEvents := make(chan claudeguard.LogEvent, 16)
	cmd.Stdout = io.MultiWriter(logWriter, &claudeLogWatcher{events: claudeEvents})
	cmd.Stderr = logWriter
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(signals)
	if err := cmd.Start(); err != nil {
		_, _ = fmt.Fprintf(logWriter, "启动 Mihomo: %v\n", err)
		return fmt.Errorf("启动 Mihomo: %w", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	stopRecovery := startConnectionRecovery(generated, logWriter)
	defer stopRecovery()
	stopPin := startClaudePin(generated, logWriter)
	defer stopPin()
	notifier := &claudeguard.Notifier{}
	stopGuard := startClaudeGuard(configDir, generated, logWriter, notifier, claudeEvents)
	defer stopGuard()

	select {
	case err := <-done:
		// Mihomo 自行退出（崩溃或配置错误），而不是经由本工具停止。
		// TUI 可能已经脱离（保持运行退出），在这里恢复 TUN 启动前
		// 保存的系统 DNS，避免留下打不开网的机器；快照不存在时是空操作。
		stopGuard()
		if recoverErr := recoverDNSAfterSessionExit(filepath.Join(configDir, "dns-state.json"), os.Getpid()); recoverErr != nil {
			_, _ = fmt.Fprintf(logWriter, "恢复系统 DNS: %v\n", recoverErr)
		}
		notifyUnexpectedExit(configDir, logWriter, notifier)
		return err
	case <-signals:
		if err := terminateWithin(cmd.Process.Pid, "", coreStopTimeout); err != nil {
			_, _ = fmt.Fprintf(logWriter, "停止 Mihomo: %v；升级为强制结束\n", err)
			if killErr := killProcess(cmd.Process.Pid); killErr != nil {
				_, _ = fmt.Fprintf(logWriter, "强制结束 Mihomo: %v\n", killErr)
				return err
			}
		}
		<-done
		return nil
	}
}

// claudeGuardStartDelay 是 Mihomo 启动后第一次检测 Claude 线路前的等待，
// 给 OpenVPN 握手留出时间。
const claudeGuardStartDelay = 10 * time.Second

// startClaudeGuard 在没有打开桌面版时监测 Claude 的线路，发现问题时写日志，
// 在 macOS 上弹出系统提醒。桌面版打开时由它负责，这里只等待。返回的函数
// 只发出停止信号、不等待：一次检测可能要十几秒，停止 VPN 不能被它拖住。
func startClaudeGuard(configDir, generated string, logWriter io.Writer, notifier *claudeguard.Notifier, events <-chan claudeguard.LogEvent) func() {
	stop := make(chan struct{})
	go runClaudeGuard(stop, configDir, generated, logWriter, notifier, events)
	var once sync.Once
	return func() { once.Do(func() { close(stop) }) }
}

func runClaudeGuard(stop <-chan struct{}, configDir, generated string, logWriter io.Writer, notifier *claudeguard.Notifier, events <-chan claudeguard.LogEvent) {
	settingsPaths := config.Paths{Settings: filepath.Join(configDir, "settings.json")}
	tracker := claudeguard.NewTracker()
	notified := ""
	// active 报告这里是否负责告警：开启了监测，而且桌面版没在负责。
	active := func() (config.Settings, bool) {
		settings, err := config.Load(settingsPaths)
		if err != nil || !claudeguard.Enabled(configDir, settings) || claudeguard.UIActive(configDir) {
			return settings, false
		}
		return settings, true
	}
	report := func(status claudeguard.Status) {
		switch {
		case status.Level == claudeguard.LevelAlert && status.Key != notified:
			notified = status.Key
			_, _ = fmt.Fprintf(logWriter, "Claude 访问告警：%s。%s\n", status.Title, status.Detail)
			notifier.Show(status.Title, status.Detail)
		case status.Level != claudeguard.LevelAlert && status.Pending == 0 && notified != "":
			notified = ""
			_, _ = fmt.Fprintf(logWriter, "Claude 访问已恢复：%s\n", status.Title)
		}
	}
	timer := time.NewTimer(claudeGuardStartDelay)
	defer timer.Stop()
	for {
		select {
		case <-stop:
			return
		case <-timer.C:
		case event := <-events:
			if _, ok := active(); !ok {
				continue
			}
			if !tracker.Event(event, time.Now()) {
				report(tracker.Status())
				continue
			}
			timer.Stop()
		}
		settings, ok := active()
		runtimeConfig, runtimeErr := claudeguard.ReadRuntime(generated)
		if !ok || runtimeErr != nil {
			// 没开监测，或桌面版在负责：重新开始计数，交接时不沿用旧结论。
			tracker = claudeguard.NewTracker()
			notified = ""
			timer.Reset(claudeguard.CheckInterval)
			continue
		}
		node, _ := claude.Load(configDir, settings)
		blocked, blockError := claudeBlockState()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		observed := claudeguard.Observe(ctx, claudeguard.Env{
			Enabled:    true,
			Running:    true,
			Offline:    system.DefaultRoute() == system.RouteUnavailable,
			Blocked:    blocked,
			BlockError: blockError,
			TunEnabled: runtimeConfig.TunEnabled,
			MixedPort:  runtimeConfig.MixedPort,
			Prober:     runtimeConfig.Client,
			Node:       node,
		})
		cancel()
		select {
		case <-stop:
			// 检测期间 VPN 已经在停止，结论作废。
			return
		default:
		}
		report(tracker.Update(observed))
		tracker.Done(time.Now())
		timer.Reset(tracker.NextDelay())
	}
}

// claudeLogWatcher 从 Mihomo 的标准输出里挑出与 Claude 有关的失败和直连。
// 只由 exec 的输出复制 goroutine 调用；事件通道满了就丢弃，不阻塞日志。
type claudeLogWatcher struct {
	events  chan<- claudeguard.LogEvent
	partial []byte
}

func (w *claudeLogWatcher) Write(p []byte) (int, error) {
	data := append(w.partial, p...)
	for {
		index := bytes.IndexByte(data, '\n')
		if index < 0 {
			break
		}
		if event, ok := claudeguard.ParseLog(logMessage(string(data[:index]))); ok {
			select {
			case w.events <- event:
			default:
			}
		}
		data = data[index+1:]
	}
	// 半行留到下次拼接；异常的超长行直接丢弃。
	if len(data) > 64<<10 {
		data = nil
	}
	w.partial = append(w.partial[:0], data...)
	return len(p), nil
}

// logMessage 取出 Mihomo 文本日志里 msg 字段的内容：
// time="..." level=info msg="[TCP] ... using Claude[住宅节点]"
func logMessage(line string) string {
	index := strings.Index(line, " msg=")
	if index < 0 {
		return line
	}
	value := strings.TrimSpace(line[index+len(" msg="):])
	if unquoted, err := strconv.Unquote(value); err == nil {
		return unquoted
	}
	return strings.Trim(value, `"`)
}

// notifyUnexpectedExit 在 Mihomo 自行退出后提醒：系统流量已不再经过代理，
// 访问 Claude 会直连。桌面版打开时由它从 PID 记录发现并提醒。
func notifyUnexpectedExit(configDir string, logWriter io.Writer, notifier *claudeguard.Notifier) {
	settings, err := config.Load(config.Paths{Settings: filepath.Join(configDir, "settings.json")})
	if err != nil || !claudeguard.Enabled(configDir, settings) {
		return
	}
	const title = "VPN 意外断开"
	message := "Mihomo 已经退出，系统流量不再经过代理，现在访问 Claude 会直连并暴露本机 IP。请先重新连接 VPN，再使用 Claude。"
	if blocked, blockError := claudeBlockState(); blocked && blockError == "" {
		message = "Mihomo 已经退出。直连保护在拦截 Claude 的连接，不会直连，但 Claude 暂时打不开。请重新连接 VPN。"
	}
	_, _ = fmt.Fprintf(logWriter, "Claude 访问告警：%s。%s\n", title, message)
	if !claudeguard.UIActive(configDir) {
		notifier.Show(title, message)
	}
}

type generatedControllerConfig struct {
	ExternalController string `yaml:"external-controller"`
	Secret             string `yaml:"secret"`
}

type routeChangeTracker struct {
	baseline  string
	candidate string
	samples   int
}

func (t *routeChangeTracker) observe(route string, stableSamples int) bool {
	if !validDefaultRoute(route) {
		return false
	}
	if t.baseline == "" {
		t.baseline = route
		return false
	}
	if route == t.baseline {
		t.candidate = ""
		t.samples = 0
		return false
	}
	if route != t.candidate {
		t.candidate = route
		t.samples = 1
	} else {
		t.samples++
	}
	return t.samples >= stableSamples
}

func (t *routeChangeTracker) accept(route string) {
	t.baseline = route
	t.candidate = ""
	t.samples = 0
}

func validDefaultRoute(route string) bool {
	return route != "" && route != system.RouteUnavailable && route != system.RouteUnsupported
}

func startConnectionRecovery(generated string, logWriter io.Writer) func() {
	client, err := controllerFromGeneratedConfig(generated)
	if err != nil {
		_, _ = fmt.Fprintf(logWriter, "自动恢复连接未启用: %v\n", err)
		return func() {}
	}
	// 没有导入 .ovpn 时没有内网隧道要维护；没有检查地址时无法主动确认隧道
	// 状态，只在睡眠唤醒后重建。
	var checkVPN func() bool
	var recoverFn func(bool) (vpnRecovery, error)
	if hasVPN, probeURL := vpnProbe(generated); hasVPN {
		recoverFn = func(closeAll bool) (vpnRecovery, error) { return recoverVPN(client, generated, closeAll) }
		if probeURL != "" {
			checkVPN = func() bool { return vpnHealthy(client, probeURL) }
		}
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		monitorDefaultRoute(stop, routeMonitor{
			defaultRoute:     system.DefaultRoute,
			closeConnections: client.CloseConnections,
			closeTencentConnections: func() (int, error) {
				return client.CloseConnectionsMatching(isTencentDirectConnection)
			},
			checkVPN:            checkVPN,
			recoverVPN:          recoverFn,
			logWriter:           logWriter,
			interval:            routePollInterval,
			stableSamples:       routeStableSamples,
			tencentRefreshDelay: tencentRefreshDelay,
			wakeGap:             wakeGap,
			wakeSettleDelay:     wakeSettleDelay,
			healthInterval:      vpnHealthCheckInterval,
			healthFailMax:       vpnHealthFailThreshold,
		})
	}()
	return func() {
		close(stop)
		<-done
	}
}

// RecoverWeChatConnections refreshes only active Tencent/WeChat connections
// that are already routed DIRECT. It does not restart Mihomo or change policy.
func (m Manager) RecoverWeChatConnections() (int, error) {
	client, err := controllerFromGeneratedConfig(m.Paths.Generated)
	if err != nil {
		return 0, fmt.Errorf("读取 Mihomo Controller 配置: %w", err)
	}
	closed, err := client.CloseConnectionsMatching(isTencentDirectConnection)
	if err != nil {
		return closed, fmt.Errorf("刷新微信直连: %w", err)
	}
	return closed, nil
}

// vpnHealthy 通过 Mihomo 延迟 API 快速探测当前 OpenVPN 节点是否仍能访问
// 内网检查地址，用于睡眠唤醒后的短超时健康检查。
func vpnHealthy(client controller.Client, probeURL string) bool {
	groups, err := client.Groups()
	if err != nil {
		return false
	}
	name := currentVPNProxy(groups)
	if name == "" {
		return false
	}
	delay, err := client.ProxyDelay(name, probeURL, vpnHealthTimeout)
	return err == nil && delay > 0
}

// vpnProbe 从生成的配置和设置里读出：有没有 OpenVPN 节点，以及检查内网
// 隧道用的地址（设置里的检查地址，否则是内网 HTTP 出口）。
func vpnProbe(generated string) (bool, string) {
	data, err := os.ReadFile(generated)
	if err != nil {
		return false, ""
	}
	var parsed struct {
		Proxies []map[string]any `yaml:"proxies"`
	}
	if yaml.Unmarshal(data, &parsed) != nil {
		return false, ""
	}
	hasVPN, probeURL := false, ""
	for _, proxy := range parsed.Proxies {
		if proxy["type"] == "openvpn" {
			hasVPN = true
		}
		if proxy["name"] == config.IntranetHTTPProxyName {
			server, _ := proxy["server"].(string)
			if port, ok := proxy["port"].(int); ok && server != "" {
				probeURL = "http://" + net.JoinHostPort(server, strconv.Itoa(port))
			}
		}
	}
	if settings, err := config.Load(config.Paths{Settings: filepath.Join(filepath.Dir(generated), "settings.json")}); err == nil && settings.IntranetProbeURL != "" {
		probeURL = settings.IntranetProbeURL
	}
	return hasVPN, probeURL
}

// vpnRecovery 描述一次主动 VPN 恢复实际执行的动作。只有单个 OpenVPN remote
// 的部署里没有可轮转的节点，恢复改为让 Mihomo 重建 outbound，因此调用方需要
// 区分「切换 remote」与「重建隧道」这两种成功路径。
type vpnRecovery int

const (
	// vpnRecoveryNone 表示既没有备用 remote，也没有重建隧道，只能等 OpenVPN
	// 自身的 ping-restart 兜底。
	vpnRecoveryNone vpnRecovery = iota
	// vpnRecoverySwitched 表示已切换到「内网」组里的下一个 OpenVPN remote。
	vpnRecoverySwitched
	// vpnRecoveryRebuilt 表示没有备用 remote，已强制 Mihomo 重载配置，用新的
	// outbound 触发一次全新握手。
	vpnRecoveryRebuilt
)

// recoverVPN 清理滞留连接，并强制建立新的 VPN 会话：配置了多个 OpenVPN
// remote 时切换到下一个；只有一个 remote 时改为让 Mihomo 重载配置以重建
// outbound，因为失效的会话可能一边收不到有效数据、一边靠零星回包不断重置
// ping-restart 的计时，从而长期不自行重建。closeAll 为 false 时只关闭经由
// 内网 VPN 链路的连接，直连流量不受影响；睡眠唤醒后整机网络都可能失效，
// 调用方会传 true 清空全部连接。
func recoverVPN(client controller.Client, generated string, closeAll bool) (vpnRecovery, error) {
	if closeAll {
		if err := client.CloseConnections(); err != nil {
			return vpnRecoveryNone, fmt.Errorf("清理旧连接: %w", err)
		}
	} else if _, err := client.CloseConnectionsMatching(isVPNConnection); err != nil {
		return vpnRecoveryNone, fmt.Errorf("清理 VPN 旧连接: %w", err)
	}
	groups, err := client.Groups()
	if err != nil {
		return vpnRecoveryNone, fmt.Errorf("读取代理组: %w", err)
	}
	next := nextVPNProxy(groups, currentVPNProxy(groups))
	if next != "" {
		if err := client.Select(vpnGroupName, next); err != nil {
			return vpnRecoveryNone, fmt.Errorf("切换 OpenVPN remote: %w", err)
		}
		return vpnRecoverySwitched, nil
	}
	// 没有备用 remote。重建需要配置文件路径；缺失时保持旧的保守行为，
	// 不猜路径也不误报已恢复。
	if strings.TrimSpace(generated) == "" {
		return vpnRecoveryNone, nil
	}
	if err := client.ReloadConfig(generated); err != nil {
		return vpnRecoveryNone, fmt.Errorf("重建 OpenVPN 隧道: %w", err)
	}
	return vpnRecoveryRebuilt, nil
}

// isVPNConnection 判断连接是否经由内网 VPN 链路（代理组或 OpenVPN 节点）。
func isVPNConnection(connection controller.Connection) bool {
	for _, chain := range connection.Chains {
		if chain == vpnGroupName || strings.HasPrefix(chain, openVPNProxyPrefix) {
			return true
		}
	}
	return false
}

// currentVPNProxy 返回「内网」代理组当前选中的 OpenVPN 节点名；当前
// 选中项不是 OpenVPN 节点或找不到该组时返回空字符串。
func currentVPNProxy(groups []controller.Group) string {
	for _, group := range groups {
		if group.Name != vpnGroupName {
			continue
		}
		if strings.HasPrefix(group.Now, openVPNProxyPrefix) {
			return group.Now
		}
		return ""
	}
	return ""
}

// nextVPNProxy 返回「内网」代理组中位于 current 之后的下一个 OpenVPN
// 节点（轮转）；只有一个节点或 current 不在列表中时返回空字符串。
func nextVPNProxy(groups []controller.Group, current string) string {
	var proxies []string
	for _, group := range groups {
		if group.Name != vpnGroupName {
			continue
		}
		for _, choice := range group.Choices {
			if strings.HasPrefix(choice, openVPNProxyPrefix) {
				proxies = append(proxies, choice)
			}
		}
	}
	if len(proxies) < 2 {
		return ""
	}
	for index, proxy := range proxies {
		if proxy == current {
			return proxies[(index+1)%len(proxies)]
		}
	}
	return proxies[0]
}

func controllerFromGeneratedConfig(path string) (controller.Client, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return controller.Client{}, err
	}
	var generated generatedControllerConfig
	if err := yaml.Unmarshal(data, &generated); err != nil {
		return controller.Client{}, err
	}
	host, portText, err := net.SplitHostPort(generated.ExternalController)
	if err != nil || (host != "127.0.0.1" && host != "localhost") {
		return controller.Client{}, fmt.Errorf("Controller 地址不是本机地址")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port <= 0 || port > 65535 {
		return controller.Client{}, fmt.Errorf("Controller 端口无效")
	}
	return controller.New(port, generated.Secret), nil
}

// routeMonitor 承载默认路由监控的全部回调与时间参数。回调在专用 goroutine
// 中运行；除 defaultRoute 外都可以为 nil，以跳过对应动作（测试中常用）。
type routeMonitor struct {
	defaultRoute            func() string
	closeConnections        func() error
	closeTencentConnections func() (int, error)
	checkVPN                func() bool
	recoverVPN              func(closeAll bool) (vpnRecovery, error)
	logWriter               io.Writer
	interval                time.Duration
	stableSamples           int
	tencentRefreshDelay     time.Duration
	wakeGap                 time.Duration
	wakeSettleDelay         time.Duration
	healthInterval          time.Duration
	healthFailMax           int
}

func monitorDefaultRoute(stop <-chan struct{}, monitor routeMonitor) {
	tracker := routeChangeTracker{}
	tracker.observe(monitor.defaultRoute(), monitor.stableSamples)
	ticker := time.NewTicker(monitor.interval)
	defer ticker.Stop()

	var targetedTimer *time.Timer
	var targetedTimerC <-chan time.Time
	defer func() {
		if targetedTimer != nil {
			targetedTimer.Stop()
		}
	}()

	var wakeTimer *time.Timer
	var wakeTimerC <-chan time.Time
	defer func() {
		if wakeTimer != nil {
			wakeTimer.Stop()
		}
	}()

	var healthTicker *time.Ticker
	var healthC <-chan time.Time
	healthFailures := 0
	recoveryBackoff := 1
	if monitor.healthInterval > 0 && monitor.checkVPN != nil {
		healthTicker = time.NewTicker(monitor.healthInterval)
		healthC = healthTicker.C
		defer healthTicker.Stop()
	}

	lastTick := time.Now()
	wakeRetries := 0
	for {
		select {
		case <-stop:
			return
		case <-targetedTimerC:
			targetedTimer = nil
			targetedTimerC = nil
			closed, err := monitor.closeTencentConnections()
			if err != nil {
				_, _ = fmt.Fprintf(monitor.logWriter, "微信连接二次刷新完成 %d 条，仍有失败: %v\n", closed, err)
			} else if closed > 0 {
				_, _ = fmt.Fprintf(monitor.logWriter, "默认出口切换后已再次刷新 %d 条微信直连\n", closed)
			}
		case <-wakeTimerC:
			wakeTimer = nil
			wakeTimerC = nil
			if !monitor.recoverAfterWake() && wakeRetries < maxWakeRecoveryRetries {
				wakeRetries++
				wakeTimer = time.NewTimer(monitor.wakeSettleDelay)
				wakeTimerC = wakeTimer.C
			}
		case <-healthC:
			if monitor.checkVPN() {
				healthFailures = 0
				recoveryBackoff = 1
				continue
			}
			healthFailures++
			threshold := monitor.healthFailMax
			if threshold < 1 {
				threshold = 1
			}
			threshold *= recoveryBackoff
			if healthFailures < threshold {
				continue
			}
			healthFailures = 0
			if monitor.recoverVPN == nil {
				continue
			}
			// 每次恢复后若隧道仍不健康，下一次恢复要累积更多失败次数，
			// 直到任一次探测成功才重置，避免长期不可达时反复清空连接。
			if recoveryBackoff < maxHealthRecoveryBackoff {
				recoveryBackoff *= 2
			}
			recovery, err := monitor.recoverVPN(false)
			if err != nil {
				_, _ = fmt.Fprintf(monitor.logWriter, "VPN 健康检查连续失败，恢复失败: %v\n", err)
				continue
			}
			switch recovery {
			case vpnRecoverySwitched:
				_, _ = fmt.Fprintf(monitor.logWriter, "VPN 健康检查连续失败，已切换 OpenVPN remote 主动恢复连接\n")
			case vpnRecoveryRebuilt:
				_, _ = fmt.Fprintf(monitor.logWriter, "VPN 健康检查连续失败，已重建 OpenVPN 隧道主动恢复连接\n")
			default:
				_, _ = fmt.Fprintf(monitor.logWriter, "VPN 健康检查连续失败，仅清理滞留连接，无可切换 remote，等待 ping-restart 兜底\n")
			}
		case <-ticker.C:
			now := time.Now()
			elapsed := now.Sub(lastTick)
			lastTick = now
			route := monitor.defaultRoute()
			if monitor.wakeGap > 0 && elapsed > monitor.wakeGap {
				// 进程刚被睡眠唤醒：默认路由可能和睡前一样，重置基线并等待
				// 网络稳定，避免把「睡前醒后同一路由」误判为无需恢复。
				tracker = routeChangeTracker{}
				tracker.observe(route, monitor.stableSamples)
				wakeRetries = 0
				if wakeTimer != nil {
					wakeTimer.Stop()
				}
				wakeTimer = time.NewTimer(monitor.wakeSettleDelay)
				wakeTimerC = wakeTimer.C
				continue
			}
			if !tracker.observe(route, monitor.stableSamples) {
				continue
			}
			previous := tracker.baseline
			if err := monitor.closeConnections(); err != nil {
				_, _ = fmt.Fprintf(monitor.logWriter, "默认出口已从 %s 变为 %s，刷新连接失败: %v\n", previous, route, err)
				continue
			}
			tracker.accept(route)
			_, _ = fmt.Fprintf(monitor.logWriter, "默认出口已从 %s 变为 %s，已刷新旧连接\n", previous, route)
			if targetedTimer != nil {
				targetedTimer.Stop()
			}
			targetedTimer = time.NewTimer(monitor.tencentRefreshDelay)
			targetedTimerC = targetedTimer.C
		}
	}
}

// recoverAfterWake 在网络稳定后做一次短超时 VPN 健康检查；失败时清理旧连接
// 并按需切换 OpenVPN remote。返回 false 表示网络仍未就绪，需要再等一轮。
func (m routeMonitor) recoverAfterWake() bool {
	route := m.defaultRoute()
	if !validDefaultRoute(route) {
		return false
	}
	if m.checkVPN != nil && m.checkVPN() {
		_, _ = fmt.Fprintf(m.logWriter, "系统从睡眠唤醒，VPN 连接正常\n")
		return true
	}
	if m.recoverVPN == nil {
		return true
	}
	recovery, err := m.recoverVPN(true)
	if err != nil {
		_, _ = fmt.Fprintf(m.logWriter, "系统从睡眠唤醒，恢复 VPN 失败: %v\n", err)
		return false
	}
	if m.checkVPN != nil && !m.checkVPN() {
		_, _ = fmt.Fprintf(m.logWriter, "系统从睡眠唤醒，恢复后 VPN 仍未就绪\n")
		return false
	}
	switch recovery {
	case vpnRecoverySwitched:
		_, _ = fmt.Fprintf(m.logWriter, "系统从睡眠唤醒，已切换 OpenVPN remote 恢复连接\n")
	case vpnRecoveryRebuilt:
		_, _ = fmt.Fprintf(m.logWriter, "系统从睡眠唤醒，已重建 OpenVPN 隧道恢复连接\n")
	default:
		_, _ = fmt.Fprintf(m.logWriter, "系统从睡眠唤醒，已恢复 VPN 连接\n")
	}
	return true
}

func isTencentDirectConnection(connection controller.Connection) bool {
	direct := false
	for _, chain := range connection.Chains {
		if strings.EqualFold(chain, "DIRECT") {
			direct = true
			break
		}
	}
	if !direct {
		return false
	}

	process := strings.ToLower(connection.Metadata.Process + " " + connection.Metadata.ProcessPath)
	if strings.Contains(process, "wechat") || strings.Contains(process, "weixin") {
		return true
	}
	for _, host := range []string{connection.Metadata.Host, connection.Metadata.SniffHost} {
		if isTencentHost(host) {
			return true
		}
	}
	ip := net.ParseIP(strings.TrimSpace(connection.Metadata.DestinationIP)).To4()
	return ip != nil && ip[0] == 157 && (ip[1] == 148 || ip[1] == 255)
}

func isTencentHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	for _, suffix := range []string{
		"qq.com",
		"qq.com.cn",
		"qpic.cn",
		"qlogo.cn",
		"wechat.com",
		"tencent.com",
		"weixinbridge.com",
	} {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}

func startDNSRecoveryGuard(statePath string) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	pid := os.Getpid()
	startToken := system.ProcessStartToken(pid)
	if startToken == "" {
		return fmt.Errorf("无法确认日志进程身份")
	}
	cmd := exec.Command("sudo", "-n", executable, "_recover-dns-after-exit", statePath, strconv.Itoa(pid), startToken)
	cmd.Stdin = nil
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	ready := make(chan bool, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		ready <- scanner.Scan() && scanner.Text() == "ready"
	}()
	select {
	case ok := <-ready:
		if !ok {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return fmt.Errorf("DNS 恢复守护进程未就绪")
		}
		_ = stdout.Close()
		return cmd.Process.Release()
	case <-time.After(2 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return fmt.Errorf("DNS 恢复守护进程启动超时")
	}
}

// RunDNSRecoveryGuard runs as root and restores DNS after the exact launcher
// process exits. The narrow path and process checks keep this internal command
// from becoming a general privileged command runner.
func RunDNSRecoveryGuard(statePath string, parentPID int, parentStartToken string) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("DNS 恢复守护进程必须以 root 运行")
	}
	cleanPath, err := validateDNSRecoveryGuardArgs(statePath, parentPID, parentStartToken)
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stdout, "ready")
	waitLauncherExit(parentPID, parentStartToken, system.ProcessStateOf, 250*time.Millisecond)
	if err := recoverDNSAfterSessionExit(cleanPath, parentPID); err != nil {
		return fmt.Errorf("后台恢复 DNS: %w", err)
	}
	return nil
}

// waitLauncherExit 等到启动器确实退出才返回。ps 偶尔失败或超时（例如机器
// 负载很高）不能当成已退出：那样会在 VPN 仍在运行时恢复系统 DNS，内网域名
// 随即解析不了。没被回收的僵尸进程算已退出，它在 ps 里仍有原来的启动时间。
func waitLauncherExit(pid int, startToken string, state func(int, string) system.ProcessState, interval time.Duration) {
	for state(pid, startToken) != system.ProcessGone {
		time.Sleep(interval)
	}
}

func validateDNSRecoveryGuardArgs(statePath string, parentPID int, parentStartToken string) (string, error) {
	cleanPath := filepath.Clean(statePath)
	if !filepath.IsAbs(cleanPath) || filepath.Base(cleanPath) != "dns-state.json" || parentPID <= 1 || parentStartToken == "" {
		return "", fmt.Errorf("DNS 恢复守护参数无效")
	}
	return cleanPath, nil
}
