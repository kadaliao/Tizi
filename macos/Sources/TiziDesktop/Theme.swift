import AppKit
import SwiftUI

// 界面的视觉基础：品牌色、路线配色、梯子标志和几种通用容器。
//
// 路线配色是贯穿各页的视觉语言：同一种路线（外网代理、直连、内网、Claude、
// Tailscale、拦截）在状态页、策略、连接、规则和日志里都是同一个颜色。

extension Color {
    /// 品牌玉绿，与应用图标一致；深色模式下提亮。
    static let brand = Color(nsColor: NSColor(name: "TiziBrand") { appearance in
        appearance.bestMatch(from: [.darkAqua, .aqua]) == .darkAqua
            ? NSColor(srgbRed: 0.24, green: 0.78, blue: 0.49, alpha: 1)
            : NSColor(srgbRed: 0.09, green: 0.58, blue: 0.33, alpha: 1)
    })

    static let brandDeep = Color(nsColor: NSColor(srgbRed: 0.05, green: 0.46, blue: 0.25, alpha: 1))
    static let brandLight = Color(nsColor: NSColor(srgbRed: 0.27, green: 0.78, blue: 0.48, alpha: 1))

    /// 面板底色：比窗口底色略亮（深色）或略白（浅色）。
    static let panel = Color(nsColor: .controlBackgroundColor)
    static let hairline = Color.primary.opacity(0.08)
}

// MARK: 路线

/// 一条连接（或一条规则、一个策略组）走的是哪一类路线。
enum RouteKind: CaseIterable {
    case proxy, direct, intranet, claude, tailscale, reject

    var title: String {
        switch self {
        case .proxy: return "外网代理"
        case .direct: return "直连"
        case .intranet: return "内网"
        case .claude: return "Claude"
        case .tailscale: return "Tailscale"
        case .reject: return "拦截"
        }
    }

    var color: Color {
        switch self {
        case .proxy: return .blue
        case .direct: return Color(nsColor: .systemGray)
        case .intranet: return .orange
        case .claude: return .purple
        case .tailscale: return .teal
        case .reject: return .red
        }
    }

    var symbol: String {
        switch self {
        case .proxy: return "globe.asia.australia.fill"
        case .direct: return "arrow.right"
        case .intranet: return "building.2.fill"
        case .claude: return "sparkle"
        case .tailscale: return "point.3.connected.trianglepath.dotted"
        case .reject: return "nosign"
        }
    }

    /// 按引擎给出的路线文字分类，例如「外网代理 -> 自动选择 -> 香港 02」
    /// 「默认出口 -> DIRECT」「Claude -> 住宅节点」。最后一跳是 DIRECT 或
    /// REJECT 时以它为准，否则看第一跳（命中的策略组）。
    init(route: String) {
        let hops = route.components(separatedBy: "->").map { $0.trimmingCharacters(in: .whitespaces) }
        let first = hops.first ?? "", last = hops.last ?? ""
        if last.uppercased().hasPrefix("REJECT") { self = .reject; return }
        if last.uppercased() == "DIRECT" || last == "直连" { self = .direct; return }
        self = RouteKind(group: first) ?? .proxy
    }

    /// 策略组名对应的路线；不是 Tizi 生成的组名时为 nil。
    init?(group: String) {
        switch group {
        case "内网": self = .intranet
        case "Claude": self = .claude
        case "Tailscale", "tailscale": self = .tailscale
        case "外网代理", "自动选择", "前置代理", "默认出口": self = .proxy
        case "DIRECT": self = .direct
        case "REJECT", "REJECT-DROP": self = .reject
        default: return nil
        }
    }
}

extension String {
    /// 把引擎路线里的 "->" 换成箭头，显示更紧凑。
    var routeDisplay: String { replacingOccurrences(of: " -> ", with: " → ") }
}

/// 带颜色圆点的路线标签。
struct RoutePill: View {
    let text: String
    var kind: RouteKind?
    var prominent = false

    init(_ text: String, kind: RouteKind? = nil, prominent: Bool = false) {
        self.text = text
        self.kind = kind
        self.prominent = prominent
    }

    var body: some View {
        let kind = self.kind ?? RouteKind(route: text)
        HStack(spacing: 5) {
            Circle()
                .fill(kind.color)
                .frame(width: 6, height: 6)
            Text(text.routeDisplay)
                .lineLimit(1)
                .truncationMode(.middle)
        }
        .padding(.horizontal, 8)
        .padding(.vertical, 2.5)
        .background(Capsule().fill(kind.color.opacity(prominent ? 0.20 : 0.12)))
        .help(text.routeDisplay)
    }
}

