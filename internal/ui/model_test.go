package ui

import (
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/kadaliao/tizi/internal/appupdate"
	"github.com/kadaliao/tizi/internal/claude"
	"github.com/kadaliao/tizi/internal/claudeguard"
	"github.com/kadaliao/tizi/internal/config"
	"github.com/kadaliao/tizi/internal/configshare"
	"github.com/kadaliao/tizi/internal/controller"
	"github.com/kadaliao/tizi/internal/core"
	"github.com/kadaliao/tizi/internal/manager"
)

const testOVPN = `client
remote vpn.example.com 1194
<ca>
-----BEGIN CERTIFICATE-----
CA-DATA
-----END CERTIFICATE-----
</ca>
<cert>
-----BEGIN CERTIFICATE-----
CERT-DATA
-----END CERTIFICATE-----
</cert>
<key>
-----BEGIN PRIVATE KEY-----
KEY-DATA
-----END PRIVATE KEY-----
</key>
`

func TestPolicyRefreshPreservesSelectionByName(t *testing.T) {
	model := Model{
		active:   2,
		cursor:   1,
		choice:   1,
		choosing: true,
		snapshot: snapshot{groups: []controller.Group{
			{Name: "A", Choices: []string{"a1"}},
			{Name: "B", Choices: []string{"b1", "b2"}},
		}},
	}
	updatedModel, _ := model.Update(snapshotMsg{groups: []controller.Group{
		{Name: "B", Choices: []string{"b2", "b1"}},
		{Name: "A", Choices: []string{"a1"}},
	}})
	updated := updatedModel.(Model)
	if updated.cursor != 0 || updated.choice != 0 || !updated.choosing {
		t.Fatalf("selection identity was not preserved: cursor=%d choice=%d choosing=%v", updated.cursor, updated.choice, updated.choosing)
	}
}

func TestLogSearchFiltersCaseInsensitivelyAndResetsScroll(t *testing.T) {
	model := Model{
		active:    tabLogs,
		logScroll: 5,
		snapshot: snapshot{logs: []string{
			"INFO connected",
			"WARN retrying connection",
			"ERROR connection refused",
		}},
	}

	updatedModel, _ := model.handleLogsKey("/")
	updated := updatedModel.(Model)
	if !updated.logSearch.active {
		t.Fatal("/ should start log search")
	}
	for _, key := range []string{"c", "O", "n", "N"} {
		updatedModel, _ = updated.handleLogsKey(key)
		updated = updatedModel.(Model)
	}
	if updated.logSearch.query != "cOnN" || updated.logScroll != 0 {
		t.Fatalf("unexpected search state: query=%q scroll=%d", updated.logSearch.query, updated.logScroll)
	}
	if got := updated.filteredLogs(); len(got) != 3 {
		t.Fatalf("case-insensitive search got %d lines, want 3", len(got))
	}

	updatedModel, _ = updated.handleLogsKey("backspace")
	updated = updatedModel.(Model)
	if updated.logSearch.query != "cOn" {
		t.Fatalf("backspace query=%q, want cOn", updated.logSearch.query)
	}
	updatedModel, _ = updated.handleLogsKey("enter")
	updated = updatedModel.(Model)
	if updated.logSearch.active {
		t.Fatal("Enter should finish log search")
	}
	updatedModel, _ = updated.handleLogsKey("esc")
	updated = updatedModel.(Model)
	if updated.logSearch.query != "" || updated.logSearch.active {
		t.Fatalf("Esc should clear search: query=%q searching=%v", updated.logSearch.query, updated.logSearch.active)
	}
}

func TestLogSearchCapturesGlobalShortcutKeys(t *testing.T) {
	model := Model{active: tabLogs, logSearch: searchBox{active: true}}
	updatedModel, cmd := model.handleKey(tea.KeyPressMsg(tea.Key{Text: "r", Code: 'r'}))
	updated := updatedModel.(Model)
	if cmd != nil || updated.logSearch.query != "r" || updated.message != "" {
		t.Fatalf("search should capture r: query=%q message=%q cmd=%v", updated.logSearch.query, updated.message, cmd)
	}
	updatedModel, _ = updated.handleKey(tea.KeyPressMsg(tea.Key{Text: "1", Code: '1'}))
	updated = updatedModel.(Model)
	if updated.active != tabLogs || updated.logSearch.query != "r1" {
		t.Fatalf("search should capture 1: active=%d query=%q", updated.active, updated.logSearch.query)
	}
}

func TestConnectionSearchFiltersTargetsProcessesAndRoutes(t *testing.T) {
	web := controller.Connection{ID: "web"}
	web.Metadata.Host = "Api.Example.com"
	web.Metadata.Process = "Browser"
	web.Chains = []string{"proxy-a", "默认出口"}
	dns := controller.Connection{ID: "dns"}
	dns.Metadata.DestinationIP = "1.1.1.1"
	dns.Rule = "MATCH"

	model := Model{active: tabConnections, snapshot: snapshot{connections: []controller.Connection{web, dns}}}
	for _, query := range []string{"example", "BROWS", "proxy-a"} {
		model.connectionSearch.query = query
		filtered := model.filteredConnections()
		if len(filtered) != 1 || filtered[0].ID != "web" {
			t.Fatalf("query %q returned %#v, want web connection", query, filtered)
		}
	}
}

func TestConnectionSearchCapturesGlobalShortcutKeys(t *testing.T) {
	model := Model{active: tabConnections, connectionSearch: searchBox{active: true}}
	updatedModel, cmd := model.handleKey(tea.KeyPressMsg(tea.Key{Text: "r", Code: 'r'}))
	updated := updatedModel.(Model)
	if cmd != nil || updated.connectionSearch.query != "r" || updated.message != "" {
		t.Fatalf("search should capture r: query=%q message=%q cmd=%v", updated.connectionSearch.query, updated.message, cmd)
	}
	updatedModel, _ = updated.handleKey(tea.KeyPressMsg(tea.Key{Text: "5", Code: '5'}))
	updated = updatedModel.(Model)
	if updated.active != tabConnections || updated.connectionSearch.query != "r5" {
		t.Fatalf("search should capture 5: active=%d query=%q", updated.active, updated.connectionSearch.query)
	}
}

func TestConnectionRatesUseCounterDeltaAndElapsedTime(t *testing.T) {
	started := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	before := controller.Connection{ID: "active"}
	before.Download, before.Upload = 100, 50
	after := controller.Connection{ID: "active"}
	after.Metadata.Host = "example.com"
	after.Metadata.DestinationPort = "443"
	after.Download, after.Upload = 2100, 1050
	model := Model{
		active: tabConnections,
		height: 30,
		snapshot: snapshot{
			connections:   []controller.Connection{before},
			connectionsAt: started,
		},
	}

	updatedModel, _ := model.Update(snapshotMsg{
		healthy:       true,
		connections:   []controller.Connection{after},
		connectionsAt: started.Add(2 * time.Second),
	})
	updated := updatedModel.(Model)
	rate, ok := updated.connectionRates["active"]
	if !ok || rate.Download != 1000 || rate.Upload != 500 {
		t.Fatalf("rate = %#v, %v; want 1000/500 B/s", rate, ok)
	}
	rendered := ansi.Strip(updated.connectionsView(100))
	if !strings.Contains(rendered, "example.com:443") || !strings.Contains(rendered, "↓  1000B/s") || !strings.Contains(rendered, "↑   500B/s") {
		t.Fatalf("connection traffic is not rendered: %s", rendered)
	}
}

