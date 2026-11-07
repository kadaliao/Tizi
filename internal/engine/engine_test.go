package engine

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kadaliao/tizi/internal/appupdate"
	"github.com/kadaliao/tizi/internal/claudeguard"
	"github.com/kadaliao/tizi/internal/config"
	"github.com/kadaliao/tizi/internal/controller"
	"github.com/kadaliao/tizi/internal/diagnostics"
)

const testProfile = `client
dev tun
proto udp
remote a.example.com 1194
remote b.example.com 1194
cipher AES-256-GCM
<ca>
-----BEGIN CERTIFICATE-----
MIIB
-----END CERTIFICATE-----
</ca>
<cert>
-----BEGIN CERTIFICATE-----
MIIC
-----END CERTIFICATE-----
</cert>
<key>
-----BEGIN PRIVATE KEY-----
MIID
-----END PRIVATE KEY-----
</key>
<tls-crypt>
-----BEGIN OpenVPN Static key V1-----
00
-----END OpenVPN Static key V1-----
</tls-crypt>
`

func testPaths(t *testing.T) config.Paths {
	t.Helper()
	root := t.TempDir()
	configDir := filepath.Join(root, "config")
	dataDir := filepath.Join(root, "data")
	return config.Paths{
		ConfigDir:   configDir,
		DataDir:     dataDir,
		Settings:    filepath.Join(configDir, "settings.json"),
		Generated:   filepath.Join(configDir, "config.yaml"),
		PIDFile:     filepath.Join(configDir, "mihomo.pid"),
		DNSState:    filepath.Join(configDir, "dns-state.json"),
		LogFile:     filepath.Join(configDir, "mihomo.log"),
		CustomRules: filepath.Join(configDir, "custom-rules.txt"),
		CoreBin:     filepath.Join(dataDir, "bin", "mihomo"),
		DownloadTmp: filepath.Join(dataDir, "bin", ".mihomo.download"),
	}
}

// newTestEngine 返回一个不访问网络的引擎；Controller 端口指向保留端口，
// 健康检查会立即失败。
func newTestEngine(t *testing.T, out io.Writer) (*Engine, config.Paths) {
	t.Helper()
	paths := testPaths(t)
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	settings := config.DefaultSettings()
	settings.ControllerPort = 1
	settings.ControllerSecret = "secret"
	if err := config.Save(paths, settings); err != nil {
		t.Fatal(err)
	}
	engine := New(paths, settings, "v0.0.1", out)
	engine.checkForUpdate = func(context.Context, string) (appupdate.Status, error) {
		return appupdate.Status{Current: "v0.0.1", Latest: "v0.0.1"}, nil
	}
	return engine, paths
}

func writeProfile(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "user.ovpn")
	if err := os.WriteFile(path, []byte(testProfile), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

type message struct {
	ID     int64           `json:"id"`
	Event  string          `json:"event"`
	Result json.RawMessage `json:"result"`
	Data   json.RawMessage `json:"data"`
	Error  *Error          `json:"error"`
}

// TestServeAnswersRequestsAndExitsOnEOF 覆盖 GUI 的基本会话：握手、未知方法、
// 坏请求都有带 ID 的回应，首个快照立即推送，关闭输入后引擎退出。
func TestServeAnswersRequestsAndExitsOnEOF(t *testing.T) {
	inReader, inWriter := io.Pipe()
	outReader, outWriter := io.Pipe()
	engine, paths := newTestEngine(t, outWriter)
	done := make(chan error, 1)
	go func() {
		done <- engine.Serve(context.Background(), inReader)
		_ = outWriter.Close()
	}()

	go func() {
		_, _ = io.WriteString(inWriter, `{"id":1,"method":"hello"}`+"\n")
		_, _ = io.WriteString(inWriter, `{"id":2,"method":"missing"}`+"\n")
		_, _ = io.WriteString(inWriter, "not json\n")
	}()

	responses := map[int64]message{}
	var snapshot *Snapshot
	var invalid bool
	scanner := bufio.NewScanner(outReader)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	deadline := time.After(10 * time.Second)
	for len(responses) < 2 || snapshot == nil || !invalid {
		lines := make(chan string, 1)
		go func() {
			if scanner.Scan() {
				lines <- scanner.Text()
			} else {
				close(lines)
			}
		}()
		select {
		case <-deadline:
			t.Fatalf("超时：responses=%v snapshot=%v invalid=%v", responses, snapshot != nil, invalid)
		case line, ok := <-lines:
			if !ok {
				t.Fatalf("输出提前结束: %v", scanner.Err())
			}
			var msg message
			if err := json.Unmarshal([]byte(line), &msg); err != nil {
				t.Fatalf("输出不是 JSON: %q", line)
			}
			switch {
			case msg.Event == "snapshot":
				var value Snapshot
				if err := json.Unmarshal(msg.Data, &value); err != nil {
					t.Fatal(err)
				}
				snapshot = &value
			case msg.ID == 0 && msg.Error != nil:
				invalid = msg.Error.Code == CodeInvalidParams
			default:
				responses[msg.ID] = msg
			}
		}
	}

	var hello Hello
	if err := json.Unmarshal(responses[1].Result, &hello); err != nil {
		t.Fatal(err)
	}
	if hello.Protocol != ProtocolVersion || hello.Version != "v0.0.1" || hello.LogFile != paths.LogFile {
		t.Fatalf("hello = %+v", hello)
	}
	if responses[2].Error == nil || responses[2].Error.Code != CodeUnknownMethod {
		t.Fatalf("未知方法应返回 unknown_method: %+v", responses[2])
	}
	if snapshot.Managed.Running || snapshot.ControllerOnline {
		t.Fatalf("测试环境不应有运行中的 Mihomo: %+v", snapshot.Managed)
	}
	if snapshot.Settings.ControllerPort != 1 {
		t.Fatalf("快照设置 = %+v", snapshot.Settings)
	}

	_ = inWriter.Close()
	go func() { _, _ = io.Copy(io.Discard, outReader) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve 返回错误: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("关闭输入后引擎没有退出")
	}
}

func TestSnapshotNeverExposesSecrets(t *testing.T) {
	engine, paths := newTestEngine(t, io.Discard)
	path := writeProfile(t, paths.ConfigDir)
	if _, err := engine.updateSettings(settingsPatch{OVPNPath: &path}); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(engine.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"secret", "PRIVATE KEY", "MIID", "Static key"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("快照包含敏感内容 %q: %s", secret, data)
		}
	}
	var snapshot Snapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		t.Fatal(err)
	}
	if !snapshot.Profile.Loaded || strings.Join(snapshot.Profile.Remotes, ",") != "a.example.com:1194,b.example.com:1194" {
		t.Fatalf("profile = %+v", snapshot.Profile)
	}
}

