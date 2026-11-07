// Package claudeguard 监测 Claude 的实际访问线路：是否经过住宅节点（配置了
// 的话）或代理节点、有没有直连、Claude 看到的出口 IP 在哪里。
//
// 检测方式是经本机混合代理端口访问 https://claude.ai/cdn-cgi/trace：请求
// 和浏览器一样走 Mihomo 的分流规则，Mihomo 记录下实际的出口链；trace 由
// Cloudflare 边缘直接返回 Claude 看到的 IP 和地区，不会到达 Anthropic 的
// 服务器，也不带任何账号信息。
package claudeguard

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kadaliao/tizi/internal/claude"
	"github.com/kadaliao/tizi/internal/config"
	"github.com/kadaliao/tizi/internal/controller"
	"gopkg.in/yaml.v3"
)

// ProbeHost 是检测时访问的 Claude 域名。
const ProbeHost = "claude.ai"

// 状态级别。只有 LevelAlert 会弹窗。
const (
	LevelOff      = "off"      // 没有开启监测
	LevelChecking = "checking" // 还没有结论
	LevelIdle     = "idle"     // VPN 是用户自己断开的
	LevelOK       = "ok"
	LevelWarn     = "warn"
	LevelAlert    = "alert"
)

// 告警原因。界面按原因判断是否需要再次弹窗：同一个原因只弹一次，恢复正常
// 后才重新计。
const (
	KeyVPNExited      = "vpn_exited"
	KeyController     = "controller"
	KeyUnreachable    = "unreachable"
	KeyDirect         = "direct"
	KeyNotResidential = "not_residential"
	KeyRegion         = "region"
	KeyNoTUN          = "no_tun"
	KeyReloadNeeded   = "reload_needed"
	KeyOffline        = "offline"
	KeyBlockInactive  = "block_inactive"
)

const (
	// AlertThreshold 是同一告警连续出现多少次才生效：一次偶发失败只触发
	// 复查，复查仍失败才弹窗。Claude 的连接失败日志算作第一次。
	AlertThreshold = 2
	// CheckInterval 是一切正常时的检测间隔。断开、连接失败和直连不靠它
	// 发现：引擎看到 Mihomo 退出、日志里出现 Claude 的失败或直连时立即检测。
	CheckInterval = 30 * time.Second
	// ConfirmInterval 是第一次失败后多久复查。
	ConfirmInterval = 3 * time.Second
	// exitConfirmInterval 是发现 Mihomo 退出后多久复查。停止 VPN 时进程退出
	// 和删除 PID 记录之间有不到一秒的间隙，复查一次排除这种情况。
	exitConfirmInterval = time.Second
	// RecheckInterval 是告警期间的复查间隔，恢复后尽快转为正常。
	RecheckInterval = 10 * time.Second
	// eventCooldown 内的连接失败日志不触发检测：检测自己失败也会写日志。
	eventCooldown = 5 * time.Second
	// wakeGap 超过它说明进程刚从睡眠中恢复（正常间隔最多是检测间隔加一次
	// 检测的耗时），之前累计的失败不再算数。
	wakeGap = 90 * time.Second
	// wakeGrace 是唤醒后不弹「连不上」的时间：隧道和网络都在重建。
	wakeGrace = 30 * time.Second
	// traceTimeout 限定 TLS 握手和读取 trace 的总时间。
	traceTimeout = 10 * time.Second
)

// unsupportedRegions 是 Claude 不提供服务、出口落在这里就一定有问题的地区。
var unsupportedRegions = map[string]string{
	"CN": "中国大陆",
	"HK": "中国香港",
	"MO": "中国澳门",
}

// Status 是一次检测的结论。
type Status struct {
	Level string
	Key   string
	Title string
	// Detail 说明原因和处理办法，可以直接展示给用户。
	Detail string
	// Residential 表示运行中的配置为 Claude 配了住宅节点。
	Residential bool
	// Route 是 Claude 连接的出口链，例如「Claude -> 住宅节点」。
	Route   string
	ExitIP  string
	Country string
	// CheckedAt 是得出这个结论的时间。
	CheckedAt time.Time
	// Pending 是还没达到告警阈值的连续失败次数，大于 0 时正在复查。
	Pending int
}

