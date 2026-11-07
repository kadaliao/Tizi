package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

const appName = "tizi"

type Paths struct {
	ConfigDir   string
	DataDir     string
	Settings    string
	Generated   string
	PIDFile     string
	DNSState    string
	LogFile     string
	CustomRules string
	// ClaudeProxy 是写成 Mihomo 节点格式的住宅节点（ss、trojan、vless 等）。
	ClaudeProxy string
	// Proxies 是节点文件：Clash/Mihomo 节点 YAML，或每行一个分享链接。
	Proxies string
	// ShareConfig 是给手机扫码导入生成的配置（去掉了只能在本机使用的部分）。
	ShareConfig string
	CoreBin     string
	DownloadTmp string
}

type CoreChannel string

const (
	CoreChannelStable CoreChannel = "stable"
	CoreChannelAlpha  CoreChannel = "alpha"
)

func ParseCoreChannel(value string) (CoreChannel, error) {
	channel := CoreChannel(value)
	switch channel {
	case CoreChannelStable, CoreChannelAlpha:
		return channel, nil
	default:
		return "", fmt.Errorf("Mihomo 核心通道必须是 stable 或 alpha")
	}
}

type Settings struct {
	// Subscription 是机场订阅地址（Clash/Mihomo YAML 或 Base64 分享链接），
	// 由 Mihomo 自己下载和定时更新。包含令牌，界面上只显示脱敏后的地址。
	Subscription string `json:"subscription,omitempty"`

	// OVPNPath 是内网 VPN 的 .ovpn 文件，可选。下面的 Intranet* 只在导入
	// .ovpn 后生效；留空时使用 .ovpn 里的 route、dhcp-option 设置。
	OVPNPath string `json:"ovpn_path,omitempty"`
	Remote   string `json:"remote,omitempty"`
	// IntranetDomains 是走内网并交给内网 DNS 解析的域名后缀。
	IntranetDomains []string `json:"intranet_domains,omitempty"`
	// IntranetCIDRs 是走内网的 IP 网段。
	IntranetCIDRs []string `json:"intranet_cidrs,omitempty"`
	// IntranetDNS 是内网 DNS 服务器，经内网隧道查询。
	IntranetDNS []string `json:"intranet_dns,omitempty"`
	// IntranetHTTPProxy 是内网里提供的 HTTP 出口（host:port），设置后可以
	// 在「外网代理」里选它，经内网访问外网。
	IntranetHTTPProxy string `json:"intranet_http_proxy,omitempty"`
	// IntranetProbeURL 是检查内网隧道是否可用的地址；留空时使用内网 HTTP
	// 出口，两者都没有时不做主动检查。
	IntranetProbeURL string `json:"intranet_probe_url,omitempty"`

	// TailscaleEnabled 让 Mihomo 以一台设备的身份加入 tailnet，访问
	// 100.64.0.0/10 和 *.ts.net。首次登录需要 TailscaleAuthKey。
	TailscaleEnabled    bool   `json:"tailscale_enabled,omitempty"`
	TailscaleAuthKey    string `json:"tailscale_auth_key,omitempty"`
	TailscaleHostname   string `json:"tailscale_hostname,omitempty"`
	TailscaleControlURL string `json:"tailscale_control_url,omitempty"`
	// TailscaleExitNode 设置后，「外网代理」里多一个经该出口节点上网的选项。
	TailscaleExitNode string `json:"tailscale_exit_node,omitempty"`

	TunEnabled       bool        `json:"tun_enabled"`
	AllowLAN         bool        `json:"allow_lan"`
	ProxyUsername    string      `json:"proxy_username,omitempty"`
	ProxyPassword    string      `json:"proxy_password,omitempty"`
	ChinaDirect      bool        `json:"china_direct"`
	MixedPort        int         `json:"mixed_port"`
	ControllerPort   int         `json:"controller_port"`
	DNSPort          int         `json:"dns_port"`
	CoreChannel      CoreChannel `json:"core_channel,omitempty"`
	ControllerSecret string      `json:"controller_secret,omitempty"`
	// ClaudeProxy 是 Claude 专用住宅节点的地址（http://、https:// 或
	// socks5://，可含用户名和密码）。其他协议写进 claude-proxy.yaml。
	ClaudeProxy string `json:"claude_proxy,omitempty"`
	// ClaudeProxyDirect 为 true 时本机直接连接住宅节点；默认经「前置代理」
	// 链式连接（没有可用的代理节点时总是直接连接）。
	ClaudeProxyDirect bool `json:"claude_proxy_direct,omitempty"`
	// ClaudeGuard 在没有配置住宅节点时也监测 Claude 的访问；配置了住宅
	// 节点时总是监测。
	ClaudeGuard      bool `json:"claude_guard,omitempty"`
	AllowVPNConflict bool `json:"-"`
}