func TestRulesPageSavesDomainRule(t *testing.T) {
	paths, _ := testPathsAndOVPN(t)
	model := Model{
		paths:      paths,
		active:     tabRules,
		cursor:     3,
		ruleInput:  "https://docs.example.com/guide",
		ruleTarget: 1,
	}
	updatedModel, cmd := model.handleRulesKey("enter")
	updated := updatedModel.(Model)
	if cmd != nil || len(updated.userRules) != 1 {
		t.Fatalf("rule save = %#v, cmd=%v", updated.userRules, cmd)
	}
	if got := updated.userRules[0]; got.Domain != "docs.example.com" || got.Target != "外网代理" {
		t.Fatalf("saved rule = %#v", got)
	}
	if !strings.Contains(updated.message, "重载 Mihomo 后生效") || updated.cursor != ruleRowReload {
		t.Fatalf("save should point at the reload row: message=%q cursor=%d", updated.message, updated.cursor)
	}
}

func TestRulesPageShowsResolvedRoute(t *testing.T) {
	model := Model{
		active:     tabRules,
		ruleTested: true,
		ruleHost:   "example.com",
		ruleMatch:  controller.Rule{Type: "DOMAIN-SUFFIX", Payload: "example.com", Proxy: "外网代理"},
		snapshot:   snapshot{groups: []controller.Group{{Name: "外网代理", Now: "hk-node"}}},
	}
	view := ansi.Strip(model.rulesView(80))
	if !strings.Contains(view, "测试当前路线") || !strings.Contains(view, "实际连接路线") || !strings.Contains(view, "DOMAIN-SUFFIX,example.com") || !strings.Contains(view, "外网代理 -> hk-node") {
		t.Fatalf("route result missing from view: %s", view)
	}
}

func TestRulesPageRecordsInconclusiveTest(t *testing.T) {
	model := Model{active: tabRules, busy: true}
	updatedModel, _ := model.Update(routeTestMsg{host: "example.cn", found: false})
	updated := updatedModel.(Model)
	if updated.busy || !updated.ruleTested || updated.ruleHost != "example.cn" {
		t.Fatalf("inconclusive test state = busy:%v tested:%v host:%q", updated.busy, updated.ruleTested, updated.ruleHost)
	}
	view := ansi.Strip(updated.rulesView(80))
	if !strings.Contains(view, "没有返回可识别的路线") {
		t.Fatalf("inconclusive result missing from view: %s", view)
	}
}

func TestRulesPageShowsActualProbeMetadata(t *testing.T) {
	model := Model{active: tabRules, busy: true}
	updatedModel, _ := model.Update(routeTestMsg{
		host:   "example.com",
		ip:     "203.0.113.8",
		rule:   controller.Rule{Type: "GeoIP", Payload: "US"},
		chains: []string{"hk-node", "外网代理"},
		found:  true,
	})
	updated := updatedModel.(Model)
	view := ansi.Strip(updated.rulesView(80))
	for _, want := range []string{"目标 IP", "203.0.113.8", "GeoIP,US", "外网代理 -> hk-node"} {
		if !strings.Contains(view, want) {
			t.Fatalf("actual probe result %q missing from view: %s", want, view)
		}
	}
}

func TestRulesPageCanExplicitlyRetestCurrentInput(t *testing.T) {
	model := Model{
		active:    tabRules,
		cursor:    1,
		ruleInput: "https://docs.example.com/guide",
		settings:  config.DefaultSettings(),
	}
	updatedModel, cmd := model.handleRulesKey("enter")
	updated := updatedModel.(Model)
	if cmd == nil || !updated.busy || updated.ruleHost != "docs.example.com" || updated.ruleTested {
		t.Fatalf("route test was not started: cmd=%v busy=%v host=%q tested=%v", cmd, updated.busy, updated.ruleHost, updated.ruleTested)
	}
}

func TestParseLogLineCompactsMihomoFormats(t *testing.T) {
	cases := []struct {
		line                      string
		timestamp, level, message string
	}{
		{"time=\"2026-07-17T12:34:56.789Z\" level=info msg=\"Mihomo started\"", "12:34:56", "info", "Mihomo started"},
		{"WARN[2026-07-17T12:34:56.789] retrying connection", "12:34:56", "warn", "retrying connection"},
		{"plain output", "", "", "plain output"},
	}
	for _, tc := range cases {
		timestamp, level, message := parseLogLine(tc.line)
		if timestamp != tc.timestamp || level != tc.level || message != tc.message {
			t.Errorf("parseLogLine(%q) = (%q, %q, %q), want (%q, %q, %q)", tc.line, timestamp, level, message, tc.timestamp, tc.level, tc.message)
		}
	}
}

func TestLogRenderingUsesLevelColors(t *testing.T) {
	rendered := ansi.Strip(renderLogLine("time=\"2026-07-17T12:34:56Z\" level=error msg=\"connection refused\"", 80))
	if rendered != "12:34:56 ERROR connection refused" {
		t.Fatalf("unexpected compact log rendering: %q", rendered)
	}
}

func TestRenderNarrowTerminal(t *testing.T) {
	withoutMihomoOnPath(t)
	settings := config.DefaultSettings()
	model := Model{settings: settings, width: 40, height: 20, snapshot: snapshot{}}
	view := model.render()
	if !strings.Contains(view, "TIZI") || !strings.Contains(view, "运行状态") {
		t.Fatalf("unexpected view: %s", view)
	}
	if !strings.Contains(view, "安装 Mihomo") || !strings.Contains(view, "启动前必需") {
		t.Fatalf("missing core install action is not visible: %s", view)
	}
	if strings.Index(view, "安装 Mihomo") > strings.Index(view, "受管 Mihomo") {
		t.Fatalf("missing core install action should precede status details in a narrow terminal: %s", view)
	}
}

func TestMissingCoreMakesInstallPrimaryAction(t *testing.T) {
	withoutMihomoOnPath(t)
	model := Model{snapshot: snapshot{}}
	actions := model.statusActions()
	if actions[0].id != statusInstall || actions[1].id != statusStart {
		t.Fatalf("unexpected action order without core: %#v", actions)
	}
}

func TestInstalledCoreKeepsStartPrimaryAction(t *testing.T) {
	model := Model{snapshot: snapshot{managed: manager.Status{CorePath: "/tmp/mihomo"}}}
	actions := model.statusActions()
	if actions[0].id != statusStart || actions[2].id != statusInstall {
		t.Fatalf("unexpected action order with core: %#v", actions)
	}
}

func TestStatusOffersClashMiShareAction(t *testing.T) {
	model := Model{snapshot: snapshot{managed: manager.Status{CorePath: "/tmp/mihomo"}}}
	actions := model.statusActions()
	if actions[len(actions)-1].id != statusShare || actions[len(actions)-1].label != "Clash Mi 扫码导入" {
		t.Fatalf("Clash Mi share action missing: %#v", actions)
	}
}

func TestShareViewRendersScannableQRAndSecurityGuidance(t *testing.T) {
	model := Model{
		width:          80,
		height:         24,
		darkBackground: true,
		share: &configshare.Session{
			URL:       "http://192.168.1.20:54321/token/config.yaml",
			ExpiresAt: time.Date(2026, 7, 27, 12, 34, 56, 0, time.Local),
		},
	}
	view := ansi.Strip(model.render())
	for _, want := range []string{"Clash Mi 扫码导入", "扫描二维码", "12:34:56", "请勿分享", "████"} {
		if !strings.Contains(view, want) {
			t.Fatalf("share view missing %q: %s", want, view)
		}
	}
	if lines := strings.Count(view, "\n") + 1; lines > model.height {
		t.Fatalf("share view uses %d lines in a %d-line terminal", lines, model.height)
	}
}