// Prober 是检测需要的 Mihomo Controller 能力；controller.Client 实现了它。
type Prober interface {
	Groups() ([]controller.Group, error)
	OpenProbe(mixedPort int, host string, port int) (net.Conn, controller.Connection, error)
}

// Env 描述检测时的运行环境。
type Env struct {
	Enabled bool
	// Running 表示受管 Mihomo 正在运行；Exited 表示它不是经由停止操作退出的。
	Running bool
	Exited  bool
	// Offline 表示本机没有可用的网络（没有物理网卡的默认路由）。
	Offline bool
	// Blocked 表示装了直连保护（防火墙拦截不经 VPN 的 Claude 连接）；
	// BlockError 非空表示自检发现它没有生效。
	Blocked    bool
	BlockError string
	TunEnabled bool
	MixedPort  int
	Prober     Prober
	// Node 是设置里的住宅节点，只用于在失败时给出提示，可以为 nil。
	Node *claude.Node
	// Applied 是运行中配置里的住宅节点，读不到或没有时为 nil。
	Applied *claude.Node
}

// nodePending 报告住宅节点的设置改过但还没生效：运行中的 Mihomo 仍在用
// 旧的地址或连接方式。
func (env Env) nodePending() bool {
	return env.Node != nil && env.Applied != nil && !env.Node.Same(*env.Applied)
}

// Enabled 报告是否需要监测：配置了住宅节点时总是监测，否则看设置开关。
func Enabled(configDir string, settings config.Settings) bool {
	return settings.ClaudeGuard || claude.Configured(configDir, settings)
}

// readTrace 在已经建立的隧道上完成 TLS 握手并读取 /cdn-cgi/trace；测试时替换。
var readTrace = fetchTrace

