import AppKit
import SwiftUI

struct ConnectionsPage: View {
    @EnvironmentObject private var model: AppModel
    @State private var query = ""
    @State private var sortOrder = [KeyPathComparator(\Connection.downloadRate, order: .reverse)]

    var body: some View {
        if let snapshot = model.snapshot, snapshot.controllerOnline {
            let rows = filtered(snapshot.connections).sorted(using: sortOrder)
            VStack(spacing: 0) {
                HStack {
                    Text(summary(snapshot, shown: rows.count))
                        .font(.callout.monospacedDigit())
                        .foregroundStyle(.secondary)
                    Spacer()
                }
                .padding(.horizontal, 16)
                .padding(.vertical, 8)
                Table(rows, sortOrder: $sortOrder) {
                    TableColumn("目标", value: \.destination) { connection in
                        Text(connection.destination).help(connection.destination)
                    }
                    .width(min: 140, ideal: 220)
                    TableColumn("路线", value: \.routeText) { connection in
                        Text(connection.routeText).help(connection.routeText)
                    }
                    .width(min: 90, ideal: 170)
                    TableColumn("↓ 速度", value: \.downloadRate) { connection in
                        Text(connection.sampled ? Format.rate(connection.downloadRate) : "—")
                            .monospacedDigit()
                    }
                    .width(min: 72, ideal: 84)
                    TableColumn("↑ 速度", value: \.uploadRate) { connection in
                        Text(connection.sampled ? Format.rate(connection.uploadRate) : "—")
                            .monospacedDigit()
                    }
                    .width(min: 72, ideal: 84)
                    TableColumn("规则", value: \.ruleText) { connection in
                        Text(connection.ruleText).help(connection.ruleText)
                    }
                    .width(min: 70, ideal: 130)
                    TableColumn("已下载", value: \.download) { connection in
                        Text(Format.bytes(connection.download)).monospacedDigit()
                    }
                    .width(min: 60, ideal: 72)
                    TableColumn("进程", value: \.processName)
                        .width(min: 50, ideal: 80)
                }
                .contextMenu(forSelectionType: Connection.ID.self) { selection in
                    Button("复制目标") {
                        let targets = rows.filter { selection.contains($0.id) }.map(\.destination)
                        NSPasteboard.general.clearContents()
                        NSPasteboard.general.setString(targets.joined(separator: "\n"), forType: .string)
                    }
                    .disabled(selection.isEmpty)
                }
            }
            .searchable(text: $query, prompt: "筛选目标、进程、规则或路线")
        } else {
            Placeholder(title: "Controller 当前不可访问", symbol: "network.slash",
                        detail: "连接后可以在这里查看活动连接和实时网速。")
        }
    }

    private func summary(_ snapshot: Snapshot, shown: Int) -> String {
        let total = snapshot.connections.count
        let count = shown == total ? "\(total) 条连接" : "\(shown) / \(total) 条连接"
        return "\(count) · ↓ \(Format.rate(snapshot.traffic.download))  ↑ \(Format.rate(snapshot.traffic.upload)) · 每 2 秒刷新"
    }

    private func filtered(_ connections: [Connection]) -> [Connection] {
        let needle = query.trimmingCharacters(in: .whitespaces).lowercased()
        guard !needle.isEmpty else { return connections }
        return connections.filter { connection in
            [connection.destination, connection.processName, connection.ruleText, connection.routeText]
                .contains { $0.lowercased().contains(needle) }
        }
    }
}