func TestQRCodeAdaptsToTerminalPalette(t *testing.T) {
	const content = "http://192.168.1.20:54321/token/config.yaml"
	dark, darkWidth, err := configshare.RenderQRCode(content, true)
	if err != nil {
		t.Fatal(err)
	}
	light, lightWidth, err := configshare.RenderQRCode(content, false)
	if err != nil {
		t.Fatal(err)
	}
	if darkWidth != lightWidth || dark == light {
		t.Fatalf("palette rendering = widths %d/%d equal=%v", darkWidth, lightWidth, dark == light)
	}
}

func TestAppVersionAppearsInHeaderAndStatus(t *testing.T) {
	model := Model{
		appVersion:       "v0.1.12",
		appUpdateChecked: true,
		width:            80,
		snapshot:         snapshot{managed: manager.Status{CorePath: "/tmp/mihomo"}},
	}
	view := ansi.Strip(model.render())
	if !strings.Contains(view, "Mihomo TUI v0.1.12") || !strings.Contains(view, "APP 版本") || !strings.Contains(view, "已是最新") {
		t.Fatalf("APP version is not visible: %s", view)
	}
}

func TestAvailableAppUpdateAddsExecutableAction(t *testing.T) {
	model := Model{
		appVersion: "v0.1.12",
		snapshot:   snapshot{managed: manager.Status{CorePath: "/tmp/mihomo"}},
	}
	updatedModel, _ := model.Update(appUpdateMsg{status: appupdate.Status{
		Current:   "v0.1.12",
		Latest:    "v0.1.13",
		Available: true,
	}})
	updated := updatedModel.(Model)
	actions := updated.statusActions()
	if actions[len(actions)-1].id != statusUpdateApp || !strings.Contains(actions[len(actions)-1].label, "v0.1.13") {
		t.Fatalf("APP update action missing: %#v", actions)
	}

	updated.cursor = len(actions) - 1
	installingModel, cmd := updated.handleStatusKey("enter")
	installing := installingModel.(Model)
	if cmd == nil || !installing.busy || !installing.updatingApp {
		t.Fatalf("APP update did not start: cmd=%v busy=%v updating=%v", cmd, installing.busy, installing.updatingApp)
	}
}

func TestCompletedAppUpdateRequestsRestart(t *testing.T) {
	model := Model{busy: true, updatingApp: true, appUpdateAvailable: true}
	updatedModel, _ := model.Update(appUpdateResultMsg{version: "v0.1.13"})
	updated := updatedModel.(Model)
	if updated.busy || updated.updatingApp || updated.appUpdateAvailable || !updated.appUpdated {
		t.Fatalf("unexpected completed update state: %#v", updated)
	}
	if !strings.Contains(updated.message, "下次启动生效") {
		t.Fatalf("restart guidance missing: %q", updated.message)
	}
}

func TestCompletedAppUpdateOffersReloadWhenRunning(t *testing.T) {
	model := Model{
		busy:        true,
		updatingApp: true,
		snapshot:    snapshot{managed: manager.Status{Running: true}},
	}
	updatedModel, _ := model.Update(appUpdateResultMsg{version: "v0.1.13"})
	updated := updatedModel.(Model)
	if !updated.appUpdated {
		t.Fatalf("appUpdated not set: %#v", updated)
	}
	if !strings.Contains(updated.message, "按 r 用新版本重启界面并重载 Mihomo") {
		t.Fatalf("reload guidance missing: %q", updated.message)
	}
}

// 更新后本进程仍是旧代码；按 r 必须退出并交给新二进制，而不是在旧进程里
// 重载（那会用旧规则生成配置），也不能在退出时停止运行中的 Mihomo。
func TestRPressedAfterUpdateHandsOverToNewBinary(t *testing.T) {
	for _, running := range []bool{true, false} {
		model := Model{
			appUpdated:       true,
			latestAppVersion: "v0.1.13",
			snapshot:         snapshot{managed: manager.Status{Running: running}},
		}
		updatedModel, cmd := model.handleKey(tea.KeyPressMsg(tea.Key{Text: "r", Code: 'r'}))
		updated := updatedModel.(Model)
		if !updated.RestartAfterUpdate() || !updated.KeepRunning() || updated.busy {
			t.Fatalf("running=%v: restart=%v keepRunning=%v busy=%v", running, updated.RestartAfterUpdate(), updated.KeepRunning(), updated.busy)
		}
		if cmd == nil {
			t.Fatalf("running=%v: quit cmd missing", running)
		}
		if _, ok := cmd().(tea.QuitMsg); !ok {
			t.Fatalf("running=%v: r after update must quit the old TUI", running)
		}
	}
}

func TestConfigFieldConstantsMatchRows(t *testing.T) {
	for field, label := range map[int]string{
		fieldSubscription:        "订阅地址",
		fieldNodes:               "节点文件",
		fieldOVPN:                "内网 OVPN",
		fieldRemote:              "Remote",
		fieldIntranetDomains:     "内网域名",
		fieldIntranetCIDRs:       "内网网段",
		fieldIntranetDNS:         "内网 DNS",
		fieldIntranetHTTP:        "内网 HTTP 出口",
		fieldIntranetProbe:       "内网检查地址",
		fieldTailscale:           "Tailscale",
		fieldTailscaleAuthKey:    "Tailscale Auth Key",
		fieldTailscaleHostname:   "Tailscale 设备名",
		fieldTailscaleControlURL: "Tailscale 控制服务器",
		fieldTailscaleExitNode:   "Tailscale 出口节点",
		fieldTun:                 "TUN 模式",
		fieldAllowLAN:            "局域网代理",
		fieldProxyUsername:       "代理用户名",
		fieldProxyPassword:       "代理密码",
		fieldClaudeProxy:         "Claude 住宅节点",
		fieldClaudeProxyFile:     "住宅节点文件",
		fieldClaudeProxyDirect:   "住宅节点连接方式",
		fieldClaudeGuard:         "Claude 访问监测",
		fieldClaudeBlock:         "Claude 直连保护",
		fieldMixedPort:           "Mixed 端口",
		fieldControllerPort:      "Controller 端口",
		fieldDNSPort:             "DNS 端口",
		fieldCoreChannel:         "核心通道",
	} {
		if configFields[field].label != label {
			t.Errorf("configFields[%d] = %q, want %q", field, configFields[field].label, label)
		}
	}
	if len(configFields) != fieldCoreChannel+1 {
		t.Errorf("configFields has %d rows, constants end at %d", len(configFields), fieldCoreChannel)
	}
}

