// Package engine 是 macOS 图形界面的后台引擎。GUI 启动引擎进程后，通过标准
// 输入输出以 JSON 行协议调用它；引擎在进程内直接复用 manager、controller、
// customrules 等核心包，与 TUI 共享同一份设置、PID 记录和 DNS 恢复点，因此
// 两个界面可以同时打开并看到同一个受管 Mihomo。
package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kadaliao/tizi/internal/appupdate"
	"github.com/kadaliao/tizi/internal/claude"
	"github.com/kadaliao/tizi/internal/claudeguard"
	"github.com/kadaliao/tizi/internal/config"
	"github.com/kadaliao/tizi/internal/configshare"
	"github.com/kadaliao/tizi/internal/controller"
	"github.com/kadaliao/tizi/internal/core"
	"github.com/kadaliao/tizi/internal/customrules"
	"github.com/kadaliao/tizi/internal/diagnostics"
	"github.com/kadaliao/tizi/internal/generator"
	"github.com/kadaliao/tizi/internal/manager"
	"github.com/kadaliao/tizi/internal/ovpn"
	"github.com/kadaliao/tizi/internal/system"
)

const (
	pollInterval     = 2 * time.Second
	logLinesDefault  = 500
	logLinesMax      = 5000
	installEventGap  = 200 * time.Millisecond
	updateTimeout    = 15 * time.Second
	longActionLimit  = 10 * time.Minute
	maxRequestLength = 1 << 20
	// minRateInterval 是计算网速的最短采样间隔。操作前后会立即刷新快照，
	// 间隔过短时一次计数跳变会被放大成尖峰，此时沿用上一次的速度。
	minRateInterval = time.Second
	// dnsPendingGrace 是「遗留 DNS 未恢复」持续多久才提示。启动时先保存
	// DNS 快照再写 PID，停止时先删 PID 再恢复 DNS，两段过渡都短于它。
	dnsPendingGrace = 3 * time.Second
	// updateCheckInterval 是重新检查新版本的间隔。桌面版常驻菜单栏，只在
	// 启动时检查的话，之后发布的版本会一直看不到。
	updateCheckInterval = 6 * time.Hour
)

// Engine 持有 GUI 会话期间的状态，除注明外由 mu 保护。快照只由 pollLoop
// 生成，网速基线和 DNS 提示计时只在那里读写。
type Engine struct {
	paths   config.Paths
	manager manager.Manager
	version string

	outMu sync.Mutex
	out   *json.Encoder

	refresh chan struct{}

	mu            sync.Mutex
	settings      config.Settings
	settingsStamp string
	busy          string
	profile       *ovpn.Profile
	profileStamp  string
	profileError  string
	update        Update
	share         *configshare.Session
	installing    bool
	// updateDone 在有版本查询进行时非空，查询结束时关闭。
	updateDone chan struct{}

	// settingsMu 串行化设置的读、改、存，避免并发补丁互相覆盖。
	settingsMu sync.Mutex

	installPhase      atomic.Int32
	installDownloaded atomic.Int64
	installTotal      atomic.Int64

	previous        []controller.Connection
	previousAt      time.Time
	previousRates   map[string]controller.Rate
	dnsPendingSince time.Time

	// claudeMu 保护 claudeTracker。检测只在 claudeLoop 里进行。
	claudeMu      sync.Mutex
	claudeTracker *claudeguard.Tracker
	// claudeKick 让 claudeLoop 立即检测一次。
	claudeKick chan struct{}
	// claudeEvents 是 Mihomo 日志里与 Claude 有关的失败和直连。
	claudeEvents chan claudeguard.LogEvent
	// lastRunning、lastExited 是上一次快照里受管 Mihomo 的状态，只在
	// pollLoop 里读写；变化时立即检测 Claude，Mihomo 意外退出几秒内就能告警。
	lastRunning bool
	lastExited  bool
	// observeClaude 默认真实检测；测试替换它以避免访问网络。
	observeClaude func(ctx context.Context, env claudeguard.Env) claudeguard.Status

	// checkForUpdate 默认查询 GHE 最新 Release；测试替换它以避免访问网络。
	checkForUpdate func(ctx context.Context, current string) (appupdate.Status, error)
	// updateEvery 是 updateLoop 的检查间隔，测试时缩短。
	updateEvery time.Duration
}

// New 创建引擎；settings 必须已经包含 Controller 密钥。
func New(paths config.Paths, settings config.Settings, version string, out io.Writer) *Engine {
	encoder := json.NewEncoder(out)
	encoder.SetEscapeHTML(false)
	return &Engine{
		paths:          paths,
		manager:        manager.Manager{Paths: paths},
		version:        version,
		out:            encoder,
		refresh:        make(chan struct{}, 1),
		settings:       settings,
		settingsStamp:  fileStamp(paths.Settings),
		checkForUpdate: appupdate.Check,
		updateEvery:    updateCheckInterval,
		claudeTracker:  claudeguard.NewTracker(),
		claudeKick:     make(chan struct{}, 1),
		claudeEvents:   make(chan claudeguard.LogEvent, 16),
		observeClaude:  claudeguard.Observe,
	}
}