// Observe 做一次检测。它不做去抖，交给 Tracker。
func Observe(ctx context.Context, env Env) Status {
	status := Status{CheckedAt: time.Now()}
	if !env.Enabled {
		status.Level = LevelOff
		status.Title = "未开启 Claude 访问监测"
		return status
	}
	protected := env.Blocked && env.BlockError == ""
	if env.Exited {
		if protected {
			return alert(status, KeyVPNExited, "VPN 意外断开",
				"Mihomo 已经退出。直连保护在拦截 Claude 的连接，不会直连，但 Claude 暂时打不开。请重新连接 VPN。")
		}
		return alert(status, KeyVPNExited, "VPN 意外断开",
			"Mihomo 已经退出，系统流量不再经过代理，现在访问 Claude 会直连并暴露本机 IP。请先重新连接 VPN，再使用 Claude。")
	}
	if !env.Running {
		status.Level = LevelIdle
		status.Title = "VPN 未连接"
		switch {
		case protected:
			status.Detail = "直连保护已开启：连接 VPN 之前 Claude 打不开，不会直连。"
		case env.Blocked:
			status.Detail = "直连保护没有生效（" + env.BlockError + "），现在访问 Claude 会直连。"
		default:
			status.Detail = "现在访问 Claude 会直连。"
		}
		return status
	}
	if env.Offline {
		// 整机断网时什么都连不上，也不会直连，不是 Claude 线路的问题。
		status.Level = LevelWarn
		status.Key = KeyOffline
		status.Title = "网络已断开"
		status.Detail = "本机没有可用的网络，Claude 暂时无法访问；网络恢复后会自动重新检测。"
		return status
	}
	groups, err := env.Prober.Groups()
	if err != nil {
		return alert(status, KeyController, "无法确认 Claude 的线路",
			"Mihomo 没有响应（"+err.Error()+"），无法确认 Claude 走哪条线路。可以重新连接 VPN。")
	}
	group, hasGroup := findGroup(groups, config.ClaudeGroup)
	status.Residential = hasGroup && containsString(group.Choices, config.ResidentialProxyName)
	exit := "代理节点"
	if status.Residential {
		exit = "住宅节点"
	}
	if hasGroup && len(group.Choices) == 1 && strings.EqualFold(group.Choices[0], "REJECT") {
		return alert(status, KeyUnreachable, "还没有可以访问 Claude 的节点",
			"Claude 不会直连，而现在既没有住宅节点，也没有订阅或节点文件里的代理节点，所以 Claude 打不开。请在「配置」页填写订阅地址或节点文件，然后重载 Mihomo。")
	}

	tunnel, connection, err := env.Prober.OpenProbe(env.MixedPort, ProbeHost, 443)
	if tunnel == nil {
		return unreachable(status, env, exit, err)
	}
	defer tunnel.Close()
	var chains []string
	if err == nil {
		chains = connection.Chains
		status.Route = controller.Route(chains)
	} else if hasGroup {
		// 没找到连接记录时，按策略组当前选择推断线路。
		status.Route = controller.ResolveRoute(groups, config.ClaudeGroup)
	}
	if containsFold(chains, "DIRECT") || strings.HasSuffix(status.Route, "-> DIRECT") {
		return alert(status, KeyDirect, "Claude 正在直连",
			"访问 Claude 的连接没有经过代理（线路："+status.Route+"），会暴露本机 IP。请重载 Mihomo，让新版本的 Claude 规则生效。")
	}
	if status.Residential && !usesResidential(chains, group) {
		return alert(status, KeyNotResidential, "Claude 没有经过住宅节点",
			"当前线路："+status.Route+"。如果不是你手动切换的，请在「策略」页把 Claude 切回「"+config.ResidentialProxyName+"」；重新连接或重载 Mihomo 后也会自动切回。")
	}

	ctx, cancel := context.WithTimeout(ctx, traceTimeout)
	defer cancel()
	trace, err := readTrace(ctx, tunnel, ProbeHost)
	if err != nil {
		return unreachable(status, env, exit, err)
	}
	status.ExitIP = trace.IP
	status.Country = trace.Country
	if region, ok := unsupportedRegions[status.Country]; ok {
		return alert(status, KeyRegion, "Claude 出口在不支持的地区",
			fmt.Sprintf("Claude 看到的出口 IP 是 %s（%s），Claude 不在这个地区提供服务，继续使用可能触发风控。请检查%s。", status.ExitIP, region, exit))
	}
	if env.Blocked && env.BlockError != "" {
		status.Level = LevelWarn
		status.Key = KeyBlockInactive
		status.Title = "直连保护没有生效"
		status.Detail = env.BlockError + "。VPN 断开时 Claude 可能直连；可以在「配置」页关闭再打开直连保护。"
		return status
	}
	if env.Node != nil && !status.Residential {
		status.Level = LevelWarn
		status.Key = KeyReloadNeeded
		status.Title = "住宅节点还没生效"
		status.Detail = "已经设置了住宅节点，但运行中的配置里还没有它，Claude 仍经代理节点访问。重载 Mihomo 后生效。"
		return status
	}
	if status.Residential && env.nodePending() {
		status.Level = LevelWarn
		status.Key = KeyReloadNeeded
		status.Title = "住宅节点的修改还没生效"
		status.Detail = pendingDetail(env) + "重载 Mihomo 后生效。"
		return status
	}
	if !env.TunEnabled {
		status.Level = LevelWarn
		status.Key = KeyNoTUN
		status.Title = "未接管系统流量"
		status.Detail = "只有设置了代理的应用会经" + exit + "访问 Claude，其他应用会直连。需要时在「配置」页打开 TUN。"
		return status
	}
	status.Level = LevelOK
	status.Title = "Claude 经" + exit + "访问正常"
	switch {
	case status.ExitIP != "" && status.Country != "":
		status.Detail = fmt.Sprintf("出口 IP %s（%s）", status.ExitIP, status.Country)
	case status.ExitIP != "":
		status.Detail = "出口 IP " + status.ExitIP
	default:
		status.Detail = "Claude 可以访问，但读不到出口 IP。"
	}
	return status
}

func alert(status Status, key, title, detail string) Status {
	status.Level = LevelAlert
	status.Key = key
	status.Title = title
	status.Detail = detail
	return status
}

// usesResidential 判断连接是否经过住宅节点。有连接记录时以出口链为准，
// 否则看「Claude」组当前的选择。
func usesResidential(chains []string, group controller.Group) bool {
	if len(chains) > 0 {
		return containsString(chains, config.ResidentialProxyName)
	}
	return group.Now == config.ResidentialProxyName
}

