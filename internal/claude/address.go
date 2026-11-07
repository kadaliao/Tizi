package claude

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"unicode"

	"github.com/kadaliao/tizi/internal/config"
)

// Address 是设置里住宅节点地址的各个部分。桌面版按项填写，不用手写协议
// 前缀，也不用自己转义密码里的 @、: 等字符。
type Address struct {
	// Scheme 是 socks5、http 或 https。
	Scheme   string
	Host     string
	Port     int
	Username string
	Password string
}

// SplitURL 把设置里的地址拆成各个部分；socks5h 当作 socks5。
func SplitURL(raw string) (Address, error) {
	if _, err := ParseURL(raw); err != nil {
		return Address{}, err
	}
	parsed, _ := url.Parse(strings.TrimSpace(raw))
	scheme := strings.ToLower(parsed.Scheme)
	if scheme == "socks5h" {
		scheme = "socks5"
	}
	port, _ := strconv.Atoi(parsed.Port())
	address := Address{Scheme: scheme, Host: parsed.Hostname(), Port: port}
	if parsed.User != nil {
		address.Username = parsed.User.Username()
		address.Password, _ = parsed.User.Password()
	}
	return address, nil
}

// URL 把各个部分拼成设置里保存的地址。
func (a Address) URL() (string, error) {
	scheme := strings.ToLower(strings.TrimSpace(a.Scheme))
	if scheme != "socks5" && scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("住宅节点协议只支持 SOCKS5、HTTP 和 HTTPS；其他协议请写进 %s", config.ClaudeProxyFile)
	}
	host := trimBrackets(strings.TrimSpace(a.Host))
	if host == "" {
		return "", fmt.Errorf("请填写住宅节点的主机")
	}
	if strings.ContainsAny(host, "/@?#[]") || strings.IndexFunc(host, unicode.IsSpace) >= 0 ||
		(strings.Contains(host, ":") && net.ParseIP(host) == nil) {
		return "", fmt.Errorf("主机只填域名或 IP，不要带协议、端口或用户名")
	}
	if a.Port < 1 || a.Port > 65535 {
		return "", fmt.Errorf("住宅节点端口必须是 1 到 65535 之间的整数")
	}
	username := strings.TrimSpace(a.Username)
	if a.Password != "" && username == "" {
		return "", fmt.Errorf("填了密码时也要填用户名")
	}
	address := url.URL{Scheme: scheme, Host: net.JoinHostPort(host, strconv.Itoa(a.Port))}
	switch {
	case a.Password != "":
		address.User = url.UserPassword(username, a.Password)
	case username != "":
		address.User = url.User(username)
	}
	raw := address.String()
	if _, err := ParseURL(raw); err != nil {
		return "", err
	}
	return raw, nil
}

// ErrUnknownFormat 表示认不出粘贴的地址是哪种写法。
var ErrUnknownFormat = errors.New("认不出这个地址的格式；请分别填写主机、端口、用户名和密码")

// ParseAny 识别服务商给出的各种住宅节点写法，拆成各项：
//
//	socks5://用户名:密码@主机:端口（协议可以省略，也可以是 http、https、socks5h）
//	主机:端口@用户名:密码
//	主机:端口:用户名:密码
//	用户名:密码:主机:端口
//	主机:端口
//	主机
//
// 用户名和密码在前还是在后，看哪一段像「主机:端口」：IP 或带点的域名，
// 后面跟 1–65535 的端口；两种都像时，选主机是 IP 的那种，再不行按最常见
// 的写法。密码里可以有冒号。没写协议时 Scheme 为空，没写端口时 Port 为 0，
// 由调用方补上。写了协议（URL 写法）时，用户名和密码按 URL 转义解码。
func ParseAny(text string) (Address, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return Address{}, ErrUnknownFormat
	}
	scheme := ""
	if prefix, rest, ok := strings.Cut(text, "://"); ok {
		scheme = strings.ToLower(prefix)
		if scheme == "socks5h" {
			scheme = "socks5"
		}
		if scheme != "socks5" && scheme != "http" && scheme != "https" {
			return Address{}, fmt.Errorf("住宅节点协议只支持 SOCKS5、HTTP 和 HTTPS；其他协议请写进 %s", config.ClaudeProxyFile)
		}
		text = strings.TrimSuffix(rest, "/")
	}
	if host := trimBrackets(text); isHost(host) {
		return Address{Scheme: scheme, Host: host}, nil
	}
	address, ok := splitLoose(text)
	if !ok {
		return Address{}, ErrUnknownFormat
	}
	address.Scheme = scheme
	if scheme != "" {
		address.Username = unescape(address.Username)
		address.Password = unescape(address.Password)
	}
	return address, nil
}

