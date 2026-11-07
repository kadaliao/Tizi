import SwiftUI

struct PolicyPage: View {
    @EnvironmentObject private var model: AppModel

    var body: some View {
        if let snapshot = model.snapshot, snapshot.controllerOnline {
            if snapshot.groups.isEmpty {
                Placeholder(title: "没有可切换的策略组", symbol: "arrow.triangle.branch")
            } else {
                Form {
                    Section {
                        ForEach(snapshot.groups) { group in
                            Picker(group.name, selection: Binding(
                                get: { group.now },
                                set: { model.selectPolicy(group: group.name, choice: $0) }
                            )) {
                                ForEach(group.choices, id: \.self) { choice in
                                    Text(choice).tag(choice)
                                }
                            }
                        }
                    } footer: {
                        FormNote("临时切换某类流量使用的出口，立即生效；重启 Mihomo 后恢复为配置中的默认选择。")
                    }
                }
                .formStyle(.grouped)
            }
        } else {
            Placeholder(title: "Controller 当前不可访问", symbol: "bolt.horizontal.circle",
                        detail: "连接后可以在这里切换代理组。")
        }
    }
}
