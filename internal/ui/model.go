package ui

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/kadaliao/tizi/internal/appupdate"
	"github.com/kadaliao/tizi/internal/claude"
	"github.com/kadaliao/tizi/internal/claudeblock"
	"github.com/kadaliao/tizi/internal/claudeguard"
	"github.com/kadaliao/tizi/internal/config"
	"github.com/kadaliao/tizi/internal/configshare"
	"github.com/kadaliao/tizi/internal/controller"
	"github.com/kadaliao/tizi/internal/core"
	"github.com/kadaliao/tizi/internal/customrules"
	"github.com/kadaliao/tizi/internal/diagnostics"
	"github.com/kadaliao/tizi/internal/generator"
	"github.com/kadaliao/tizi/internal/manager"
	"github.com/kadaliao/tizi/internal/nodes"
	"github.com/kadaliao/tizi/internal/ovpn"
	"github.com/kadaliao/tizi/internal/system"
)

const (
	tabStatus = iota
	tabConfig
	tabPolicy
	tabRules
	tabConnections
	tabLogs
	tabDiagnostics
	editRuleWebsite = -1
)

// 配置页字段下标，与 configFields 的顺序一一对应。
const (
	fieldSubscription = iota
	fieldNodes
	fieldOVPN
	fieldRemote
	fieldIntranetDomains
	fieldIntranetCIDRs
	fieldIntranetDNS
	fieldIntranetHTTP
	fieldIntranetProbe
	fieldTailscale
	fieldTailscaleAuthKey
	fieldTailscaleHostname
	fieldTailscaleControlURL
	fieldTailscaleExitNode
	fieldTun
	fieldAllowLAN
	fieldProxyUsername
	fieldProxyPassword
	fieldChinaDirect
	fieldClaudeProxy
	fieldClaudeProxyFile
	fieldClaudeProxyDirect
	fieldClaudeGuard
	fieldClaudeBlock
	fieldMixedPort
	fieldControllerPort
	fieldDNSPort
	fieldCoreChannel
)

var tabs = []string{"状态", "配置", "策略", "规则", "连接", "日志", "诊断"}

type snapshot struct {
	// loaded 表示这是一次真实的刷新结果，而不是界面刚打开时的空状态。
	loaded        bool
	managed       manager.Status
	vpn           system.VPNState
	healthy       bool
	groups        []controller.Group
	connections   []controller.Connection
	connectionsAt time.Time
	profile       *ovpn.Profile
	// profileStamp 标识 profile 解析自哪个文件版本（路径+mtime+大小），
	// 用于跳过未变化文件的重复解析。
	profileStamp string
	logs         []string
	// dnsLeftover 表示 Mihomo 没在运行，上次运行的 DNS 恢复点却还在。
	dnsLeftover bool
}

type snapshotMsg snapshot
type connectionRate = controller.Rate
type actionMsg struct {
	text string
	err  error
	// claudeReset 表示这次操作改变了 Claude 的线路，之前的检测结论作废。
	claudeReset bool
}
type administratorAuthorizationMsg struct {
	operation string
	settings  config.Settings
	err       error
}
type noticeMsg actionMsg
type diagnosticsMsg []diagnostics.Check
type appUpdateMsg struct {
	status appupdate.Status
	err    error
}
type appUpdateResultMsg struct {
	version string
	desktop string
	err     error
}
type tickMsg time.Time
type routeTestMsg struct {
	host   string
	ip     string
	rule   controller.Rule
	chains []string
	found  bool
	err    error
}
type configShareMsg struct {
	session *configshare.Session
	err     error
}
type configShareClosedMsg struct {
	session *configshare.Session
}

// claudeMsg 是一次 Claude 线路检测的结果；manual 表示是用户手动触发的。
type claudeMsg struct {
	status claudeguard.Status
	manual bool
}

type statusActionID int

const (
	statusStart statusActionID = iota
	statusValidate
	statusInstall
	statusShare
	statusUpdateApp
	statusReload
	statusRecoverDNS
	statusRecoverWeChat
	statusClaudeCheck
)

type statusAction struct {
	id    statusActionID
	label string
}

const (
	confirmForceStart  = "force-start"
	confirmStopOpenVPN = "stop-openvpn"
	confirmQuit        = "quit"
	confirmReinstall   = "reinstall"
)

type pickerKind int

const (
	pickerRemote pickerKind = iota
	pickerScan
)

// picker 是配置页的内联候选列表（remote 选择、扫描到的 OVPN）。
type picker struct {
	kind   pickerKind
	title  string
	values []string
	cursor int
}

// installState 由下载 goroutine 写入、渲染循环读取，跨 Model 拷贝共享。
type installState struct {
	active     atomic.Bool
	phase      atomic.Int32
	downloaded atomic.Int64
	total      atomic.Int64
}

type Model struct {
	paths              config.Paths
	settings           config.Settings
	manager            manager.Manager
	active             int
	cursor             int
	choice             int
	choosing           bool
	width              int
	height             int
	busy               bool
	help               bool
	darkBackground     bool
	editing            bool
	editField          int
	editBuffer         string
	editCursor         int
	pathCompletions    []string
	pathCompletion     int
	pathCompletionText string
	confirm            string
	message            string
	messageAt          time.Time
	errorText          string
	snapshot           snapshot
	diagnostics        []diagnostics.Check
	scannedOVPN        []string
	picker             *picker
	logScroll          int
	logSearch          searchBox
	connectionScroll   int
	connectionSearch   searchBox
	connectionRates    map[string]connectionRate
	// coreFound 缓存 core.Find 的结果，避免 View 每帧多次 stat + LookPath。
	// New 时同步探测一次，之后随每次状态快照更新。
	coreFound bool
	// keyInput 是当前正在处理的按键携带的可打印文本（tea.Key.Text），
	// 供编辑框和搜索框接受多 rune 的字素簇；只在 handleKey 调用链内有效。
	keyInput           string
	ruleInput          string
	ruleTarget         int
	ruleTested         bool
	ruleHost           string
	ruleIP             string
	ruleMatch          controller.Rule
	ruleChains         []string
	userRules          []customrules.Rule
	keepRunning        bool
	install            *installState
	share              *configshare.Session
	spinner            int
	appVersion         string
	latestAppVersion   string
	appUpdateAvailable bool
	appUpdateChecked   bool
	appUpdateError     string
	updatingApp        bool
	appUpdated         bool
	restartAfterUpdate bool
	// claude 是最近一次 Claude 线路检测的结论；开启监测时经 claudeTracker
	// 去抖（偶发失败先复查），没开启时只有手动检测的结果。
	claude         claudeguard.Status
	claudeTracker  *claudeguard.Tracker
	claudeChecking bool
	// claudeNext 是开启监测时下一次自动检测的时间；零值表示尽快检测。
	claudeNext time.Time
	// dnsPending 表示遗留的 DNS 恢复点已经持续了 dnsPendingGrace，需要提示；
	// 在快照到达时算好，保证同一快照下渲染和按键看到的操作列表一致。
	dnsPending      bool
	dnsPendingSince time.Time
}

// dnsPendingGrace 是「遗留 DNS 未恢复」持续多久才提示：另一个界面启动
// Mihomo 时会先保存 DNS 恢复点再写入 PID，中间短暂出现这种状态。
const dnsPendingGrace = 3 * time.Second

func New(paths config.Paths, settings config.Settings, version string) Model {
	version = strings.TrimSpace(version)
	if version == "" {
		version = "dev"
	}
	m := Model{
		paths:          paths,
		settings:       settings,
		manager:        manager.Manager{Paths: paths},
		scannedOVPN:    ovpn.Scan(settings.OVPNPath),
		install:        &installState{},
		darkBackground: true,
		appVersion:     version,
		coreFound:      core.Find(paths) != "",
	}
	m.userRules, _ = customrules.Load(m.customRulesPath())
	m.setMessage("正在读取本机状态...")
	return m
}

// KeepRunning 报告用户是否选择了退出 TUI 但保留 Mihomo 运行。
func (m Model) KeepRunning() bool {
	return m.keepRunning
}

// RestartAfterUpdate 报告用户是否要求在 APP 更新后换用新二进制：退出后由
// 新版本重载运行中的 Mihomo 并重新打开界面。
func (m Model) RestartAfterUpdate() bool {
	return m.restartAfterUpdate
}

func (m *Model) setMessage(text string) {
	m.message = text
	m.messageAt = time.Now()
}

// fail 结束后台操作并显示错误。
func (m *Model) fail(err error) {
	m.busy = false
	m.errorText = err.Error()
	m.message = ""
}

// finish 结束后台操作并显示结果消息。
func (m *Model) finish(text string) {
	m.busy = false
	m.setMessage(text)
	m.errorText = ""
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(refreshCmd(m), diagnosticsCmd(m), checkAppUpdateCmd(m.appVersion), tickCmd(false), requestBackgroundCmd)
}

func requestBackgroundCmd() tea.Msg {
	return tea.RequestBackgroundColor()
}