// endpoint 是认出来的「主机:端口」。
type endpoint struct {
	host string
	port int
	ip   bool
}

func splitLoose(text string) (Address, bool) {
	if strings.Contains(text, "@") {
		// 用户名:密码@主机:端口，主机在最后一个 @ 之后（密码里可能有 @）。
		last := strings.LastIndex(text, "@")
		credsFirst, okFirst := parseEndpoint(splitColons(text[last+1:]))
		// 主机:端口@用户名:密码，主机在第一个 @ 之前。
		first := strings.Index(text, "@")
		hostFirst, okHost := parseEndpoint(splitColons(text[:first]))
		if pickFirst(credsFirst, okFirst, hostFirst, okHost) {
			username, password, _ := strings.Cut(text[:last], ":")
			return address(credsFirst, username, password), true
		}
		if okHost {
			username, password, _ := strings.Cut(text[first+1:], ":")
			return address(hostFirst, username, password), true
		}
		return Address{}, false
	}
	parts := splitColons(text)
	n := len(parts)
	switch {
	case n == 2:
		if found, ok := parseEndpoint(parts); ok {
			return address(found, "", ""), true
		}
	case n == 3:
		if found, ok := parseEndpoint(parts[:2]); ok {
			return address(found, parts[2], ""), true
		}
	case n >= 4:
		// 主机:端口:用户名:密码 比 用户名:密码:主机:端口 常见，两种都像时选它。
		hostFirst, okHost := parseEndpoint(parts[:2])
		credsFirst, okCreds := parseEndpoint(parts[n-2:])
		if pickFirst(hostFirst, okHost, credsFirst, okCreds) {
			return address(hostFirst, parts[2], strings.Join(parts[3:], ":")), true
		}
		if okCreds {
			return address(credsFirst, parts[0], strings.Join(parts[1:n-2], ":")), true
		}
	}
	return Address{}, false
}

// pickFirst 在两种拆法里选一种：只有一种像主机:端口时选它；都像时选主机
// 是 IP 的；还分不出来时选第一种。
func pickFirst(first endpoint, okFirst bool, second endpoint, okSecond bool) bool {
	if !okFirst || !okSecond {
		return okFirst
	}
	return first.ip || !second.ip
}

func address(found endpoint, username, password string) Address {
	return Address{Host: found.host, Port: found.port, Username: username, Password: password}
}

func parseEndpoint(parts []string) (endpoint, bool) {
	if len(parts) != 2 {
		return endpoint{}, false
	}
	host := trimBrackets(strings.TrimSpace(parts[0]))
	text := strings.TrimSpace(parts[1])
	port, err := strconv.Atoi(text)
	if err != nil || port < 1 || port > 65535 || strings.TrimLeft(text, "0123456789") != "" || !isHost(host) {
		return endpoint{}, false
	}
	return endpoint{host: host, port: port, ip: net.ParseIP(host) != nil}, true
}

// isHost 判断是不是 IP 或带点的域名。
func isHost(host string) bool {
	if net.ParseIP(host) != nil {
		return true
	}
	if len(host) > 253 || !strings.Contains(host, ".") {
		return false
	}
	labels := strings.Split(host, ".")
	for _, label := range labels {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if !(r == '-' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
				return false
			}
		}
	}
	// 顶级域名不是纯数字，免得把 1.2.3 这样的东西当成域名。
	return strings.IndexFunc(labels[len(labels)-1], unicode.IsLetter) >= 0
}

// splitColons 按冒号拆分，方括号里的 IPv6 地址不拆。
func splitColons(text string) []string {
	var parts []string
	depth, start := 0, 0
	for i, r := range text {
		switch r {
		case '[':
			depth++
		case ']':
			if depth > 0 {
				depth--
			}
		case ':':
			if depth == 0 {
				parts = append(parts, text[start:i])
				start = i + 1
			}
		}
	}
	return append(parts, text[start:])
}

func trimBrackets(host string) string {
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		return host[1 : len(host)-1]
	}
	return host
}

func unescape(value string) string {
	if decoded, err := url.PathUnescape(value); err == nil {
		return decoded
	}
	return value
}
