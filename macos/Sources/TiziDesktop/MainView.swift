import AppKit
import SwiftUI

struct MainView: View {
    @EnvironmentObject private var model: AppModel

    var body: some View {
        NavigationSplitView {
            List(Page.allCases, selection: pageSelection) { page in
                Label(page.title, systemImage: page.symbol)
                    .tag(page)
            }
            .navigationSplitViewColumnWidth(min: 150, ideal: 170, max: 220)
            .safeAreaInset(edge: .bottom) { SidebarStatus() }
        } detail: {
            VStack(spacing: 0) {
                if let notice = model.notice {
                    NoticeBar(notice: notice)
                }
                content
                    .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
            }
            .navigationTitle(model.page.title)
        }
    }

    private var pageSelection: Binding<Page?> {
        Binding(get: { model.page }, set: { if let page = $0 { model.page = page } })
    }

    @ViewBuilder
    private var content: some View {
        switch model.engineState {
        case .running:
            switch model.page {
            case .status: StatusPage()
            case .config: ConfigPage()
            case .policy: PolicyPage()
            case .rules: RulesPage()
            case .connections: ConnectionsPage()
            case .logs: LogsPage()
            case .diagnostics: DiagnosticsPage()
            }
        default:
            EngineStateView()
        }
    }
}

private struct SidebarStatus: View {
    @EnvironmentObject private var model: AppModel

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            Label(model.statusTitle, systemImage: model.menuBarSymbol)
                .font(.callout.weight(.medium))
                .foregroundStyle(model.running ? Color.green : Color.secondary)
                .lineLimit(1)
            if let traffic = model.snapshot?.traffic, model.running {
                Text("↓ \(Format.rate(traffic.download))  ↑ \(Format.rate(traffic.upload))")
                    .font(.caption.monospacedDigit())
                    .foregroundStyle(.secondary)
            }
            Button("切换到简洁模式") { model.mode = .simple }
                .buttonStyle(.link)
                .font(.caption)
                .padding(.top, 4)
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .padding(12)
    }
}

struct NoticeBar: View {
    @EnvironmentObject private var model: AppModel
    let notice: Notice

    var body: some View {
        HStack(alignment: .top, spacing: 8) {
            Image(systemName: notice.isError ? "exclamationmark.triangle.fill" : "checkmark.circle.fill")
                .foregroundStyle(notice.isError ? Color.orange : Color.green)
            Text(notice.text)
                .textSelection(.enabled)
                .frame(maxWidth: .infinity, alignment: .leading)
            Button {
                model.dismissNotice()
            } label: {
                Image(systemName: "xmark")
            }
            .buttonStyle(.borderless)
        }
        .font(.callout)
        .padding(.horizontal, 16)
        .padding(.vertical, 10)
        .background(notice.isError ? Color.orange.opacity(0.12) : Color.green.opacity(0.10))
    }
}

/// 引擎不可用时的引导页：找不到程序、版本过旧或意外退出。
struct EngineStateView: View {
    @EnvironmentObject private var model: AppModel

    var body: some View {
        VStack(alignment: .leading, spacing: 16) {
            switch model.engineState {
            case .starting:
                ProgressView("正在启动后台引擎…")
            case .missing:
                Label("没有找到 tizi", systemImage: "questionmark.folder")
                    .font(.title2.weight(.semibold))
                Text("桌面版复用终端版的全部连接能力，需要先安装 tizi。在“终端”中执行：")
                CommandBox(command: "curl -fsSL https://raw.github.com/kadaliao/tizi/main/install.sh | sh")
                Text("安装位置默认是 ~/.local/bin/tizi。装在其他位置时，可执行 defaults write com.kadaliao.tizi.desktop EnginePath <路径> 指定。")
                    .font(.callout)
                    .foregroundStyle(.secondary)
                retryButton
            case .unsupported(let detail):
                Label("需要更新 tizi", systemImage: "arrow.down.circle")
                    .font(.title2.weight(.semibold))
                Text(detail).textSelection(.enabled)
                CommandBox(command: "\(model.enginePath?.path ?? "tizi") update")
                retryButton
            case .crashed(let detail):
                Label("后台引擎已退出", systemImage: "exclamationmark.octagon")
                    .font(.title2.weight(.semibold))
                Text("网络连接本身不受影响：已经启动的 Mihomo 会继续运行。重新启动引擎后可继续管理。")
                ScrollView {
                    Text(detail)
                        .font(.callout.monospaced())
                        .textSelection(.enabled)
                        .frame(maxWidth: .infinity, alignment: .leading)
                }
                .frame(maxHeight: 180)
                .padding(8)
                .background(RoundedRectangle(cornerRadius: 6).fill(Color.secondary.opacity(0.08)))
                retryButton
            case .running:
                EmptyView()
            }
        }
        .frame(maxWidth: 620, alignment: .leading)
        .padding(28)
    }

    private var retryButton: some View {
        Button("重新检测") { model.startEngine() }
            .keyboardShortcut(.defaultAction)
    }
}

struct CommandBox: View {
    let command: String

    var body: some View {
        HStack {
            Text(command)
                .font(.callout.monospaced())
                .textSelection(.enabled)
                .frame(maxWidth: .infinity, alignment: .leading)
            Button("复制") {
                NSPasteboard.general.clearContents()
                NSPasteboard.general.setString(command, forType: .string)
            }
        }
        .padding(10)
        .background(RoundedRectangle(cornerRadius: 6).fill(Color.secondary.opacity(0.08)))
    }
}

/// 表单分组下方的说明文字；分组表单默认让多行说明右对齐，这里统一左对齐。
struct FormNote: View {
    let text: String

    init(_ text: String) {
        self.text = text
    }

    var body: some View {
        Text(text)
            .font(.callout)
            .foregroundStyle(.secondary)
            .multilineTextAlignment(.leading)
            .frame(maxWidth: .infinity, alignment: .leading)
    }
}

/// 空状态占位（ContentUnavailableView 需要 macOS 14）。
struct Placeholder: View {
    let title: String
    let symbol: String
    var detail: String?

    var body: some View {
        VStack(spacing: 10) {
            Image(systemName: symbol)
                .font(.system(size: 34))
                .foregroundStyle(.tertiary)
            Text(title).font(.headline)
            if let detail {
                Text(detail)
                    .font(.callout)
                    .foregroundStyle(.secondary)
                    .multilineTextAlignment(.center)
            }
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
        .padding(32)
    }
}