// Serve 读取请求直到 in 关闭（GUI 退出）。它不会停止 Mihomo：是否断开由
// GUI 在退出前明确调用 stop 决定，与 TUI 的「保持运行退出」一致。
func (e *Engine) Serve(ctx context.Context, in io.Reader) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var workers sync.WaitGroup
	workers.Add(4)
	go func() { defer workers.Done(); e.pollLoop(ctx) }()
	go func() { defer workers.Done(); e.updateLoop(ctx) }()
	go func() { defer workers.Done(); e.claudeLoop(ctx) }()
	go func() { defer workers.Done(); e.claudeLogLoop(ctx) }()

	lines := make(chan []byte)
	readErr := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(in)
		scanner.Buffer(make([]byte, 64<<10), maxRequestLength)
		for scanner.Scan() {
			line := append([]byte(nil), bytes.TrimSpace(scanner.Bytes())...)
			select {
			case lines <- line:
			case <-ctx.Done():
				return
			}
		}
		readErr <- scanner.Err()
	}()

	var requests sync.WaitGroup
	var err error
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case err = <-readErr:
			break loop
		case line := <-lines:
			if len(line) == 0 {
				continue
			}
			var request Request
			if decodeErr := json.Unmarshal(line, &request); decodeErr != nil {
				e.write(Response{Error: &Error{Code: CodeInvalidParams, Message: "无法解析请求: " + decodeErr.Error()}})
				continue
			}
			requests.Add(1)
			go func() {
				defer requests.Done()
				e.write(e.handle(ctx, request))
			}()
		}
	}
	// GUI 已经离开，还在等用户输入的密码框没人处理了：关掉它，让对应操作以
	// 授权失败结束，避免之后输入密码时在没有界面的情况下启动 VPN。
	system.CancelAuthorization()
	// 已经开始的启动、停止等操作要走完，避免 GUI 中途退出留下半套系统改动。
	requests.Wait()
	cancel()
	workers.Wait()
	e.closeShare()
	return err
}

func (e *Engine) write(value any) {
	e.outMu.Lock()
	defer e.outMu.Unlock()
	_ = e.out.Encode(value)
}

func (e *Engine) emit(event string, data any) {
	e.write(Event{Event: event, Data: data})
}

// requestRefresh 让 pollLoop 立即推送一次快照；已有待处理的刷新时合并。
func (e *Engine) requestRefresh() {
	select {
	case e.refresh <- struct{}{}:
	default:
	}
}

func (e *Engine) pollLoop(ctx context.Context) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		e.emit("snapshot", e.snapshot())
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-e.refresh:
		}
	}
}

type codedError struct {
	code    string
	message string
}

func (e codedError) Error() string { return e.message }

func failure(code, format string, args ...any) error {
	return codedError{code: code, message: fmt.Sprintf(format, args...)}
}

func (e *Engine) handle(ctx context.Context, request Request) Response {
	result, err := e.dispatch(ctx, request)
	if err != nil {
		var coded codedError
		if errors.As(err, &coded) {
			return Response{ID: request.ID, Error: &Error{Code: coded.code, Message: coded.message}}
		}
		if errors.Is(err, manager.ErrVPNConflict) {
			return Response{ID: request.ID, Error: &Error{Code: CodeVPNConflict, Message: err.Error()}}
		}
		return Response{ID: request.ID, Error: &Error{Code: CodeFailed, Message: err.Error()}}
	}
	if result == nil {
		result = struct{}{}
	}
	return Response{ID: request.ID, Result: result}
}

func decodeParams(raw json.RawMessage, target any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return failure(CodeInvalidParams, "参数无效: %v", err)
	}
	return nil
}

