package ui

import (
	"fmt"
	"image/color"
	"path/filepath"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/kadaliao/tizi/internal/claude"
	"github.com/kadaliao/tizi/internal/claudeguard"
	"github.com/kadaliao/tizi/internal/configshare"
	"github.com/kadaliao/tizi/internal/controller"
	"github.com/kadaliao/tizi/internal/core"
	"github.com/kadaliao/tizi/internal/diagnostics"
	"github.com/kadaliao/tizi/internal/generator"
	"github.com/kadaliao/tizi/internal/nodes"
)

// 布局常量。面板使用圆角边框（左右各 1 列）加水平 padding 2，
// 因此面板内容宽度 = 面板宽度 - panelChrome。
const (
	panelChrome   = 6
	labelColWidth = 18
	nameColWidth  = 20
	// logViewChrome 是日志面板之外的行数开销：外层 padding、页眉、
	// 消息行、页脚、搜索栏和面板自身的边框与标题。
	logViewChrome = 17
)

var (
	titleStyle    lipgloss.Style
	selectedStyle lipgloss.Style
	dimStyle      lipgloss.Style
	okStyle       lipgloss.Style
	warnStyle     lipgloss.Style
	errorStyle    lipgloss.Style
	borderColor   color.Color
)

func init() {
	applyPalette(true)
}

// applyPalette 根据终端背景明暗重建全部样式；浅色终端沿用暗色值会不可读。
func applyPalette(dark bool) {
	var green, yellow, red, cyan, dim, border, title color.Color
	if dark {
		green, yellow, red, cyan = lipgloss.Color("42"), lipgloss.Color("214"), lipgloss.Color("196"), lipgloss.Color("45")
		dim, border, title = lipgloss.Color("245"), lipgloss.Color("238"), lipgloss.Color("255")
	} else {
		green, yellow, red, cyan = lipgloss.Color("28"), lipgloss.Color("130"), lipgloss.Color("160"), lipgloss.Color("25")
		dim, border, title = lipgloss.Color("240"), lipgloss.Color("250"), lipgloss.Color("235")
	}
	borderColor = border
	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(title)
	selectedStyle = lipgloss.NewStyle().Bold(true).Foreground(cyan)
	dimStyle = lipgloss.NewStyle().Foreground(dim)
	okStyle = lipgloss.NewStyle().Foreground(green)
	warnStyle = lipgloss.NewStyle().Foreground(yellow)
	errorStyle = lipgloss.NewStyle().Foreground(red)
}

func (m Model) View() teaView {
	return newTeaView(m.render())
}

func (m Model) render() string {
	width := m.width
	if width <= 0 {
		width = 80
	}
	contentWidth := max(28, width-8)
	if m.share != nil {
		return m.shareView(width)
	}
	var body string
	switch {
	case m.confirm != "":
		body = m.confirmView(contentWidth)
	case m.help:
		body = m.helpView(contentWidth)
	default:
		switch m.active {
		case tabStatus:
			body = m.statusView(contentWidth)
		case tabConfig:
			body = m.configView(contentWidth)
		case tabPolicy:
			body = m.policyView(contentWidth)
		case tabRules:
			body = m.rulesView(contentWidth)
		case tabConnections:
			body = m.connectionsView(contentWidth)
		case tabLogs:
			body = m.logsView(contentWidth)
		case tabDiagnostics:
			body = m.diagnosticsView(contentWidth)
		}
	}
	parts := []string{m.header(width), body}
	if m.errorText != "" {
		parts = append(parts, renderError(m.errorText))
	} else if m.message != "" {
		parts = append(parts, dimStyle.Render(m.message))
	}
	parts = append(parts, m.footer(width))
	return lipgloss.NewStyle().Padding(1, 2).Width(width).Render(strings.Join(parts, "\n\n"))
}

// renderError 展示可能多行的错误（例如 Mihomo 校验输出），最多 4 行。
func renderError(text string) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) > 4 {
		lines = append(lines[:4], "…")
	}
	return errorStyle.Render("错误  ") + strings.Join(lines, "\n      ")
}

func (m Model) header(width int) string {
	appVersion := or(m.appVersion, "dev")
	brand := titleStyle.Render("TIZI") + "  " + dimStyle.Render("Mihomo TUI "+appVersion)
	var rendered []string
	for i, tab := range tabs {
		label := fmt.Sprintf("%d %s", i+1, tab)
		if i == m.active {
			rendered = append(rendered, selectedStyle.Render("["+label+"]"))
		} else {
			rendered = append(rendered, dimStyle.Render(" "+label+" "))
		}
	}
	tabsLine := strings.Join(rendered, "  ")
	return brand + "\n" + lipgloss.NewStyle().Foreground(borderColor).Render(strings.Repeat("─", max(1, width-4))) + "\n" + tabsLine
}

// statusViewChrome 是状态页面板内容之外的行数：外层 padding、页眉、消息行、
// 页脚和面板自身的边框、padding 与标题。
const statusViewChrome = 16

// minStatusDetails 是窗口再矮也要显示的状态行数。
const minStatusDetails = 3

