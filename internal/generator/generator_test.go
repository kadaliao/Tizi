package generator

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/kadaliao/tizi/internal/config"
	"github.com/kadaliao/tizi/internal/ovpn"
	"gopkg.in/yaml.v3"
)

func TestGenerateSelectedRemoteAndPermissions(t *testing.T) {
	oldLookup := lookupIPAddr
	lookupIPAddr = func(ctx context.Context, host string) ([]net.IPAddr, error) {
		if host != "b.example.com" {
			t.Fatalf("unexpected host lookup: %s", host)
		}
		return []net.IPAddr{{IP: net.ParseIP("203.0.113.7")}}, nil
	}
	t.Cleanup(func() { lookupIPAddr = oldLookup })

	profile := ovpn.Profile{
		Remotes: []ovpn.Remote{{Host: "a.example.com", Port: 1194}, {Host: "b.example.com", Port: 443}},
		Proto:   "udp", Dev: "tun", Cipher: "AES-128-GCM", Auth: "SHA256", CompLZO: "no",
		CA:   "-----BEGIN CERTIFICATE-----\nCA\n-----END CERTIFICATE-----",
		Cert: "-----BEGIN CERTIFICATE-----\nCERT\n-----END CERTIFICATE-----",
		Key:  "-----BEGIN PRIVATE KEY-----\nKEY\n-----END PRIVATE KEY-----",
	}
	settings := config.DefaultSettings()
	settings.Remote = "b"
	settings.ControllerSecret = "test-secret"
	settings.AllowLAN = true
	settings.IntranetDomains = []string{"corp.example.com"}
	settings.IntranetDNS = []string{"10.20.0.53"}
	settings.IntranetHTTPProxy = "10.20.0.2:3128"
	output := filepath.Join(t.TempDir(), "config.yaml")
	if err := Generate(&profile, settings, output); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(output)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("unexpected permissions: %o", info.Mode().Perm())
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Proxies     []map[string]any `yaml:"proxies"`
		ProxyGroups []map[string]any `yaml:"proxy-groups"`
		Rules       []string         `yaml:"rules"`
		DNS         map[string]any   `yaml:"dns"`
		Sniffer     map[string]any   `yaml:"sniffer"`
		Secret      string           `yaml:"secret"`
		Tun         map[string]any   `yaml:"tun"`
		AllowLAN    bool             `yaml:"allow-lan"`
		BindAddress string           `yaml:"bind-address"`
	}
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Proxies) != 2 || parsed.Proxies[0]["server"] != "b.example.com" || parsed.Proxies[1]["name"] != config.IntranetHTTPProxyName {
		t.Fatalf("unexpected proxies: %#v", parsed.Proxies)
	}
	if parsed.Proxies[0]["remote-dns-resolve"] != true || !reflect.DeepEqual(parsed.Proxies[0]["dns"], []any{"10.20.0.53"}) {
		t.Fatalf("OpenVPN node must resolve through the intranet DNS: %#v", parsed.Proxies[0])
	}
	if len(parsed.Rules) == 0 || parsed.Rules[len(parsed.Rules)-1] != "MATCH,默认出口" {
		t.Fatalf("unexpected rules: %#v", parsed.Rules)
	}
	if parsed.Secret != "test-secret" || parsed.Proxies[1]["dialer-proxy"] != "内网" {
		t.Fatalf("unexpected controller or dialer settings: %#v", parsed)
	}
	if !parsed.AllowLAN || parsed.BindAddress != "0.0.0.0" {
		t.Fatalf("unexpected LAN listener configuration: %#v", parsed)
	}
	if parsed.Proxies[0]["handshake-timeout"] != 15 {
		t.Fatalf("unexpected OpenVPN handshake timeout: %#v", parsed.Proxies[0])
	}
	if parsed.Proxies[0]["mtu"] != tunnelMTU || parsed.Tun["mtu"] != tunnelMTU {
		t.Fatalf("tunnel MTU is not consistently bounded: proxy=%#v tun=%#v", parsed.Proxies[0], parsed.Tun)
	}
	defaultChoices, ok := parsed.ProxyGroups[2]["proxies"].([]any)
	if !ok || !reflect.DeepEqual(defaultChoices, []any{"外网代理", "DIRECT"}) {
		t.Fatalf("unexpected default route choices: %#v", parsed.ProxyGroups[2])
	}
	fallback, ok := parsed.DNS["fallback"].([]any)
	if !ok || !reflect.DeepEqual(fallback, []any{"https://1.1.1.1/dns-query#外网代理"}) {
		t.Fatalf("unexpected DNS fallback: %#v", parsed.DNS)
	}
	fallbackFilter, ok := parsed.DNS["fallback-filter"].(map[string]any)
	if !ok || fallbackFilter["geoip-code"] != "CN" || fallbackFilter["fallback-lazy-query"] != false {
		t.Fatalf("unexpected DNS fallback filter: %#v", parsed.DNS)
	}
	nameserverPolicy, ok := parsed.DNS["nameserver-policy"].(map[string]any)
	if !ok || !reflect.DeepEqual(nameserverPolicy["+.corp.example.com"], []any{"10.20.0.53#内网"}) {
		t.Fatalf("intranet domains must use the intranet DNS through the tunnel: %#v", parsed.DNS)
	}
	fakeIPFilter, ok := parsed.DNS["fake-ip-filter"].([]any)
	if !ok || !reflect.DeepEqual(fakeIPFilter, []any{"+.corp.example.com"}) {
		t.Fatalf("intranet domains are not excluded from fake IP responses: %#v", parsed.DNS)
	}
	if !contains(parsed.Rules, "DOMAIN-SUFFIX,corp.example.com,内网") || !contains(parsed.Rules, "IP-CIDR,10.0.0.0/8,内网,no-resolve") {
		t.Fatalf("intranet rules missing: %#v", parsed.Rules)
	}
	proxyChoices, _ := parsed.ProxyGroups[1]["proxies"].([]any)
	if parsed.ProxyGroups[1]["name"] != config.ProxyGroup || !reflect.DeepEqual(proxyChoices, []any{config.IntranetHTTPProxyName, "DIRECT"}) {
		t.Fatalf("intranet HTTP egress must be offered in 外网代理: %#v", parsed.ProxyGroups[1])
	}
	if parsed.Sniffer["enable"] != true || parsed.Sniffer["parse-pure-ip"] != true || parsed.Sniffer["override-destination"] != true {
		t.Fatalf("unexpected sniffer configuration: %#v", parsed.Sniffer)
	}
	// 微信按 HTTPDNS 选出的 IP 直连，嗅探结果不能改写目标地址。
	if !reflect.DeepEqual(parsed.Sniffer["skip-domain"], []any{"+.weixin.qq.com", "+.weixin.qq.com.cn"}) {
		t.Fatalf("WeChat domains must skip sniffer override: %#v", parsed.Sniffer)
	}
	excludes, ok := parsed.Tun["route-exclude-address"].([]any)
	if !ok {
		t.Fatalf("missing route excludes: %#v", parsed.Tun)
	}
	wantExcludes := []any{"203.0.113.7/32"}
	if runtime.GOOS == "linux" {
		wantExcludes = append(wantExcludes, "169.254.0.0/16")
	}
	if !reflect.DeepEqual(excludes, wantExcludes) {
		t.Fatalf("unexpected route excludes: %#v", excludes)
	}
}