/// 当前连接按路线分类的分布：一条按连接数分段的横条，加图例。
struct RouteMix: View {
    let connections: [Connection]
    var showLegend = true
    var legendAlignment: HorizontalAlignment = .center
    var showCounts = true

    private var counts: [(kind: RouteKind, count: Int)] {
        var counts: [RouteKind: Int] = [:]
        for connection in connections {
            counts[RouteKind(route: connection.routeText.isEmpty ? connection.ruleText : connection.routeText), default: 0] += 1
        }
        return RouteKind.allCases.compactMap { kind in
            counts[kind].map { (kind, $0) }
        }
    }

    var body: some View {
        let counts = self.counts
        let total = max(1, counts.reduce(0) { $0 + $1.count })
        VStack(alignment: .leading, spacing: 8) {
            GeometryReader { geometry in
                HStack(spacing: 2) {
                    if counts.isEmpty {
                        Capsule().fill(Color.hairline)
                    }
                    ForEach(counts, id: \.kind) { item in
                        Rectangle()
                            .fill(item.kind.color.gradient)
                            .frame(width: max(4, (geometry.size.width - CGFloat(counts.count - 1) * 2)
                                * CGFloat(item.count) / CGFloat(total)))
                    }
                }
                .clipShape(Capsule())
            }
            .frame(height: 6)
            if showLegend {
                FlowLayout(spacing: 12, alignment: legendAlignment) {
                    if counts.isEmpty {
                        Text("暂无活动连接").foregroundStyle(.secondary)
                    }
                    ForEach(counts, id: \.kind) { item in
                        HStack(spacing: 5) {
                            Circle().fill(item.kind.color).frame(width: 7, height: 7)
                            Text(item.kind.title)
                            if showCounts {
                                Text("\(item.count)")
                                    .foregroundStyle(.secondary)
                                    .monospacedDigit()
                            }
                        }
                    }
                }
                .font(.caption)
            }
        }
        .accessibilityElement(children: .combine)
        .accessibilityLabel("路线分布")
    }
}

// MARK: 梯子标志

/// 梯子（Tizi）标志：两根略向上收拢的扶手和四级横档。未连接时是灰色线条；
/// 连接中横档自下而上依次点亮；已连接时是玉绿底上的白色梯子。
struct LadderMark: View {
    enum Phase { case off, connecting, on }

    let phase: Phase
    var size: CGFloat = 120

    @Environment(\.accessibilityReduceMotion) private var reduceMotion

    static let rungs = 4

    var body: some View {
        ZStack {
            background
            if phase == .connecting && !reduceMotion {
                TimelineView(.periodic(from: .now, by: 0.28)) { context in
                    let step = Int(context.date.timeIntervalSinceReferenceDate / 0.28) % (Self.rungs + 2)
                    ladder(lit: min(step, Self.rungs))
                }
            } else {
                ladder(lit: phase == .off ? 0 : Self.rungs)
            }
        }
        .frame(width: size, height: size)
        .accessibilityHidden(true)
    }

    @ViewBuilder private var background: some View {
        switch phase {
        case .on:
            Circle()
                .fill(LinearGradient(colors: [.brandLight, .brandDeep], startPoint: .topLeading, endPoint: .bottomTrailing))
                .overlay(Circle().strokeBorder(.white.opacity(0.18), lineWidth: 1))
                .shadow(color: .brand.opacity(0.45), radius: size * 0.14, y: size * 0.04)
                .background(Circle().fill(Color.brand.opacity(0.14)).padding(-size * 0.09))
        case .connecting:
            Circle()
                .fill(Color.brand.opacity(0.12))
                .overlay(Circle().strokeBorder(Color.brand.opacity(0.35), lineWidth: max(1, size * 0.012)))
        case .off:
            Circle()
                .fill(Color.primary.opacity(0.05))
                .overlay(Circle().strokeBorder(Color.primary.opacity(0.10), lineWidth: max(1, size * 0.01)))
        }
    }

