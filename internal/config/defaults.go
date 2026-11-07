package config

// 生成配置里的代理组和节点名，generator、manager、diagnostics、界面共用。
const (
	// IntranetGroup 是内网（OpenVPN）策略组，只有导入 .ovpn 后才生成。
	IntranetGroup = "内网"
	// ProxyGroup 是访问外网用的策略组：订阅、节点文件、内网 HTTP 出口或
	// Tailscale 出口节点，最后是 DIRECT。
	ProxyGroup = "外网代理"
	// DefaultGroup 是兜底规则使用的策略组。
	DefaultGroup = "默认出口"
	// AutoGroup 是按延迟自动选择订阅和节点文件里节点的策略组。
	AutoGroup = "自动选择"
	// ChainGroup 是住宅节点链式连接时的前置代理组，没有 DIRECT 选项。
	ChainGroup = "前置代理"
	// TailscaleGroup 是 tailnet 设备（100.64.0.0/10、*.ts.net）使用的策略组。
	TailscaleGroup = "Tailscale"

	// IntranetHTTPProxyName 是内网 HTTP 出口在生成配置里的节点名。
	IntranetHTTPProxyName = "intranet-http"
	// TailscaleProxyName 是内置 Tailscale 节点的名字，也用作 tailscale://
	// DNS 的地址。
	TailscaleProxyName = "tailscale"
	// TailscaleStateDir 是 Tailscale 登录状态目录，相对于配置目录。
	TailscaleStateDir = "tailscale"
	// TailscaleCIDR 是 tailnet 设备使用的 CGNAT 地址段。
	TailscaleCIDR = "100.64.0.0/10"
	// TailscaleDomain 是 MagicDNS 的域名后缀。
	TailscaleDomain = "ts.net"

	// SubscriptionProvider 和 NodesProvider 是订阅与节点文件对应的
	// proxy-provider 名。
	SubscriptionProvider = "subscription"
	NodesProvider        = "nodes"
	// SubscriptionCache 是 Mihomo 下载订阅后保存的位置，相对于配置目录。
	SubscriptionCache = "providers/subscription.yaml"
	// ProxiesFile 是配置目录下的节点文件名（Clash 节点 YAML 或分享链接）。
	ProxiesFile = "proxies.yaml"

	// ClaudeGroup 是 Claude 相关域名使用的策略组，没有 DIRECT 选项。
	ClaudeGroup = "Claude"
	// ResidentialProxyName 是 Claude 专用住宅节点在生成配置里的名字。
	ResidentialProxyName = "住宅节点"
	// ClaudeProxyFile 是配置目录下写成 Mihomo 节点格式的住宅节点文件名。
	ClaudeProxyFile = "claude-proxy.yaml"

	// HealthCheckURL 是自动选择和订阅节点测速使用的地址。
	HealthCheckURL = "https://www.gstatic.com/generate_204"

	// InfoNodePattern 匹配机场写在订阅里的说明条目（剩余流量、到期时间、
	// 官网地址等），它们不是能用的节点。自动选择排除它们，测速也跳过。
	// Go 和 Mihomo（.NET 风格正则）都能解析这个写法。
	InfoNodePattern = "剩余|到期|过期|有效期|重置|套餐|官网|网址|(?i:expire|traffic)"
)

// DefaultIntranetCIDRs 是导入了 .ovpn、但设置和 .ovpn 里都没有内网网段时
// 使用的网段。返回新切片，调用方可以安全修改。
func DefaultIntranetCIDRs() []string {
	return []string{"10.0.0.0/8"}
}

// PublicDNSUpstreams 返回国内公共 DNS，用作默认上游和恢复系统 DNS 的回退。
// 返回新切片，调用方可以安全修改。
func PublicDNSUpstreams() []string {
	return []string{"223.5.5.5", "114.114.114.114"}
}
