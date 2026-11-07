package ovpn

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Remote struct {
	Host  string
	Port  int
	Proto string
}

func (r Remote) String() string {
	return fmt.Sprintf("%s:%d", r.Host, r.Port)
}

type Profile struct {
	Source      string
	Remotes     []Remote
	Proto       string
	Dev         string
	Cipher      string
	Auth        string
	CompLZO     string
	Ping        int
	PingRestart int
	CA          string
	Cert        string
	Key         string
	TLSCrypt    string
	TLSCryptV2  string
	// TLSAuth 与 KeyDirection 记录 tls-auth 静态密钥和方向。
	TLSAuth      string
	KeyDirection string
	Username     string
	Password     string
	NeedsLogin   bool
	// Routes、DNS、Domains 来自 .ovpn 里的 route 和 dhcp-option，用作
	// 内网网段、内网 DNS、内网域名的默认值。服务端推送的设置不在文件里。
	Routes  []string
	DNS     []string
	Domains []string
}

func ParseFile(path string) (Profile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Profile{}, fmt.Errorf("读取 OVPN: %w", err)
	}
	content := string(data)
	profile, err := parseContent(content)
	if err != nil {
		return Profile{}, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return Profile{}, fmt.Errorf("解析 OVPN 路径: %w", err)
	}
	profile.Source = abs

	for directive, target := range map[string]*string{
		"ca": &profile.CA, "cert": &profile.Cert, "key": &profile.Key, "tls-crypt": &profile.TLSCrypt,
		"tls-crypt-v2": &profile.TLSCryptV2, "tls-auth": &profile.TLSAuth,
	} {
		if *target != "" {
			continue
		}
		source := directivePath(content, directive)
		if directive == "tls-auth" {
			// tls-auth 允许 "tls-auth ta.key 1" 形式，第二个字段是密钥方向。
			path, direction := splitTLSAuthDirective(source)
			source = path
			if direction != "" && profile.KeyDirection == "" {
				profile.KeyDirection = direction
			}
		}
		if source != "" {
			loaded, readErr := readRelativeFile(abs, source)
			if readErr != nil {
				return Profile{}, fmt.Errorf("读取 %s 文件: %w", directive, readErr)
			}
			*target = strings.TrimSpace(string(loaded))
		}
	}

	if profile.NeedsLogin && profile.Username == "" {
		credentialPath := authUserPassPath(content)
		if credentialPath != "" {
			credentials, readErr := readRelativeFile(abs, credentialPath)
			if readErr != nil {
				return Profile{}, fmt.Errorf("读取 auth-user-pass 凭据文件: %w", readErr)
			}
			lines := strings.Split(strings.ReplaceAll(string(credentials), "\r\n", "\n"), "\n")
			if len(lines) > 0 {
				profile.Username = strings.TrimSpace(lines[0])
			}
			if len(lines) > 1 {
				profile.Password = strings.TrimSpace(lines[1])
			}
		}
	}
	if profile.NeedsLogin && profile.Username == "" {
		return Profile{}, errors.New("OVPN 使用 auth-user-pass，但没有可用的内联或文件凭据")
	}
	if err := validateProfile(profile); err != nil {
		return Profile{}, err
	}
	return profile, nil
}

func Parse(content string) (Profile, error) {
	profile, err := parseContent(content)
	if err != nil {
		return Profile{}, err
	}
	if err := validateProfile(profile); err != nil {
		return Profile{}, err
	}
	return profile, nil
}

