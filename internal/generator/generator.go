package generator

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kadaliao/tizi/internal/claude"
	"github.com/kadaliao/tizi/internal/config"
	"github.com/kadaliao/tizi/internal/customrules"
	"github.com/kadaliao/tizi/internal/nodes"
	"github.com/kadaliao/tizi/internal/ovpn"
	"github.com/kadaliao/tizi/internal/system"
	"gopkg.in/yaml.v3"
)

const (
	tunnelMTU = 1400

	// openVPNHandshakeTimeout bounds a single OpenVPN handshake attempt.
	// macOS 睡眠唤醒后 UDP 会话可能已经失效，旧值 60s 会让一次失败的
	// 握手长时间阻塞恢复；缩短后能更快重试并切换到新的 remote。
	openVPNHandshakeTimeout = 15
)

type mihomoConfig struct {
	MixedPort          int              `yaml:"mixed-port"`
	AllowLAN           bool             `yaml:"allow-lan"`
	BindAddress        string           `yaml:"bind-address"`
	Authentication     []string         `yaml:"authentication,omitempty"`
	SkipAuthPrefixes   []string         `yaml:"skip-auth-prefixes,omitempty"`
	Mode               string           `yaml:"mode"`
	LogLevel           string           `yaml:"log-level"`
	IPv6               bool             `yaml:"ipv6"`
	ExternalController string           `yaml:"external-controller"`
	Secret             string           `yaml:"secret"`
	ExternalUI         string           `yaml:"external-ui"`
	ExternalUIURL      string           `yaml:"external-ui-url"`
	Tun                map[string]any   `yaml:"tun"`
	DNS                map[string]any   `yaml:"dns"`
	Sniffer            map[string]any   `yaml:"sniffer"`
	Proxies            []map[string]any `yaml:"proxies"`
	ProxyProviders     map[string]any   `yaml:"proxy-providers,omitempty"`
	ProxyGroups        []map[string]any `yaml:"proxy-groups"`
	Rules              []string         `yaml:"rules"`
}

var lookupIPAddr = func(ctx context.Context, host string) ([]net.IPAddr, error) {
	return net.DefaultResolver.LookupIPAddr(ctx, host)
}

// linkDNSServers 返回 Ubuntu 默认路由网卡上原有的 DNS（通常由 DHCP 下发）。
// 工具只改 TUN 网卡的 DNS，物理网卡的设置在连接期间保持不变，所以重载时
// 读到的仍是原值。
var linkDNSServers = func() []string {
	if runtime.GOOS != "linux" {
		return nil
	}
	state, err := system.ActiveDNS()
	if err != nil {
		return nil
	}
	return state.Servers
}

// Intranet 是实际生效的内网设置：设置里填了就用设置，否则用 .ovpn 里的
// route、dhcp-option。*Auto 表示该项来自 .ovpn 或内置默认值。
type Intranet struct {
	Domains     []string
	CIDRs       []string
	DNS         []string
	DomainsAuto bool
	CIDRsAuto   bool
	DNSAuto     bool
}

// EffectiveIntranet 返回实际生效的内网设置；profile 为 nil（没有导入
// .ovpn）时返回空值。
func EffectiveIntranet(profile *ovpn.Profile, settings config.Settings) Intranet {
	if profile == nil {
		return Intranet{}
	}
	result := Intranet{
		Domains: config.NormalizeDomains(settings.IntranetDomains),
		CIDRs:   append([]string(nil), settings.IntranetCIDRs...),
		DNS:     append([]string(nil), settings.IntranetDNS...),
	}
	if len(result.Domains) == 0 {
		result.Domains = config.NormalizeDomains(profile.Domains)
		result.DomainsAuto = true
	}
	if len(result.CIDRs) == 0 {
		result.CIDRs = append([]string(nil), profile.Routes...)
		if len(result.CIDRs) == 0 {
			result.CIDRs = config.DefaultIntranetCIDRs()
		}
		result.CIDRsAuto = true
	}
	if len(result.DNS) == 0 {
		result.DNS = append([]string(nil), profile.DNS...)
		result.DNSAuto = true
	}
	return result
}

