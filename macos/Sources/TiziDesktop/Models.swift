import Foundation

// 与 internal/engine/protocol.go 一一对应；字段含义见 Go 侧注释。
let supportedProtocol = 1

struct Hello: Decodable {
    var version: String
    var `protocol`: Int
    var configDir: String
    var settingsFile: String
    var logFile: String
    var customRules: String
    var generated: String
    /// 写成 Mihomo 节点格式的住宅节点文件；旧引擎没有这个字段。
    var claudeProxyFile: String?
    /// 节点文件 proxies.yaml（Clash 节点 YAML 或分享链接）；旧引擎没有这个字段。
    var proxiesFile: String?
}

struct Snapshot: Decodable {
    var at: Int64
    var busy: String?
    var managed: Managed
    var controllerOnline: Bool
    var conflicts: Conflicts
    var dnsRecoveryPending: Bool
    var settings: EngineSettings
    var profile: Profile
    var groups: [PolicyGroup]
    var traffic: Traffic
    var connections: [Connection]
    var update: UpdateInfo
    var share: Share?
    var install: InstallProgress?
    /// Claude 访问监测的结论；旧引擎没有这个字段。
    var claude: ClaudeStatus?
}

/// 与 Go 侧 engine.Claude 对应。level 为 alert 时弹窗，同一个 key 只弹一次。
struct ClaudeStatus: Decodable, Equatable {
    var level: String
    var key: String?
    var title: String
    var detail: String?
    var residential: Bool
    var route: String?
    var exitIP: String?
    var country: String?
    var checkedAt: Int64?
    var pending: Int?

    var isAlert: Bool { level == "alert" }
    var isOK: Bool { level == "ok" }
    /// 还没达到告警阈值的失败次数大于 0，表示正在复查。
    var rechecking: Bool { (pending ?? 0) > 0 }

    var exitText: String? {
        guard let ip = exitIP, !ip.isEmpty else { return nil }
        if let country, !country.isEmpty { return "\(ip)（\(country)）" }
        return ip
    }

    var checkedDate: Date? {
        checkedAt.map { Date(timeIntervalSince1970: TimeInterval($0) / 1000) }
    }
}

struct Managed: Decodable {
    var running: Bool
    var pid: Int?
    var privileged: Bool
    var coreInstalled: Bool
    var coreVersion: String?
}

struct Conflicts: Decodable {
    var tunnelblick: Int
    var openvpn: Int

    var any: Bool { tunnelblick > 0 || openvpn > 0 }
}

struct EngineSettings: Decodable, Equatable {
    // 线路设置。旧引擎没有这些字段，缺少时按默认值解码。
    /// 至少配置了一种线路（订阅、节点文件、.ovpn、Tailscale 或住宅节点），可以连接。
    var ready: Bool
    /// 脱敏后的订阅地址，例如 https://host/…；完整地址不离开引擎。
    var subscription: String
    var hasSubscription: Bool
    /// 节点文件里的节点数；nodesError 是节点文件的问题。
    var nodeCount: Int
    var nodesError: String?

    /// 内网 .ovpn，可选；为空表示没有导入。
    var ovpnPath: String
    var remote: String
    /// 设置里填写的内网参数，留空表示自动（来自 .ovpn 或默认值）。
    var intranetDomains: [String]
    var intranetCIDRs: [String]
    var intranetDNS: [String]
    var intranetHTTPProxy: String
    var intranetProbeURL: String
    /// 实际生效的内网参数，只在导入了 .ovpn 时有值。
    var intranet: IntranetView

    var tailscaleEnabled: Bool
    /// 脱敏后的 Auth Key。
    var tailscaleAuthKey: String
    var tailscaleHasAuthKey: Bool
    /// 已经登录过，之后不再需要 Auth Key。
    var tailscaleLoggedIn: Bool
    /// 设置里填写的设备名；tailscaleDeviceName 是实际使用的名字。
    var tailscaleHostname: String
    var tailscaleDeviceName: String
    var tailscaleControlURL: String
    var tailscaleExitNode: String