// ProxyAuthEnabled 报告局域网代理是否要求认证。服务器有公网地址时，没有
// 认证的局域网代理任何人都能用来进内网。用户名和密码只保存在本机的
// settings.json 里。
func (s Settings) ProxyAuthEnabled() bool {
	return s.ProxyUsername != "" && s.ProxyPassword != ""
}

// ValidateProxyCredentials 检查局域网代理的用户名和密码。Mihomo 按第一个
// 冒号拆分 user:pass，所以用户名不能含冒号；空白和控制字符写进代理 URL
// 容易出错。只设置其中一项时报错，以免误以为开了认证。
func ValidateProxyCredentials(username, password string) error {
	if err := CheckProxyUsername(username); err != nil {
		return err
	}
	if err := CheckProxyPassword(password); err != nil {
		return err
	}
	if (username == "") != (password == "") {
		return fmt.Errorf("代理认证需要同时设置用户名和密码")
	}
	return nil
}

// CheckProxyUsername 只检查用户名本身的字符，界面逐项编辑时使用。
func CheckProxyUsername(username string) error {
	if strings.Contains(username, ":") {
		return fmt.Errorf("代理用户名不能包含冒号")
	}
	if hasSpaceOrControl(username) {
		return fmt.Errorf("代理用户名不能包含空白或控制字符")
	}
	return nil
}

// CheckProxyPassword 只检查密码本身的字符，界面逐项编辑时使用。
func CheckProxyPassword(password string) error {
	if hasSpaceOrControl(password) {
		return fmt.Errorf("代理密码不能包含空白或控制字符")
	}
	return nil
}

func hasSpaceOrControl(value string) bool {
	return strings.IndexFunc(value, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0
}

// NewProxyPassword 生成局域网代理认证密码。十六进制不含 : 和 @，可以
// 直接写进 Mihomo 的 user:pass 和代理 URL。
func NewProxyPassword() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("生成代理密码: %w", err)
	}
	return hex.EncodeToString(value), nil
}

func EnsureControllerSecret(paths Paths, settings *Settings) error {
	if settings.ControllerSecret != "" {
		return nil
	}
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return fmt.Errorf("生成 Controller 密钥: %w", err)
	}
	settings.ControllerSecret = hex.EncodeToString(value)
	return Save(paths, *settings)
}

func DefaultSettings() Settings {
	return Settings{
		TunEnabled:     true,
		ChinaDirect:    true,
		MixedPort:      7890,
		ControllerPort: 9090,
		DNSPort:        1053,
		CoreChannel:    CoreChannelStable,
	}
}

