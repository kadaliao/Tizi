import SwiftUI

struct PolicyPage: View {
    @EnvironmentObject private var model: AppModel

    var body: some View {
        if let snapshot = model.snapshot, snapshot.controllerOnline {
            if snapshot.groups.isEmpty {
                Placeholder(title: "没有可切换的策略组", symbol: "arrow.triangle.branch")
            } else {
                ScrollView {
                    VStack(alignment: .leading, spacing: 14) {
                        ForEach(snapshot.groups) { group in
                            PolicyGroupCard(group: group)
                        }
                        Text("切换立即生效，只影响这一次运行；重载 Mihomo 后恢复默认选择。")
                            .font(.callout)
                            .foregroundStyle(.secondary)
                    }
                    .padding(20)
                }
            }
        } else {
            Placeholder(title: "还没有连接", symbol: "arrow.triangle.branch",
                        detail: "连接后可以在这里切换每类流量使用的节点。")
        }
    }
}

/// 一个策略组：标题说明它管哪类流量，下面是可选节点。节点多时先显示一部分。
private struct PolicyGroupCard: View {
    @EnvironmentObject private var model: AppModel
    let group: PolicyGroup

    @State private var expanded = false

    private static let collapsedCount = 12

    private var kind: RouteKind { RouteKind(group: group.name) ?? .proxy }

    private var visibleChoices: [String] {
        guard !expanded, group.choices.count > Self.collapsedCount else { return group.choices }
        var shown = Array(group.choices.prefix(Self.collapsedCount - 1))
        if !shown.contains(group.now), group.choices.contains(group.now) { shown.append(group.now) }
        return shown
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            HStack(spacing: 10) {
                IconBadge(symbol: kind.symbol, color: kind.color, size: 26)
                VStack(alignment: .leading, spacing: 1) {
                    Text(group.name).font(.headline)
                    if let about = Self.about[group.name] {
                        Text(about).font(.caption).foregroundStyle(.secondary)
                    }
                }
                Spacer()
                RoutePill(group.now, kind: RouteKind(group: group.now) ?? kind, prominent: true)
                    .font(.callout)
            }
            LazyVGrid(columns: [GridItem(.adaptive(minimum: 132), spacing: 8)], alignment: .leading, spacing: 8) {
                ForEach(visibleChoices, id: \.self) { choice in
                    ChoiceButton(title: choice, selected: choice == group.now,
                                 tint: kind.color, kind: RouteKind(group: choice)) {
                        model.selectPolicy(group: group.name, choice: choice)
                    }
                }
            }
            if group.choices.count > Self.collapsedCount {
                Button(expanded ? "收起" : "显示全部 \(group.choices.count) 个") { expanded.toggle() }
                    .buttonStyle(.link)
                    .font(.callout)
            }
        }
        .panel(radius: 12, padding: 16)
    }

    /// Tizi 生成的策略组各管哪类流量。
    static let about: [String: String] = [
        "外网代理": "海外网站走的出口",
        "自动选择": "按延迟自动挑选最快的节点",
        "Claude": "Claude 专用线路，不会直连",
        "前置代理": "连住宅节点之前先经过的节点",
        "默认出口": "没有命中任何规则的流量",
        "内网": "单位或家里的内网",
        "Tailscale": "tailnet 里的设备",
    ]
}

private struct ChoiceButton: View {
    let title: String
    let selected: Bool
    let tint: Color
    /// 选项本身是另一个策略组（或 DIRECT、REJECT）时的路线。
    let kind: RouteKind?
    let action: () -> Void

    var body: some View {
        Button(action: action) {
            HStack(spacing: 6) {
                if let kind {
                    Circle().fill(kind.color).frame(width: 6, height: 6)
                }
                Text(title)
                    .lineLimit(1)
                    .truncationMode(.middle)
                Spacer(minLength: 0)
                if selected {
                    Image(systemName: "checkmark")
                        .font(.caption.weight(.bold))
                        .foregroundStyle(tint)
                }
            }
            .font(.callout.weight(selected ? .semibold : .regular))
            .padding(.horizontal, 10)
            .frame(height: 30)
            .background(RoundedRectangle(cornerRadius: 7, style: .continuous)
                .fill(selected ? tint.opacity(0.16) : Color.primary.opacity(0.04)))
            .overlay(RoundedRectangle(cornerRadius: 7, style: .continuous)
                .strokeBorder(selected ? tint.opacity(0.7) : Color.hairline, lineWidth: selected ? 1.5 : 1))
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .help(title)
        .accessibilityAddTraits(selected ? .isSelected : [])
    }
}