// testRules 生成带一个内网（corp.example.com、10.0.0.0/8）的规则。
func testRules(chinaDirect bool, custom []string) []string {
	return rules(plan{
		intranet:    true,
		intranetNet: Intranet{Domains: []string{"corp.example.com"}, CIDRs: []string{"10.0.0.0/8"}},
		chinaDirect: chinaDirect,
		custom:      custom,
	})
}

func TestRulesUseConfiguredChinaRoute(t *testing.T) {
	if got := testRules(true, nil); !contains(got, "GEOIP,CN,DIRECT") {
		t.Fatalf("direct China rule missing: %#v", got)
	}
	if got := testRules(false, nil); !contains(got, "GEOIP,CN,外网代理") {
		t.Fatalf("proxy China rule missing: %#v", got)
	}
}

func TestRulesRoutePublicTrafficByGeoIP(t *testing.T) {
	got := testRules(true, nil)
	chinaIndex := indexOf(got, "GEOIP,CN,DIRECT")
	matchIndex := indexOf(got, "MATCH,默认出口")
	if chinaIndex < 0 || matchIndex <= chinaIndex {
		t.Fatalf("GeoIP and fallback rules out of order: %#v", got)
	}
	for _, rule := range got {
		if strings.HasPrefix(rule, "DOMAIN-SUFFIX,") && strings.HasSuffix(rule, ",外网代理") {
			// PinnedProxyDomains 是唯一允许固定走外网代理的公共域名族：
			// 它们靠解析判定归属会让路由结果随 DNS 漂移，必须写死。
			if isPinnedDomainRule(rule) {
				continue
			}
			t.Errorf("public domain must use GeoIP fallback instead of a built-in rule: %s", rule)
		}
	}
	if !contains(got, "AND,((NETWORK,TCP),(DST-PORT,5223),(IP-CIDR,17.0.0.0/8)),DIRECT") {
		t.Error("APNs direct exception missing")
	}
	if !contains(got, "AND,((NETWORK,TCP),(DST-PORT,80)),DIRECT") {
		t.Error("HTTP direct exception missing")
	}
}