func (m Model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := message.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case snapshotMsg:
		oldGroup, oldChoice := "", ""
		if m.active == tabPolicy && m.cursor < len(m.snapshot.groups) {
			oldGroup = m.snapshot.groups[m.cursor].Name
			if m.choosing && m.choice < len(m.snapshot.groups[m.cursor].Choices) {
				oldChoice = m.snapshot.groups[m.cursor].Choices[m.choice]
			}
		}
		next := snapshot(msg)
		var oldAction statusActionID
		hadAction := false
		if m.active == tabStatus {
			if actions := m.statusActions(); m.cursor < len(actions) {
				oldAction, hadAction = actions[m.cursor].id, true
			}
		}
		previous := m.snapshot
		m.connectionRates = calculateConnectionRates(m.snapshot, next)
		m.snapshot = next
		m.coreFound = next.managed.CorePath != ""
		m.updateDNSPending(next.dnsLeftover, time.Now())
		if previous.loaded && next.loaded && (previous.managed.Running != next.managed.Running ||
			previous.managed.PID != next.managed.PID || previous.managed.Exited != next.managed.Exited) {
			m.claudeRouteChanged(false)
		}
		if m.active == tabStatus {
			m.cursor = m.statusCursorFor(oldAction, hadAction)
		}
		if m.active == tabPolicy {
			if len(m.snapshot.groups) == 0 {
				m.cursor, m.choice, m.choosing = 0, 0, false
			} else {
				foundGroup := false
				for i, group := range m.snapshot.groups {
					if group.Name == oldGroup {
						m.cursor, foundGroup = i, true
						break
					}
				}
				if !foundGroup {
					m.cursor, m.choice, m.choosing = 0, 0, false
				}
				choices := m.snapshot.groups[m.cursor].Choices
				if m.choosing {
					foundChoice := false
					for i, choice := range choices {
						if choice == oldChoice {
							m.choice, foundChoice = i, true
							break
						}
					}
					if !foundChoice {
						m.choice, m.choosing = 0, false
					}
				}
			}
		}
		m.connectionScroll = min(m.connectionScroll, max(0, len(m.filteredConnections())-m.connectionVisibleLines()))
		return m, nil
	case diagnosticsMsg:
		m.diagnostics = []diagnostics.Check(msg)
		return m, nil
	case appUpdateMsg:
		m.appUpdateChecked = true
		if msg.err != nil {
			m.appUpdateError = msg.err.Error()
			m.appUpdateAvailable = false
		} else {
			m.appUpdateError = ""
			m.latestAppVersion = msg.status.Latest
			m.appUpdateAvailable = msg.status.Available
		}
		return m, nil
	case appUpdateResultMsg:
		m.updatingApp = false
		if msg.err != nil {
			m.fail(msg.err)
			return m, nil
		}
		m.latestAppVersion = msg.version
		m.appUpdateAvailable = false
		m.appUpdateChecked = true
		m.appUpdateError = ""
		m.appUpdated = true
		text := "APP 已更新到 " + msg.version + "；按 r 用新版本重启界面，或下次启动生效"
		if m.snapshot.managed.Running {
			text = "APP 已更新到 " + msg.version + "；按 r 用新版本重启界面并重载 Mihomo"
		}
		if msg.desktop != "" {
			text += "。" + msg.desktop
		}
		m.finish(text)
		return m, nil
	case routeTestMsg:
		m.busy = false
		if msg.err != nil {
			m.fail(msg.err)
			return m, nil
		}
		m.ruleHost = msg.host
		m.ruleIP = msg.ip
		m.ruleMatch = msg.rule
		m.ruleChains = append([]string(nil), msg.chains...)
		if !msg.found {
			m.ruleMatch = controller.Rule{}
			m.ruleChains = nil
		}
		// An inconclusive lookup is still a completed test and must render a
		// result instead of silently returning to the untouched form.
		m.ruleTested = true
		m.errorText = ""
		return m, nil
	case actionMsg:
		if msg.err != nil {
			m.fail(msg.err)
		} else {
			m.finish(msg.text)
		}
		if msg.claudeReset {
			m.claudeRouteChanged(true)
		}
		return m, tea.Batch(refreshCmd(m), diagnosticsCmd(m))
	case claudeMsg:
		m.claudeChecking = false
		now := time.Now()
		if msg.manual && !claudeguard.Enabled(m.paths.ConfigDir, m.settings) {
			// 没开启监测时只有这一次检测，直接展示结果，不必去抖。
			m.claude = msg.status
		} else {
			if m.claudeTracker == nil {
				m.claudeTracker = claudeguard.NewTracker()
			}
			m.claude = m.claudeTracker.Update(msg.status)
			m.claudeTracker.Done(now)
			m.claudeNext = now.Add(m.claudeTracker.NextDelay())
		}
		if msg.manual {
			m.setMessage("Claude 线路检测完成：" + m.claude.Title)
		}
		return m, nil
	case configShareMsg:
		m.busy = false
		if msg.err != nil {
			m.fail(msg.err)
			return m, nil
		}
		m.share = msg.session
		m.errorText = ""
		return m, waitConfigShareCmd(msg.session)
	case configShareClosedMsg:
		if m.share == msg.session {
			m.share = nil
			m.setMessage("二维码已过期，配置分享已关闭")
		}
		return m, nil
	case administratorAuthorizationMsg:
		if msg.err != nil {
			m.fail(fmt.Errorf("管理员授权未完成: %w", msg.err))
			return m, nil
		}
		switch msg.operation {
		case "stop":
			return m, runStopCmd(m.manager)
		case "reload":
			return m, runReloadCmd(m.manager, msg.settings)
		case "recover-dns":
			return m, runRecoverDNSCmd(m.manager)
		case "claude-block-on", "claude-block-off":
			return m, claudeBlockCmd(msg.operation == "claude-block-on")
		case "stop-openvpn-and-start":
			return m, runStopOpenVPNAndStartCmd(m.manager, msg.settings)
		default:
			return m, runStartCmd(m.manager, msg.settings)
		}
	case noticeMsg:
		if msg.err != nil {
			m.errorText = msg.err.Error()
		} else {
			m.setMessage(msg.text)
			m.errorText = ""
		}
		return m, nil
	case tickMsg:
		if m.message != "" && !m.busy && time.Since(m.messageAt) > messageTTL {
			m.message = ""
		}
		if m.busy {
			// 后台操作期间不刷新状态，避免与启动/安装过程互相干扰；
			// 加快 tick 让安装进度及时重绘，并推进 spinner 证明界面存活。
			m.spinner++
			return m, tickCmd(true)
		}
		cmds := []tea.Cmd{refreshCmd(m), tickCmd(false)}
		if m.claudeDue(time.Now()) {
			m.claudeChecking = true
			cmds = append(cmds, claudeCheckCmd(m.manager, m.settings, false))
		}
		return m, tea.Batch(cmds...)
	case tea.BackgroundColorMsg:
		applyPalette(msg.IsDark())
		m.darkBackground = msg.IsDark()
		return m, nil
	case tea.PasteMsg:
		if m.active == tabConnections && m.connectionSearch.active {
			m.connectionSearch.appendText(msg.Content)
			m.connectionScroll = 0
			return m, nil
		}
		if m.active == tabLogs && m.logSearch.active {
			m.logSearch.appendText(msg.Content)
			m.logScroll = 0
			return m, nil
		}
		return m.handlePaste(msg.Content)
	case tea.KeyPressMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m Model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	m.keyInput = msg.Text
	if m.confirm != "" {
		return m.handleConfirm(key)
	}
	if m.share != nil {
		return m.handleShareKey(key)
	}
	// 搜索框是独占输入态，避免关键词触发刷新、跳页或退出等全局快捷键。
	if m.active == tabConnections && m.connectionSearch.active {
		return m.handleConnectionsKey(key)
	}
	if m.active == tabLogs && m.logSearch.active {
		return m.handleLogsKey(key)
	}
	if key == "ctrl+c" {
		return m.requestQuit()
	}
	if m.editing {
		return m.handleEdit(key)
	}
	if m.help {
		if key == "?" || key == "esc" || key == "q" {
			m.help = false
		}
		return m, nil
	}
	if m.picker != nil {
		return m.handlePicker(key)
	}
	if key == "q" {
		return m.requestQuit()
	}
	if key == "?" {
		m.help = true
		return m, nil
	}
	if key >= "1" && key <= "7" {
		m.active = int(key[0] - '1')
		m.resetViewState()
		return m, nil
	}
	switch key {
	case "left", "h":
		m.active = (m.active + len(tabs) - 1) % len(tabs)
		m.resetViewState()
		return m, nil
	case "right", "l":
		m.active = (m.active + 1) % len(tabs)
		m.resetViewState()
		return m, nil
	case "r":
		if m.busy {
			// 后台操作期间不刷新，与 tickMsg 的抑制保持一致，避免刷新
			// 清掉「更新 APP」行导致光标错位。
			return m, nil
		}
		if m.appUpdated {
			// 本进程仍是旧代码，在这里重载会用旧规则生成配置。退出后由
			// 新二进制重载 Mihomo 并重新打开界面；保持运行避免退出时被停止。
			m.restartAfterUpdate = true
			m.keepRunning = true
			return m, tea.Quit
		}
		m.setMessage("正在刷新...")
		m.claudeNext = time.Time{}
		m.appUpdateChecked = false
		m.appUpdateError = ""
		m.appUpdateAvailable = false
		return m, tea.Batch(refreshCmd(m), diagnosticsCmd(m), checkAppUpdateCmd(m.appVersion))
	case "d":
		if !m.snapshot.healthy {
			m.errorText = "Controller 不在线；启动 Mihomo 后才能打开面板"
			return m, nil
		}
		return m, openDashboardCmd(m.settings.ControllerPort, m.settings.ControllerSecret)
	}
	if m.busy {
		return m, nil
	}

	switch m.active {
	case tabStatus:
		return m.handleStatusKey(key)
	case tabConfig:
		return m.handleConfigKey(key)
	case tabPolicy:
		return m.handlePolicyKey(key)
	case tabRules:
		return m.handleRulesKey(key)
	case tabConnections:
		return m.handleConnectionsKey(key)
	case tabLogs:
		return m.handleLogsKey(key)
	case tabDiagnostics:
		if key == "enter" {
			return m, diagnosticsCmd(m)
		}
	}
	return m, nil
}

func (m Model) handleShareKey(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "esc", "enter", "space":
		_ = m.share.Close()
		m.share = nil
		m.setMessage("配置分享已关闭")
		return m, nil
	case "q", "ctrl+c":
		_ = m.share.Close()
		m.share = nil
		return m.requestQuit()
	}
	return m, nil
}

func (m *Model) resetViewState() {
	m.cursor, m.choice, m.choosing = 0, 0, false
	m.picker = nil
	m.logScroll = 0
	m.logSearch.active = false
	m.connectionScroll = 0
	m.connectionSearch.active = false
}

func (m Model) customRulesPath() string {
	if m.paths.CustomRules != "" {
		return m.paths.CustomRules
	}
	return filepath.Join(m.paths.ConfigDir, "custom-rules.txt")
}

// requestQuit 在受管 Mihomo 运行时先确认退出方式，否则直接退出。
func (m Model) requestQuit() (tea.Model, tea.Cmd) {
	if m.snapshot.managed.Running {
		m.confirm = confirmQuit
		return m, nil
	}
	return m, tea.Quit
}

func (m Model) handleConfirm(key string) (tea.Model, tea.Cmd) {
	switch m.confirm {
	case confirmQuit:
		switch key {
		case "s", "S", "enter", "ctrl+c":
			m.keepRunning = false
			return m, tea.Quit
		case "k", "K":
			m.keepRunning = true
			return m, tea.Quit
		case "n", "N", "esc", "q":
			m.confirm = ""
			m.setMessage("已取消退出")
		}
	case confirmForceStart:
		switch key {
		case "y", "Y":
			m.confirm = ""
			m.busy = true
			settings := m.settings
			settings.AllowVPNConflict = true
			return m, startCmd(m.manager, settings)
		case "k", "K":
			m.confirm = confirmStopOpenVPN
		case "n", "N", "esc":
			m.confirm = ""
			m.setMessage("已取消")
		}
	case confirmStopOpenVPN:
		switch key {
		case "y", "Y", "enter":
			m.confirm = ""
			m.busy = true
			return m, stopOpenVPNAndStartCmd(m.manager, m.settings)
		case "n", "N", "esc":
			m.confirm = ""
			m.setMessage("已取消")
		}
	case confirmReinstall:
		switch key {
		case "y", "Y":
			m.confirm = ""
			m.busy = true
			return m, installCmd(m.paths, m.settings.CoreChannel, m.install)
		case "n", "N", "esc":
			m.confirm = ""
			m.setMessage("已取消")
		}
	}
	return m, nil
}