func (e *Engine) dispatch(ctx context.Context, request Request) (any, error) {
	switch request.Method {
	case "hello":
		return Hello{
			Version:         e.version,
			Protocol:        ProtocolVersion,
			ConfigDir:       e.paths.ConfigDir,
			SettingsFile:    e.paths.Settings,
			LogFile:         e.paths.LogFile,
			CustomRules:     e.customRulesPath(),
			Generated:       e.paths.Generated,
			ClaudeProxyFile: e.claudeProxyPath(),
			ProxiesFile:     e.proxiesPath(),
		}, nil
	case "refresh":
		e.requestRefresh()
		go e.checkUpdate(ctx, 0)
		return nil, nil
	case "start":
		var params struct {
			AllowConflict bool `json:"allowConflict"`
			StopOpenVPN   bool `json:"stopOpenVPN"`
		}
		if err := decodeParams(request.Params, &params); err != nil {
			return nil, err
		}
		return e.exclusive("启动 Mihomo", func() (any, error) { return e.start(params.AllowConflict, params.StopOpenVPN) })
	case "stop":
		return e.exclusive("停止 Mihomo", func() (any, error) {
			if err := e.manager.Stop(); err != nil {
				return nil, err
			}
			return ActionResult{Message: "Mihomo 已停止，系统 DNS 已恢复"}, nil
		})
	case "reload":
		return e.exclusive("重载 Mihomo", func() (any, error) {
			if err := e.requireConfigurable(); err != nil {
				return nil, err
			}
			pid, err := e.manager.Reload(e.currentSettings())
			if err != nil {
				return nil, err
			}
			return ActionResult{Message: fmt.Sprintf("已重新生成配置并重启 Mihomo，PID %d", pid)}, nil
		})
	case "validate":
		return e.exclusive("生成并校验配置", func() (any, error) {
			if err := e.requireConfigurable(); err != nil {
				return nil, err
			}
			if err := e.manager.Validate(e.currentSettings()); err != nil {
				return nil, err
			}
			return ActionResult{Message: "配置生成并通过 Mihomo 校验"}, nil
		})
	case "installCore":
		return e.exclusive("安装 Mihomo 核心", func() (any, error) { return e.installCore(ctx) })
	case "recoverDNS":
		return e.exclusive("恢复系统 DNS", func() (any, error) {
			restored, err := e.manager.RecoverLeftoverDNS()
			if err != nil {
				return nil, err
			}
			if !restored {
				return ActionResult{Message: "没有需要恢复的 DNS 设置"}, nil
			}
			return ActionResult{Message: "已恢复启动前的系统 DNS"}, nil
		})
	case "updateSettings":
		var patch settingsPatch
		if err := decodeParams(request.Params, &patch); err != nil {
			return nil, err
		}
		return e.updateSettings(patch)
	case "scanOVPN":
		paths := ovpn.Scan(e.currentSettings().OVPNPath)
		if paths == nil {
			paths = []string{}
		}
		return paths, nil
	case "selectPolicy":
		var params struct {
			Group  string `json:"group"`
			Choice string `json:"choice"`
		}
		if err := decodeParams(request.Params, &params); err != nil {
			return nil, err
		}
		settings := e.currentSettings()
		if err := controller.New(settings.ControllerPort, settings.ControllerSecret).Select(params.Group, params.Choice); err != nil {
			return nil, err
		}
		e.requestRefresh()
		e.kickClaude(true)
		return ActionResult{Message: fmt.Sprintf("%s 已切换为 %s", params.Group, params.Choice)}, nil
	case "diagnostics":
		checks := diagnostics.Run(e.paths, e.currentSettings())
		result := make([]DiagnosticCheck, 0, len(checks))
		for _, check := range checks {
			result = append(result, DiagnosticCheck{Level: string(check.Level), Name: check.Name, Detail: check.Detail})
		}
		return result, nil
	case "logs":
		var params struct {
			Limit int `json:"limit"`
		}
		if err := decodeParams(request.Params, &params); err != nil {
			return nil, err
		}
		limit := params.Limit
		if limit <= 0 {
			limit = logLinesDefault
		}
		lines := manager.TailLog(e.paths.LogFile, min(limit, logLinesMax))
		if lines == nil {
			lines = []string{}
		}
		return lines, nil
	case "testRoute":
		var params struct {
			Input string `json:"input"`
		}
		if err := decodeParams(request.Params, &params); err != nil {
			return nil, err
		}
		return e.testRoute(params.Input)
	case "rules":
		return e.rules()
	case "saveRule":
		var params struct {
			Input  string `json:"input"`
			Target string `json:"target"`
		}
		if err := decodeParams(request.Params, &params); err != nil {
			return nil, err
		}
		domain, err := customrules.DomainFromInput(params.Input)
		if err != nil {
			return nil, err
		}
		if err := customrules.Save(e.customRulesPath(), customrules.Rule{Domain: domain, Target: params.Target}); err != nil {
			return nil, err
		}
		return ActionResult{Message: fmt.Sprintf("已保存 %s -> %s；重载 Mihomo 后生效", domain, params.Target)}, nil
	case "shareStart":
		return e.exclusive("生成分享配置", e.startShare)
	case "shareStop":
		e.closeShare()
		e.requestRefresh()
		return ActionResult{Message: "配置分享已关闭"}, nil
	case "checkUpdate":
		// maxAge（秒）为 0 时总是重新查询；桌面版打开菜单、唤醒后的自动检查
		// 带上它，短时间内重复触发不会反复访问 GHE。
		var params struct {
			MaxAge int `json:"maxAge"`
		}
		if err := decodeParams(request.Params, &params); err != nil {
			return nil, err
		}
		if params.MaxAge < 0 {
			return nil, failure(CodeInvalidParams, "maxAge 不能为负数")
		}
		return e.checkUpdate(ctx, time.Duration(params.MaxAge)*time.Second), nil
	case "installUpdate":
		return e.exclusive("更新 APP", func() (any, error) { return e.installUpdate(ctx) })
	case "recoverWeChat":
		closed, err := e.manager.RecoverWeChatConnections()
		if err != nil {
			return nil, fmt.Errorf("恢复微信图片连接（已刷新 %d 条）: %w", closed, err)
		}
		return ActionResult{Message: fmt.Sprintf("已刷新 %d 条微信直连，Mihomo 保持运行", closed)}, nil
	case "claudeCheck":
		e.kickClaude(false)
		return ActionResult{Message: "正在重新检测 Claude 的线路"}, nil
	case "claudeProxyFile":
		return e.ensureClaudeProxyFile()
	case "proxiesFile":
		return e.ensureProxiesFile()
	case "parseClaudeAddress":
		var params struct {
			Text string `json:"text"`
		}
		if err := decodeParams(request.Params, &params); err != nil {
			return nil, err
		}
		node, err := claude.ParseAny(params.Text)
		if err != nil {
			return nil, failure(CodeInvalidParams, "%s", err.Error())
		}
		return ParsedClaudeNode{Scheme: node.Scheme, Host: node.Host, Port: node.Port, Username: node.Username, Password: node.Password}, nil
	case "setClaudeBlock":
		var params struct {
			Enabled bool `json:"enabled"`
		}
		if err := decodeParams(request.Params, &params); err != nil {
			return nil, err
		}
		return e.exclusive("配置直连保护", func() (any, error) { return e.setClaudeBlock(params.Enabled) })
	case "proxyCredentials":
		settings := e.currentSettings()
		return ProxyCredentials{Username: settings.ProxyUsername, Password: settings.ProxyPassword}, nil
	case "dashboardURL":
		settings := e.currentSettings()
		if !controller.New(settings.ControllerPort, settings.ControllerSecret).Healthy() {
			return nil, failure(CodeOffline, "Controller 不在线；启动 Mihomo 后才能打开面板")
		}
		return controller.DashboardURL(settings.ControllerPort, settings.ControllerSecret), nil
	default:
		return nil, failure(CodeUnknownMethod, "未知方法 %q", request.Method)
	}
}