// ResolverDomains 返回连接期间交给 Mihomo DNS 解析的私有域名后缀：内网域名
// 和 Tailscale 的 MagicDNS。
func ResolverDomains(profile *ovpn.Profile, settings config.Settings) []string {
	domains := append([]string(nil), EffectiveIntranet(profile, settings).Domains...)
	if settings.TailscaleEnabled {
		domains = append(domains, config.TailscaleDomain)
	}
	return config.NormalizeDomains(domains)
}

// Generate 生成本机运行的 Mihomo 配置。profile 为 nil 表示没有导入 .ovpn。
func Generate(profile *ovpn.Profile, settings config.Settings, output string) error {
	return generate(profile, settings, output, false)
}

// GenerateShare 生成给手机导入的配置：去掉内置 Tailscale 节点和只能在本机
// 读取的分享链接节点文件，其余与本机一致。
func GenerateShare(profile *ovpn.Profile, settings config.Settings, output string) error {
	return generate(profile, settings, output, true)
}

// plan 汇总生成规则和策略组需要的信息。
type plan struct {
	intranet    bool
	intranetNet Intranet
	tailscale   bool
	chinaDirect bool
	custom      []string
}

func generate(profile *ovpn.Profile, settings config.Settings, output string, share bool) error {
	if settings.AllowLAN {
		// 只设了一半的认证会让局域网代理不设防，生成前就拦下。
		if err := config.ValidateProxyCredentials(settings.ProxyUsername, settings.ProxyPassword); err != nil {
			return err
		}
	}
	if err := config.ValidateSubscription(settings.Subscription); err != nil {
		return err
	}
	if err := config.ValidateIntranet(settings); err != nil {
		return err
	}
	if err := config.ValidateTailscale(settings); err != nil {
		return err
	}
	configDir := filepath.Dir(output)
	tailscale := settings.TailscaleEnabled && !share

	var proxies []map[string]any
	var openVPNNames []string
	var remotes []ovpn.Remote
	intranet := EffectiveIntranet(profile, settings)
	if profile != nil {
		var err error
		remotes, err = profile.SelectRemotes(settings.Remote)
		if err != nil {
			return err
		}
		if len(remotes) == 0 {
			return fmt.Errorf("没有可用的 OpenVPN remote")
		}
		for i, remote := range remotes {
			name := fmt.Sprintf("openvpn-%d", i+1)
			openVPNNames = append(openVPNNames, name)
			proxies = append(proxies, openVPNProxy(name, *profile, remote, intranet.DNS))
		}
	}

	// front 是可以访问外网、又不是 DIRECT 的选项，按默认优先级排列。
	var front []string
	reserved := reservedNames(openVPNNames)
	nodeFile, err := nodes.Load(filepath.Join(configDir, config.ProxiesFile), reserved)
	if err != nil {
		return err
	}
	providers := map[string]any{}
	var use []string
	if settings.Subscription != "" {
		providers[config.SubscriptionProvider] = map[string]any{
			"type":         "http",
			"url":          settings.Subscription,
			"path":         "./" + config.SubscriptionCache,
			"interval":     86400,
			"proxy":        "DIRECT",
			"health-check": healthCheck(),
		}
		use = append(use, config.SubscriptionProvider)
	}
	if nodeFile.Links > 0 && !share {
		providers[config.NodesProvider] = map[string]any{
			"type":         "file",
			"path":         "./" + config.ProxiesFile,
			"health-check": healthCheck(),
		}
		use = append(use, config.NodesProvider)
	}
	proxies = append(proxies, nodeFile.Proxies...)
	nodeNames := nodeFile.Names()
	hasNodes := len(nodeNames) > 0 || len(use) > 0
	if hasNodes {
		front = append(front, config.AutoGroup)
		front = append(front, nodeNames...)
	}
	if profile != nil && settings.IntranetHTTPProxy != "" {
		host, port, _ := config.SplitHostPort(settings.IntranetHTTPProxy)
		proxies = append(proxies, map[string]any{
			"name":         config.IntranetHTTPProxyName,
			"type":         "http",
			"server":       host,
			"port":         port,
			"dialer-proxy": config.IntranetGroup,
			"ip-version":   "ipv4",
		})
		front = append(front, config.IntranetHTTPProxyName)
	}
	if tailscale {
		proxies = append(proxies, tailscaleProxy(settings))
		if settings.TailscaleExitNode != "" {
			front = append(front, config.TailscaleProxyName)
		}
	}

	residential, err := claude.LoadWith(configDir, settings, len(front) > 0)
	if err != nil {
		return err
	}
	if residential != nil {
		proxies = append(proxies, residential.Proxy)
	}

	custom, err := customRules(configDir)
	if err != nil {
		return err
	}
	custom, err = checkCustomTargets(custom, profile != nil, tailscale, share)
	if err != nil {
		return err
	}

	tun := tunConfig(runtime.GOOS, settings)
	if settings.TunEnabled {
		if excludes := tunRouteExcludes(runtime.GOOS, remotes); len(excludes) > 0 {
			tun["route-exclude-address"] = excludes
		}
	}
	var privateDomains []string
	nameserverPolicy := map[string]any{}
	if profile != nil {
		privateDomains = append(privateDomains, intranet.Domains...)
		if len(intranet.DNS) > 0 {
			servers := make([]string, 0, len(intranet.DNS))
			for _, server := range intranet.DNS {
				servers = append(servers, server+"#"+config.IntranetGroup)
			}
			for _, domain := range intranet.Domains {
				nameserverPolicy["+."+domain] = servers
			}
		}
	}
	if tailscale {
		privateDomains = append(privateDomains, config.TailscaleDomain)
		nameserverPolicy["+."+config.TailscaleDomain] = []string{"tailscale://" + config.TailscaleProxyName}
	}
	fakeIPFilter := make([]string, 0, len(privateDomains))
	for _, domain := range privateDomains {
		fakeIPFilter = append(fakeIPFilter, "+."+domain)
	}
	snifferSkip := make([]string, 0, len(config.SnifferSkipDomains))
	for _, domain := range config.SnifferSkipDomains {
		snifferSkip = append(snifferSkip, "+."+domain)
	}
	if servers := cloudNameservers(linkDNSServers()); len(servers) > 0 && !share {
		for _, domain := range config.CloudInternalDomains {
			nameserverPolicy["+."+domain] = servers
		}
	}

	groups := []map[string]any{}
	if profile != nil {
		groups = append(groups, map[string]any{"name": config.IntranetGroup, "type": "select", "proxies": openVPNNames})
	}
	if hasNodes {
		groups = append(groups, withUse(map[string]any{
			"name":      config.AutoGroup,
			"type":      "url-test",
			"proxies":   nodeNames,
			"url":       config.HealthCheckURL,
			"interval":  300,
			"tolerance": 50,
			"lazy":      true,
		}, use))
	}
	groups = append(groups,
		withUse(map[string]any{"name": config.ProxyGroup, "type": "select", "proxies": append(append([]string(nil), front...), "DIRECT")}, use),
		map[string]any{"name": config.DefaultGroup, "type": "select", "proxies": []string{config.ProxyGroup, "DIRECT"}},
	)
	if residential != nil && !residential.Direct {
		groups = append(groups, withUse(map[string]any{"name": config.ChainGroup, "type": "select", "proxies": front}, use))
	}
	groups = append(groups, withUse(map[string]any{"name": config.ClaudeGroup, "type": "select", "proxies": claudeChoices(residential, front, len(use) > 0)}, use))
	if tailscale {
		groups = append(groups, map[string]any{"name": config.TailscaleGroup, "type": "select", "proxies": []string{config.TailscaleProxyName}})
	}

	cfg := mihomoConfig{
		MixedPort:          settings.MixedPort,
		AllowLAN:           settings.AllowLAN,
		BindAddress:        bindAddress(settings.AllowLAN),
		Authentication:     proxyAuthentication(settings),
		SkipAuthPrefixes:   skipAuthPrefixes(settings),
		Mode:               "rule",
		LogLevel:           "info",
		IPv6:               false,
		ExternalController: fmt.Sprintf("127.0.0.1:%d", settings.ControllerPort),
		Secret:             settings.ControllerSecret,
		ExternalUI:         "ui",
		ExternalUIURL:      "https://github.com/MetaCubeX/metacubexd/archive/refs/heads/gh-pages.zip",
		Tun:                tun,
		DNS: map[string]any{
			"enable":                  true,
			"ipv6":                    false,
			"listen":                  fmt.Sprintf("127.0.0.1:%d", settings.DNSPort),
			"enhanced-mode":           "fake-ip",
			"fake-ip-range":           "198.18.0.1/16",
			"fake-ip-filter":          fakeIPFilter,
			"respect-rules":           true,
			"default-nameserver":      config.PublicDNSUpstreams(),
			"proxy-server-nameserver": config.PublicDNSUpstreams(),
			"nameserver":              config.PublicDNSUpstreams(),
			"fallback":                []string{"https://1.1.1.1/dns-query#" + config.ProxyGroup},
			"fallback-filter": map[string]any{
				"geoip":               true,
				"geoip-code":          "CN",
				"fallback-lazy-query": false,
			},
			"nameserver-policy": nameserverPolicy,
		},
		Sniffer: map[string]any{
			"enable":               true,
			"parse-pure-ip":        true,
			"override-destination": true,
			"skip-domain":          snifferSkip,
			"sniff": map[string]any{
				"HTTP": map[string]any{"ports": []any{80, "8080-8880"}},
				"TLS":  map[string]any{"ports": []int{443, 8443}},
			},
		},
		Proxies:        proxies,
		ProxyProviders: providers,
		ProxyGroups:    groups,
		Rules: rules(plan{
			intranet:    profile != nil,
			intranetNet: intranet,
			tailscale:   tailscale,
			chinaDirect: settings.ChinaDirect,
			custom:      custom,
		}),
	}
	if len(cfg.Proxies) == 0 {
		cfg.Proxies = []map[string]any{}
	}

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("生成 YAML: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(output), 0o700); err != nil {
		return fmt.Errorf("创建配置目录: %w", err)
	}
	if err := config.WriteFileAtomic(output, data, 0o600); err != nil {
		return fmt.Errorf("保存 YAML: %w", err)
	}
	return nil
}