func (m Model) handleStatusKey(key string) (tea.Model, tea.Cmd) {
	actions := m.statusActions()
	if m.cursor >= len(actions) {
		m.cursor = 0
	}
	switch key {
	case "up", "k":
		m.cursor = (m.cursor + len(actions) - 1) % len(actions)
	case "down", "j":
		m.cursor = (m.cursor + 1) % len(actions)
	case "enter", "space":
		m.message = ""
		m.errorText = ""
		switch actions[m.cursor].id {
		case statusStart:
			if m.snapshot.managed.Running {
				m.busy = true
				return m, stopCmd(m.manager, m.snapshot.managed.Privileged)
			}
			return m.startMihomo()
		case statusReload:
			return m.reloadMihomo()
		case statusRecoverDNS:
			m.busy = true
			return m, recoverDNSCmd(m.manager)
		case statusRecoverWeChat:
			m.busy = true
			return m, recoverWeChatCmd(m.manager)
		case statusClaudeCheck:
			return m.checkClaudeNow()
		case statusValidate:
			if generator.CheckReady(m.paths.ConfigDir, m.settings) != nil {
				return m.guideToSetup()
			}
			if core.Find(m.paths) == "" {
				return m.selectInstall()
			}
			m.busy = true
			return m, validateCmd(m.manager, m.settings)
		case statusInstall:
			if m.snapshot.managed.Running {
				m.confirm = confirmReinstall
				return m, nil
			}
			m.busy = true
			return m, installCmd(m.paths, m.settings.CoreChannel, m.install)
		case statusShare:
			if generator.CheckReady(m.paths.ConfigDir, m.settings) != nil {
				return m.guideToSetup()
			}
			m.busy = true
			return m, shareConfigCmd(m.manager, m.settings)
		case statusUpdateApp:
			m.busy = true
			m.updatingApp = true
			return m, updateAppCmd(m.latestAppVersion)
		}
	}
	return m, nil
}

// startMihomo 检查线路、核心和其他 OpenVPN 后启动 Mihomo。
func (m Model) startMihomo() (tea.Model, tea.Cmd) {
	if generator.CheckReady(m.paths.ConfigDir, m.settings) != nil {
		return m.guideToSetup()
	}
	if core.Find(m.paths) == "" {
		return m.selectInstall()
	}
	if m.settings.TunEnabled && m.snapshot.vpn.HasActiveOpenVPN() {
		m.confirm = confirmForceStart
		return m, nil
	}
	m.busy = true
	return m, startCmd(m.manager, m.settings)
}

// reloadMihomo 重新生成配置并重启 Mihomo，让设置和自定义规则的修改生效；
// 没在运行时按启动处理。
func (m Model) reloadMihomo() (tea.Model, tea.Cmd) {
	if !m.snapshot.managed.Running {
		return m.startMihomo()
	}
	if generator.CheckReady(m.paths.ConfigDir, m.settings) != nil {
		return m.guideToSetup()
	}
	if core.Find(m.paths) == "" {
		return m.selectInstall()
	}
	m.busy = true
	return m, reloadCmd(m.manager, m.settings, m.snapshot.managed.Privileged)
}

// checkClaudeNow 立即检测一次 Claude 的线路。没开启监测时也检测，结果只
// 展示这一次。
func (m Model) checkClaudeNow() (tea.Model, tea.Cmd) {
	if m.claudeChecking {
		m.setMessage("正在检测 Claude 的线路，请稍候")
		return m, nil
	}
	m.claudeChecking = true
	m.setMessage("正在检测 Claude 的线路...")
	settings := m.settings
	settings.ClaudeGuard = true
	return m, claudeCheckCmd(m.manager, settings, true)
}

// claudeDue 报告是否该自动检测 Claude 的线路：开启了监测、没有检测在进行、
// 到了下一次检测的时间。没开启监测时不自动检测。
func (m Model) claudeDue(now time.Time) bool {
	return !m.claudeChecking && !now.Before(m.claudeNext) && claudeguard.Enabled(m.paths.ConfigDir, m.settings)
}

// claudeRouteChanged 在 Mihomo 启停、重载或切换策略后让下一次 tick 立即检测；
// reset 为真时丢掉之前的结论（用户刚改变了 Claude 的线路）。没开启监测时
// 清掉手动检测的旧结果，它已经不再成立。
func (m *Model) claudeRouteChanged(reset bool) {
	m.claudeNext = time.Time{}
	if !claudeguard.Enabled(m.paths.ConfigDir, m.settings) {
		m.claude = claudeguard.Status{}
		return
	}
	if reset && m.claudeTracker != nil {
		m.claudeTracker.Reset()
		m.claude = m.claudeTracker.Status()
	}
}

// updateDNSPending 记录遗留 DNS 恢复点出现的时间，持续 dnsPendingGrace 后才提示。
func (m *Model) updateDNSPending(leftover bool, now time.Time) {
	switch {
	case !leftover:
		m.dnsPendingSince = time.Time{}
	case m.dnsPendingSince.IsZero():
		m.dnsPendingSince = now
	}
	m.dnsPending = leftover && now.Sub(m.dnsPendingSince) >= dnsPendingGrace
}

// statusCursorFor 在操作列表变化后（例如 Mihomo 启停让「重载」出现或消失）
// 让光标停在原来的操作上；原操作没了就停在原位置附近。
func (m Model) statusCursorFor(id statusActionID, found bool) int {
	actions := m.statusActions()
	if found {
		for i, action := range actions {
			if action.id == id {
				return i
			}
		}
	}
	return min(m.cursor, len(actions)-1)
}

// guideToSetup 把首次使用的用户直接带到配置页的订阅地址编辑态。
func (m Model) guideToSetup() (tea.Model, tea.Cmd) {
	m.busy = false
	m.active = tabConfig
	m.resetViewState()
	m.cursor = fieldSubscription
	m.beginEdit(fieldSubscription, "")
	m.setMessage("请先粘贴机场订阅地址；也可以按 Esc 改为编辑节点文件、导入内网 .ovpn 或开启 Tailscale")
	return m, nil
}

// intranetListField 是内网域名、网段、DNS 这类列表设置的配置行：留空时
// 显示从 .ovpn 读出的值。
func intranetListField(label string, field int, target func(*config.Settings) *[]string, effective func(generator.Intranet) ([]string, bool)) configField {
	return configField{
		label: label,
		value: func(m Model) string {
			if m.settings.OVPNPath == "" {
				return "—"
			}
			values, auto := effective(generator.EffectiveIntranet(m.snapshot.profile, m.settings))
			text := or(config.JoinList(values), "无")
			if auto {
				return "自动：" + text
			}
			return text
		},
		enter: func(m Model) (tea.Model, tea.Cmd) {
			if m.settings.OVPNPath == "" {
				m.errorText = "先导入内网 .ovpn"
				return m, nil
			}
			m.beginEdit(field, config.JoinList(*target(&m.settings)))
			return m, nil
		},
	}
}

// tailscaleTextField 是 Tailscale 文本设置的配置行。
func tailscaleTextField(label string, field int, target func(*config.Settings) *string, empty func(Model) string) configField {
	return configField{
		label: label,
		value: func(m Model) string {
			if value := *target(&m.settings); value != "" {
				return value
			}
			return empty(m)
		},
		enter: func(m Model) (tea.Model, tea.Cmd) {
			m.beginEdit(field, *target(&m.settings))
			return m, nil
		},
	}
}

func tailscaleLoggedIn(m Model) bool {
	_, err := os.Stat(filepath.Join(m.paths.ConfigDir, config.TailscaleStateDir))
	return err == nil
}

// editProxiesFile 用 $EDITOR 编辑节点文件。
func (m Model) editProxiesFile() (tea.Model, tea.Cmd) {
	path := m.paths.Proxies
	return m.editFile(path, nodes.FileTemplate, "编辑节点文件", func() error {
		_, err := nodes.Load(path, nil)
		return err
	}, "节点文件已保存；重载 Mihomo 后生效")
}

// editClaudeProxyFile 用 $EDITOR 编辑住宅节点文件 claude-proxy.yaml（ss、
// trojan、vless 等需要完整 Mihomo 节点格式的协议写在这里）。
func (m Model) editClaudeProxyFile() (tea.Model, tea.Cmd) {
	configDir, settings := m.paths.ConfigDir, m.settings
	return m.editFile(filepath.Join(configDir, config.ClaudeProxyFile), claude.FileTemplate, "编辑住宅节点文件", func() error {
		_, err := claude.Load(configDir, settings)
		return err
	}, "住宅节点文件已保存；重载 Mihomo 后生效")
}

// editFile 用 $VISUAL / $EDITOR（默认 vi）打开 path；文件不存在时先写入只有
// 注释的 template。编辑器退出后用 check 校验内容，通过时显示 saved。
func (m Model) editFile(path, template, action string, check func() error, saved string) (tea.Model, tea.Cmd) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := m.paths.Ensure(); err != nil {
			m.errorText = err.Error()
			return m, nil
		}
		if err := config.WriteFileAtomic(path, []byte(template), 0o600); err != nil {
			m.errorText = err.Error()
			return m, nil
		}
	}
	editor := strings.TrimSpace(os.Getenv("VISUAL"))
	if editor == "" {
		editor = strings.TrimSpace(os.Getenv("EDITOR"))
	}
	if editor == "" {
		editor = "vi"
	}
	fields := strings.Fields(editor)
	command := exec.Command(fields[0], append(fields[1:], path)...)
	return m, tea.ExecProcess(command, func(err error) tea.Msg {
		if err != nil {
			return actionMsg{err: fmt.Errorf("%s: %w", action, err)}
		}
		if err := check(); err != nil {
			return actionMsg{err: err}
		}
		return actionMsg{text: saved}
	})
}

func (m Model) statusActions() []statusAction {
	running := m.snapshot.managed.Running
	start := statusAction{id: statusStart, label: "启动 Mihomo"}
	if running {
		start.label = "停止受管 Mihomo"
	}
	var actions []statusAction
	if !m.hasCore() {
		actions = append(actions, statusAction{id: statusInstall, label: fmt.Sprintf("安装 Mihomo 核心（%s，启动前必需）", coreChannelText(m.settings.CoreChannel))})
	}
	actions = append(actions, start)
	if running {
		actions = append(actions, statusAction{id: statusReload, label: "重载 Mihomo（重新生成配置并重启，让修改生效）"})
	}
	if m.dnsPending {
		actions = append(actions, statusAction{id: statusRecoverDNS, label: "恢复系统 DNS（上次运行遗留）"})
	}
	actions = append(actions, statusAction{id: statusValidate, label: "生成并校验配置"})
	if m.hasCore() {
		actions = append(actions, statusAction{id: statusInstall, label: fmt.Sprintf("更新 Mihomo 核心（%s）", coreChannelText(m.settings.CoreChannel))})
	}
	if m.snapshot.healthy {
		actions = append(actions, statusAction{id: statusRecoverWeChat, label: "刷新微信直连（微信图片发不出去时用）"})
	}
	claudeLabel := "检测 Claude 线路"
	if m.claudeChecking {
		claudeLabel += "（正在检测...）"
	}
	actions = append(actions,
		statusAction{id: statusClaudeCheck, label: claudeLabel},
		statusAction{id: statusShare, label: "Clash Mi 扫码导入"},
	)
	if m.appUpdateAvailable {
		actions = append(actions, statusAction{
			id:    statusUpdateApp,
			label: fmt.Sprintf("更新 APP（%s -> %s）", m.appVersion, m.latestAppVersion),
		})
	}
	return actions
}

