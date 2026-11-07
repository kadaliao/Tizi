import SwiftUI

struct StatusPage: View {
    @EnvironmentObject private var model: AppModel
    @State private var confirmReinstall = false

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 14) {
                if let snapshot = model.snapshot {
                    hero(snapshot)
                    alerts(snapshot)
                    overview(snapshot)
                    actions(snapshot)
                } else {
                    ProgressView("正在读取本机状态…")
                        .frame(maxWidth: .infinity, minHeight: 200)
                }
            }
            .padding(20)
        }
        .confirmationDialog("Mihomo 正在运行", isPresented: $confirmReinstall, titleVisibility: .visible) {
            Button("继续更新核心") { Task { await model.installCore() } }
            Button("取消", role: .cancel) {}
        } message: {
            Text("核心会被替换为最新版本；运行中的 Mihomo 需要重载后才会换用新核心。")
        }
    }

    private var busy: Bool { model.busyLabel != nil }

    // MARK: 首屏结论

    /// 首屏：梯子标志、连接状态和开关；连接后加上实时网速和路线分布。
    private func hero(_ snapshot: Snapshot) -> some View {
        let running = snapshot.managed.running
        return VStack(alignment: .leading, spacing: 18) {
            HStack(alignment: .center, spacing: 18) {
                LadderMark(phase: busy ? .connecting : (running ? .on : .off), size: 64)
                VStack(alignment: .leading, spacing: 4) {
                    Text(model.statusTitle)
                        .font(.system(size: 22, weight: .semibold))
                    Text(summary(snapshot))
                        .font(.callout)
                        .foregroundStyle(.secondary)
                        .textSelection(.enabled)
                }
                Spacer()
                Button {
                    Task { await model.toggleConnection() }
                } label: {
                    Text(running ? "断开" : "连接")
                }
                .buttonStyle(CapsuleButtonStyle(prominent: !running, width: 112))
                .disabled(busy)
                .keyboardShortcut(running ? nil : .defaultAction)
            }
            if running {
                HStack(alignment: .center, spacing: 28) {
                    SpeedReadout(download: snapshot.traffic.download, upload: snapshot.traffic.upload, size: 22)
                    Text("\(snapshot.traffic.connections) 条连接")
                        .font(.callout.monospacedDigit())
                        .foregroundStyle(.secondary)
                    Spacer()
                    Button("查看连接") { model.page = .connections }
                        .buttonStyle(.link)
                        .font(.callout)
                }
                RouteMix(connections: snapshot.connections, legendAlignment: .leading)
            }
        }
        .panel(radius: 16, padding: 20)
    }

    private func summary(_ snapshot: Snapshot) -> String {
        let settings = snapshot.settings
        var text = settings.tunEnabled ? "接管全部流量（TUN）" : "只提供本地代理端口"
        if let pid = snapshot.managed.pid, snapshot.managed.running {
            text += "，Mihomo 进程 \(pid)"
        }
        return text
    }

    // MARK: 需要处理的问题

    @ViewBuilder
    private func alerts(_ snapshot: Snapshot) -> some View {
        // 正常时由下面的 Claude 面板说明，这里只提示需要处理的情况。
        if let claude = model.claudeStatus, claude.level != "off", claude.level != "ok" || claude.rechecking {
            ClaudeStatusRow(status: claude)
        }
        if !snapshot.settings.ready {
            AlertRow(symbol: "link.badge.plus", tint: .orange,
                     text: "还没有设置线路：填写订阅地址、编辑节点文件、导入 .ovpn 或开启 Tailscale，任选一种即可。") {
                Button("去配置") { model.page = .config }
            }
        }
        if snapshot.settings.hasOVPN, let error = snapshot.profile.error {
            AlertRow(symbol: "exclamationmark.triangle.fill", tint: .orange, text: "内网 .ovpn 无法使用：\(error)") {
                Button("去配置") { model.page = .config }
            }
        }
        if let error = snapshot.settings.nodesError, !error.isEmpty {
            AlertRow(symbol: "exclamationmark.triangle.fill", tint: .orange, text: "节点文件有问题：\(error)") {
                Button("编辑") { Task { await model.openProxiesFile() } }
            }
        }
        if !snapshot.managed.coreInstalled {
            AlertRow(symbol: "shippingbox", tint: .orange,
                     text: "尚未安装 Mihomo 核心。安装到当前用户目录，不需要管理员权限。") {
                Button("安装核心") { Task { await model.installCore() } }.disabled(busy)
            }
        }
        if let install = model.install ?? snapshot.install {
            VStack(alignment: .leading, spacing: 6) {
                if let fraction = install.fraction {
                    ProgressView(value: fraction) {
                        Text(install.label)
                    } currentValueLabel: {
                        Text("\(Format.bytes(install.downloaded)) / \(Format.bytes(install.total))")
                    }
                } else {
                    ProgressView(install.label)
                }
            }
            .padding(.horizontal, 4)
        }
        if snapshot.conflicts.tunnelblick > 0 || snapshot.conflicts.openvpn > 0 {
            AlertRow(symbol: "exclamationmark.triangle.fill", tint: .orange,
                     text: snapshot.conflicts.tunnelblick > 0
                        ? "检测到 Tunnelblick（\(snapshot.conflicts.tunnelblick) 个 OpenVPN 进程）。请先断开它，再由本工具启动 TUN。"
                        : "检测到 \(snapshot.conflicts.openvpn) 个其他 OpenVPN 进程；同时接管网络可能导致内网或互联网不可用。") {
                EmptyView()
            }
        }
        if let share = model.activeShare, !model.showShare {
            AlertRow(symbol: "qrcode", tint: .orange,
                     text: "Clash Mi 配置分享进行中，\(share.expiry.formatted(date: .omitted, time: .standard)) 自动关闭。局域网内的设备可以下载含节点密码和私钥的配置。") {
                HStack {
                    Button("显示二维码") { model.showShare = true }
                    Button("关闭分享") { model.stopShare() }
                }
            }
        }
        if snapshot.dnsRecoveryPending {
            AlertRow(symbol: "arrow.uturn.backward.circle", tint: .orange,
                     text: "上次运行遗留的系统 DNS 设置尚未恢复。") {
                Button("恢复 DNS") { Task { await model.recoverDNS() } }.disabled(busy)
            }
        }
        if let installed = snapshot.update.installed {
            AlertRow(symbol: "arrow.clockwise.circle.fill", tint: .blue,
                     text: snapshot.managed.running
                        ? "APP 已更新到 \(installed)。重启后台引擎并重载 Mihomo 后，新版本的规则和修复才会生效。"
                        : "APP 已更新到 \(installed)。重启后台引擎后使用新版本。") {
                Button(snapshot.managed.running ? "重启引擎并重载" : "重启引擎") {
                    model.restartEngine(reload: snapshot.managed.running)
                }
                .disabled(busy)
            }
        } else if snapshot.update.available, let latest = snapshot.update.latest {
            AlertRow(symbol: "arrow.down.circle.fill", tint: .blue,
                     text: "发现新版本 \(latest)（当前 \(model.hello?.version ?? "未知")）。") {
                Button("更新 APP") { Task { await model.installUpdate() } }.disabled(busy)
            }
        }
    }

    // MARK: 概览

    /// 线路、Claude 和本机三块信息：每块只回答一个问题。
    private func overview(_ snapshot: Snapshot) -> some View {
        let settings = snapshot.settings
        return VStack(spacing: 14) {
            HStack(alignment: .top, spacing: 14) {
                InfoPanel(title: "线路", action: ("修改", { model.page = .config })) {
                    InfoRow(label: "代理节点", value: proxyNodes(settings), kind: settings.hasSubscription || settings.nodeCount > 0 ? .proxy : nil)
                    InfoRow(label: "内网", value: settings.hasOVPN ? intranet(settings) : "未导入", kind: settings.hasOVPN ? .intranet : nil)
                    InfoRow(label: "Tailscale", value: tailscale(settings), kind: settings.tailscaleEnabled ? .tailscale : nil)
                    InfoRow(label: "国内流量", value: settings.chinaDirect ? "直连" : "经外网代理", kind: settings.chinaDirect ? .direct : .proxy)
                }
                InfoPanel(title: "Claude", action: model.claudeStatus.map { _ in ("检测", { model.checkClaude() }) }) {
                    InfoRow(label: "线路", value: claudeRoute(settings), kind: .claude)
                    InfoRow(label: "出口 IP", value: model.claudeStatus?.exitText ?? (snapshot.managed.running ? "检测中…" : "未连接"))
                    InfoRow(label: "访问监测", value: settings.claudeGuardActive == true ? "开启" : "关闭")
                    if settings.claudeBlockSupported == true {
                        InfoRow(label: "直连保护", value: settings.claudeBlock == true ? "开启" : "关闭")
                    }
                }
            }
            .fixedSize(horizontal: false, vertical: true)
            InfoPanel(title: "本机", action: (model.checkingUpdate ? "正在检查更新…" : "检查更新", {
                Task { await model.checkForUpdates() }
            })) {
                Grid(alignment: .leadingFirstTextBaseline, horizontalSpacing: 24, verticalSpacing: 9) {
                    GridRow {
                        InfoRow(label: "本地代理", value: localProxy(settings))
                        InfoRow(label: "Mihomo", value: snapshot.managed.coreVersion ?? (snapshot.managed.coreInstalled ? "正在读取…" : "未安装"))
                    }
                    GridRow {
                        InfoRow(label: "Controller", value: snapshot.controllerOnline ? "在线，端口 \(settings.controllerPort)" : "不可访问")
                        InfoRow(label: "桌面版", value: Bundle.main.object(forInfoDictionaryKey: "TiziBuildVersion") as? String ?? "开发构建")
                    }
                    GridRow {
                        InfoRow(label: "APP 版本", value: appVersion(snapshot))
                            .gridCellColumns(2)
                    }
                }
            }
        }
    }

    private func proxyNodes(_ settings: EngineSettings) -> String {
        var parts: [String] = []
        if settings.hasSubscription { parts.append("订阅") }
        if settings.nodeCount > 0 { parts.append("节点文件 \(settings.nodeCount) 个") }
        return parts.isEmpty ? "未设置" : parts.joined(separator: "，")
    }

    private func intranet(_ settings: EngineSettings) -> String {
        let name = (settings.ovpnPath as NSString).lastPathComponent
        return settings.remote.isEmpty ? name : "\(name)（\(settings.remote)）"
    }

    private func tailscale(_ settings: EngineSettings) -> String {
        guard settings.tailscaleEnabled else { return "关闭" }
        if !settings.tailscaleLoggedIn && !settings.tailscaleHasAuthKey { return "首次登录需要 Auth Key" }
        return settings.tailscaleDeviceName.isEmpty ? "开启" : settings.tailscaleDeviceName
    }

    private func claudeRoute(_ settings: EngineSettings) -> String {
        guard settings.hasResidentialNode else { return "代理节点" }
        return settings.claudeProxyDirect == true || !settings.canChain ? "住宅节点，直接连接" : "住宅节点，经前置代理"
    }

    private func localProxy(_ settings: EngineSettings) -> String {
        let address = "\(settings.allowLAN ? "0.0.0.0" : "127.0.0.1"):\(settings.mixedPort)"
        guard settings.allowLAN else { return address }
        return address + (settings.proxyAuth ? "，需要密码" : "，不需要密码")
    }

    private func appVersion(_ snapshot: Snapshot) -> String {
        let current = model.hello?.version ?? "未知"
        if let installed = snapshot.update.installed { return "\(current)（已更新至 \(installed)，重启引擎后生效）" }
        if !snapshot.update.checked || model.checkingUpdate { return "\(current)（正在检查更新…）" }
        if snapshot.update.available, let latest = snapshot.update.latest { return "\(current)（最新 \(latest)）" }
        if snapshot.update.error != nil { return "\(current)（更新检查失败）" }
        guard let checked = snapshot.update.lastChecked else { return "\(current)，已是最新" }
        let day: Date.FormatStyle.DateStyle = Calendar.current.isDateInToday(checked) ? .omitted : .abbreviated
        return "\(current)，已是最新（\(checked.formatted(date: day, time: .shortened)) 检查）"
    }

    // MARK: 操作

    private func actions(_ snapshot: Snapshot) -> some View {
        let online = snapshot.controllerOnline
        return VStack(alignment: .leading, spacing: 10) {
            Text("操作").font(.headline)
            FlowLayout(spacing: 8) {
                ActionButton(title: "重载 Mihomo", symbol: "arrow.clockwise",
                             help: "重新生成配置并重启 Mihomo，让自定义规则和设置修改立即生效") {
                    Task { await model.reload() }
                }
                .disabled(!snapshot.managed.running)
                ActionButton(title: "刷新微信直连", symbol: "message",
                             help: "微信图片反复发送失败时使用；只关闭腾讯直连，不重启 Mihomo") {
                    Task { await model.recoverWeChat() }
                }
                .disabled(!online)
                ActionButton(title: "Mihomo 面板", symbol: "safari", help: "在浏览器里打开 Mihomo 的管理面板") {
                    Task { await model.openDashboard() }
                }
                .disabled(!online)
                ActionButton(title: "Clash Mi 扫码导入", symbol: "qrcode", help: "生成二维码，让手机上的 Clash Mi 导入同样的配置") {
                    Task { await model.startShare() }
                }
                ActionButton(title: "生成并校验配置", symbol: "checkmark.seal", help: "只生成配置并用 Mihomo 校验，不连接") {
                    Task { await model.validate() }
                }
                ActionButton(title: snapshot.managed.coreInstalled ? "更新 Mihomo 核心（\(channelName(snapshot))）" : "安装 Mihomo 核心（\(channelName(snapshot))）",
                             symbol: "shippingbox", help: "从 Mihomo 官方 Release 下载核心") {
                    if snapshot.managed.running {
                        confirmReinstall = true
                    } else {
                        Task { await model.installCore() }
                    }
                }
            }
        }
        .disabled(busy)
    }

    private func channelName(_ snapshot: Snapshot) -> String {
        snapshot.settings.coreChannel == "alpha" ? "Alpha" : "Stable"
    }
}

