import AppKit
import Network
import SwiftUI
import UniformTypeIdentifiers

struct ConfigPage: View {
    @EnvironmentObject private var model: AppModel

    var body: some View {
        if let snapshot = model.snapshot {
            form(snapshot)
                .task { await model.scanOVPN(quiet: true) }
        } else {
            ProgressView().frame(maxWidth: .infinity, maxHeight: .infinity)
        }
    }

    private func form(_ snapshot: Snapshot) -> some View {
        let settings = snapshot.settings
        return Form {
            ProxyNodesSection(settings: settings)
            IntranetSection(snapshot: snapshot)
            TailscaleSection(settings: settings)

            Section {
                Toggle("接管系统流量（TUN）", isOn: binding(settings.tunEnabled) { value in
                    model.updateSettings(["tunEnabled": value]) { $0.tunEnabled = value }
                })
                Toggle("允许局域网设备使用本机代理", isOn: binding(settings.allowLAN) { value in
                    model.updateSettings(["allowLAN": value]) { $0.allowLAN = value }
                })
                if settings.allowLAN {
                    ProxyAuthRows(settings: settings)
                }
                Picker("国内流量", selection: binding(settings.chinaDirect) { value in
                    model.updateSettings(["chinaDirect": value]) { $0.chinaDirect = value }
                }) {
                    Text("直连").tag(true)
                    Text("经外网代理").tag(false)
                }
            } header: {
                SectionTitle(title: "网络", symbol: "network", color: .indigo)
            } footer: {
                FormNote("关闭 TUN 后只提供本地代理端口，需要在应用或系统里手动设置代理。国内网站直连更快。")
            }

            ClaudeSection(settings: settings)

            Section {
                PortField(label: "Mixed 代理", value: settings.mixedPort) { port in
                    model.updateSettings(["mixedPort": port]) { $0.mixedPort = port }
                }
                PortField(label: "Controller", value: settings.controllerPort) { port in
                    model.updateSettings(["controllerPort": port]) { $0.controllerPort = port }
                }
                PortField(label: "DNS", value: settings.dnsPort) { port in
                    model.updateSettings(["dnsPort": port]) { $0.dnsPort = port }
                }
            } header: {
                SectionTitle(title: "端口", symbol: "number", color: Color(nsColor: .systemGray))
            }

            Section {
                Picker("核心通道", selection: binding(settings.coreChannel) { value in
                    model.updateSettings(["coreChannel": value]) { $0.coreChannel = value }
                }) {
                    Text("Stable").tag("stable")
                    Text("Alpha（测试尚未进入稳定版的修复）").tag("alpha")
                }
            } header: {
                SectionTitle(title: "Mihomo 核心", symbol: "shippingbox.fill", color: .brown)
            } footer: {
                FormNote("切换通道后，在「状态」中更新核心才会生效。")
            }

            Section {
                HStack {
                    Text(snapshot.managed.running
                         ? "设置保存在 settings.json，与终端版共用。Mihomo 运行中，重载后生效。"
                         : "设置保存在 settings.json，与终端版共用，下次连接时生效。")
                        .foregroundStyle(.secondary)
                    Spacer()
                    if snapshot.managed.running {
                        Button("重载 Mihomo") { Task { await model.reload() } }
                            .disabled(model.busyLabel != nil)
                    }
                }
            }
        }
        .formStyle(.grouped)
    }

    private func binding<Value>(_ value: Value, set: @escaping (Value) -> Void) -> Binding<Value> {
        Binding(get: { value }, set: set)
    }
}

enum OVPNFile {
    /// 打开文件选择框，默认从「下载」开始，只能选 .ovpn。
    static func choose() -> String? {
        let panel = NSOpenPanel()
        panel.title = "选择内网 .ovpn 配置"
        panel.prompt = "使用此文件"
        panel.allowsMultipleSelection = false
        panel.canChooseDirectories = false
        if let type = UTType(filenameExtension: "ovpn") {
            panel.allowedContentTypes = [type]
        }
        panel.directoryURL = FileManager.default.urls(for: .downloadsDirectory, in: .userDomainMask).first
        guard panel.runModal() == .OK, let url = panel.url else { return nil }
        return url.path
    }
}

// MARK: 代理节点

