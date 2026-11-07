import AppKit
import SwiftUI

struct MenuBarContent: View {
    @EnvironmentObject private var model: AppModel
    @Environment(\.openWindow) private var openWindow

    var body: some View {
        Text("Tizi · \(model.statusTitle)")
        if model.running, let traffic = model.snapshot?.traffic {
            Text("↓ \(Format.rate(traffic.download))   ↑ \(Format.rate(traffic.upload))")
        }
        if let claude = model.claudeStatus, claude.level != "checking" {
            Button("Claude：\(claude.title)") { ClaudeAlertWindow.shared.show() }
        }
        Divider()
        Button(model.running ? "断开" : "连接") {
            Task { await model.toggleConnection() }
        }
        .disabled(model.engineState != .running || model.busyLabel != nil)

        // 策略组、面板和扫码分享只在高级模式出现，简洁模式的菜单只管连接。
        if model.mode == .advanced {
            if let groups = model.snapshot?.groups, !groups.isEmpty {
                Divider()
                ForEach(groups) { group in
                    Menu("\(group.name)：\(group.now)") {
                        Picker(group.name, selection: Binding(
                            get: { group.now },
                            set: { model.selectPolicy(group: group.name, choice: $0) }
                        )) {
                            ForEach(group.choices, id: \.self) { choice in
                                Text(choiceLabel(choice)).tag(choice)
                            }
                        }
                        .pickerStyle(.inline)
                        .labelsHidden()
                    }
                }
            }
        }

        Divider()
        Button("打开主窗口…") { showMainWindow() }
        if model.mode == .advanced {
            Button("打开 Mihomo 面板") { Task { await model.openDashboard() } }
                .disabled(model.snapshot?.controllerOnline != true)
            Button("Clash Mi 扫码导入…") {
                showMainWindow()
                Task { await model.startShare() }
            }
            .disabled(model.engineState != .running)
        }
        Divider()
        updateItem
        Button(Self.quitTitle) { NSApp.terminate(nil) }
            .keyboardShortcut("q")
            .onAppear(perform: registerOpenWindow)
    }

    /// 只有菜单栏菜单里有这一项，靠它的标题认出菜单栏菜单。
    static let quitTitle = "退出 Tizi"

    /// 菜单栏菜单每次打开时检查更新。SwiftUI 不会在每次打开 MenuBarExtra 的
    /// 菜单时调用 onAppear（实测），所以监听 AppKit 的菜单开始显示。通知发出时
    /// SwiftUI 还没填入菜单项，等到下一轮主线程循环再按标题认菜单。
    static func checkForUpdatesWhenOpened() {
        NotificationCenter.default.addObserver(forName: NSMenu.didBeginTrackingNotification, object: nil, queue: .main) { notification in
            guard let menu = notification.object as? NSMenu, menu.supermenu == nil else { return }
            DispatchQueue.main.async {
                guard menu.items.contains(where: { $0.title == quitTitle }) else { return }
                MainActor.assumeIsolated { AppModel.shared.checkForUpdatesQuietly() }
            }
        }
    }

    /// 两种模式都有：平时是「检查更新…」，已知有新版本时直接给出更新入口。
    @ViewBuilder
    private var updateItem: some View {
        let update = model.snapshot?.update
        let busy = model.busyLabel != nil
        if let installed = update?.installed {
            Button("新版本 \(installed) 已下载，立即生效") { model.restartEngine(reload: model.running) }
                .disabled(busy)
        } else if update?.available == true, let latest = update?.latest {
            Button("更新到 \(latest)") { Task { await model.installUpdate() } }
                .disabled(busy)
        } else {
            Button(model.checkingUpdate ? "正在检查更新…" : "检查更新…") { Task { await model.checkForUpdates() } }
                .disabled(model.engineState != .running || model.checkingUpdate)
        }
    }

    /// 菜单里的选项：自动选择写出它此刻用的节点，节点写出最近一次测速的延迟。
    private func choiceLabel(_ choice: String) -> String {
        guard let snapshot = model.snapshot else { return choice }
        var label = choice
        if let node = snapshot.autoNow(choice) { label += " → \(node)" }
        if let delay = snapshot.delay(of: choice) { label += delay > 0 ? "  \(delay)ms" : "  超时" }
        return label
    }

    private func showMainWindow() {
        openWindow(id: "main")
        NSApp.activate(ignoringOtherApps: true)
    }

    private func registerOpenWindow() {
        model.openMainWindow = { [openWindow] in openWindow(id: "main") }
    }
}