func openVPNProxy(name string, profile ovpn.Profile, remote ovpn.Remote, dns []string) map[string]any {
	proto := profile.Proto
	if remote.Proto != "" {
		proto = remote.Proto
	}
	proxy := map[string]any{
		"name":              name,
		"type":              "openvpn",
		"server":            remote.Host,
		"port":              remote.Port,
		"proto":             proto,
		"dev":               profile.Dev,
		"cipher":            profile.Cipher,
		"auth":              profile.Auth,
		"comp-lzo":          profile.CompLZO,
		"udp":               true,
		"ping":              positiveOrDefault(profile.Ping, 10),
		"ping-restart":      positiveOrDefault(profile.PingRestart, 45),
		"handshake-timeout": openVPNHandshakeTimeout,
		"mtu":               tunnelMTU,
		"ca":                profile.CA,
	}
	if len(dns) > 0 {
		proxy["remote-dns-resolve"] = true
		proxy["dns"] = dns
	}
	if profile.Cert != "" {
		proxy["cert"] = profile.Cert
		proxy["key"] = profile.Key
	}
	if profile.TLSCrypt != "" {
		proxy["tls-crypt"] = profile.TLSCrypt
	}
	if profile.TLSCryptV2 != "" {
		proxy["tls-crypt-v2"] = profile.TLSCryptV2
	}
	if profile.TLSAuth != "" {
		proxy["tls-auth"] = profile.TLSAuth
		if profile.KeyDirection != "" {
			proxy["key-direction"] = profile.KeyDirection
		}
	}
	if profile.NeedsLogin {
		proxy["username"] = profile.Username
		proxy["password"] = profile.Password
	}
	return proxy
}

