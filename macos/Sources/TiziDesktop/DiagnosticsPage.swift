import SwiftUI

struct DiagnosticsPage: View {
    @EnvironmentObject private var model: AppModel

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 14) {
                summary
                if !model.diagnostics.isEmpty {
                    VStack(spacing: 0) {
                        ForEach(Array(model.diagnostics.enumerated()), id: \.element.id) { index, check in
                            if index > 0 { Divider().padding(.leading, 46) }
                            CheckRow(check: check)
                        }
                    }
                    .panel(radius: 12, padding: 0)
                } else if !model.diagnosticsLoading {
                    Text("尚未运行诊断").foregroundStyle(.secondary)
                }
                Text("诊断只读取本机状态，不会连接或修改网络设置，和终端里的 tizi diagnose 相同。")
                    .font(.callout)
                    .foregroundStyle(.secondary)
            }
            .padding(20)
        }
        .task { await model.loadDiagnostics() }
    }

    /// 结论放在最上面：全部正常，或者有几项需要处理。
    private var summary: some View {
        let failed = model.diagnostics.filter { $0.level == "fail" }.count
        let warned = model.diagnostics.filter { $0.level == "warn" }.count
        let level = failed > 0 ? "fail" : (warned > 0 ? "warn" : "ok")
        return HStack(spacing: 14) {
            Image(systemName: CheckRow.symbol(level))
                .font(.system(size: 30))
                .foregroundStyle(CheckRow.color(level))
            VStack(alignment: .leading, spacing: 3) {
                Text(model.diagnostics.isEmpty ? "正在检查…" : headline(failed: failed, warned: warned))
                    .font(.system(size: 20, weight: .semibold))
                Text("程序、端口、系统网络、内网和 Claude 直连保护")
                    .font(.callout)
                    .foregroundStyle(.secondary)
            }
            Spacer()
            if model.diagnosticsLoading {
                ProgressView().controlSize(.small)
            }
            Button("重新诊断") { Task { await model.loadDiagnostics() } }
                .disabled(model.diagnosticsLoading)
        }
        .panel(radius: 16, padding: 18)
    }

    private func headline(failed: Int, warned: Int) -> String {
        switch (failed, warned) {
        case (0, 0): return "一切正常"
        case (0, _): return "\(warned) 项需要留意"
        case (_, 0): return "\(failed) 项有问题"
        default: return "\(failed) 项有问题，\(warned) 项需要留意"
        }
    }
}

private struct CheckRow: View {
    let check: DiagnosticCheck

    var body: some View {
        HStack(alignment: .firstTextBaseline, spacing: 12) {
            Image(systemName: Self.symbol(check.level))
                .foregroundStyle(Self.color(check.level))
                .frame(width: 20)
            Text(check.name)
                .fontWeight(check.level == "fail" ? .semibold : .regular)
                .frame(width: 120, alignment: .leading)
            Text(check.detail)
                .foregroundStyle(check.level == "fail" ? .primary : .secondary)
                .textSelection(.enabled)
                .frame(maxWidth: .infinity, alignment: .leading)
        }
        .padding(.horizontal, 14)
        .padding(.vertical, 10)
        .background(check.level == "fail" ? Color.red.opacity(0.06) : Color.clear)
    }

    static func symbol(_ level: String) -> String {
        switch level {
        case "ok": return "checkmark.circle.fill"
        case "warn": return "exclamationmark.triangle.fill"
        case "fail": return "xmark.octagon.fill"
        default: return "info.circle"
        }
    }

    static func color(_ level: String) -> Color {
        switch level {
        case "ok": return .brand
        case "warn": return .orange
        case "fail": return .red
        default: return .secondary
        }
    }
}
