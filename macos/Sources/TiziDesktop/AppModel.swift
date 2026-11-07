import AppKit
import Foundation

enum Page: String, CaseIterable, Identifiable {
    case status, config, policy, rules, connections, logs, diagnostics

    var id: String { rawValue }

    // 与 TUI 的 1-7 页顺序和名称保持一致，两边切换时不需要重新找功能。
    var title: String {
        switch self {
        case .status: return "状态"
        case .config: return "配置"
        case .policy: return "策略"
        case .rules: return "规则"
        case .connections: return "连接"
        case .logs: return "日志"
        case .diagnostics: return "诊断"
        }
    }

    var symbol: String {
        switch self {
        case .status: return "gauge.with.dots.needle.33percent"
        case .config: return "slider.horizontal.3"
        case .policy: return "arrow.triangle.branch"
        case .rules: return "list.bullet.rectangle"
        case .connections: return "network"
        case .logs: return "doc.text.magnifyingglass"
        case .diagnostics: return "stethoscope"
        }
    }
}

/// 简洁模式只有「设置线路」和「连接/断开」，给没有网络基础的用户；
/// 高级模式是与 TUI 一一对应的 7 个页面。
enum InterfaceMode: String {
    case simple, advanced
}

enum EngineState: Equatable {
    case starting
    case running
    /// 没有找到 tizi。
    case missing
    /// 找到了，但版本不支持桌面版或协议不兼容。
    case unsupported(String)
    case crashed(String)
}

struct Notice: Identifiable, Equatable {
    let id = UUID()
    var text: String
    var isError: Bool
}

@MainActor
final class AppModel: ObservableObject {
    static let shared = AppModel()

    @Published private(set) var engineState: EngineState = .starting
    @Published private(set) var enginePath: URL?
    @Published private(set) var hello: Hello?
    @Published var snapshot: Snapshot?
    @Published private(set) var install: InstallProgress?
    @Published private(set) var notice: Notice?
    /// 本地发起、引擎快照尚未反映的操作，用于按钮立即进入忙碌态。
    @Published private(set) var pendingActions: [UUID: String] = [:]
    @Published var conflictPrompt = false
    @Published var mode = InterfaceMode(rawValue: UserDefaults.standard.string(forKey: "InterfaceMode") ?? "") ?? .simple {
        didSet { UserDefaults.standard.set(mode.rawValue, forKey: "InterfaceMode") }
    }
    @Published var page: Page = .status
    @Published var showShare = false
    @Published private(set) var diagnostics: [DiagnosticCheck] = []
    @Published private(set) var diagnosticsLoading = false
    @Published private(set) var logs: [String] = []
    @Published private(set) var rules: Rules?
    @Published private(set) var routeTest: RouteTest?
    @Published private(set) var routeTesting = false
    @Published private(set) var scannedOVPN: [String] = []
    /// 用户点了「检查更新」、还没有结果。自动检查不改它，界面不会因此闪动。
    @Published private(set) var checkingUpdate = false
    /// 用户点了「测速」、还没有结果。
    @Published private(set) var testingDelays = false
    private var wakeUpdateCheck: Task<Void, Never>?
    /// 正在进行的配置分享。以 shareStart 的结果为准，直到之后生成的快照
    /// 显示分享已关闭（过期或被关闭），避免与发起前生成的旧快照赛跑。
    @Published private(set) var activeShare: Share?
    private var shareReceivedAt: Date?

    /// 本轮已经弹过窗（或用户已经知道）的 Claude 告警原因。同一个原因只弹一次，
    /// Claude 恢复正常（或监测关闭、VPN 被主动断开）后清空，下次出问题再弹。
    private var claudeAlertedKeys: Set<String> = []

    private let client = EngineClient()
    private var restarting = false
    /// 引擎就绪后重载 Mihomo：更新后重启引擎，或更新后重新打开的桌面版
    /// （启动参数 --reload-mihomo）使用，让新版本的规则立即生效。
    private var reloadAfterRestart = CommandLine.arguments.contains("--reload-mihomo")
    /// 更新后正在重新打开自己，退出时不再询问是否断开 VPN。
    private(set) var relaunching = false
    private var noticeTask: Task<Void, Never>?
    /// 启动时的桌面版构建版本，以及引擎启动时程序文件的修改时间。终端版或
    /// 命令行更新会在桌面版运行时替换这两者，据此发现并换用新版本。
    private let launchedBuild = Bundle.main.object(forInfoDictionaryKey: "TiziBuildVersion") as? String
    private var engineModified: Date?