func tailscaleProxy(settings config.Settings) map[string]any {
	proxy := map[string]any{
		"name":          config.TailscaleProxyName,
		"type":          "tailscale",
		"hostname":      TailscaleHostname(settings),
		"state-dir":     config.TailscaleStateDir,
		"udp":           true,
		"accept-routes": true,
	}
	if settings.TailscaleAuthKey != "" {
		proxy["auth-key"] = settings.TailscaleAuthKey
	}
	if settings.TailscaleControlURL != "" {
		proxy["control-url"] = settings.TailscaleControlURL
	}
	if settings.TailscaleExitNode != "" {
		proxy["exit-node"] = settings.TailscaleExitNode
	}
	return proxy
}

// TailscaleHostname 返回加入 tailnet 时使用的设备名；没有设置时用
// tizi-<主机名>。
func TailscaleHostname(settings config.Settings) string {
	if settings.TailscaleHostname != "" {
		return settings.TailscaleHostname
	}
	host, _ := os.Hostname()
	host = strings.ToLower(strings.SplitN(host, ".", 2)[0])
	var builder strings.Builder
	for _, r := range host {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			builder.WriteRune(r)
		}
	}
	if cleaned := strings.Trim(builder.String(), "-"); cleaned != "" {
		return "tizi-" + cleaned
	}
	return "tizi"
}