func parseContent(content string) (Profile, error) {
	profile := Profile{
		Proto:       "udp",
		Dev:         "tun",
		Cipher:      "AES-256-GCM",
		Auth:        "SHA256",
		CompLZO:     "no",
		Ping:        10,
		PingRestart: 45,
	}
	blocks := extractBlocks(content)
	dataCipherSeen := false
	legacyCipher := ""
	ncpDisabled := false
	// CA 可能是「根证书 + 中间证书」的证书链，必须完整保留；cert/key 只取
	// 第一段，跳过 OpenSSL 输出里 PEM 之前的文本描述。
	profile.CA = pemChain(blocks["ca"])
	profile.Cert = pemOnly(blocks["cert"])
	profile.Key = pemOnly(blocks["key"])
	profile.TLSCrypt = strings.TrimSpace(blocks["tls-crypt"])
	profile.TLSCryptV2 = strings.TrimSpace(blocks["tls-crypt-v2"])
	profile.TLSAuth = strings.TrimSpace(blocks["tls-auth"])
	if credentials := strings.Split(strings.TrimSpace(blocks["auth-user-pass"]), "\n"); len(credentials) >= 1 && strings.TrimSpace(credentials[0]) != "" {
		profile.Username = strings.TrimSpace(credentials[0])
		if len(credentials) > 1 {
			profile.Password = strings.TrimSpace(credentials[1])
		}
		profile.NeedsLogin = true
	}

	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "<") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "remote":
			if len(fields) < 2 {
				return Profile{}, errors.New("OVPN 中存在无效 remote")
			}
			port := 1194
			if len(fields) >= 3 {
				parsed, err := strconv.Atoi(fields[2])
				if err != nil || parsed < 1 || parsed > 65535 {
					return Profile{}, fmt.Errorf("OVPN remote %s 的端口无效", fields[1])
				}
				port = parsed
			}
			remoteProto := ""
			if len(fields) >= 4 {
				remoteProto = normalizeProto(fields[3])
			}
			profile.Remotes = append(profile.Remotes, Remote{Host: fields[1], Port: port, Proto: remoteProto})
		case "proto":
			if len(fields) > 1 {
				profile.Proto = normalizeProto(fields[1])
			}
		case "dev":
			if len(fields) > 1 {
				profile.Dev = fields[1]
			}
		case "cipher":
			if len(fields) > 1 {
				legacyCipher = normalizeCipher(fields[1])
				if ncpDisabled && !dataCipherSeen {
					profile.Cipher = legacyCipher
				}
			}
		case "data-ciphers", "ncp-ciphers":
			if len(fields) > 1 {
				profile.Cipher = preferredCipher(fields[1], profile.Cipher)
				dataCipherSeen = true
			}
		case "ncp-disable":
			ncpDisabled = true
			if legacyCipher != "" && !dataCipherSeen {
				profile.Cipher = legacyCipher
			}
		case "auth":
			if len(fields) > 1 {
				profile.Auth = fields[1]
			}
		case "keepalive":
			if len(fields) > 2 {
				if ping, err := strconv.Atoi(fields[1]); err == nil && ping > 0 {
					profile.Ping = ping
				}
				if restart, err := strconv.Atoi(fields[2]); err == nil && restart > 0 {
					profile.PingRestart = restart
				}
			}
		case "ping":
			if len(fields) > 1 {
				if ping, err := strconv.Atoi(fields[1]); err == nil && ping > 0 {
					profile.Ping = ping
				}
			}
		case "ping-restart":
			if len(fields) > 1 {
				if restart, err := strconv.Atoi(fields[1]); err == nil && restart > 0 {
					profile.PingRestart = restart
				}
			}
		case "comp-lzo":
			if len(fields) > 1 {
				profile.CompLZO = fields[1]
			} else {
				profile.CompLZO = "adaptive"
			}
		case "auth-user-pass":
			profile.NeedsLogin = true
		case "tls-auth":
			if len(fields) > 2 {
				profile.KeyDirection = fields[2]
			}
		case "route":
			if cidr := routeCIDR(fields[1:]); cidr != "" {
				profile.Routes = appendUnique(profile.Routes, cidr)
			}
		case "dhcp-option":
			if len(fields) < 3 {
				continue
			}
			switch strings.ToUpper(fields[1]) {
			case "DNS":
				if addr, err := netip.ParseAddr(fields[2]); err == nil && addr.Is4() {
					profile.DNS = appendUnique(profile.DNS, addr.String())
				}
			case "DOMAIN", "DOMAIN-SEARCH", "DOMAIN-ROUTE", "ADAPTER_DOMAIN_SUFFIX":
				domain := strings.ToLower(strings.Trim(fields[2], "."))
				if domain != "" {
					profile.Domains = appendUnique(profile.Domains, domain)
				}
			}
		case "key-direction":
			if len(fields) > 1 {
				profile.KeyDirection = fields[1]
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return Profile{}, fmt.Errorf("读取 OVPN 内容: %w", err)
	}
	if !dataCipherSeen && ncpDisabled && legacyCipher != "" {
		profile.Cipher = legacyCipher
	}
	return profile, nil
}

func validateProfile(profile Profile) error {
	if len(profile.Remotes) == 0 {
		return errors.New("OVPN 中没有 remote 配置")
	}
	if profile.CA == "" {
		return errors.New("OVPN 中没有可用的 CA 证书")
	}
	if (profile.Cert == "") != (profile.Key == "") {
		return errors.New("OVPN 的 cert 和 key 必须同时存在")
	}
	if profile.Cert == "" && !profile.NeedsLogin {
		return errors.New("OVPN 必须提供证书私钥或 auth-user-pass")
	}
	return nil
}