// hasCore 只读缓存，不触发文件系统探测；View 每帧会多次调用。
func (m Model) hasCore() bool {
	return m.snapshot.managed.CorePath != "" || m.coreFound
}

func (m Model) selectInstall() (tea.Model, tea.Cmd) {
	// The core may have disappeared after the latest status refresh. Clear the
	// cached path so the next render immediately uses the missing-core layout.
	m.snapshot.managed.CorePath = ""
	m.snapshot.managed.CoreVersion = ""
	m.coreFound = false
	m.active = tabStatus
	m.resetViewState()
	m.cursor = 0
	m.busy = false
	m.errorText = "尚未安装 Mihomo 核心；已选中安装操作，请按 Enter 执行（不需要 sudo）"
	return m, nil
}

// configField 是配置页字段的单一定义：标签、取值和 Enter 行为。
// model 与 view 都从这张表取数据，避免两处清单漂移。
type configField struct {
	label string
	value func(m Model) string
	enter func(m Model) (tea.Model, tea.Cmd)
}

var configFields = []configField{
	{
		label: "订阅地址",
		value: func(m Model) string {
			return or(config.MaskSubscription(m.settings.Subscription), "未设置（Enter 粘贴机场订阅地址）")
		},
		enter: func(m Model) (tea.Model, tea.Cmd) {
			m.beginEdit(fieldSubscription, m.settings.Subscription)
			return m, nil
		},
	},
	{
		label: "节点文件",
		value: func(m Model) string {
			file, err := nodes.Load(m.paths.Proxies, nil)
			switch {
			case err != nil:
				return "有错误：" + err.Error()
			case file.Count() == 0:
				return "未设置（Enter 用 $EDITOR 编辑 " + config.ProxiesFile + "）"
			default:
				return fmt.Sprintf("%d 个节点（Enter 编辑 %s）", file.Count(), config.ProxiesFile)
			}
		},
		enter: Model.editProxiesFile,
	},
	{
		label: "内网 OVPN",
		value: func(m Model) string {
			return or(m.settings.OVPNPath, "未导入（可选，连接公司或家里的内网）")
		},
		enter: func(m Model) (tea.Model, tea.Cmd) {
			m.beginEdit(fieldOVPN, m.settings.OVPNPath)
			return m, nil
		},
	},
	{
		label: "Remote",
		value: func(m Model) string {
			if m.settings.OVPNPath == "" {
				return "—"
			}
			return or(m.settings.Remote, "全部 remote")
		},
		enter: Model.openRemotePicker,
	},
	intranetListField("内网域名", fieldIntranetDomains, func(s *config.Settings) *[]string { return &s.IntranetDomains },
		func(i generator.Intranet) ([]string, bool) { return i.Domains, i.DomainsAuto }),
	intranetListField("内网网段", fieldIntranetCIDRs, func(s *config.Settings) *[]string { return &s.IntranetCIDRs },
		func(i generator.Intranet) ([]string, bool) { return i.CIDRs, i.CIDRsAuto }),
	intranetListField("内网 DNS", fieldIntranetDNS, func(s *config.Settings) *[]string { return &s.IntranetDNS },
		func(i generator.Intranet) ([]string, bool) { return i.DNS, i.DNSAuto }),
	{
		label: "内网 HTTP 出口",
		value: func(m Model) string {
			if m.settings.OVPNPath == "" {
				return "—"
			}
			return or(m.settings.IntranetHTTPProxy, "未设置（可选，host:port）")
		},
		enter: func(m Model) (tea.Model, tea.Cmd) {
			if m.settings.OVPNPath == "" {
				m.errorText = "先导入内网 .ovpn"
				return m, nil
			}
			m.beginEdit(fieldIntranetHTTP, m.settings.IntranetHTTPProxy)
			return m, nil
		},
	},
	{
		label: "内网检查地址",
		value: func(m Model) string {
			if m.settings.OVPNPath == "" {
				return "—"
			}
			if m.settings.IntranetProbeURL == "" && m.settings.IntranetHTTPProxy != "" {
				return "自动：http://" + m.settings.IntranetHTTPProxy
			}
			return or(m.settings.IntranetProbeURL, "未设置（可选，用来发现隧道失效）")
		},
		enter: func(m Model) (tea.Model, tea.Cmd) {
			if m.settings.OVPNPath == "" {
				m.errorText = "先导入内网 .ovpn"
				return m, nil
			}
			m.beginEdit(fieldIntranetProbe, m.settings.IntranetProbeURL)
			return m, nil
		},
	},
	{
		label: "Tailscale",
		value: func(m Model) string {
			if !m.settings.TailscaleEnabled {
				return "关闭"
			}
			return "开启（设备名 " + generator.TailscaleHostname(m.settings) + "）"
		},
		enter: func(m Model) (tea.Model, tea.Cmd) {
			m.settings.TailscaleEnabled = !m.settings.TailscaleEnabled
			message := "已关闭 Tailscale；重载 Mihomo 后生效"
			if m.settings.TailscaleEnabled {
				message = "已开启 Tailscale；重载 Mihomo 后生效"
				if !tailscaleLoggedIn(m) && m.settings.TailscaleAuthKey == "" {
					message += "。首次登录需要填写 Auth Key"
				}
			}
			return m.saveSettings(message)
		},
	},
	{
		label: "Tailscale Auth Key",
		value: func(m Model) string {
			switch {
			case m.settings.TailscaleAuthKey != "":
				return config.MaskSecret(m.settings.TailscaleAuthKey)
			case tailscaleLoggedIn(m):
				return "已登录，不需要"
			default:
				return "未设置（首次登录需要 tskey-auth-…）"
			}
		},
		enter: func(m Model) (tea.Model, tea.Cmd) {
			m.beginEdit(fieldTailscaleAuthKey, m.settings.TailscaleAuthKey)
			return m, nil
		},
	},
	tailscaleTextField("Tailscale 设备名", fieldTailscaleHostname, func(s *config.Settings) *string { return &s.TailscaleHostname },
		func(m Model) string { return "默认 " + generator.TailscaleHostname(m.settings) }),
	tailscaleTextField("Tailscale 控制服务器", fieldTailscaleControlURL, func(s *config.Settings) *string { return &s.TailscaleControlURL },
		func(Model) string { return "官方服务器（Headscale 填地址）" }),
	tailscaleTextField("Tailscale 出口节点", fieldTailscaleExitNode, func(s *config.Settings) *string { return &s.TailscaleExitNode },
		func(Model) string { return "未设置（可选，经这台设备上网）" }),
	{
		label: "TUN 模式",
		value: func(m Model) string { return boolText(m.settings.TunEnabled, "开启", "关闭") },
		enter: func(m Model) (tea.Model, tea.Cmd) {
			m.settings.TunEnabled = !m.settings.TunEnabled
			return m.saveSettings("TUN 模式已更新")
		},
	},
	{
		label: "局域网代理",
		value: func(m Model) string { return boolText(m.settings.AllowLAN, "允许 0.0.0.0", "仅本机 127.0.0.1") },
		enter: func(m Model) (tea.Model, tea.Cmd) {
			m.settings.AllowLAN = !m.settings.AllowLAN
			return m.saveSettings("局域网代理已更新；重载 Mihomo 后生效")
		},
	},
	{
		label: "代理用户名",
		value: func(m Model) string { return or(m.settings.ProxyUsername, "未设置（局域网代理不认证）") },
		enter: func(m Model) (tea.Model, tea.Cmd) {
			m.beginEdit(fieldProxyUsername, m.settings.ProxyUsername)
			return m, nil
		},
	},
	{
		label: "代理密码",
		value: func(m Model) string {
			if m.settings.ProxyPassword == "" {
				return "未设置"
			}
			return "已设置（Enter 查看或修改）"
		},
		enter: func(m Model) (tea.Model, tea.Cmd) {
			m.beginEdit(fieldProxyPassword, m.settings.ProxyPassword)
			return m, nil
		},
	},
	{
		label: "国内流量",
		value: func(m Model) string { return boolText(m.settings.ChinaDirect, "直连", "走外网代理") },
		enter: func(m Model) (tea.Model, tea.Cmd) {
			m.settings.ChinaDirect = !m.settings.ChinaDirect
			return m.saveSettings("国内流量策略已更新；重载 Mihomo 后生效")
		},
	},
	{
		label: "Claude 住宅节点",
		value: func(m Model) string {
			switch {
			case m.settings.ClaudeProxy != "":
				return claude.MaskURL(m.settings.ClaudeProxy)
			case claude.Configured(m.paths.ConfigDir, m.settings):
				return "写在 " + config.ClaudeProxyFile
			default:
				return "未设置（Claude 走代理节点）"
			}
		},
		enter: func(m Model) (tea.Model, tea.Cmd) {
			m.beginEdit(fieldClaudeProxy, m.settings.ClaudeProxy)
			return m, nil
		},
	},
	{
		label: "住宅节点文件",
		value: func(m Model) string {
			fileOnly := m.settings
			fileOnly.ClaudeProxy = ""
			if !claude.Configured(m.paths.ConfigDir, fileOnly) {
				return "未使用（ss、trojan 等协议写这里；Enter 用 $EDITOR 编辑 " + config.ClaudeProxyFile + "）"
			}
			if _, err := claude.Load(m.paths.ConfigDir, m.settings); err != nil {
				return "有错误：" + err.Error()
			}
			return "已写入节点（Enter 编辑 " + config.ClaudeProxyFile + "）"
		},
		enter: Model.editClaudeProxyFile,
	},
	{
		label: "住宅节点连接方式",
		value: func(m Model) string {
			if localClaudeNode(m) {
				return "本机直接连接（本机地址只能直接连接）"
			}
			if !claude.Chainable(m.paths.ConfigDir, m.settings) {
				return "本机直接连接（没有可做前置代理的节点）"
			}
			return boolText(m.settings.ClaudeProxyDirect, "本机直接连接", "经前置代理链式连接")
		},
		enter: func(m Model) (tea.Model, tea.Cmd) {
			if localClaudeNode(m) {
				m.errorText = "住宅节点是本机地址（例如 SSH 端口转发），前置代理连不到它，只能本机直接连接"
				return m, nil
			}
			m.settings.ClaudeProxyDirect = !m.settings.ClaudeProxyDirect
			if _, err := claude.Load(m.paths.ConfigDir, m.settings); err != nil {
				m.settings.ClaudeProxyDirect = !m.settings.ClaudeProxyDirect
				m.errorText = err.Error()
				return m, nil
			}
			return m.saveSettings("住宅节点连接方式已更新；重载 Mihomo 后生效")
		},
	},
	{
		label: "Claude 访问监测",
		value: func(m Model) string {
			if claude.Configured(m.paths.ConfigDir, m.settings) {
				return "开启（已配置住宅节点）"
			}
			return boolText(m.settings.ClaudeGuard, "开启", "关闭")
		},
		enter: func(m Model) (tea.Model, tea.Cmd) {
			if claude.Configured(m.paths.ConfigDir, m.settings) {
				m.errorText = "配置了住宅节点时总是监测 Claude 的线路"
				return m, nil
			}
			m.settings.ClaudeGuard = !m.settings.ClaudeGuard
			m.claudeRouteChanged(true)
			return m.saveSettings(boolText(m.settings.ClaudeGuard,
				"已开启 Claude 访问监测：状态页显示检测结论，出问题时桌面版或 macOS 系统弹窗提醒",
				"已关闭 Claude 访问监测"))
		},
	},
	{
		label: "Claude 直连保护",
		value: func(m Model) string {
			switch {
			case !claudeblock.Supported():
				return "仅 macOS 支持"
			case claudeblock.Installed():
				return "开启（防火墙拦截不经隧道的 Claude 连接）"
			default:
				return "关闭"
			}
		},
		enter: func(m Model) (tea.Model, tea.Cmd) {
			if !claudeblock.Supported() {
				m.errorText = "直连保护目前只支持 macOS"
				return m, nil
			}
			operation := "claude-block-on"
			if claudeblock.Installed() {
				operation = "claude-block-off"
			}
			m.busy = true
			if !system.CanUseSudoWithoutPrompt() {
				return m, authorizeAdministratorCmd(operation, m.settings)
			}
			return m, claudeBlockCmd(operation == "claude-block-on")
		},
	},
	{
		label: "Mixed 端口",
		value: func(m Model) string { return strconv.Itoa(m.settings.MixedPort) },
		enter: func(m Model) (tea.Model, tea.Cmd) {
			m.beginEdit(fieldMixedPort, strconv.Itoa(m.settings.MixedPort))
			return m, nil
		},
	},
	{
		label: "Controller 端口",
		value: func(m Model) string { return strconv.Itoa(m.settings.ControllerPort) },
		enter: func(m Model) (tea.Model, tea.Cmd) {
			m.beginEdit(fieldControllerPort, strconv.Itoa(m.settings.ControllerPort))
			return m, nil
		},
	},
	{
		label: "DNS 端口",
		value: func(m Model) string { return strconv.Itoa(m.settings.DNSPort) },
		enter: func(m Model) (tea.Model, tea.Cmd) {
			m.beginEdit(fieldDNSPort, strconv.Itoa(m.settings.DNSPort))
			return m, nil
		},
	},
	{
		label: "核心通道",
		value: func(m Model) string { return coreChannelText(m.settings.CoreChannel) },
		enter: func(m Model) (tea.Model, tea.Cmd) {
			if m.settings.CoreChannel == config.CoreChannelAlpha {
				m.settings.CoreChannel = config.CoreChannelStable
			} else {
				m.settings.CoreChannel = config.CoreChannelAlpha
			}
			return m.saveSettings("核心通道已切换为 " + coreChannelText(m.settings.CoreChannel) + "；更新核心后生效")
		},
	},
}