    private init() {
        client.onEvent = { [weak self] name, line in self?.handleEvent(name, line) }
        client.onExit = { [weak self] status, stderr in self?.engineExited(status: status, stderr: stderr) }
    }

    // MARK: 派生状态

    var running: Bool { snapshot?.managed.running ?? false }

    /// 引擎正在执行的操作，或本地刚发起、快照还没反映的操作。
    var busyLabel: String? { snapshot?.busy ?? pendingActions.values.first }

    var menuBarSymbol: String {
        switch engineState {
        case .missing, .unsupported, .crashed: return "exclamationmark.shield"
        case .starting: return "shield"
        case .running: break
        }
        if busyLabel != nil { return "arrow.triangle.2.circlepath" }
        return running ? "checkmark.shield.fill" : "shield.slash"
    }

    var statusTitle: String {
        switch engineState {
        case .starting: return "正在连接后台引擎"
        case .missing: return "未找到 tizi"
        case .unsupported: return "需要更新 tizi"
        case .crashed: return "后台引擎已退出"
        case .running: break
        }
        if let busy = busyLabel { return (mode == .simple ? Self.plainBusy(busy) : "正在\(busy)") + "…" }
        return running ? "已连接" : "未连接"
    }

    /// 简洁模式把引擎的操作名换成不含 Mihomo、DNS 等术语的说法。
    static func plainBusy(_ label: String) -> String {
        switch label {
        case "启动 Mihomo": return "正在连接"
        case "停止 Mihomo": return "正在断开"
        case "重载 Mihomo": return "正在重新连接"
        case "安装 Mihomo 核心": return "正在下载必要组件"
        case "更新 APP": return "正在更新"
        case "恢复系统 DNS": return "正在修复网络设置"
        case "刷新微信直连": return "正在刷新微信连接"
        case "生成并校验配置": return "正在检查配置"
        case "保存设置": return "正在保存"
        case "配置直连保护": return "正在设置直连保护"
        default: return "正在\(label)"
        }
    }

    // MARK: 引擎生命周期

    func startEngine() {
        Self.installBundledEngineIfNeeded()
        guard let path = Self.locateEngine() else {
            enginePath = nil
            engineState = .missing
            reloadAfterRestart = false
            return
        }
        enginePath = path
        engineModified = Self.modificationDate(of: path)
        engineState = .starting
        hello = nil
        do {
            try client.launch(executable: path, arguments: ["_gui-engine", "--askpass", Self.askPassPath()])
        } catch {
            engineState = .crashed("无法启动 \(path.path)：\(error.localizedDescription)")
            reloadAfterRestart = false
            return
        }
        Task {
            do {
                let hello: Hello = try await client.call("hello")
                guard hello.protocol == supportedProtocol else {
                    engineState = .unsupported("tizi \(hello.version) 的引擎协议 v\(hello.protocol) 与桌面版（v\(supportedProtocol)）不兼容。请同时更新两者。")
                    reloadAfterRestart = false
                    client.shutdown()
                    return
                }
                self.hello = hello
                engineState = .running
                if reloadAfterRestart {
                    reloadAfterRestart = false
                    // 只在 Mihomo 仍在运行时重载；未运行时 reload 会顺带启动 VPN。
                    for _ in 0..<50 where snapshot == nil {
                        try? await Task.sleep(nanoseconds: 100_000_000)
                    }
                    if running { await reload() }
                }
            } catch {
                // 进程已退出时由 engineExited 给出更准确的原因。
                reloadAfterRestart = false
                if client.isRunning {
                    engineState = .crashed("后台引擎没有响应：\(error.localizedDescription)")
                }
            }
        }
    }

    /// 用磁盘上的新版本重新启动引擎；reload 为真时随后重载 Mihomo，让新规则生效。
    func restartEngine(reload: Bool) {
        reloadAfterRestart = reload
        guard client.isRunning else {
            startEngine()
            return
        }
        restarting = true
        snapshot = nil
        client.shutdown()
    }

    /// 关闭引擎输入后立即返回：引擎会自己走完已开始的操作，并关掉无人处理的
    /// 密码框，不需要界面等待。
    func shutdownEngine() {
        client.shutdown()
    }

    private func engineExited(status: Int32, stderr: String) {
        if restarting {
            restarting = false
            startEngine()
            return
        }
        pendingActions = [:]
        reloadAfterRestart = false
        // 协议不兼容时是桌面版主动关闭了引擎，保留「需要更新」的说明。
        if case .unsupported = engineState { return }
        if stderr.contains("未知命令"), let path = enginePath {
            let version = Self.engineVersion(at: path).map { "（\($0)）" } ?? ""
            engineState = .unsupported("已安装的 tizi\(version)还不支持桌面版。请更新到最新版本后点「重新检测」。")
            return
        }
        let detail = stderr.trimmingCharacters(in: .whitespacesAndNewlines)
        engineState = .crashed(detail.isEmpty ? "退出码 \(status)" : detail)
    }