// exclusive 串行执行会改变运行状态的操作，并在快照中标注进行中的操作，
// 对应 TUI 的 busy 状态。另一个进程（例如同时打开的 TUI）并发启动时，
// 由 manager 的启动锁兜底。
func (e *Engine) exclusive(label string, action func() (any, error)) (any, error) {
	e.mu.Lock()
	if e.busy != "" {
		current := e.busy
		e.mu.Unlock()
		return nil, failure(CodeBusy, "正在%s，请稍候", current)
	}
	e.busy = label
	e.mu.Unlock()
	e.requestRefresh()
	defer func() {
		e.mu.Lock()
		e.busy = ""
		e.mu.Unlock()
		e.requestRefresh()
		// 连接、断开、重载之后线路变了，立即重新检测，不沿用之前累计的失败。
		e.kickClaude(true)
	}()
	return action()
}

// currentSettings 返回最新设置；settings.json 被 TUI 或命令行改动后自动重新读取。
func (e *Engine) currentSettings() config.Settings {
	e.mu.Lock()
	defer e.mu.Unlock()
	stamp := fileStamp(e.paths.Settings)
	if stamp == e.settingsStamp {
		return e.settings
	}
	settings, err := config.Load(e.paths)
	if err != nil {
		// 读取失败（例如正在被其他进程改写）时沿用上一份，下次轮询重试。
		return e.settings
	}
	if settings.ControllerSecret == "" {
		if err := config.EnsureControllerSecret(e.paths, &settings); err != nil {
			return e.settings
		}
		stamp = fileStamp(e.paths.Settings)
	}
	e.settings = settings
	e.settingsStamp = stamp
	return e.settings
}

func (e *Engine) customRulesPath() string {
	if e.paths.CustomRules != "" {
		return e.paths.CustomRules
	}
	return filepath.Join(e.paths.ConfigDir, "custom-rules.txt")
}

// requireConfigurable 复刻 TUI 在生成配置前的引导检查。
func (e *Engine) requireConfigurable() error {
	if err := generator.CheckReady(e.paths.ConfigDir, e.currentSettings()); err != nil {
		return failure(CodeNotConfigured, "%s", err.Error())
	}
	if core.Find(e.paths) == "" {
		return failure(CodeCoreMissing, "尚未安装 Mihomo 核心；请先在「状态」中安装（不需要管理员权限）")
	}
	return nil
}

func (e *Engine) start(allowConflict, stopOpenVPN bool) (any, error) {
	if err := e.requireConfigurable(); err != nil {
		return nil, err
	}
	settings := e.currentSettings()
	if status := e.manager.Status(); status.Running {
		return nil, failure(CodeFailed, "Mihomo 已在运行，PID %d", status.PID)
	}
	stopped := 0
	if stopOpenVPN {
		count, err := e.manager.StopOtherOpenVPN()
		if err != nil {
			return nil, err
		}
		stopped = count
	} else if settings.TunEnabled && !allowConflict && system.InspectVPN().HasActiveOpenVPN() {
		return nil, failure(CodeVPNConflict, "检测到其他 OpenVPN 正在运行（例如 Tunnelblick）；同时接管网络可能导致内网或互联网不可用")
	}
	settings.AllowVPNConflict = allowConflict
	pid, err := e.manager.Start(settings)
	if err != nil {
		return nil, err
	}
	if stopped > 0 {
		return ActionResult{Message: fmt.Sprintf("已结束 %d 个 OpenVPN 进程，Mihomo 已启动，PID %d", stopped, pid)}, nil
	}
	return ActionResult{Message: fmt.Sprintf("Mihomo 已启动，PID %d", pid)}, nil
}

func (e *Engine) installCore(ctx context.Context) (any, error) {
	ctx, cancel := context.WithTimeout(ctx, longActionLimit)
	defer cancel()
	e.mu.Lock()
	e.installing = true
	e.mu.Unlock()
	e.installPhase.Store(int32(core.PhaseResolve))
	e.installDownloaded.Store(0)
	e.installTotal.Store(0)
	defer func() {
		e.mu.Lock()
		e.installing = false
		e.mu.Unlock()
	}()
	var lastEvent time.Time
	version, err := core.Install(ctx, e.paths, e.currentSettings().CoreChannel, func(phase core.Phase, downloaded, total int64) {
		e.installPhase.Store(int32(phase))
		e.installDownloaded.Store(downloaded)
		e.installTotal.Store(total)
		if now := time.Now(); now.Sub(lastEvent) >= installEventGap || (total > 0 && downloaded >= total) {
			lastEvent = now
			e.emit("install", e.installProgress())
		}
	})
	if err != nil {
		return nil, err
	}
	return ActionResult{Message: "Mihomo " + version + " 安装完成"}, nil
}

func (e *Engine) installProgress() InstallProgress {
	phase := "resolve"
	switch core.Phase(e.installPhase.Load()) {
	case core.PhaseCore:
		phase = "core"
	case core.PhaseGeoIP:
		phase = "geoip"
	}
	return InstallProgress{Phase: phase, Downloaded: e.installDownloaded.Load(), Total: e.installTotal.Load()}
}

