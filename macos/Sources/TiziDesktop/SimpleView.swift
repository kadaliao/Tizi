import AppKit
import SwiftUI

/// 简洁模式：设置一次线路（订阅地址、节点文件或内网 .ovpn），之后只有「连接 /
/// 断开」。不出现 Mihomo、TUN、策略组等术语；需要这些时从右下角进入高级模式。
struct SimpleView: View {
    @EnvironmentObject private var model: AppModel
    @State private var dropTargeted = false
    @State private var editingSubscription = false

    var body: some View {
        VStack(spacing: 0) {
            if let notice = model.notice {
                NoticeBar(notice: notice)
            }
            content
                .frame(maxWidth: .infinity, maxHeight: .infinity)
            Divider()
            footer
        }
        // 整个窗口都可以接收拖进来的 .ovpn，首次设置和日后更换都适用。
        .dropDestination(for: URL.self) { urls, _ in
            guard let url = urls.first(where: { $0.pathExtension.lowercased() == "ovpn" }) else {
                model.show("请拖入以 .ovpn 结尾的内网配置文件", error: true)
                return false
            }
            Task { await model.useOVPN(url.path) }
            return true
        } isTargeted: { dropTargeted = $0 }
        .overlay {
            if dropTargeted {
                RoundedRectangle(cornerRadius: 10)
                    .strokeBorder(Color.accentColor, style: StrokeStyle(lineWidth: 3, dash: [8, 6]))
                    .padding(6)
                    .allowsHitTesting(false)
            }
        }
        .sheet(isPresented: $editingSubscription) {
            if let settings = model.snapshot?.settings {
                SubscriptionSheet(settings: settings)
            }
        }
    }

    @ViewBuilder
    private var content: some View {
        switch model.engineState {
        case .starting:
            ProgressView("正在启动…")
        case .running:
            if let snapshot = model.snapshot {
                if needsSetup(snapshot) {
                    SetupPanel()
                } else {
                    ConnectPanel(snapshot: snapshot)
                }
            } else {
                ProgressView()
            }
        default:
            ScrollView { EngineStateView() }
        }
    }

    /// 还没有任何线路时先引导设置。
    private func needsSetup(_ snapshot: Snapshot) -> Bool {
        !snapshot.managed.running && !snapshot.settings.ready
    }

    private var configured: Bool {
        guard model.engineState == .running, let snapshot = model.snapshot else { return false }
        return !needsSetup(snapshot)
    }

    private var footer: some View {
        HStack(spacing: 14) {
            if configured, let settings = model.snapshot?.settings {
                routesMenu(settings)
                    .fixedSize()
            }
            Spacer()
            if configured {
                troubleMenu
                    .fixedSize()
            }
            Button("高级模式") { model.mode = .advanced }
                .foregroundStyle(.primary)
                .help("查看连接详情、分流规则、日志和诊断，设置 Tailscale")
        }
        .menuStyle(.borderlessButton)
        .buttonStyle(.borderless)
        .font(.callout)
        .padding(.horizontal, 14)
        .padding(.vertical, 10)
    }

    /// 更换订阅、编辑节点文件、导入或移除内网 .ovpn。连接中改动会自动重新连接。
    private func routesMenu(_ settings: EngineSettings) -> some View {
        let busy = model.busyLabel != nil
        return Menu {
            Button(settings.hasSubscription ? "更换订阅地址…" : "添加订阅地址…") { editingSubscription = true }
            Button("编辑节点文件…") { Task { await model.openProxiesFile() } }
            Divider()
            Button(settings.hasOVPN ? "更换内网 .ovpn…" : "导入内网 .ovpn…") {
                if let path = OVPNFile.choose() { Task { await model.useOVPN(path) } }
            }
            if settings.hasOVPN {
                Button("移除内网 .ovpn") { Task { await model.useOVPN("") } }
                    .disabled(settings.routeCount <= 1)
                Button("在访达中显示 .ovpn") {
                    NSWorkspace.shared.activateFileViewerSelecting([URL(fileURLWithPath: settings.ovpnPath)])
                }
            }
        } label: {
            Label("线路设置", systemImage: "network")
        }
        .menuIndicator(.hidden)
        .disabled(busy)
        .help("订阅地址、节点文件和内网 .ovpn")
    }