func (m Model) statusView(width int) string {
	contentWidth := width - panelChrome
	valueWidth := contentWidth - labelColWidth - 1
	details := m.statusDetails(valueWidth)
	actions := m.statusActions()
	cursor := m.cursor
	if cursor >= len(actions) {
		cursor = 0
	}
	// 操作不截断：窄终端里折行显示，「启动前必需」之类的说明不能丢。
	actionRows := make([]string, 0, len(actions))
	actionLines := 0
	for i, action := range actions {
		disabled := m.busy || (action.id == statusClaudeCheck && m.claudeChecking)
		actionRows = append(actionRows, selectable(i == cursor, action.label, disabled))
		actionLines += (lipgloss.Width(action.label) + 2 + contentWidth - 1) / max(1, contentWidth)
	}

	// 窗口太矮时先省略 Claude 告警的说明，再省略排在后面的状态行，保证
	// 操作始终可见。
	notices := m.statusNotices(contentWidth, false)
	if m.height > 0 && m.height-statusViewChrome-actionLines-2-len(notices)-1 < len(details) {
		notices = m.statusNotices(contentWidth, true)
	}
	fixed := actionLines + 2
	if len(notices) > 0 {
		fixed += len(notices) + 1
	}
	if m.height > 0 {
		available := m.height - statusViewChrome - fixed
		if available < len(details) {
			keep := max(minStatusDetails, available-1)
			if keep < len(details) {
				hidden := len(details) - keep
				details = append(details[:keep:keep], dimStyle.Render(truncate(fmt.Sprintf("（另有 %d 项状态，拉高窗口可见）", hidden), contentWidth)))
			}
		}
	}

	var rows []string
	if len(notices) > 0 {
		rows = append(rows, notices...)
		rows = append(rows, "")
	}
	if !m.hasCore() {
		rows = append(rows, titleStyle.Render("下一步"))
		rows = append(rows, actionRows...)
		rows = append(rows, "", titleStyle.Render("当前状态"))
		rows = append(rows, details...)
	} else {
		rows = append(rows, details...)
		rows = append(rows, "", titleStyle.Render("操作"))
		rows = append(rows, actionRows...)
	}
	return panel("运行状态", strings.Join(rows, "\n"), width)
}

// statusNotices 是状态页顶部需要处理的问题，每条一行；compact 时省略
// Claude 告警的说明。
func (m Model) statusNotices(width int, compact bool) []string {
	var notices []string
	if !m.hasCore() {
		notices = append(notices, warnStyle.Render(truncate("尚未安装 Mihomo 核心。安装到当前用户目录，不需要 sudo。", width)))
	}
	switch m.claude.Level {
	case claudeguard.LevelAlert, claudeguard.LevelWarn:
		style := warnStyle
		if m.claude.Level == claudeguard.LevelAlert {
			style = errorStyle
		}
		notices = append(notices, style.Render(truncate("Claude："+m.claude.Title, width)))
		if m.claude.Detail != "" && !compact {
			notices = append(notices, dimStyle.Render(truncate(m.claude.Detail, width)))
		}
	}
	if m.dnsPending {
		notices = append(notices, warnStyle.Render(truncate("上次运行遗留的系统 DNS 设置尚未恢复，选「恢复系统 DNS」修复。", width)))
	}
	if len(m.snapshot.vpn.Tunnelblick) > 0 {
		notices = append(notices, warnStyle.Render(truncate("检测到 Tunnelblick。请先停止它，再由本工具独立启动 TUN。", width)))
	}
	return notices
}

// statusDetails 按重要程度排列状态行；窗口太矮时从后往前省略。
func (m Model) statusDetails(valueWidth int) []string {
	status := errorStyle.Render("已停止")
	if m.snapshot.managed.Running {
		status = okStyle.Render(fmt.Sprintf("运行中  PID %d", m.snapshot.managed.PID))
	}
	details := []string{row("受管 Mihomo", status)}
	if m.snapshot.healthy {
		details = append(details, row("实时网速", truncate(m.trafficText(), valueWidth)))
	}
	details = append(details,
		row("Claude 监测", truncate(m.claudeStatusText(), valueWidth)),
		row("Claude 出口", truncate(m.claudeExitText(), valueWidth)),
		row("线路", truncate(m.routeSummary(), valueWidth)),
		row("转发模式", boolText(m.settings.TunEnabled, "TUN 全局接管", "仅本地代理端口")),
		row("本地代理", truncate(m.localProxyText(), valueWidth)),
		row("国内流量", boolText(m.settings.ChinaDirect, "直连", "经外网代理")),
		row("内网 OVPN", truncate(or(m.settings.OVPNPath, "未导入"), valueWidth)),
	)
	if m.settings.OVPNPath != "" {
		details = append(details, row("Remote", truncate(or(m.settings.Remote, "全部"), valueWidth)))
	}
	controllerStatus := dimStyle.Render("不可访问")
	if m.snapshot.healthy {
		controllerStatus = okStyle.Render(fmt.Sprintf("在线（127.0.0.1:%d）", m.settings.ControllerPort))
	}
	vpnStatus := okStyle.Render("无")
	if len(m.snapshot.vpn.Tunnelblick) > 0 {
		vpnStatus = warnStyle.Render(fmt.Sprintf("检测到 Tunnelblick，%d 个 OpenVPN 进程", len(m.snapshot.vpn.Tunnelblick)))
	} else if len(m.snapshot.vpn.OpenVPN) > 0 {
		vpnStatus = warnStyle.Render(fmt.Sprintf("检测到 OpenVPN，%d 个进程", len(m.snapshot.vpn.OpenVPN)))
	}
	coreVersion := m.snapshot.managed.CoreVersion
	if coreVersion == "" {
		coreVersion = boolText(m.hasCore(), "正在读取...", "未安装")
	}
	details = append(details,
		row("Tailscale", truncate(m.tailscaleText(), valueWidth)),
		row("Controller", controllerStatus),
		row("VPN 冲突", vpnStatus),
		row("Mihomo 核心", truncate(coreVersion, valueWidth)),
		row("APP 版本", truncate(m.appVersionText(), valueWidth)),
	)
	return details
}