func TestProxyCredentialsEditPersists(t *testing.T) {
	paths, _ := testPathsAndOVPN(t)
	model := Model{paths: paths, settings: config.DefaultSettings(), cursor: fieldProxyUsername}
	edit := func(m Model, value string) Model {
		updatedModel, _ := m.handleConfigKey("enter")
		updated := updatedModel.(Model)
		updated.setEditBuffer(value)
		committed, _ := updated.commitEdit()
		return committed.(Model)
	}

	model = edit(model, "alice")
	model.cursor = fieldProxyPassword
	model = edit(model, "s3cret")
	loaded, err := config.Load(paths)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ProxyUsername != "alice" || loaded.ProxyPassword != "s3cret" {
		t.Fatalf("persisted credentials = %q / %q", loaded.ProxyUsername, loaded.ProxyPassword)
	}
	if got := configFields[fieldProxyPassword].value(model); strings.Contains(got, "s3cret") {
		t.Fatalf("password must be masked in the config list: %q", got)
	}

	model.cursor = fieldProxyUsername
	if rejected := edit(model, "a:b"); rejected.errorText == "" || rejected.settings.ProxyUsername != "alice" {
		t.Fatalf("username with a colon accepted: %+v", rejected.settings)
	}
}

func TestClaudeProxyEditPersistsAndMasks(t *testing.T) {
	paths, _ := testPathsAndOVPN(t)
	model := Model{paths: paths, settings: config.DefaultSettings(), cursor: fieldClaudeProxy}
	edit := func(m Model, value string) Model {
		updatedModel, _ := m.handleConfigKey("enter")
		updated := updatedModel.(Model)
		updated.setEditBuffer(value)
		committed, _ := updated.commitEdit()
		return committed.(Model)
	}

	model = edit(model, "socks5://alice:s3cret@203.0.113.9:443")
	loaded, err := config.Load(paths)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ClaudeProxy != "socks5://alice:s3cret@203.0.113.9:443" {
		t.Fatalf("persisted address = %q", loaded.ClaudeProxy)
	}
	if got := configFields[fieldClaudeProxy].value(model); strings.Contains(got, "s3cret") {
		t.Fatalf("password must be masked in the config list: %q", got)
	}
	if got := configFields[fieldClaudeGuard].value(model); !strings.Contains(got, "开启") {
		t.Fatalf("a residential node always enables the guard: %q", got)
	}

	if rejected := edit(model, "ss://abc@1.2.3.4:443"); rejected.errorText == "" || rejected.settings.ClaudeProxy != loaded.ClaudeProxy {
		t.Fatalf("unsupported scheme accepted: %+v", rejected.settings)
	}
	if cleared := edit(model, ""); cleared.settings.ClaudeProxy != "" {
		t.Fatalf("empty input must clear the address: %q", cleared.settings.ClaudeProxy)
	}
}

func TestCoreChannelTogglePersistsAlpha(t *testing.T) {
	paths, _ := testPathsAndOVPN(t)
	model := Model{
		paths:    paths,
		settings: config.DefaultSettings(),
		cursor:   len(configFields) - 1,
		snapshot: snapshot{managed: manager.Status{CorePath: "/tmp/mihomo"}},
	}

	updatedModel, cmd := model.handleConfigKey("enter")
	updated := updatedModel.(Model)
	if cmd == nil || updated.settings.CoreChannel != config.CoreChannelAlpha {
		t.Fatalf("channel toggle = %q, cmd=%v", updated.settings.CoreChannel, cmd)
	}
	loaded, err := config.Load(paths)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.CoreChannel != config.CoreChannelAlpha {
		t.Fatalf("persisted channel = %q", loaded.CoreChannel)
	}
	if label := updated.statusActions()[2].label; !strings.Contains(label, "Alpha") {
		t.Fatalf("install action does not show Alpha channel: %q", label)
	}
}

func TestStartWithoutCoreSelectsExecutableInstallAction(t *testing.T) {
	withoutMihomoOnPath(t)
	paths, ovpnPath := testPathsAndOVPN(t)
	settings := config.DefaultSettings()
	settings.OVPNPath = ovpnPath
	model := Model{paths: paths, settings: settings, install: &installState{}, cursor: 1}

	updatedModel, cmd := model.handleStatusKey("enter")
	updated := updatedModel.(Model)
	if cmd != nil {
		t.Fatal("missing core should wait for explicit install confirmation")
	}
	if updated.busy || updated.cursor != 0 {
		t.Fatalf("install action was not selected: busy=%v cursor=%d", updated.busy, updated.cursor)
	}
	if !strings.Contains(updated.errorText, "按 Enter 执行") || !strings.Contains(updated.errorText, "不需要 sudo") {
		t.Fatalf("missing install guidance: %q", updated.errorText)
	}

	confirmedModel, install := updated.handleStatusKey("enter")
	confirmed := confirmedModel.(Model)
	if install == nil || !confirmed.busy {
		t.Fatalf("second Enter should explicitly start installation: cmd=%v busy=%v", install, confirmed.busy)
	}
}

func TestValidateWithoutCoreAlsoSelectsInstallAction(t *testing.T) {
	withoutMihomoOnPath(t)
	paths, ovpnPath := testPathsAndOVPN(t)
	settings := config.DefaultSettings()
	settings.OVPNPath = ovpnPath
	model := Model{paths: paths, settings: settings, install: &installState{}, cursor: 2}

	updatedModel, cmd := model.handleStatusKey("enter")
	updated := updatedModel.(Model)
	if cmd != nil || updated.cursor != 0 || updated.busy {
		t.Fatalf("validate did not redirect to install: cmd=%v cursor=%d busy=%v", cmd, updated.cursor, updated.busy)
	}
}

func TestStartWithoutOVPNGuidesToConfigEdit(t *testing.T) {
	withoutMihomoOnPath(t)
	paths, _ := testPathsAndOVPN(t)
	model := Model{paths: paths, settings: config.DefaultSettings(), install: &installState{}, cursor: 1}

	updatedModel, cmd := model.handleStatusKey("enter")
	updated := updatedModel.(Model)
	if cmd != nil {
		t.Fatal("missing OVPN should not run any action")
	}
	if updated.busy || updated.active != 1 || !updated.editing || updated.editField != 0 {
		t.Fatalf("expected config edit guidance: busy=%v active=%d editing=%v field=%d", updated.busy, updated.active, updated.editing, updated.editField)
	}
	if updated.message == "" {
		t.Fatal("guidance message missing")
	}
}

func TestOpenVPNConflictCanRequestConfirmedStop(t *testing.T) {
	model := Model{confirm: confirmForceStart}

	confirmationModel, cmd := model.handleConfirm("k")
	confirmation := confirmationModel.(Model)
	if cmd != nil || confirmation.confirm != confirmStopOpenVPN || confirmation.busy {
		t.Fatalf("k should open the OpenVPN stop confirmation: cmd=%v confirm=%q busy=%v", cmd, confirmation.confirm, confirmation.busy)
	}
	if view := confirmation.confirmView(80); !strings.Contains(view, "结束 OpenVPN 确认") || !strings.Contains(view, "确认结束并启动") {
		t.Fatalf("unexpected OpenVPN stop confirmation: %s", view)
	}

	startedModel, start := confirmation.handleConfirm("enter")
	started := startedModel.(Model)
	if start == nil || started.confirm != "" || !started.busy {
		t.Fatalf("confirmation should start cleanup: cmd=%v confirm=%q busy=%v", start, started.confirm, started.busy)
	}
}