    var tunEnabled: Bool
    var allowLAN: Bool
    /// 局域网代理认证的用户名；proxyAuth 表示用户名和密码都设置了。密码不随
    /// 快照推送，需要时用 proxyCredentials 取。旧引擎没有这些字段。
    var proxyUsername: String
    var proxyAuth: Bool
    var hasProxyPassword: Bool
    var chinaDirect: Bool
    var mixedPort: Int
    var controllerPort: Int
    var dnsPort: Int
    var coreChannel: String
    // 以下是住宅节点和 Claude 访问监测的设置；旧引擎没有这些字段。
    /// 住宅节点地址，密码已替换成 ***。
    var claudeProxy: String?
    /// 住宅节点地址的各项，用来预填表单；密码只说明有没有。
    var claudeNode: ClaudeNode?
    var claudeProxyInFile: Bool?
    var claudeProxyError: String?
    var claudeProxyDirect: Bool?
    var claudeGuard: Bool?
    var claudeGuardActive: Bool?
    /// 是否装了直连保护（系统状态）；claudeBlockSupported 表示当前系统支持。
    var claudeBlock: Bool?
    var claudeBlockSupported: Bool?

    private enum CodingKeys: String, CodingKey {
        case ready, subscription, hasSubscription, nodeCount, nodesError
        case ovpnPath, remote, intranetDomains, intranetCIDRs, intranetDNS, intranetHTTPProxy, intranetProbeURL, intranet
        case tailscaleEnabled, tailscaleAuthKey, tailscaleHasAuthKey, tailscaleLoggedIn, tailscaleHostname
        case tailscaleDeviceName, tailscaleControlURL, tailscaleExitNode
        case tunEnabled, allowLAN, proxyUsername, proxyAuth, hasProxyPassword
        case chinaDirect, mixedPort, controllerPort, dnsPort, coreChannel
        case claudeProxy, claudeNode, claudeProxyInFile, claudeProxyError, claudeProxyDirect
        case claudeGuard, claudeGuardActive, claudeBlock, claudeBlockSupported
    }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        ovpnPath = try c.decodeIfPresent(String.self, forKey: .ovpnPath) ?? ""
        remote = try c.decodeIfPresent(String.self, forKey: .remote) ?? ""
        // 旧引擎只认 .ovpn：有 .ovpn 就算配置好了。
        ready = try c.decodeIfPresent(Bool.self, forKey: .ready) ?? !ovpnPath.isEmpty
        subscription = try c.decodeIfPresent(String.self, forKey: .subscription) ?? ""
        hasSubscription = try c.decodeIfPresent(Bool.self, forKey: .hasSubscription) ?? !subscription.isEmpty
        nodeCount = try c.decodeIfPresent(Int.self, forKey: .nodeCount) ?? 0
        nodesError = try c.decodeIfPresent(String.self, forKey: .nodesError)
        intranetDomains = try c.decodeIfPresent([String].self, forKey: .intranetDomains) ?? []
        intranetCIDRs = try c.decodeIfPresent([String].self, forKey: .intranetCIDRs) ?? []
        intranetDNS = try c.decodeIfPresent([String].self, forKey: .intranetDNS) ?? []
        intranetHTTPProxy = try c.decodeIfPresent(String.self, forKey: .intranetHTTPProxy) ?? ""
        intranetProbeURL = try c.decodeIfPresent(String.self, forKey: .intranetProbeURL) ?? ""
        intranet = try c.decodeIfPresent(IntranetView.self, forKey: .intranet) ?? IntranetView()
        tailscaleEnabled = try c.decodeIfPresent(Bool.self, forKey: .tailscaleEnabled) ?? false
        tailscaleAuthKey = try c.decodeIfPresent(String.self, forKey: .tailscaleAuthKey) ?? ""
        tailscaleHasAuthKey = try c.decodeIfPresent(Bool.self, forKey: .tailscaleHasAuthKey) ?? false
        tailscaleLoggedIn = try c.decodeIfPresent(Bool.self, forKey: .tailscaleLoggedIn) ?? false
        tailscaleHostname = try c.decodeIfPresent(String.self, forKey: .tailscaleHostname) ?? ""
        tailscaleDeviceName = try c.decodeIfPresent(String.self, forKey: .tailscaleDeviceName) ?? ""
        tailscaleControlURL = try c.decodeIfPresent(String.self, forKey: .tailscaleControlURL) ?? ""
        tailscaleExitNode = try c.decodeIfPresent(String.self, forKey: .tailscaleExitNode) ?? ""
        tunEnabled = try c.decode(Bool.self, forKey: .tunEnabled)
        allowLAN = try c.decode(Bool.self, forKey: .allowLAN)
        proxyUsername = try c.decodeIfPresent(String.self, forKey: .proxyUsername) ?? ""
        proxyAuth = try c.decodeIfPresent(Bool.self, forKey: .proxyAuth) ?? false
        hasProxyPassword = try c.decodeIfPresent(Bool.self, forKey: .hasProxyPassword) ?? false
        chinaDirect = try c.decode(Bool.self, forKey: .chinaDirect)
        mixedPort = try c.decode(Int.self, forKey: .mixedPort)
        controllerPort = try c.decode(Int.self, forKey: .controllerPort)
        dnsPort = try c.decode(Int.self, forKey: .dnsPort)
        coreChannel = try c.decode(String.self, forKey: .coreChannel)
        claudeProxy = try c.decodeIfPresent(String.self, forKey: .claudeProxy)
        claudeNode = try c.decodeIfPresent(ClaudeNode.self, forKey: .claudeNode)
        claudeProxyInFile = try c.decodeIfPresent(Bool.self, forKey: .claudeProxyInFile)
        claudeProxyError = try c.decodeIfPresent(String.self, forKey: .claudeProxyError)
        claudeProxyDirect = try c.decodeIfPresent(Bool.self, forKey: .claudeProxyDirect)
        claudeGuard = try c.decodeIfPresent(Bool.self, forKey: .claudeGuard)
        claudeGuardActive = try c.decodeIfPresent(Bool.self, forKey: .claudeGuardActive)
        claudeBlock = try c.decodeIfPresent(Bool.self, forKey: .claudeBlock)
        claudeBlockSupported = try c.decodeIfPresent(Bool.self, forKey: .claudeBlockSupported)
    }

    var hasResidentialNode: Bool { !(claudeProxy ?? "").isEmpty || claudeProxyInFile == true }
    var hasOVPN: Bool { !ovpnPath.isEmpty }

    /// 有没有能做「前置代理」的节点，与引擎的 claude.Chainable 一致。没有时
    /// 住宅节点总是直接连接。
    var canChain: Bool {
        hasSubscription || nodeCount > 0 || (hasOVPN && !intranetHTTPProxy.isEmpty)
            || (tailscaleEnabled && !tailscaleExitNode.isEmpty)
    }

    /// 配置了几种线路；只剩一种时，界面不提供移除它的快捷入口。
    var routeCount: Int {
        [hasSubscription, nodeCount > 0, hasOVPN, tailscaleEnabled, hasResidentialNode].filter { $0 }.count
    }

    /// 已配置的线路，简短列出，例如 ["订阅", "3 个节点", "内网", "Tailscale"]。
    var routeSummary: [String] {
        var parts: [String] = []
        if hasSubscription { parts.append("订阅") }
        if nodeCount > 0 { parts.append("\(nodeCount) 个节点") }
        if hasOVPN { parts.append("内网") }
        if tailscaleEnabled { parts.append("Tailscale") }
        if parts.isEmpty && hasResidentialNode { parts.append("Claude 住宅节点") }
        return parts
    }
}