    private var troubleMenu: some View {
        let busy = model.busyLabel != nil
        return Menu("遇到问题") {
            Button("网页打不开：重新连接") { Task { await model.reload() } }
                .disabled(!model.running || busy)
            Button("微信图片发不出去") { Task { await model.recoverWeChat() } }
                .disabled(model.snapshot?.controllerOnline != true || busy)
            Divider()
            Button("查看诊断详情…") {
                model.page = .diagnostics
                model.mode = .advanced
            }
            Button(model.checkingUpdate ? "正在检查更新…" : "检查更新…") { Task { await model.checkForUpdates() } }
                .disabled(model.checkingUpdate)
        }
    }
}

// MARK: 首次设置

/// 还没有任何线路时的引导：以订阅地址为主，节点文件和内网 .ovpn 是次要入口，
/// Tailscale 在高级模式里设置。
private struct SetupPanel: View {
    @EnvironmentObject private var model: AppModel
    @State private var subscription = ""
    @State private var saving = false

    private var trimmed: String { subscription.trimmingCharacters(in: .whitespacesAndNewlines) }

    var body: some View {
        VStack(spacing: 12) {
            Spacer(minLength: 4)
            Image(systemName: "link.badge.plus")
                .font(.system(size: 40))
                .foregroundStyle(Color.accentColor)
            Text("先添加代理节点")
                .font(.title2.weight(.semibold))
            Text("粘贴机场给你的订阅地址，只需要设置一次。")
                .font(.callout)
                .foregroundStyle(.secondary)
                .multilineTextAlignment(.center)
                .fixedSize(horizontal: false, vertical: true)
            TextField("订阅地址", text: $subscription, prompt: Text("https://…"))
                .labelsHidden()
                .textFieldStyle(.roundedBorder)
                .onSubmit(save)
                .padding(.top, 4)
            Button(action: save) {
                Text("保存").frame(width: 120)
            }
            .buttonStyle(.borderedProminent)
            .controlSize(.large)
            .keyboardShortcut(.defaultAction)
            .disabled(saving || trimmed.isEmpty)

            HStack(spacing: 8) {
                VStack { Divider() }
                Text("或者").font(.caption).foregroundStyle(.secondary)
                VStack { Divider() }
            }
            .padding(.top, 6)
            VStack(spacing: 8) {
                Button("编辑节点文件…") { Task { await model.openProxiesFile() } }
                    .help("自己的节点：每行一个分享链接（ss://、trojan:// 等），或 Mihomo 节点格式")
                ovpnButton
            }
            .buttonStyle(.link)
            Text("Tailscale 在「高级模式 → 配置」里设置。也可以把 .ovpn 文件直接拖进这个窗口。")
                .font(.caption)
                .foregroundStyle(.secondary)
                .multilineTextAlignment(.center)
                .fixedSize(horizontal: false, vertical: true)
            Spacer(minLength: 4)
        }
        .padding(.horizontal, 28)
        .padding(.vertical, 16)
        .disabled(model.busyLabel != nil)
        .task { await model.scanOVPN(quiet: true) }
    }

    /// 在下载、桌面等位置找到了 .ovpn 时列出来直接选，否则打开选择框。
    @ViewBuilder
    private var ovpnButton: some View {
        let title = "导入 .ovpn（可选，连接内网）"
        if model.scannedOVPN.isEmpty {
            Button(title, action: chooseOVPN)
        } else {
            Menu(title) {
                ForEach(model.scannedOVPN.prefix(5), id: \.self) { path in
                    Button(Self.label(for: path)) { Task { await model.useOVPN(path) } }
                }
                Divider()
                Button("选择其他文件…", action: chooseOVPN)
            }
            .menuStyle(.borderlessButton)
            .menuIndicator(.hidden)
            .foregroundStyle(Color.accentColor)
            .fixedSize()
        }
    }

    private func save() {
        guard !saving, !trimmed.isEmpty else { return }
        saving = true
        Task {
            if await model.setSubscription(trimmed) { subscription = "" }
            saving = false
        }
    }

    private func chooseOVPN() {
        if let path = OVPNFile.choose() { Task { await model.useOVPN(path) } }
    }

