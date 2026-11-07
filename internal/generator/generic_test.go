package generator

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kadaliao/tizi/internal/config"
	"github.com/kadaliao/tizi/internal/ovpn"
	"gopkg.in/yaml.v3"
)

type generated struct {
	Proxies        []map[string]any          `yaml:"proxies"`
	ProxyProviders map[string]map[string]any `yaml:"proxy-providers"`
	ProxyGroups    []map[string]any          `yaml:"proxy-groups"`
	Rules          []string                  `yaml:"rules"`
	DNS            map[string]any            `yaml:"dns"`
}

func (g generated) group(name string) map[string]any {
	for _, group := range g.ProxyGroups {
		if group["name"] == name {
			return group
		}
	}
	return nil
}

func (g generated) proxy(name string) map[string]any {
	for _, proxy := range g.Proxies {
		if proxy["name"] == name {
			return proxy
		}
	}
	return nil
}

func generateFor(t *testing.T, dir string, profile *ovpn.Profile, settings config.Settings, share bool) generated {
	t.Helper()
	oldLookup, oldLinkDNS := lookupIPAddr, linkDNSServers
	lookupIPAddr = func(ctx context.Context, host string) ([]net.IPAddr, error) { return nil, nil }
	linkDNSServers = func() []string { return nil }
	t.Cleanup(func() { lookupIPAddr, linkDNSServers = oldLookup, oldLinkDNS })
	output := filepath.Join(dir, "config.yaml")
	generate := Generate
	if share {
		generate = GenerateShare
	}
	if err := generate(profile, settings, output); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	var parsed generated
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		t.Fatal(err)
	}
	return parsed
}

func TestGenerateWithoutOVPNUsesSubscription(t *testing.T) {
	settings := config.DefaultSettings()
	settings.Subscription = "https://sub.example.com/api/v1/client?token=secret"
	got := generateFor(t, t.TempDir(), nil, settings, false)
	if got.group(config.IntranetGroup) != nil {
		t.Fatalf("no intranet group without an ovpn: %#v", got.ProxyGroups)
	}
	provider := got.ProxyProviders[config.SubscriptionProvider]
	if provider == nil || provider["type"] != "http" || provider["url"] != settings.Subscription || provider["path"] != "./providers/subscription.yaml" {
		t.Fatalf("unexpected subscription provider: %#v", got.ProxyProviders)
	}
	auto := got.group(config.AutoGroup)
	if auto == nil || auto["type"] != "url-test" || !reflect.DeepEqual(auto["use"], []any{config.SubscriptionProvider}) {
		t.Fatalf("unexpected auto group: %#v", auto)
	}
	if _, ok := auto["proxies"]; ok {
		t.Fatalf("empty proxies list must be omitted: %#v", auto)
	}
	proxy := got.group(config.ProxyGroup)
	if !reflect.DeepEqual(proxy["proxies"], []any{config.AutoGroup, "DIRECT"}) || !reflect.DeepEqual(proxy["use"], []any{config.SubscriptionProvider}) {
		t.Fatalf("unexpected 外网代理 group: %#v", proxy)
	}
	if !contains(got.Rules, "IP-CIDR,10.0.0.0/8,DIRECT,no-resolve") {
		t.Fatalf("private ranges must stay direct: %#v", got.Rules)
	}
	if len(got.Proxies) != 0 {
		t.Fatalf("no inline proxies expected: %#v", got.Proxies)
	}
}