func healthCheck() map[string]any {
	return map[string]any{"enable": true, "url": config.HealthCheckURL, "interval": 600, "lazy": true}
}

func withUse(group map[string]any, use []string) map[string]any {
	if len(use) > 0 {
		group["use"] = use
	}
	if proxies, ok := group["proxies"].([]string); ok && len(proxies) == 0 {
		delete(group, "proxies")
	}
	return group
}

// reservedNames 是节点文件里不能使用的名字：策略组、内置节点和 Mihomo
// 的特殊出口。
func reservedNames(openVPNNames []string) map[string]bool {
	reserved := map[string]bool{
		"DIRECT": true, "REJECT": true, "REJECT-DROP": true, "PASS": true, "COMPATIBLE": true, "GLOBAL": true,
		config.IntranetGroup: true, config.ProxyGroup: true, config.DefaultGroup: true, config.AutoGroup: true,
		config.ChainGroup: true, config.ClaudeGroup: true, config.TailscaleGroup: true,
		config.IntranetHTTPProxyName: true, config.TailscaleProxyName: true, config.ResidentialProxyName: true,
	}
	for _, name := range openVPNNames {
		reserved[name] = true
	}
	return reserved
}

// claudeChoices 是「Claude」组的选项。没有 DIRECT：住宅节点不可用时 Claude
// 打不开并弹窗提醒，而不是悄悄换成别的出口。一个代理节点都没有时只有
// REJECT，Claude 直接打不开，也不会直连。
func claudeChoices(residential *claude.Node, front []string, hasProviders bool) []string {
	choices := make([]string, 0, len(front)+1)
	if residential != nil {
		choices = append(choices, config.ResidentialProxyName)
	}
	choices = append(choices, front...)
	if len(choices) == 0 && !hasProviders {
		return []string{"REJECT"}
	}
	return choices
}