// settingsPatch 中为 nil 的字段保持不变。列表类的内网设置按文本传入，
// 用逗号、空格或换行分隔。
type settingsPatch struct {
	Subscription        *string `json:"subscription"`
	IntranetDomains     *string `json:"intranetDomains"`
	IntranetCIDRs       *string `json:"intranetCIDRs"`
	IntranetDNS         *string `json:"intranetDNS"`
	IntranetHTTPProxy   *string `json:"intranetHTTPProxy"`
	IntranetProbeURL    *string `json:"intranetProbeURL"`
	TailscaleEnabled    *bool   `json:"tailscaleEnabled"`
	TailscaleAuthKey    *string `json:"tailscaleAuthKey"`
	TailscaleHostname   *string `json:"tailscaleHostname"`
	TailscaleControlURL *string `json:"tailscaleControlURL"`
	TailscaleExitNode   *string `json:"tailscaleExitNode"`

	OVPNPath   *string `json:"ovpnPath"`
	Remote     *string `json:"remote"`
	TunEnabled *bool   `json:"tunEnabled"`
	AllowLAN   *bool   `json:"allowLAN"`
	// ProxyAuth 为 true 时局域网代理要求认证：需要用户名（本次传入或已保存），
	// 没有密码时自动生成；为 false 时清除用户名和密码。
	ProxyAuth     *bool   `json:"proxyAuth"`
	ProxyUsername *string `json:"proxyUsername"`
	// ProxyPassword 为空字符串时重新生成密码。
	ProxyPassword  *string `json:"proxyPassword"`
	ChinaDirect    *bool   `json:"chinaDirect"`
	MixedPort      *int    `json:"mixedPort"`
	ControllerPort *int    `json:"controllerPort"`
	DNSPort        *int    `json:"dnsPort"`
	CoreChannel    *string `json:"coreChannel"`
	// ClaudeProxy 为空字符串时清除住宅节点地址。
	ClaudeProxy *string `json:"claudeProxy"`
	// ClaudeNode 按项设置住宅节点地址（桌面版的表单），不能和 ClaudeProxy
	// 同时传。
	ClaudeNode        *claudeNodePatch `json:"claudeNode"`
	ClaudeProxyDirect *bool            `json:"claudeProxyDirect"`
	ClaudeGuard       *bool            `json:"claudeGuard"`
}

// claudeNodePatch 是按项填写的住宅节点地址。Password 为 nil 时沿用已保存
// 的密码：界面拿不到它，留空表示不修改。
type claudeNodePatch struct {
	Scheme   string  `json:"scheme"`
	Host     string  `json:"host"`
	Port     int     `json:"port"`
	Username string  `json:"username"`
	Password *string `json:"password"`
}

// address 拼出要保存的地址。没有传密码时沿用 current 里的密码；用户名
// 留空时不带密码。
func (p claudeNodePatch) address(current string) (string, error) {
	node := claude.Address{Scheme: p.Scheme, Host: p.Host, Port: p.Port, Username: p.Username}
	switch {
	case p.Password != nil:
		node.Password = *p.Password
	case strings.TrimSpace(p.Username) != "":
		if previous, err := claude.SplitURL(current); err == nil {
			node.Password = previous.Password
		}
	}
	return node.URL()
}

func (e *Engine) updateSettings(patch settingsPatch) (any, error) {
	e.settingsMu.Lock()
	defer e.settingsMu.Unlock()
	settings := e.currentSettings()
	var profile *ovpn.Profile
	if patch.OVPNPath != nil && strings.TrimSpace(*patch.OVPNPath) == "" {
		// 空路径表示移除 .ovpn，不再连接内网。
		settings.OVPNPath = ""
		settings.Remote = ""
	} else if patch.OVPNPath != nil {
		path := filepath.Clean(strings.TrimSpace(*patch.OVPNPath))
		if !filepath.IsAbs(path) {
			return nil, failure(CodeInvalidParams, "OVPN 路径必须是绝对路径")
		}
		parsed, err := ovpn.ParseFile(path)
		if err != nil {
			return nil, err
		}
		profile = &parsed
		if path != settings.OVPNPath {
			// 与 TUI 一致：换了 OVPN 后原 remote 选择不再有意义。
			settings.Remote = ""
		}
		settings.OVPNPath = path
	}
	if patch.Subscription != nil {
		value := strings.TrimSpace(*patch.Subscription)
		if err := config.ValidateSubscription(value); err != nil {
			return nil, failure(CodeInvalidParams, "%s", err.Error())
		}
		settings.Subscription = value
	}
	for _, item := range []struct {
		value  *string
		target *[]string
	}{
		{patch.IntranetDomains, &settings.IntranetDomains},
		{patch.IntranetCIDRs, &settings.IntranetCIDRs},
		{patch.IntranetDNS, &settings.IntranetDNS},
	} {
		if item.value != nil {
			*item.target = config.SplitList(*item.value)
		}
	}
	settings.IntranetDomains = config.NormalizeDomains(settings.IntranetDomains)
	for _, item := range []struct {
		value  *string
		target *string
	}{
		{patch.IntranetHTTPProxy, &settings.IntranetHTTPProxy},
		{patch.IntranetProbeURL, &settings.IntranetProbeURL},
		{patch.TailscaleAuthKey, &settings.TailscaleAuthKey},
		{patch.TailscaleHostname, &settings.TailscaleHostname},
		{patch.TailscaleControlURL, &settings.TailscaleControlURL},
		{patch.TailscaleExitNode, &settings.TailscaleExitNode},
	} {
		if item.value != nil {
			*item.target = strings.TrimSpace(*item.value)
		}
	}
	if patch.TailscaleEnabled != nil {
		settings.TailscaleEnabled = *patch.TailscaleEnabled
	}
	if err := config.ValidateIntranet(settings); err != nil {
		return nil, failure(CodeInvalidParams, "%s", err.Error())
	}
	if err := config.ValidateTailscale(settings); err != nil {
		return nil, failure(CodeInvalidParams, "%s", err.Error())
	}
	if patch.Remote != nil {
		remote := strings.TrimSpace(*patch.Remote)
		if remote != "" {
			if profile == nil {
				parsed, err := ovpn.ParseFile(settings.OVPNPath)
				if err != nil {
					return nil, err
				}
				profile = &parsed
			}
			if _, err := profile.SelectRemotes(remote); err != nil {
				return nil, err
			}
		}
		settings.Remote = remote
	}
	if patch.TunEnabled != nil {
		settings.TunEnabled = *patch.TunEnabled
	}
	if patch.AllowLAN != nil {
		settings.AllowLAN = *patch.AllowLAN
	}
	if err := applyProxyAuth(&settings, patch); err != nil {
		return nil, err
	}
	if patch.ChinaDirect != nil {
		settings.ChinaDirect = *patch.ChinaDirect
	}
	for _, port := range []struct {
		value  *int
		target *int
	}{
		{patch.MixedPort, &settings.MixedPort},
		{patch.ControllerPort, &settings.ControllerPort},
		{patch.DNSPort, &settings.DNSPort},
	} {
		if port.value == nil {
			continue
		}
		if *port.value < 1 || *port.value > 65535 {
			return nil, failure(CodeInvalidParams, "端口必须是 1 到 65535 之间的整数")
		}
		*port.target = *port.value
	}
	if patch.CoreChannel != nil {
		channel, err := config.ParseCoreChannel(strings.ToLower(strings.TrimSpace(*patch.CoreChannel)))
		if err != nil {
			return nil, err
		}
		settings.CoreChannel = channel
	}
	if patch.ClaudeProxy != nil && patch.ClaudeNode != nil {
		return nil, failure(CodeInvalidParams, "claudeProxy 和 claudeNode 只能传一个")
	}
	if patch.ClaudeProxy != nil {
		settings.ClaudeProxy = strings.TrimSpace(*patch.ClaudeProxy)
	}
	if patch.ClaudeNode != nil {
		address, err := patch.ClaudeNode.address(settings.ClaudeProxy)
		if err != nil {
			return nil, failure(CodeInvalidParams, "%s", err.Error())
		}
		settings.ClaudeProxy = address
	}
	nodeChanged := patch.ClaudeProxy != nil || patch.ClaudeNode != nil
	if patch.ClaudeProxyDirect != nil {
		settings.ClaudeProxyDirect = *patch.ClaudeProxyDirect
	}
	if patch.ClaudeGuard != nil {
		settings.ClaudeGuard = *patch.ClaudeGuard
	}
	if nodeChanged || patch.ClaudeProxyDirect != nil {
		// 保存前就拦下生成配置时才会报的错：地址无效、与节点文件重复、
		// UDP 协议不能链式连接。
		node, err := claude.Load(e.paths.ConfigDir, settings)
		if err != nil {
			return nil, failure(CodeInvalidParams, "%s", err.Error())
		}
		// 本机节点（例如 SSH 端口转发）总是直接连接，设置里也记成直接连接，
		// 界面显示的就是实际的连接方式。
		if node != nil && claude.IsLocal(node.Server()) {
			if patch.ClaudeProxyDirect != nil && !*patch.ClaudeProxyDirect {
				return nil, failure(CodeInvalidParams, "住宅节点 %s 是本机地址，前置代理连不到它，只能本机直接连接", node.Server())
			}
			settings.ClaudeProxyDirect = true
		}
	}

	e.mu.Lock()
	err := config.Save(e.paths, settings)
	if err == nil {
		e.settings = settings
		e.settingsStamp = fileStamp(e.paths.Settings)
	}
	e.mu.Unlock()
	if err != nil {
		return nil, err
	}
	e.requestRefresh()
	if patch.ClaudeGuard != nil || nodeChanged {
		e.kickClaude(true)
	}
	message := "设置已保存"
	if e.manager.Status().Running {
		message += "；重载 Mihomo 后生效"
	}
	return ActionResult{Message: message}, nil
}