func coreChannelText(channel config.CoreChannel) string {
	if channel == config.CoreChannelAlpha {
		return "Alpha"
	}
	return "Stable"
}

func (m Model) handleConfigKey(key string) (tea.Model, tea.Cmd) {
	fields := len(configFields)
	switch key {
	case "up", "k":
		m.cursor = (m.cursor + fields - 1) % fields
	case "down", "j":
		m.cursor = (m.cursor + 1) % fields
	case "s":
		return m.openScanPicker()
	case "enter", "space", "e":
		if m.cursor < fields {
			return configFields[m.cursor].enter(m)
		}
	}
	return m, nil
}

func (m Model) openRemotePicker() (tea.Model, tea.Cmd) {
	profile := m.snapshot.profile
	if profile == nil {
		m.errorText = "请先选择有效的 OVPN"
		return m, nil
	}
	values := []string{""}
	for _, remote := range profile.Remotes {
		values = append(values, remote.String())
	}
	cursor := 0
	for i, value := range values {
		if value == m.settings.Remote {
			cursor = i
			break
		}
	}
	m.picker = &picker{kind: pickerRemote, title: "选择 remote", values: values, cursor: cursor}
	m.errorText = ""
	return m, nil
}

func (m Model) openScanPicker() (tea.Model, tea.Cmd) {
	m.scannedOVPN = ovpn.Scan(m.settings.OVPNPath)
	if len(m.scannedOVPN) == 0 {
		m.errorText = "未在下载、桌面或配置目录发现 .ovpn"
		return m, nil
	}
	cursor := 0
	for i, path := range m.scannedOVPN {
		if path == m.settings.OVPNPath {
			cursor = i
			break
		}
	}
	m.picker = &picker{kind: pickerScan, title: "选择扫描到的 OVPN", values: append([]string(nil), m.scannedOVPN...), cursor: cursor}
	m.errorText = ""
	return m, nil
}

func (m Model) handlePicker(key string) (tea.Model, tea.Cmd) {
	p := m.picker
	switch key {
	case "up", "k":
		p.cursor = (p.cursor + len(p.values) - 1) % len(p.values)
	case "down", "j":
		p.cursor = (p.cursor + 1) % len(p.values)
	case "esc", "backspace", "q":
		m.picker = nil
	case "enter", "space":
		value := p.values[p.cursor]
		m.picker = nil
		switch p.kind {
		case pickerRemote:
			m.settings.Remote = value
			return m.saveSettings("Remote 已更新")
		case pickerScan:
			return m.applyOVPNPath(value)
		}
	}
	return m, nil
}

// applyOVPNPath 校验并保存新的 OVPN 路径，同时重置 remote 选择。
func (m Model) applyOVPNPath(path string) (tea.Model, tea.Cmd) {
	profile, err := ovpn.ParseFile(path)
	if err != nil {
		m.errorText = err.Error()
		return m, nil
	}
	m.settings.OVPNPath = path
	m.settings.Remote = ""
	m.snapshot.profile = &profile
	m.snapshot.profileStamp = profileStamp(path)
	return m.saveSettings("OVPN 已更新")
}

func (m Model) handlePolicyKey(key string) (tea.Model, tea.Cmd) {
	groups := m.snapshot.groups
	if len(groups) == 0 {
		return m, nil
	}
	if m.choosing {
		choices := groups[m.cursor].Choices
		switch key {
		case "up", "k":
			m.choice = (m.choice + len(choices) - 1) % len(choices)
		case "down", "j":
			m.choice = (m.choice + 1) % len(choices)
		case "esc", "backspace":
			m.choosing = false
		case "enter", "space":
			m.choosing = false
			m.busy = true
			return m, selectPolicyCmd(m.settings.ControllerPort, m.settings.ControllerSecret, groups[m.cursor].Name, choices[m.choice])
		}
		return m, nil
	}
	switch key {
	case "up", "k":
		m.cursor = (m.cursor + len(groups) - 1) % len(groups)
	case "down", "j":
		m.cursor = (m.cursor + 1) % len(groups)
	case "enter", "space":
		m.choosing = true
		m.choice = 0
	}
	return m, nil
}

var ruleTargets = customrules.DomainTargets

// 规则页各行，与 rulesView 的顺序一致。
const (
	ruleRowWebsite = iota
	ruleRowTest
	ruleRowTarget
	ruleRowSave
	ruleRowReload
	ruleRows
)

func (m Model) handleRulesKey(key string) (tea.Model, tea.Cmd) {
	const fields = ruleRows
	switch key {
	case "up", "k":
		m.cursor = (m.cursor + fields - 1) % fields
	case "down", "j":
		m.cursor = (m.cursor + 1) % fields
	case "enter", "space", "e":
		switch m.cursor {
		case ruleRowWebsite:
			m.beginEdit(editRuleWebsite, m.ruleInput)
		case ruleRowTest:
			return m.testRuleRoute()
		case ruleRowTarget:
			m.ruleTarget = (m.ruleTarget + 1) % len(ruleTargets)
		case ruleRowReload:
			m.message = ""
			m.errorText = ""
			return m.reloadMihomo()
		case ruleRowSave:
			domain, err := customrules.DomainFromInput(m.ruleInput)
			if err != nil {
				m.errorText = err.Error()
				return m, nil
			}
			rule := customrules.Rule{Domain: domain, Target: ruleTargets[m.ruleTarget]}
			if err := customrules.Save(m.customRulesPath(), rule); err != nil {
				m.errorText = err.Error()
				return m, nil
			}
			m.userRules, _ = customrules.Load(m.customRulesPath())
			// 光标移到「重载」行，再按一次 Enter 即可让规则生效。
			m.cursor = ruleRowReload
			m.setMessage("规则已保存；按 Enter 重载 Mihomo 后生效")
			m.errorText = ""
		}
	}
	return m, nil
}

func (m Model) testRuleRoute() (tea.Model, tea.Cmd) {
	domain, port, err := customrules.DestinationFromInput(m.ruleInput)
	if err != nil {
		m.errorText = err.Error()
		return m, nil
	}
	m.ruleHost = domain
	m.ruleIP = ""
	m.ruleMatch = controller.Rule{}
	m.ruleChains = nil
	m.ruleTested = false
	m.errorText = ""
	m.message = ""
	m.busy = true
	return m, testRouteCmd(m.settings.ControllerPort, m.settings.ControllerSecret, m.settings.MixedPort, domain, port)
}

func (m Model) handleConnectionsKey(key string) (tea.Model, tea.Cmd) {
	if m.connectionSearch.active {
		m.connectionSearch.handleKey(key, m.keyInput)
		m.connectionScroll = 0
		return m, nil
	}
	// Controller 离线时页面没有列表也没有搜索栏，进入搜索态只会吞掉按键。
	if key == "/" && m.snapshot.healthy {
		m.connectionSearch.active = true
		return m, nil
	}
	if key == "esc" && m.connectionSearch.query != "" {
		m.connectionSearch.query = ""
		m.connectionScroll = 0
		return m, nil
	}

	maxScroll := max(0, len(m.filteredConnections())-m.connectionVisibleLines())
	m.connectionScroll = min(m.connectionScroll, maxScroll)
	switch key {
	case "up", "k":
		m.connectionScroll = max(m.connectionScroll-1, 0)
	case "down", "j":
		m.connectionScroll = min(m.connectionScroll+1, maxScroll)
	case "pgup", "ctrl+u":
		m.connectionScroll = max(m.connectionScroll-m.connectionVisibleLines()/2, 0)
	case "pgdown", "ctrl+d":
		m.connectionScroll = min(m.connectionScroll+m.connectionVisibleLines()/2, maxScroll)
	case "g", "home":
		m.connectionScroll = 0
	case "G", "end":
		m.connectionScroll = maxScroll
	}
	return m, nil
}