// checkCustomTargets 检查自定义规则用到的路线是否存在：没有导入 .ovpn 时
// 没有「内网」，没有开启 Tailscale 时没有「Tailscale」。给手机生成配置时
// 去掉走 Tailscale 的规则。
func checkCustomTargets(custom []string, intranet, tailscale, share bool) ([]string, error) {
	result := make([]string, 0, len(custom))
	for _, rule := range custom {
		target := customrules.RuleTarget(rule)
		switch {
		case target == config.IntranetGroup && !intranet:
			return nil, fmt.Errorf("自定义规则 %q 使用了「%s」，但还没有导入 .ovpn；请导入后再试，或修改这条规则", rule, config.IntranetGroup)
		case target == config.TailscaleGroup && !tailscale:
			if share {
				continue
			}
			return nil, fmt.Errorf("自定义规则 %q 使用了「%s」，但还没有开启 Tailscale；请开启后再试，或修改这条规则", rule, config.TailscaleGroup)
		}
		result = append(result, rule)
	}
	return result, nil
}

func tunConfig(goos string, settings config.Settings) map[string]any {
	tun := map[string]any{
		"enable":                settings.TunEnabled,
		"stack":                 "mixed",
		"mtu":                   tunnelMTU,
		"auto-route":            true,
		"auto-detect-interface": true,
		"dns-hijack":            []string{"any:53", "tcp://any:53"},
	}
	if goos == "linux" {
		tun["device"] = "tizi"
	}
	return tun
}

func bindAddress(allowLAN bool) string {
	if allowLAN {
		return "0.0.0.0"
	}
	return "127.0.0.1"
}

// proxyAuthentication 只在局域网代理开启且设置了用户名和密码时要求认证；
// 只监听 127.0.0.1 时认证没有意义。
func proxyAuthentication(settings config.Settings) []string {
	if !settings.AllowLAN || !settings.ProxyAuthEnabled() {
		return nil
	}
	return []string{settings.ProxyUsername + ":" + settings.ProxyPassword}
}

// skipAuthPrefixes 让本机程序免认证：规则页的路线测试和本机设置的
// http_proxy 都从回环地址连接代理端口。
func skipAuthPrefixes(settings config.Settings) []string {
	if proxyAuthentication(settings) == nil {
		return nil
	}
	return []string{"127.0.0.1/8"}
}

func positiveOrDefault(value, fallback int) int {
	if value > 0 {
		return value
	}
	return fallback
}

// tunRouteExcludes 返回不进入 TUN 的地址段。Linux 上还排除链路本地地址：
// Mihomo 拨号时只对全局单播地址绑定物理网卡，DIRECT 连向 169.254.0.0/16
// （云主机的 metadata、软件源）的 TCP 会按策略路由绕回 TUN，直到超时。
// macOS 的物理网卡自带更具体的 169.254 链路路由，不需要排除。
func tunRouteExcludes(goos string, remotes []ovpn.Remote) []string {
	excludes := routeExcludeAddresses(remotes)
	if goos == "linux" {
		excludes = append(excludes, "169.254.0.0/16")
	}
	return excludes
}

