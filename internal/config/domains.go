package config

var (
	// ExternalManagedResolverDomains 是 macOS 上额外交给 Mihomo DNS 解析的
	// 外网域名族（写进 /etc/resolver），避免系统绕过 Mihomo 拿到污染结果。
	ExternalManagedResolverDomains = []string{
		"google.com",
		"googleapis.com",
		"gstatic.com",
		"youtube.com",
		"googlevideo.com",
		"ytimg.com",
		"twimg.com",
		"ggpht.com",
		"youtu.be",
		"googleusercontent.com",
		"openai.com",
		"chatgpt.com",
		"oaistatic.com",
		"oaiusercontent.com",
		"anthropic.com",
		"claudeusercontent.com",
		"claude.ai",
		"claude.com",
		"github.com",
		"githubusercontent.com",
	}
	// SnifferSkipDomains 是嗅探到后不得改写目标地址的域名族。
	//
	// 微信用自家 HTTPDNS 挑选服务器后按 IP 直连，SNI/Host 只是占位
	// （如 c2c.cdn.weixin.qq.com、sni.cdn.weixin.qq.com）。sniffer 的
	// override-destination 会清空原 IP、按域名重新解析，把图片 CDN 请求
	// 送到朋友圈视频等其他集群，表现为部分图片反复重试、一直是占位图。
	// 跳过后连接保留微信选定的 IP，按端口与 GEOIP 规则直连（大陆微信的
	// 服务器 IP 都判为 CN）。
	//
	// 注意：被跳过的连接不再匹配域名规则，因此不要加入 wechat.com 这类
	// 服务器在境外的域名，否则会落到 MATCH 走外网代理。
	// CloudInternalDomains 是云主机厂商的内网域名族，只有主机所在网络下发的
	// DNS 能解析，公共 DNS 返回空结果。
	//
	// 腾讯云的软件源、NTP 与 metadata（mirrors、ntpupdate、metadata
	// .tencentyun.com）都解析到 169.254.0.0/16。服务器连上 VPN 后所有 DNS
	// 都交给 Mihomo，它只问公共 DNS，apt 和时间同步就会失败。这些域名固定
	// 直连，Ubuntu 上还交给默认网卡原来的 DNS 解析。
	CloudInternalDomains = []string{
		"tencentyun.com",
	}
	SnifferSkipDomains = []string{
		"weixin.qq.com",
		"weixin.qq.com.cn",
	}
	// PinnedProxyDomains 是固定走「外网代理」的公共域名族：流媒体与生成式
	// 媒体。Claude 的域名另见 ClaudeDomains。
	//
	// 这些域名在大陆是 DNS 污染与丢包最严重的一类，如果路由结果依赖
	// GEOIP 判定，解析一旦返回被判为 CN 的地址（或客户端拿到污染地址后
	// 按 IP 连接），媒体流就会掉到 DIRECT 上并连接失败，表现为列表和封面
	// 正常、视频与大文件永远打不开。纯域名规则不依赖解析，路由结果不会
	// 随 DNS 漂移，因此把它们内置到规则里。
	//
	// 注意：usercontent.google.com 与 googleusercontent.com 是两个不同的
	// 域名，前者承载 Gemini/Bard 的生成媒体，不能只写后者。
	PinnedProxyDomains = []string{
		"googlevideo.com",
		"googleusercontent.com",
		"usercontent.google.com",
		"ytimg.com",
		"ggpht.com",
		"youtube.com",
		"youtu.be",
		"generativelanguage.googleapis.com",
		"gemini.gstatic.com",
	}
	// ClaudeDomains 固定走「Claude」策略组：配置了住宅节点时经住宅节点，
	// 否则经订阅或节点文件里的代理节点。
	//
	// 与 PinnedProxyDomains 的区别是不允许直连：Claude 会按来源 IP 做风控，
	// 一次直连就会暴露国内 IP。所以「Claude」组里没有 DIRECT，规则排在
	// 自定义规则之前（custom-rules.txt 不能把它们改成直连），而且不依赖
	// 「外网代理」组（那个组可以手动切到 DIRECT）。
	ClaudeDomains = []string{
		"claude.ai",
		"claude.com",
		"anthropic.com",
		"claudeusercontent.com",
	}
)