    static func locateEngine() -> URL? {
        let fileManager = FileManager.default
        var candidates: [String] = []
        if let override = UserDefaults.standard.string(forKey: "EnginePath"), !override.isEmpty {
            candidates.append((override as NSString).expandingTildeInPath)
        }
        if let env = ProcessInfo.processInfo.environment["TIZI_BIN"], !env.isEmpty {
            candidates.append(env)
        }
        let home = fileManager.homeDirectoryForCurrentUser.path
        candidates += ["\(home)/.local/bin/tizi", "/opt/homebrew/bin/tizi", "/usr/local/bin/tizi"]
        return candidates.first(where: fileManager.isExecutableFile(atPath:)).map { URL(fileURLWithPath: $0) }
    }

    /// 通过 dmg 分发的 .app 自带引擎（Contents/Helpers）。没有安装引擎、或已安装的
    /// 比自带的旧时，把它装到 ~/.local/bin，之后的更新、终端版都沿用现有逻辑。
    /// 显式指定了 EnginePath 或 TIZI_BIN（开发场景）时不动用户的安装。
    static func installBundledEngineIfNeeded() {
        let fileManager = FileManager.default
        let bundled = Bundle.main.bundleURL.appendingPathComponent("Contents/Helpers/tizi")
        guard fileManager.isExecutableFile(atPath: bundled.path) else { return }
        if let override = UserDefaults.standard.string(forKey: "EnginePath"), !override.isEmpty { return }
        if let env = ProcessInfo.processInfo.environment["TIZI_BIN"], !env.isEmpty { return }

        let directory = fileManager.homeDirectoryForCurrentUser.appendingPathComponent(".local/bin")
        let target = directory.appendingPathComponent("tizi")
        if fileManager.isExecutableFile(atPath: target.path) {
            guard let have = engineVersion(at: target).flatMap(parseVersion),
                  let want = engineVersion(at: bundled).flatMap(parseVersion),
                  have.lexicographicallyPrecedes(want) else { return }
        }
        let staged = directory.appendingPathComponent(".tizi.\(getpid())")
        do {
            try fileManager.createDirectory(at: directory, withIntermediateDirectories: true)
            try? fileManager.removeItem(at: staged)
            try fileManager.copyItem(at: bundled, to: staged)
            try fileManager.setAttributes([.posixPermissions: 0o755], ofItemAtPath: staged.path)
            // rename 原子替换；正在运行的旧引擎仍持有旧文件，不受影响。
            guard rename(staged.path, target.path) == 0 else { throw CocoaError(.fileWriteUnknown) }
        } catch {
            try? fileManager.removeItem(at: staged)
            return
        }
        // 与 install.sh 一致：生成默认的用户配置（custom-rules.txt 等）。
        let process = Process()
        process.executableURL = target
        process.arguments = ["init"]
        process.standardOutput = FileHandle.nullDevice
        process.standardError = FileHandle.nullDevice
        if (try? process.run()) != nil { process.waitUntilExit() }
    }

    /// "v0.2.26"、"v0.2.26-3-gabc-dirty" 都取前面的数字三段；无法识别时返回 nil。
    static func parseVersion(_ text: String) -> [Int]? {
        let core = text.drop(while: { $0 == "v" }).prefix(while: { $0.isNumber || $0 == "." })
        let parts = core.split(separator: ".").compactMap { Int($0) }
        return parts.count == 3 ? parts : nil
    }

    /// 读取 `tizi version` 的版本号，用于提示旧版本需要更新。
    static func engineVersion(at path: URL) -> String? {
        let process = Process()
        process.executableURL = path
        process.arguments = ["version"]
        let output = Pipe()
        process.standardOutput = output
        process.standardError = FileHandle.nullDevice
        guard (try? process.run()) != nil else { return nil }
        process.waitUntilExit()
        let text = String(decoding: output.fileHandleForReading.readDataToEndOfFile(), as: UTF8.self)
        return text.split(separator: " ").last.map { $0.trimmingCharacters(in: .whitespacesAndNewlines) }
    }