/// 订阅地址与节点文件。完整的订阅地址不回传到界面（通常含令牌），这里只显示
/// 脱敏后的样子；粘贴新地址保存即替换。
private struct ProxyNodesSection: View {
    @EnvironmentObject private var model: AppModel
    let settings: EngineSettings

    @State private var draft = ""
    @State private var saving = false

    private var trimmed: String { draft.trimmingCharacters(in: .whitespacesAndNewlines) }

    var body: some View {
        Section {
            TextField("订阅地址", text: $draft,
                      prompt: Text(settings.hasSubscription ? "已设置；粘贴新地址可替换" : "粘贴机场订阅地址，https://…"))
                .onSubmit(save)
            HStack {
                Text(subscriptionStatus)
                    .foregroundStyle(.secondary)
                    .lineLimit(1)
                    .truncationMode(.middle)
                    .textSelection(.enabled)
                Spacer()
                if settings.hasSubscription {
                    Button("清除") {
                        model.updateSettings(["subscription": ""]) { $0.subscription = ""; $0.hasSubscription = false }
                    }
                }
                Button("保存", action: save)
                    .disabled(saving || trimmed.isEmpty)
            }
            LabeledContent("节点文件") {
                HStack {
                    Text(nodeStatus)
                        .foregroundStyle(.secondary)
                    Spacer()
                    Button("编辑 proxies.yaml…") { Task { await model.openProxiesFile() } }
                        .help("每行一个分享链接（ss://、trojan://、vless:// 等），或 Mihomo 节点格式")
                }
            }
            if let error = settings.nodesError, !error.isEmpty {
                Text(error).font(.callout).foregroundStyle(.orange)
            }
        } header: {
            SectionTitle(title: "代理节点", symbol: RouteKind.proxy.symbol, color: RouteKind.proxy.color)
        } footer: {
            FormNote("有机场订阅时填订阅地址即可；自己的节点写进 proxies.yaml。两者都会出现在「自动选择」和「外网代理」里。")
        }
    }

    private var subscriptionStatus: String {
        if !trimmed.isEmpty { return settings.hasSubscription ? "有修改，尚未保存" : "填好后点「保存」" }
        return settings.hasSubscription ? "当前：\(settings.subscription)" : "未设置"
    }

    private var nodeStatus: String {
        if settings.nodesError?.isEmpty == false { return "有错误" }
        return settings.nodeCount > 0 ? "\(settings.nodeCount) 个节点" : "没有节点"
    }

    private func save() {
        guard !saving, !trimmed.isEmpty else { return }
        saving = true
        Task {
            if await model.saveSettings(["subscription": trimmed]) { draft = "" }
            saving = false
        }
    }
}

// MARK: 内网

/// 可选的内网 .ovpn 及其参数。参数留空时用 .ovpn 里的 route、dhcp-option，
/// 输入框的提示里显示实际生效的自动值。
private struct IntranetSection: View {
    @EnvironmentObject private var model: AppModel
    let snapshot: Snapshot

    private var settings: EngineSettings { snapshot.settings }