func ResolvePaths() (Paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, fmt.Errorf("获取用户目录: %w", err)
	}
	configRoot := os.Getenv("XDG_CONFIG_HOME")
	if configRoot == "" {
		configRoot = filepath.Join(home, ".config")
	}
	dataRoot := os.Getenv("XDG_DATA_HOME")
	if dataRoot == "" {
		dataRoot = filepath.Join(home, ".local", "share")
	}
	configDir := filepath.Join(configRoot, appName)
	dataDir := filepath.Join(dataRoot, appName)
	return Paths{
		ConfigDir:   configDir,
		DataDir:     dataDir,
		Settings:    filepath.Join(configDir, "settings.json"),
		Generated:   filepath.Join(configDir, "config.yaml"),
		PIDFile:     filepath.Join(configDir, "mihomo.pid"),
		DNSState:    filepath.Join(configDir, "dns-state.json"),
		LogFile:     filepath.Join(configDir, "mihomo.log"),
		CustomRules: filepath.Join(configDir, "custom-rules.txt"),
		ClaudeProxy: filepath.Join(configDir, ClaudeProxyFile),
		Proxies:     filepath.Join(configDir, ProxiesFile),
		ShareConfig: filepath.Join(configDir, "share.yaml"),
		CoreBin:     filepath.Join(dataDir, "bin", "mihomo"),
		DownloadTmp: filepath.Join(dataDir, "bin", ".mihomo.download"),
	}, nil
}

func (p Paths) Ensure() error {
	for _, dir := range []string{p.ConfigDir, p.DataDir, filepath.Dir(p.CoreBin)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("创建目录 %s: %w", dir, err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("设置目录权限 %s: %w", dir, err)
		}
	}
	return nil
}

func Load(paths Paths) (Settings, error) {
	settings := DefaultSettings()
	data, err := os.ReadFile(paths.Settings)
	if errors.Is(err, os.ErrNotExist) {
		return settings, nil
	}
	if err != nil {
		return settings, fmt.Errorf("读取设置: %w", err)
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		return settings, fmt.Errorf("解析设置: %w", err)
	}
	settings.normalize()
	return settings, nil
}

func Save(paths Paths, settings Settings) error {
	settings.normalize()
	if err := paths.Ensure(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化设置: %w", err)
	}
	data = append(data, '\n')
	if err := WriteFileAtomic(paths.Settings, data, 0o600); err != nil {
		return fmt.Errorf("保存设置: %w", err)
	}
	return nil
}

// WriteFileAtomic 先写入同目录临时文件并 fsync，再原子替换目标文件，
// 避免断电或崩溃时留下空文件或半截内容；perm 会强制应用到最终文件。
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	file, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return fmt.Errorf("创建临时文件: %w", err)
	}
	cleanup := func(err error) error {
		_ = file.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := file.Chmod(perm); err != nil {
		return cleanup(fmt.Errorf("设置临时文件权限: %w", err))
	}
	if _, err := file.Write(data); err != nil {
		return cleanup(fmt.Errorf("写入临时文件: %w", err))
	}
	if err := file.Sync(); err != nil {
		return cleanup(fmt.Errorf("同步临时文件: %w", err))
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("关闭临时文件: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("替换目标文件: %w", err)
	}
	return nil
}

func (s *Settings) normalize() {
	s.Subscription = strings.TrimSpace(s.Subscription)
	s.IntranetDomains = NormalizeDomains(s.IntranetDomains)
	s.IntranetCIDRs = cleanList(s.IntranetCIDRs)
	s.IntranetDNS = cleanList(s.IntranetDNS)
	s.IntranetHTTPProxy = strings.TrimSpace(s.IntranetHTTPProxy)
	s.IntranetProbeURL = strings.TrimSpace(s.IntranetProbeURL)
	s.TailscaleAuthKey = strings.TrimSpace(s.TailscaleAuthKey)
	s.TailscaleHostname = strings.TrimSpace(s.TailscaleHostname)
	s.TailscaleControlURL = strings.TrimSpace(s.TailscaleControlURL)
	s.TailscaleExitNode = strings.TrimSpace(s.TailscaleExitNode)
	if _, err := ParseCoreChannel(string(s.CoreChannel)); err != nil {
		s.CoreChannel = CoreChannelStable
	}
	if s.MixedPort <= 0 || s.MixedPort > 65535 {
		s.MixedPort = 7890
	}
	if s.ControllerPort <= 0 || s.ControllerPort > 65535 {
		s.ControllerPort = 9090
	}
	if s.DNSPort <= 0 || s.DNSPort > 65535 {
		s.DNSPort = 1053
	}
}