// unreachable 是连不上 Claude 时的告警。住宅节点的修改还没生效时，先让
// 用户重载：按旧设置连不上很正常，界面会在这个原因旁边放「重载」按钮。
func unreachable(status Status, env Env, exit string, err error) Status {
	title := "无法经" + exit + "访问 Claude"
	if status.Residential && env.nodePending() {
		return alert(status, KeyReloadNeeded, title,
			"检测失败："+describeError(err)+"。"+pendingDetail(env)+"请先重载 Mihomo。")
	}
	return alert(status, KeyUnreachable, title, unreachableDetail(err, env, status.Residential))
}

// pendingDetail 说明住宅节点哪里改了还没生效。
func pendingDetail(env Env) string {
	running, saved := env.Applied.Describe(), env.Node.Describe()
	if running == saved {
		return "住宅节点的协议、用户名或密码改过，运行中的 Mihomo 还在用旧设置。"
	}
	return "住宅节点的设置改过，运行中的 Mihomo 还在按旧设置（" + running + "）连接，新设置是" + saved + "。"
}

func unreachableDetail(err error, env Env, residential bool) string {
	parts := []string{"检测失败：" + describeError(err) + "。"}
	// 按运行中实际生效的节点给提示，读不到时才用设置里的。
	node := env.Applied
	if node == nil {
		node = env.Node
	}
	if residential && node != nil {
		if hint := node.Hint(); hint != "" {
			parts = append(parts, hint)
		}
	}
	if residential {
		parts = append(parts, "为避免出口 IP 来回切换，Claude 不会自动改走其他线路。急用时可以在「策略」页把 Claude 临时切到其他代理节点，重新连接或重载 Mihomo 后会自动切回住宅节点。")
	} else {
		parts = append(parts, "当前的代理节点可能暂时不可用，可以在「策略」页给 Claude 换一个节点，或稍后重试。")
	}
	return strings.Join(parts, "")
}

func describeError(err error) string {
	if err == nil {
		return "未知错误"
	}
	return strings.TrimSpace(err.Error())
}

// Trace 是 Cloudflare /cdn-cgi/trace 返回的出口信息。
type Trace struct {
	IP      string
	Country string
}

func fetchTrace(ctx context.Context, conn net.Conn, host string) (Trace, error) {
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	client := tls.Client(conn, &tls.Config{
		ServerName: host,
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"http/1.1"},
	})
	if err := client.HandshakeContext(ctx); err != nil {
		return Trace{}, fmt.Errorf("TLS 握手失败: %w", err)
	}
	request := "GET /cdn-cgi/trace HTTP/1.1\r\nHost: " + host + "\r\nUser-Agent: tizi\r\nAccept: text/plain\r\nConnection: close\r\n\r\n"
	if _, err := io.WriteString(client, request); err != nil {
		return Trace{}, fmt.Errorf("发送请求失败: %w", err)
	}
	response, err := http.ReadResponse(bufio.NewReader(client), nil)
	if err != nil {
		return Trace{}, fmt.Errorf("读取响应失败: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 16<<10))
	if err != nil {
		return Trace{}, fmt.Errorf("读取响应失败: %w", err)
	}
	// 能收到 HTTP 响应就说明线路是通的；Claude 以后不再用 Cloudflare 时，
	// 只是读不到出口 IP。
	if response.StatusCode != http.StatusOK {
		return Trace{}, nil
	}
	return ParseTrace(string(body)), nil
}

// ParseTrace 解析 trace 的 key=value 行。
func ParseTrace(body string) Trace {
	var trace Trace
	for _, line := range strings.Split(body, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "ip":
			trace.IP = value
		case "loc":
			trace.Country = strings.ToUpper(value)
		}
	}
	return trace
}

func findGroup(groups []controller.Group, name string) (controller.Group, bool) {
	for _, group := range groups {
		if group.Name == name {
			return group, true
		}
	}
	return controller.Group{}, false
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func containsFold(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(value, want) {
			return true
		}
	}
	return false
}

// Tracker 把检测结果和日志事件变成对外的状态：同一个告警要连续出现
// AlertThreshold 次才生效，没达到之前保留上一个结论并标出正在复查。不是
// 并发安全的，调用方加锁或只在一个 goroutine 里使用。
type Tracker struct {
	status     Status
	pendingKey string
	pending    int
	last       time.Time
	lastDone   time.Time
	graceUntil time.Time
}