// applyProxyAuth 按 patch 修改局域网代理认证，规则与命令行的
// --proxy-user / --proxy-auth / --no-proxy-auth 一致。
func applyProxyAuth(settings *config.Settings, patch settingsPatch) error {
	if patch.ProxyAuth == nil && patch.ProxyUsername == nil && patch.ProxyPassword == nil {
		return nil
	}
	if patch.ProxyAuth != nil && !*patch.ProxyAuth {
		settings.ProxyUsername, settings.ProxyPassword = "", ""
		return nil
	}
	if patch.ProxyUsername != nil {
		username := strings.TrimSpace(*patch.ProxyUsername)
		if username == "" {
			return failure(CodeInvalidParams, "请填写代理用户名")
		}
		if err := config.CheckProxyUsername(username); err != nil {
			return failure(CodeInvalidParams, "%s", err.Error())
		}
		settings.ProxyUsername = username
	}
	if settings.ProxyUsername == "" {
		return failure(CodeInvalidParams, "请先填写代理用户名")
	}
	if patch.ProxyPassword != nil {
		password := strings.TrimSpace(*patch.ProxyPassword)
		if err := config.CheckProxyPassword(password); err != nil {
			return failure(CodeInvalidParams, "%s", err.Error())
		}
		settings.ProxyPassword = password
	}
	if settings.ProxyPassword == "" {
		password, err := config.NewProxyPassword()
		if err != nil {
			return err
		}
		settings.ProxyPassword = password
	}
	return nil
}

func (e *Engine) testRoute(input string) (any, error) {
	domain, port, err := customrules.DestinationFromInput(input)
	if err != nil {
		return nil, err
	}
	settings := e.currentSettings()
	client := controller.New(settings.ControllerPort, settings.ControllerSecret)
	if !client.Healthy() {
		return nil, failure(CodeOffline, "Controller 不在线；启动 Mihomo 后才能测试实际路线")
	}
	connection, err := client.ProbeRoute(settings.MixedPort, domain, port)
	if err != nil {
		return nil, fmt.Errorf("实际路线测试失败: %w", err)
	}
	result := RouteTest{
		Host:  domain,
		IP:    connection.Metadata.DestinationIP,
		Route: controller.Route(connection.Chains),
		Found: connection.Rule != "" || len(connection.Chains) > 0,
	}
	if connection.Rule != "" {
		result.Rule = connection.Rule
		if connection.Payload != "" {
			result.Rule += "," + connection.Payload
		}
	}
	return result, nil
}

