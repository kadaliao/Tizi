package config

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

// SplitList 把界面或命令行里用逗号、空格或换行分隔的一串值拆开，去掉空项。
func SplitList(text string) []string {
	return cleanList(strings.FieldsFunc(text, func(r rune) bool {
		return r == ',' || r == '，' || r == ';' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	}))
}

// JoinList 是 SplitList 的反向操作，用于在界面上显示。
func JoinList(values []string) string {
	return strings.Join(values, ", ")
}

func cleanList(values []string) []string {
	var result []string
	seen := map[string]bool{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}

// NormalizeDomains 把域名后缀统一成小写、去掉开头的 *. / +. / . 和结尾的点。
func NormalizeDomains(values []string) []string {
	normalized := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		for _, prefix := range []string{"*.", "+.", "."} {
			value = strings.TrimPrefix(value, prefix)
		}
		normalized = append(normalized, strings.TrimSuffix(value, "."))
	}
	return cleanList(normalized)
}

// ValidateIntranet 检查内网相关设置的格式。
func ValidateIntranet(settings Settings) error {
	for _, domain := range settings.IntranetDomains {
		if !validDomainSuffix(domain) {
			return fmt.Errorf("内网域名 %q 无效，应类似 corp.example.com", domain)
		}
	}
	for _, cidr := range settings.IntranetCIDRs {
		prefix, err := netip.ParsePrefix(cidr)
		if err != nil || !prefix.Addr().Is4() {
			return fmt.Errorf("内网网段 %q 无效，应类似 10.0.0.0/8", cidr)
		}
	}
	for _, server := range settings.IntranetDNS {
		if _, err := netip.ParseAddr(server); err != nil {
			return fmt.Errorf("内网 DNS %q 无效，应是 IP 地址", server)
		}
	}
	if settings.IntranetHTTPProxy != "" {
		if _, _, err := SplitHostPort(settings.IntranetHTTPProxy); err != nil {
			return fmt.Errorf("内网 HTTP 出口 %q 无效，应类似 10.1.2.3:8080", settings.IntranetHTTPProxy)
		}
	}
	if settings.IntranetProbeURL != "" {
		parsed, err := url.Parse(settings.IntranetProbeURL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return fmt.Errorf("内网检查地址 %q 无效，应类似 http://wiki.corp.example.com/", settings.IntranetProbeURL)
		}
	}
	return nil
}

// ValidateSubscription 检查订阅地址。
func ValidateSubscription(raw string) error {
	if raw == "" {
		return nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return fmt.Errorf("订阅地址无效，应以 https:// 或 http:// 开头")
	}
	return nil
}

// ValidateTailscale 检查 Tailscale 设置。
func ValidateTailscale(settings Settings) error {
	if settings.TailscaleControlURL != "" {
		parsed, err := url.Parse(settings.TailscaleControlURL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return fmt.Errorf("Tailscale 控制服务器地址无效，应类似 https://headscale.example.com")
		}
	}
	if strings.ContainsAny(settings.TailscaleHostname, " /\\:@") {
		return fmt.Errorf("Tailscale 设备名只能包含字母、数字和连字符")
	}
	if strings.ContainsAny(settings.TailscaleAuthKey, " \t\r\n") {
		return fmt.Errorf("Tailscale Auth Key 不能包含空白")
	}
	return nil
}

// MaskSubscription 只保留订阅地址的协议和主机，其余部分（通常含令牌）隐去。
func MaskSubscription(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return "（地址无效）"
	}
	if parsed.Path == "" && parsed.RawQuery == "" && parsed.User == nil {
		return parsed.Scheme + "://" + parsed.Host
	}
	return parsed.Scheme + "://" + parsed.Host + "/…"
}

// MaskSecret 只显示密钥的开头几位。
func MaskSecret(value string) string {
	if value == "" {
		return ""
	}
	if len(value) <= 12 {
		return "***"
	}
	return value[:8] + "…"
}

// SplitHostPort 拆分 host:port，端口必须在 1–65535 之间。
func SplitHostPort(value string) (string, int, error) {
	host, portText, err := net.SplitHostPort(strings.TrimSpace(value))
	if err != nil {
		return "", 0, err
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 || host == "" {
		return "", 0, fmt.Errorf("端口无效")
	}
	return host, port, nil
}

func validDomainSuffix(domain string) bool {
	if domain == "" || len(domain) > 253 || strings.ContainsAny(domain, "/\\:@ ") {
		return false
	}
	for _, label := range strings.Split(domain, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if !(r == '-' || r == '_' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z') {
				return false
			}
		}
	}
	return true
}
