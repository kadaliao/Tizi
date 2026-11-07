import SwiftUI

struct DiagnosticsPage: View {
    @EnvironmentObject private var model: AppModel

    var body: some View {
        Form {
            Section {
                ForEach(model.diagnostics) { check in
                    HStack(alignment: .firstTextBaseline, spacing: 10) {
                        Image(systemName: symbol(check.level))
                            .foregroundStyle(color(check.level))
                            .frame(width: 18)
                        Text(check.name)
                            .frame(width: 120, alignment: .leading)
                        Text(check.detail)
                            .foregroundStyle(.secondary)
                            .textSelection(.enabled)
                            .frame(maxWidth: .infinity, alignment: .leading)
                    }
                }
                if model.diagnostics.isEmpty && !model.diagnosticsLoading {
                    Text("尚未运行诊断").foregroundStyle(.secondary)
                }
            } header: {
                HStack {
                    Text("检查程序、端口、网络和内网是否正常")
                    Spacer()
                    if model.diagnosticsLoading {
                        ProgressView().controlSize(.small)
                    }
                    Button("重新诊断") { Task { await model.loadDiagnostics() } }
                        .disabled(model.diagnosticsLoading)
                }
            } footer: {
                FormNote("诊断只读取本机状态，不会启动 Mihomo 或修改网络。等价于终端执行 tizi diagnose。")
            }
        }
        .formStyle(.grouped)
        .task { await model.loadDiagnostics() }
    }

    private func symbol(_ level: String) -> String {
        switch level {
        case "ok": return "checkmark.circle.fill"
        case "warn": return "exclamationmark.triangle.fill"
        case "fail": return "xmark.octagon.fill"
        default: return "info.circle"
        }
    }

    private func color(_ level: String) -> Color {
        switch level {
        case "ok": return .green
        case "warn": return .orange
        case "fail": return .red
        default: return .blue
        }
    }
}