/// 实际生效的内网设置；*Auto 表示该项来自 .ovpn 或默认值。
struct IntranetView: Decodable, Equatable {
    var domains: [String] = []
    var cidrs: [String] = []
    var dns: [String] = []
    var domainsAuto = false
    var cidrsAuto = false
    var dnsAuto = false

    private enum CodingKeys: String, CodingKey {
        case domains, cidrs, dns, domainsAuto, cidrsAuto, dnsAuto
    }

    init() {}

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        domains = try c.decodeIfPresent([String].self, forKey: .domains) ?? []
        cidrs = try c.decodeIfPresent([String].self, forKey: .cidrs) ?? []
        dns = try c.decodeIfPresent([String].self, forKey: .dns) ?? []
        domainsAuto = try c.decodeIfPresent(Bool.self, forKey: .domainsAuto) ?? false
        cidrsAuto = try c.decodeIfPresent(Bool.self, forKey: .cidrsAuto) ?? false
        dnsAuto = try c.decodeIfPresent(Bool.self, forKey: .dnsAuto) ?? false
    }
}

struct ClaudeNode: Decodable, Equatable {
    var scheme: String
    var host: String
    var port: Int
    var username: String?
    var hasPassword: Bool
}

/// 引擎从粘贴的整段地址里认出的各项；没写协议或端口时为空。
struct ParsedClaudeNode: Decodable {
    var scheme: String?
    var host: String
    var port: Int?
    var username: String?
    var password: String?
}

