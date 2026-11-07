package diagnostics

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kadaliao/tizi/internal/config"
	"github.com/kadaliao/tizi/internal/controller"
	"github.com/kadaliao/tizi/internal/generator"
	"github.com/kadaliao/tizi/internal/system"
)

func TestPortCheck(t *testing.T) {
	tests := []struct {
		name           string
		owner          system.Process
		ownedByManaged bool
		wantLevel      Level
		wantDetail     string
	}{
		{name: "available", wantLevel: OK, wantDetail: "可用"},
		{name: "managed", owner: system.Process{PID: 42, Command: "mihomo"}, ownedByManaged: true, wantLevel: OK, wantDetail: "受管 mihomo (PID 42)"},
		{name: "conflict", owner: system.Process{PID: 43, Command: "other"}, wantLevel: Warn, wantDetail: "other (PID 43)"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			check := portCheck("DNS", 1053, test.owner, test.ownedByManaged)
			if check.Level != test.wantLevel || check.Detail != test.wantDetail || check.Name != "DNS 端口 1053" {
				t.Fatalf("portCheck() = %#v", check)
			}
		})
	}
}

func TestIPv6Check(t *testing.T) {
	tests := []struct {
		name       string
		iface      string
		err        error
		tunActive  bool
		wantLevel  Level
		wantDetail string
	}{
		{name: "no route", wantLevel: OK, wantDetail: "没有 IPv6 默认路由"},
		{name: "no route while connected", tunActive: true, wantLevel: OK, wantDetail: "没有 IPv6 默认路由"},
		{name: "leak while connected", iface: "en1", tunActive: true, wantLevel: Warn, wantDetail: "经 en1 直出"},
		{name: "route before connecting", iface: "en1", wantLevel: Info, wantDetail: "连接后会自动关闭"},
		{name: "unreadable", err: errors.New("boom"), tunActive: true, wantLevel: Info, wantDetail: "无法读取: boom"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			check := ipv6Check(test.iface, test.err, test.tunActive)
			if check.Level != test.wantLevel || check.Name != "IPv6 路由" || !strings.Contains(check.Detail, test.wantDetail) {
				t.Fatalf("ipv6Check() = %#v", check)
			}
		})
	}
}

func TestResidentialCheck(t *testing.T) {
	dir := t.TempDir()
	if got := residentialCheck(dir, config.Settings{}); got.Level != Info {
		t.Fatalf("no node: %+v", got)
	}
	if got := residentialCheck(dir, config.Settings{ClaudeProxy: "socks5://u:p@203.0.113.9:1080", Subscription: "https://sub.example.com/x"}); got.Level != OK || strings.Contains(got.Detail, "p@") || !strings.Contains(got.Detail, "前置代理") {
		t.Fatalf("chained node: %+v", got)
	}
	if got := residentialCheck(dir, config.Settings{ClaudeProxy: "socks5://u:p@203.0.113.9:1080"}); got.Level != Warn || !strings.Contains(got.Detail, "明文") {
		t.Fatalf("plaintext node dialed directly: %+v", got)
	}
	if got := residentialCheck(dir, config.Settings{ClaudeProxy: "ftp://x"}); got.Level != Fail {
		t.Fatalf("invalid node: %+v", got)
	}
}

func TestUpstreamCheck(t *testing.T) {
	dir := t.TempDir()
	if got := upstreamCheck(dir, config.Settings{}); got.Level != Warn {
		t.Fatalf("nothing configured: %+v", got)
	}
	got := upstreamCheck(dir, config.Settings{Subscription: "https://sub.example.com/api?token=secret"})
	if got.Level != OK || strings.Contains(got.Detail, "secret") {
		t.Fatalf("subscription must be reported masked: %+v", got)
	}
	if got := upstreamCheck(dir, config.Settings{Subscription: "ftp://x"}); got.Level != Fail {
		t.Fatalf("invalid subscription: %+v", got)
	}
}

func TestIntranetChecksWarnWithoutDNS(t *testing.T) {
	checks := intranetChecks(generator.Intranet{Domains: []string{"corp.example.com"}, CIDRs: []string{"10.0.0.0/8"}, CIDRsAuto: true}, config.Settings{})
	var dns *Check
	for i := range checks {
		if checks[i].Name == "内网 DNS" {
			dns = &checks[i]
		}
	}
	if dns == nil || dns.Level != Warn {
		t.Fatalf("intranet domains without DNS must warn: %+v", checks)
	}
	if !strings.Contains(checks[0].Detail, "来自 .ovpn") {
		t.Fatalf("auto CIDRs must say where they come from: %+v", checks[0])
	}
}

func TestClaudeBlockCheck(t *testing.T) {
	if got := claudeBlockCheck(false, nil); got.Level != Info {
		t.Fatalf("not installed: %+v", got)
	}
	if got := claudeBlockCheck(true, func() error { return nil }); got.Level != OK {
		t.Fatalf("working: %+v", got)
	}
	if got := claudeBlockCheck(true, func() error { return errors.New("超时") }); got.Level != Fail {
		t.Fatalf("broken: %+v", got)
	}
}

type fakeProbe struct {
	groups []controller.Group
	auto   []controller.Group
	delay  int
	err    error
	probed []string
}

func (f *fakeProbe) Overview(string) (controller.Overview, error) {
	return controller.Overview{Groups: f.groups, Auto: f.auto}, nil
}

func (f *fakeProbe) ProxyDelay(name, _ string, _ time.Duration) (int, error) {
	f.probed = append(f.probed, name)
	return f.delay, f.err
}

func TestProxyCheckProbesThroughProxyGroup(t *testing.T) {
	groups := []controller.Group{{Name: config.ProxyGroup, Now: config.AutoGroup}}
	auto := []controller.Group{{Name: config.AutoGroup, Now: "日本 02"}}

	ok := &fakeProbe{groups: groups, auto: auto, delay: 85}
	if check := ProxyCheck(ok); check.Level != OK || check.Detail != "外网代理 -> 自动选择 -> 日本 02，访问 www.gstatic.com 85ms" {
		t.Fatalf("working route: %#v", check)
	}
	if len(ok.probed) != 1 || ok.probed[0] != config.ProxyGroup {
		t.Fatalf("must probe the 外网代理 group itself, probed %v", ok.probed)
	}

	// 「自动选择」停在一个已经坏掉的节点上：诊断必须报出来，而不是显示正常。
	broken := &fakeProbe{groups: groups, auto: auto, err: errors.New("timeout")}
	if check := ProxyCheck(broken); check.Level != Fail || !strings.Contains(check.Detail, "日本 02 访问 www.gstatic.com 失败") {
		t.Fatalf("broken route: %#v", check)
	}

	direct := &fakeProbe{groups: []controller.Group{{Name: config.ProxyGroup, Now: "DIRECT"}}}
	if check := ProxyCheck(direct); check.Level != Warn || len(direct.probed) != 0 {
		t.Fatalf("direct route: %#v, probed %v", check, direct.probed)
	}
}