func TestRulesKeepIntranetIPRoutingByAddress(t *testing.T) {
	got := testRules(true, nil)
	if !contains(got, "IP-CIDR,10.0.0.0/8,内网,no-resolve") {
		t.Fatalf("non-resolving intranet IP rule missing: %#v", got)
	}
}

func TestRulesRouteIntranetDomainsBeforeIntranetIPs(t *testing.T) {
	got := testRules(true, nil)
	domainIndex := indexOf(got, "DOMAIN-SUFFIX,corp.example.com,内网")
	intranetIPIndex := indexOf(got, "IP-CIDR,10.0.0.0/8,内网,no-resolve")
	if domainIndex < 0 {
		t.Fatalf("intranet domain rule missing: %#v", got)
	}
	if intranetIPIndex < 0 || domainIndex > intranetIPIndex {
		t.Fatalf("intranet domain rule must precede the non-resolving intranet IP rule: %#v", got)
	}
}

func TestRulesWithoutIntranetKeepPrivateRangesDirect(t *testing.T) {
	got := rules(plan{chinaDirect: true})
	for _, rule := range got {
		if strings.HasSuffix(rule, ","+config.IntranetGroup) || strings.Contains(rule, ","+config.IntranetGroup+",") {
			t.Fatalf("no intranet rule may exist without an ovpn: %s", rule)
		}
	}
	if !contains(got, "IP-CIDR,10.0.0.0/8,DIRECT,no-resolve") {
		t.Fatalf("10.0.0.0/8 must stay direct without an intranet: %#v", got)
	}
}

func TestRulesRouteTailnetBeforeCGNATDirect(t *testing.T) {
	got := rules(plan{chinaDirect: true, tailscale: true})
	tailnet := indexOf(got, "IP-CIDR,100.64.0.0/10,Tailscale,no-resolve")
	direct := indexOf(got, "IP-CIDR,100.64.0.0/10,DIRECT,no-resolve")
	if tailnet < 0 || direct < 0 || tailnet > direct || !contains(got, "DOMAIN-SUFFIX,ts.net,Tailscale") {
		t.Fatalf("tailnet rules missing or out of order: %#v", got)
	}
}

func TestRulesKeepTencentDirectWithoutResolution(t *testing.T) {
	got := testRules(true, nil)
	chinaIndex := indexOf(got, "GEOIP,CN,DIRECT")
	for _, rule := range []string{
		"DOMAIN-SUFFIX,qq.com,DIRECT",
		"DOMAIN-SUFFIX,qq.com.cn,DIRECT",
		"DOMAIN-SUFFIX,qpic.cn,DIRECT",
		"DOMAIN-SUFFIX,qlogo.cn,DIRECT",
		"DOMAIN-SUFFIX,wechat.com,DIRECT",
		"DOMAIN-SUFFIX,tencent.com,DIRECT",
	} {
		index := indexOf(got, rule)
		if index < 0 {
			t.Errorf("Tencent direct rule missing: %s", rule)
		} else if index > chinaIndex {
			t.Errorf("Tencent direct rule must precede GeoIP fallback: %s", rule)
		}
	}
}