// NewTracker 返回一个处于「检测中」的 Tracker。
func NewTracker() *Tracker {
	return &Tracker{status: Status{Level: LevelChecking, Title: checkingTitle}}
}

const checkingTitle = "正在检测 Claude 的线路…"

// Status 返回当前对外的状态。
func (t *Tracker) Status() Status {
	return t.status
}

// Update 记录一次检测结果并返回新的对外状态。
func (t *Tracker) Update(observed Status) Status {
	if !t.last.IsZero() && observed.CheckedAt.Sub(t.last) > wakeGap {
		t.pendingKey, t.pending = "", 0
		t.graceUntil = observed.CheckedAt.Add(wakeGrace)
	}
	t.last = observed.CheckedAt
	if observed.Level != LevelAlert {
		t.pendingKey, t.pending = "", 0
		t.status = observed
		return t.status
	}
	if t.status.Level == LevelAlert && t.status.Key == observed.Key {
		t.pendingKey, t.pending = "", 0
		t.status = observed
		return t.status
	}
	if observed.Key == t.pendingKey {
		t.pending++
	} else {
		t.pendingKey, t.pending = observed.Key, 1
	}
	// 断开和直连会暴露本机 IP，唤醒后也照常告警；连不上之类的问题在唤醒后
	// 的一小段时间里不告警，那时隧道本来就在重建。
	inGrace := observed.CheckedAt.Before(t.graceUntil) && observed.Key != KeyVPNExited && observed.Key != KeyDirect
	if t.pending >= AlertThreshold && !inGrace {
		t.pendingKey, t.pending = "", 0
		t.status = observed
		return t.status
	}
	t.status.Pending = t.pending
	return t.status
}

// Event 处理一条与 Claude 有关的日志事件，返回 true 表示应该立即检测一次。
// 看到直连立即告警，不必等检测确认：那条连接已经暴露了本机 IP。连接失败
// 算作一次失败，由随后的检测确认。
func (t *Tracker) Event(event LogEvent, now time.Time) bool {
	if event.Direct {
		status := Status{Residential: t.status.Residential, CheckedAt: now}
		t.pendingKey, t.pending = "", 0
		t.status = alert(status, KeyDirect, "Claude 正在直连",
			"刚刚有一条访问 "+event.Host+" 的连接没有经过代理，会暴露本机 IP。请重载 Mihomo；仍然出现时检查「策略」页和 custom-rules.txt。")
		return false
	}
	if t.status.Level == LevelAlert || now.Sub(t.lastDone) < eventCooldown {
		return false
	}
	if t.pendingKey != KeyUnreachable || t.pending == 0 {
		t.pendingKey, t.pending = KeyUnreachable, 1
	}
	return true
}

// Done 记录一次检测结束的时间。检测自己失败也会写进 Mihomo 日志，紧随其后
// 的失败事件因此不再触发检测。
func (t *Tracker) Done(now time.Time) {
	t.lastDone = now
}

// Reset 丢掉之前的结论和累计的失败，回到「检测中」；用于用户刚刚连接、
// 断开或切换线路之后，那时旧结论已经不再成立。
func (t *Tracker) Reset() {
	t.pendingKey, t.pending = "", 0
	t.status = Status{Level: LevelChecking, Title: checkingTitle}
}

// NextDelay 返回下一次检测前的等待时间。
func (t *Tracker) NextDelay() time.Duration {
	switch {
	case t.pending > 0 && t.pendingKey == KeyVPNExited:
		return exitConfirmInterval
	case t.pending > 0:
		return ConfirmInterval
	case t.status.Level == LevelAlert:
		return RecheckInterval
	default:
		return CheckInterval
	}
}

// LogEvent 是 Mihomo 日志里与 Claude 有关的一条记录。
type LogEvent struct {
	Host string
	// Direct 表示这条连接走了直连。
	Direct bool
	// Error 非空表示连接失败，内容是 Mihomo 给出的原因。
	Error string
}

