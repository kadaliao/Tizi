// swift-tools-version: 5.9
import PackageDescription

// Tizi 桌面版：SwiftUI 界面只负责展示和交互，全部连接逻辑由
// tizi 的后台引擎（`tizi _gui-engine`）完成。
let package = Package(
    name: "TiziDesktop",
    platforms: [.macOS(.v13)],
    targets: [
        .executableTarget(name: "TiziDesktop", path: "Sources/TiziDesktop"),
        .executableTarget(name: "TiziAskPass", path: "Sources/TiziAskPass"),
    ]
)
