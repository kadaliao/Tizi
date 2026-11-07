package claudeguard

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kadaliao/tizi/internal/claude"
	"github.com/kadaliao/tizi/internal/config"
	"github.com/kadaliao/tizi/internal/controller"
)

type fakeProber struct {
	groups     []controller.Group
	groupsErr  error
	chains     []string
	openErr    error
	noTunnel   bool
	lookupFail bool
}

func (f fakeProber) Groups() ([]controller.Group, error) { return f.groups, f.groupsErr }

func (f fakeProber) OpenProbe(int, string, int) (net.Conn, controller.Connection, error) {
	if f.noTunnel {
		return nil, controller.Connection{}, f.openErr
	}
	client, server := net.Pipe()
	_ = server.Close()
	if f.lookupFail {
		return client, controller.Connection{}, errors.New("no record")
	}
	return client, controller.Connection{Chains: f.chains}, nil
}

func residentialGroups(now string) []controller.Group {
	return []controller.Group{{Name: "Claude", Now: now, Choices: []string{"住宅节点", "自动选择"}}}
}

func withTrace(t *testing.T, trace Trace, err error) {
	t.Helper()
	old := readTrace
	readTrace = func(context.Context, net.Conn, string) (Trace, error) { return trace, err }
	t.Cleanup(func() { readTrace = old })
}

func observe(env Env) Status {
	env.Enabled = true
	if !env.Exited && env.Prober != nil {
		env.Running = true
	}
	return Observe(context.Background(), env)
}