// ParseLog 从一条 Mihomo 连接日志里找出与 Claude 有关的直连或失败；其他
// 日志返回 false。日志形如：
//
//	[TCP] 28.0.0.1:61309 --> claude.ai:443 match DomainSuffix(claude.ai) using Claude[住宅节点]
//	[TCP] dial Claude (match DomainSuffix/claude.ai) 28.0.0.1:61309 --> claude.ai:443 error: ...
func ParseLog(payload string) (LogEvent, bool) {
	_, rest, ok := strings.Cut(payload, " --> ")
	if !ok {
		return LogEvent{}, false
	}
	destination, _, _ := strings.Cut(rest, " ")
	host := destination
	if parsed, _, err := net.SplitHostPort(destination); err == nil {
		host = parsed
	}
	event := LogEvent{Host: host}
	isClaude := claude.DomainOf(host) != ""
	viaGroup := strings.Contains(payload, "dial "+config.ClaudeGroup+" (")
	if (isClaude || viaGroup) && strings.Contains(payload, "dial ") {
		if _, reason, found := strings.Cut(rest, " error: "); found {
			event.Error = strings.TrimSpace(reason)
			return event, true
		}
	}
	if isClaude && (strings.HasSuffix(payload, " using DIRECT") || strings.HasSuffix(payload, "[DIRECT]")) {
		event.Direct = true
		return event, true
	}
	return LogEvent{}, false
}

// Runtime 是运行中的 Mihomo 实际使用的配置，从生成的 config.yaml 读出；
// 设置改了但还没重载时，以它为准。
type Runtime struct {
	MixedPort  int
	TunEnabled bool
	Client     controller.Client
	// Residential 是运行中配置里的住宅节点定义，没有时为 nil。
	Residential map[string]any
}

// ReadRuntime 读取生成的 config.yaml。
func ReadRuntime(generated string) (Runtime, error) {
	data, err := os.ReadFile(generated)
	if err != nil {
		return Runtime{}, err
	}
	var parsed struct {
		MixedPort          int    `yaml:"mixed-port"`
		ExternalController string `yaml:"external-controller"`
		Secret             string `yaml:"secret"`
		Tun                struct {
			Enable bool `yaml:"enable"`
		} `yaml:"tun"`
		Proxies []map[string]any `yaml:"proxies"`
	}
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		return Runtime{}, err
	}
	host, portText, err := net.SplitHostPort(parsed.ExternalController)
	if err != nil || (host != "127.0.0.1" && host != "localhost") {
		return Runtime{}, fmt.Errorf("Controller 地址不是本机地址")
	}
	var port int
	if _, err := fmt.Sscanf(portText, "%d", &port); err != nil || port <= 0 || port > 65535 || parsed.MixedPort <= 0 {
		return Runtime{}, fmt.Errorf("配置里的端口无效")
	}
	runtime := Runtime{
		MixedPort:  parsed.MixedPort,
		TunEnabled: parsed.Tun.Enable,
		Client:     controller.New(port, parsed.Secret),
	}
	for _, proxy := range parsed.Proxies {
		if proxy["name"] == config.ResidentialProxyName {
			runtime.Residential = proxy
		}
	}
	return runtime, nil
}

// heartbeatName 是桌面版引擎在配置目录里定期更新的文件。后台进程看到它
// 足够新，就知道桌面版在负责弹窗，自己不再弹系统提醒。
const heartbeatName = "claude-guard-ui"

// heartbeatTTL 必须明显长于引擎的最长检测间隔加上一次检测的耗时。
const heartbeatTTL = 75 * time.Second

// TouchHeartbeat 记录桌面版正在负责 Claude 告警。
func TouchHeartbeat(configDir string) error {
	path := filepath.Join(configDir, heartbeatName)
	now := time.Now()
	if err := os.Chtimes(path, now, now); err == nil {
		return nil
	}
	return os.WriteFile(path, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o600)
}

// RemoveHeartbeat 在桌面版引擎退出时调用，后台进程立即接手。
func RemoveHeartbeat(configDir string) {
	_ = os.Remove(filepath.Join(configDir, heartbeatName))
}

// UIActive 报告桌面版是否正在负责 Claude 告警。
func UIActive(configDir string) bool {
	info, err := os.Stat(filepath.Join(configDir, heartbeatName))
	return err == nil && time.Since(info.ModTime()) < heartbeatTTL
}