func routeExcludeAddresses(remotes []ovpn.Remote) []string {
	seen := make(map[string]struct{})
	var pending []string
	for _, remote := range remotes {
		host := strings.Trim(strings.TrimSpace(remote.Host), "[]")
		if host == "" {
			continue
		}
		if addr, err := netip.ParseAddr(host); err == nil {
			addRouteExclude(seen, addr)
			continue
		}
		pending = append(pending, host)
	}
	if len(pending) > 0 {
		results := make([][]net.IPAddr, len(pending))
		var wg sync.WaitGroup
		for i, host := range pending {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				results[i], _ = lookupIPAddr(ctx, host)
			}()
		}
		wg.Wait()
		for _, addrs := range results {
			for _, ipAddr := range addrs {
				if addr, ok := netip.AddrFromSlice(ipAddr.IP); ok {
					addRouteExclude(seen, addr)
				}
			}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	values := make([]string, 0, len(seen))
	for value := range seen {
		values = append(values, value)
	}
	sort.Strings(values)
	return values
}

// cloudNameservers 把网卡 DNS 转成 Mihomo 的直连上游。respect-rules 会让
// DNS 查询本身也按规则分流，关闭「国内直连」时会落到不支持 UDP 的 HTTP
// 代理，所以显式指定 #DIRECT。TUN 只接管 IPv4，也排除本机回环上的 stub。
func cloudNameservers(servers []string) []string {
	var result []string
	for _, server := range servers {
		addr, err := netip.ParseAddr(server)
		if err != nil || !addr.Is4() || addr.IsLoopback() || addr.IsUnspecified() {
			continue
		}
		result = append(result, addr.String()+"#DIRECT")
	}
	return result
}

func addRouteExclude(seen map[string]struct{}, addr netip.Addr) {
	addr = addr.Unmap()
	bits := 128
	if addr.Is4() {
		bits = 32
	}
	seen[netip.PrefixFrom(addr, bits).String()] = struct{}{}
}

// customRules 读取配置目录下 custom-rules.txt 的用户自定义分流规则。
// 每行一条 Mihomo 规则，# 开头为注释；文件不存在时返回空。规则在这里
// 先做校验并给出行号，避免 `mihomo -t` 只报整份配置无效。
func customRules(configDir string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(configDir, "custom-rules.txt"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取自定义规则: %w", err)
	}
	return customrules.ParseRules(string(data))
}

func rules(p plan) []string {
	chinaRule := "GEOIP,CN," + config.ProxyGroup
	if p.chinaDirect {
		chinaRule = "GEOIP,CN,DIRECT"
	}
	builtin := []string{
		"AND,((NETWORK,TCP),(DST-PORT,5223),(IP-CIDR,17.0.0.0/8)),DIRECT",
	}
	if p.intranet {
		for _, domain := range p.intranetNet.Domains {
			builtin = append(builtin, "DOMAIN-SUFFIX,"+domain+","+config.IntranetGroup)
		}
		for _, cidr := range p.intranetNet.CIDRs {
			builtin = append(builtin, "IP-CIDR,"+cidr+","+config.IntranetGroup+",no-resolve")
		}
	}
	if p.tailscale {
		builtin = append(builtin,
			"DOMAIN-SUFFIX,"+config.TailscaleDomain+","+config.TailscaleGroup,
			"IP-CIDR,"+config.TailscaleCIDR+","+config.TailscaleGroup+",no-resolve",
		)
	}
	builtin = append(builtin,
		// 微信图片依赖自家 HTTPDNS（dns.weixin.qq.com.cn:8080）取 CDN 地址。
		// GEOIP 规则要先解析域名，解析抖动时会掉进 MATCH 兜底代理导致 403；
		// 纯域名规则不依赖解析，保证腾讯系流量始终直连。
		"DOMAIN-SUFFIX,qq.com,DIRECT",
		"DOMAIN-SUFFIX,qq.com.cn,DIRECT",
		"DOMAIN-SUFFIX,qpic.cn,DIRECT",
		"DOMAIN-SUFFIX,qlogo.cn,DIRECT",
		"DOMAIN-SUFFIX,wechat.com,DIRECT",
		"DOMAIN-SUFFIX,tencent.com,DIRECT",
		// 飞书 API 与图片 CDN 必须保持同一出口；仅靠 GEOIP 会因 CDN
		// 调度结果变化而在 DIRECT 和默认代理之间漂移。头像等静态资源
		// 还会落在 larksuite.com 与字节系 byteimg.com CDN 上。
		"DOMAIN-SUFFIX,feishu.cn,DIRECT",
		"DOMAIN-SUFFIX,feishucdn.com,DIRECT",
		"DOMAIN-SUFFIX,larksuite.com,DIRECT",
		"DOMAIN-SUFFIX,byteimg.com,DIRECT",
	)
	for _, domain := range config.CloudInternalDomains {
		builtin = append(builtin, "DOMAIN-SUFFIX,"+domain+",DIRECT")
	}
	// 没有按端口放行直连的规则：http:// 和 https:// 按同样的规则分流，
	// 海外网站的 http 地址也走代理，否则多半打不开。
	tail := []string{
		"IP-CIDR,127.0.0.0/8,DIRECT,no-resolve",
		"IP-CIDR,10.0.0.0/8,DIRECT,no-resolve",
		"IP-CIDR,172.16.0.0/12,DIRECT,no-resolve",
		"IP-CIDR,192.168.0.0/16,DIRECT,no-resolve",
		"IP-CIDR,100.64.0.0/10,DIRECT,no-resolve",
		// 链路本地地址：云主机的 metadata、软件源和 NTP 多在这里。
		"IP-CIDR,169.254.0.0/16,DIRECT,no-resolve",
		"IP-CIDR,224.0.0.0/4,DIRECT,no-resolve",
		chinaRule,
		"AND,((NETWORK,UDP),(DST-PORT,443)),REJECT",
		"AND,((NETWORK,UDP),(DST-PORT,8443)),REJECT",
		"MATCH," + config.DefaultGroup,
	}
	// 固定域名族走外网代理，排在自定义规则之后：用户仍可用
	// custom-rules.txt 覆盖内置的分流。
	pinned := make([]string, 0, len(config.PinnedProxyDomains))
	for _, domain := range config.PinnedProxyDomains {
		pinned = append(pinned, "DOMAIN-SUFFIX,"+domain+","+config.ProxyGroup)
	}
	result := make([]string, 0, len(builtin)+len(claudeRules())+len(p.custom)+len(pinned)+len(tail))
	result = append(result, builtin...)
	result = append(result, claudeRules()...)
	result = append(result, p.custom...)
	result = append(result, pinned...)
	return append(result, tail...)
}

// claudeRules 把 Claude 域名固定到「Claude」组，排在自定义规则之前，
// custom-rules.txt 不能把它们改成直连。
//
// Mihomo 遇到出口不支持 UDP 的规则会跳过它继续往下匹配，全部跳过后落到
// DIRECT。HTTP 类出口不支持 UDP，所以 Claude 域名的 UDP 流量会绕过
// 「Claude」组直连（443 端口有后面的 QUIC 拦截规则兜底，其他端口没有）。
// 紧跟一条 UDP 拦截规则：出口支持 UDP 时前一条规则先匹配，不受影响。
func claudeRules() []string {
	result := make([]string, 0, 2*len(config.ClaudeDomains))
	for _, domain := range config.ClaudeDomains {
		result = append(result, "DOMAIN-SUFFIX,"+domain+","+config.ClaudeGroup)
	}
	for _, domain := range config.ClaudeDomains {
		result = append(result, "AND,((NETWORK,UDP),(DOMAIN-SUFFIX,"+domain+")),REJECT")
	}
	return result
}

// ErrNothingConfigured 表示既没有代理节点，也没有内网、Tailscale 或住宅节点。
var ErrNothingConfigured = errors.New("还没有可用的线路：请在「配置」中填写订阅地址或节点文件，或者导入内网 .ovpn、开启 Tailscale")

// CheckReady 检查是否至少配置了一种线路；什么都没有时连接没有意义。
func CheckReady(configDir string, settings config.Settings) error {
	if strings.TrimSpace(settings.OVPNPath) != "" || settings.TailscaleEnabled || claude.Chainable(configDir, settings) ||
		claude.Configured(configDir, settings) {
		return nil
	}
	return ErrNothingConfigured
}