    var body: some View {
        Section {
            LabeledContent("OVPN 文件") {
                HStack {
                    Text(settings.hasOVPN ? settings.ovpnPath : "未导入")
                        .foregroundStyle(settings.hasOVPN ? .primary : .secondary)
                        .lineLimit(1)
                        .truncationMode(.middle)
                        .textSelection(.enabled)
                    Spacer()
                    if !model.scannedOVPN.isEmpty {
                        Menu("已发现") {
                            ForEach(model.scannedOVPN, id: \.self) { path in
                                Button(path) { model.updateSettings(["ovpnPath": path]) }
                            }
                        }
                        .fixedSize()
                    }
                    Button(settings.hasOVPN ? "更换…" : "导入…") { chooseOVPN() }
                    if settings.hasOVPN {
                        Button("移除") {
                            model.updateSettings(["ovpnPath": ""]) { $0.ovpnPath = ""; $0.remote = "" }
                        }
                        .help("不再连接内网；.ovpn 文件本身不会被删除")
                    }
                }
            }
            if settings.hasOVPN {
                if let error = snapshot.profile.error {
                    Text(error).font(.callout).foregroundStyle(.orange)
                }
                Picker("Remote", selection: Binding(get: { settings.remote }, set: { value in
                    model.updateSettings(["remote": value]) { $0.remote = value }
                })) {
                    Text("全部 remote").tag("")
                    ForEach(remoteChoices, id: \.self) { remote in
                        Text(remote).tag(remote)
                    }
                }
                .disabled(!snapshot.profile.loaded)
                SettingField(label: "内网域名", value: list(settings.intranetDomains),
                             prompt: autoPrompt(settings.intranet.domains, auto: settings.intranet.domainsAuto,
                                                fallback: "可选，例如 corp.example.com")) { value in
                    model.updateSettings(["intranetDomains": value])
                }
                SettingField(label: "内网网段", value: list(settings.intranetCIDRs),
                             prompt: autoPrompt(settings.intranet.cidrs, auto: settings.intranet.cidrsAuto,
                                                fallback: "例如 10.0.0.0/8")) { value in
                    model.updateSettings(["intranetCIDRs": value])
                }
                SettingField(label: "内网 DNS", value: list(settings.intranetDNS),
                             prompt: autoPrompt(settings.intranet.dns, auto: settings.intranet.dnsAuto,
                                                fallback: "可选，例如 10.0.0.2")) { value in
                    model.updateSettings(["intranetDNS": value])
                }
                SettingField(label: "内网 HTTP 出口", value: settings.intranetHTTPProxy,
                             prompt: "可选：内网里的 HTTP 代理，主机:端口") { value in
                    model.updateSettings(["intranetHTTPProxy": value]) { $0.intranetHTTPProxy = value }
                }
                SettingField(label: "内网检查地址", value: settings.intranetProbeURL,
                             prompt: settings.intranetHTTPProxy.isEmpty
                                ? "可选：用来发现内网断开，例如 http://wiki.corp.example.com/"
                                : "自动：http://\(settings.intranetHTTPProxy)") { value in
                    model.updateSettings(["intranetProbeURL": value]) { $0.intranetProbeURL = value }
                }
            }
        } header: {
            SectionTitle(title: "内网（可选）", symbol: RouteKind.intranet.symbol, color: RouteKind.intranet.color)
        } footer: {
            FormNote(settings.hasOVPN
                     ? "内网域名和网段经「内网」访问；留空的项自动使用 .ovpn 里的设置，多个值用逗号或空格分隔。填了内网 HTTP 出口时，「外网代理」里多一个经内网上网的选项。.ovpn 文件包含个人证书和私钥，不要发送给别人。"
                     : "需要访问内网（例如单位或家里的网络）时，导入 OpenVPN 的 .ovpn 文件。可在下载、桌面和配置目录中自动发现，也可以不导入。")
        }
    }

    /// remote 列表之外的当前值（例如命令行设置的逗号列表）也保留为可选项。
    private var remoteChoices: [String] {
        var choices = snapshot.profile.remotes
        let current = settings.remote
        if !current.isEmpty && !choices.contains(current) {
            choices.append(current)
        }
        return choices
    }

    private func list(_ values: [String]) -> String {
        values.joined(separator: ", ")
    }

    /// 留空时显示实际生效的自动值，例如「自动：10.0.0.0/8（来自 .ovpn）」。
    private func autoPrompt(_ values: [String], auto: Bool, fallback: String) -> String {
        guard auto, !values.isEmpty else { return fallback }
        // .ovpn 里没有内网网段时用内置的 10.0.0.0/8。
        let source = values == ["10.0.0.0/8"] ? "默认" : "来自 .ovpn"
        return "自动：\(list(values))（\(source)）"
    }

    private func chooseOVPN() {
        guard let path = OVPNFile.choose() else { return }
        model.updateSettings(["ovpnPath": path])
    }
}

// MARK: Tailscale

/// Mihomo 内置的 Tailscale 节点。Auth Key 不回传到界面，只显示脱敏后的样子；
/// 登录过之后登录状态保存在配置目录，不再需要 Auth Key。
private struct TailscaleSection: View {
    @EnvironmentObject private var model: AppModel
    let settings: EngineSettings