    /// 打包后的 askpass 在 Contents/Helpers 内独立的 .app 中；`swift run`
    /// 开发时退回到与主程序同目录的可执行文件。
    static func askPassPath() -> String {
        let bundled = Bundle.main.bundleURL
            .appendingPathComponent("Contents/Helpers/TiziAskPass.app/Contents/MacOS/TiziAskPass")
        if FileManager.default.isExecutableFile(atPath: bundled.path) {
            return bundled.path
        }
        return Bundle.main.executableURL!.deletingLastPathComponent().appendingPathComponent("TiziAskPass").path
    }

    private func handleEvent(_ name: String, _ line: Data) {
        switch name {
        case "snapshot":
            guard let value = EngineClient.decodeEvent(Snapshot.self, from: line) else { return }
            snapshot = value
            adoptReplacedBinaries()
            evaluateClaudeAlert(value)
            if value.install == nil { install = nil }
            if let share = value.share {
                activeShare = share
            } else if let received = shareReceivedAt, TimeInterval(value.at) / 1000 > received.timeIntervalSince1970 {
                activeShare = nil
                shareReceivedAt = nil
            }
        case "install":
            install = EngineClient.decodeEvent(InstallProgress.self, from: line)
        default:
            break
        }
    }

    // MARK: Claude 访问监测

    /// 当前监测结论；没有开启监测时为 nil。
    var claudeStatus: ClaudeStatus? {
        guard let snapshot, snapshot.settings.claudeGuardActive == true else { return nil }
        return snapshot.claude
    }

    /// 出现新的告警原因时弹出置顶的大窗口；恢复后清空记录。
    private func evaluateClaudeAlert(_ snapshot: Snapshot) {
        guard snapshot.settings.claudeGuardActive == true, let claude = snapshot.claude else {
            claudeAlertedKeys = []
            ClaudeAlertWindow.shared.close()
            return
        }
        switch claude.level {
        case "alert":
            guard let key = claude.key, !claudeAlertedKeys.contains(key) else { return }
            claudeAlertedKeys.insert(key)
            ClaudeAlertWindow.shared.show()
        case "checking":
            break
        default:
            // 复查中（偶发失败）不算恢复，保留记录，免得抖动时反复弹窗。
            if !claude.rechecking { claudeAlertedKeys = [] }
        }
    }

    func checkClaude() {
        Task { _ = try? await client.call("claudeCheck", as: ActionResult.self) }
    }

    /// 住宅节点不可用时 Claude 可以临时改走的选项：「Claude」策略组里第一个
    /// 不是住宅节点的选项（通常是「自动选择」）。没有时为 nil。
    var claudeFallbackChoice: String? {
        snapshot?.groups.first(where: { $0.name == Self.claudeGroup })?.choices
            .first(where: { $0 != Self.residentialNode && $0 != "REJECT" })
    }

    /// 住宅节点不可用时临时让 Claude 改走其他节点。这是用户明确的选择，
    /// 随之出现的「没有经过住宅节点」不再弹窗。
    func useFallbackForClaude() {
        guard let choice = claudeFallbackChoice else {
            show("「\(Self.claudeGroup)」里没有住宅节点以外的选项", error: true)
            return
        }
        claudeAlertedKeys.insert("not_residential")
        selectPolicy(group: Self.claudeGroup, choice: choice)
    }

    static let claudeGroup = "Claude"
    static let residentialNode = "住宅节点"

    /// 安装或卸载直连保护（防火墙），会弹出管理员密码框。
    func setClaudeBlock(_ enabled: Bool) async {
        await perform("配置直连保护", "setClaudeBlock", ["enabled": enabled])
    }

    func useResidentialForClaude() {
        selectPolicy(group: Self.claudeGroup, choice: Self.residentialNode)
    }

    /// 打开 claude-proxy.yaml；不存在时引擎先写入只有注释的说明。
    func openClaudeProxyFile() async {
        await openEngineFile("claudeProxyFile")
    }

    /// 打开节点文件 proxies.yaml；不存在时引擎先写入只有注释的说明。
    /// 简洁模式下已连接时提示怎样让改动生效。
    func openProxiesFile() async {
        guard await openEngineFile("proxiesFile") else { return }
        if mode == .simple && running {
            show("改好后保存文件，再点「遇到问题 → 重新连接」生效")
        }
    }

    /// 向引擎要文件路径（method 负责在文件不存在时写入模板）并打开它；
    /// 没有关联应用时用「文本编辑」打开。
    @discardableResult
    private func openEngineFile(_ method: String) async -> Bool {
        do {
            let path: String = try await client.call(method)
            let url = URL(fileURLWithPath: path)
            if !NSWorkspace.shared.open(url) {
                let textEdit = URL(fileURLWithPath: "/System/Applications/TextEdit.app")
                NSWorkspace.shared.open([url], withApplicationAt: textEdit, configuration: NSWorkspace.OpenConfiguration(), completionHandler: nil)
            }
            return true
        } catch {
            show(error.localizedDescription, error: true)
            return false
        }
    }