    /// lit 是从下往上点亮的横档数。横档画在扶手下面，扶手是不透明的，
    /// 交叉处不会叠出深色接缝；未连接时整体画成实色再统一调淡。
    private func ladder(lit: Int) -> some View {
        let style = StrokeStyle(lineWidth: size * 0.055, lineCap: .round)
        let railColor: Color = phase == .on ? .white : (phase == .connecting ? .brand : .primary)
        return ZStack {
            ForEach(0..<Self.rungs, id: \.self) { index in
                LadderRung(index: index, count: Self.rungs)
                    .stroke(rungColor(lit: index < lit), style: style)
            }
            LadderRails()
                .stroke(railColor, style: style)
        }
        .frame(width: size * 0.56, height: size * 0.56)
        .compositingGroup()
        .opacity(phase == .off ? 0.32 : 1)
        .animation(reduceMotion ? nil : .easeOut(duration: 0.2), value: lit)
    }

    private func rungColor(lit: Bool) -> Color {
        switch phase {
        case .on: return .white
        case .connecting: return lit ? .brand : Color.brand.opacity(0.22)
        case .off: return .primary
        }
    }
}

/// 梯子的几何：单位框内，底部扶手间距较宽，顶部收拢，像远去的梯子。
enum LadderGeometry {
    static let bottomY: CGFloat = 1.0, topY: CGFloat = 0.0
    static let bottomInset: CGFloat = 0.14, topInset: CGFloat = 0.26

    static func railX(_ y: CGFloat, left: Bool) -> CGFloat {
        let inset = topInset + (bottomInset - topInset) * y
        return left ? inset : 1 - inset
    }

    /// 第 index 级横档（0 在最下面）的纵坐标。
    static func rungY(_ index: Int, count: Int) -> CGFloat {
        0.86 - CGFloat(index) * 0.72 / CGFloat(max(1, count - 1))
    }
}

struct LadderRails: Shape {
    func path(in rect: CGRect) -> Path {
        var path = Path()
        for left in [true, false] {
            path.move(to: CGPoint(x: rect.minX + rect.width * LadderGeometry.railX(1, left: left), y: rect.maxY))
            path.addLine(to: CGPoint(x: rect.minX + rect.width * LadderGeometry.railX(0, left: left), y: rect.minY))
        }
        return path
    }
}

struct LadderRung: Shape {
    let index: Int
    let count: Int

    func path(in rect: CGRect) -> Path {
        let y = LadderGeometry.rungY(index, count: count)
        var path = Path()
        path.move(to: CGPoint(x: rect.minX + rect.width * LadderGeometry.railX(y, left: true), y: rect.minY + rect.height * y))
        path.addLine(to: CGPoint(x: rect.minX + rect.width * LadderGeometry.railX(y, left: false), y: rect.minY + rect.height * y))
        return path
    }
}

// MARK: 容器

/// 内容面板：浅色底、细边框。主面板（hero）圆角更大，次级面板更小，层级靠
/// 圆角和留白区分，而不是给所有东西套同一种卡片。
struct PanelModifier: ViewModifier {
    var radius: CGFloat = 10
    var padding: CGFloat = 14

    func body(content: Content) -> some View {
        content
            .padding(padding)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(RoundedRectangle(cornerRadius: radius, style: .continuous).fill(Color.panel))
            .overlay(RoundedRectangle(cornerRadius: radius, style: .continuous).strokeBorder(Color.hairline))
    }
}

extension View {
    func panel(radius: CGFloat = 10, padding: CGFloat = 14) -> some View {
        modifier(PanelModifier(radius: radius, padding: padding))
    }
}

/// 系统设置风格的彩色图标块，用在侧栏和配置分组标题上。
struct IconBadge: View {
    let symbol: String
    let color: Color
    var size: CGFloat = 20

    var body: some View {
        Image(systemName: symbol)
            .font(.system(size: size * 0.56, weight: .semibold))
            .foregroundStyle(.white)
            .frame(width: size, height: size)
            .background(RoundedRectangle(cornerRadius: size * 0.27, style: .continuous).fill(color.gradient))
            .accessibilityHidden(true)
    }
}

/// 配置页分组标题：彩色图标块加标题。
struct SectionTitle: View {
    let title: String
    let symbol: String
    let color: Color

    var body: some View {
        HStack(spacing: 8) {
            IconBadge(symbol: symbol, color: color, size: 18)
            Text(title)
        }
    }
}

/// 上下行速度：大号圆体数字，单位较小。
struct SpeedReadout: View {
    let download: Double
    let upload: Double
    var size: CGFloat = 20

    var body: some View {
        HStack(spacing: size * 0.9) {
            figure("arrow.down", download, .brand)
            figure("arrow.up", upload, .blue)
        }
        .accessibilityElement(children: .combine)
        .accessibilityLabel("下载 \(Format.rate(download))，上传 \(Format.rate(upload))")
    }

