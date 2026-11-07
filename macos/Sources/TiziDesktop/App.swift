import AppKit
import SwiftUI

@main
struct TiziDesktopApp: App {
    @NSApplicationDelegateAdaptor(AppDelegate.self) private var delegate
    @ObservedObject private var model = AppModel.shared

    var body: some Scene {
        Window("Tizi", id: "main") {
            RootView()
                .environmentObject(model)
        }
        // 简洁模式是固定大小的小窗口，高级模式可自由调整；窗口约束跟随内容。
        .defaultSize(width: 900, height: 620)
        .windowResizability(.contentSize)
        .commands {
            CommandGroup(replacing: .newItem) {}
            CommandGroup(after: .appInfo) {
                Button(model.checkingUpdate ? "正在检查更新…" : "检查更新…") { Task { await model.checkForUpdates() } }
                    .disabled(model.engineState != .running || model.checkingUpdate)
            }
            CommandMenu("连接") {
                Button(model.running ? "断开" : "连接") {
                    Task { await model.toggleConnection() }
                }
                .keyboardShortcut("k")
                .disabled(model.engineState != .running || model.busyLabel != nil)
                Button("刷新") { model.refresh() }
                    .keyboardShortcut("r")
                Divider()
                Button(model.mode == .simple ? "切换到高级模式" : "切换到简洁模式") {
                    model.mode = model.mode == .simple ? .advanced : .simple
                }
                .keyboardShortcut("0")
                ForEach(Array(Page.allCases.enumerated()), id: \.element) { index, page in
                    Button(page.title) {
                        model.page = page
                        model.mode = .advanced
                    }
                    .keyboardShortcut(KeyEquivalent(Character(String(index + 1))))
                }
            }
        }

        MenuBarExtra {
            MenuBarContent()
                .environmentObject(model)
        } label: {
            Image(systemName: model.menuBarSymbol)
        }
    }
}

final class AppDelegate: NSObject, NSApplicationDelegate {
    func applicationWillFinishLaunching(_ notification: Notification) {
        // 引擎意外退出后再写它的标准输入会触发 SIGPIPE；忽略后写入改为抛错。
        signal(SIGPIPE, SIG_IGN)
    }

    func applicationDidFinishLaunching(_ notification: Notification) {
        NSWorkspace.shared.notificationCenter.addObserver(forName: NSWorkspace.didWakeNotification, object: nil, queue: .main) { _ in
            MainActor.assumeIsolated { AppModel.shared.checkForUpdatesAfterWake() }
        }
        MenuBarContent.checkForUpdatesWhenOpened()
        Task { @MainActor in
            AppModel.shared.startEngine()
            #if DEBUG
            DebugSnapshots.runIfRequested()
            #endif
        }
    }

    func applicationShouldHandleReopen(_ sender: NSApplication, hasVisibleWindows flag: Bool) -> Bool {
        true
    }

    // 菜单栏应用：关闭主窗口后继续在菜单栏运行。
    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool {
        false
    }

    func applicationShouldTerminate(_ sender: NSApplication) -> NSApplication.TerminateReply {
        MainActor.assumeIsolated {
            let model = AppModel.shared
            if model.relaunching {
                return .terminateNow
            }
            if model.engineState == .running, let busy = model.busyLabel {
                // 与 TUI 一致：操作进行中不退出，避免界面消失后操作在后台悄悄完成。
                let alert = NSAlert()
                alert.messageText = "正在\(busy)"
                alert.informativeText = "请等待当前操作完成后再退出。如果在等待管理员密码，可以在密码框中取消。"
                alert.addButton(withTitle: "好")
                NSApp.activate(ignoringOtherApps: true)
                alert.runModal()
                return .terminateCancel
            }
            guard model.engineState == .running, model.running else {
                model.shutdownEngine()
                return .terminateNow
            }
            // 与 TUI 的退出确认一致：停止并退出，或保持连接在后台运行。
            let alert = NSAlert()
            alert.messageText = "Tizi 仍在连接"
            alert.informativeText = model.mode == .simple
                ? "「停止并退出」会断开连接，网络恢复原样。\n「保持运行退出」只关闭窗口，连接继续工作；想断开时重新打开 Tizi 点「断开」。"
                : "「停止并退出」会停止 Mihomo 并恢复原来的网络设置。\n「保持运行退出」只关闭桌面版，Mihomo 继续工作；之后可以重新打开桌面版，或在终端执行 tizi stop 断开。"
            alert.addButton(withTitle: "停止并退出")
            alert.addButton(withTitle: "保持运行退出")
            alert.addButton(withTitle: "取消")
            NSApp.activate(ignoringOtherApps: true)
            switch alert.runModal() {
            case .alertFirstButtonReturn:
                Task { @MainActor in
                    let stopped = await model.stop()
                    if stopped { model.shutdownEngine() }
                    sender.reply(toApplicationShouldTerminate: stopped)
                }
                return .terminateLater
            case .alertSecondButtonReturn:
                model.shutdownEngine()
                return .terminateNow
            default:
                return .terminateCancel
            }
        }
    }
}
