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
                VStack(alignment: .leading, spacing: 10) {
                    HStack(alignment: .firstTextBaseline, spacing: 20) {
                        SpeedReadout(download: snapshot.traffic.download, upload: snapshot.traffic.upload, size: 17)
                        Text(count(snapshot, shown: rows.count))
                            .font(.callout.monospacedDigit())
                            .foregroundStyle(.secondary)
                        Spacer()
                        Text("每 2 秒刷新")
                            .font(.caption)
                            .foregroundStyle(.tertiary)
                    }
                    RouteMix(connections: snapshot.connections, legendAlignment: .leading)
                }
                .padding(.horizontal, 16)
                .padding(.vertical, 12)
                Divider()
                Table(rows, sortOrder: $sortOrder) {
                    TableColumn("目标", value: \.destination) { connection in
                        Text(connection.destination).help(connection.destination)
                    }
                    .width(min: 140, ideal: 220)
                    TableColumn("路线", value: \.routeText) { connection in
                        HStack {
                            RoutePill(connection.routeText.isEmpty ? connection.ruleText : connection.routeText)
                                .font(.callout)
                            Spacer(minLength: 0)
                        }
                    }
                    .width(min: 110, ideal: 190)
                    TableColumn("↓ 速度", value: \.downloadRate) { connection in
                        RateText(rate: connection.downloadRate, sampled: connection.sampled)
                    }
                    .width(min: 72, ideal: 84)
                    TableColumn("↑ 速度", value: \.uploadRate) { connection in
                        RateText(rate: connection.uploadRate, sampled: connection.sampled)
                    }
                    .width(min: 72, ideal: 84)
                    TableColumn("规则", value: \.ruleText) { connection in
                        Text(connection.ruleText)
                            .foregroundStyle(.secondary)
                            .help(connection.ruleText)
                    }
                    .width(min: 70, ideal: 120)
                    TableColumn("已下载", value: \.download) { connection in
                        Text(Format.bytes(connection.download))
                            .monospacedDigit()
                            .foregroundStyle(.secondary)
                    }
                    .width(min: 60, ideal: 72)
                    TableColumn("进程", value: \.processName) { connection in
                        Text(connection.processName).foregroundStyle(.secondary)
                    }
                    .width(min: 50, ideal: 90)
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
            Placeholder(title: "还没有连接", symbol: "network.slash",
                        detail: "连接后可以在这里查看每条连接走哪条路线、实时网速多少。")
        }
    }

    private func count(_ snapshot: Snapshot, shown: Int) -> String {
        let total = snapshot.connections.count
        return shown == total ? "\(total) 条连接" : "显示 \(shown) 条，共 \(total) 条"
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

/// 实时速度：没有流量时淡化，有流量的连接一眼可见。
private struct RateText: View {
    let rate: Double
    let sampled: Bool

    var body: some View {
        Text(sampled ? Format.rate(rate) : "—")
            .monospacedDigit()
            .foregroundStyle(sampled && rate >= 1 ? .primary : .tertiary)
    }
}