// searchBox 是连接页和日志页共用的即时筛选输入框。
type searchBox struct {
	query  string
	active bool
}

// handleKey 处理搜索态下的一次按键；input 是按键携带的可打印文本，
// 为空时退回到按键名推断。
func (s *searchBox) handleKey(key, input string) {
	switch key {
	case "esc":
		s.query = ""
		s.active = false
	case "enter":
		s.active = false
	case "backspace", "ctrl+h":
		runes := []rune(s.query)
		if len(runes) > 0 {
			s.query = string(runes[:len(runes)-1])
		}
	case "ctrl+u":
		s.query = ""
	default:
		if text := keyText(key, input); text != "" {
			s.query += text
		}
	}
}

// appendText 追加粘贴内容，去掉换行以保持单行查询。
func (s *searchBox) appendText(text string) {
	s.query += strings.ReplaceAll(strings.ReplaceAll(text, "\r", ""), "\n", "")
}

func (m Model) filteredConnections() []controller.Connection {
	query := strings.ToLower(strings.TrimSpace(m.connectionSearch.query))
	if query == "" {
		return m.snapshot.connections
	}
	connections := make([]controller.Connection, 0, len(m.snapshot.connections))
	for _, connection := range m.snapshot.connections {
		if connectionMatchesSearch(connection, query) {
			connections = append(connections, connection)
		}
	}
	return connections
}

func connectionMatchesSearch(connection controller.Connection, query string) bool {
	values := []string{
		connection.Metadata.Host,
		connection.Metadata.SniffHost,
		connection.Metadata.SourceIP,
		connection.Metadata.SourcePort,
		connection.Metadata.DestinationIP,
		connection.Metadata.DestinationPort,
		connection.Metadata.Process,
		connection.Metadata.ProcessPath,
		connection.Rule,
		connection.Payload,
	}
	values = append(values, connection.Chains...)
	for _, value := range values {
		if strings.Contains(strings.ToLower(value), query) {
			return true
		}
	}
	return false
}

func (m Model) connectionVisibleLines() int {
	return max(6, m.height-logViewChrome-1)
}

func calculateConnectionRates(previous, current snapshot) map[string]connectionRate {
	if previous.connectionsAt.IsZero() || current.connectionsAt.IsZero() {
		return nil
	}
	return controller.Rates(previous.connections, current.connections, current.connectionsAt.Sub(previous.connectionsAt))
}

func (m Model) handleLogsKey(key string) (tea.Model, tea.Cmd) {
	if m.logSearch.active {
		m.logSearch.handleKey(key, m.keyInput)
		m.logScroll = 0
		return m, nil
	}
	// 尚无日志时页面不画搜索栏，进入搜索态只会吞掉按键。
	if key == "/" && len(m.snapshot.logs) > 0 {
		m.logSearch.active = true
		return m, nil
	}
	if key == "esc" && m.logSearch.query != "" {
		m.logSearch.query = ""
		m.logScroll = 0
		return m, nil
	}

	maxScroll := max(0, len(m.filteredLogs())-m.logVisibleLines())
	// 日志轮转或窗口变化后 logScroll 可能超过新上限，先收敛再处理按键，
	// 否则向下滚动要从过期的大值慢慢减回来。
	m.logScroll = min(m.logScroll, maxScroll)
	switch key {
	case "o":
		return m, openPathCmd(m.paths.LogFile)
	case "up", "k":
		m.logScroll = min(m.logScroll+1, maxScroll)
	case "down", "j":
		m.logScroll = max(m.logScroll-1, 0)
	case "pgup", "ctrl+u":
		m.logScroll = min(m.logScroll+m.logVisibleLines()/2, maxScroll)
	case "pgdown", "ctrl+d":
		m.logScroll = max(m.logScroll-m.logVisibleLines()/2, 0)
	case "g", "home":
		m.logScroll = maxScroll
	case "G", "end":
		m.logScroll = 0
	}
	return m, nil
}

func (m Model) filteredLogs() []string {
	query := strings.ToLower(strings.TrimSpace(m.logSearch.query))
	if query == "" {
		return m.snapshot.logs
	}
	lines := make([]string, 0, len(m.snapshot.logs))
	for _, line := range m.snapshot.logs {
		if strings.Contains(strings.ToLower(line), query) {
			lines = append(lines, line)
		}
	}
	return lines
}

// logVisibleLines 是日志面板一屏可容纳的行数，与 logsView 的布局保持一致。
func (m Model) logVisibleLines() int {
	return max(8, m.height-logViewChrome)
}

func (m *Model) beginEdit(field int, value string) {
	m.editing = true
	m.editField = field
	m.editBuffer = value
	m.editCursor = utf8.RuneCountInString(value)
	m.resetPathCompletion()
	m.errorText = ""
}

// setEditBuffer 替换整个编辑内容并把光标移到末尾。
func (m *Model) setEditBuffer(value string) {
	m.editBuffer = value
	m.editCursor = utf8.RuneCountInString(value)
}

func (m Model) handleEdit(key string) (tea.Model, tea.Cmd) {
	runes := []rune(m.editBuffer)
	if m.editCursor > len(runes) {
		m.editCursor = len(runes)
	}
	switch key {
	case "esc":
		m.editing = false
		m.resetPathCompletion()
		m.setMessage("已取消编辑")
		return m, nil
	case "enter":
		return m.commitEdit()
	case "tab", "shift+tab":
		if m.editField == fieldOVPN {
			m.completeOVPNPath(key == "shift+tab")
		}
		return m, nil
	case "left", "ctrl+b":
		if m.editCursor > 0 {
			m.editCursor--
		}
		return m, nil
	case "right", "ctrl+f":
		if m.editCursor < len(runes) {
			m.editCursor++
		}
		return m, nil
	case "home", "ctrl+a":
		m.editCursor = 0
		return m, nil
	case "end", "ctrl+e":
		m.editCursor = len(runes)
		return m, nil
	case "backspace":
		if m.editCursor > 0 {
			m.editBuffer = string(runes[:m.editCursor-1]) + string(runes[m.editCursor:])
			m.editCursor--
		}
		m.resetPathCompletion()
		m.errorText = ""
		return m, nil
	case "delete", "ctrl+d":
		if m.editCursor < len(runes) {
			m.editBuffer = string(runes[:m.editCursor]) + string(runes[m.editCursor+1:])
		}
		m.resetPathCompletion()
		m.errorText = ""
		return m, nil
	case "ctrl+u":
		m.editBuffer = string(runes[m.editCursor:])
		m.editCursor = 0
		m.resetPathCompletion()
		m.errorText = ""
		return m, nil
	case "ctrl+k":
		m.editBuffer = string(runes[:m.editCursor])
		m.resetPathCompletion()
		m.errorText = ""
		return m, nil
	}
	if msgText := keyText(key, m.keyInput); msgText != "" {
		m.editBuffer = string(runes[:m.editCursor]) + msgText + string(runes[m.editCursor:])
		m.editCursor += utf8.RuneCountInString(msgText)
		m.resetPathCompletion()
		m.errorText = ""
	}
	return m, nil
}

func (m *Model) completeOVPNPath(reverse bool) {
	if len(m.pathCompletions) > 1 && m.editBuffer == m.pathCompletionText {
		if reverse {
			if m.pathCompletion < 0 {
				m.pathCompletion = len(m.pathCompletions)
			}
			m.pathCompletion = (m.pathCompletion + len(m.pathCompletions) - 1) % len(m.pathCompletions)
		} else {
			m.pathCompletion = (m.pathCompletion + 1) % len(m.pathCompletions)
		}
		m.setEditBuffer(m.pathCompletions[m.pathCompletion])
		m.pathCompletionText = m.editBuffer
		m.errorText = ""
		return
	}

	candidates, err := ovpnPathCompletions(m.editBuffer)
	m.resetPathCompletion()
	if err != nil {
		m.errorText = err.Error()
		return
	}
	if len(candidates) == 0 {
		m.errorText = "没有匹配的目录或 .ovpn 文件"
		return
	}
	m.errorText = ""
	if len(candidates) == 1 {
		m.setEditBuffer(candidates[0])
		return
	}

	m.setEditBuffer(commonStringPrefix(candidates))
	m.pathCompletions = candidates
	m.pathCompletion = -1
	m.pathCompletionText = m.editBuffer
}

func (m *Model) resetPathCompletion() {
	m.pathCompletions = nil
	m.pathCompletion = -1
	m.pathCompletionText = ""
}

func (m Model) handlePaste(content string) (tea.Model, tea.Cmd) {
	// 选择器打开时粘贴会绕过它直接改设置，而选择器仍留在屏幕上，
	// 下一次 Enter 又用过期候选覆盖回去；此时直接忽略粘贴。
	if m.help || m.confirm != "" || m.busy || m.picker != nil {
		return m, nil
	}
	if m.editing {
		if m.editField == fieldOVPN {
			m.setEditBuffer(content)
			m.resetPathCompletion()
			return m.commitEdit()
		}
		runes := []rune(m.editBuffer)
		if m.editCursor > len(runes) {
			m.editCursor = len(runes)
		}
		m.editBuffer = string(runes[:m.editCursor]) + content + string(runes[m.editCursor:])
		m.editCursor += utf8.RuneCountInString(content)
		return m, nil
	}
	if m.active != tabConfig {
		return m, nil
	}
	// 配置页上直接粘贴（或从文件管理器拖入）：.ovpn 路径导入内网，
	// http(s) 地址保存为订阅，其余按光标所在行处理。
	trimmed := strings.TrimSpace(content)
	switch {
	case m.cursor == fieldOVPN || strings.HasSuffix(strings.ToLower(strings.Trim(trimmed, `'"`)), ".ovpn"):
		m.cursor = fieldOVPN
		m.beginEdit(fieldOVPN, content)
		return m.commitEdit()
	case m.cursor == fieldSubscription || strings.HasPrefix(trimmed, "https://") || strings.HasPrefix(trimmed, "http://"):
		m.cursor = fieldSubscription
		m.beginEdit(fieldSubscription, trimmed)
		return m.commitEdit()
	}
	return m, nil
}