func TestQuitConfirmWhenManagedRunning(t *testing.T) {
	model := Model{snapshot: snapshot{managed: manager.Status{Running: true, PID: 1}}}

	updatedModel, cmd := model.requestQuit()
	updated := updatedModel.(Model)
	if cmd != nil || updated.confirm != confirmQuit {
		t.Fatalf("running Mihomo should ask before quitting: cmd=%v confirm=%q", cmd, updated.confirm)
	}

	keptModel, quit := updated.handleConfirm("k")
	kept := keptModel.(Model)
	if quit == nil || !kept.keepRunning {
		t.Fatalf("k should quit while keeping Mihomo: cmd=%v keepRunning=%v", quit, kept.keepRunning)
	}

	cancelledModel, _ := updated.handleConfirm("esc")
	cancelled := cancelledModel.(Model)
	if cancelled.confirm != "" || cancelled.keepRunning {
		t.Fatalf("esc should cancel quit: confirm=%q keepRunning=%v", cancelled.confirm, cancelled.keepRunning)
	}
}

func TestQuitWithoutManagedProcessExitsImmediately(t *testing.T) {
	model := Model{}
	_, cmd := model.requestQuit()
	if cmd == nil {
		t.Fatal("idle TUI should quit without confirmation")
	}
}

func TestPasteImportsEscapedOVPNPathWithoutEnteringEdit(t *testing.T) {
	paths, ovpnPath := testPathsAndOVPN(t)
	model := Model{
		paths:    paths,
		settings: config.DefaultSettings(),
		active:   1,
		cursor:   0,
	}
	escaped := strings.ReplaceAll(ovpnPath, " ", `\ `)

	updatedModel, _ := model.Update(tea.PasteMsg{Content: escaped})
	updated := updatedModel.(Model)
	if updated.editing {
		t.Fatal("valid paste should finish editing")
	}
	if updated.settings.OVPNPath != ovpnPath || updated.snapshot.profile == nil {
		t.Fatalf("OVPN was not imported: %#v", updated.settings)
	}
	loaded, err := config.Load(paths)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.OVPNPath != ovpnPath {
		t.Fatalf("pasted path was not saved: %q", loaded.OVPNPath)
	}
}

func TestPasteReplacesExistingOVPNEditBuffer(t *testing.T) {
	paths, ovpnPath := testPathsAndOVPN(t)
	model := Model{paths: paths, settings: config.DefaultSettings(), active: 1, cursor: fieldOVPN}
	model.beginEdit(fieldOVPN, "/old/profile.ovpn")
	model.pathCompletions = []string{"/old/profile.ovpn", "/old/profile-2.ovpn"}
	model.pathCompletionText = model.editBuffer

	updatedModel, _ := model.Update(tea.PasteMsg{Content: "'" + ovpnPath + "'"})
	updated := updatedModel.(Model)
	if updated.settings.OVPNPath != ovpnPath || updated.editing {
		t.Fatalf("paste did not replace and commit the edit: buffer=%q path=%q", updated.editBuffer, updated.settings.OVPNPath)
	}
	if len(updated.pathCompletions) != 0 {
		t.Fatalf("paste kept stale completion candidates: %#v", updated.pathCompletions)
	}
}

func TestOVPNPathTabCompletesDirectoriesAndFiles(t *testing.T) {
	dir := t.TempDir()
	profileDir := filepath.Join(dir, "VPN Files")
	if err := os.Mkdir(profileDir, 0o700); err != nil {
		t.Fatal(err)
	}
	profilePath := filepath.Join(profileDir, "office profile.ovpn")
	if err := os.WriteFile(profilePath, []byte(testOVPN), 0o600); err != nil {
		t.Fatal(err)
	}

	model := Model{}
	model.beginEdit(fieldOVPN, filepath.Join(dir, "VPN"))
	updatedModel, _ := model.handleEdit("tab")
	updated := updatedModel.(Model)
	if updated.editBuffer != profileDir+string(filepath.Separator) {
		t.Fatalf("directory completion = %q, want %q", updated.editBuffer, profileDir+string(filepath.Separator))
	}

	updated.editBuffer += "off"
	updated.resetPathCompletion()
	updatedModel, _ = updated.handleEdit("tab")
	updated = updatedModel.(Model)
	if updated.editBuffer != profilePath {
		t.Fatalf("file completion = %q, want %q", updated.editBuffer, profilePath)
	}
}

func TestOVPNPathTabCompletesCommonPrefixAndCycles(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"alpha.ovpn", "alpine.ovpn", "also.txt", ".hidden.ovpn"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(testOVPN), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	model := Model{}
	model.beginEdit(fieldOVPN, filepath.Join(dir, "a"))
	updatedModel, _ := model.handleEdit("tab")
	updated := updatedModel.(Model)
	if want := filepath.Join(dir, "alp"); updated.editBuffer != want {
		t.Fatalf("common prefix = %q, want %q", updated.editBuffer, want)
	}
	if len(updated.pathCompletions) != 2 {
		t.Fatalf("completion candidates = %#v, want two .ovpn files", updated.pathCompletions)
	}

	updatedModel, _ = updated.handleEdit("tab")
	updated = updatedModel.(Model)
	if want := filepath.Join(dir, "alpha.ovpn"); updated.editBuffer != want {
		t.Fatalf("first candidate = %q, want %q", updated.editBuffer, want)
	}
	updatedModel, _ = updated.handleEdit("shift+tab")
	updated = updatedModel.(Model)
	if want := filepath.Join(dir, "alpine.ovpn"); updated.editBuffer != want {
		t.Fatalf("reverse candidate = %q, want %q", updated.editBuffer, want)
	}
}

func TestOVPNPathCompletionSupportsHomeAndCaseInsensitiveFallback(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	downloads := filepath.Join(home, "Downloads")
	if err := os.Mkdir(downloads, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(downloads, "Profile.OVPN"), []byte(testOVPN), 0o600); err != nil {
		t.Fatal(err)
	}

	matches, err := ovpnPathCompletions("~/down")
	if err != nil || len(matches) != 1 || matches[0] != "~/Downloads/" {
		t.Fatalf("home directory completion = %#v, err=%v", matches, err)
	}
	matches, err = ovpnPathCompletions("~/Downloads/pro")
	if err != nil || len(matches) != 1 || matches[0] != "~/Downloads/Profile.OVPN" {
		t.Fatalf("case-insensitive file completion = %#v, err=%v", matches, err)
	}
}

func TestNormalizeOVPNFileURL(t *testing.T) {
	_, ovpnPath := testPathsAndOVPN(t)
	value := (&url.URL{Scheme: "file", Path: ovpnPath}).String()

	got, err := normalizeOVPNPath(value)
	if err != nil {
		t.Fatal(err)
	}
	if got != ovpnPath {
		t.Fatalf("got %q, want %q", got, ovpnPath)
	}
}

func TestNormalizeOVPNRejectsMultipleFiles(t *testing.T) {
	if _, err := normalizeOVPNPath(`/tmp/a.ovpn /tmp/b.ovpn`); err == nil || !strings.Contains(err.Error(), "一次只能") {
		t.Fatalf("expected one-file error, got %v", err)
	}
}

func testPathsAndOVPN(t *testing.T) (config.Paths, string) {
	t.Helper()
	dir := t.TempDir()
	configDir := filepath.Join(dir, "config")
	dataDir := filepath.Join(dir, "data")
	paths := config.Paths{
		ConfigDir: configDir,
		DataDir:   dataDir,
		Settings:  filepath.Join(configDir, "settings.json"),
		CoreBin:   filepath.Join(dataDir, "bin", "mihomo"),
	}
	profileDir := filepath.Join(dir, "VPN Files")
	if err := os.MkdirAll(profileDir, 0o700); err != nil {
		t.Fatal(err)
	}
	ovpnPath := filepath.Join(profileDir, "office profile.ovpn")
	if err := os.WriteFile(ovpnPath, []byte(testOVPN), 0o600); err != nil {
		t.Fatal(err)
	}
	return paths, ovpnPath
}