func (m Model) appVersionText() string {
	text := or(m.appVersion, "dev")
	switch {
	case !m.appUpdateChecked:
		return text + "（正在检查更新...）"
	case m.appUpdateError != "":
		return text + "（更新检查失败）"
	case m.appUpdateAvailable:
		return text + "（最新 " + m.latestAppVersion + "）"
	case m.appUpdated:
		return text + "（已更新至 " + m.latestAppVersion + "，按 r 用新版本重启）"
	default:
		return text + "（已是最新）"
	}
}

// trafficText 汇总全部活动连接的实时网速。
func (m Model) trafficText() string {
	var download, upload float64
	for _, rate := range m.connectionRates {
		download += rate.Download
		upload += rate.Upload
	}
	return fmt.Sprintf("↓ %s  ↑ %s · %d 条连接", formatByteRate(download), formatByteRate(upload), len(m.snapshot.connections))
}

// localProxyText 说明本地代理端口，以及局域网设备连接时是否要求认证。
func (m Model) localProxyText() string {
	host := "127.0.0.1"
	if m.settings.AllowLAN {
		host = "0.0.0.0"
	}
	text := fmt.Sprintf("%s:%d（HTTP/SOCKS5）", host, m.settings.MixedPort)
	if m.settings.AllowLAN {
		text += boolText(m.settings.ProxyAuthEnabled(), " · 局域网需要用户名和密码", " · 局域网不要求认证")
	}
	return text
}

func (m Model) tailscaleText() string {
	if !m.settings.TailscaleEnabled {
		return "关闭"
	}
	text := "开启（设备名 " + generator.TailscaleHostname(m.settings) + "）"
	if !tailscaleLoggedIn(m) && m.settings.TailscaleAuthKey == "" {
		text += " · 首次登录需要 Auth Key"
	}
	return text
}

// claudeExitText 说明 Claude 走哪类出口，以及最近一次检测看到的出口 IP。
func (m Model) claudeExitText() string {
	text := "代理节点"
	if claude.Configured(m.paths.ConfigDir, m.settings) {
		text = "住宅节点（经前置代理链式连接）"
		if m.settings.ClaudeProxyDirect || localClaudeNode(m) || !claude.Chainable(m.paths.ConfigDir, m.settings) {
			text = "住宅节点（直接连接）"
		}
	}
	if ip := m.claude.ExitIP; ip != "" {
		text += " · " + ip
		if m.claude.Country != "" {
			text += "（" + m.claude.Country + "）"
		}
	}
	return text
}

// claudeStatusText 是 Claude 访问监测的结论，按级别着色。
func (m Model) claudeStatusText() string {
	enabled := claudeguard.Enabled(m.paths.ConfigDir, m.settings)
	if m.claude.Level == "" || (m.claude.Level == claudeguard.LevelChecking && m.claude.CheckedAt.IsZero()) {
		switch {
		case m.claudeChecking:
			return dimStyle.Render("正在检测...")
		case enabled:
			return dimStyle.Render("等待检测")
		default:
			return dimStyle.Render("未开启（选「检测 Claude 线路」手动检查一次）")
		}
	}
	style := dimStyle
	switch m.claude.Level {
	case claudeguard.LevelOK:
		style = okStyle
	case claudeguard.LevelWarn:
		style = warnStyle
	case claudeguard.LevelAlert:
		style = errorStyle
	}
	text := m.claude.Title
	if m.claude.Pending > 0 {
		text += " · 正在复查"
	}
	if !m.claude.CheckedAt.IsZero() {
		text += " · " + m.claude.CheckedAt.Format("15:04:05") + " 检测"
	}
	if !enabled {
		text += " · 未开启自动监测"
	}
	return style.Render(text)
}