func TestUpdateSettingsValidatesAndPersists(t *testing.T) {
	engine, paths := newTestEngine(t, io.Discard)
	path := writeProfile(t, paths.ConfigDir)
	remote := "b.example.com"
	tun := false
	channel := "Alpha"
	port := 7891
	if _, err := engine.updateSettings(settingsPatch{OVPNPath: &path, Remote: &remote, TunEnabled: &tun, CoreChannel: &channel, MixedPort: &port}); err != nil {
		t.Fatal(err)
	}
	saved, err := config.Load(paths)
	if err != nil {
		t.Fatal(err)
	}
	if saved.OVPNPath != path || saved.Remote != remote || saved.TunEnabled || saved.CoreChannel != config.CoreChannelAlpha || saved.MixedPort != 7891 {
		t.Fatalf("保存的设置 = %+v", saved)
	}
	if saved.ControllerSecret != "secret" {
		t.Fatal("保存设置不应丢失 Controller 密钥")
	}

	// 换 OVPN 时与 TUI 一致地清空 remote 选择。
	other := filepath.Join(paths.ConfigDir, "other.ovpn")
	if err := os.WriteFile(other, []byte(testProfile), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.updateSettings(settingsPatch{OVPNPath: &other}); err != nil {
		t.Fatal(err)
	}
	if got := engine.currentSettings(); got.Remote != "" || got.OVPNPath != other {
		t.Fatalf("更换 OVPN 后 = %+v", got)
	}

	relative := "user.ovpn"
	badPort := 70000
	unknownRemote := "c.example.com"
	badChannel := "nightly"
	for name, patch := range map[string]settingsPatch{
		"相对路径":      {OVPNPath: &relative},
		"端口越界":      {DNSPort: &badPort},
		"未知 remote": {Remote: &unknownRemote},
		"未知通道":      {CoreChannel: &badChannel},
	} {
		if _, err := engine.updateSettings(patch); err == nil {
			t.Errorf("%s 应被拒绝", name)
		}
	}
	if got := engine.currentSettings(); got.DNSPort != config.DefaultSettings().DNSPort {
		t.Fatalf("被拒绝的修改不应落盘: %+v", got)
	}
}

// TestCurrentSettingsFollowsOtherWriters 保证 TUI 或命令行改动 settings.json 后，
// GUI 不需要重启就能看到新设置。
func TestCurrentSettingsFollowsOtherWriters(t *testing.T) {
	engine, paths := newTestEngine(t, io.Discard)
	settings, err := config.Load(paths)
	if err != nil {
		t.Fatal(err)
	}
	settings.ChinaDirect = false
	settings.MixedPort = 17890
	// 同一纳秒内的改写会得到相同的时间戳；刻意推后修改时间。
	if err := config.Save(paths, settings); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Second)
	if err := os.Chtimes(paths.Settings, future, future); err != nil {
		t.Fatal(err)
	}
	got := engine.currentSettings()
	if got.ChinaDirect || got.MixedPort != 17890 || got.ControllerSecret != "secret" {
		t.Fatalf("currentSettings = %+v", got)
	}
}