/// 概览面板：标题、可选的一个操作，下面是若干 InfoRow。
struct InfoPanel<Content: View>: View {
    let title: String
    let action: (String, () -> Void)?
    @ViewBuilder var content: () -> Content

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            HStack {
                Text(title).font(.headline)
                Spacer()
                if let action {
                    Button(action.0, action: action.1)
                        .buttonStyle(.link)
                        .font(.callout)
                }
            }
            VStack(alignment: .leading, spacing: 9) {
                content()
            }
        }
        .panel()
        .frame(maxHeight: .infinity, alignment: .top)
    }
}

/// 概览里的一行：左侧标签，右侧值；值属于某种路线时前面带路线颜色的圆点。
struct InfoRow: View {
    let label: String
    let value: String
    var kind: RouteKind?

    var body: some View {
        HStack(alignment: .firstTextBaseline, spacing: 10) {
            Text(label)
                .foregroundStyle(.secondary)
                .frame(width: 68, alignment: .leading)
            HStack(spacing: 6) {
                if let kind {
                    Circle().fill(kind.color).frame(width: 7, height: 7)
                        .alignmentGuide(.firstTextBaseline) { $0[.bottom] - 1 }
                }
                Text(value)
                    .lineLimit(1)
                    .truncationMode(.middle)
                    .textSelection(.enabled)
                    .help(value)
            }
        }
        .font(.callout)
        .frame(maxWidth: .infinity, alignment: .leading)
    }
}