    var body: some View {
        Section {
            Toggle("开启 Tailscale", isOn: Binding(get: { settings.tailscaleEnabled }, set: { value in
                model.updateSettings(["tailscaleEnabled": value]) { $0.tailscaleEnabled = value }
            }))
            if settings.tailscaleEnabled {
                SecretField(label: "Auth Key", prompt: authKeyPrompt) { value in
                    await model.saveSettings(["tailscaleAuthKey": value])
                }
                if settings.tailscaleHasAuthKey {
                    HStack {
                        Text("已保存 \(settings.tailscaleAuthKey)")
                            .foregroundStyle(.secondary)
                        Spacer()
                        Button("清除") {
                            model.updateSettings(["tailscaleAuthKey": ""]) {
                                $0.tailscaleAuthKey = ""
                                $0.tailscaleHasAuthKey = false
                            }
                        }
                    }
                }
                if !settings.tailscaleLoggedIn && !settings.tailscaleHasAuthKey {
                    Text("首次登录需要 Auth Key：在 Tailscale 管理后台的 Settings → Keys 生成。")
                        .font(.callout)
                        .foregroundStyle(.orange)
                }
                SettingField(label: "设备名", value: settings.tailscaleHostname,
                             prompt: settings.tailscaleDeviceName.isEmpty ? "可选" : "默认 \(settings.tailscaleDeviceName)") { value in
                    model.updateSettings(["tailscaleHostname": value]) { $0.tailscaleHostname = value }
                }
                SettingField(label: "控制服务器", value: settings.tailscaleControlURL,
                             prompt: "官方服务器；使用 Headscale 时填写地址") { value in
                    model.updateSettings(["tailscaleControlURL": value]) { $0.tailscaleControlURL = value }
                }
                SettingField(label: "出口节点", value: settings.tailscaleExitNode,
                             prompt: "可选：经这台设备上网") { value in
                    model.updateSettings(["tailscaleExitNode": value]) { $0.tailscaleExitNode = value }
                }
            }
        } header: {
            SectionTitle(title: "Tailscale（可选）", symbol: RouteKind.tailscale.symbol, color: RouteKind.tailscale.color)
        } footer: {
            FormNote("开启后可以访问 tailnet 里的设备（100.64.0.0/10 和 *.ts.net）。登录一次后不再需要 Auth Key；设置出口节点后，「外网代理」里多一个 tailscale 选项。")
        }
    }

    private var authKeyPrompt: String {
        if settings.tailscaleLoggedIn { return "已登录，不需要 Auth Key" }
        if settings.tailscaleHasAuthKey { return "已保存；不修改可留空" }
        return "tskey-auth-…"
    }
}

/// 单行文本设置：回车或离开输入框时保存，与 PortField 一致。value 是已保存
/// 的值，prompt 是留空时显示的提示（例如自动值）；清空后保存表示恢复自动。
private struct SettingField: View {
    let label: String
    let value: String
    var prompt = ""
    let commit: (String) -> Void

    @State private var text = ""
    @FocusState private var focused: Bool

    var body: some View {
        TextField(label, text: $text, prompt: Text(prompt))
            .focused($focused)
            .onSubmit(save)
            .onAppear { text = value }
            .onChange(of: value) { newValue in
                if !focused { text = newValue }
            }
            .onChange(of: focused) { isFocused in
                if !isFocused { save() }
            }
    }

    private func save() {
        let trimmed = text.trimmingCharacters(in: .whitespacesAndNewlines)
        if trimmed != value { commit(trimmed) }
    }
}

/// 密钥输入框：已保存的值不回传，留空表示不修改；保存成功后清空。
private struct SecretField: View {
    let label: String
    let prompt: String
    let commit: (String) async -> Bool

    @State private var text = ""
    @State private var saving = false
    @FocusState private var focused: Bool

    var body: some View {
        SecureField(label, text: $text, prompt: Text(prompt))
            .focused($focused)
            .onSubmit(save)
            .onChange(of: focused) { isFocused in
                if !isFocused { save() }
            }
    }

    private func save() {
        let trimmed = text.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !saving, !trimmed.isEmpty else { return }
        saving = true
        Task {
            if await commit(trimmed) { text = "" }
            saving = false
        }
    }
}

private struct PortField: View {
    let label: String
    let value: Int
    let commit: (Int) -> Void

    @State private var text = ""
    @State private var invalid = false
    @FocusState private var focused: Bool

    var body: some View {
        LabeledContent(label) {
            HStack(spacing: 8) {
                if invalid {
                    Text("1–65535").font(.caption).foregroundStyle(.orange)
                }
                TextField(label, text: $text)
                    .labelsHidden()
                    .multilineTextAlignment(.trailing)
                    .frame(width: 90)
                    .focused($focused)
                    .onSubmit(save)
            }
        }
        .onAppear { text = String(value) }
        .onChange(of: value) { newValue in
            if !focused { text = String(newValue) }
        }
        .onChange(of: focused) { isFocused in
            if !isFocused { save() }
        }
    }