func TestExclusiveRejectsConcurrentActions(t *testing.T) {
	engine, _ := newTestEngine(t, io.Discard)
	entered := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_, _ = engine.exclusive("启动 Mihomo", func() (any, error) {
			close(entered)
			<-release
			return nil, nil
		})
	}()
	<-entered
	_, err := engine.exclusive("停止 Mihomo", func() (any, error) { return nil, nil })
	if coded, ok := err.(codedError); !ok || coded.code != CodeBusy || !strings.Contains(coded.message, "启动 Mihomo") {
		t.Fatalf("并发操作应返回 busy: %v", err)
	}
	if snapshot := engine.snapshot(); snapshot.Busy != "启动 Mihomo" {
		t.Fatalf("快照应标注进行中的操作: %q", snapshot.Busy)
	}
	close(release)
}

func TestActionsRequireOVPNAndCore(t *testing.T) {
	engine, paths := newTestEngine(t, io.Discard)
	if _, err := engine.start(false, false); err == nil || err.(codedError).code != CodeNotConfigured {
		t.Fatalf("未设置 OVPN 时应提示选择: %v", err)
	}
	path := writeProfile(t, paths.ConfigDir)
	if _, err := engine.updateSettings(settingsPatch{OVPNPath: &path}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())
	if _, err := engine.start(false, false); err == nil || err.(codedError).code != CodeCoreMissing {
		t.Fatalf("未安装核心时应提示安装: %v", err)
	}
}

func TestConnectionViewsComputeRates(t *testing.T) {
	engine, _ := newTestEngine(t, io.Discard)
	first := []controller.Connection{{ID: "a", Download: 1000, Upload: 100, Chains: []string{"hk-node", "外网代理"}}}
	first[0].Metadata.Host = "example.com"
	first[0].Metadata.DestinationPort = "443"
	start := time.Now()
	var traffic Traffic
	views := engine.connectionViews(first, start, &traffic)
	if len(views) != 1 || views[0].Sampled || traffic.Download != 0 {
		t.Fatalf("首次采样不应有速度: %+v %+v", views, traffic)
	}
	second := []controller.Connection{first[0], {ID: "b", Download: 5}}
	second[0].Download, second[0].Upload = 3000, 300
	traffic = Traffic{}
	views = engine.connectionViews(second, start.Add(2*time.Second), &traffic)
	if !views[0].Sampled || views[0].DownloadRate != 1000 || views[0].UploadRate != 100 {
		t.Fatalf("速度 = %+v", views[0])
	}
	if views[1].Sampled {
		t.Fatal("新连接没有可信速度")
	}
	if traffic.Download != 1000 || traffic.Upload != 100 || traffic.Connections != 2 {
		t.Fatalf("traffic = %+v", traffic)
	}
	if views[0].Destination != "example.com:443" || views[0].Route != "外网代理 -> hk-node" {
		t.Fatalf("展示字段 = %+v", views[0])
	}
}

func TestDecodeParamsRejectsUnknownFields(t *testing.T) {
	var params struct {
		Limit int `json:"limit"`
	}
	if err := decodeParams(json.RawMessage(`{"limt":5}`), &params); err == nil {
		t.Fatal("拼错的参数名应被拒绝，而不是静默使用默认值")
	}
	if err := decodeParams(nil, &params); err != nil {
		t.Fatal(err)
	}
}

func TestConnectionViewsIgnoreTooShortIntervals(t *testing.T) {
	engine, _ := newTestEngine(t, io.Discard)
	start := time.Now()
	sample := func(download int64, at time.Time) Connection {
		var traffic Traffic
		return engine.connectionViews([]controller.Connection{{ID: "a", Download: download}}, at, &traffic)[0]
	}
	sample(0, start)
	if got := sample(2000, start.Add(2*time.Second)); got.DownloadRate != 1000 {
		t.Fatalf("正常间隔的速度 = %v", got.DownloadRate)
	}
	// 操作后的立即刷新只隔几十毫秒：沿用上一次速度，而不是算出 1 MB/s 的尖峰。
	if got := sample(52000, start.Add(2050*time.Millisecond)); got.DownloadRate != 1000 || !got.Sampled {
		t.Fatalf("过短间隔应沿用上次速度: %+v", got)
	}
	// 基线没有推进，下一次按 2s -> 4s 的完整间隔计算。
	if got := sample(6000, start.Add(4*time.Second)); got.DownloadRate != 2000 {
		t.Fatalf("基线应停留在上一次正常采样: %v", got.DownloadRate)
	}
}