func (m Model) configView(width int) string {
	valueWidth := width - panelChrome - 2 - labelColWidth - 1
	var rows []string
	first, last := configWindow(m.cursor, len(configFields), m.height)
	if first > 0 {
		rows = append(rows, dimStyle.Render(fmt.Sprintf("↑ 还有 %d 项", first)))
	}
	for i, field := range configFields {
		if i < first || i >= last {
			continue
		}
		var value string
		if m.editing && m.editField == i {
			// editingValue 自己完成两侧裁剪并保证光标可见，
			// 再套一层 truncate 会把光标裁掉。
			value = m.editingValue(valueWidth)
		} else {
			value = truncate(field.value(m), valueWidth)
		}
		line := padRight(field.label, labelColWidth) + " " + value
		rows = append(rows, selectable(i == m.cursor, line, false))
	}
	if last < len(configFields) {
		rows = append(rows, dimStyle.Render(fmt.Sprintf("↓ 还有 %d 项", len(configFields)-last)))
	}
	rows = append(rows, "", dimStyle.Render("OVPN：直接粘贴或从文件管理器拖入终端  s 扫描  Enter/e 编辑或选择  编辑时清空并回车可移除"))
	if m.editing && m.editField == fieldOVPN {
		rows = append(rows, dimStyle.Render("Tab 补全路径并切换候选  Shift+Tab 反向  ←/→ 移动光标  Esc 取消"))
		if len(m.pathCompletions) > 1 {
			labels := make([]string, 0, min(5, len(m.pathCompletions)))
			for _, candidate := range m.pathCompletions[:min(5, len(m.pathCompletions))] {
				isDir := strings.HasSuffix(candidate, string(filepath.Separator))
				label := filepath.Base(strings.TrimSuffix(candidate, string(filepath.Separator)))
				if isDir {
					label += string(filepath.Separator)
				}
				labels = append(labels, label)
			}
			suffix := ""
			if len(m.pathCompletions) > len(labels) {
				suffix = fmt.Sprintf(" 等 %d 项", len(m.pathCompletions))
			}
			rows = append(rows, dimStyle.Render(truncate("候选："+strings.Join(labels, "  ")+suffix, width-panelChrome)))
		}
	}
	if p := m.picker; p != nil {
		rows = append(rows, "", titleStyle.Render(p.title))
		for i, value := range p.values {
			label := value
			if p.kind == pickerRemote && value == "" {
				label = "全部 remote"
			}
			rows = append(rows, "  "+selectable(i == p.cursor, truncate(label, width-panelChrome-4), false))
		}
		rows = append(rows, dimStyle.Render("Enter 选择  Esc 取消"))
	} else if len(m.scannedOVPN) > 0 {
		rows = append(rows, dimStyle.Render(fmt.Sprintf("已发现 %d 个 OVPN，按 s 选择", len(m.scannedOVPN))))
	}
	return panel("连接配置", strings.Join(rows, "\n"), width)
}

// configWindow 返回配置页在当前终端高度下能显示的字段范围，保证光标所在
// 行可见。高度未知（0）时全部显示。
func configWindow(cursor, total, height int) (int, int) {
	visible := height - 22
	if height <= 0 || visible >= total {
		return 0, total
	}
	visible = max(visible, 6)
	first := min(max(cursor-visible/2, 0), total-visible)
	return first, first + visible
}

// routeSummary 用一句话说明配置了哪些线路。
func (m Model) routeSummary() string {
	var parts []string
	if m.settings.Subscription != "" {
		parts = append(parts, "订阅")
	}
	if file, err := nodes.Load(m.paths.Proxies, nil); err == nil && file.Count() > 0 {
		parts = append(parts, fmt.Sprintf("%d 个节点", file.Count()))
	}
	if m.settings.OVPNPath != "" {
		parts = append(parts, "内网")
	}
	if m.settings.TailscaleEnabled {
		parts = append(parts, "Tailscale")
	}
	if len(parts) == 0 {
		return "未配置（到「配置」页填写订阅地址）"
	}
	return strings.Join(parts, " + ")
}

// editingValue 渲染带光标的编辑内容，自行完成两侧裁剪：
// 光标前的部分超宽时向左裁剪，光标后的部分按剩余宽度截断，
// 保证光标始终可见且整行不超过 width。
func (m Model) editingValue(width int) string {
	runes := []rune(m.editBuffer)
	cursor := min(m.editCursor, len(runes))
	before, after := string(runes[:cursor]), string(runes[cursor:])
	budget := width - 1 // 光标占一列
	if budget < 2 {
		return selectedStyle.Render("▌")
	}
	if lipgloss.Width(before) > budget {
		before = "…" + tailClip(before, budget-1)
	}
	remaining := budget - lipgloss.Width(before)
	if remaining <= 0 {
		after = ""
	} else if lipgloss.Width(after) > remaining {
		after = ansi.Truncate(after, remaining, "…")
	}
	return before + selectedStyle.Render("▌") + after
}

// tailClip 保留字符串末尾不超过 width 显示列的部分。
func tailClip(value string, width int) string {
	runes := []rune(value)
	used := 0
	i := len(runes)
	for i > 0 {
		w := lipgloss.Width(string(runes[i-1]))
		if used+w > width {
			break
		}
		used += w
		i--
	}
	return string(runes[i:])
}

func (m Model) policyView(width int) string {
	if !m.snapshot.healthy {
		return panel("策略组", dimStyle.Render("Controller 当前不可访问。启动 Mihomo 后可在这里切换代理组。"), width)
	}
	if len(m.snapshot.groups) == 0 {
		return panel("策略组", dimStyle.Render("没有可切换的策略组。"), width)
	}
	var rows []string
	for i, group := range m.snapshot.groups {
		line := padRight(group.Name, nameColWidth) + " " + selectedStyle.Render(group.Now)
		rows = append(rows, selectable(!m.choosing && i == m.cursor, line, false))
		if m.choosing && i == m.cursor {
			for j, choice := range group.Choices {
				label := choice
				if choice == group.Now {
					label += "  " + okStyle.Render("●")
				}
				rows = append(rows, "   "+selectable(j == m.choice, label, false))
			}
		}
	}
	rows = append(rows, "", dimStyle.Render("Enter 选择  ↑/↓ 移动  Esc 返回策略组"))
	return panel("策略组", strings.Join(rows, "\n"), width)
}