func TestObserve(t *testing.T) {
	us := Trace{IP: "198.51.100.7", Country: "US"}
	for _, tc := range []struct {
		name    string
		env     Env
		trace   Trace
		err     error
		level   string
		key     string
		inTitle string
	}{
		{name: "residential ok", env: Env{TunEnabled: true, Prober: fakeProber{groups: residentialGroups("住宅节点"), chains: []string{"住宅节点", "Claude"}}}, trace: us, level: LevelOK, inTitle: "住宅节点"},
		{name: "proxy node ok", env: Env{TunEnabled: true, Prober: fakeProber{groups: []controller.Group{{Name: "Claude", Now: "自动选择", Choices: []string{"自动选择"}}}, chains: []string{"自动选择", "Claude"}}}, trace: us, level: LevelOK, inTitle: "代理节点"},
		{name: "switched away from residential", env: Env{TunEnabled: true, Prober: fakeProber{groups: residentialGroups("自动选择"), chains: []string{"自动选择", "Claude"}}}, trace: us, level: LevelAlert, key: KeyNotResidential},
		{name: "old config routes direct", env: Env{TunEnabled: true, Prober: fakeProber{groups: []controller.Group{{Name: "外网代理", Now: "DIRECT"}}, chains: []string{"DIRECT", "外网代理"}}}, trace: us, level: LevelAlert, key: KeyDirect},
		{name: "node down", env: Env{TunEnabled: true, Prober: fakeProber{groups: residentialGroups("住宅节点"), noTunnel: true, openErr: errors.New("HTTP 502")}}, level: LevelAlert, key: KeyUnreachable, inTitle: "住宅节点"},
		{name: "tls fails", env: Env{TunEnabled: true, Prober: fakeProber{groups: residentialGroups("住宅节点"), chains: []string{"住宅节点", "Claude"}}}, err: errors.New("EOF"), level: LevelAlert, key: KeyUnreachable},
		{name: "exit in hong kong", env: Env{TunEnabled: true, Prober: fakeProber{groups: residentialGroups("住宅节点"), chains: []string{"住宅节点", "Claude"}}}, trace: Trace{IP: "203.0.113.1", Country: "HK"}, level: LevelAlert, key: KeyRegion},
		{name: "no tun", env: Env{Prober: fakeProber{groups: residentialGroups("住宅节点"), chains: []string{"住宅节点", "Claude"}}}, trace: us, level: LevelWarn, key: KeyNoTUN},
		{name: "controller down", env: Env{TunEnabled: true, Prober: fakeProber{groupsErr: errors.New("refused")}}, level: LevelAlert, key: KeyController},
		{name: "record missing falls back to group", env: Env{TunEnabled: true, Prober: fakeProber{groups: residentialGroups("自动选择"), lookupFail: true}}, trace: us, level: LevelAlert, key: KeyNotResidential},
		{name: "mihomo crashed", env: Env{Exited: true}, level: LevelAlert, key: KeyVPNExited},
		{name: "no proxy node at all", env: Env{TunEnabled: true, Prober: fakeProber{groups: []controller.Group{{Name: "Claude", Now: "REJECT", Choices: []string{"REJECT"}}}}}, level: LevelAlert, key: KeyUnreachable, inTitle: "还没有"},
		{name: "node saved but not reloaded", env: Env{TunEnabled: true, Node: &claude.Node{}, Prober: fakeProber{groups: []controller.Group{{Name: "外网代理", Now: "自动选择"}}, chains: []string{"自动选择", "外网代理"}}}, trace: us, level: LevelWarn, key: KeyReloadNeeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withTrace(t, tc.trace, tc.err)
			got := observe(tc.env)
			if got.Level != tc.level || got.Key != tc.key || !strings.Contains(got.Title, tc.inTitle) {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

func TestObserveStates(t *testing.T) {
	if got := Observe(context.Background(), Env{}); got.Level != LevelOff {
		t.Fatalf("disabled guard: %+v", got)
	}
	if got := Observe(context.Background(), Env{Enabled: true}); got.Level != LevelIdle {
		t.Fatalf("user stopped VPN: %+v", got)
	}
}

func TestUnreachableExplainsFailClosedPolicy(t *testing.T) {
	withTrace(t, Trace{}, nil)
	settings := settingsWithProxy("socks5://u:p@203.0.113.9:1080")
	settings.ClaudeProxyDirect = true
	node, err := claude.Load(t.TempDir(), settings)
	if err != nil {
		t.Fatal(err)
	}
	got := observe(Env{TunEnabled: true, Node: node, Prober: fakeProber{groups: residentialGroups("住宅节点"), noTunnel: true, openErr: errors.New("HTTP 502")}})
	if !strings.Contains(got.Detail, "明文") || !strings.Contains(got.Detail, "不会自动改走") {
		t.Fatalf("detail should carry the node hint and the fail-closed policy: %q", got.Detail)
	}
}

func TestTrackerNeedsConsecutiveAlerts(t *testing.T) {
	tracker := NewTracker()
	now := time.Now()
	ok := Status{Level: LevelOK, CheckedAt: now}
	down := Status{Level: LevelAlert, Key: KeyUnreachable, CheckedAt: now.Add(10 * time.Second)}

	if got := tracker.Update(ok); got.Level != LevelOK || tracker.NextDelay() != CheckInterval {
		t.Fatalf("first ok: %+v", got)
	}
	got := tracker.Update(down)
	if got.Level != LevelOK || got.Pending != 1 || tracker.NextDelay() != ConfirmInterval {
		t.Fatalf("a single failure must only trigger a quick recheck: %+v", got)
	}
	down.CheckedAt = down.CheckedAt.Add(ConfirmInterval)
	if got := tracker.Update(down); got.Level != LevelAlert || got.Key != KeyUnreachable || tracker.NextDelay() != RecheckInterval {
		t.Fatalf("second failure must alert: %+v", got)
	}
	ok.CheckedAt = down.CheckedAt.Add(RecheckInterval)
	if got := tracker.Update(ok); got.Level != LevelOK || got.Pending != 0 || tracker.NextDelay() != CheckInterval {
		t.Fatalf("recovery must clear the alert at once: %+v", got)
	}
}

func TestTrackerConfirmsExitWithinSeconds(t *testing.T) {
	tracker := NewTracker()
	now := time.Now()
	exited := Status{Level: LevelAlert, Key: KeyVPNExited, CheckedAt: now}
	tracker.Update(exited)
	if tracker.NextDelay() != exitConfirmInterval {
		t.Fatalf("an exit must be rechecked after %s, got %s", exitConfirmInterval, tracker.NextDelay())
	}
	exited.CheckedAt = now.Add(exitConfirmInterval)
	if got := tracker.Update(exited); got.Key != KeyVPNExited || got.Level != LevelAlert {
		t.Fatalf("confirmed exit must alert: %+v", got)
	}
}

func TestTrackerGracePeriodAfterSleep(t *testing.T) {
	tracker := NewTracker()
	now := time.Now()
	tracker.Update(Status{Level: LevelOK, CheckedAt: now})
	woke := now.Add(10 * time.Minute)
	for i := 0; i < 3; i++ {
		got := tracker.Update(Status{Level: LevelAlert, Key: KeyUnreachable, CheckedAt: woke.Add(time.Duration(i) * ConfirmInterval)})
		if got.Level == LevelAlert {
			t.Fatalf("failures right after waking up must not alert: %+v", got)
		}
	}
	if got := tracker.Update(Status{Level: LevelAlert, Key: KeyUnreachable, CheckedAt: woke.Add(wakeGrace)}); got.Level != LevelAlert {
		t.Fatalf("failures that outlast the grace period must alert: %+v", got)
	}

	// 唤醒后发现 Mihomo 已经退出，照常告警：这时访问 Claude 会直连。
	tracker = NewTracker()
	tracker.Update(Status{Level: LevelOK, CheckedAt: now})
	tracker.Update(Status{Level: LevelAlert, Key: KeyVPNExited, CheckedAt: woke})
	if got := tracker.Update(Status{Level: LevelAlert, Key: KeyVPNExited, CheckedAt: woke.Add(time.Second)}); got.Level != LevelAlert {
		t.Fatalf("an exit must alert even right after waking up: %+v", got)
	}
}

func TestTrackerEvents(t *testing.T) {
	tracker := NewTracker()
	now := time.Now()
	tracker.Update(Status{Level: LevelOK, Residential: true, CheckedAt: now})
	tracker.Done(now)

	failure := LogEvent{Host: "claude.ai", Error: "connect failed"}
	if tracker.Event(failure, now.Add(time.Second)) {
		t.Fatal("failures logged right after a check come from the check itself and must not trigger another")
	}
	if !tracker.Event(failure, now.Add(eventCooldown+time.Second)) {
		t.Fatal("a later failure must trigger a check")
	}
	// 日志里的失败算第一次，紧接着的检测失败就告警。
	if got := tracker.Update(Status{Level: LevelAlert, Key: KeyUnreachable, CheckedAt: now.Add(eventCooldown + 2*time.Second)}); got.Level != LevelAlert {
		t.Fatalf("a logged failure confirmed by a check must alert at once: %+v", got)
	}
	if tracker.Event(failure, now.Add(time.Minute)) {
		t.Fatal("failures during an alert must not trigger extra checks")
	}

	tracker = NewTracker()
	tracker.Update(Status{Level: LevelOK, Residential: true, CheckedAt: now})
	if tracker.Event(LogEvent{Host: "api.anthropic.com", Direct: true}, now) {
		t.Fatal("a direct connection needs no confirming check")
	}
	if got := tracker.Status(); got.Level != LevelAlert || got.Key != KeyDirect || !got.Residential || !strings.Contains(got.Detail, "api.anthropic.com") {
		t.Fatalf("a direct connection must alert immediately: %+v", got)
	}
}

func TestTrackerResetAfterUserAction(t *testing.T) {
	tracker := NewTracker()
	now := time.Now()
	tracker.Update(Status{Level: LevelAlert, Key: KeyVPNExited, CheckedAt: now})
	tracker.Update(Status{Level: LevelAlert, Key: KeyVPNExited, CheckedAt: now.Add(time.Second)})
	tracker.Reset()
	if got := tracker.Status(); got.Level != LevelChecking {
		t.Fatalf("reset must drop the stale alert: %+v", got)
	}
	if got := tracker.Update(Status{Level: LevelAlert, Key: KeyVPNExited, CheckedAt: now.Add(2 * time.Second)}); got.Level == LevelAlert {
		t.Fatalf("reset must restart the count: %+v", got)
	}
}

func TestParseLog(t *testing.T) {
	for _, tc := range []struct {
		payload string
		want    LogEvent
		ok      bool
	}{
		{payload: "[TCP] 28.0.0.1:1 --> claude.ai:443 match DomainSuffix(claude.ai) using Claude[住宅节点]"},
		{payload: "[TCP] 28.0.0.1:1 --> claude.ai:443 match DomainSuffix(claude.ai) using 外网代理[DIRECT]", want: LogEvent{Host: "claude.ai", Direct: true}, ok: true},
		{payload: "[UDP] 28.0.0.1:1 --> statsig.anthropic.com:3478 doesn't match any rule using DIRECT", want: LogEvent{Host: "statsig.anthropic.com", Direct: true}, ok: true},
		{payload: "[TCP] dial Claude (match DomainSuffix/claude.ai) 28.0.0.1:1 --> claude.ai:443 error: connect failed", want: LogEvent{Host: "claude.ai", Error: "connect failed"}, ok: true},
		// 自定义规则把其他域名交给 Claude 组时，它们的失败也算。
		{payload: "[TCP] dial Claude (match DomainSuffix/example.org) 28.0.0.1:1 --> cdn.example.org:443 error: timeout", want: LogEvent{Host: "cdn.example.org", Error: "timeout"}, ok: true},
		{payload: "[TCP] 28.0.0.1:1 --> www.youtube.com:443 match Match using DIRECT"},
		{payload: "[UDP] 28.0.0.1:1 --> claude.ai:443 match AND(((Network,udp) && (DomainSuffix,claude.ai))) using REJECT"},
		{payload: "[TCP] dial 默认出口 (match Match/) 28.0.0.1:1 --> example.com:443 error: timeout"},
		{payload: "Start initial configuration in progress"},
	} {
		got, ok := ParseLog(tc.payload)
		if ok != tc.ok || got != tc.want {
			t.Errorf("ParseLog(%q) = %+v, %v; want %+v, %v", tc.payload, got, ok, tc.want, tc.ok)
		}
	}
}

func TestObserveOfflineDoesNotAlert(t *testing.T) {
	got := observe(Env{Offline: true, TunEnabled: true, Prober: fakeProber{groupsErr: errors.New("unused")}})
	if got.Level != LevelWarn || got.Key != KeyOffline {
		t.Fatalf("offline: %+v", got)
	}
}

func TestParseTrace(t *testing.T) {
	got := ParseTrace("fl=1\nh=claude.ai\nip=198.51.100.23\nloc=us\nwarp=off\n")
	if got.IP != "198.51.100.23" || got.Country != "US" {
		t.Fatalf("ParseTrace = %+v", got)
	}
}

func TestHeartbeat(t *testing.T) {
	dir := t.TempDir()
	if UIActive(dir) {
		t.Fatal("no heartbeat yet")
	}
	if err := TouchHeartbeat(dir); err != nil {
		t.Fatal(err)
	}
	if !UIActive(dir) {
		t.Fatal("fresh heartbeat must count as active")
	}
	old := time.Now().Add(-2 * heartbeatTTL)
	if err := os.Chtimes(filepath.Join(dir, heartbeatName), old, old); err != nil {
		t.Fatal(err)
	}
	if UIActive(dir) {
		t.Fatal("stale heartbeat must not count")
	}
	RemoveHeartbeat(dir)
	if _, err := os.Stat(filepath.Join(dir, heartbeatName)); !os.IsNotExist(err) {
		t.Fatal("heartbeat must be removed")
	}
}

func TestReadRuntime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := "mixed-port: 7890\nexternal-controller: 127.0.0.1:9090\nsecret: s\ntun:\n  enable: true\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadRuntime(path)
	if err != nil || got.MixedPort != 7890 || !got.TunEnabled || got.Client.BaseURL != "http://127.0.0.1:9090" || got.Client.Secret != "s" {
		t.Fatalf("ReadRuntime = %+v, %v", got, err)
	}
	if err := os.WriteFile(path, []byte("mixed-port: 7890\nexternal-controller: 0.0.0.0:9090\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRuntime(path); err == nil {
		t.Fatal("non-local controller must be rejected")
	}
}

func settingsWithProxy(address string) config.Settings {
	return config.Settings{ClaudeProxy: address}
}

func TestObserveWithDirectProtection(t *testing.T) {
	if got := observe(Env{Exited: true, Blocked: true}); got.Key != KeyVPNExited || !strings.Contains(got.Detail, "不会直连") {
		t.Fatalf("exit with protection: %+v", got)
	}
	if got := Observe(context.Background(), Env{Enabled: true, Blocked: true}); got.Level != LevelIdle || !strings.Contains(got.Detail, "不会直连") {
		t.Fatalf("idle with protection: %+v", got)
	}
	if got := Observe(context.Background(), Env{Enabled: true, Blocked: true, BlockError: "超时"}); !strings.Contains(got.Detail, "会直连") {
		t.Fatalf("idle with broken protection: %+v", got)
	}
	withTrace(t, Trace{IP: "198.51.100.7", Country: "US"}, nil)
	got := observe(Env{TunEnabled: true, Blocked: true, BlockError: "防火墙规则没有生效", Prober: fakeProber{groups: residentialGroups("住宅节点"), chains: []string{"住宅节点", "Claude"}}})
	if got.Level != LevelWarn || got.Key != KeyBlockInactive {
		t.Fatalf("running with broken protection: %+v", got)
	}
}

func TestObserveReportsNodeChangesNotYetApplied(t *testing.T) {
	load := func(address string, direct bool) *claude.Node {
		t.Helper()
		node, err := claude.Load(t.TempDir(), config.Settings{ClaudeProxy: address, ClaudeProxyDirect: direct, Subscription: "https://sub.example.com/x"})
		if err != nil {
			t.Fatal(err)
		}
		return node
	}
	// 设置改成了直接连接，运行中的 Mihomo 还在经前置代理链式连接旧节点。
	saved := load("socks5://u:p@203.0.113.9:10003", true)
	running := load("socks5://u:p@203.0.113.9:10003", false)
	down := fakeProber{groups: residentialGroups("住宅节点"), noTunnel: true, openErr: errors.New("HTTP 403")}
	up := fakeProber{groups: residentialGroups("住宅节点"), chains: []string{"住宅节点", "Claude"}}

	got := observe(Env{TunEnabled: true, Node: saved, Applied: running, Prober: down})
	if got.Level != LevelAlert || got.Key != KeyReloadNeeded ||
		!strings.Contains(got.Detail, "经前置代理链式连接 203.0.113.9:10003") ||
		!strings.Contains(got.Detail, "本机直接连接 203.0.113.9:10003") || !strings.Contains(got.Detail, "重载") {
		t.Fatalf("unapplied change should ask for a reload: %+v", got)
	}
	if strings.Contains(got.Detail, "明文") {
		t.Fatalf("hint must not describe the saved-but-unapplied node: %q", got.Detail)
	}

	withTrace(t, Trace{IP: "198.51.100.7", Country: "US"}, nil)
	got = observe(Env{TunEnabled: true, Node: saved, Applied: running, Prober: up})
	if got.Level != LevelWarn || got.Key != KeyReloadNeeded {
		t.Fatalf("working but unapplied change should warn: %+v", got)
	}
	got = observe(Env{TunEnabled: true, Node: saved, Applied: load("socks5://u:other@203.0.113.9:10003", true), Prober: up})
	if got.Key != KeyReloadNeeded || !strings.Contains(got.Detail, "用户名或密码") {
		t.Fatalf("password-only change should still ask for a reload: %+v", got)
	}
	got = observe(Env{TunEnabled: true, Node: saved, Applied: load("socks5://u:p@203.0.113.9:10003", true), Prober: up})
	if got.Level != LevelOK {
		t.Fatalf("applied node should be ok: %+v", got)
	}

	// 没有改动时，提示按运行中的节点给出。
	got = observe(Env{TunEnabled: true, Node: saved, Applied: saved, Prober: down})
	if got.Key != KeyUnreachable || !strings.Contains(got.Detail, "明文") {
		t.Fatalf("unchanged node should carry its hint: %+v", got)
	}
}

func TestReadRuntimeFindsResidentialNode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := "mixed-port: 7890\nexternal-controller: 127.0.0.1:9090\nproxies:\n" +
		"  - {name: intranet-http, type: http, server: 10.1.2.3, port: 8080}\n" +
		"  - {name: 住宅节点, type: socks5, server: 127.0.0.1, port: 10003}\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadRuntime(path)
	if err != nil || got.Residential["server"] != "127.0.0.1" || got.Residential["port"] != 10003 {
		t.Fatalf("ReadRuntime = %+v, %v", got, err)
	}
}
