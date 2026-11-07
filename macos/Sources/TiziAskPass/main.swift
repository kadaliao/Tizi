// sudo 的 askpass 程序：后台引擎以 `sudo -A` 请求管理员权限时由 sudo 启动，
// 第一个参数是提示文字。用户确认后把密码写到标准输出并以 0 退出，取消则以 1
// 退出。密码只经过这个进程和 sudo，不经过 GUI 或引擎。
import AppKit

/// 无人处理的密码框自动取消，对应操作以授权失败结束，不会在之后悄悄继续。
let timeout: TimeInterval = 120

let prompt = CommandLine.arguments.dropFirst().first ?? "需要管理员权限。"
let sudoPID = getppid()

let app = NSApplication.shared
app.setActivationPolicy(.accessory)

// 附件视图里的输入框靠主菜单的「编辑」项响应 ⌘V、⌘A 等快捷键。
let mainMenu = NSMenu()
let editItem = NSMenuItem()
let editMenu = NSMenu(title: "编辑")
editMenu.addItem(withTitle: "剪切", action: #selector(NSText.cut(_:)), keyEquivalent: "x")
editMenu.addItem(withTitle: "复制", action: #selector(NSText.copy(_:)), keyEquivalent: "c")
editMenu.addItem(withTitle: "粘贴", action: #selector(NSText.paste(_:)), keyEquivalent: "v")
editMenu.addItem(withTitle: "全选", action: #selector(NSText.selectAll(_:)), keyEquivalent: "a")
editItem.submenu = editMenu
mainMenu.addItem(editItem)
app.mainMenu = mainMenu

let alert = NSAlert()
alert.messageText = "Tizi 需要管理员权限"
alert.informativeText = "\(prompt)\n\n用户：\(NSUserName())"
alert.alertStyle = .informational
if let icon = Bundle.main.url(forResource: "AppIcon", withExtension: "icns").flatMap(NSImage.init(contentsOf:)) {
    alert.icon = icon
}
alert.addButton(withTitle: "授权")
alert.addButton(withTitle: "取消")

let field = NSSecureTextField(frame: NSRect(x: 0, y: 0, width: 280, height: 24))
field.placeholderString = "登录密码"
alert.accessoryView = field
alert.window.initialFirstResponder = field
alert.window.level = .floating

// 超时或 sudo 已经退出（例如引擎取消了授权）时关闭密码框。runModal 运行在
// 模态 run loop 模式下，定时器要加到 common 模式才会触发。
let started = Date()
let watchdog = Timer(timeInterval: 1, repeats: true) { _ in
    if Date().timeIntervalSince(started) > timeout || getppid() != sudoPID {
        exit(1)
    }
}
RunLoop.main.add(watchdog, forMode: .common)

if #available(macOS 14.0, *) {
    app.activate()
} else {
    app.activate(ignoringOtherApps: true)
}

guard alert.runModal() == .alertFirstButtonReturn else {
    exit(1)
}
FileHandle.standardOutput.write(Data((field.stringValue + "\n").utf8))
exit(0)
