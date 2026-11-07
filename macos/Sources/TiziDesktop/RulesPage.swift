import SwiftUI

struct RulesPage: View {
    @EnvironmentObject private var model: AppModel
    @State private var website = ""
    @State private var target = "外网代理"

    var body: some View {
        Form {
            Section {
                HStack {
                    TextField("网址或域名", text: $website, prompt: Text("例如 https://www.example.com/path 或 example.com"))
                        .onSubmit(test)
                    Button("测试当前路线", action: test)
                        .disabled(website.trimmingCharacters(in: .whitespaces).isEmpty || model.routeTesting
                                  || model.snapshot?.controllerOnline != true)
                }
                if model.routeTesting {
                    ProgressView().controlSize(.small)
                } else if let result = model.routeTest {
                    routeResult(result)
                }
            } header: {
                SectionTitle(title: "查看网站走哪条路线", symbol: "magnifyingglass", color: .blue)
            } footer: {
                FormNote("测试经本机代理建立一次连接，不请求网页内容；需要先连接。")
            }

            Section {
                Picker("路线", selection: $target) {
                    ForEach(model.rules?.targets ?? ["内网", "外网代理", "默认出口", "DIRECT"], id: \.self) { name in
                        Text(name).tag(name)
                    }
                }
                HStack {
                    Spacer()
                    Button("保存域名规则") {
                        Task { await model.saveRule(website, target: target) }
                    }
                    .disabled(website.trimmingCharacters(in: .whitespaces).isEmpty)
                }
            } header: {
                SectionTitle(title: "保存自定义路线", symbol: "plus", color: .brand)
            } footer: {
                FormNote("规则写入 custom-rules.txt，包含子域名，和终端版共用；重载 Mihomo 后生效。")
            }

            Section {
                if let rules = model.rules?.rules, !rules.isEmpty {
                    ForEach(rules) { rule in
                        LabeledContent {
                            RoutePill(rule.target, kind: RouteKind(group: rule.target) ?? .proxy)
                        } label: {
                            Text(rule.domain).textSelection(.enabled)
                        }
                    }
                } else {
                    Text("暂无。输入网站后选择路线并保存。")
                        .foregroundStyle(.secondary)
                }
                HStack {
                    Button("编辑 custom-rules.txt") { model.openCustomRules() }
                    Spacer()
                    Button("重载 Mihomo 使规则生效") { Task { await model.reload() } }
                        .disabled(model.busyLabel != nil)
                }
            } header: {
                SectionTitle(title: "已保存的域名规则", symbol: "list.bullet", color: Color(nsColor: .systemGray))
            }
        }
        .formStyle(.grouped)
        .task { await model.loadRules() }
    }

    private func test() {
        let input = website.trimmingCharacters(in: .whitespaces)
        guard !input.isEmpty else { return }
        Task { await model.testRoute(input) }
    }

    @ViewBuilder
    private func routeResult(_ result: RouteTest) -> some View {
        if result.found {
            LabeledContent("域名", value: result.host)
            if let ip = result.ip, !ip.isEmpty {
                LabeledContent("目标 IP", value: ip)
            }
            if let rule = result.rule {
                LabeledContent("命中规则", value: rule)
            }
            LabeledContent("实际链路") {
                if let route = result.route, !route.isEmpty {
                    RoutePill(route, prominent: true)
                } else {
                    Text("—")
                }
            }
        } else {
            Text("Mihomo 已完成连接，但没有返回可识别的路线。")
                .foregroundStyle(.orange)
        }
    }
}