    /// 「文件名（所在位置）」，下载、桌面用中文名。
    private static func label(for path: String) -> String {
        let directory = (path as NSString).deletingLastPathComponent
        let home = FileManager.default.homeDirectoryForCurrentUser.path
        let folder: String
        switch directory {
        case home + "/Downloads": folder = "下载"
        case home + "/Desktop": folder = "桌面"
        default: folder = (directory as NSString).abbreviatingWithTildeInPath
        }
        return "\((path as NSString).lastPathComponent)（\(folder)）"
    }
}

/// 添加或更换订阅地址。完整的旧地址不回传到界面，只显示脱敏后的样子。
private struct SubscriptionSheet: View {
    @EnvironmentObject private var model: AppModel
    @Environment(\.dismiss) private var dismiss
    let settings: EngineSettings

    @State private var draft = ""
    @State private var saving = false

    private var trimmed: String { draft.trimmingCharacters(in: .whitespacesAndNewlines) }

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            Text(settings.hasSubscription ? "更换订阅地址" : "添加订阅地址")
                .font(.headline)
            if settings.hasSubscription {
                Text("当前：\(settings.subscription)")
                    .font(.callout)
                    .foregroundStyle(.secondary)
                    .lineLimit(1)
                    .truncationMode(.middle)
            }
            TextField("订阅地址", text: $draft, prompt: Text("粘贴机场订阅地址，https://…"))
                .labelsHidden()
                .textFieldStyle(.roundedBorder)
            if model.running {
                Text("保存后会自动重新连接。")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
            HStack {
                if settings.hasSubscription {
                    Button("清除订阅") { submit("") }
                        .disabled(saving || settings.routeCount <= 1)
                        .help(settings.routeCount <= 1 ? "这是唯一的线路，清除后就无法连接了" : "不再使用这个订阅")
                }
                Spacer()
                Button("取消") { dismiss() }
                    .keyboardShortcut(.cancelAction)
                Button("保存") { submit(trimmed) }
                    .keyboardShortcut(.defaultAction)
                    .disabled(saving || trimmed.isEmpty)
            }
        }
        .padding(20)
        .frame(width: 340)
    }

    private func submit(_ value: String) {
        saving = true
        Task {
            if await model.setSubscription(value) { dismiss() }
            saving = false
        }
    }
}

// MARK: 连接

private struct ConnectPanel: View {
    @EnvironmentObject private var model: AppModel
    let snapshot: Snapshot

    private var running: Bool { snapshot.managed.running }
    private var busy: Bool { model.busyLabel != nil }

    var body: some View {
        VStack(spacing: 0) {
            Spacer(minLength: 16)
            StatusBadge(running: running, busy: busy)
            Text(model.statusTitle)
                .font(.title.weight(.semibold))
                .padding(.top, 18)
            Text(subtitle)
                .font(.callout)
                .foregroundStyle(.secondary)
                .multilineTextAlignment(.center)
                .frame(maxWidth: 290)
                .fixedSize(horizontal: false, vertical: true)
                .padding(.top, 6)
            mainButton
                .controlSize(.large)
                .disabled(busy)
                .padding(.top, 22)
            detail
                .frame(height: 34)
                .padding(.top, 12)
            Spacer(minLength: 8)
            banners
        }
        .padding(.horizontal, 20)
        .padding(.vertical, 16)
    }

    private var subtitle: String {
        switch model.busyLabel {
        case "启动 Mihomo"?: return "如果弹出密码框，请输入这台 Mac 的登录密码"
        case "安装 Mihomo 核心"?: return "第一次使用需要下载必要组件，请稍候"
        case .some: return "请稍候"
        case nil:
            let routes = snapshot.settings.routeSummary.joined(separator: " · ")
            if routes.isEmpty { return running ? "可以正常上网了" : "点「连接」开始使用" }
            return running ? "正在使用：\(routes)" : "已设置：\(routes)"
        }
    }

    @ViewBuilder
    private var mainButton: some View {
        if running {
            Button { Task { await model.stop() } } label: { buttonLabel("断开") }
                .buttonStyle(.bordered)
        } else {
            // 回车只用来连接；断开保持普通按钮，避免误触。
            Button { Task { await model.start() } } label: { buttonLabel("连接") }
                .buttonStyle(.borderedProminent)
                .keyboardShortcut(.defaultAction)
        }
    }

