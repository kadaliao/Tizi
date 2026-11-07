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
                        DelayToolbar(snapshot: snapshot)
                        ForEach(snapshot.groups) { group in
                            PolicyGroupCard(group: group)
                        }
                        Text("切换立即生效，只影响这一次运行；重载 Mihomo 后恢复默认选择。测速经节点访问 www.gstatic.com，「超时」表示这个节点现在用不了。自动选择每 5 分钟测一次，选它时也会马上重测。")
                            .font(.callout)
                            .foregroundStyle(.secondary)
                            .fixedSize(horizontal: false, vertical: true)
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

/// 页首：测速按钮和排序方式。
private struct DelayToolbar: View {
    @EnvironmentObject private var model: AppModel
    let snapshot: Snapshot

    @AppStorage("PolicySortByDelay") private var sortByDelay = false

    var body: some View {
        HStack(spacing: 12) {
            Button {
                Task { await model.testDelays() }
            } label: {
                HStack(spacing: 6) {
                    if model.delayTesting {
                        ProgressView().controlSize(.small)
                    } else {
                        Image(systemName: "speedometer")
                    }
                    Text(model.delayTesting ? "正在测速…" : "测速")
                }
            }
            .disabled(model.delayTesting)
            .help("测一遍所有节点的延迟，自动选择会按结果重新挑节点")
            if snapshot.hasDelays {
                Toggle("按延迟排序", isOn: $sortByDelay)
                    .toggleStyle(.checkbox)
                    .help("最快的节点排在前面，测不通的排在最后")
            }
            Spacer()
        }
    }
}

/// 一个策略组：标题说明它管哪类流量，下面是可选节点。节点多时先显示一部分。
private struct PolicyGroupCard: View {
    @EnvironmentObject private var model: AppModel
    let group: PolicyGroup

    @State private var expanded = false
    @AppStorage("PolicySortByDelay") private var sortByDelay = false

    private static let collapsedCount = 12

    private var kind: RouteKind { RouteKind(group: group.name) ?? .proxy }

    /// 按延迟排序时：策略组和 DIRECT 等保持在最前，节点从快到慢，没测过的
    /// 其次，测不通的最后。
    private var orderedChoices: [String] {
        guard sortByDelay, let snapshot = model.snapshot, snapshot.hasDelays else { return group.choices }
        func rank(_ choice: String) -> (Int, Int) {
            // 策略组、DIRECT 和 REJECT 都有 RouteKind，普通节点没有。
            if RouteKind(group: choice) != nil { return (0, 0) }
            switch snapshot.delays?[choice] {
            case let delay? where delay > 0: return (1, delay)
            case nil: return (2, 0)
            default: return (3, 0)
            }
        }
        return group.choices.enumerated()
            .sorted { lhs, rhs in
                let (l, r) = (rank(lhs.element), rank(rhs.element))
                return l != r ? l < r : lhs.offset < rhs.offset
            }
            .map(\.element)
    }

    private var visibleChoices: [String] {
        let choices = orderedChoices
        guard !expanded, choices.count > Self.collapsedCount else { return choices }
        var shown = Array(choices.prefix(Self.collapsedCount - 1))
        if !shown.contains(group.now), choices.contains(group.now) { shown.append(group.now) }
        return shown
    }

    /// 当前选项是自动选择时，它此刻用的节点。
    private var autoNode: String? { model.snapshot?.autoNow(group.now) }

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
            if let autoNode {
                HStack(spacing: 6) {
                    Text("\(group.now)此刻用的是").foregroundStyle(.secondary)
                    Text(autoNode).fontWeight(.medium)
                    if let delay = model.snapshot?.delays?[autoNode] {
                        DelayLabel(delay: delay)
                    }
                }
                .font(.callout)
                .lineLimit(1)
                .truncationMode(.middle)
            }
            LazyVGrid(columns: [GridItem(.adaptive(minimum: 150), spacing: 8)], alignment: .leading, spacing: 8) {
                ForEach(visibleChoices, id: \.self) { choice in
                    ChoiceButton(title: choice, selected: choice == group.now,
                                 tint: kind.color, kind: RouteKind(group: choice),
                                 delay: model.snapshot?.delay(of: choice),
                                 detail: model.snapshot?.autoNow(choice)) {
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
    /// 最近一次测速的延迟（毫秒，0 表示没测通）；没测过为 nil。
    var delay: Int?
    /// 选项是自动选择时，它此刻用的节点。
    var detail: String?
    let action: () -> Void

    private var helpText: String {
        var text = title
        if let detail { text += " → \(detail)" }
        if let delay { text += delay > 0 ? "，\(delay)ms" : "，测不通" }
        return text
    }

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
                if let delay {
                    DelayLabel(delay: delay)
                }
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
        .help(helpText)
        .accessibilityLabel(helpText)
        .accessibilityAddTraits(selected ? .isSelected : [])
    }
}

/// 延迟读数：300ms 以内绿色，1 秒以内橙色，更慢或测不通红色。
struct DelayLabel: View {
    let delay: Int

    var body: some View {
        Text(delay > 0 ? "\(delay)ms" : "超时")
            .font(.caption.monospacedDigit())
            .foregroundStyle(color)
    }

    private var color: Color {
        switch delay {
        case ...0: return .red
        case ..<300: return .brand
        case ..<1000: return .orange
        default: return .red
        }
    }
}
