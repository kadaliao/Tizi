import AppKit
import SwiftUI

/// Claude 线路出问题时弹出的置顶大窗口。它不跟随主窗口：菜单栏常驻、主窗口
/// 关着的时候也会出现在所有桌面和全屏应用之上。内容随快照实时更新，恢复
/// 正常后变成绿色的「已恢复」。
@MainActor
final class ClaudeAlertWindow: NSObject, NSWindowDelegate {
    static let shared = ClaudeAlertWindow()

    private var window: NSWindow?

    func show() {
        let window = self.window ?? makeWindow()
        self.window = window
        if !window.isVisible { window.center() }
        NSApp.activate(ignoringOtherApps: true)
        window.makeKeyAndOrderFront(nil)
        window.orderFrontRegardless()
        NSSound(named: "Basso")?.play()
    }

    func close() {
        window?.close()
    }

    #if DEBUG
    var debugWindow: NSWindow? { window }
    #endif

    private func makeWindow() -> NSWindow {
        let hosting = NSHostingController(rootView: ClaudeAlertView().environmentObject(AppModel.shared))
        let window = NSWindow(contentViewController: hosting)
        window.title = "Claude 访问提醒"
        window.styleMask = [.titled, .closable]
        // 高于普通窗口和浮动面板；出现在所有桌面，包括全屏应用所在的桌面。
        window.level = .modalPanel
        window.collectionBehavior = [.canJoinAllSpaces, .fullScreenAuxiliary]
        window.isReleasedWhenClosed = false
        window.delegate = self
        return window
    }
}

struct ClaudeAlertView: View {
    @EnvironmentObject private var model: AppModel

    var body: some View {
        let status = model.claudeStatus
        VStack(alignment: .leading, spacing: 18) {
            HStack(alignment: .top, spacing: 18) {
                Image(systemName: symbol(status))
                    .font(.system(size: 56))
                    .foregroundStyle(tint(status))
                    .frame(width: 64)
                VStack(alignment: .leading, spacing: 10) {
                    Text(title(status))
                        .font(.system(size: 24, weight: .bold))
                        .fixedSize(horizontal: false, vertical: true)
                    if let detail = status?.detail, !detail.isEmpty {
                        Text(detail)
                            .font(.title3)
                            .fixedSize(horizontal: false, vertical: true)
                            .textSelection(.enabled)
                    }
                    facts(status)
                }
            }
            HStack(spacing: 10) {
                Button("重新检测") { model.checkClaude() }
                Button("打开 Tizi") { model.showMainWindow() }
                Spacer()
                fix(status)
                Button(status?.isAlert == true ? "知道了" : "关闭") {
                    ClaudeAlertWindow.shared.close()
                }
                .keyboardShortcut(.defaultAction)
            }
            .controlSize(.large)
        }
        .padding(24)
        .frame(width: 560)
    }

    @ViewBuilder
    private func facts(_ status: ClaudeStatus?) -> some View {
        VStack(alignment: .leading, spacing: 4) {
            if let route = status?.route, !route.isEmpty {
                Text("线路：\(route)")
            }
            if let exit = status?.exitText, status?.isOK == false {
                Text("出口：\(exit)")
            }
            if let checked = status?.checkedDate {
                Text("检测时间：\(checked.formatted(date: .omitted, time: .standard))")
            }
            if status?.rechecking == true {
                Text("正在复查…")
            }
        }
        .font(.callout)
        .foregroundStyle(.secondary)
    }

    /// 针对告警原因的直接处理办法。
    @ViewBuilder
    private func fix(_ status: ClaudeStatus?) -> some View {
        let busy = model.busyLabel != nil
        switch status?.isAlert == true ? status?.key : nil {
        case "vpn_exited"?:
            Button("重新连接") { Task { await model.start() } }.disabled(busy)
        case "controller"?, "direct"?:
            Button("重新连接") { Task { await model.reload() } }.disabled(busy || !model.running)
        case "reload_needed"?:
            Button("重载") { Task { await model.reload() } }.disabled(busy || !model.running)
        case "not_residential"?:
            Button("切回住宅节点") { model.useResidentialForClaude() }
        case "unreachable"? where status?.residential == true:
            if let fallback = model.claudeFallbackChoice {
                Button("临时改走其他节点") { model.useFallbackForClaude() }
                    .help("Claude 改走「\(fallback)」，出口 IP 会随之改变；重新连接或重载 Mihomo 后自动切回住宅节点")
            }
        default:
            EmptyView()
        }
    }

    private func title(_ status: ClaudeStatus?) -> String {
        guard let status else { return "Claude 访问监测已关闭" }
        if status.isAlert { return status.title }
        if status.isOK { return "已恢复：" + status.title }
        return status.title
    }

    private func symbol(_ status: ClaudeStatus?) -> String {
        switch status?.level {
        case "alert": return "exclamationmark.octagon.fill"
        case "ok": return "checkmark.shield.fill"
        default: return "exclamationmark.triangle.fill"
        }
    }

    private func tint(_ status: ClaudeStatus?) -> Color {
        switch status?.level {
        case "alert": return .red
        case "ok": return .green
        default: return .orange
        }
    }
}

/// 状态页和简洁模式共用的一行 Claude 状态。
struct ClaudeStatusRow: View {
    @EnvironmentObject private var model: AppModel
    let status: ClaudeStatus
    var compact = false

    var body: some View {
        AlertRow(symbol: symbol, tint: tint, text: text) {
            HStack {
                if status.key == "reload_needed" {
                    Button("重载") { Task { await model.reload() } }.disabled(model.busyLabel != nil)
                }
                if status.isAlert && !compact {
                    Button("详情") { ClaudeAlertWindow.shared.show() }
                }
                Button(compact ? "检测" : "重新检测") { model.checkClaude() }
            }
        }
    }

    private var text: String {
        var parts = ["Claude：" + status.title]
        if status.isOK, let exit = status.exitText {
            parts.append("出口 " + exit)
        } else if !compact, let detail = status.detail, !detail.isEmpty {
            parts.append(detail)
        }
        if status.rechecking && status.level != "checking" { parts.append("正在复查…") }
        return parts.joined(separator: compact ? " · " : "\n")
    }

    private var symbol: String {
        switch status.level {
        case "alert": return "exclamationmark.octagon.fill"
        case "ok": return "checkmark.shield"
        case "warn": return "exclamationmark.triangle.fill"
        default: return "shield"
        }
    }

    private var tint: Color {
        switch status.level {
        case "alert": return .red
        case "ok": return .green
        case "warn": return .orange
        default: return .secondary
        }
    }
}