    private func figure(_ symbol: String, _ value: Double, _ tint: Color) -> some View {
        let parts = Format.rateParts(value)
        return HStack(alignment: .firstTextBaseline, spacing: 4) {
            Image(systemName: symbol)
                .font(.system(size: size * 0.6, weight: .bold))
                .foregroundStyle(tint)
            Text(parts.number)
                .font(.system(size: size, weight: .semibold, design: .rounded))
                .monospacedDigit()
            Text(parts.unit)
                .font(.system(size: size * 0.55, weight: .medium, design: .rounded))
                .foregroundStyle(.secondary)
        }
    }
}

/// 简洁模式和首次设置的主按钮：胶囊形，主操作用品牌色填充。
struct CapsuleButtonStyle: ButtonStyle {
    var prominent: Bool
    var width: CGFloat = 168

    @Environment(\.isEnabled) private var isEnabled

    func makeBody(configuration: Configuration) -> some View {
        configuration.label
            .font(.system(size: 15, weight: .semibold))
            .foregroundStyle(prominent ? Color.white : Color.primary)
            .frame(width: width, height: 38)
            .background {
                if prominent {
                    Capsule().fill(LinearGradient(colors: [.brandLight, .brand], startPoint: .top, endPoint: .bottom))
                        .shadow(color: .brand.opacity(0.35), radius: 6, y: 2)
                } else {
                    Capsule().fill(Color.primary.opacity(0.06))
                        .overlay(Capsule().strokeBorder(Color.primary.opacity(0.14)))
                }
            }
            .opacity(isEnabled ? (configuration.isPressed ? 0.8 : 1) : 0.45)
            .scaleEffect(configuration.isPressed ? 0.98 : 1)
            .contentShape(Capsule())
    }
}

/// 自动换行的横向排列（标签云），macOS 13 起可用 Layout。
struct FlowLayout: Layout {
    var spacing: CGFloat = 6
    var alignment: HorizontalAlignment = .leading

    func sizeThatFits(proposal: ProposedViewSize, subviews: Subviews, cache: inout ()) -> CGSize {
        let rows = arrange(width: proposal.width ?? .infinity, subviews: subviews)
        let width = rows.map(\.width).max() ?? 0
        let height = rows.reduce(0) { $0 + $1.height } + spacing * CGFloat(max(0, rows.count - 1))
        return CGSize(width: proposal.width.map { min($0, width) } ?? width, height: height)
    }

    func placeSubviews(in bounds: CGRect, proposal: ProposedViewSize, subviews: Subviews, cache: inout ()) {
        var y = bounds.minY
        for row in arrange(width: bounds.width, subviews: subviews) {
            var x: CGFloat
            switch alignment {
            case .center: x = bounds.minX + (bounds.width - row.width) / 2
            case .trailing: x = bounds.maxX - row.width
            default: x = bounds.minX
            }
            for index in row.indices {
                let size = subviews[index].sizeThatFits(.unspecified)
                subviews[index].place(at: CGPoint(x: x, y: y + (row.height - size.height) / 2), proposal: ProposedViewSize(size))
                x += size.width + spacing
            }
            y += row.height + spacing
        }
    }

    private struct Row {
        var indices: [Int] = []
        var width: CGFloat = 0
        var height: CGFloat = 0
    }

    private func arrange(width: CGFloat, subviews: Subviews) -> [Row] {
        var rows: [Row] = []
        var current = Row()
        for index in subviews.indices {
            let size = subviews[index].sizeThatFits(.unspecified)
            let needed = current.indices.isEmpty ? size.width : current.width + spacing + size.width
            if needed > width && !current.indices.isEmpty {
                rows.append(current)
                current = Row()
            }
            current.width = current.indices.isEmpty ? size.width : current.width + spacing + size.width
            current.height = max(current.height, size.height)
            current.indices.append(index)
        }
        if !current.indices.isEmpty { rows.append(current) }
        return rows
    }
}

extension EngineSettings {
    /// 已设置的线路，每种一个带路线颜色的标签。
    var routeChips: [(title: String, kind: RouteKind)] {
        var chips: [(String, RouteKind)] = []
        if hasSubscription { chips.append(("订阅", .proxy)) }
        if nodeCount > 0 { chips.append(("\(nodeCount) 个节点", .proxy)) }
        if hasOVPN { chips.append(("内网", .intranet)) }
        if tailscaleEnabled { chips.append(("Tailscale", .tailscale)) }
        if hasResidentialNode { chips.append(("Claude 住宅节点", .claude)) }
        return chips
    }
}