func withoutMihomoOnPath(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
}

func TestFooterShowsInstallPhases(t *testing.T) {
	model := Model{busy: true, install: &installState{}}
	model.install.active.Store(true)

	model.install.phase.Store(int32(core.PhaseResolve))
	if footer := model.footer(80); !strings.Contains(footer, "查询 Mihomo Stable 最新版本") {
		t.Fatalf("resolve phase label missing: %s", footer)
	}

	model.install.phase.Store(int32(core.PhaseCore))
	model.install.downloaded.Store(8 << 20)
	model.install.total.Store(16 << 20)
	if footer := model.footer(80); !strings.Contains(footer, "下载 Mihomo 核心 8.0 / 16.0 MiB (50%)") {
		t.Fatalf("core phase label missing: %s", footer)
	}

	model.install.phase.Store(int32(core.PhaseGeoIP))
	model.install.downloaded.Store(1 << 20)
	model.install.total.Store(0)
	if footer := model.footer(80); !strings.Contains(footer, "下载 GeoIP 数据 1.0 MiB") {
		t.Fatalf("geoip phase label missing: %s", footer)
	}
}

func TestDesktopOpenArgs(t *testing.T) {
	name, args := desktopOpenArgs("linux", "/tmp/mihomo.log", true)
	if name != "xdg-open" || len(args) != 1 || args[0] != "/tmp" {
		t.Fatalf("linux reveal = %s %v", name, args)
	}
	name, args = desktopOpenArgs("darwin", "https://example.com", false)
	if name != "open" || len(args) != 1 || args[0] != "https://example.com" {
		t.Fatalf("darwin open = %s %v", name, args)
	}
}

func TestSearchRefusedWhenNothingToSearch(t *testing.T) {
	offline := Model{active: tabConnections}
	updatedModel, _ := offline.handleKey(tea.KeyPressMsg(tea.Key{Text: "/", Code: '/'}))
	updated := updatedModel.(Model)
	if updated.connectionSearch.active {
		t.Fatal("/ must not enter search while controller is offline")
	}
	updatedModel, _ = updated.handleKey(tea.KeyPressMsg(tea.Key{Text: "1", Code: '1'}))
	if updatedModel.(Model).active != tabStatus {
		t.Fatal("global shortcut should still work after a stray /")
	}

	noLogs := Model{active: tabLogs}
	updatedModel, _ = noLogs.handleKey(tea.KeyPressMsg(tea.Key{Text: "/", Code: '/'}))
	if updatedModel.(Model).logSearch.active {
		t.Fatal("/ must not enter search while there are no logs")
	}
}

func TestSearchRowRendersEvenWhenListIsUnavailable(t *testing.T) {
	// 搜索态激活后 controller 掉线：按键仍被搜索框吞掉，页面必须继续显示搜索栏。
	model := Model{active: tabConnections, connectionSearch: searchBox{active: true, query: "api"}, width: 80, height: 30}
	if view := model.connectionsView(80); !strings.Contains(view, "搜索: api_") {
		t.Fatalf("offline connections view hides active search: %s", view)
	}
	model = Model{active: tabLogs, logSearch: searchBox{active: true, query: "err"}, width: 80, height: 30}
	if view := model.logsView(80); !strings.Contains(view, "搜索: err_") {
		t.Fatalf("empty logs view hides active search: %s", view)
	}
}

func TestRefreshKeyIsIgnoredWhileBusy(t *testing.T) {
	model := Model{busy: true, appUpdateAvailable: true, updatingApp: true}
	updatedModel, cmd := model.handleKey(tea.KeyPressMsg(tea.Key{Text: "r", Code: 'r'}))
	updated := updatedModel.(Model)
	if cmd != nil || !updated.appUpdateAvailable || updated.message != "" {
		t.Fatalf("r should be a no-op while busy: cmd=%v available=%v message=%q", cmd, updated.appUpdateAvailable, updated.message)
	}
}

func TestPasteIsIgnoredWhilePickerIsOpen(t *testing.T) {
	paths, ovpnPath := testPathsAndOVPN(t)
	model := Model{
		paths:    paths,
		settings: config.DefaultSettings(),
		active:   tabConfig,
		cursor:   fieldOVPN,
		picker:   &picker{kind: pickerScan, values: []string{"/stale/profile.ovpn"}},
	}
	updatedModel, _ := model.Update(tea.PasteMsg{Content: ovpnPath})
	updated := updatedModel.(Model)
	if updated.settings.OVPNPath != "" || updated.picker == nil {
		t.Fatalf("paste should not bypass an open picker: path=%q picker=%v", updated.settings.OVPNPath, updated.picker)
	}
}

func TestKeyTextAcceptsMultiRuneGraphemes(t *testing.T) {
	model := Model{active: tabLogs, logSearch: searchBox{active: true}, snapshot: snapshot{logs: []string{"x"}}}
	combining := "é"
	updatedModel, _ := model.handleKey(tea.KeyPressMsg(tea.Key{Text: combining, Code: 'e'}))
	if got := updatedModel.(Model).logSearch.query; got != combining {
		t.Fatalf("grapheme cluster dropped: %q", got)
	}
	if got := keyText("space", ""); got != " " {
		t.Fatalf("space fallback = %q", got)
	}
	if got := keyText("enter", ""); got != "" {
		t.Fatalf("named key should not insert text: %q", got)
	}

	edit := Model{editing: true, editField: fieldOVPN}
	updatedModel, _ = edit.handleKey(tea.KeyPressMsg(tea.Key{Text: "👍🏽", Code: '👍'}))
	if got := updatedModel.(Model).editBuffer; got != "👍🏽" {
		t.Fatalf("edit buffer dropped grapheme cluster: %q", got)
	}
}

func TestHasCoreUsesCachedProbe(t *testing.T) {
	withoutMihomoOnPath(t)
	model := Model{coreFound: true}
	if !model.hasCore() {
		t.Fatal("cached probe should report core present")
	}
	updatedModel, _ := model.Update(snapshotMsg(snapshot{}))
	if updatedModel.(Model).hasCore() {
		t.Fatal("snapshot without core path should clear cached probe")
	}
	updatedModel, _ = model.Update(snapshotMsg(snapshot{managed: manager.Status{CorePath: "/tmp/mihomo"}}))
	if !updatedModel.(Model).hasCore() {
		t.Fatal("snapshot with core path should mark core present")
	}
}

func TestPasteSubscriptionOnConfigPage(t *testing.T) {
	paths, _ := testPathsAndOVPN(t)
	model := Model{paths: paths, settings: config.DefaultSettings(), active: tabConfig, cursor: fieldMixedPort}
	updatedModel, _ := model.Update(tea.PasteMsg{Content: " https://sub.example.com/api?token=abc \n"})
	updated := updatedModel.(Model)
	if updated.editing || updated.settings.Subscription != "https://sub.example.com/api?token=abc" {
		t.Fatalf("subscription paste was not saved: %#v", updated.settings)
	}
	if got := configFields[fieldSubscription].value(updated); strings.Contains(got, "token") {
		t.Fatalf("subscription must be shown masked, got %q", got)
	}
}