    /// 打开主窗口并切到状态页；告警窗口上的「打开 Tizi」使用。
    func showMainWindow() {
        if mode == .advanced { page = .status }
        NSApp.activate(ignoringOtherApps: true)
        // SwiftUI 给 Window(id: "main") 的窗口标识以 main 开头。
        if let window = NSApp.windows.first(where: { $0.identifier?.rawValue.hasPrefix("main") == true }), window.isVisible {
            window.makeKeyAndOrderFront(nil)
        } else if let openMainWindow {
            openMainWindow()
        } else {
            // 主窗口已关闭、也没有视图注册过打开方法：向自己发送「重新打开」，
            // 由 applicationShouldHandleReopen 打开主窗口。
            NSWorkspace.shared.openApplication(at: Bundle.main.bundleURL, configuration: NSWorkspace.OpenConfiguration())
        }
    }

    /// 由能拿到 openWindow 环境值的视图（菜单栏菜单）注册，用来重新打开已关闭的主窗口。
    var openMainWindow: (() -> Void)?

    // MARK: 提示

    func show(_ text: String, error: Bool = false) {
        let notice = Notice(text: text, isError: error)
        self.notice = notice
        noticeTask?.cancel()
        noticeTask = Task {
            try? await Task.sleep(nanoseconds: (error ? 15 : 8) * 1_000_000_000)
            if !Task.isCancelled, self.notice == notice { self.notice = nil }
        }
    }

    func dismissNotice() {
        noticeTask?.cancel()
        notice = nil
    }

    // MARK: 操作

    /// 执行一个改变状态的引擎操作。handle 返回 true 表示错误已被专门处理；
    /// announce 为假时成功不弹提示（简洁模式由状态本身说明结果）。
    @discardableResult
    private func perform(_ label: String, _ method: String, _ params: [String: Any]? = nil,
                         announce: Bool = true, handle: ((EngineError) -> Bool)? = nil) async -> Bool {
        let token = UUID()
        pendingActions[token] = label
        defer { pendingActions[token] = nil }
        if Self.mayNeedAuthorization.contains(method) { Self.allowAskPassActivation() }
        do {
            let result: ActionResult = try await client.call(method, params)
            if announce { show(result.message) }
            return true
        } catch let error as EngineError {
            if handle?(error) != true { show(error.message, error: true) }
        } catch {
            show(error.localizedDescription, error: true)
        }
        return false
    }

    private static let mayNeedAuthorization: Set<String> = ["start", "stop", "reload", "recoverDNS", "setClaudeBlock"]
    private static let askPassBundleID = "com.kadaliao.tizi.desktop.askpass"