func TestGenerateResolvesCloudDomainsWithLinkDNS(t *testing.T) {
	oldLookup, oldLinkDNS := lookupIPAddr, linkDNSServers
	lookupIPAddr = func(ctx context.Context, host string) ([]net.IPAddr, error) { return nil, nil }
	linkDNSServers = func() []string { return []string{"183.60.83.19", "127.0.0.53", "fe80::1", "183.60.82.98"} }
	t.Cleanup(func() { lookupIPAddr, linkDNSServers = oldLookup, oldLinkDNS })

	profile := ovpn.Profile{Remotes: []ovpn.Remote{{Host: "203.0.113.7", Port: 1194}}, Proto: "udp"}
	settings := config.DefaultSettings()
	output := filepath.Join(t.TempDir(), "config.yaml")
	if err := Generate(&profile, settings, output); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		DNS map[string]any `yaml:"dns"`
	}
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		t.Fatal(err)
	}
	policy, _ := parsed.DNS["nameserver-policy"].(map[string]any)
	want := []any{"183.60.83.19#DIRECT", "183.60.82.98#DIRECT"}
	if !reflect.DeepEqual(policy["+.tencentyun.com"], want) {
		t.Fatalf("cloud domains must use the link DNS directly: %#v", policy)
	}

	linkDNSServers = func() []string { return nil }
	if err := Generate(&profile, settings, output); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	parsed.DNS = nil
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		t.Fatal(err)
	}
	policy, _ = parsed.DNS["nameserver-policy"].(map[string]any)
	if _, exists := policy["+.tencentyun.com"]; exists {
		t.Fatalf("cloud DNS policy without link DNS: %#v", policy)
	}
}

func TestRulesKeepCloudInternalTrafficDirect(t *testing.T) {
	got := testRules(false, nil)
	chinaIndex := indexOf(got, "GEOIP,CN,外网代理")
	for _, rule := range []string{
		"DOMAIN-SUFFIX,tencentyun.com,DIRECT",
		"IP-CIDR,169.254.0.0/16,DIRECT,no-resolve",
	} {
		index := indexOf(got, rule)
		if index < 0 {
			t.Errorf("cloud internal direct rule missing: %s", rule)
		} else if index > chinaIndex {
			t.Errorf("cloud internal direct rule must precede GeoIP fallback: %s", rule)
		}
	}
}

func TestRulesKeepFeishuTrafficOnDirectRoute(t *testing.T) {
	got := testRules(true, nil)
	chinaIndex := indexOf(got, "GEOIP,CN,DIRECT")
	for _, rule := range []string{
		"DOMAIN-SUFFIX,feishu.cn,DIRECT",
		"DOMAIN-SUFFIX,feishucdn.com,DIRECT",
		"DOMAIN-SUFFIX,larksuite.com,DIRECT",
		"DOMAIN-SUFFIX,byteimg.com,DIRECT",
	} {
		index := indexOf(got, rule)
		if index < 0 {
			t.Errorf("Feishu direct rule missing: %s", rule)
		} else if index > chinaIndex {
			t.Errorf("Feishu direct rule must precede GeoIP fallback: %s", rule)
		}
	}
}

