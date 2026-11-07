import SwiftUI

struct LogsPage: View {
    @EnvironmentObject private var model: AppModel
    @State private var query = ""
    @State private var follow = true
    @State private var minimum = LogLevelFilter.all

    var body: some View {
        let entries = filtered
        VStack(spacing: 0) {
            HStack(spacing: 12) {
                Picker("级别", selection: $minimum) {
                    ForEach(LogLevelFilter.allCases, id: \.self) { level in
                        Text(level.title).tag(level)
                    }
                }
                .pickerStyle(.segmented)
                .labelsHidden()
                .fixedSize()
                Text(entries.count == model.logs.count ? "最近 \(model.logs.count) 行" : "显示 \(entries.count) 行，共 \(model.logs.count) 行")
                    .font(.callout.monospacedDigit())
                    .foregroundStyle(.secondary)
                Spacer()
                Toggle("跟随最新", isOn: $follow)
                    .toggleStyle(.checkbox)
                Button("在 Finder 中显示") { model.revealLog() }
            }
            .padding(.horizontal, 16)
            .padding(.vertical, 10)
            Divider()
            if model.logs.isEmpty {
                Placeholder(title: "尚无日志", symbol: "doc.text",
                            detail: "启动 Mihomo 后会写入：\n\(model.hello?.logFile ?? "mihomo.log")")
            } else {
                ScrollViewReader { proxy in
                    ScrollView {
                        LazyVStack(alignment: .leading, spacing: 0) {
                            ForEach(entries) { entry in
                                LogLine(entry: entry).id(entry.id)
                            }
                        }
                        .padding(.vertical, 6)
                        .textSelection(.enabled)
                    }
                    .onChange(of: model.logs.last) { _ in
                        if follow, let last = filtered.last { proxy.scrollTo(last.id, anchor: .bottom) }
                    }
                    .onAppear {
                        if let last = entries.last { proxy.scrollTo(last.id, anchor: .bottom) }
                    }
                }
            }
        }
        .searchable(text: $query, prompt: "筛选日志")
        .task {
            while !Task.isCancelled {
                await model.loadLogs()
                try? await Task.sleep(nanoseconds: 2_000_000_000)
            }
        }
    }

    private var filtered: [LogEntry] {
        let needle = query.trimmingCharacters(in: .whitespaces).lowercased()
        return model.logs.enumerated().compactMap { index, line in
            if !needle.isEmpty && !line.lowercased().contains(needle) { return nil }
            let entry = LogEntry(id: index, raw: line)
            return minimum.includes(entry.level) ? entry : nil
        }
    }
}

/// 日志页的级别筛选：全部、警告及以上、只看错误。
enum LogLevelFilter: CaseIterable {
    case all, warnings, errors

    var title: String {
        switch self {
        case .all: return "全部"
        case .warnings: return "警告"
        case .errors: return "错误"
        }
    }

    func includes(_ level: String?) -> Bool {
        let severity = LogSeverity(level)
        switch self {
        case .all: return true
        case .warnings: return severity >= .warning
        case .errors: return severity >= .error
        }
    }
}

enum LogSeverity: Int, Comparable {
    case debug, info, warning, error

    init(_ level: String?) {
        switch level?.lowercased() {
        case "error"?, "fatal"?: self = .error
        case "warning"?, "warn"?: self = .warning
        case "debug"?: self = .debug
        default: self = .info
        }
    }

    static func < (lhs: LogSeverity, rhs: LogSeverity) -> Bool { lhs.rawValue < rhs.rawValue }

    var tag: String {
        switch self {
        case .debug: return "DEBUG"
        case .info: return "INFO"
        case .warning: return "WARN"
        case .error: return "ERROR"
        }
    }

    var color: Color {
        switch self {
        case .debug: return .secondary
        case .info: return Color(nsColor: .systemGray)
        case .warning: return .orange
        case .error: return .red
        }
    }
}

/// Mihomo 日志形如 time="…" level=info msg="…"；解析失败时按原文显示。
struct LogEntry: Identifiable {
    let id: Int
    let time: String?
    let level: String?
    let message: String

    init(id: Int, raw: String) {
        self.id = id
        if let match = raw.firstMatch(of: #/^time="[^"]*T(\d{2}:\d{2}:\d{2})[^"]*" level=(\w+) msg="(.*)"$/#) {
            time = String(match.1)
            level = String(match.2)
            message = String(match.3).replacingOccurrences(of: "\\\"", with: "\"")
        } else {
            time = nil
            level = nil
            message = raw
        }
    }
}

/// 一行日志：时间、着色的级别标签和内容；警告和错误整行带底色，翻长日志时
/// 一眼能找到。
private struct LogLine: View {
    let entry: LogEntry

    var body: some View {
        let severity = LogSeverity(entry.level)
        HStack(alignment: .firstTextBaseline, spacing: 10) {
            if let time = entry.time {
                Text(time).foregroundStyle(.tertiary)
            }
            if entry.level != nil {
                Text(severity.tag)
                    .font(.system(size: 9.5, weight: .bold, design: .monospaced))
                    .foregroundStyle(severity >= .warning ? Color.white : severity.color)
                    .frame(width: 42)
                    .padding(.vertical, 1.5)
                    .background(RoundedRectangle(cornerRadius: 3).fill(severity >= .warning ? severity.color : severity.color.opacity(0.15)))
            }
            Text(entry.message)
                .foregroundStyle(severity == .error ? Color.red : (severity == .debug ? Color.secondary : Color.primary))
        }
        .font(.system(size: 11.5, design: .monospaced))
        .padding(.horizontal, 14)
        .padding(.vertical, 3)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(severity >= .warning ? severity.color.opacity(0.07) : Color.clear)
    }
}