func (m Model) rulesView(width int) string {
	valueWidth := width - panelChrome - labelColWidth - 1
	website := m.ruleInput
	if m.editing && m.editField == editRuleWebsite {
		website = m.editingValue(valueWidth)
	} else {
		website = truncate(or(website, "输入网址或域名"), valueWidth)
	}
	rows := []string{
		selectable(m.cursor == 0, row("测试网站", website), m.busy),
		selectable(m.cursor == 1, "测试当前路线", m.busy || strings.TrimSpace(m.ruleInput) == ""),
		selectable(m.cursor == 2, row("规则路线", ruleTargets[m.ruleTarget]), false),
		selectable(m.cursor == ruleRowSave, "保存域名规则", false),
		selectable(m.cursor == ruleRowReload, boolText(m.snapshot.managed.Running, "重载 Mihomo 使规则生效", "启动 Mihomo 使规则生效"), m.busy),
	}
	if m.ruleTested && (m.ruleMatch.Type != "" || len(m.ruleChains) > 0) {
		route := controller.Route(m.ruleChains)
		if route == "" {
			route = controller.ResolveRoute(m.snapshot.groups, m.ruleMatch.Proxy)
		}
		rows = append(rows, "", titleStyle.Render("实际连接路线"))
		rows = append(rows, row("域名", truncate(m.ruleHost, valueWidth)))
		if m.ruleIP != "" {
			rows = append(rows, row("目标 IP", truncate(m.ruleIP, valueWidth)))
		}
		if m.ruleMatch.Type != "" {
			matchedRule := m.ruleMatch.Type
			if m.ruleMatch.Payload != "" {
				matchedRule += "," + m.ruleMatch.Payload
			}
			rows = append(rows, row("命中规则", truncate(matchedRule, valueWidth)))
		}
		rows = append(rows, row("实际链路", truncate(route, valueWidth)))
	} else if m.ruleTested {
		rows = append(rows, "", warnStyle.Render("Mihomo 已完成连接，但没有返回可识别的路线。"))
	}
	rows = append(rows, "", titleStyle.Render("已保存的域名规则"))
	if len(m.userRules) == 0 {
		rows = append(rows, dimStyle.Render("暂无。输入网站后选择路线并保存。"))
	} else {
		for _, rule := range m.userRules {
			rows = append(rows, truncate("DOMAIN-SUFFIX,"+rule.Domain+" -> "+rule.Target, width-panelChrome))
		}
	}
	rows = append(rows, "", dimStyle.Render("Enter 编辑/测试/切换/保存/重载  测试只建立连接，不请求网页内容"))
	return panel("网站路线与自定义规则", strings.Join(rows, "\n"), width)
}

// searchRow 渲染搜索栏：搜索态显示光标；已有查询时显示命中数。
// 只要搜索态激活就必须画出来，否则按键被搜索框吞掉而用户毫无提示。
func searchRow(box searchBox, matched, total, width int) string {
	if box.active {
		return selectedStyle.Render(truncate("搜索: "+box.query+"_", width-panelChrome))
	}
	if box.query != "" {
		label := fmt.Sprintf("搜索: %s (%d/%d)", box.query, matched, total)
		return selectedStyle.Render(truncate(label, width-panelChrome))
	}
	return ""
}

func (m Model) connectionsView(width int) string {
	if !m.snapshot.healthy {
		body := dimStyle.Render("Controller 当前不可访问。启动 Mihomo 后可在这里查看活动连接和实时网速。")
		if search := searchRow(m.connectionSearch, 0, 0, width); search != "" {
			body = search + "\n\n" + body
		}
		return panel("当前连接", body, width)
	}

	connections := m.filteredConnections()
	search := searchRow(m.connectionSearch, len(connections), len(m.snapshot.connections), width)
	if len(m.snapshot.connections) == 0 {
		body := dimStyle.Render("当前没有活动连接。打开网站后，这里会每 2 秒更新。")
		if search != "" {
			body = search + "\n\n" + body
		}
		return panel("当前连接", body, width)
	}
	if len(connections) == 0 {
		return panel("当前连接", search+"\n\n"+dimStyle.Render("没有匹配的连接。Esc 清空搜索"), width)
	}

	visible := m.connectionVisibleLines()
	start := min(m.connectionScroll, max(0, len(connections)-visible))
	end := min(len(connections), start+visible)
	contentWidth := max(1, width-panelChrome)
	showRoute := contentWidth >= 66
	rateWidth := 27
	routeWidth := 18
	targetWidth := max(10, contentWidth-rateWidth-1)
	if showRoute {
		targetWidth = max(10, contentWidth-rateWidth-routeWidth-2)
	}
	rows := make([]string, 0, end-start+1)
	header := padRight("目标", targetWidth)
	if showRoute {
		header += " " + padRight("路线", routeWidth)
	}
	header += " " + padRight("实时网速", rateWidth)
	rows = append(rows, dimStyle.Render(truncate(header, contentWidth)))
	for _, connection := range connections[start:end] {
		line := padRight(truncate(connection.Destination(), targetWidth), targetWidth)
		if showRoute {
			route := or(controller.Route(connection.Chains), connection.Rule)
			line += " " + padRight(truncate(or(route, "—"), routeWidth), routeWidth)
		}
		rate, sampled := m.connectionRates[connection.ID]
		line += " " + connectionRateText(rate, sampled)
		rows = append(rows, truncate(line, contentWidth))
	}

	hint := "/ 搜索  r 刷新  ↑/↓ 滚动  PgUp/PgDn 翻页  g/G 首条/末条"
	if m.connectionSearch.active {
		hint = "输入关键字实时筛选目标、进程、路线和规则  Enter 完成  Esc 清空  Backspace 删除"
	} else if end < len(connections) {
		hint = fmt.Sprintf("第 %d-%d / %d 条  ↓ 继续  / 搜索", start+1, end, len(connections))
	} else if start > 0 {
		hint = fmt.Sprintf("第 %d-%d / %d 条  ↑ 返回  g 回到首条  / 搜索", start+1, end, len(connections))
	}
	bodyParts := make([]string, 0, 3)
	if search != "" {
		bodyParts = append(bodyParts, search)
	}
	bodyParts = append(bodyParts, strings.Join(rows, "\n"), dimStyle.Render(truncate(hint, contentWidth)))
	title := fmt.Sprintf("当前连接 · %d 条 · 每 2 秒刷新", len(m.snapshot.connections))
	return panel(title, strings.Join(bodyParts, "\n\n"), width)
}