func (m Model) commitEdit() (tea.Model, tea.Cmd) {
	value := strings.TrimSpace(m.editBuffer)
	if m.editField == editRuleWebsite {
		if _, err := customrules.DomainFromInput(value); err != nil {
			m.errorText = err.Error()
			return m, nil
		}
		m.ruleInput = value
		m.editing = false
		m.resetPathCompletion()
		m.cursor = ruleRowTest
		return m.testRuleRoute()
	}
	switch m.editField {
	case fieldSubscription:
		if err := config.ValidateSubscription(value); err != nil {
			m.errorText = err.Error()
			return m, nil
		}
		m.settings.Subscription = value
		m.editing = false
		m.resetPathCompletion()
		if value == "" {
			return m.saveSettings("已清除订阅地址；重载 Mihomo 后生效")
		}
		return m.saveSettings("订阅地址已保存；启动或重载 Mihomo 后自动下载节点")
	case fieldIntranetDomains, fieldIntranetCIDRs, fieldIntranetDNS, fieldIntranetHTTP, fieldIntranetProbe:
		updated := m.settings
		switch m.editField {
		case fieldIntranetDomains:
			updated.IntranetDomains = config.NormalizeDomains(config.SplitList(value))
		case fieldIntranetCIDRs:
			updated.IntranetCIDRs = config.SplitList(value)
		case fieldIntranetDNS:
			updated.IntranetDNS = config.SplitList(value)
		case fieldIntranetHTTP:
			updated.IntranetHTTPProxy = value
		case fieldIntranetProbe:
			updated.IntranetProbeURL = value
		}
		if err := config.ValidateIntranet(updated); err != nil {
			m.errorText = err.Error()
			return m, nil
		}
		m.settings = updated
		m.editing = false
		m.resetPathCompletion()
		return m.saveSettings("内网设置已保存；重载 Mihomo 后生效")
	case fieldTailscaleAuthKey, fieldTailscaleHostname, fieldTailscaleControlURL, fieldTailscaleExitNode:
		updated := m.settings
		switch m.editField {
		case fieldTailscaleAuthKey:
			updated.TailscaleAuthKey = value
		case fieldTailscaleHostname:
			updated.TailscaleHostname = value
		case fieldTailscaleControlURL:
			updated.TailscaleControlURL = value
		case fieldTailscaleExitNode:
			updated.TailscaleExitNode = value
		}
		if err := config.ValidateTailscale(updated); err != nil {
			m.errorText = err.Error()
			return m, nil
		}
		m.settings = updated
		m.editing = false
		m.resetPathCompletion()
		return m.saveSettings("Tailscale 设置已保存；重载 Mihomo 后生效")
	case fieldOVPN:
		if value == "" {
			m.settings.OVPNPath = ""
			m.settings.Remote = ""
			m.editing = false
			m.resetPathCompletion()
			return m.saveSettings("已移除内网 .ovpn；重载 Mihomo 后生效")
		}
		normalized, err := normalizeOVPNPath(value)
		if err != nil {
			m.errorText = err.Error()
			return m, nil
		}
		m.editing = false
		m.resetPathCompletion()
		return m.applyOVPNPath(normalized)
	case fieldProxyUsername, fieldProxyPassword:
		check, target := config.CheckProxyUsername, &m.settings.ProxyUsername
		if m.editField == fieldProxyPassword {
			check, target = config.CheckProxyPassword, &m.settings.ProxyPassword
		}
		if err := check(value); err != nil {
			m.errorText = err.Error()
			return m, nil
		}
		*target = value
		m.editing = false
		m.resetPathCompletion()
		return m.saveSettings(proxyAuthMessage(m.settings))
	case fieldClaudeProxy:
		previous := m.settings.ClaudeProxy
		m.settings.ClaudeProxy = value
		if _, err := claude.Load(m.paths.ConfigDir, m.settings); err != nil {
			m.settings.ClaudeProxy = previous
			m.errorText = err.Error()
			return m, nil
		}
		m.editing = false
		m.resetPathCompletion()
		if value == "" {
			return m.saveSettings("已清除住宅节点地址；重载 Mihomo 后生效")
		}
		return m.saveSettings("住宅节点已保存；重载 Mihomo 后 Claude 改走住宅节点")
	case fieldMixedPort, fieldControllerPort, fieldDNSPort:
		port, err := strconv.Atoi(value)
		if err != nil || port < 1 || port > 65535 {
			m.errorText = "端口必须是 1 到 65535 之间的整数"
			return m, nil
		}
		switch m.editField {
		case fieldMixedPort:
			m.settings.MixedPort = port
		case fieldControllerPort:
			m.settings.ControllerPort = port
		case fieldDNSPort:
			m.settings.DNSPort = port
		}
	}
	m.editing = false
	m.resetPathCompletion()
	return m.saveSettings("设置已保存")
}

// proxyAuthMessage 说明保存后局域网代理的认证状态；用户名和密码要都设置
// 才会生效，只设一项时提示另一项。
func proxyAuthMessage(settings config.Settings) string {
	switch {
	case settings.ProxyAuthEnabled():
		return "代理认证已更新；重载 Mihomo 后生效"
	case settings.ProxyUsername == "" && settings.ProxyPassword == "":
		return "代理认证已关闭；重载 Mihomo 后生效"
	default:
		return "已保存；用户名和密码都设置后局域网代理才要求认证"
	}
}

func (m Model) saveSettings(message string) (tea.Model, tea.Cmd) {
	if err := config.Save(m.paths, m.settings); err != nil {
		m.errorText = err.Error()
		return m, nil
	}
	m.setMessage(message)
	m.errorText = ""
	// 保存设置只需要刷新状态；完整诊断（sudo、lsof、TCP 拨号）留给
	// r 手动刷新和动作完成后触发。
	return m, refreshCmd(m)
}

func refreshCmd(m Model) tea.Cmd {
	prevProfile, prevStamp := m.snapshot.profile, m.snapshot.profileStamp
	return func() tea.Msg {
		result := snapshot{loaded: true}
		result.managed = m.manager.Status()
		result.dnsLeftover = m.manager.LeftoverDNS(result.managed.Running)
		result.vpn = system.InspectVPN()
		client := controller.New(m.settings.ControllerPort, m.settings.ControllerSecret)
		result.healthy = client.Healthy()
		if result.healthy {
			result.groups, _ = client.Groups()
			if connections, err := client.Connections(); err == nil {
				result.connections = connections
				result.connectionsAt = time.Now()
			}
		}
		result.profile, result.profileStamp = loadProfile(m.settings.OVPNPath, prevProfile, prevStamp)
		result.logs = manager.TailLog(m.paths.LogFile, 200)
		return snapshotMsg(result)
	}
}

// profileStamp 用路径、修改时间和大小标识文件版本；文件不可读时返回空。
func profileStamp(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%s|%d|%d", path, info.ModTime().UnixNano(), info.Size())
}

// loadProfile 只在 OVPN 文件变化时重新解析，避免 2 秒 tick 反复读证书。
func loadProfile(path string, prev *ovpn.Profile, prevStamp string) (*ovpn.Profile, string) {
	if path == "" {
		return nil, ""
	}
	stamp := profileStamp(path)
	if stamp == "" {
		return nil, ""
	}
	if prev != nil && prevStamp == stamp {
		return prev, stamp
	}
	profile, err := ovpn.ParseFile(path)
	if err != nil {
		return nil, ""
	}
	return &profile, stamp
}

func diagnosticsCmd(m Model) tea.Cmd {
	return func() tea.Msg { return diagnosticsMsg(diagnostics.Run(m.paths, m.settings)) }
}

func checkAppUpdateCmd(current string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		status, err := appupdate.Check(ctx, current)
		return appUpdateMsg{status: status, err: err}
	}
}

func startCmd(man manager.Manager, settings config.Settings) tea.Cmd {
	if settings.TunEnabled && !system.CanUseSudoWithoutPrompt() {
		return authorizeAdministratorCmd("start", settings)
	}
	return runStartCmd(man, settings)
}

func runStartCmd(man manager.Manager, settings config.Settings) tea.Cmd {
	return func() tea.Msg {
		pid, err := man.Start(settings)
		return actionMsg{text: fmt.Sprintf("Mihomo 已启动，PID %d", pid), err: err}
	}
}

func stopOpenVPNAndStartCmd(man manager.Manager, settings config.Settings) tea.Cmd {
	if !system.CanUseSudoWithoutPrompt() {
		return authorizeAdministratorCmd("stop-openvpn-and-start", settings)
	}
	return runStopOpenVPNAndStartCmd(man, settings)
}

func runStopOpenVPNAndStartCmd(man manager.Manager, settings config.Settings) tea.Cmd {
	return func() tea.Msg {
		count, err := man.StopOtherOpenVPN()
		if err != nil {
			return actionMsg{err: err}
		}
		pid, err := man.Start(settings)
		return actionMsg{text: fmt.Sprintf("已结束 %d 个 OpenVPN 进程，Mihomo 已启动，PID %d", count, pid), err: err}
	}
}

func stopCmd(man manager.Manager, privileged bool) tea.Cmd {
	if privileged && !system.CanUseSudoWithoutPrompt() {
		return authorizeAdministratorCmd("stop", config.Settings{})
	}
	return runStopCmd(man)
}

func runStopCmd(man manager.Manager) tea.Cmd {
	return func() tea.Msg { return actionMsg{text: "Mihomo 已停止", err: man.Stop()} }
}

// reloadCmd 重新生成配置并重启受管 Mihomo，让设置和规则的修改立即生效。
// 停止以管理员身份运行的 Mihomo、以 TUN 模式重新启动都需要管理员授权。
func reloadCmd(man manager.Manager, settings config.Settings, privileged bool) tea.Cmd {
	if (privileged || settings.TunEnabled) && !system.CanUseSudoWithoutPrompt() {
		return authorizeAdministratorCmd("reload", settings)
	}
	return runReloadCmd(man, settings)
}

func runReloadCmd(man manager.Manager, settings config.Settings) tea.Cmd {
	return func() tea.Msg {
		pid, err := man.Reload(settings)
		return actionMsg{text: fmt.Sprintf("已重新生成配置并重启 Mihomo，PID %d", pid), err: err}
	}
}

// recoverDNSCmd 恢复上次运行遗留的系统 DNS 设置，需要管理员授权。
func recoverDNSCmd(man manager.Manager) tea.Cmd {
	if !system.CanUseSudoWithoutPrompt() {
		return authorizeAdministratorCmd("recover-dns", config.Settings{})
	}
	return runRecoverDNSCmd(man)
}

func runRecoverDNSCmd(man manager.Manager) tea.Cmd {
	return func() tea.Msg {
		restored, err := man.RecoverLeftoverDNS()
		switch {
		case err != nil:
			return actionMsg{err: err}
		case !restored:
			return actionMsg{text: "没有需要恢复的 DNS 设置"}
		default:
			return actionMsg{text: "已恢复启动前的系统 DNS"}
		}
	}
}

// recoverWeChatCmd 只关闭已经直连的腾讯连接，让微信重新建立，不重启 Mihomo。
func recoverWeChatCmd(man manager.Manager) tea.Cmd {
	return func() tea.Msg {
		closed, err := man.RecoverWeChatConnections()
		if err != nil {
			return actionMsg{err: fmt.Errorf("恢复微信图片连接（已刷新 %d 条）: %w", closed, err)}
		}
		return actionMsg{text: fmt.Sprintf("已刷新 %d 条微信直连，Mihomo 保持运行", closed)}
	}
}

// claudeCheckTimeout 限定一次 Claude 线路检测的总时间。
const claudeCheckTimeout = 30 * time.Second

// probeClaude 做一次 Claude 线路检测；测试替换它以避免访问网络。
var probeClaude = func(man manager.Manager, settings config.Settings) claudeguard.Status {
	ctx, cancel := context.WithTimeout(context.Background(), claudeCheckTimeout)
	defer cancel()
	return claudeguard.Observe(ctx, man.ClaudeEnv(settings))
}

