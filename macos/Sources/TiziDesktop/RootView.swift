import AppKit
import SwiftUI

/// 主窗口：按界面模式显示简洁页或 7 页的高级界面。两种模式共用的弹窗放在这里，
/// 切换模式时不会丢失。
struct RootView: View {
    @EnvironmentObject private var model: AppModel
    @Environment(\.openWindow) private var openWindow
    @State private var window: NSWindow?

    static let simpleSize = CGSize(width: 380, height: 520)
    static let advancedSize = CGSize(width: 900, height: 620)

    var body: some View {
        Group {
            switch model.mode {
            case .simple:
                SimpleView()
                    .frame(width: Self.simpleSize.width, height: Self.simpleSize.height)
            case .advanced:
                MainView()
                    .frame(minWidth: 760, minHeight: 520)
            }
        }
        .tint(.brand)
        .background(WindowReader(window: $window))
        .onChange(of: model.mode) { mode in resize(for: mode) }
        .confirmationDialog(conflictTitle, isPresented: $model.conflictPrompt, titleVisibility: .visible) {
            Button(model.mode == .simple ? "关闭它并连接" : "结束其他 OpenVPN 后启动") {
                Task { await model.start(stopOpenVPN: true) }
            }
            Button(model.mode == .simple ? "仍然连接" : "仍然启动") { Task { await model.start(allowConflict: true) } }
            Button("取消", role: .cancel) {}
        } message: {
            Text(model.mode == .simple
                 ? "例如 Tunnelblick。两个 VPN 同时开着，可能都上不了网。"
                 : "例如 Tunnelblick。两个 VPN 同时接管网络可能导致内网或互联网不可用；建议先结束其他 OpenVPN。只会结束 OpenVPN 进程，不会退出 VPN 客户端本身。")
        }
        // 按 Esc 关掉弹窗也要停止分享，局域网上不能留着带私钥的配置。
        .sheet(isPresented: $model.showShare, onDismiss: {
            if model.activeShare != nil { model.stopShare() }
        }) {
            ShareSheet()
        }
        .onAppear {
            // 菜单栏应用平时不占 Dock；主窗口打开期间作为普通应用出现在 Dock 和 ⌘Tab 中。
            NSApp.setActivationPolicy(.regular)
            NSApp.activate(ignoringOtherApps: true)
            model.openMainWindow = { [openWindow] in openWindow(id: "main") }
        }
        .onDisappear {
            NSApp.setActivationPolicy(.accessory)
        }
    }

    private var conflictTitle: String {
        model.mode == .simple ? "另一个 VPN 正在运行" : "检测到其他 OpenVPN 正在运行"
    }

    /// 简洁模式的固定尺寸由内容约束决定；高级模式只保证不小于最小尺寸，这里
    /// 再放大到舒适尺寸并保持左上角不动。等 SwiftUI 按新内容更新完窗口约束后
    /// 再调整，否则两边的调整会互相覆盖。
    private func resize(for mode: InterfaceMode) {
        guard mode == .advanced, let window else { return }
        DispatchQueue.main.async {
            let frame = window.frame
            let width = max(frame.width, Self.advancedSize.width)
            let height = max(frame.height, Self.advancedSize.height)
            window.setFrame(NSRect(x: frame.minX, y: frame.maxY - height, width: width, height: height), display: true)
        }
    }
}

/// 取得承载 SwiftUI 内容的 NSWindow。
private struct WindowReader: NSViewRepresentable {
    @Binding var window: NSWindow?

    func makeNSView(context: Context) -> NSView {
        let view = NSView()
        DispatchQueue.main.async { window = view.window }
        return view
    }

    func updateNSView(_ view: NSView, context: Context) {
        if view.window !== window {
            DispatchQueue.main.async { window = view.window }
        }
    }
}
