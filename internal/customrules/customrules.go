// Package customrules stores user-managed domain routing rules.
package customrules

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kadaliao/tizi/internal/claude"
	"github.com/kadaliao/tizi/internal/config"
)

type Rule struct {
	Domain string
	Target string
}

const Template = `# 用户自定义 Mihomo 分流规则
#
# 每行一条规则，按从上到下的顺序匹配。空行和以 # 开头的注释会被忽略。
# 可使用的路线：外网代理、默认出口、Claude、DIRECT、REJECT，导入 .ovpn 后
# 还有「内网」，开启 Tailscale 后还有「Tailscale」。
# Claude 的域名（claude.ai、anthropic.com 等）固定走 Claude 路线，这里改不了。
# 取消下面示例前的 # 并按需修改；保存后重新生成配置并重启 Mihomo。
#
# DOMAIN-SUFFIX,example.com,外网代理
# DOMAIN,api.example.com,DIRECT
# DOMAIN-SUFFIX,corp.example.com,内网
# DOMAIN-SUFFIX,home.example.com,Tailscale
# IP-CIDR,203.0.113.0/24,DIRECT,no-resolve
`

// DomainTargets 是界面上保存域名规则时可选的路线，按常用程度排列；
// 不提供 REJECT，避免一次误操作把整个网站拦截。
// Claude 路线让其他域名也经 Claude 的出口（住宅节点或代理节点）。
// 「内网」和「Tailscale」只在导入 .ovpn、开启 Tailscale 后可用，见 Targets。
var DomainTargets = []string{config.IntranetGroup, config.ProxyGroup, config.DefaultGroup, config.ClaudeGroup, config.TailscaleGroup, "DIRECT"}

// Targets 返回当前设置下可用的域名规则路线。
func Targets(intranet, tailscale bool) []string {
	result := make([]string, 0, len(DomainTargets))
	for _, target := range DomainTargets {
		if (target == config.IntranetGroup && !intranet) || (target == config.TailscaleGroup && !tailscale) {
			continue
		}
		result = append(result, target)
	}
	return result
}

var allowedTargets = map[string]bool{
	config.IntranetGroup:  true,
	config.ProxyGroup:     true,
	config.DefaultGroup:   true,
	config.ClaudeGroup:    true,
	config.TailscaleGroup: true,
	"DIRECT":              true,
	"REJECT":              true,
}

// RuleTarget 返回一条已校验规则的路线（去掉 no-resolve 等选项）。
func RuleTarget(line string) string {
	fields := strings.Split(line, ",")
	for i := range fields {
		fields[i] = strings.TrimSpace(fields[i])
	}
	end := len(fields)
	for end > 2 && ruleOptions[fields[end-1]] {
		end--
	}
	return fields[end-1]
}

// ruleTypes 是允许写入 custom-rules.txt 的 Mihomo 规则类型；true 表示
// 逻辑规则（AND/OR/NOT），其载荷带括号，只校验最后一个字段的路线。
var ruleTypes = map[string]bool{
	"DOMAIN": false, "DOMAIN-SUFFIX": false, "DOMAIN-KEYWORD": false, "DOMAIN-REGEX": false,
	"GEOSITE": false, "GEOIP": false, "SRC-GEOIP": false,
	"IP-CIDR": false, "IP-CIDR6": false, "IP-SUFFIX": false, "IP-ASN": false,
	"SRC-IP-CIDR": false, "SRC-IP-SUFFIX": false, "SRC-IP-ASN": false,
	"DST-PORT": false, "SRC-PORT": false, "IN-PORT": false, "IN-TYPE": false, "IN-USER": false, "IN-NAME": false,
	"PROCESS-NAME": false, "PROCESS-PATH": false, "PROCESS-NAME-REGEX": false, "PROCESS-PATH-REGEX": false,
	"UID": false, "NETWORK": false, "DSCP": false, "RULE-SET": false,
	"AND": true, "OR": true, "NOT": true,
}

// ruleOptions 是可以跟在路线后面的规则选项。
var ruleOptions = map[string]bool{"no-resolve": true, "src": true}

// ParseRules 解析 custom-rules.txt 内容，返回去掉注释与空行后的规则；
// 任一行无效时返回带行号的错误，供 TUI 与生成器共用。
func ParseRules(content string) ([]string, error) {
	var rules []string
	for index, raw := range strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n") {
		line := stripComment(raw)
		if line == "" {
			continue
		}
		if err := validateRule(line); err != nil {
			return nil, fmt.Errorf("custom-rules.txt 第 %d 行: %w", index+1, err)
		}
		rules = append(rules, line)
	}
	return rules, nil
}

// stripComment 去掉整行注释与行尾 " # 说明" 形式的注释，并裁剪空白。
func stripComment(line string) string {
	line = strings.TrimSpace(line)
	if strings.HasPrefix(line, "#") {
		return ""
	}
	for i := 1; i < len(line); i++ {
		if line[i] == '#' && (line[i-1] == ' ' || line[i-1] == '\t') {
			return strings.TrimSpace(line[:i])
		}
	}
	return line
}

