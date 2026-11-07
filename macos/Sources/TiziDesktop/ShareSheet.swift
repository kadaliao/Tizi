import AppKit
import CoreImage.CIFilterBuiltins
import SwiftUI

struct ShareSheet: View {
    @EnvironmentObject private var model: AppModel

    var body: some View {
        VStack(spacing: 14) {
            Text("Clash Mi 扫码导入")
                .font(.title3.weight(.semibold))
            if let share = model.activeShare {
                if let image = QRCode.image(for: share.url) {
                    Image(nsImage: image)
                        .interpolation(.none)
                        .resizable()
                        .frame(width: 240, height: 240)
                        .padding(10)
                        .background(Color.white)
                        .clipShape(RoundedRectangle(cornerRadius: 8))
                }
                Text(share.url)
                    .font(.caption.monospaced())
                    .textSelection(.enabled)
                (Text("有效期至 \(share.expiry.formatted(date: .omitted, time: .standard))，剩余 ")
                    + Text(timerInterval: Date()...max(Date(), share.expiry), countsDown: true))
                    .font(.callout.monospacedDigit())
                    .foregroundStyle(.secondary)
            } else {
                Placeholder(title: "配置分享已关闭", symbol: "qrcode", detail: "二维码已过期或已被关闭。需要时重新发起扫码导入。")
                    .frame(height: 260)
            }
            Text("在 Clash Mi 中打开「我的配置」，点右上角 +，选择「扫描二维码」。配置含节点密码、证书等私密信息：请勿截图或转发，也不要在不可信的局域网中使用。")
                .font(.callout)
                .foregroundStyle(.orange)
                .fixedSize(horizontal: false, vertical: true)
            Button(model.activeShare == nil ? "关闭" : "导入完成，关闭分享") { model.stopShare() }
                .keyboardShortcut(.defaultAction)
        }
        .padding(24)
        .frame(width: 420)
    }
}

enum QRCode {
    static func image(for text: String) -> NSImage? {
        let filter = CIFilter.qrCodeGenerator()
        filter.message = Data(text.utf8)
        filter.correctionLevel = "M"
        guard let output = filter.outputImage else { return nil }
        let representation = NSCIImageRep(ciImage: output)
        let image = NSImage(size: representation.size)
        image.addRepresentation(representation)
        return image
    }
}