struct Profile: Decodable {
    var loaded: Bool
    var error: String?
    var remotes: [String]
}

struct PolicyGroup: Decodable, Identifiable {
    var name: String
    var now: String
    var choices: [String]

    var id: String { name }
}

struct Traffic: Decodable {
    var download: Double
    var upload: Double
    var connections: Int
}

struct Connection: Decodable, Identifiable {
    var id: String
    var destination: String
    var process: String?
    var rule: String?
    var route: String?
    var download: Int64
    var upload: Int64
    var downloadRate: Double
    var uploadRate: Double
    var sampled: Bool

    // Table 排序需要非可选的键。
    var processName: String { process ?? "" }
    var ruleText: String { rule ?? "" }
    var routeText: String { route ?? "" }
}

struct UpdateInfo: Decodable {
    var checked: Bool
    var available: Bool
    var latest: String?
    var error: String?
    var installed: String?
    /// 最近一次成功查询的时间（Unix 毫秒）。
    var checkedAt: Int64?

    var lastChecked: Date? {
        checkedAt.map { Date(timeIntervalSince1970: TimeInterval($0) / 1000) }
    }
}

struct Share: Decodable, Equatable {
    var url: String
    var expiresAt: Int64

    var expiry: Date { Date(timeIntervalSince1970: TimeInterval(expiresAt) / 1000) }
}

struct InstallProgress: Decodable {
    var phase: String
    var downloaded: Int64
    var total: Int64

    var label: String {
        switch phase {
        case "core": return "下载 Mihomo 核心"
        case "geoip": return "下载 GeoIP 数据"
        default: return "查询 Mihomo 最新版本"
        }
    }

    var fraction: Double? {
        total > 0 ? min(1, Double(downloaded) / Double(total)) : nil
    }
}

struct ActionResult: Decodable {
    var message: String
}

struct UpdateResult: Decodable {
    var message: String
    var version: String
    /// 同步更新了的桌面版位置。
    var desktopApp: String?
}

struct DiagnosticCheck: Decodable, Identifiable {
    var level: String
    var name: String
    var detail: String

    var id: String { name + "|" + detail }
}

struct RouteTest: Decodable {
    var host: String
    var ip: String?
    var rule: String?
    var route: String?
    var found: Bool
}

struct CustomRule: Decodable, Identifiable {
    var domain: String
    var target: String

    var id: String { domain }
}

struct ProxyCredentials: Decodable {
    var username: String
    var password: String
}

struct Rules: Decodable {
    var rules: [CustomRule]
    var targets: [String]
}

enum Format {
    static func rate(_ bytesPerSecond: Double) -> String {
        let kib = 1024.0, mib = kib * 1024, gib = mib * 1024
        switch bytesPerSecond {
        case gib...: return String(format: "%.1f GiB/s", bytesPerSecond / gib)
        case mib...: return String(format: "%.1f MiB/s", bytesPerSecond / mib)
        case kib...: return String(format: "%.1f KiB/s", bytesPerSecond / kib)
        default: return String(format: "%.0f B/s", bytesPerSecond)
        }
    }

    /// 拆成数字和单位，界面里数字大、单位小。
    static func rateParts(_ bytesPerSecond: Double) -> (number: String, unit: String) {
        let text = rate(bytesPerSecond)
        guard let space = text.lastIndex(of: " ") else { return (text, "") }
        return (String(text[..<space]), String(text[text.index(after: space)...]))
    }

    static func bytes(_ count: Int64) -> String {
        ByteCountFormatter.string(fromByteCount: count, countStyle: .binary)
    }
}