func (e *Engine) rules() (any, error) {
	loaded, err := customrules.Load(e.customRulesPath())
	if err != nil {
		return nil, err
	}
	result := Rules{Rules: make([]CustomRule, 0, len(loaded)), Targets: customrules.DomainTargets}
	for _, rule := range loaded {
		result.Rules = append(result.Rules, CustomRule{Domain: rule.Domain, Target: rule.Target})
	}
	return result, nil
}

func (e *Engine) startShare() (any, error) {
	settings := e.currentSettings()
	if err := generator.CheckReady(e.paths.ConfigDir, settings); err != nil {
		return nil, failure(CodeNotConfigured, "%s", err.Error())
	}
	shareConfig, err := e.manager.GenerateShare(settings)
	if err != nil {
		return nil, err
	}
	session, err := configshare.Start(shareConfig)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	previous := e.share
	e.share = session
	e.mu.Unlock()
	if previous != nil {
		_ = previous.Close()
	}
	go func() {
		<-session.Done()
		e.mu.Lock()
		if e.share == session {
			e.share = nil
		}
		e.mu.Unlock()
		e.requestRefresh()
	}()
	return Share{URL: session.URL, ExpiresAt: session.ExpiresAt.UnixMilli()}, nil
}

func (e *Engine) closeShare() {
	e.mu.Lock()
	session := e.share
	e.share = nil
	e.mu.Unlock()
	if session != nil {
		_ = session.Close()
	}
}