func validateRule(line string) error {
	fields := strings.Split(line, ",")
	for i := range fields {
		fields[i] = strings.TrimSpace(fields[i])
	}
	ruleType := strings.ToUpper(fields[0])
	if ruleType == "MATCH" {
		if len(fields) != 2 {
			return fmt.Errorf("MATCH 规则格式应为 MATCH,路线")
		}
		return validateTarget(fields[1])
	}
	logical, known := ruleTypes[ruleType]
	if !known {
		return fmt.Errorf("不支持的规则类型 %q", fields[0])
	}
	if len(fields) < 3 {
		return fmt.Errorf("规则格式应为 类型,内容,路线")
	}
	end := len(fields)
	for end > 3 && ruleOptions[fields[end-1]] {
		end--
	}
	if !logical && end != 3 {
		return fmt.Errorf("规则格式应为 类型,内容,路线[,no-resolve]")
	}
	if fields[1] == "" {
		return fmt.Errorf("规则内容不能为空")
	}
	if logical && !strings.HasPrefix(fields[1], "(") {
		return fmt.Errorf("%s 规则的条件必须用括号包裹", ruleType)
	}
	return validateTarget(fields[end-1])
}

func validateTarget(target string) error {
	if target == "" {
		return fmt.Errorf("缺少路线")
	}
	if !allowedTargets[target] {
		return fmt.Errorf("不支持的路线 %q，可用：内网、外网代理、默认出口、Claude、Tailscale、DIRECT、REJECT", target)
	}
	return nil
}

// DestinationFromInput extracts the hostname and TCP port used by a route
// probe. Bare domains default to HTTPS because that is the common website case.
func DestinationFromInput(input string) (string, int, error) {
	value := strings.TrimSpace(strings.ToLower(input))
	if value == "" {
		return "", 0, fmt.Errorf("请输入网站地址或域名")
	}
	if !strings.Contains(value, "://") {
		value = "//" + value
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Hostname() == "" {
		return "", 0, fmt.Errorf("无法识别网站地址")
	}
	host := strings.TrimSuffix(parsed.Hostname(), ".")
	if host == "" || net.ParseIP(host) != nil {
		return "", 0, fmt.Errorf("请输入域名，暂不支持 IP 地址规则")
	}

	port := 0
	if parsed.Port() != "" {
		port, err = strconv.Atoi(parsed.Port())
		if err != nil || port < 1 || port > 65535 {
			return "", 0, fmt.Errorf("网址端口必须是 1 到 65535 之间的整数")
		}
	} else {
		switch parsed.Scheme {
		case "", "https":
			port = 443
		case "http":
			port = 80
		default:
			return "", 0, fmt.Errorf("无法确定 %s 协议的目标端口，请在网址中明确指定", parsed.Scheme)
		}
	}
	return host, port, nil
}

// DomainFromInput accepts a website URL or a domain name and returns a
// normalized domain suitable for a DOMAIN-SUFFIX rule.
func DomainFromInput(input string) (string, error) {
	host, _, err := DestinationFromInput(input)
	return host, err
}

// EnsureTemplate creates the custom rule file on first install. Existing user
// rules are never replaced, including an intentionally empty file.
func EnsureTemplate(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("检查自定义规则: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("创建规则目录: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if os.IsExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("创建自定义规则模板: %w", err)
	}
	if _, err := file.WriteString(Template); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return fmt.Errorf("写入自定义规则模板: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("保存自定义规则模板: %w", err)
	}
	return nil
}

func Load(path string) ([]Rule, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取自定义规则: %w", err)
	}
	var result []Rule
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.Split(strings.TrimSpace(line), ",")
		if len(parts) < 3 || !strings.EqualFold(parts[0], "DOMAIN-SUFFIX") {
			continue
		}
		domain := strings.TrimPrefix(strings.TrimSpace(parts[1]), ".")
		target := strings.TrimSpace(parts[2])
		if domain != "" && allowedTargets[target] {
			result = append(result, Rule{Domain: domain, Target: target})
		}
	}
	return result, nil
}

// Save adds a simple domain rule, replacing the target for an existing domain.
// Unrelated hand-written lines and comments remain unchanged.
func Save(path string, rule Rule) error {
	domain, err := DomainFromInput(rule.Domain)
	if err != nil {
		return err
	}
	if !allowedTargets[rule.Target] {
		return fmt.Errorf("不支持的规则路线 %q", rule.Target)
	}
	if family := claude.DomainOf(domain); family != "" {
		// Claude 规则排在自定义规则之前，这里保存的规则不会生效。
		return fmt.Errorf("%s 属于 Claude（%s），固定走「%s」策略组；要换出口请在「策略」页切换", domain, family, config.ClaudeGroup)
	}

	var lines []string
	if data, readErr := os.ReadFile(path); readErr == nil {
		lines = strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	} else if !os.IsNotExist(readErr) {
		return fmt.Errorf("读取自定义规则: %w", readErr)
	}

	line := "DOMAIN-SUFFIX," + domain + "," + rule.Target
	replaced := false
	for i, current := range lines {
		parts := strings.Split(strings.TrimSpace(current), ",")
		if len(parts) < 3 || !strings.EqualFold(parts[0], "DOMAIN-SUFFIX") {
			continue
		}
		currentDomain := strings.TrimPrefix(strings.TrimSpace(parts[1]), ".")
		if strings.EqualFold(currentDomain, domain) {
			lines[i] = line
			replaced = true
			break
		}
	}
	if !replaced {
		lines = append(lines, line)
	}
	content := strings.Join(lines, "\n") + "\n"
	if _, err := ParseRules(content); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("创建规则目录: %w", err)
	}
	if err := config.WriteFileAtomic(path, []byte(content), 0o600); err != nil {
		return fmt.Errorf("保存自定义规则: %w", err)
	}
	return nil
}