    /// macOS 14 起激活是协作式的：先允许密码框接过焦点，否则它可能出现在前台
    /// 却拿不到键盘输入。
    private static func allowAskPassActivation() {
        if #available(macOS 14.0, *) {
            NSApp.yieldActivation(toApplicationWithBundleIdentifier: askPassBundleID)
        }
    }

    func toggleConnection() async {
        if running {
            await stop()
        } else {
            await start()
        }
    }

    /// 一键连接：核心还没装时先装好再启动，用户不需要知道有「核心」这一步。
    func start(allowConflict: Bool = false, stopOpenVPN: Bool = false) async {
        if snapshot?.managed.coreInstalled == false {
            guard await perform("安装 Mihomo 核心", "installCore", announce: mode == .advanced) else { return }
        }
        await perform("启动 Mihomo", "start", ["allowConflict": allowConflict, "stopOpenVPN": stopOpenVPN],
                      announce: mode == .advanced) { error in
            switch error.code {
            case "vpn_conflict":
                self.conflictPrompt = true
                return true
            case "not_configured":
                // 简洁模式由首次设置页引导；高级模式转到配置页。
                self.page = .config
                self.show(error.message, error: true)
                return true
            default:
                return false
            }
        }
    }

    @discardableResult
    func stop() async -> Bool {
        await perform("停止 Mihomo", "stop", announce: mode == .advanced)
    }

    func reload() async {
        await perform("重载 Mihomo", "reload", announce: mode == .advanced) { error in
            if error.code == "vpn_conflict" {
                self.conflictPrompt = true
                return true
            }
            return false
        }
    }

    func validate() async { await perform("生成并校验配置", "validate") }
    func installCore() async { await perform("安装 Mihomo 核心", "installCore") }
    func recoverDNS() async {
        let simple = mode == .simple
        if await perform("恢复系统 DNS", "recoverDNS", announce: !simple), simple {
            show("网络设置已恢复")
        }
    }

    /// 「网页打不开」：先重新测速，让自动选择换掉坏节点，再经外网代理试一次；
    /// 通了就不用重新连接（重新连接要输密码），还是不通再重新连接。
    func fixBrowsing() async {
        guard snapshot?.controllerOnline == true else {
            await reload()
            return
        }
        let token = UUID()
        pendingActions[token] = "检查线路"
        let result = try? await client.call("fixBrowsing", as: BrowsingCheck.self)
        pendingActions[token] = nil
        if let result, result.ok {
            show(result.message)
            return
        }
        if let result { show(result.message) }
        await reload()
    }

    func recoverWeChat() async {
        let simple = mode == .simple
        if await perform("刷新微信直连", "recoverWeChat", announce: !simple), simple {
            show("已刷新微信的连接，请重新发送图片")
        }
    }
    func installUpdate() async {
        let token = UUID()
        pendingActions[token] = "更新 APP"
        defer { pendingActions[token] = nil }
        do {
            let result: UpdateResult = try await client.call("installUpdate")
            if let desktop = result.desktopApp, Self.samePath(desktop, Bundle.main.bundlePath) {
                relaunch(reload: running)
                return
            }
            if let desktop = result.desktopApp {
                show("\(result.message)。\(desktop) 也已更新。")
            } else {
                show(result.message)
            }
        } catch {
            show(error.localizedDescription, error: true)
        }
    }

    // MARK: 检查更新

    /// 用户点「检查更新」：总是重新查询，用对话框给出结果，有新版本时可以直接更新。
    /// 菜单栏应用不一定有可见窗口，所以不用窗口顶部的提示条。
    func checkForUpdates() async {
        guard engineState == .running, !checkingUpdate else { return }
        checkingUpdate = true
        let info: UpdateInfo
        do {
            info = try await client.call("checkUpdate")
        } catch {
            checkingUpdate = false
            _ = askAboutUpdate("检查更新失败", error.localizedDescription)
            return
        }
        checkingUpdate = false
        let current = hello?.version ?? "未知"
        if let installed = info.installed {
            if askAboutUpdate("新版本 \(installed) 已下载", "重启后台引擎后生效。", action: "立即生效"), readyToUpdate() {
                restartEngine(reload: running)
            }
        } else if info.available, let latest = info.latest {
            if askAboutUpdate("发现新版本 \(latest)", "当前版本 \(current)。", action: "立即更新"), readyToUpdate() {
                await installUpdate()
            }
        } else if let error = info.error {
            _ = askAboutUpdate("检查更新失败", "需要能访问 GitHub 才能检查更新。\n\n\(error)")
        } else {
            _ = askAboutUpdate("已是最新版本", "当前版本 \(current)。")
        }
    }

    /// 打开菜单栏菜单时调用。5 分钟内查过就沿用结果；有新版本时快照随即更新，
    /// 菜单里的「检查更新…」变成「更新到 …」。
    func checkForUpdatesQuietly() {
        guard engineState == .running else { return }
        Task { _ = try? await client.call("checkUpdate", ["maxAge": 5 * 60], as: UpdateInfo.self) }
    }

    /// Mac 唤醒后检查一次。引擎每 6 小时的定时检查只算醒着的时间（Go 在 macOS 上
    /// 的计时器睡眠时不走），合盖多的笔记本要隔好几天才会查。刚唤醒时网络和 VPN
    /// 还没恢复，先等 20 秒，失败再试两次；连续几次唤醒只保留最后一次。
    func checkForUpdatesAfterWake() {
        wakeUpdateCheck?.cancel()
        wakeUpdateCheck = Task {
            for delay: UInt64 in [20, 60, 120] {
                try? await Task.sleep(nanoseconds: delay * 1_000_000_000)
                if Task.isCancelled { return }
                guard engineState == .running else { continue }
                let info = try? await client.call("checkUpdate", ["maxAge": 60], as: UpdateInfo.self)
                if let info, info.error == nil { return }
            }
        }
    }

    /// 显示检查结果；action 非空时多一个「稍后」，返回是否点了 action。
    private func askAboutUpdate(_ title: String, _ detail: String, action: String? = nil) -> Bool {
        let alert = NSAlert()
        alert.messageText = title
        alert.informativeText = detail
        alert.addButton(withTitle: action ?? "好")
        if action != nil { alert.addButton(withTitle: "稍后") }
        NSApp.activate(ignoringOtherApps: true)
        return alert.runModal() == .alertFirstButtonReturn && action != nil
    }

    /// 对话框开着时可能开始了别的操作；更新和重启引擎都要等它结束。
    private func readyToUpdate() -> Bool {
        guard let busy = busyLabel else { return true }
        let label = mode == .simple ? Self.plainBusy(busy) : "正在\(busy)"
        _ = askAboutUpdate("请稍后再更新", "\(label)，完成后再试。")
        return false
    }

    /// 等当前进程完全退出后重新打开同一个 .app；VPN 保持运行。
    func relaunch(reload: Bool) {
        let process = Process()
        process.executableURL = URL(fileURLWithPath: "/bin/sh")
        // open 遇到仍在退出的旧实例只会激活它，所以先等旧进程消失。
        process.arguments = [
            "-c", "while kill -0 \"$1\" 2>/dev/null; do sleep 0.2; done; exec /usr/bin/open \"$2\" ${3:+--args \"$3\"}",
            "relaunch", String(ProcessInfo.processInfo.processIdentifier), Bundle.main.bundlePath,
            reload ? "--reload-mihomo" : "",
        ]
        do {
            try process.run()
        } catch {
            show("桌面版已更新，但无法自动重新打开：\(error.localizedDescription)。请手动退出后重新打开。", error: true)
            return
        }
        relaunching = true
        shutdownEngine()
        NSApp.terminate(nil)
    }

    /// 终端版或命令行更新了 APP 时，正在运行的桌面版和引擎还是旧代码：
    /// bundle 被换成其他版本就重新打开自己；只有引擎程序被换掉（例如桌面版
    /// 没有一起更新）就重启引擎。VPN 都保持运行，新版本的重载由更新方负责。
    /// 有操作进行时不动，桌面版自己的「更新 APP」也由它自己的流程收尾。
    private func adoptReplacedBinaries() {
        guard busyLabel == nil, !relaunching, !restarting else { return }
        if let launchedBuild, let onDisk = Self.bundleBuildOnDisk(), onDisk != launchedBuild {
            relaunch(reload: false)
            return
        }
        // 更新先换引擎、紧接着换 bundle；等 10 秒再判断，免得先重启引擎、
        // 马上又重新打开整个应用。引擎自己装的更新由状态页提示重启并重载。
        guard snapshot?.update.installed == nil, let path = enginePath, let started = engineModified,
              let current = Self.modificationDate(of: path), current != started,
              Date().timeIntervalSince(current) > 10 else { return }
        restartEngine(reload: false)
    }

    private static func bundleBuildOnDisk() -> String? {
        let plist = Bundle.main.bundleURL.appendingPathComponent("Contents/Info.plist")
        return NSDictionary(contentsOf: plist)?["TiziBuildVersion"] as? String
    }

    private static func modificationDate(of path: URL) -> Date? {
        let attributes = try? FileManager.default.attributesOfItem(atPath: path.resolvingSymlinksInPath().path)
        return attributes?[.modificationDate] as? Date
    }

    private static func samePath(_ lhs: String, _ rhs: String) -> Bool {
        URL(fileURLWithPath: lhs).resolvingSymlinksInPath().standardizedFileURL
            == URL(fileURLWithPath: rhs).resolvingSymlinksInPath().standardizedFileURL
    }

    func refresh() {
        Task { _ = try? await client.call("refresh", as: Empty.self) }
    }

    /// 立即改写本地快照再提交，开关不必等下一次推送才回弹；失败时下一次快照会纠正。
    func updateSettings(_ patch: [String: Any], optimistic: ((inout EngineSettings) -> Void)? = nil) {
        if let optimistic, snapshot != nil { optimistic(&snapshot!.settings) }
        Task { await perform("保存设置", "updateSettings", patch) }
    }

    /// 保存设置并等待结果，成功返回 true；表单据此清空已提交的草稿。
    @discardableResult
    func saveSettings(_ patch: [String: Any]) async -> Bool {
        await perform("保存设置", "updateSettings", patch)
    }

    /// 按项保存住宅节点；password 为 nil 时引擎沿用已保存的密码，direct 不为
    /// nil 时连同连接方式一起保存。成功返回 true。
    func saveClaudeNode(scheme: String, host: String, port: Int, username: String, password: String?,
                        direct: Bool? = nil) async -> Bool {
        var node: [String: Any] = ["scheme": scheme, "host": host, "port": port, "username": username]
        if let password { node["password"] = password }
        var patch: [String: Any] = ["claudeNode": node]
        if let direct {
            patch["claudeProxyDirect"] = direct
            if snapshot != nil { snapshot!.settings.claudeProxyDirect = direct }
        }
        return await perform("保存设置", "updateSettings", patch)
    }

    /// 让引擎识别粘贴的整段住宅节点地址（用户名密码在前或在后都可以）。
    func parseClaudeAddress(_ text: String) async throws -> ParsedClaudeNode {
        try await client.call("parseClaudeAddress", ["text": text])
    }

    /// 正在测速：用户点了「测速」，或者切到自动选择后引擎在重新测速。
    var delayTesting: Bool { testingDelays || snapshot?.delayTesting == true }

    /// 测一遍可以上外网的节点，测完后自动选择按新结果重新挑节点。不经过
    /// perform：测速只读节点状态，不应该让连接按钮跟着变灰。
    func testDelays() async {
        testingDelays = true
        defer { testingDelays = false }
        do {
            let result: ActionResult = try await client.call("testDelays")
            show(result.message)
        } catch {
            show(error.localizedDescription, error: true)
        }
    }

    func selectPolicy(group: String, choice: String) {
        if let index = snapshot?.groups.firstIndex(where: { $0.name == group }) {
            snapshot?.groups[index].now = choice
        }
        Task { await perform("切换策略", "selectPolicy", ["group": group, "choice": choice]) }
    }

    /// 简洁模式改线路（订阅、.ovpn）：连接中就用新设置重新连接，不需要用户
    /// 再点一次。成功返回 true。
    @discardableResult
    func changeRoutes(_ patch: [String: Any]) async -> Bool {
        let reconnect = running
        guard await perform("保存设置", "updateSettings", patch, announce: false) else { return false }
        if reconnect { await reload() }
        return true
    }

    /// 导入或更换内网 .ovpn；path 为空表示移除。
    func useOVPN(_ path: String) async {
        await changeRoutes(["ovpnPath": path])
    }

    /// 保存订阅地址；空字符串表示清除。
    @discardableResult
    func setSubscription(_ url: String) async -> Bool {
        await changeRoutes(["subscription": url.trimmingCharacters(in: .whitespacesAndNewlines)])
    }

    func scanOVPN(quiet: Bool = false) async {
        scannedOVPN = (try? await client.call("scanOVPN", as: [String].self)) ?? []
        if scannedOVPN.isEmpty && !quiet { show("未在下载、桌面或配置目录发现 .ovpn 文件", error: true) }
    }

    func loadDiagnostics() async {
        diagnosticsLoading = true
        defer { diagnosticsLoading = false }
        do {
            diagnostics = try await client.call("diagnostics")
        } catch {
            show(error.localizedDescription, error: true)
        }
    }

    func loadLogs(limit: Int = 1000) async {
        if let lines = try? await client.call("logs", ["limit": limit], as: [String].self) {
            logs = lines
        }
    }

    func loadRules() async {
        rules = try? await client.call("rules")
    }

    func testRoute(_ input: String) async {
        routeTesting = true
        defer { routeTesting = false }
        routeTest = nil
        do {
            routeTest = try await client.call("testRoute", ["input": input])
        } catch {
            show(error.localizedDescription, error: true)
        }
    }

    func saveRule(_ input: String, target: String) async {
        if await perform("保存规则", "saveRule", ["input": input, "target": target]) {
            await loadRules()
        }
    }

    func startShare() async {
        do {
            activeShare = try await client.call("shareStart", as: Share.self)
            shareReceivedAt = Date()
            showShare = true
        } catch {
            show(error.localizedDescription, error: true)
        }
    }

    func stopShare() {
        showShare = false
        activeShare = nil
        shareReceivedAt = nil
        Task { _ = try? await client.call("shareStop", as: ActionResult.self) }
    }

    /// 局域网代理认证的用户名和密码，给其他设备配置代理时显示或复制。
    func proxyCredentials() async -> ProxyCredentials? {
        do {
            return try await client.call("proxyCredentials")
        } catch {
            show(error.localizedDescription, error: true)
            return nil
        }
    }

    func openDashboard() async {
        do {
            let url: String = try await client.call("dashboardURL")
            if let target = URL(string: url) { NSWorkspace.shared.open(target) }
        } catch {
            show(error.localizedDescription, error: true)
        }
    }

    func revealLog() {
        guard let path = hello?.logFile else { return }
        if FileManager.default.fileExists(atPath: path) {
            NSWorkspace.shared.activateFileViewerSelecting([URL(fileURLWithPath: path)])
        } else {
            show("日志文件尚不存在；启动 Mihomo 后生成", error: true)
        }
    }

    func openCustomRules() {
        guard let path = hello?.customRules else { return }
        NSWorkspace.shared.open(URL(fileURLWithPath: path))
    }
}