func claudeCheckCmd(man manager.Manager, settings config.Settings, manual bool) tea.Cmd {
	return func() tea.Msg {
		return claudeMsg{status: probeClaude(man, settings), manual: manual}
	}
}

func authorizeAdministratorCmd(operation string, settings config.Settings) tea.Cmd {
	return tea.ExecProcess(system.AdministratorAuthorizationCommand(), func(err error) tea.Msg {
		return administratorAuthorizationMsg{operation: operation, settings: settings, err: err}
	})
}

// claudeBlockCmd 安装或卸载直连保护；调用前已经取得管理员授权。
func claudeBlockCmd(enabled bool) tea.Cmd {
	return func() tea.Msg {
		if !enabled {
			return actionMsg{text: "直连保护已关闭：VPN 断开时访问 Claude 会直连", err: claudeblock.Remove()}
		}
		if err := claudeblock.Install(); err != nil {
			return actionMsg{err: err}
		}
		if err := claudeblock.SelfTest(); err != nil && !errors.Is(err, claudeblock.ErrSelfTestOffline) {
			return actionMsg{err: fmt.Errorf("直连保护已安装，但自检没有通过：%w", err)}
		}
		return actionMsg{text: "直连保护已开启：不经 VPN 的 Claude 连接会被拦截，重启后依然有效"}
	}
}

func validateCmd(man manager.Manager, settings config.Settings) tea.Cmd {
	return func() tea.Msg {
		return actionMsg{text: "配置生成并通过 Mihomo 校验", err: man.Validate(settings)}
	}
}

func shareConfigCmd(man manager.Manager, settings config.Settings) tea.Cmd {
	return func() tea.Msg {
		shareConfig, err := man.GenerateShare(settings)
		if err != nil {
			return configShareMsg{err: err}
		}
		session, err := configshare.Start(shareConfig)
		return configShareMsg{session: session, err: err}
	}
}

func waitConfigShareCmd(session *configshare.Session) tea.Cmd {
	return func() tea.Msg {
		<-session.Done()
		return configShareClosedMsg{session: session}
	}
}

func updateAppCmd(version string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		result, err := appupdate.Install(ctx, version)
		return appUpdateResultMsg{version: version, desktop: result.DesktopNote(), err: err}
	}
}

func installCmd(paths config.Paths, channel config.CoreChannel, install *installState) tea.Cmd {
	return func() tea.Msg {
		install.phase.Store(int32(core.PhaseResolve))
		install.downloaded.Store(0)
		install.total.Store(0)
		install.active.Store(true)
		defer install.active.Store(false)
		// 上限兜底：网络挂死时不至于让安装永远处于处理中。
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		version, err := core.Install(ctx, paths, channel, func(phase core.Phase, downloaded, total int64) {
			install.phase.Store(int32(phase))
			install.downloaded.Store(downloaded)
			install.total.Store(total)
		})
		return actionMsg{text: "Mihomo " + version + " 安装完成", err: err}
	}
}

func openDashboardCmd(port int, secret string) tea.Cmd {
	return func() tea.Msg {
		url := controller.DashboardURL(port, secret)
		name, args := desktopOpenArgs(runtime.GOOS, url, false)
		if err := newCommand(name, args...).Run(); err != nil {
			return noticeMsg{err: fmt.Errorf("打开面板失败: %w", err)}
		}
		return noticeMsg{text: "已在浏览器打开面板"}
	}
}

func testRouteCmd(controllerPort int, secret string, mixedPort int, host string, destinationPort int) tea.Cmd {
	return func() tea.Msg {
		connection, err := controller.New(controllerPort, secret).ProbeRoute(mixedPort, host, destinationPort)
		if err != nil {
			return routeTestMsg{err: fmt.Errorf("实际路线测试失败: %w", err)}
		}
		rule := controller.Rule{Type: connection.Rule, Payload: connection.Payload}
		found := rule.Type != "" || len(connection.Chains) > 0
		return routeTestMsg{
			host:   host,
			ip:     connection.Metadata.DestinationIP,
			rule:   rule,
			chains: connection.Chains,
			found:  found,
		}
	}
}

func selectPolicyCmd(port int, secret, group, choice string) tea.Cmd {
	return func() tea.Msg {
		err := controller.New(port, secret).Select(group, choice)
		return actionMsg{text: fmt.Sprintf("%s 已切换为 %s", group, choice), err: err, claudeReset: err == nil}
	}
}

func openPathCmd(path string) tea.Cmd {
	return func() tea.Msg {
		if _, err := os.Stat(path); err != nil {
			return noticeMsg{err: fmt.Errorf("日志文件尚不存在")}
		}
		name, args := desktopOpenArgs(runtime.GOOS, path, true)
		err := newCommand(name, args...).Run()
		return noticeMsg{text: "已在文件管理器中定位日志", err: err}
	}
}

func desktopOpenArgs(goos, target string, reveal bool) (string, []string) {
	if goos == "linux" {
		if reveal {
			target = filepath.Dir(target)
		}
		return "xdg-open", []string{target}
	}
	if reveal {
		return "open", []string{"-R", target}
	}
	return "open", []string{target}
}

const messageTTL = 8 * time.Second

func tickCmd(busy bool) tea.Cmd {
	interval := 2 * time.Second
	if busy {
		// 忙碌时加快重绘，让安装进度和 spinner 及时更新。
		interval = 250 * time.Millisecond
	}
	return tea.Tick(interval, func(value time.Time) tea.Msg { return tickMsg(value) })
}

func ovpnPathCompletions(value string) ([]string, error) {
	if value == "~" {
		return []string{"~/"}, nil
	}
	if strings.HasPrefix(value, "~") && !strings.HasPrefix(value, "~/") {
		return nil, fmt.Errorf("用户目录补全仅支持 ~/ 路径")
	}

	expanded := expandHome(value)
	searchDir, namePrefix := filepath.Split(expanded)
	displayDir, _ := filepath.Split(value)
	if searchDir == "" {
		searchDir = "."
	}
	entries, err := os.ReadDir(searchDir)
	if err != nil {
		return nil, fmt.Errorf("无法读取目录 %s: %w", or(displayDir, "."), err)
	}

	collect := func(caseSensitive bool) []string {
		var matches []string
		for _, entry := range entries {
			name := entry.Name()
			if strings.HasPrefix(name, ".") && !strings.HasPrefix(namePrefix, ".") {
				continue
			}
			matched := strings.HasPrefix(name, namePrefix)
			if !caseSensitive {
				matched = strings.HasPrefix(strings.ToLower(name), strings.ToLower(namePrefix))
			}
			if !matched {
				continue
			}

			info, statErr := os.Stat(filepath.Join(searchDir, name))
			if statErr != nil {
				continue
			}
			if !info.IsDir() && (!info.Mode().IsRegular() || !strings.EqualFold(filepath.Ext(name), ".ovpn")) {
				continue
			}
			candidate := displayDir + name
			if info.IsDir() {
				candidate += string(filepath.Separator)
			}
			matches = append(matches, candidate)
		}
		return matches
	}

	matches := collect(true)
	if len(matches) == 0 && namePrefix != "" {
		matches = collect(false)
	}
	return matches, nil
}

func commonStringPrefix(values []string) string {
	if len(values) == 0 {
		return ""
	}
	prefix := []rune(values[0])
	for _, value := range values[1:] {
		runes := []rune(value)
		limit := min(len(prefix), len(runes))
		i := 0
		for i < limit && prefix[i] == runes[i] {
			i++
		}
		prefix = prefix[:i]
		if len(prefix) == 0 {
			break
		}
	}
	return string(prefix)
}

func normalizeOVPNPath(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("请粘贴或拖入一个 .ovpn 文件")
	}
	if strings.ContainsAny(value, "\r\n") {
		return "", fmt.Errorf("一次只能导入一个 OVPN 文件")
	}

	var path string
	if strings.HasPrefix(strings.ToLower(value), "file://") {
		parsed, err := url.Parse(value)
		if err != nil || (parsed.Host != "" && parsed.Host != "localhost") {
			return "", fmt.Errorf("无法识别拖入的文件地址")
		}
		path = parsed.Path
	} else {
		path = trimMatchingQuotes(value)
		if _, err := os.Stat(expandHome(path)); err != nil {
			words, parseErr := splitShellWords(value)
			if parseErr != nil {
				return "", parseErr
			}
			if len(words) != 1 {
				return "", fmt.Errorf("一次只能导入一个 OVPN 文件")
			}
			path = words[0]
		}
	}

	path = expandHome(path)
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("解析 OVPN 路径: %w", err)
	}
	return filepath.Clean(abs), nil
}

func expandHome(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
	}
	return path
}

func trimMatchingQuotes(value string) string {
	if len(value) >= 2 && ((value[0] == '\'' && value[len(value)-1] == '\'') || (value[0] == '"' && value[len(value)-1] == '"')) {
		return value[1 : len(value)-1]
	}
	return value
}

// splitShellWords handles the quoting and backslash escaping emitted when a
// terminal turns a Finder drop into text. It deliberately does not expand
// variables or run a shell.
func splitShellWords(value string) ([]string, error) {
	var words []string
	var word strings.Builder
	quote := rune(0)
	escaped := false
	started := false
	flush := func() {
		if started {
			words = append(words, word.String())
			word.Reset()
			started = false
		}
	}

	for _, r := range value {
		if escaped {
			word.WriteRune(r)
			escaped = false
			started = true
			continue
		}
		if r == '\\' && quote != '\'' {
			escaped = true
			started = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				word.WriteRune(r)
			}
			started = true
			continue
		}
		if r == '\'' || r == '"' {
			quote = r
			started = true
			continue
		}
		if unicode.IsSpace(r) {
			flush()
			continue
		}
		word.WriteRune(r)
		started = true
	}
	if escaped || quote != 0 {
		return nil, fmt.Errorf("拖入的文件路径包含未闭合的引号或转义")
	}
	flush()
	return words, nil
}

// keyText 返回一次按键应插入的文本。优先使用终端解码出的 input
// （tea.Key.Text，可能是多 rune 的字素簇，如带修饰符的 emoji 或组合
// 重音字符）；没有时退回按键名：单个 rune 视为字面输入，space 视为空格。
func keyText(key, input string) string {
	if input != "" {
		return input
	}
	if utf8.RuneCountInString(key) == 1 {
		return key
	}
	if key == "space" {
		return " "
	}
	return ""
}

var newCommand = func(name string, args ...string) commandRunner {
	return osCommand{name: name, args: args}
}

type commandRunner interface{ Run() error }

type osCommand struct {
	name string
	args []string
}

func (c osCommand) Run() error {
	return runCommand(c.name, c.args...)
}

// localClaudeNode 报告住宅节点是不是本机地址；本机节点总是直接连接。
func localClaudeNode(m Model) bool {
	node, err := claude.Load(m.paths.ConfigDir, m.settings)
	return err == nil && node != nil && claude.IsLocal(node.Server())
}