func (p Profile) SelectRemotes(filter string) ([]Remote, error) {
	filter = strings.TrimSpace(filter)
	if filter == "" {
		return append([]Remote(nil), p.Remotes...), nil
	}

	filters := strings.Split(filter, ",")
	selected := make([]Remote, 0, len(filters))
	seen := make(map[string]bool, len(filters))
	for _, value := range filters {
		value = strings.TrimSpace(value)
		if value == "" {
			return nil, fmt.Errorf("remote 列表 %q 包含空值", filter)
		}
		matched := false
		for _, remote := range p.Remotes {
			short := strings.SplitN(remote.Host, ".", 2)[0]
			if value != remote.String() && value != remote.Host && value != short {
				continue
			}
			matched = true
			key := remote.String()
			if !seen[key] {
				selected = append(selected, remote)
				seen[key] = true
			}
		}
		if !matched {
			return nil, fmt.Errorf("找不到 remote %q", value)
		}
	}
	return selected, nil
}

func extractBlocks(content string) map[string]string {
	blocks := make(map[string]string)
	var tag string
	var lines []string
	for _, raw := range strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n") {
		line := strings.TrimSpace(raw)
		if tag == "" {
			if strings.HasPrefix(line, "<") && strings.HasSuffix(line, ">") && !strings.HasPrefix(line, "</") {
				tag = strings.TrimSuffix(strings.TrimPrefix(line, "<"), ">")
				lines = nil
			}
			continue
		}
		if line == "</"+tag+">" {
			blocks[tag] = strings.TrimSpace(strings.Join(lines, "\n"))
			tag = ""
			lines = nil
			continue
		}
		lines = append(lines, raw)
	}
	return blocks
}

// pemOnly 返回 block 中的第一段 PEM；没有完整 PEM 时返回空。
func pemOnly(block string) string {
	blocks := pemBlocks(block)
	if len(blocks) == 0 {
		return ""
	}
	return blocks[0]
}

// pemChain 返回 block 中的全部 PEM 段，按原顺序用换行拼接。
func pemChain(block string) string {
	return strings.Join(pemBlocks(block), "\n")
}

func pemBlocks(block string) []string {
	var result []string
	var current []string
	inBlock := false
	for _, line := range strings.Split(block, "\n") {
		trimmed := strings.TrimSpace(line)
		if !inBlock {
			if strings.HasPrefix(trimmed, "-----BEGIN ") {
				inBlock = true
				current = []string{trimmed}
			}
			continue
		}
		current = append(current, trimmed)
		if strings.HasPrefix(trimmed, "-----END ") {
			result = append(result, strings.Join(current, "\n"))
			current = nil
			inBlock = false
		}
	}
	return result
}

// splitTLSAuthDirective 拆分 "path [direction]" 形式的 tls-auth 参数。
func splitTLSAuthDirective(value string) (string, string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", ""
	}
	if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') {
		quote := value[0]
		if end := strings.IndexByte(value[1:], quote); end >= 0 {
			path := value[1 : end+1]
			return path, strings.TrimSpace(value[end+2:])
		}
	}
	fields := strings.Fields(value)
	if len(fields) == 1 {
		return fields[0], ""
	}
	return fields[0], fields[1]
}

func authUserPassPath(content string) string {
	return directivePath(content, "auth-user-pass")
}

func directivePath(content, directive string) string {
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == directive {
			value := strings.TrimSpace(strings.TrimPrefix(line, directive))
			if len(value) >= 2 && ((value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'')) {
				value = value[1 : len(value)-1]
			}
			return value
		}
	}
	return ""
}

// routeCIDR 把 "route 网络 [掩码] ..." 转成 CIDR；网络不是 IPv4 地址（例如
// 主机名或 vpn_gateway）、掩码无效或是默认路由时返回空。
func routeCIDR(fields []string) string {
	if len(fields) == 0 {
		return ""
	}
	network, err := netip.ParseAddr(fields[0])
	if err != nil || !network.Is4() {
		return ""
	}
	bits := 32
	if len(fields) > 1 && fields[1] != "default" {
		mask := net.ParseIP(fields[1]).To4()
		if mask == nil {
			return ""
		}
		ones, size := net.IPMask(mask).Size()
		if size == 0 {
			return ""
		}
		bits = ones
	}
	if bits == 0 {
		return ""
	}
	prefix, err := network.Prefix(bits)
	if err != nil {
		return ""
	}
	return prefix.String()
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func normalizeProto(value string) string {
	if strings.HasPrefix(strings.ToLower(value), "tcp") {
		return "tcp"
	}
	return "udp"
}

func normalizeCipher(value string) string {
	return strings.ToUpper(value)
}

func preferredCipher(values, fallback string) string {
	for _, value := range strings.Split(values, ":") {
		value = normalizeCipher(value)
		switch value {
		case "AES-128-GCM", "AES-256-GCM", "AES-128-CBC", "AES-256-CBC", "CHACHA20-POLY1305":
			return value
		}
	}
	return fallback
}

func readRelativeFile(profilePath, source string) ([]byte, error) {
	if !filepath.IsAbs(source) {
		source = filepath.Join(filepath.Dir(profilePath), source)
	}
	return os.ReadFile(source)
}
