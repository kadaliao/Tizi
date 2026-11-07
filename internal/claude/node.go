// Package claude 管理 Claude 专用的住宅节点：从设置里的地址或
// claude-proxy.yaml 读出节点，转换成 Mihomo 代理定义，并决定连接方式
// （经「前置代理」链式连接，或本机直接连接）。
package claude

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kadaliao/tizi/internal/config"
	"github.com/kadaliao/tizi/internal/nodes"
	"gopkg.in/yaml.v3"
)

// Node 是一个可以写进 Mihomo 配置的住宅节点。
type Node struct {
	// Proxy 是 Mihomo 代理定义，name 和 dialer-proxy 已按设置填好。
	Proxy map[string]any
	// Source 说明节点来自哪里，用于界面和报错。
	Source string
	// FromFile 表示节点写在 claude-proxy.yaml 里，而不是设置里的地址。
	FromFile bool
	// Direct 为 true 时本机直接连接节点，否则经「前置代理」链式连接。
	Direct bool
}

// Running 把运行中配置里的住宅节点定义包装成 Node，用来判断设置的改动
// 有没有生效，以及按实际生效的连接方式给出提示。proxy 为 nil 时返回 nil。
func Running(proxy map[string]any) *Node {
	if proxy == nil {
		return nil
	}
	_, chained := proxy["dialer-proxy"]
	return &Node{Proxy: proxy, Source: "运行中的配置", Direct: !chained}
}

// Same 报告两个节点生成的 Mihomo 定义是否相同。
func (n Node) Same(other Node) bool {
	left, err := yaml.Marshal(n.Proxy)
	if err != nil {
		return false
	}
	right, err := yaml.Marshal(other.Proxy)
	return err == nil && bytes.Equal(left, right)
}

// Describe 用一句话说明节点的地址和连接方式，不含密码。
func (n Node) Describe() string {
	mode := "经前置代理链式连接"
	if n.Direct {
		mode = "本机直接连接"
	}
	return fmt.Sprintf("%s %s", mode, net.JoinHostPort(n.Server(), strconv.Itoa(n.Port())))
}

// IsLocal 判断节点地址是不是本机（127.0.0.0/8、::1、localhost），例如 SSH
// 端口转发出来的节点。本机节点只能直接连接：链式连接时前置代理连的是它
// 自己的本机地址，只会被拒绝。
func IsLocal(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(trimBrackets(host))
	return ip != nil && ip.IsLoopback()
}

// Type 返回节点的 Mihomo 协议类型。
func (n Node) Type() string {
	value, _ := n.Proxy["type"].(string)
	return value
}

// Port 返回节点端口；读不出时返回 0。
func (n Node) Port() int {
	return portValue(n.Proxy["port"])
}

// Server 返回节点地址。
func (n Node) Server() string {
	value, _ := n.Proxy["server"].(string)
	return value
}

// Hint 给出这个节点已知的可用性问题，没有问题时返回空。连接失败时附在
// 告警里，帮助判断是节点问题还是网络问题。
func (n Node) Hint() string {
	// 本机节点的流量不出本机，加密由转发它的隧道负责。
	if n.Direct && plaintextTypes[n.Type()] && n.Proxy["tls"] != true && !IsLocal(n.Server()) {
		return "从国内直接连接明文的 HTTP/SOCKS5 节点容易被干扰；可以改为经前置代理链式连接，或使用加密协议的节点。"
	}
	return ""
}

var plaintextTypes = map[string]bool{"http": true, "socks5": true}

// unusableTypes 不是能把流量送到住宅 IP 的代理。
var unusableTypes = map[string]bool{
	"direct":      true,
	"reject":      true,
	"reject-drop": true,
	"pass":        true,
	"dns":         true,
	"compatible":  true,
}

// DomainOf 返回 host 所属的 Claude 域名族（见 config.ClaudeDomains）；
// 不属于时返回空。
func DomainOf(host string) string {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	for _, suffix := range config.ClaudeDomains {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return suffix
		}
	}
	return ""
}

// Configured 报告是否设置了住宅节点（地址或节点文件），不校验内容。
func Configured(configDir string, settings config.Settings) bool {
	if strings.TrimSpace(settings.ClaudeProxy) != "" {
		return true
	}
	proxy, err := readNodeFile(filepath.Join(configDir, config.ClaudeProxyFile))
	return err != nil || proxy != nil
}

// Load 读取住宅节点。没有设置时返回 nil, nil。设置里的地址和节点文件只能
// 二选一，避免不知道哪个在生效。
func Load(configDir string, settings config.Settings) (*Node, error) {
	return LoadWith(configDir, settings, Chainable(configDir, settings))
}

// Chainable 报告有没有可以做前置代理的节点：订阅、节点文件、内网 HTTP
// 出口或 Tailscale 出口节点。
func Chainable(configDir string, settings config.Settings) bool {
	if settings.Subscription != "" || (settings.OVPNPath != "" && settings.IntranetHTTPProxy != "") ||
		(settings.TailscaleEnabled && settings.TailscaleExitNode != "") {
		return true
	}
	file, err := nodes.Load(filepath.Join(configDir, config.ProxiesFile), nil)
	return err == nil && file.Count() > 0
}