func TestStartWithoutAnyRouteGuidesToSubscription(t *testing.T) {
	paths, _ := testPathsAndOVPN(t)
	model := Model{paths: paths, settings: config.DefaultSettings(), active: tabStatus, coreFound: true}
	updatedModel, _ := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	updated := updatedModel.(Model)
	if updated.active != tabConfig || !updated.editing || updated.editField != fieldSubscription {
		t.Fatalf("start without routes should open the subscription editor: active=%d editing=%v field=%d", updated.active, updated.editing, updated.editField)
	}
}

func TestIntranetListEditValidates(t *testing.T) {
	paths, ovpnPath := testPathsAndOVPN(t)
	settings := config.DefaultSettings()
	settings.OVPNPath = ovpnPath
	model := Model{paths: paths, settings: settings, active: tabConfig, cursor: fieldIntranetCIDRs}
	model.beginEdit(fieldIntranetCIDRs, "10.20.0.0/16, not-a-cidr")
	updatedModel, _ := model.commitEdit()
	if updated := updatedModel.(Model); updated.errorText == "" || len(updated.settings.IntranetCIDRs) != 0 {
		t.Fatalf("invalid CIDR must be rejected: %#v", updated.settings.IntranetCIDRs)
	}
	model.beginEdit(fieldIntranetCIDRs, "10.20.0.0/16, 172.30.0.0/16")
	updatedModel, _ = model.commitEdit()
	if got := updatedModel.(Model).settings.IntranetCIDRs; len(got) != 2 || got[1] != "172.30.0.0/16" {
		t.Fatalf("CIDRs = %#v", got)
	}
}

func actionIDs(actions []statusAction) []statusActionID {
	ids := make([]statusActionID, len(actions))
	for i, action := range actions {
		ids[i] = action.id
	}
	return ids
}

func TestStatusActionsFollowRuntimeState(t *testing.T) {
	model := Model{snapshot: snapshot{managed: manager.Status{CorePath: "/tmp/mihomo"}}}
	stopped := actionIDs(model.statusActions())
	want := []statusActionID{statusStart, statusValidate, statusInstall, statusClaudeCheck, statusShare}
	if !slices.Equal(stopped, want) {
		t.Fatalf("stopped actions = %v, want %v", stopped, want)
	}

	model.snapshot.managed.Running = true
	model.snapshot.healthy = true
	running := actionIDs(model.statusActions())
	want = []statusActionID{statusStart, statusReload, statusValidate, statusInstall, statusRecoverWeChat, statusClaudeCheck, statusShare}
	if !slices.Equal(running, want) {
		t.Fatalf("running actions = %v, want %v", running, want)
	}

	model = Model{snapshot: snapshot{managed: manager.Status{CorePath: "/tmp/mihomo"}}, dnsPending: true}
	if ids := actionIDs(model.statusActions()); ids[1] != statusRecoverDNS {
		t.Fatalf("leftover DNS should offer recovery right after start: %v", ids)
	}
}

// Mihomo 启停会让「重载」出现或消失，光标要留在原来的操作上。
func TestStatusCursorFollowsActionAcrossRefresh(t *testing.T) {
	model := Model{
		active:   tabStatus,
		cursor:   1,
		snapshot: snapshot{loaded: true, managed: manager.Status{CorePath: "/tmp/mihomo"}},
	}
	if model.statusActions()[1].id != statusValidate {
		t.Fatal("test expects validate at index 1 while stopped")
	}
	updatedModel, _ := model.Update(snapshotMsg{loaded: true, managed: manager.Status{CorePath: "/tmp/mihomo", Running: true, PID: 7}})
	updated := updatedModel.(Model)
	if got := updated.statusActions()[updated.cursor].id; got != statusValidate {
		t.Fatalf("cursor moved to %v after reload appeared", got)
	}
}

