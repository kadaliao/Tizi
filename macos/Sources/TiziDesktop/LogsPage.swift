import SwiftUI

struct LogsPage: View {
    @EnvironmentObject private var model: AppModel
    @State private var query = ""
    @State private var follow = true

    var body: some View {
        let entries = filtered
        VStack(spacing: 0) {
            HStack(spacing: 12) {
                Text(query.isEmpty ? "最近 \(model.logs.count) 行" : "\(entries.count) / \(model.logs.count) 行")
                    .font(.callout.monospacedDigit())
                    .foregroundStyle(.secondary)
                Spacer()
                Toggle("跟随最新", isOn: $follow)
                    .toggleStyle(.checkbox)
                Button("在 Finder 中显示") { model.revealLog() }
            }
            .padding(.horizontal, 16)
            .padding(.vertical, 8)
            Divider()
            if model.logs.isEmpty {
                Placeholder(title: "尚无日志", symbol: "doc.text",
                            detail: "启动 Mihomo 后会写入：\n\(model.hello?.logFile ?? "mihomo.log")")
            } else {
                ScrollViewReader { proxy in
                    ScrollView {
                        LazyVStack(alignment: .leading, spacing: 2) {
                            ForEach(entries) { entry in
                                LogLine(entry: entry).id(entry.id)
                            }
                        }
                        .padding(.horizontal, 12)
                        .padding(.vertical, 8)
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
            return LogEntry(id: index, raw: line)
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

private struct LogLine: View {
    let entry: LogEntry

    var body: some View {
        HStack(alignment: .firstTextBaseline, spacing: 8) {
            if let time = entry.time {
                Text(time).foregroundStyle(.secondary)
            }
            if let level = entry.level {
                Text(level.uppercased())
                    .foregroundStyle(color(level))
                    .frame(width: 44, alignment: .leading)
            }
            Text(entry.message)
                .foregroundStyle(entry.level.map(color) ?? .primary)
        }
        .font(.system(.caption, design: .monospaced))
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    private func color(_ level: String) -> Color {
        switch level.lowercased() {
        case "error", "fatal": return .red
        case "warning", "warn": return .orange
        case "debug": return .secondary
        default: return .primary
        }
    }
}