    private func save() {
        guard let port = Int(text.trimmingCharacters(in: .whitespaces)), (1...65535).contains(port) else {
            invalid = true
            return
        }
        invalid = false
        if port != value { commit(port) }
    }
}

/// 住宅节点与 Claude 访问监测。住宅节点按项填写，协议从下拉框选；密码不会
/// 回传到界面，修改时留空表示沿用已保存的密码。
private struct ClaudeSection: View {
    @EnvironmentObject private var model: AppModel
    let settings: EngineSettings

    @State private var draft = NodeDraft()
    @State private var saving = false
    /// 粘贴的整段地址认不出来时的说明。
    @State private var pasteError: String?

    private var hasNode: Bool { settings.hasResidentialNode }
    private var direct: Bool { settings.claudeProxyDirect ?? false }

    var body: some View {
        Section {
            if settings.claudeProxyInFile == true {
                LabeledContent("住宅节点") {
                    Text("写在 claude-proxy.yaml").foregroundStyle(.secondary)
                }
            } else {
                nodeFields
            }
            if let error = settings.claudeProxyError {
                Text(error).font(.callout).foregroundStyle(.orange)
            }
            Picker("连接方式", selection: Binding<Bool>(
                get: { direct || draft.isLocal || !settings.canChain },
                set: { value in setDirect(value) }
            )) {
                Text("经前置代理链式连接").tag(false)
                Text("本机直接连接").tag(true)
            }
            .disabled((!hasNode && draft.trimmedHost.isEmpty) || draft.isLocal || !settings.canChain)
            if draft.isLocal {
                Text("本机地址（例如 SSH 端口转发）只能直接连接：前置代理连不到这台电脑。")
                    .font(.callout)
                    .foregroundStyle(.secondary)
            } else if !settings.canChain && (hasNode || !draft.trimmedHost.isEmpty) {
                Text("还没有可做前置代理的节点（订阅或节点文件），住宅节点会直接连接。")
                    .font(.callout)
                    .foregroundStyle(.secondary)
            }
            Toggle("Claude 访问监测", isOn: Binding<Bool>(
                get: { hasNode || settings.claudeGuard == true },
                set: { value in model.updateSettings(["claudeGuard": value]) { $0.claudeGuard = value; $0.claudeGuardActive = value } }
            ))
            .disabled(hasNode)
            if settings.claudeBlockSupported == true {
                Toggle(isOn: Binding<Bool>(
                    get: { settings.claudeBlock ?? false },
                    set: { value in Task { await model.setClaudeBlock(value) } }
                )) {
                    VStack(alignment: .leading, spacing: 2) {
                        Text("直连保护")
                        Text("用系统防火墙拦截不经 Tizi 的 Claude 连接；Tizi 断开、崩溃或开机后还没连接时，Claude 打不开，不会直连。开关时需要输入密码。")
                            .font(.caption)
                            .foregroundStyle(.secondary)
                            .fixedSize(horizontal: false, vertical: true)
                    }
                }
                .disabled(model.busyLabel != nil)
            }
            HStack {
                Button("编辑 claude-proxy.yaml…") { Task { await model.openClaudeProxyFile() } }
                    .help("ss、trojan、vless 等其他协议的节点写在这个文件里")
                Spacer()
                Button("立即检测") { model.checkClaude() }
                    .disabled(!(settings.claudeGuardActive ?? false))
            }
        } header: {
            SectionTitle(title: "Claude", symbol: RouteKind.claude.symbol, color: RouteKind.claude.color)
        } footer: {
            FormNote("""
            claude.ai、anthropic.com 等域名固定走「Claude」策略组，不会直连。配置住宅节点后经住宅节点访问，节点不可用时 Claude 打不开并弹窗提醒，不会自动换成其他节点。\
            链式连接先经「前置代理」（订阅或节点文件里的节点）再连住宅节点，适合从国内直连不稳定的节点；本机地址（例如用 SSH 把节点转发到 127.0.0.1）总是直接连接。服务商给的整段地址可以直接粘贴到「主机」，会自动识别并拆开，用户名密码在前（socks5://用户:密码@主机:端口）或在后（主机:端口:用户:密码）都可以。\
            ss、trojan、vless 等其他协议写进 claude-proxy.yaml（和上面的节点只能设置一个）。修改后重载 Mihomo 生效。
            """)
        }
        .onSubmit { save() }
        .onAppear { draft = NodeDraft(settings.claudeNode) }
        .onChange(of: settings.claudeNode) { node in
            draft = NodeDraft(node)
            pasteError = nil
        }
    }

