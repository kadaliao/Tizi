#if DEBUG
import AppKit

/// 调试构建专用：设置 TIZI_SNAPSHOT_DIR 后，依次切换各页面并把主窗口
/// 渲染为 PNG，用于在没有录屏权限的环境里检查界面。页面名 simple 表示简洁模式，
/// 其余是高级模式的页面。正式构建不包含这段代码。
@MainActor
enum DebugSnapshots {
    static func runIfRequested() {
        guard let directory = ProcessInfo.processInfo.environment["TIZI_SNAPSHOT_DIR"] else { return }
        let names = ProcessInfo.processInfo.environment["TIZI_SNAPSHOT_PAGES"]
            .map { $0.split(separator: ",").map(String.init) } ?? ["simple"] + Page.allCases.map(\.rawValue)
        Task { @MainActor in
            try? await Task.sleep(nanoseconds: 4_000_000_000)
            for name in names {
                if name == "claude-alert" {
                    // 等引擎连续两次发现问题后自己弹出告警窗口，最多 30 秒。
                    for _ in 0..<60 where ClaudeAlertWindow.shared.debugWindow?.isVisible != true {
                        try? await Task.sleep(nanoseconds: 500_000_000)
                    }
                    try? await Task.sleep(nanoseconds: 1_000_000_000)
                    if let window = ClaudeAlertWindow.shared.debugWindow {
                        capture(window, to: URL(fileURLWithPath: directory).appendingPathComponent("\(name).png"))
                    }
                    continue
                }
                if let page = Page(rawValue: name) {
                    AppModel.shared.page = page
                    AppModel.shared.mode = .advanced
                } else if name == "simple" {
                    AppModel.shared.mode = .simple
                } else {
                    continue
                }
                try? await Task.sleep(nanoseconds: 3_000_000_000)
                capture(to: URL(fileURLWithPath: directory).appendingPathComponent("\(name).png"))
            }
            if ProcessInfo.processInfo.environment["TIZI_SNAPSHOT_QUIT"] == "1" {
                AppModel.shared.shutdownEngine()
                exit(0)
            }
        }
    }

    static func capture(to url: URL) {
        // 主窗口标题随导航标题变化，按「可见、不是菜单栏窗口、不是 Claude 告警」挑选。
        guard let window = NSApp.windows.first(where: {
            $0.isVisible && !($0.className.contains("StatusBar")) && $0.frame.width > 300
                && $0 !== ClaudeAlertWindow.shared.debugWindow
        }) else { return }
        capture(window, to: url)
    }

    static func capture(_ window: NSWindow, to url: URL) {
        guard let view = window.contentView?.superview ?? window.contentView,
              let bitmap = view.bitmapImageRepForCachingDisplay(in: view.bounds) else { return }
        view.cacheDisplay(in: view.bounds, to: bitmap)
        try? bitmap.representation(using: .png, properties: [:])?.write(to: url)
    }
}
#endif