// updateLoop 启动时检查一次新版本，之后每隔 updateEvery 再检查一次。
func (e *Engine) updateLoop(ctx context.Context) {
	ticker := time.NewTicker(e.updateEvery)
	defer ticker.Stop()
	for {
		e.checkUpdate(ctx, 0)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// checkUpdate 查询最新版本并返回结果。maxAge > 0 时，距上次成功检查不到
// maxAge 就直接返回已知结果；按墙上时间算，睡眠的时间也算在内。已有查询在
// 进行时等它结束并沿用它的结果。查询失败时保留已知的新版本，只记录错误，
// 避免网络抖动让「更新 APP」入口消失。
func (e *Engine) checkUpdate(ctx context.Context, maxAge time.Duration) Update {
	e.mu.Lock()
	if maxAge > 0 && e.update.CheckedAt != 0 {
		// UnixMilli 得到的时间不带单调时钟读数，Since 因此按墙上时间计算。
		if age := time.Since(time.UnixMilli(e.update.CheckedAt)); age >= 0 && age < maxAge {
			defer e.mu.Unlock()
			return e.update
		}
	}
	if pending := e.updateDone; pending != nil {
		e.mu.Unlock()
		select {
		case <-pending:
		case <-ctx.Done():
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		return e.update
	}
	done := make(chan struct{})
	e.updateDone = done
	e.mu.Unlock()

	checkCtx, cancel := context.WithTimeout(ctx, updateTimeout)
	status, err := e.checkForUpdate(checkCtx, e.version)
	cancel()

	e.mu.Lock()
	e.updateDone = nil
	close(done)
	e.update.Checked = true
	if err != nil {
		e.update.Error = err.Error()
	} else {
		e.update.Error = ""
		e.update.CheckedAt = time.Now().UnixMilli()
		e.update.Latest = status.Latest
		e.update.Available = status.Available && status.Latest != e.update.Installed
	}
	update := e.update
	e.mu.Unlock()
	e.requestRefresh()
	return update
}

func (e *Engine) installUpdate(ctx context.Context) (any, error) {
	e.mu.Lock()
	update := e.update
	e.mu.Unlock()
	if !update.Available || update.Latest == "" {
		return nil, failure(CodeFailed, "没有可安装的新版本")
	}
	ctx, cancel := context.WithTimeout(ctx, longActionLimit)
	defer cancel()
	result, err := appupdate.Install(ctx, update.Latest)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	e.update.Available = false
	e.update.Installed = update.Latest
	e.mu.Unlock()
	message := "APP 已更新到 " + update.Latest + "；重启后台引擎后生效"
	if result.DesktopError != "" {
		message += "。" + result.DesktopNote()
	}
	return UpdateResult{Message: message, Version: update.Latest, DesktopApp: result.DesktopApp}, nil
}

func (e *Engine) snapshot() Snapshot {
	settings := e.currentSettings()
	status := e.manager.Status()
	if status.Running != e.lastRunning || status.Exited != e.lastExited {
		e.lastRunning, e.lastExited = status.Running, status.Exited
		e.kickClaude(false)
	}
	vpn := system.InspectVPN()
	client := controller.New(settings.ControllerPort, settings.ControllerSecret)
	result := Snapshot{
		At: time.Now().UnixMilli(),
		Managed: Managed{
			Running:       status.Running,
			PID:           status.PID,
			Privileged:    status.Privileged,
			CoreInstalled: status.CorePath != "",
			CoreVersion:   status.CoreVersion,
		},
		Conflicts: Conflicts{Tunnelblick: len(vpn.Tunnelblick), OpenVPN: len(vpn.OpenVPN)},
		Settings:  settingsView(settings),
		Profile:   e.profileView(settings.OVPNPath),
		Groups:    []Group{},
		Claude:    e.claudeView(),
	}
	e.addClaudeSettings(&result.Settings, settings)
	e.addRouteSettings(&result.Settings, settings)
	e.mu.Lock()
	busy := e.busy
	e.mu.Unlock()
	pending := false
	if !status.Running && busy == "" {
		_, err := os.Stat(e.paths.DNSState)
		pending = err == nil
	}
	if !pending {
		e.dnsPendingSince = time.Time{}
	} else if e.dnsPendingSince.IsZero() {
		e.dnsPendingSince = time.Now()
	}
	result.DNSRecoveryPending = pending && time.Since(e.dnsPendingSince) >= dnsPendingGrace

	var connections []controller.Connection
	var sampledAt time.Time
	result.ControllerOnline = client.Healthy()
	if result.ControllerOnline {
		if groups, err := client.Groups(); err == nil {
			for _, group := range groups {
				result.Groups = append(result.Groups, Group{Name: group.Name, Now: group.Now, Choices: group.Choices})
			}
		}
		if current, err := client.Connections(); err == nil {
			connections = current
			sampledAt = time.Now()
		}
	}
	result.Connections = e.connectionViews(connections, sampledAt, &result.Traffic)

	e.mu.Lock()
	result.Busy = e.busy
	result.Update = e.update
	if e.share != nil {
		result.Share = &Share{URL: e.share.URL, ExpiresAt: e.share.ExpiresAt.UnixMilli()}
	}
	installing := e.installing
	e.mu.Unlock()
	if installing {
		progress := e.installProgress()
		result.Install = &progress
	}
	return result
}

// connectionViews 把本次连接列表与上一次采样对比得出实时网速，并更新采样基线。
// 距上次采样不足 minRateInterval 时沿用上一次的速度、不推进基线。
func (e *Engine) connectionViews(connections []controller.Connection, sampledAt time.Time, traffic *Traffic) []Connection {
	var rates map[string]controller.Rate
	switch {
	case sampledAt.IsZero():
		e.previous, e.previousAt, e.previousRates = nil, time.Time{}, nil
	case !e.previousAt.IsZero() && sampledAt.Sub(e.previousAt) < minRateInterval:
		rates = e.previousRates
	default:
		if !e.previousAt.IsZero() {
			rates = controller.Rates(e.previous, connections, sampledAt.Sub(e.previousAt))
		}
		e.previous, e.previousAt, e.previousRates = connections, sampledAt, rates
	}

	views := make([]Connection, 0, len(connections))
	for _, connection := range connections {
		view := Connection{
			ID:          connection.ID,
			Destination: connection.Destination(),
			Process:     connection.Metadata.Process,
			Route:       controller.Route(connection.Chains),
			Download:    connection.Download,
			Upload:      connection.Upload,
		}
		if connection.Rule != "" {
			view.Rule = connection.Rule
			if connection.Payload != "" {
				view.Rule += "," + connection.Payload
			}
		}
		if rate, ok := rates[connection.ID]; ok {
			view.DownloadRate, view.UploadRate, view.Sampled = rate.Download, rate.Upload, true
			traffic.Download += rate.Download
			traffic.Upload += rate.Upload
		}
		views = append(views, view)
	}
	traffic.Connections = len(connections)
	return views
}

// profileView 只在 OVPN 文件变化时重新解析，避免每 2 秒重复读取证书。
func (e *Engine) profileView(path string) Profile {
	if strings.TrimSpace(path) == "" {
		return Profile{Remotes: []string{}}
	}
	stamp := fileStamp(path)
	e.mu.Lock()
	defer e.mu.Unlock()
	if stamp == "" {
		e.profile, e.profileStamp, e.profileError = nil, "", "无法读取 OVPN 文件"
	} else if stamp != e.profileStamp {
		e.profileStamp = stamp
		if parsed, err := ovpn.ParseFile(path); err != nil {
			e.profile, e.profileError = nil, err.Error()
		} else {
			e.profile, e.profileError = &parsed, ""
		}
	}
	view := Profile{Loaded: e.profile != nil, Error: e.profileError, Remotes: []string{}}
	if e.profile != nil {
		for _, remote := range e.profile.Remotes {
			view.Remotes = append(view.Remotes, remote.String())
		}
	}
	return view
}

func settingsView(settings config.Settings) Settings {
	return Settings{
		Subscription:        config.MaskSubscription(settings.Subscription),
		HasSubscription:     settings.Subscription != "",
		IntranetDomains:     nonNil(settings.IntranetDomains),
		IntranetCIDRs:       nonNil(settings.IntranetCIDRs),
		IntranetDNS:         nonNil(settings.IntranetDNS),
		IntranetHTTPProxy:   settings.IntranetHTTPProxy,
		IntranetProbeURL:    settings.IntranetProbeURL,
		Intranet:            IntranetView{Domains: []string{}, CIDRs: []string{}, DNS: []string{}},
		TailscaleEnabled:    settings.TailscaleEnabled,
		TailscaleAuthKey:    config.MaskSecret(settings.TailscaleAuthKey),
		TailscaleHasAuthKey: settings.TailscaleAuthKey != "",
		TailscaleHostname:   settings.TailscaleHostname,
		TailscaleDeviceName: generator.TailscaleHostname(settings),
		TailscaleControlURL: settings.TailscaleControlURL,
		TailscaleExitNode:   settings.TailscaleExitNode,
		OVPNPath:            settings.OVPNPath,
		Remote:              settings.Remote,
		TunEnabled:          settings.TunEnabled,
		AllowLAN:            settings.AllowLAN,
		ProxyUsername:       settings.ProxyUsername,
		ProxyAuth:           settings.ProxyAuthEnabled(),
		HasProxyPassword:    settings.ProxyPassword != "",
		ChinaDirect:         settings.ChinaDirect,
		MixedPort:           settings.MixedPort,
		ControllerPort:      settings.ControllerPort,
		DNSPort:             settings.DNSPort,
		CoreChannel:         string(settings.CoreChannel),
	}
}

// fileStamp 用路径、修改时间和大小标识文件版本；文件不可读时返回空。
func fileStamp(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%s|%d|%d", path, info.ModTime().UnixNano(), info.Size())
}
