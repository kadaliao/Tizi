package engine

import "encoding/json"

// ProtocolVersion 在请求、响应或快照字段发生不兼容变化时递增；GUI 握手时
// 比对它，旧 GUI 搭配新引擎（或反之）时提示更新，而不是静默解析失败。
const ProtocolVersion = 1

// Request 是 GUI 发来的一行 JSON。ID 由 GUI 分配，响应原样带回。
type Request struct {
	ID     int64           `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// Response 对应一个 Request；Error 非空时 Result 省略。
type Response struct {
	ID     int64  `json:"id"`
	Result any    `json:"result,omitempty"`
	Error  *Error `json:"error,omitempty"`
}

// Error 的 Code 供 GUI 分支处理（例如 vpn_conflict 弹出确认框），
// Message 是可以直接展示给用户的中文说明。
type Error struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
}

// Event 是引擎主动推送的消息，没有 ID。
type Event struct {
	Event string `json:"event"`
	Data  any    `json:"data"`
}

// 错误码。
const (
	CodeBusy          = "busy"
	CodeInvalidParams = "invalid_params"
	CodeUnknownMethod = "unknown_method"
	// CodeNotConfigured 表示还没有任何线路（订阅、节点文件、.ovpn、
	// Tailscale 或住宅节点）。
	CodeNotConfigured = "not_configured"
	CodeCoreMissing   = "core_missing"
	CodeVPNConflict   = "vpn_conflict"
	CodeOffline       = "controller_offline"
	CodeFailed        = "failed"
)

// Hello 是握手结果，也告诉 GUI 可以直接打开的本地文件位置。
type Hello struct {
	Version      string `json:"version"`
	Protocol     int    `json:"protocol"`
	ConfigDir    string `json:"configDir"`
	SettingsFile string `json:"settingsFile"`
	LogFile      string `json:"logFile"`
	CustomRules  string `json:"customRules"`
	Generated    string `json:"generated"`
	// ClaudeProxyFile 是写成 Mihomo 节点格式的住宅节点文件。
	ClaudeProxyFile string `json:"claudeProxyFile"`
	// ProxiesFile 是节点文件（Clash 节点 YAML 或分享链接）。
	ProxiesFile string `json:"proxiesFile"`
}

// Snapshot 是每 2 秒推送一次的完整状态，字段含义与 TUI 状态页一致。
type Snapshot struct {
	At                 int64            `json:"at"`
	Busy               string           `json:"busy,omitempty"`
	Managed            Managed          `json:"managed"`
	ControllerOnline   bool             `json:"controllerOnline"`
	Conflicts          Conflicts        `json:"conflicts"`
	DNSRecoveryPending bool             `json:"dnsRecoveryPending"`
	Settings           Settings         `json:"settings"`
	Profile            Profile          `json:"profile"`
	Groups             []Group          `json:"groups"`
	Traffic            Traffic          `json:"traffic"`
	Connections        []Connection     `json:"connections"`
	Update             Update           `json:"update"`
	Share              *Share           `json:"share,omitempty"`
	Install            *InstallProgress `json:"install,omitempty"`
	Claude             Claude           `json:"claude"`
}

// ClaudeNode 是住宅节点地址的各项。密码不离开引擎，只说明有没有。
type ClaudeNode struct {
	Scheme      string `json:"scheme"`
	Host        string `json:"host"`
	Port        int    `json:"port"`
	Username    string `json:"username,omitempty"`
	HasPassword bool   `json:"hasPassword"`
}

// ParsedClaudeNode 是 parseClaudeAddress 从粘贴的整段地址里认出的各项。
// 地址里没写协议时 Scheme 为空，没写端口时 Port 为 0。
type ParsedClaudeNode struct {
	Scheme   string `json:"scheme,omitempty"`
	Host     string `json:"host"`
	Port     int    `json:"port,omitempty"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

// Claude 是 Claude 访问监测的当前结论，字段含义见 claudeguard.Status。
// Level 为 alert 时界面弹窗；同一个 Key 只弹一次，恢复正常后重新计。
type Claude struct {
	Level       string `json:"level"`
	Key         string `json:"key,omitempty"`
	Title       string `json:"title"`
	Detail      string `json:"detail,omitempty"`
	Residential bool   `json:"residential"`
	Route       string `json:"route,omitempty"`
	ExitIP      string `json:"exitIP,omitempty"`
	Country     string `json:"country,omitempty"`
	CheckedAt   int64  `json:"checkedAt,omitempty"`
	Pending     int    `json:"pending,omitempty"`
}

type Managed struct {
	Running       bool   `json:"running"`
	PID           int    `json:"pid,omitempty"`
	Privileged    bool   `json:"privileged"`
	CoreInstalled bool   `json:"coreInstalled"`
	CoreVersion   string `json:"coreVersion,omitempty"`
}

type Conflicts struct {
	Tunnelblick int `json:"tunnelblick"`
	OpenVPN     int `json:"openvpn"`
}

// Settings 是 config.Settings 去掉 Controller 密钥后的视图。订阅地址和
// Tailscale Auth Key 不离开引擎，只给出脱敏后的样子。
type Settings struct {
	// Ready 表示至少配置了一种线路，可以连接。
	Ready bool `json:"ready"`
	// Subscription 是脱敏后的订阅地址；HasSubscription 表示设置了订阅。
	Subscription    string `json:"subscription"`
	HasSubscription bool   `json:"hasSubscription"`
	// NodeCount 是节点文件里的节点数；NodesError 是节点文件的问题。
	NodeCount  int    `json:"nodeCount"`
	NodesError string `json:"nodesError,omitempty"`

	OVPNPath string `json:"ovpnPath"`
	Remote   string `json:"remote"`
	// Intranet* 是设置里填写的内网参数（留空表示用 .ovpn 里的值）；
	// Intranet 是实际生效的值。
	IntranetDomains   []string     `json:"intranetDomains"`
	IntranetCIDRs     []string     `json:"intranetCIDRs"`
	IntranetDNS       []string     `json:"intranetDNS"`
	IntranetHTTPProxy string       `json:"intranetHTTPProxy"`
	IntranetProbeURL  string       `json:"intranetProbeURL"`
	Intranet          IntranetView `json:"intranet"`

	TailscaleEnabled bool `json:"tailscaleEnabled"`
	// TailscaleAuthKey 是脱敏后的 Auth Key。
	TailscaleAuthKey    string `json:"tailscaleAuthKey"`
	TailscaleHasAuthKey bool   `json:"tailscaleHasAuthKey"`
	// TailscaleLoggedIn 表示已经有登录状态，之后不再需要 Auth Key。
	TailscaleLoggedIn   bool   `json:"tailscaleLoggedIn"`
	TailscaleHostname   string `json:"tailscaleHostname"`
	TailscaleDeviceName string `json:"tailscaleDeviceName"`
	TailscaleControlURL string `json:"tailscaleControlURL"`
	TailscaleExitNode   string `json:"tailscaleExitNode"`

	TunEnabled     bool   `json:"tunEnabled"`
	AllowLAN       bool   `json:"allowLAN"`
	ChinaDirect    bool   `json:"chinaDirect"`
	MixedPort      int    `json:"mixedPort"`
	ControllerPort int    `json:"controllerPort"`
	DNSPort        int    `json:"dnsPort"`
	CoreChannel    string `json:"coreChannel"`
	// ClaudeProxy 是住宅节点地址，密码已替换成 ***。
	ClaudeProxy string `json:"claudeProxy"`
	// ClaudeNode 是住宅节点地址的各项，桌面版用来预填表单；地址无效或
	// 没有设置时为空。
	ClaudeNode *ClaudeNode `json:"claudeNode,omitempty"`
	// ClaudeProxyInFile 表示住宅节点写在 claude-proxy.yaml 里。
	ClaudeProxyInFile bool `json:"claudeProxyInFile"`
	// ClaudeProxyError 是住宅节点设置的问题，生成配置时会因此失败。
	ClaudeProxyError  string `json:"claudeProxyError,omitempty"`
	ClaudeProxyDirect bool   `json:"claudeProxyDirect"`
	ClaudeGuard       bool   `json:"claudeGuard"`
	// ClaudeGuardActive 表示正在监测：手动开启，或者配置了住宅节点。
	ClaudeGuardActive bool `json:"claudeGuardActive"`
	// ClaudeBlock 表示装了直连保护（这是系统状态，不是 settings.json 里的
	// 设置）；ClaudeBlockSupported 表示当前系统支持它。
	ClaudeBlock          bool `json:"claudeBlock"`
	ClaudeBlockSupported bool `json:"claudeBlockSupported"`
}

// IntranetView 是实际生效的内网设置，*Auto 表示来自 .ovpn 或默认值。
type IntranetView struct {
	Domains     []string `json:"domains"`
	CIDRs       []string `json:"cidrs"`
	DNS         []string `json:"dns"`
	DomainsAuto bool     `json:"domainsAuto"`
	CIDRsAuto   bool     `json:"cidrsAuto"`
	DNSAuto     bool     `json:"dnsAuto"`
}

// Profile 只暴露 OVPN 的 remote 列表；证书、私钥和口令不离开引擎进程。
type Profile struct {
	Loaded  bool     `json:"loaded"`
	Error   string   `json:"error,omitempty"`
	Remotes []string `json:"remotes"`
}

type Group struct {
	Name    string   `json:"name"`
	Now     string   `json:"now"`
	Choices []string `json:"choices"`
}

// Traffic 是全部活动连接的实时总速度（字节/秒）。
type Traffic struct {
	Download    float64 `json:"download"`
	Upload      float64 `json:"upload"`
	Connections int     `json:"connections"`
}

type Connection struct {
	ID           string  `json:"id"`
	Destination  string  `json:"destination"`
	Process      string  `json:"process,omitempty"`
	Rule         string  `json:"rule,omitempty"`
	Route        string  `json:"route,omitempty"`
	Download     int64   `json:"download"`
	Upload       int64   `json:"upload"`
	DownloadRate float64 `json:"downloadRate"`
	UploadRate   float64 `json:"uploadRate"`
	Sampled      bool    `json:"sampled"`
}

type Update struct {
	Checked   bool   `json:"checked"`
	Available bool   `json:"available"`
	Latest    string `json:"latest,omitempty"`
	Error     string `json:"error,omitempty"`
	// CheckedAt 是最近一次成功查询的时间（Unix 毫秒）；还没成功过时为 0。
	CheckedAt int64 `json:"checkedAt,omitempty"`
	// Installed 非空表示本进程已把 APP 更新到该版本，需要重启引擎才会用上新代码。
	Installed string `json:"installed,omitempty"`
}

type Share struct {
	URL       string `json:"url"`
	ExpiresAt int64  `json:"expiresAt"`
}

// InstallProgress 描述正在进行的 Mihomo 核心安装；也作为 install 事件推送。
type InstallProgress struct {
	Phase      string `json:"phase"`
	Downloaded int64  `json:"downloaded"`
	Total      int64  `json:"total"`
}

// ActionResult 是改变状态的操作的通用结果。
type ActionResult struct {
	Message string `json:"message"`
}

// UpdateResult 是 installUpdate 的结果。DesktopApp 非空表示该位置的桌面版也已
// 换成新版本；正在运行的正是它时，GUI 应重新打开自己，而不只是重启引擎。
type UpdateResult struct {
	Message    string `json:"message"`
	Version    string `json:"version"`
	DesktopApp string `json:"desktopApp,omitempty"`
}

type DiagnosticCheck struct {
	Level  string `json:"level"`
	Name   string `json:"name"`
	Detail string `json:"detail"`
}

type RouteTest struct {
	Host  string `json:"host"`
	IP    string `json:"ip,omitempty"`
	Rule  string `json:"rule,omitempty"`
	Route string `json:"route,omitempty"`
	Found bool   `json:"found"`
}

type CustomRule struct {
	Domain string `json:"domain"`
	Target string `json:"target"`
}

type Rules struct {
	Rules   []CustomRule `json:"rules"`
	Targets []string     `json:"targets"`
}