// LoadWith 与 Load 相同，但由调用方说明有没有前置代理；没有时住宅节点
// 总是直接连接。
func LoadWith(configDir string, settings config.Settings, chainable bool) (*Node, error) {
	file := filepath.Join(configDir, config.ClaudeProxyFile)
	fromFile, err := readNodeFile(file)
	if err != nil {
		return nil, err
	}
	address := strings.TrimSpace(settings.ClaudeProxy)
	var node Node
	switch {
	case address != "" && fromFile != nil:
		return nil, fmt.Errorf("同时设置了住宅节点地址和 %s，请只保留一个", file)
	case address != "":
		proxy, err := ParseURL(address)
		if err != nil {
			return nil, err
		}
		node = Node{Proxy: proxy, Source: "住宅节点地址"}
	case fromFile != nil:
		node = Node{Proxy: fromFile, Source: file, FromFile: true}
	default:
		return nil, nil
	}
	node.Direct = settings.ClaudeProxyDirect || IsLocal(node.Server()) || !chainable
	node.Proxy["name"] = config.ResidentialProxyName
	delete(node.Proxy, "dialer-proxy")
	if !node.Direct {
		// 引用「前置代理」而不是「外网代理」：后者可以手动切到 DIRECT，
		// 那样链式连接就悄悄变成了直连。
		node.Proxy["dialer-proxy"] = config.ChainGroup
	}
	return &node, nil
}

// ParseURL 把 http://、https:// 或 socks5:// 地址转换成 Mihomo 代理定义。
func ParseURL(raw string) (map[string]any, error) {
	raw = strings.TrimSpace(raw)
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("住宅节点地址无效，应类似 socks5://用户名:密码@1.2.3.4:1080")
	}
	proxy := map[string]any{}
	switch strings.ToLower(parsed.Scheme) {
	case "http":
		proxy["type"] = "http"
	case "https":
		proxy["type"] = "http"
		proxy["tls"] = true
	case "socks5", "socks5h":
		proxy["type"] = "socks5"
	default:
		return nil, fmt.Errorf("住宅节点地址只支持 http://、https:// 和 socks5://；其他协议请写进 %s", config.ClaudeProxyFile)
	}
	if (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("住宅节点地址不能包含路径或参数")
	}
	host := parsed.Hostname()
	port, err := strconv.Atoi(parsed.Port())
	if host == "" || err != nil || port < 1 || port > 65535 {
		return nil, fmt.Errorf("住宅节点地址需要包含主机和端口，例如 socks5://用户名:密码@1.2.3.4:1080")
	}
	proxy["server"] = host
	proxy["port"] = port
	if parsed.User != nil {
		if username := parsed.User.Username(); username != "" {
			proxy["username"] = username
		}
		if password, ok := parsed.User.Password(); ok && password != "" {
			proxy["password"] = password
		}
	}
	return proxy, nil
}

// MaskURL 隐去地址里的密码，用于在界面上显示。无法解析时只保留协议。
func MaskURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return "（地址无效）"
	}
	if parsed.User != nil {
		if _, ok := parsed.User.Password(); ok {
			parsed.User = url.UserPassword(parsed.User.Username(), "***")
		}
	}
	// url.String 会转义 *，这里手工替换回来，界面上更好认。
	return strings.Replace(parsed.String(), "%2A%2A%2A", "***", 1)
}

// readNodeFile 读取 claude-proxy.yaml。文件不存在、为空或只有注释时返回
// nil, nil。支持三种写法：单个节点；只含一个节点的列表（从 Clash 配置里
// 复制的 `- {name: ..., type: ...}`）；带 proxies: 的完整片段。
func readNodeFile(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取 %s: %w", path, err)
	}
	var value any
	if err := yaml.Unmarshal(data, &value); err != nil {
		return nil, fmt.Errorf("解析 %s: %w", path, err)
	}
	if value == nil {
		return nil, nil
	}
	if mapping, ok := value.(map[string]any); ok {
		if list, ok := mapping["proxies"]; ok && mapping["type"] == nil {
			value = list
		}
	}
	if list, ok := value.([]any); ok {
		if len(list) != 1 {
			return nil, fmt.Errorf("%s 只能包含一个节点，现在有 %d 个", path, len(list))
		}
		value = list[0]
	}
	proxy, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s 应是一个 Mihomo 节点（包含 type、server、port）", path)
	}
	kind, _ := proxy["type"].(string)
	kind = strings.ToLower(strings.TrimSpace(kind))
	server, _ := proxy["server"].(string)
	if kind == "" || strings.TrimSpace(server) == "" || portValue(proxy["port"]) == 0 {
		return nil, fmt.Errorf("%s 中的节点缺少 type、server 或 port", path)
	}
	if unusableTypes[kind] {
		return nil, fmt.Errorf("%s 中的节点类型 %s 不能作为住宅节点", path, kind)
	}
	proxy["type"] = kind
	return proxy, nil
}

func portValue(value any) int {
	var port int
	switch typed := value.(type) {
	case int:
		port = typed
	case int64:
		port = int(typed)
	case float64:
		port = int(typed)
	case string:
		port, _ = strconv.Atoi(strings.TrimSpace(typed))
	}
	if port < 1 || port > 65535 {
		return 0
	}
	return port
}

// FileTemplate 是首次从界面打开 claude-proxy.yaml 时写入的说明，全部是
// 注释，不会被当成节点。
const FileTemplate = `# Claude 专用住宅节点（Mihomo 节点格式）
#
# 只在节点不是 HTTP/SOCKS5 时使用本文件（例如 ss、trojan、vless、vmess）；
# HTTP/SOCKS5 节点直接在「配置」页填地址即可。两处只能设置一个。
#
# 去掉下面某一段的注释并改成你的节点，保存后重载 Mihomo 生效。name 和
# dialer-proxy 会被忽略：连接方式由「配置」页的「住宅节点连接方式」决定。
#
# type: trojan
# server: example.com
# port: 443
# password: your-password
# sni: example.com
#
# type: ss
# server: 203.0.113.10
# port: 443
# cipher: 2022-blake3-aes-128-gcm
# password: your-password
`