    @ViewBuilder private var nodeFields: some View {
        Picker("协议", selection: $draft.scheme) {
            Text("SOCKS5").tag("socks5")
            Text("HTTP").tag("http")
            Text("HTTPS（HTTP over TLS）").tag("https")
        }
        TextField("主机", text: Binding(get: { draft.host }, set: { setHost($0) }), prompt: Text("域名或 IP，可粘贴整段地址"))
        if let pasteError {
            Text(pasteError).font(.callout).foregroundStyle(.orange)
        }
        TextField("端口", text: $draft.port, prompt: Text("1–65535"))
        if let warning = portWarning {
            Text(warning).font(.callout).foregroundStyle(.orange)
        }
        TextField("用户名", text: $draft.username, prompt: Text("没有可不填"))
        SecureField("密码", text: $draft.password,
                    prompt: Text(settings.claudeNode?.hasPassword == true ? "已保存；不修改可留空" : "没有可不填"))
        HStack {
            Text(nodeStatus)
                .foregroundStyle(.secondary)
                .lineLimit(1)
                .truncationMode(.middle)
                .textSelection(.enabled)
            Spacer()
            if hasNode {
                Button("清除") { clear() }
            }
            Button("保存") { save() }
                .disabled(!canSave)
        }
    }

    private var portWarning: String? {
        let text = draft.port.trimmingCharacters(in: .whitespaces)
        guard !text.isEmpty else { return nil }
        return draft.portValue == nil ? "端口须是 1–65535 之间的整数。" : nil
    }

    private var nodeStatus: String {
        if draft.differs(from: settings.claudeNode) { return hasNode ? "有修改，尚未保存" : "填好后点「保存」" }
        if let proxy = settings.claudeProxy, !proxy.isEmpty { return proxy }
        return "未设置，Claude 走代理节点"
    }

    private var canSave: Bool {
        !saving && draft.differs(from: settings.claudeNode) && !draft.trimmedHost.isEmpty && draft.portValue != nil
    }

    /// 改连接方式时，表单里还没保存的节点一起保存。只存连接方式的话，看起来
    /// 设好了，节点地址却还是旧的（或者根本没有）。
    private func setDirect(_ value: Bool) {
        if canSave {
            save(direct: value)
        } else {
            model.updateSettings(["claudeProxyDirect": value]) { $0.claudeProxyDirect = value }
        }
    }

    private func save(direct: Bool? = nil) {
        guard canSave, let port = draft.portValue else { return }
        let password = draft.password.isEmpty && !draft.replacesPassword ? nil : draft.password
        saving = true
        Task {
            if await model.saveClaudeNode(scheme: draft.scheme, host: draft.trimmedHost, port: port,
                                          username: draft.trimmedUsername, password: password, direct: direct) {
                draft.password = ""
            }
            saving = false
        }
    }

    /// 一次粘贴进整段地址时交给引擎识别格式，拆到各项；逐字输入时不拆，免得
    /// 输到一半就被拆开。
    private func setHost(_ value: String) {
        let pasted = value.count - draft.host.count > 1
        draft.host = value
        pasteError = nil
        guard pasted, value.contains(":") || value.contains("@") else { return }
        Task {
            do {
                let node = try await model.parseClaudeAddress(value)
                if draft.host == value { draft.apply(node) }
            } catch {
                if draft.host == value { pasteError = (error as? EngineError)?.message ?? error.localizedDescription }
            }
        }
    }

    private func clear() {
        model.updateSettings(["claudeProxy": ""]) { $0.claudeProxy = ""; $0.claudeNode = nil }
    }
}

/// 住宅节点表单的草稿。端口按文本保存，输入过程中可以暂时无效。
private struct NodeDraft {
    var scheme = "socks5"
    var host = ""
    var port = ""
    var username = ""
    var password = ""
    /// 粘贴了整段地址：保存时连密码一起替换，地址里没有密码就清掉原密码。
    var replacesPassword = false

    init() {}