func readyPathsWithCore(t *testing.T) (config.Paths, config.Settings) {
	t.Helper()
	withoutMihomoOnPath(t)
	paths, _ := testPathsAndOVPN(t)
	if err := os.MkdirAll(filepath.Dir(paths.CoreBin), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.CoreBin, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	settings := config.DefaultSettings()
	settings.Subscription = "https://example.com/api/v1/client/subscribe?token=abc"
	return paths, settings
}

func TestReloadActionStartsBackgroundReload(t *testing.T) {
	paths, settings := readyPathsWithCore(t)
	model := Model{
		paths:    paths,
		settings: settings,
		snapshot: snapshot{managed: manager.Status{CorePath: paths.CoreBin, Running: true}},
	}
	model.cursor = slices.Index(actionIDs(model.statusActions()), statusReload)
	updatedModel, cmd := model.handleStatusKey("enter")
	updated := updatedModel.(Model)
	if !updated.busy || cmd == nil {
		t.Fatalf("reload was not started: busy=%v cmd=%v", updated.busy, cmd)
	}
}

func TestRulesReloadRowStartsMihomoWhenStopped(t *testing.T) {
	paths, _ := testPathsAndOVPN(t)
	model := Model{paths: paths, settings: config.DefaultSettings(), active: tabRules, cursor: ruleRowReload}
	updatedModel, _ := model.handleRulesKey("enter")
	updated := updatedModel.(Model)
	// 没有任何线路时和状态页的「启动」一样，引导去填订阅地址。
	if updated.active != tabConfig || !updated.editing || updated.editField != fieldSubscription {
		t.Fatalf("reload without routes should guide to setup: active=%d editing=%v field=%d", updated.active, updated.editing, updated.editField)
	}
	view := ansi.Strip(Model{active: tabRules, snapshot: snapshot{managed: manager.Status{Running: true}}}.rulesView(80))
	if !strings.Contains(view, "重载 Mihomo 使规则生效") || !strings.Contains(view, "测试只建立连接") {
		t.Fatalf("rules page lacks the reload entry: %s", view)
	}
}

func TestManualClaudeCheckWithoutMonitoringShowsResultDirectly(t *testing.T) {
	paths, _ := testPathsAndOVPN(t)
	model := Model{paths: paths, settings: config.DefaultSettings()}
	checked := claudeguard.Status{Level: claudeguard.LevelAlert, Key: claudeguard.KeyDirect, Title: "Claude 正在直连", CheckedAt: time.Now()}

	original := probeClaude
	t.Cleanup(func() { probeClaude = original })
	var probed config.Settings
	probeClaude = func(_ manager.Manager, settings config.Settings) claudeguard.Status {
		probed = settings
		return checked
	}

	updatedModel, cmd := model.checkClaudeNow()
	updated := updatedModel.(Model)
	if !updated.claudeChecking || cmd == nil {
		t.Fatalf("manual check not started: checking=%v cmd=%v", updated.claudeChecking, cmd)
	}
	msg := cmd()
	if !probed.ClaudeGuard {
		t.Fatal("a manual check must run even when monitoring is off")
	}
	updatedModel, _ = updated.Update(msg)
	updated = updatedModel.(Model)
	if updated.claudeChecking || updated.claude.Title != "Claude 正在直连" {
		t.Fatalf("manual result not shown: %#v", updated.claude)
	}
	view := ansi.Strip(updated.statusView(100))
	if !strings.Contains(view, "Claude：Claude 正在直连") || !strings.Contains(view, "未开启自动监测") {
		t.Fatalf("status page lacks the Claude conclusion: %s", view)
	}
	if updated.claudeDue(time.Now()) {
		t.Fatal("no automatic checks while monitoring is off")
	}
}

func TestMonitoredClaudeAlertNeedsConfirmation(t *testing.T) {
	paths, _ := testPathsAndOVPN(t)
	settings := config.DefaultSettings()
	settings.ClaudeGuard = true
	model := Model{paths: paths, settings: settings}
	if !model.claudeDue(time.Now()) {
		t.Fatal("monitoring on and never checked: a check is due")
	}
	model.claudeChecking = true
	if model.claudeDue(time.Now()) {
		t.Fatal("no second check while one is running")
	}

	alert := claudeguard.Status{Level: claudeguard.LevelAlert, Key: claudeguard.KeyUnreachable, Title: "无法经代理节点访问 Claude", CheckedAt: time.Now()}
	updatedModel, _ := model.Update(claudeMsg{status: alert})
	updated := updatedModel.(Model)
	if updated.claude.Level == claudeguard.LevelAlert || updated.claude.Pending != 1 {
		t.Fatalf("a single failure should only be pending: %#v", updated.claude)
	}
	if wait := time.Until(updated.claudeNext); wait > claudeguard.ConfirmInterval {
		t.Fatalf("recheck scheduled %v later, want within %v", wait, claudeguard.ConfirmInterval)
	}
	alert.CheckedAt = time.Now()
	updatedModel, _ = updated.Update(claudeMsg{status: alert})
	updated = updatedModel.(Model)
	if updated.claude.Level != claudeguard.LevelAlert {
		t.Fatalf("confirmed failure should alert: %#v", updated.claude)
	}

	// 切换策略后旧结论作废，下一次 tick 立即检测。
	updatedModel, _ = updated.Update(actionMsg{text: "Claude 已切换为 住宅节点", claudeReset: true})
	updated = updatedModel.(Model)
	if updated.claude.Level != claudeguard.LevelChecking || !updated.claudeDue(time.Now()) {
		t.Fatalf("policy switch should reset the conclusion: %#v due=%v", updated.claude, updated.claudeDue(time.Now()))
	}
}

func TestTickStartsDueClaudeCheck(t *testing.T) {
	paths, _ := testPathsAndOVPN(t)
	settings := config.DefaultSettings()
	settings.ClaudeGuard = true
	model := Model{paths: paths, settings: settings, manager: manager.Manager{Paths: paths}}
	updatedModel, _ := model.Update(tickMsg(time.Now()))
	if !updatedModel.(Model).claudeChecking {
		t.Fatal("tick should start a due Claude check")
	}
	settings.ClaudeGuard = false
	model.settings = settings
	updatedModel, _ = model.Update(tickMsg(time.Now()))
	if updatedModel.(Model).claudeChecking {
		t.Fatal("tick must not probe Claude while monitoring is off")
	}
}

func TestLeftoverDNSIsReportedAfterGrace(t *testing.T) {
	var model Model
	start := time.Now()
	model.updateDNSPending(true, start)
	if model.dnsPending {
		t.Fatal("a fresh DNS snapshot may belong to a start in progress")
	}
	model.updateDNSPending(true, start.Add(dnsPendingGrace))
	if !model.dnsPending {
		t.Fatal("a lasting DNS snapshot should be reported")
	}
	model.updateDNSPending(false, start.Add(2*dnsPendingGrace))
	if model.dnsPending || !model.dnsPendingSince.IsZero() {
		t.Fatal("recovered DNS should clear the notice")
	}
}

func TestStatusShowsTrafficProxyTailscaleAndClaudeExit(t *testing.T) {
	paths, _ := testPathsAndOVPN(t)
	settings := config.DefaultSettings()
	settings.AllowLAN = true
	settings.ProxyUsername, settings.ProxyPassword = "tizi", "secret"
	settings.TailscaleEnabled = true
	settings.TailscaleHostname = "office-mac"
	settings.ChinaDirect = false
	model := Model{
		paths:           paths,
		settings:        settings,
		snapshot:        snapshot{healthy: true, managed: manager.Status{Running: true, PID: 9}, connections: []controller.Connection{{ID: "a"}, {ID: "b"}}},
		connectionRates: map[string]connectionRate{"a": {Download: 2 << 20, Upload: 1024}, "b": {Download: 1 << 20}},
		claude:          claudeguard.Status{Level: claudeguard.LevelOK, Title: "Claude 经代理节点访问正常", ExitIP: "203.0.113.9", Country: "US", CheckedAt: time.Now()},
	}
	view := ansi.Strip(model.statusView(120))
	for _, want := range []string{
		"↓ 3.0MiB/s  ↑ 1.0KiB/s · 2 条连接",
		"0.0.0.0:" + strconv.Itoa(settings.MixedPort) + "（HTTP/SOCKS5） · 局域网需要用户名和密码",
		"开启（设备名 office-mac）",
		"经外网代理",
		"代理节点 · 203.0.113.9（US）",
		"Claude 经代理节点访问正常",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("status page lacks %q:\n%s", want, view)
		}
	}
}

func TestShortTerminalKeepsActionsVisible(t *testing.T) {
	model := Model{
		settings: config.DefaultSettings(),
		height:   24,
		snapshot: snapshot{healthy: true, managed: manager.Status{Running: true, CorePath: "/tmp/mihomo"}},
	}
	view := ansi.Strip(model.statusView(80))
	for _, want := range []string{"受管 Mihomo", "拉高窗口可见", "Clash Mi 扫码导入", "重载 Mihomo"} {
		if !strings.Contains(view, want) {
			t.Errorf("short status page lacks %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "APP 版本") {
		t.Errorf("low-priority rows should be trimmed first:\n%s", view)
	}
}

func TestEditClaudeProxyFileWritesTemplateFirst(t *testing.T) {
	paths, _ := testPathsAndOVPN(t)
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "true")
	model := Model{paths: paths, settings: config.DefaultSettings(), cursor: fieldClaudeProxyFile}
	_, cmd := model.handleConfigKey("enter")
	if cmd == nil {
		t.Fatal("editing claude-proxy.yaml should open the editor")
	}
	data, err := os.ReadFile(filepath.Join(paths.ConfigDir, config.ClaudeProxyFile))
	if err != nil || string(data) != claude.FileTemplate {
		t.Fatalf("template not written: %v", err)
	}
	if value := configFields[fieldClaudeProxyFile].value(model); !strings.Contains(value, "未使用") {
		t.Fatalf("a comment-only file is not a node: %q", value)
	}
}

func TestHelpMentionsNewStatusActions(t *testing.T) {
	help := ansi.Strip(Model{}.helpView(120))
	for _, want := range []string{"重载 Mihomo", "刷新微信直连", "检测 Claude 线路", "claude-proxy.yaml"} {
		if !strings.Contains(help, want) {
			t.Errorf("help lacks %q", want)
		}
	}
}

func TestRefreshReportsLeftoverDNS(t *testing.T) {
	paths, _ := testPathsAndOVPN(t)
	paths.DNSState = filepath.Join(paths.ConfigDir, "dns-state.json")
	paths.PIDFile = filepath.Join(paths.ConfigDir, "mihomo.pid")
	paths.LogFile = filepath.Join(paths.ConfigDir, "mihomo.log")
	if err := os.MkdirAll(paths.ConfigDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.DNSState, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	settings := config.DefaultSettings()
	settings.ControllerPort = 1 // 不会有 Controller 在线
	model := Model{paths: paths, settings: settings, manager: manager.Manager{Paths: paths}}
	msg := refreshCmd(model)().(snapshotMsg)
	if !msg.loaded || !msg.dnsLeftover {
		t.Fatalf("refresh should report a leftover DNS snapshot: loaded=%v leftover=%v", msg.loaded, msg.dnsLeftover)
	}
}