func connectionRateText(rate connectionRate, sampled bool) string {
	if !sampled {
		return "↓        —  ↑        —"
	}
	return fmt.Sprintf("↓ %8s  ↑ %8s", formatByteRate(rate.Download), formatByteRate(rate.Upload))
}

func formatByteRate(bytesPerSecond float64) string {
	switch {
	case bytesPerSecond >= 1<<30:
		return fmt.Sprintf("%.1fGiB/s", bytesPerSecond/(1<<30))
	case bytesPerSecond >= 1<<20:
		return fmt.Sprintf("%.1fMiB/s", bytesPerSecond/(1<<20))
	case bytesPerSecond >= 1<<10:
		return fmt.Sprintf("%.1fKiB/s", bytesPerSecond/(1<<10))
	default:
		return fmt.Sprintf("%.0fB/s", bytesPerSecond)
	}
}

func (m Model) logsView(width int) string {
	if len(m.snapshot.logs) == 0 {
		body := dimStyle.Render("尚无日志。启动 Mihomo 后会写入：\n" + m.paths.LogFile)
		if search := searchRow(m.logSearch, 0, 0, width); search != "" {
			body = search + "\n\n" + body
		}
		return panel("Mihomo 日志", body, width)
	}
	visible := m.logVisibleLines()
	lines := m.filteredLogs()
	search := searchRow(m.logSearch, len(lines), len(m.snapshot.logs), width)
	if len(lines) == 0 {
		body := search + "\n\n" + dimStyle.Render("没有匹配的日志。Esc 清空搜索")
		return panel("Mihomo 日志", body, width)
	}
	scroll := min(m.logScroll, max(0, len(lines)-visible))
	end := len(lines) - scroll
	start := max(0, end-visible)
	window := make([]string, end-start)
	for i, line := range lines[start:end] {
		window[i] = renderLogLine(line, width-panelChrome)
	}
	hint := " / 搜索  r 刷新  ↑/↓ 滚动  PgUp/PgDn 翻页  g/G 顶部/末尾  o 定位文件"
	if m.logSearch.active {
		hint = "输入关键字实时筛选  Enter 完成  Esc 清空  Backspace 删除"
	} else if scroll > 0 {
		hint = fmt.Sprintf("回看中，距末尾 %d 行  G 回到末尾  / 搜索  o 定位文件", scroll)
	}
	bodyParts := make([]string, 0, 3)
	if search != "" {
		bodyParts = append(bodyParts, search)
	}
	bodyParts = append(bodyParts, strings.Join(window, "\n"), dimStyle.Render(hint))
	return panel("Mihomo 日志", strings.Join(bodyParts, "\n\n"), width)
}

func renderLogLine(line string, width int) string {
	timestamp, level, message := parseLogLine(line)
	parts := make([]string, 0, 3)
	if timestamp != "" {
		parts = append(parts, dimStyle.Render(timestamp))
	}
	if level != "" {
		parts = append(parts, logLevelStyle(level).Render(padRight(strings.ToUpper(level), 5)))
	}
	parts = append(parts, logLevelStyle(level).Render(message))
	return truncate(strings.Join(parts, " "), width)
}

func parseLogLine(line string) (timestamp, level, message string) {
	if strings.HasPrefix(line, "time=") {
		timestamp = compactLogTime(logField(line, "time"))
		level = logField(line, "level")
		message = logField(line, "msg")
		if message != "" {
			return timestamp, level, message
		}
	}

	if bracket := strings.Index(line, "["); bracket > 0 {
		candidate := strings.ToLower(line[:bracket])
		if isLogLevel(candidate) {
			if end := strings.Index(line[bracket:], "]"); end >= 0 {
				return compactLogTime(line[bracket+1 : bracket+end]), candidate, strings.TrimSpace(line[bracket+end+1:])
			}
		}
	}
	return "", detectedLogLevel(line), line
}