    private func buttonLabel(_ title: String) -> some View {
        Text(title)
            .font(.title3.weight(.medium))
            .frame(width: 150)
            .padding(.vertical, 3)
    }

    /// 下载组件时显示进度，已连接时显示实时网速。
    @ViewBuilder
    private var detail: some View {
        if let install = model.install ?? snapshot.install {
            if let fraction = install.fraction {
                ProgressView(value: fraction)
                    .frame(width: 200)
            } else {
                ProgressView().controlSize(.small)
            }
        } else if running {
            HStack(spacing: 18) {
                Label(Format.rate(snapshot.traffic.download), systemImage: "arrow.down")
                Label(Format.rate(snapshot.traffic.upload), systemImage: "arrow.up")
            }
            .font(.callout.monospacedDigit())
            .foregroundStyle(.secondary)
            .help("当前下载和上传速度")
        }
    }

    @ViewBuilder
    private var banners: some View {
        VStack(spacing: 8) {
            if let claude = model.claudeStatus, claude.isAlert || claude.level == "warn" {
                ClaudeStatusRow(status: claude, compact: true)
            }
            if snapshot.settings.hasOVPN, let error = snapshot.profile.error {
                AlertRow(symbol: "exclamationmark.triangle.fill", tint: .orange,
                         text: "内网 .ovpn 用不了：\(error)。可以拖入新的文件替换。") {
                    Button("移除") { Task { await model.useOVPN("") } }
                        .disabled(busy || snapshot.settings.routeCount <= 1)
                }
            }
            if let error = snapshot.settings.nodesError, !error.isEmpty {
                AlertRow(symbol: "exclamationmark.triangle.fill", tint: .orange, text: "节点文件有问题：\(error)") {
                    Button("编辑") { Task { await model.openProxiesFile() } }
                }
            }
            if snapshot.conflicts.any {
                AlertRow(symbol: "exclamationmark.triangle.fill", tint: .orange,
                         text: "另一个 VPN（如 Tunnelblick）正在运行，同时开可能都用不了，建议先断开它。") {
                    EmptyView()
                }
            }
            if snapshot.dnsRecoveryPending && !running {
                AlertRow(symbol: "wrench.adjustable.fill", tint: .orange,
                         text: "上次没有正常断开，网络设置还没恢复。") {
                    Button("修复") { Task { await model.recoverDNS() } }.disabled(busy)
                }
            }
            if let share = model.activeShare, !model.showShare {
                AlertRow(symbol: "qrcode", tint: .orange,
                         text: "手机扫码导入仍开着，\(share.expiry.formatted(date: .omitted, time: .shortened)) 自动关闭。") {
                    Button("关闭") { model.stopShare() }
                }
            }
            if let installed = snapshot.update.installed {
                AlertRow(symbol: "arrow.clockwise.circle.fill", tint: .blue,
                         text: "新版本 \(installed) 已下载，重启后生效。") {
                    Button("立即生效") { model.restartEngine(reload: running) }.disabled(busy)
                }
            } else if snapshot.update.available, let latest = snapshot.update.latest {
                AlertRow(symbol: "arrow.down.circle.fill", tint: .blue, text: "有新版本 \(latest)") {
                    Button("更新") { Task { await model.installUpdate() } }.disabled(busy)
                }
            }
        }
        .font(.callout)
    }
}

private struct StatusBadge: View {
    let running: Bool
    let busy: Bool

    var body: some View {
        let tint: Color = running ? .green : .secondary
        ZStack {
            Circle()
                .fill(tint.opacity(0.10))
                .frame(width: 124, height: 124)
            Circle()
                .fill(tint.opacity(0.16))
                .frame(width: 92, height: 92)
            if busy {
                ProgressView()
                    .controlSize(.large)
            } else {
                Image(systemName: running ? "checkmark.shield.fill" : "shield.slash")
                    .font(.system(size: 44))
                    .foregroundStyle(tint)
            }
        }
        .accessibilityHidden(true)
    }
}
