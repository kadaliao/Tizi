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
        let environment = ProcessInfo.processInfo.environment
        // TIZI_SNAPSHOT_APPEARANCE=light|dark 固定外观；TIZI_SNAPSHOT_HEIGHT 拉高主窗口，截到长页面的下半部分。
        switch environment["TIZI_SNAPSHOT_APPEARANCE"] {
        case "light"?: NSApp.appearance = NSAppearance(named: .aqua)
        case "dark"?: NSApp.appearance = NSAppearance(named: .darkAqua)
        default: break
        }
        let height = environment["TIZI_SNAPSHOT_HEIGHT"].flatMap { Double($0) }
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
                try? await Task.sleep(nanoseconds: 1_500_000_000)
                if let height, AppModel.shared.mode == .advanced,
                   let window = NSApp.windows.first(where: { $0.isVisible && $0.frame.width > 300 && $0 !== ClaudeAlertWindow.shared.debugWindow }) {
                    var frame = window.frame
                    frame.origin.y += frame.height - height
                    frame.size.height = height
                    window.setFrame(frame, display: true)
                }
                // TIZI_SNAPSHOT_SCROLL=0…1 把页面里最长的滚动区域滚到对应位置。
                if let fraction = environment["TIZI_SNAPSHOT_SCROLL"].flatMap({ Double($0) }),
                   let root = NSApp.windows.first(where: { $0.isVisible && $0.frame.width > 300 })?.contentView,
                   let scroll = scrollViews(in: root).max(by: { ($0.documentView?.frame.height ?? 0) < ($1.documentView?.frame.height ?? 0) }),
                   let document = scroll.documentView {
                    let y = max(0, document.frame.height - scroll.contentView.bounds.height) * fraction
                    scroll.contentView.scroll(to: NSPoint(x: 0, y: y))
                    scroll.reflectScrolledClipView(scroll.contentView)
                }
                try? await Task.sleep(nanoseconds: 1_500_000_000)
                capture(to: URL(fileURLWithPath: directory).appendingPathComponent("\(name).png"))
            }
            if ProcessInfo.processInfo.environment["TIZI_SNAPSHOT_QUIT"] == "1" {
                AppModel.shared.shutdownEngine()
                exit(0)
            }
        }
    }

    static func scrollViews(in view: NSView) -> [NSScrollView] {
        var found: [NSScrollView] = []
        if let scroll = view as? NSScrollView { found.append(scroll) }
        for subview in view.subviews { found += scrollViews(in: subview) }
        return found
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