func logField(line, name string) string {
	start := strings.Index(line, name+"=")
	if start < 0 {
		return ""
	}
	value := line[start+len(name)+1:]
	if value == "" {
		return ""
	}
	if value[0] == '"' {
		if end := strings.Index(value[1:], "\""); end >= 0 {
			return value[1 : end+1]
		}
	}
	fields := strings.Fields(value)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

func compactLogTime(value string) string {
	if start := strings.Index(value, "T"); start >= 0 && len(value) >= start+9 {
		return value[start+1 : start+9]
	}
	return value
}

func detectedLogLevel(line string) string {
	lower := strings.ToLower(line)
	for _, level := range []string{"fatal", "panic", "error", "warn", "info", "debug", "trace"} {
		if strings.Contains(lower, level) {
			return level
		}
	}
	return ""
}

func isLogLevel(value string) bool {
	switch value {
	case "fatal", "panic", "error", "warn", "warning", "info", "debug", "trace":
		return true
	}
	return false
}

func logLevelStyle(level string) lipgloss.Style {
	switch strings.ToLower(level) {
	case "fatal", "panic", "error":
		return errorStyle
	case "warn", "warning":
		return warnStyle
	case "info":
		return selectedStyle
	case "debug", "trace":
		return dimStyle
	default:
		return lipgloss.NewStyle()
	}
}

func (m Model) diagnosticsView(width int) string {
	if len(m.diagnostics) == 0 {
		return panel("只读诊断", dimStyle.Render("正在诊断..."), width)
	}
	detailWidth := width - panelChrome - nameColWidth - 4
	var rows []string
	for _, check := range m.diagnostics {
		marker := dimStyle.Render("i")
		switch check.Level {
		case diagnostics.OK:
			marker = okStyle.Render("●")
		case diagnostics.Warn:
			marker = warnStyle.Render("▲")
		case diagnostics.Fail:
			marker = errorStyle.Render("×")
		}
		rows = append(rows, marker+"  "+padRight(check.Name, nameColWidth)+" "+truncate(check.Detail, detailWidth))
	}
	rows = append(rows, "", dimStyle.Render("诊断只读取进程、路由、端口和连通性，不修改系统网络。r 刷新"))
	return panel("只读诊断", strings.Join(rows, "\n"), width)
}

// spinnerFrames 是忙碌指示动画帧；宽度均为 1 列。
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func (m Model) footer(width int) string {
	if m.share != nil {
		return dimStyle.Render("Esc/Enter 关闭分享并返回  q 退出")
	}
	busy := ""
	if m.busy {
		label := "处理中..."
		if m.updatingApp {
			label = "正在更新 APP..."
		} else if m.install != nil && m.install.active.Load() {
			label = m.installLabel()
		}
		frame := spinnerFrames[m.spinner%len(spinnerFrames)]
		busy = warnStyle.Render(frame + " " + label + "  ")
	}
	return busy + dimStyle.Render("←/→ 视图  ↑/↓ 选择  Enter 执行  d 面板  r 刷新  ? 帮助  q 退出")
}

// installLabel 按安装阶段给出进度文案，避免核心下完后 GeoIP 阶段
// 数字停在终值、看起来像卡死。
func (m Model) installLabel() string {
	name := "Mihomo 核心"
	switch core.Phase(m.install.phase.Load()) {
	case core.PhaseResolve:
		return "正在查询 Mihomo " + coreChannelText(m.settings.CoreChannel) + " 最新版本..."
	case core.PhaseGeoIP:
		name = "GeoIP 数据"
	}
	downloaded := float64(m.install.downloaded.Load()) / (1 << 20)
	if total := m.install.total.Load(); total > 0 {
		percent := m.install.downloaded.Load() * 100 / total
		return fmt.Sprintf("下载 %s %.1f / %.1f MiB (%d%%)", name, downloaded, float64(total)/(1<<20), percent)
	}
	return fmt.Sprintf("下载 %s %.1f MiB", name, downloaded)
}

func (m Model) confirmView(width int) string {
	var title, text string
	switch m.confirm {
	case confirmQuit:
		title = "退出确认"
		text = warnStyle.Render("受管 Mihomo 正在运行") + "\n\n" +
			"停止并退出会结束 Mihomo 并恢复系统 DNS。\n" +
			"保持运行退出后，可用 tizi stop 停止。\n\n" +
			selectedStyle.Render("停止并退出：s / Enter") + "    " +
			selectedStyle.Render("保持运行退出：k") + "    取消：Esc"
	case confirmReinstall:
		title = "更新确认"
		text = warnStyle.Render("Mihomo 正在运行") + "\n\n" +
			"目标通道：" + coreChannelText(m.settings.CoreChannel) + "\n" +
			"更新核心不会中断当前进程，但新版本要重载 Mihomo 后才生效。\n\n" +
			selectedStyle.Render("继续更新：y") + "    取消：n / Esc"
	case confirmStopOpenVPN:
		title = "结束 OpenVPN 确认"
		text = warnStyle.Render("即将结束检测到的 OpenVPN 进程") + "\n\n" +
			"这会断开这些 OpenVPN 连接；Tunnelblick 等客户端仍可能保持打开。\n" +
			"确认后将结束 OpenVPN 进程并启动 Mihomo。\n\n" +
			selectedStyle.Render("确认结束并启动：y / Enter") + "    取消：n / Esc"
	default:
		title = "启动确认"
		text = warnStyle.Render("检测到其他 OpenVPN 进程正在运行") + "\n\n" +
			"并行启动 Mihomo TUN 可能产生路由和 DNS 冲突。\n\n" +
			selectedStyle.Render("结束其他 OpenVPN：k") + "    " +
			selectedStyle.Render("仍要尝试：y") + "    取消：n / Esc"
	}
	return panel(title, text, width)
}

func (m Model) shareView(width int) string {
	contentWidth := max(1, width-4)
	qrCode, qrWidth, err := configshare.RenderQRCode(m.share.URL, m.darkBackground)
	header := titleStyle.Render("Clash Mi 扫码导入") + "  " + warnStyle.Render("配置含订阅、节点密码或证书，请勿分享")
	rows := []string{truncate(header, contentWidth)}
	if m.height >= 27 {
		rows = append(rows, dimStyle.Render("我的配置 → 右上角 + → 扫描二维码；手机和电脑需在同一局域网"))
	}
	switch {
	case err != nil:
		rows = append(rows, errorStyle.Render(err.Error()))
	case qrWidth > contentWidth:
		rows = append(rows, warnStyle.Render(fmt.Sprintf("终端过窄，二维码需要至少 %d 列；请拉宽窗口。", qrWidth+4)))
	default:
		rows = append(rows, lipgloss.NewStyle().Width(contentWidth).Align(lipgloss.Center).Render(qrCode))
	}
	rows = append(rows, selectedStyle.Render("Clash Mi 选择「扫描二维码」后确认保存")+"  "+dimStyle.Render("Enter/Esc 关闭 · "+m.share.ExpiresAt.Format("15:04:05")+" 自动关闭"))
	if m.height >= 29 {
		rows = append(rows, dimStyle.Render("配置地址 "+truncate(m.share.URL, max(1, contentWidth-lipgloss.Width("配置地址 ")))))
	}
	return lipgloss.NewStyle().Padding(0, 2).Width(width).Render(strings.Join(rows, "\n"))
}

func (m Model) helpView(width int) string {
	text := "1-7 或 h/l ←/→  切换视图      j/k ↑/↓  移动选择\n" +
		"Enter/Space  执行或展开       d  打开 Web 面板\n" +
		"状态页：启动/停止、重载 Mihomo（让设置和规则生效）、校验配置、安装或更新核心、\n" +
		"        刷新微信直连、检测 Claude 线路、Clash Mi 扫码导入；遗留 DNS 时可恢复系统 DNS\n" +
		"配置页：Enter 编辑或选择，Remote 和 s 扫描都是列表选择\n" +
		"        选中 OVPN 行可直接粘贴或从文件管理器拖入\n" +
		"        节点文件和住宅节点文件（claude-proxy.yaml）用 $EDITOR 编辑\n" +
		"编辑中：←/→ 移动光标  Home/End 行首行尾  Tab 补全  Ctrl+U/K 删至行首/行尾\n" +
		"策略页：Enter 展开并切换当前策略\n" +
		"规则页：输入网址后 Enter 测试当前路线；选择路线后保存域名规则，再选「重载 Mihomo」生效\n" +
		"连接页：实时查看目标、路线和上下行网速；/ 搜索，↑/↓ 滚动\n" +
		"日志页：/ 搜索  Enter 完成  Esc 清空  ↑/↓ 滚动  PgUp/PgDn 翻页  g/G 顶部/末尾  o 定位文件\n" +
		"r 刷新（APP 更新后按 r 用新版本重启）    q 退出（运行中会先确认）    ?/Esc 关闭帮助"
	return panel("快捷键", text, width)
}

func panel(title, body string, width int) string {
	style := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(borderColor).Padding(1, 2).Width(max(20, width))
	return style.Render(titleStyle.Render(title) + "\n\n" + body)
}

func row(label, value string) string {
	return padRight(label, labelColWidth) + " " + value
}

func selectable(selected bool, value string, disabled bool) string {
	prefix := "  "
	style := lipgloss.NewStyle()
	if selected {
		prefix = "› "
		style = selectedStyle
	}
	if disabled {
		style = dimStyle
	}
	return style.Render(prefix + value)
}

// padRight 按终端显示宽度补空格；fmt 的 %-Ns 按 rune 计数，
// 中文标签一字占两列会导致列错位。
func padRight(value string, width int) string {
	gap := width - lipgloss.Width(value)
	if gap <= 0 {
		return value
	}
	return value + strings.Repeat(" ", gap)
}

// truncate 按显示宽度截断，对 ANSI 转义和宽字符安全。
// 任何正数宽度都强制截断，否则窄终端下长值会越过面板边界导致折行。
func truncate(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if lipgloss.Width(value) <= limit {
		return value
	}
	return ansi.Truncate(value, limit, "…")
}

func boolText(value bool, yes, no string) string {
	if value {
		return yes
	}
	return no
}

func or(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
