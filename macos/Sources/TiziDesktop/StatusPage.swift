import SwiftUI

struct StatusPage: View {
    @EnvironmentObject private var model: AppModel
    @State private var confirmReinstall = false

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 16) {
                if let snapshot = model.snapshot {
                    hero(snapshot)
                    alerts(snapshot)
                    details(snapshot)
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

    private func hero(_ snapshot: Snapshot) -> some View {
        let running = snapshot.managed.running
        return HStack(alignment: .center, spacing: 16) {
            ZStack {
                Circle()
                    .fill(running ? Color.green.opacity(0.18) : Color.secondary.opacity(0.12))
                    .frame(width: 56, height: 56)
                if busy {
                    ProgressView().controlSize(.small)
                } else {
                    Image(systemName: running ? "checkmark.shield.fill" : "shield.slash")
                        .font(.system(size: 24))
                        .foregroundStyle(running ? Color.green : Color.secondary)
                }
            }
            VStack(alignment: .leading, spacing: 4) {
                Text(model.statusTitle)
                    .font(.title2.weight(.semibold))
                Text(summary(snapshot))
                    .foregroundStyle(.secondary)
                    .textSelection(.enabled)
                if running {
                    Text("↓ \(Format.rate(snapshot.traffic.download))   ↑ \(Format.rate(snapshot.traffic.upload))   \(snapshot.traffic.connections) 条连接")
                        .font(.callout.monospacedDigit())
                        .foregroundStyle(.secondary)
                }
            }
            Spacer()
            Button {
                Task { await model.toggleConnection() }
            } label: {
                Text(running ? "断开" : "连接")
                    .frame(minWidth: 72)
            }
            .controlSize(.large)
            .buttonStyle(.borderedProminent)
            .tint(running ? .red : .accentColor)
            .disabled(busy)
            .keyboardShortcut(.defaultAction)
        }
        .padding(16)
        .background(RoundedRectangle(cornerRadius: 10).fill(Color.secondary.opacity(0.06)))
    }

    private func summary(_ snapshot: Snapshot) -> String {
        let settings = snapshot.settings
        var parts = [settings.tunEnabled ? "TUN 全局接管" : "仅本地代理端口"]
        let routes = settings.routeSummary
        parts.append(routes.isEmpty ? "未设置线路" : routes.joined(separator: "、"))
        if let pid = snapshot.managed.pid, snapshot.managed.running {
            parts.append("PID \(pid)")
        }
        return parts.joined(separator: " · ")
    }

    // MARK: 需要处理的问题

    @ViewBuilder
    private func alerts(_ snapshot: Snapshot) -> some View {
        if let claude = model.claudeStatus, claude.level != "off" {
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

    // MARK: 详情

    private func details(_ snapshot: Snapshot) -> some View {
        GroupBox {
            Grid(alignment: .leadingFirstTextBaseline, horizontalSpacing: 16, verticalSpacing: 8) {
                GridRow {
                    Text("APP 版本")
                        .foregroundStyle(.secondary)
                        .gridColumnAlignment(.trailing)
                    HStack(spacing: 8) {
                        Text(appVersion(snapshot))
                            .textSelection(.enabled)
                        Button(model.checkingUpdate ? "正在检查…" : "检查更新") { Task { await model.checkForUpdates() } }
                            .controlSize(.small)
                            .disabled(model.checkingUpdate)
                    }
                }
                DetailRow(label: "桌面版", value: Bundle.main.object(forInfoDictionaryKey: "TiziBuildVersion") as? String ?? "开发构建")
                DetailRow(label: "Mihomo 核心", value: snapshot.managed.coreVersion ?? (snapshot.managed.coreInstalled ? "正在读取…" : "未安装"))
                DetailRow(label: "Controller", value: snapshot.controllerOnline ? "在线（127.0.0.1:\(snapshot.settings.controllerPort)）" : "不可访问")
                DetailRow(label: "代理节点", value: proxyNodes(snapshot.settings))
                DetailRow(label: "内网", value: snapshot.settings.hasOVPN ? snapshot.settings.ovpnPath : "未导入")
                if snapshot.settings.hasOVPN {
                    DetailRow(label: "Remote", value: snapshot.settings.remote.isEmpty ? "全部" : snapshot.settings.remote)
                }
                DetailRow(label: "Tailscale", value: tailscale(snapshot.settings))
                DetailRow(label: "转发模式", value: snapshot.settings.tunEnabled ? "TUN 全局接管" : "仅本地代理端口")
                DetailRow(label: "本地代理", value: "\(snapshot.settings.allowLAN ? "0.0.0.0" : "127.0.0.1"):\(snapshot.settings.mixedPort)（HTTP/SOCKS5）")
                DetailRow(label: "国内流量", value: snapshot.settings.chinaDirect ? "直连" : "经外网代理")
                DetailRow(label: "Claude 出口", value: claudeExit(snapshot))
            }
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(6)
        } label: {
            Text("运行状态")
        }
    }

    private func proxyNodes(_ settings: EngineSettings) -> String {
        var parts: [String] = []
        if settings.hasSubscription { parts.append("订阅 \(settings.subscription)") }
        if settings.nodeCount > 0 { parts.append("节点文件 \(settings.nodeCount) 个节点") }
        return parts.isEmpty ? "未设置" : parts.joined(separator: " · ")
    }

    private func tailscale(_ settings: EngineSettings) -> String {
        guard settings.tailscaleEnabled else { return "关闭" }
        var text = "开启"
        if !settings.tailscaleDeviceName.isEmpty { text += "（设备名 \(settings.tailscaleDeviceName)）" }
        if !settings.tailscaleLoggedIn && !settings.tailscaleHasAuthKey { text += " · 首次登录需要 Auth Key" }
        return text
    }

    private func claudeExit(_ snapshot: Snapshot) -> String {
        let settings = snapshot.settings
        var text = settings.hasResidentialNode
            ? (settings.claudeProxyDirect == true || !settings.canChain ? "住宅节点（直接连接）" : "住宅节点（经前置代理链式连接）")
            : "代理节点"
        if let exit = model.claudeStatus?.exitText { text += " · " + exit }
        return text
    }

    private func appVersion(_ snapshot: Snapshot) -> String {
        let current = model.hello?.version ?? "未知"
        if let installed = snapshot.update.installed { return "\(current)（已更新至 \(installed)，重启引擎后生效）" }
        if !snapshot.update.checked || model.checkingUpdate { return "\(current)（正在检查更新…）" }
        if snapshot.update.available, let latest = snapshot.update.latest { return "\(current)（最新 \(latest)）" }
        if snapshot.update.error != nil { return "\(current)（更新检查失败）" }
        guard let checked = snapshot.update.lastChecked else { return "\(current)（已是最新）" }
        let day: Date.FormatStyle.DateStyle = Calendar.current.isDateInToday(checked) ? .omitted : .abbreviated
        return "\(current)（已是最新，\(checked.formatted(date: day, time: .shortened)) 检查）"
    }

    // MARK: 操作

    private func actions(_ snapshot: Snapshot) -> some View {
        GroupBox {
            VStack(alignment: .leading, spacing: 10) {
                HStack(spacing: 10) {
                    Button("生成并校验配置") { Task { await model.validate() } }
                    Button(snapshot.managed.coreInstalled ? "更新 Mihomo 核心（\(channelName(snapshot))）" : "安装 Mihomo 核心（\(channelName(snapshot))）") {
                        if snapshot.managed.running {
                            confirmReinstall = true
                        } else {
                            Task { await model.installCore() }
                        }
                    }
                    Button("Clash Mi 扫码导入") { Task { await model.startShare() } }
                }
                HStack(spacing: 10) {
                    Button("重载 Mihomo") { Task { await model.reload() } }
                        .help("重新生成配置并重启 Mihomo，让自定义规则和设置修改立即生效")
                    Button("打开 Mihomo 面板") { Task { await model.openDashboard() } }
                        .disabled(!snapshot.controllerOnline)
                    Button("刷新微信直连") { Task { await model.recoverWeChat() } }
                        .disabled(!snapshot.controllerOnline)
                        .help("微信图片反复发送失败时使用；只关闭腾讯直连，不重启 Mihomo")
                }
            }
            .disabled(busy)
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(6)
        } label: {
            Text("操作")
        }
    }

    private func channelName(_ snapshot: Snapshot) -> String {
        snapshot.settings.coreChannel == "alpha" ? "Alpha" : "Stable"
    }
}

struct DetailRow: View {
    let label: String
    let value: String

    var body: some View {
        GridRow {
            Text(label)
                .foregroundStyle(.secondary)
                .gridColumnAlignment(.trailing)
            Text(value)
                .textSelection(.enabled)
                .lineLimit(2)
                .truncationMode(.middle)
        }
    }
}

struct AlertRow<Accessory: View>: View {
    let symbol: String
    let tint: Color
    let text: String
    @ViewBuilder var accessory: () -> Accessory

    var body: some View {
        HStack(spacing: 10) {
            Image(systemName: symbol)
                .foregroundStyle(tint)
            Text(text)
                .frame(maxWidth: .infinity, alignment: .leading)
                .textSelection(.enabled)
            accessory()
        }
        .padding(12)
        .background(RoundedRectangle(cornerRadius: 8).fill(tint.opacity(0.10)))
    }
}