    init(_ node: ClaudeNode?) {
        guard let node else { return }
        scheme = node.scheme
        host = node.host
        port = String(node.port)
        username = node.username ?? ""
    }

    var trimmedHost: String { host.trimmingCharacters(in: .whitespaces) }

    /// 主机是不是本机地址（127.0.0.0/8、::1、localhost），与引擎的 claude.IsLocal 一致。
    var isLocal: Bool {
        var host = trimmedHost.lowercased()
        if host.hasSuffix(".") { host.removeLast() }
        if host.hasPrefix("[") && host.hasSuffix("]") { host = String(host.dropFirst().dropLast()) }
        if host == "localhost" || host.hasSuffix(".localhost") { return true }
        if let ip = IPv4Address(host) { return ip.isLoopback }
        if let ip = IPv6Address(host) { return ip.isLoopback }
        return false
    }
    var trimmedUsername: String { username.trimmingCharacters(in: .whitespaces) }

    var portValue: Int? {
        guard let port = Int(port.trimmingCharacters(in: .whitespaces)), (1...65535).contains(port) else { return nil }
        return port
    }

    /// 和已保存的节点比有没有改动；填了新密码总算改动。
    func differs(from node: ClaudeNode?) -> Bool {
        if !password.isEmpty || replacesPassword { return true }
        guard let node else { return !trimmedHost.isEmpty || !port.isEmpty || !trimmedUsername.isEmpty }
        return scheme != node.scheme || trimmedHost != node.host || portValue != node.port
            || trimmedUsername != (node.username ?? "")
    }

    /// 用粘贴识别出的各项替换草稿；地址里没有的项（协议、端口、用户名和
    /// 密码）保持原样。
    mutating func apply(_ node: ParsedClaudeNode) {
        if let scheme = node.scheme, !scheme.isEmpty { self.scheme = scheme }
        host = node.host
        if let port = node.port { self.port = String(port) }
        if let username = node.username, !username.isEmpty {
            self.username = username
            password = node.password ?? ""
            replacesPassword = true
        }
    }
}

/// 局域网代理认证：开启后其他设备连代理端口需要用户名和密码。密码由引擎
/// 生成并保存，界面只在点「显示」或「复制」时取一次。
private struct ProxyAuthRows: View {
    @EnvironmentObject private var model: AppModel
    let settings: EngineSettings

    @State private var password: String?

    var body: some View {
        Toggle("要求用户名和密码", isOn: Binding(get: { settings.proxyAuth }, set: { enabled in
            password = nil
            if enabled {
                let username = settings.proxyUsername.isEmpty ? "tizi" : settings.proxyUsername
                model.updateSettings(["proxyAuth": true, "proxyUsername": username]) {
                    $0.proxyAuth = true
                    $0.proxyUsername = username
                }
            } else {
                model.updateSettings(["proxyAuth": false]) {
                    $0.proxyAuth = false
                    $0.proxyUsername = ""
                }
            }
        }))
        if settings.proxyAuth {
            SettingField(label: "用户名", value: settings.proxyUsername, prompt: "不能含冒号或空格") { value in
                model.updateSettings(["proxyUsername": value]) { $0.proxyUsername = value }
            }
            LabeledContent("密码") {
                HStack(spacing: 8) {
                    Text(password ?? "••••••••••••")
                        .font(.callout.monospaced())
                        .textSelection(.enabled)
                        .lineLimit(1)
                    Button(password == nil ? "显示" : "隐藏") {
                        if password == nil {
                            Task { password = await model.proxyCredentials()?.password }
                        } else {
                            password = nil
                        }
                    }
                    Button("复制") {
                        Task {
                            guard let credentials = await model.proxyCredentials() else { return }
                            NSPasteboard.general.clearContents()
                            NSPasteboard.general.setString(credentials.password, forType: .string)
                            model.show("密码已复制")
                        }
                    }
                    Button("重新生成") {
                        password = nil
                        model.updateSettings(["proxyPassword": ""])
                    }
                    .help("生成新密码；已经配置过的设备需要改用新密码")
                }
            }
        } else {
            Text("局域网里的任何设备都能使用这个代理端口，只应在可信网络中打开；在有公网地址的电脑上请开启「要求用户名和密码」。")
                .font(.callout)
                .foregroundStyle(.orange)
                .fixedSize(horizontal: false, vertical: true)
        }
    }
}