/// 状态页的操作按钮：图标加文字，统一宽度不一的按钮的视觉重量。
struct ActionButton: View {
    let title: String
    let symbol: String
    var help = ""
    let action: () -> Void

    var body: some View {
        Button(action: action) {
            Label(title, systemImage: symbol)
                .padding(.horizontal, 4)
                .padding(.vertical, 2)
        }
        .controlSize(.large)
        .help(help)
    }
}

/// 需要处理的问题：左侧着色图标，右侧放处理按钮。
struct AlertRow<Accessory: View>: View {
    let symbol: String
    let tint: Color
    let text: String
    @ViewBuilder var accessory: () -> Accessory

    var body: some View {
        HStack(spacing: 12) {
            Image(systemName: symbol)
                .font(.system(size: 15, weight: .semibold))
                .foregroundStyle(tint)
                .frame(width: 22)
            Text(text)
                .frame(maxWidth: .infinity, alignment: .leading)
                .fixedSize(horizontal: false, vertical: true)
                .textSelection(.enabled)
            accessory()
        }
        .padding(.horizontal, 14)
        .padding(.vertical, 11)
        .background(RoundedRectangle(cornerRadius: 10, style: .continuous).fill(tint.opacity(0.10)))
        .overlay(RoundedRectangle(cornerRadius: 10, style: .continuous).strokeBorder(tint.opacity(0.22)))
    }
}