func TestCustomRulesInsertedBeforeTail(t *testing.T) {
	dir := t.TempDir()
	content := "# 注释行\n\nDOMAIN-SUFFIX,example.com,外网代理\nDOMAIN-SUFFIX,internal.test,内网\n"
	if err := os.WriteFile(filepath.Join(dir, "custom-rules.txt"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	custom, err := customRules(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(custom) != 2 || custom[0] != "DOMAIN-SUFFIX,example.com,外网代理" {
		t.Fatalf("unexpected custom rules: %#v", custom)
	}
	got := testRules(true, custom)
	customIndex := indexOf(got, "DOMAIN-SUFFIX,example.com,外网代理")
	builtinIndex := indexOf(got, "IP-CIDR,10.0.0.0/8,内网,no-resolve")
	tailIndex := indexOf(got, "IP-CIDR,127.0.0.0/8,DIRECT,no-resolve")
	if customIndex < 0 || builtinIndex < 0 || tailIndex < 0 {
		t.Fatalf("expected rules missing: %#v", got)
	}
	if customIndex < builtinIndex || customIndex > tailIndex {
		t.Fatalf("custom rule at %d, want between builtin %d and tail %d", customIndex, builtinIndex, tailIndex)
	}
}

func TestCustomRulesMissingFile(t *testing.T) {
	got, err := customRules(t.TempDir())
	if err != nil || got != nil {
		t.Fatalf("missing file should yield no rules: %#v, err=%v", got, err)
	}
}

func TestCustomRulesInvalidLineReportsLineNumber(t *testing.T) {
	dir := t.TempDir()
	content := "# ok\nDOMAIN-SUFFIX,example.com,DIRECT\nDOMAIN-SUFIX,typo.example,DIRECT\n"
	if err := os.WriteFile(filepath.Join(dir, "custom-rules.txt"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := customRules(dir)
	if err == nil || !strings.Contains(err.Error(), "custom-rules.txt 第 3 行") {
		t.Fatalf("expected line-numbered error, got %v", err)
	}
	profile := ovpn.Profile{
		Remotes: []ovpn.Remote{{Host: "203.0.113.1", Port: 1194}},
		Proto:   "udp", Dev: "tun", Cipher: "AES-128-GCM", Auth: "SHA256", CompLZO: "no",
		CA:   "-----BEGIN CERTIFICATE-----\nCA\n-----END CERTIFICATE-----",
		Cert: "-----BEGIN CERTIFICATE-----\nCERT\n-----END CERTIFICATE-----",
		Key:  "-----BEGIN PRIVATE KEY-----\nKEY\n-----END PRIVATE KEY-----",
	}
	output := filepath.Join(dir, "config.yaml")
	if err := Generate(&profile, config.DefaultSettings(), output); err == nil || !strings.Contains(err.Error(), "第 3 行") {
		t.Fatalf("Generate should surface the invalid custom rule, got %v", err)
	}
	if _, statErr := os.Stat(output); !os.IsNotExist(statErr) {
		t.Fatalf("config must not be written when custom rules are invalid: %v", statErr)
	}
}

func isPinnedDomainRule(rule string) bool {
	for _, domain := range config.PinnedProxyDomains {
		if rule == "DOMAIN-SUFFIX,"+domain+",外网代理" {
			return true
		}
	}
	return false
}

func TestRulesPinFixedDomainsBeforeGeoIPFallback(t *testing.T) {
	got := testRules(true, nil)
	chinaIndex := indexOf(got, "GEOIP,CN,DIRECT")
	if chinaIndex < 0 {
		t.Fatalf("GeoIP rule missing: %#v", got)
	}
	for _, domain := range config.PinnedProxyDomains {
		rule := "DOMAIN-SUFFIX," + domain + ",外网代理"
		index := indexOf(got, rule)
		if index < 0 {
			t.Errorf("media proxy rule missing: %s", rule)
		} else if index > chinaIndex {
			t.Errorf("media proxy rule must precede GeoIP fallback: %s", rule)
		}
	}
}

// Claude 的域名固定走「Claude」组，并且排在自定义规则之前：custom-rules.txt
// 不能把它们改成直连。这一条锁的是清单内容和顺序。
func TestClaudeDomainsUseClaudeGroupBeforeCustomRules(t *testing.T) {
	custom := []string{"DOMAIN-SUFFIX,claude.ai,DIRECT"}
	got := testRules(true, custom)
	customIndex := indexOf(got, custom[0])
	chinaIndex := indexOf(got, "GEOIP,CN,DIRECT")
	if customIndex < 0 || chinaIndex < 0 {
		t.Fatalf("custom or GeoIP rule missing: %#v", got)
	}
	for _, domain := range []string{"claude.ai", "claude.com", "anthropic.com", "claudeusercontent.com"} {
		rule := "DOMAIN-SUFFIX," + domain + ",Claude"
		index := indexOf(got, rule)
		if index < 0 {
			t.Fatalf("Claude rule missing: %s", rule)
		}
		if index > customIndex {
			t.Errorf("Claude rule must precede custom rules: %s", rule)
		}
		// HTTP 类出口不支持 UDP，Mihomo 会跳过上一条规则；必须紧跟拦截，
		// 否则 UDP 会一路落到 DIRECT。
		udp := "AND,((NETWORK,UDP),(DOMAIN-SUFFIX," + domain + ")),REJECT"
		udpIndex := indexOf(got, udp)
		if udpIndex < index || udpIndex > customIndex {
			t.Errorf("UDP block for %s must sit between the Claude rule and custom rules: %#v", domain, got)
		}
		if contains(got, "DOMAIN-SUFFIX,"+domain+",外网代理") {
			t.Errorf("%s must not route through 外网代理, which can be switched to DIRECT", domain)
		}
	}
}

func TestClaudeGroupNeverOffersDirect(t *testing.T) {
	profile := ovpn.Profile{
		Remotes: []ovpn.Remote{{Host: "203.0.113.1", Port: 1194}},
		Proto:   "udp", Dev: "tun", Cipher: "AES-128-GCM", Auth: "SHA256",
		CA: "-----BEGIN CERTIFICATE-----\nCA\n-----END CERTIFICATE-----",
	}
	for _, tc := range []struct {
		name         string
		address      string
		direct       bool
		subscription bool
		choices      []any
		dialer       any
		hasNode      bool
	}{
		{name: "nothing to reach Claude", choices: []any{"REJECT"}},
		{name: "subscription only", subscription: true, choices: []any{"自动选择"}},
		{name: "chained residential node", address: "socks5://u:p@203.0.113.9:443", subscription: true, choices: []any{"住宅节点", "自动选择"}, dialer: "前置代理", hasNode: true},
		{name: "direct residential node", address: "https://u:p@203.0.113.9:8443", direct: true, subscription: true, choices: []any{"住宅节点", "自动选择"}, hasNode: true},
		{name: "residential node without front proxy", address: "socks5://u:p@203.0.113.9:443", choices: []any{"住宅节点"}, hasNode: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings := config.DefaultSettings()
			settings.ClaudeProxy = tc.address
			settings.ClaudeProxyDirect = tc.direct
			if tc.subscription {
				settings.Subscription = "https://sub.example.com/api?token=x"
			}
			output := filepath.Join(t.TempDir(), "config.yaml")
			if err := Generate(&profile, settings, output); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(output)
			if err != nil {
				t.Fatal(err)
			}
			var parsed struct {
				Proxies     []map[string]any `yaml:"proxies"`
				ProxyGroups []map[string]any `yaml:"proxy-groups"`
			}
			if err := yaml.Unmarshal(data, &parsed); err != nil {
				t.Fatal(err)
			}
			var group map[string]any
			for _, candidate := range parsed.ProxyGroups {
				if candidate["name"] == "Claude" {
					group = candidate
				}
			}
			if group == nil || !reflect.DeepEqual(group["proxies"], tc.choices) {
				t.Fatalf("unexpected Claude group: %#v", group)
			}
			for _, choice := range tc.choices {
				if choice == "DIRECT" {
					t.Fatalf("Claude group must never offer DIRECT: %#v", group)
				}
			}
			var node map[string]any
			for _, proxy := range parsed.Proxies {
				if proxy["name"] == "住宅节点" {
					node = proxy
				}
			}
			if (node != nil) != tc.hasNode {
				t.Fatalf("residential node presence = %v, want %v", node != nil, tc.hasNode)
			}
			if node != nil && node["dialer-proxy"] != tc.dialer {
				t.Fatalf("dialer-proxy = %#v, want %#v", node["dialer-proxy"], tc.dialer)
			}
		})
	}
}

func TestCustomRulesCanOverrideMediaRouting(t *testing.T) {
	got := testRules(true, []string{"DOMAIN-SUFFIX,youtube.com,DIRECT"})
	customIndex := indexOf(got, "DOMAIN-SUFFIX,youtube.com,DIRECT")
	mediaIndex := indexOf(got, "DOMAIN-SUFFIX,youtube.com,外网代理")
	if customIndex < 0 || mediaIndex < 0 {
		t.Fatalf("expected both rules present: %#v", got)
	}
	if customIndex > mediaIndex {
		t.Fatalf("custom rule at %d must precede built-in media rule at %d", customIndex, mediaIndex)
	}
}

func indexOf(values []string, want string) int {
	for i, value := range values {
		if value == want {
			return i
		}
	}
	return -1
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestLANProxyAuthentication(t *testing.T) {
	settings := config.DefaultSettings()
	settings.ProxyUsername = "alice"
	settings.ProxyPassword = "secret"
	if got := proxyAuthentication(settings); got != nil {
		t.Fatalf("loopback-only proxy must not require auth: %v", got)
	}
	settings.AllowLAN = true
	if got := proxyAuthentication(settings); !reflect.DeepEqual(got, []string{"alice:secret"}) {
		t.Fatalf("LAN proxy authentication = %v", got)
	}
	if got := skipAuthPrefixes(settings); !reflect.DeepEqual(got, []string{"127.0.0.1/8"}) {
		t.Fatalf("local clients must skip auth: %v", got)
	}
	settings.ProxyUsername, settings.ProxyPassword = "", ""
	if got := proxyAuthentication(settings); got != nil {
		t.Fatalf("auth without credentials: %v", got)
	}
	if got := skipAuthPrefixes(settings); got != nil {
		t.Fatalf("skip-auth without auth: %v", got)
	}
}

func TestGenerateRejectsHalfConfiguredLANAuth(t *testing.T) {
	oldLookup, oldLinkDNS := lookupIPAddr, linkDNSServers
	lookupIPAddr = func(ctx context.Context, host string) ([]net.IPAddr, error) { return nil, nil }
	linkDNSServers = func() []string { return nil }
	t.Cleanup(func() { lookupIPAddr, linkDNSServers = oldLookup, oldLinkDNS })

	profile := ovpn.Profile{Remotes: []ovpn.Remote{{Host: "203.0.113.7", Port: 1194}}, Proto: "udp"}
	settings := config.DefaultSettings()
	settings.ProxyPassword = "secret"
	output := filepath.Join(t.TempDir(), "config.yaml")
	if err := Generate(&profile, settings, output); err != nil {
		t.Fatalf("auth settings must not matter without LAN access: %v", err)
	}
	settings.AllowLAN = true
	if err := Generate(&profile, settings, output); err == nil {
		t.Fatal("LAN proxy with a password but no username must be rejected")
	}
}

func TestBindAddress(t *testing.T) {
	if got := bindAddress(false); got != "127.0.0.1" {
		t.Fatalf("local bind address = %q", got)
	}
	if got := bindAddress(true); got != "0.0.0.0" {
		t.Fatalf("LAN bind address = %q", got)
	}
}

func TestLinuxTUNKeepsLinkLocalOutOfTunnel(t *testing.T) {
	remotes := []ovpn.Remote{{Host: "203.0.113.7", Port: 1194}}
	if got := tunRouteExcludes("linux", remotes); !reflect.DeepEqual(got, []string{"203.0.113.7/32", "169.254.0.0/16"}) {
		t.Fatalf("linux TUN route excludes = %v", got)
	}
	if got := tunRouteExcludes("darwin", remotes); !reflect.DeepEqual(got, []string{"203.0.113.7/32"}) {
		t.Fatalf("darwin TUN route excludes = %v", got)
	}
}

func TestLinuxTUNUsesStableInterfaceName(t *testing.T) {
	settings := config.DefaultSettings()
	linux := tunConfig("linux", settings)
	if linux["device"] != "tizi" {
		t.Fatalf("linux TUN device = %v", linux["device"])
	}
	darwin := tunConfig("darwin", settings)
	if _, exists := darwin["device"]; exists {
		t.Fatalf("darwin TUN unexpectedly pins device: %#v", darwin)
	}
}