func TestCheckUpdateKeepsKnownVersionOnFailure(t *testing.T) {
	engine, _ := newTestEngine(t, io.Discard)
	engine.checkForUpdate = func(context.Context, string) (appupdate.Status, error) {
		return appupdate.Status{Latest: "v9.9.9", Available: true}, nil
	}
	engine.checkUpdate(context.Background(), 0)
	engine.checkForUpdate = func(context.Context, string) (appupdate.Status, error) {
		return appupdate.Status{}, io.ErrUnexpectedEOF
	}
	engine.checkUpdate(context.Background(), 0)
	if update := engine.snapshot().Update; !update.Available || update.Latest != "v9.9.9" || update.Error == "" {
		t.Fatalf("检查失败不应抹掉已知的新版本: %+v", update)
	}
}

func TestCheckUpdateMaxAgeReusesRecentSuccess(t *testing.T) {
	engine, _ := newTestEngine(t, io.Discard)
	var checks atomic.Int32
	fail := false
	engine.checkForUpdate = func(context.Context, string) (appupdate.Status, error) {
		checks.Add(1)
		if fail {
			return appupdate.Status{}, io.ErrUnexpectedEOF
		}
		return appupdate.Status{Latest: "v9.9.9", Available: true}, nil
	}
	request := Request{Method: "checkUpdate", Params: json.RawMessage(`{"maxAge":300}`)}
	call := func() Update {
		t.Helper()
		result, err := engine.dispatch(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		return result.(Update)
	}

	if update := call(); checks.Load() != 1 || !update.Available || update.CheckedAt == 0 {
		t.Fatalf("还没查过时应立即查询: checks=%d %+v", checks.Load(), update)
	}
	if call(); checks.Load() != 1 {
		t.Fatalf("maxAge 内应沿用上次结果，实际查询了 %d 次", checks.Load())
	}

	// 上次成功查询已超过 maxAge（例如电脑睡了一晚）时重新查询。
	engine.mu.Lock()
	engine.update.CheckedAt = time.Now().Add(-301 * time.Second).UnixMilli()
	engine.mu.Unlock()
	fail = true
	if update := call(); checks.Load() != 2 || update.Error == "" {
		t.Fatalf("超过 maxAge 应重新查询: checks=%d %+v", checks.Load(), update)
	}
	// 失败不算查过：下一次带 maxAge 的检查仍要重新查询。
	fail = false
	if update := call(); checks.Load() != 3 || update.Error != "" {
		t.Fatalf("上次查询失败时不应沿用旧结果: checks=%d %+v", checks.Load(), update)
	}

	// 不带 maxAge 的检查（用户点「检查更新」）总是重新查询。
	if _, err := engine.dispatch(context.Background(), Request{Method: "checkUpdate"}); err != nil {
		t.Fatal(err)
	}
	if checks.Load() != 4 {
		t.Fatalf("手动检查应总是重新查询，实际查询了 %d 次", checks.Load())
	}
	if _, err := engine.dispatch(context.Background(), Request{Method: "checkUpdate", Params: json.RawMessage(`{"maxAge":-1}`)}); err == nil {
		t.Fatal("负数 maxAge 应报参数错误")
	}
}

func TestCheckUpdateWaitsForQueryInProgress(t *testing.T) {
	engine, _ := newTestEngine(t, io.Discard)
	started := make(chan struct{})
	release := make(chan struct{})
	var checks atomic.Int32
	engine.checkForUpdate = func(context.Context, string) (appupdate.Status, error) {
		if checks.Add(1) == 1 {
			close(started)
		}
		<-release
		return appupdate.Status{Latest: "v9.9.9", Available: true}, nil
	}
	first := make(chan Update, 1)
	go func() { first <- engine.checkUpdate(context.Background(), 0) }()
	<-started
	second := make(chan Update, 1)
	go func() { second <- engine.checkUpdate(context.Background(), 0) }()
	select {
	case update := <-second:
		t.Fatalf("查询进行中时不应直接返回旧结果: %+v", update)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	for _, result := range []chan Update{first, second} {
		if update := <-result; !update.Available || update.Latest != "v9.9.9" {
			t.Fatalf("两次检查都应得到这次查询的结果: %+v", update)
		}
	}
	if got := checks.Load(); got != 1 {
		t.Fatalf("同时发起的检查应只查询一次，实际 %d 次", got)
	}
}

func TestUpdateLoopRechecksPeriodically(t *testing.T) {
	engine, _ := newTestEngine(t, io.Discard)
	engine.updateEvery = 10 * time.Millisecond
	var checks atomic.Int32
	engine.checkForUpdate = func(context.Context, string) (appupdate.Status, error) {
		checks.Add(1)
		return appupdate.Status{Latest: "v0.0.1"}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { engine.updateLoop(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for checks.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if got := checks.Load(); got < 3 {
		t.Fatalf("常驻的引擎应定期重新检查新版本，只检查了 %d 次", got)
	}
}

func TestDNSRecoveryPendingWaitsOutTransitions(t *testing.T) {
	engine, paths := newTestEngine(t, io.Discard)
	if err := os.WriteFile(paths.DNSState, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if engine.snapshot().DNSRecoveryPending {
		t.Fatal("刚出现的 DNS 快照可能属于正在进行的启动，不应立即提示")
	}
	engine.dnsPendingSince = time.Now().Add(-dnsPendingGrace)
	if !engine.snapshot().DNSRecoveryPending {
		t.Fatal("持续存在的遗留 DNS 快照应提示恢复")
	}
	if err := os.Remove(paths.DNSState); err != nil {
		t.Fatal(err)
	}
	if engine.snapshot().DNSRecoveryPending || !engine.dnsPendingSince.IsZero() {
		t.Fatal("快照消失后应清除提示和计时")
	}
}

func TestScanOVPNNeverReturnsNull(t *testing.T) {
	engine, _ := newTestEngine(t, io.Discard)
	t.Setenv("HOME", t.TempDir())
	result, err := engine.dispatch(context.Background(), Request{Method: "scanOVPN"})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(result)
	if string(data) != "[]" {
		t.Fatalf("没有结果时应返回空数组，GUI 按数组解码: %s", data)
	}
}

func TestClaudeSettingsAreValidatedAndMasked(t *testing.T) {
	engine, paths := newTestEngine(t, io.Discard)
	address := "socks5://alice:residential-pass@203.0.113.9:443"
	if _, err := engine.updateSettings(settingsPatch{ClaudeProxy: &address}); err != nil {
		t.Fatal(err)
	}
	saved, err := config.Load(paths)
	if err != nil || saved.ClaudeProxy != address {
		t.Fatalf("saved = %+v, %v", saved, err)
	}
	data, err := json.Marshal(engine.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "residential-pass") {
		t.Fatalf("快照包含住宅节点密码: %s", data)
	}
	var snapshot Snapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Settings.ClaudeProxy != "socks5://alice:***@203.0.113.9:443" || !snapshot.Settings.ClaudeGuardActive || snapshot.Settings.ClaudeProxyError != "" {
		t.Fatalf("settings view = %+v", snapshot.Settings)
	}

	bad := "ss://abc@203.0.113.9:443"
	_, err = engine.updateSettings(settingsPatch{ClaudeProxy: &bad})
	var coded codedError
	if !errors.As(err, &coded) || coded.code != CodeInvalidParams {
		t.Fatalf("unsupported address error = %v", err)
	}
	if got := engine.currentSettings().ClaudeProxy; got != address {
		t.Fatalf("rejected address must not be saved: %q", got)
	}
}

func TestClaudeNodeFieldsKeepPasswordWhenOmitted(t *testing.T) {
	engine, paths := newTestEngine(t, io.Discard)
	update := func(params string) error {
		_, err := engine.dispatch(context.Background(), Request{Method: "updateSettings", Params: json.RawMessage(params)})
		return err
	}
	if err := update(`{"claudeNode":{"scheme":"http","host":"203.0.113.9","port":10003,"username":"alice","password":"p@ss:1"}}`); err != nil {
		t.Fatal(err)
	}
	saved, _ := config.Load(paths)
	if saved.ClaudeProxy != "http://alice:p%40ss%3A1@203.0.113.9:10003" {
		t.Fatalf("saved address = %q", saved.ClaudeProxy)
	}
	view := engine.snapshot().Settings
	want := ClaudeNode{Scheme: "http", Host: "203.0.113.9", Port: 10003, Username: "alice", HasPassword: true}
	if view.ClaudeNode == nil || *view.ClaudeNode != want {
		t.Fatalf("node view = %+v", view.ClaudeNode)
	}

	// 只改协议和端口、不传密码：沿用已保存的密码。
	if err := update(`{"claudeNode":{"scheme":"socks5","host":"203.0.113.9","port":443,"username":"alice-2"}}`); err != nil {
		t.Fatal(err)
	}
	if got := engine.currentSettings().ClaudeProxy; got != "socks5://alice-2:p%40ss%3A1@203.0.113.9:443" {
		t.Fatalf("password should be kept: %q", got)
	}
	// 用户名留空时不带密码。
	if err := update(`{"claudeNode":{"scheme":"socks5","host":"203.0.113.9","port":443,"username":""}}`); err != nil {
		t.Fatal(err)
	}
	if got := engine.currentSettings().ClaudeProxy; got != "socks5://203.0.113.9:443" {
		t.Fatalf("password should be dropped without a username: %q", got)
	}

	before := engine.currentSettings().ClaudeProxy
	for _, params := range []string{
		`{"claudeNode":{"scheme":"socks5","host":"socks5://203.0.113.9","port":443,"username":""}}`,
		`{"claudeNode":{"scheme":"vless","host":"203.0.113.9","port":443,"username":""}}`,
		`{"claudeProxy":"","claudeNode":{"scheme":"socks5","host":"203.0.113.9","port":443,"username":""}}`,
	} {
		var coded codedError
		if err := update(params); !errors.As(err, &coded) || coded.code != CodeInvalidParams {
			t.Errorf("%s: error = %v", params, err)
		}
	}
	if got := engine.currentSettings().ClaudeProxy; got != before {
		t.Fatalf("rejected node must not be saved: %q", got)
	}
}

func TestParseClaudeAddress(t *testing.T) {
	engine, _ := newTestEngine(t, io.Discard)
	result, err := engine.dispatch(context.Background(), Request{Method: "parseClaudeAddress", Params: json.RawMessage(`{"text":"gw.example.com:10003:alice:secret"}`)})
	want := ParsedClaudeNode{Host: "gw.example.com", Port: 10003, Username: "alice", Password: "secret"}
	if err != nil || result != want {
		t.Fatalf("result = %+v, %v", result, err)
	}
	_, err = engine.dispatch(context.Background(), Request{Method: "parseClaudeAddress", Params: json.RawMessage(`{"text":"alice:secret"}`)})
	var coded codedError
	if !errors.As(err, &coded) || coded.code != CodeInvalidParams {
		t.Fatalf("unknown format error = %v", err)
	}
}

func TestClaudeLoopAlertsAfterRepeatedFailuresAndKeepsHeartbeat(t *testing.T) {
	engine, paths := newTestEngine(t, io.Discard)
	observed := make(chan struct{}, 10)
	engine.observeClaude = func(context.Context, claudeguard.Env) claudeguard.Status {
		observed <- struct{}{}
		return claudeguard.Status{Level: claudeguard.LevelAlert, Key: claudeguard.KeyUnreachable, Title: "无法访问", CheckedAt: time.Now()}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); engine.claudeLoop(ctx) }()

	<-observed
	if engine.claudeView().Level == claudeguard.LevelAlert {
		t.Fatal("一次失败不应告警")
	}
	if !claudeguard.UIActive(paths.ConfigDir) {
		t.Fatal("引擎运行时应维持心跳，后台进程才不会重复弹窗")
	}
	engine.kickClaude(false)
	<-observed
	deadline := time.Now().Add(2 * time.Second)
	for engine.claudeView().Level != claudeguard.LevelAlert {
		if time.Now().After(deadline) {
			t.Fatalf("连续两次失败后应告警，当前 %+v", engine.claudeView())
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if claudeguard.UIActive(paths.ConfigDir) {
		t.Fatal("引擎退出后应删除心跳，让后台进程接手")
	}
}

func TestClaudeReactsToExitAndDirectWithoutWaiting(t *testing.T) {
	engine, paths := newTestEngine(t, io.Discard)
	enabled := true
	if _, err := engine.updateSettings(settingsPatch{ClaudeGuard: &enabled}); err != nil {
		t.Fatal(err)
	}
	// 清掉保存设置触发的检测请求。
	select {
	case <-engine.claudeKick:
	default:
	}

	// PID 记录还在、进程已经不在：Mihomo 意外退出，快照立即触发检测。
	record := fmt.Sprintf(`{"pid":999999,"start_token":"x","executable":"/x","config_file":%q}`, paths.Generated)
	if err := os.WriteFile(paths.PIDFile, []byte(record), 0o600); err != nil {
		t.Fatal(err)
	}
	engine.snapshot()
	select {
	case <-engine.claudeKick:
	default:
		t.Fatal("Mihomo 退出后应立即检测 Claude 的线路")
	}
	engine.snapshot()
	select {
	case <-engine.claudeKick:
		t.Fatal("状态没变时不应重复触发检测")
	default:
	}

	if engine.handleClaudeEvent(claudeguard.LogEvent{Host: "claude.ai", Direct: true}) {
		t.Fatal("看到直连不需要再检测确认")
	}
	if got := engine.claudeView(); got.Level != claudeguard.LevelAlert || got.Key != claudeguard.KeyDirect {
		t.Fatalf("看到直连应立即告警，当前 %+v", got)
	}
}

func TestLocalClaudeNodeIsAlwaysDirect(t *testing.T) {
	engine, paths := newTestEngine(t, io.Discard)
	update := func(params string) error {
		_, err := engine.dispatch(context.Background(), Request{Method: "updateSettings", Params: json.RawMessage(params)})
		return err
	}
	// SSH 端口转发到本机的节点：保存后记成直接连接。
	if err := update(`{"claudeNode":{"scheme":"socks5","host":"127.0.0.1","port":10003,"username":"u","password":"wJ+K6"}}`); err != nil {
		t.Fatal(err)
	}
	saved, _ := config.Load(paths)
	if saved.ClaudeProxy != "socks5://u:wJ+K6@127.0.0.1:10003" || !saved.ClaudeProxyDirect {
		t.Fatalf("saved = %+v", saved)
	}
	var coded codedError
	if err := update(`{"claudeProxyDirect":false}`); !errors.As(err, &coded) || coded.code != CodeInvalidParams {
		t.Fatalf("chaining a local node must be rejected, got %v", err)
	}
	if !engine.currentSettings().ClaudeProxyDirect {
		t.Fatal("rejected change must not be saved")
	}

	// 界面改连接方式时连同没保存的节点一起提交。
	if err := update(`{"claudeNode":{"scheme":"http","host":"203.0.113.9","port":10003,"username":"u"},"claudeProxyDirect":false}`); err != nil {
		t.Fatal(err)
	}
	saved, _ = config.Load(paths)
	if saved.ClaudeProxy != "http://u:wJ+K6@203.0.113.9:10003" || saved.ClaudeProxyDirect {
		t.Fatalf("node and connection method should be saved together: %+v", saved)
	}
}

// TestProxyAuthMatchesCommandLine 覆盖桌面版的局域网代理认证：开启时自动生成
// 密码，密码不进快照、只能用 proxyCredentials 取，关闭时清除用户名和密码。
func TestProxyAuthMatchesCommandLine(t *testing.T) {
	engine, paths := newTestEngine(t, io.Discard)
	update := func(params string) error {
		_, err := engine.dispatch(context.Background(), Request{Method: "updateSettings", Params: json.RawMessage(params)})
		return err
	}

	if err := update(`{"proxyAuth":true}`); err == nil {
		t.Fatal("没有用户名时开启认证应被拒绝")
	}
	if err := update(`{"proxyAuth":true,"proxyUsername":"a:b"}`); err == nil {
		t.Fatal("含冒号的用户名应被拒绝")
	}
	if err := update(`{"allowLAN":true,"proxyAuth":true,"proxyUsername":" alice "}`); err != nil {
		t.Fatal(err)
	}
	saved, err := config.Load(paths)
	if err != nil {
		t.Fatal(err)
	}
	if saved.ProxyUsername != "alice" || len(saved.ProxyPassword) != 32 || !saved.AllowLAN {
		t.Fatalf("保存的认证 = %q / %q", saved.ProxyUsername, saved.ProxyPassword)
	}

	data, err := json.Marshal(engine.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), saved.ProxyPassword) {
		t.Fatal("快照不应包含代理密码")
	}
	var snapshot Snapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		t.Fatal(err)
	}
	if !snapshot.Settings.ProxyAuth || snapshot.Settings.ProxyUsername != "alice" || !snapshot.Settings.HasProxyPassword {
		t.Fatalf("快照里的认证 = %+v", snapshot.Settings)
	}
	result, err := engine.dispatch(context.Background(), Request{Method: "proxyCredentials"})
	if err != nil {
		t.Fatal(err)
	}
	if credentials := result.(ProxyCredentials); credentials.Password != saved.ProxyPassword {
		t.Fatalf("proxyCredentials = %+v", credentials)
	}

	// 改用户名保留密码；传空密码重新生成。
	if err := update(`{"proxyUsername":"bob"}`); err != nil {
		t.Fatal(err)
	}
	if got := engine.currentSettings(); got.ProxyUsername != "bob" || got.ProxyPassword != saved.ProxyPassword {
		t.Fatalf("改用户名后 = %q / %q", got.ProxyUsername, got.ProxyPassword)
	}
	if err := update(`{"proxyPassword":""}`); err != nil {
		t.Fatal(err)
	}
	if got := engine.currentSettings(); got.ProxyPassword == "" || got.ProxyPassword == saved.ProxyPassword {
		t.Fatal("空密码应重新生成")
	}

	if err := update(`{"proxyAuth":false}`); err != nil {
		t.Fatal(err)
	}
	if got := engine.currentSettings(); got.ProxyUsername != "" || got.ProxyPassword != "" {
		t.Fatalf("关闭认证后 = %q / %q", got.ProxyUsername, got.ProxyPassword)
	}
}

// TestTestDelaysReportsAndSnapshotShowsAutoNode 覆盖测速：没连接时直接说明，
// 连接后测完给出总结，快照里有自动选择此刻用的节点和每个节点的延迟。
func TestTestDelaysReportsAndSnapshotShowsAutoNode(t *testing.T) {
	engine, _ := newTestEngine(t, io.Discard)
	if _, err := engine.dispatch(context.Background(), Request{Method: "testDelays"}); err == nil || !strings.Contains(err.Error(), "连接后才能测速") {
		t.Fatalf("offline testDelays error = %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		path, _ := url.PathUnescape(request.URL.EscapedPath())
		switch path {
		case "/version":
			_, _ = writer.Write([]byte(`{"version":"test"}`))
		case "/proxies":
			_, _ = writer.Write([]byte(`{"proxies": {
				"外网代理": {"type": "Selector", "now": "自动选择", "all": ["自动选择", "香港 01", "日本 02", "DIRECT"]},
				"自动选择": {"type": "URLTest", "now": "日本 02", "all": ["香港 01", "日本 02"]},
				"日本 02": {"type": "Shadowsocks", "extra": {"https://www.gstatic.com/generate_204": {"alive": true, "history": [{"delay": 85}]}}},
				"香港 01": {"type": "Trojan"}}}`))
		case "/group/自动选择/delay":
			_, _ = writer.Write([]byte(`{"日本 02": 85}`))
		case "/connections":
			_, _ = writer.Write([]byte(`{"connections": []}`))
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	engine.mu.Lock()
	engine.settings.ControllerPort, _ = strconv.Atoi(port)
	engine.mu.Unlock()

	result, err := engine.dispatch(context.Background(), Request{Method: "testDelays"})
	if err != nil {
		t.Fatal(err)
	}
	if message := result.(ActionResult).Message; message != "测速完成：2 个节点里 1 个能用，最快的是 日本 02（85ms）" {
		t.Fatalf("message = %q", message)
	}
	snapshot := engine.snapshot()
	if len(snapshot.AutoGroups) != 1 || snapshot.AutoGroups[0].Now != "日本 02" || snapshot.Delays["日本 02"] != 85 {
		t.Fatalf("snapshot auto groups = %#v, delays = %#v", snapshot.AutoGroups, snapshot.Delays)
	}
	if len(snapshot.Groups) != 1 || snapshot.Groups[0].Name != "外网代理" || snapshot.DelayTesting {
		t.Fatalf("snapshot groups = %#v, testing = %v", snapshot.Groups, snapshot.DelayTesting)
	}
}

// TestFixBrowsingRetestsThenReportsNode 覆盖「网页打不开」：没连接时让 GUI
// 去重新连接；连接时先重新测速，外网通了就说出换到的节点，不通再重新连接。
func TestFixBrowsingRetestsThenReportsNode(t *testing.T) {
	engine, _ := newTestEngine(t, io.Discard)
	if result := engine.fixBrowsing(); result.OK {
		t.Fatalf("offline fixBrowsing = %#v", result)
	}

	var tested atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		path, _ := url.PathUnescape(request.URL.EscapedPath())
		switch path {
		case "/version":
			_, _ = writer.Write([]byte(`{"version":"test"}`))
		case "/proxies":
			_, _ = writer.Write([]byte(`{"proxies": {
				"外网代理": {"type": "Selector", "now": "自动选择", "all": ["自动选择", "日本 02", "DIRECT"]},
				"自动选择": {"type": "URLTest", "now": "日本 02", "all": ["日本 02"]}}}`))
		case "/group/自动选择/delay":
			tested.Add(1)
			_, _ = writer.Write([]byte(`{"日本 02": 85}`))
		default:
			writer.WriteHeader(http.StatusNoContent)
		}
	}))
	defer server.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	engine.mu.Lock()
	engine.settings.ControllerPort, _ = strconv.Atoi(port)
	engine.mu.Unlock()

	engine.probeProxy = func(controller.Client) diagnostics.ProxyResult {
		return diagnostics.ProxyResult{Route: "外网代理 -> 自动选择 -> 日本 02", Node: "日本 02", Delay: 85}
	}
	result := engine.fixBrowsing()
	if !result.OK || result.Message != "已换到能用的节点「日本 02」（85ms），再打开网页试试" || tested.Load() != 1 {
		t.Fatalf("fixBrowsing = %#v, tested %d times", result, tested.Load())
	}

	engine.probeProxy = func(controller.Client) diagnostics.ProxyResult {
		return diagnostics.ProxyResult{Route: "外网代理 -> 自动选择 -> 日本 02", Node: "日本 02"}
	}
	if result := engine.fixBrowsing(); result.OK || !strings.Contains(result.Message, "正在重新连接") {
		t.Fatalf("still broken: %#v", result)
	}
}