func TestGenerateInlinesYAMLNodeFile(t *testing.T) {
	dir := t.TempDir()
	content := "proxies:\n  - {name: hk, type: trojan, server: hk.example.com, port: 443, password: x}\n  - {name: jp, type: ss, server: 203.0.113.5, port: 8388, cipher: aes-256-gcm, password: y}\n"
	if err := os.WriteFile(filepath.Join(dir, config.ProxiesFile), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	got := generateFor(t, dir, nil, config.DefaultSettings(), false)
	if got.proxy("hk") == nil || got.proxy("jp") == nil {
		t.Fatalf("YAML nodes must be inlined: %#v", got.Proxies)
	}
	if len(got.ProxyProviders) != 0 {
		t.Fatalf("YAML nodes need no provider: %#v", got.ProxyProviders)
	}
	if want := []any{config.AutoGroup, "hk", "jp", "DIRECT"}; !reflect.DeepEqual(got.group(config.ProxyGroup)["proxies"], want) {
		t.Fatalf("外网代理 = %#v, want %#v", got.group(config.ProxyGroup)["proxies"], want)
	}
	if want := []any{config.AutoGroup, "hk", "jp"}; !reflect.DeepEqual(got.group(config.ClaudeGroup)["proxies"], want) {
		t.Fatalf("Claude = %#v, want %#v", got.group(config.ClaudeGroup)["proxies"], want)
	}
}

func TestGenerateUsesFileProviderForShareLinks(t *testing.T) {
	dir := t.TempDir()
	content := "# my nodes\ntrojan://p@hk.example.com:443#hk\nss://YWVzLTI1Ni1nY206cA@203.0.113.5:8388#jp\n"
	if err := os.WriteFile(filepath.Join(dir, config.ProxiesFile), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	got := generateFor(t, dir, nil, config.DefaultSettings(), false)
	provider := got.ProxyProviders[config.NodesProvider]
	if provider == nil || provider["type"] != "file" || provider["path"] != "./proxies.yaml" {
		t.Fatalf("share links must go through a file provider: %#v", got.ProxyProviders)
	}
	// 手机上没有这个文件，分享的配置里不能引用它。
	shared := generateFor(t, dir, nil, config.DefaultSettings(), true)
	if len(shared.ProxyProviders) != 0 || shared.group(config.AutoGroup) != nil {
		t.Fatalf("share config must not reference the local node file: %#v", shared)
	}
}

func TestGenerateRejectsReservedNodeNames(t *testing.T) {
	dir := t.TempDir()
	content := "proxies:\n  - {name: DIRECT, type: socks5, server: 127.0.0.1, port: 1080}\n"
	if err := os.WriteFile(filepath.Join(dir, config.ProxiesFile), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Generate(nil, config.DefaultSettings(), filepath.Join(dir, "config.yaml")); err == nil || !strings.Contains(err.Error(), "冲突") {
		t.Fatalf("reserved node name must be rejected, got %v", err)
	}
}

func TestGenerateTailscale(t *testing.T) {
	settings := config.DefaultSettings()
	settings.TailscaleEnabled = true
	settings.TailscaleAuthKey = "tskey-auth-example"
	settings.TailscaleHostname = "my-mac"
	settings.TailscaleExitNode = "home-server"
	settings.TailscaleControlURL = "https://hs.example.com"
	dir := t.TempDir()
	got := generateFor(t, dir, nil, settings, false)
	node := got.proxy(config.TailscaleProxyName)
	if node == nil || node["type"] != "tailscale" || node["auth-key"] != "tskey-auth-example" || node["hostname"] != "my-mac" ||
		node["exit-node"] != "home-server" || node["control-url"] != "https://hs.example.com" || node["state-dir"] != "tailscale" {
		t.Fatalf("unexpected tailscale node: %#v", node)
	}
	if !reflect.DeepEqual(got.group(config.TailscaleGroup)["proxies"], []any{config.TailscaleProxyName}) {
		t.Fatalf("unexpected Tailscale group: %#v", got.ProxyGroups)
	}
	if !reflect.DeepEqual(got.group(config.ProxyGroup)["proxies"], []any{config.TailscaleProxyName, "DIRECT"}) {
		t.Fatalf("exit node must be offered in 外网代理: %#v", got.group(config.ProxyGroup))
	}
	policy, _ := got.DNS["nameserver-policy"].(map[string]any)
	if !reflect.DeepEqual(policy["+.ts.net"], []any{"tailscale://tailscale"}) {
		t.Fatalf("MagicDNS must resolve through the tailscale node: %#v", got.DNS)
	}
	if !contains(got.Rules, "IP-CIDR,100.64.0.0/10,Tailscale,no-resolve") {
		t.Fatalf("tailnet rule missing: %#v", got.Rules)
	}

	// 手机导入的配置里不带内置 Tailscale。
	shared := generateFor(t, dir, nil, settings, true)
	if shared.proxy(config.TailscaleProxyName) != nil || shared.group(config.TailscaleGroup) != nil {
		t.Fatalf("share config must not carry the tailscale node: %#v", shared)
	}
	for _, rule := range shared.Rules {
		if strings.Contains(rule, "Tailscale") {
			t.Fatalf("share config must not route to Tailscale: %s", rule)
		}
	}
}

func TestCustomRulesNeedTheirRoute(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "custom-rules.txt"), []byte("DOMAIN-SUFFIX,corp.example.com,内网\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := Generate(nil, config.DefaultSettings(), filepath.Join(dir, "config.yaml"))
	if err == nil || !strings.Contains(err.Error(), ".ovpn") {
		t.Fatalf("intranet rule without ovpn must be explained, got %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "custom-rules.txt"), []byte("DOMAIN-SUFFIX,home.example.com,Tailscale\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = Generate(nil, config.DefaultSettings(), filepath.Join(dir, "config.yaml"))
	if err == nil || !strings.Contains(err.Error(), "Tailscale") {
		t.Fatalf("tailscale rule without tailscale must be explained, got %v", err)
	}
}

func TestEffectiveIntranetFallsBackToProfile(t *testing.T) {
	profile := &ovpn.Profile{Routes: []string{"10.20.0.0/16"}, DNS: []string{"10.20.0.53"}, Domains: []string{"corp.example.com"}}
	got := EffectiveIntranet(profile, config.Settings{})
	if !got.CIDRsAuto || !got.DNSAuto || !got.DomainsAuto || !reflect.DeepEqual(got.CIDRs, []string{"10.20.0.0/16"}) {
		t.Fatalf("profile values must be used: %+v", got)
	}
	got = EffectiveIntranet(&ovpn.Profile{}, config.Settings{IntranetCIDRs: []string{"172.30.0.0/16"}})
	if got.CIDRsAuto || !reflect.DeepEqual(got.CIDRs, []string{"172.30.0.0/16"}) {
		t.Fatalf("settings must win: %+v", got)
	}
	if got := EffectiveIntranet(&ovpn.Profile{}, config.Settings{}); !reflect.DeepEqual(got.CIDRs, []string{"10.0.0.0/8"}) {
		t.Fatalf("default intranet CIDR = %+v", got)
	}
	if got := EffectiveIntranet(nil, config.Settings{IntranetCIDRs: []string{"10.0.0.0/8"}}); len(got.CIDRs) != 0 {
		t.Fatalf("no intranet without an ovpn: %+v", got)
	}
}

func TestCheckReady(t *testing.T) {
	dir := t.TempDir()
	if err := CheckReady(dir, config.Settings{}); err == nil {
		t.Fatal("nothing configured must not be ready")
	}
	for _, settings := range []config.Settings{
		{Subscription: "https://sub.example.com/x"},
		{OVPNPath: "/tmp/a.ovpn"},
		{TailscaleEnabled: true},
		{ClaudeProxy: "socks5://203.0.113.9:1080"},
	} {
		if err := CheckReady(dir, settings); err != nil {
			t.Fatalf("%+v should be ready: %v", settings, err)
		}
	}
}
