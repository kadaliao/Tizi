// 生成 1024×1024 的应用图标 PNG：玉绿色圆角底 + 白色梯子（Tizi，梯子）。
// 梯子的比例与界面里的 LadderMark（Theme.swift）一致。
// 用法：swift make-icon.swift <输出 PNG 路径>
import AppKit

guard CommandLine.arguments.count == 2 else {
    FileHandle.standardError.write(Data("usage: make-icon.swift <output.png>\n".utf8))
    exit(2)
}

let side = 1024
guard let bitmap = NSBitmapImageRep(
    bitmapDataPlanes: nil, pixelsWide: side, pixelsHigh: side, bitsPerSample: 8,
    samplesPerPixel: 4, hasAlpha: true, isPlanar: false, colorSpaceName: .deviceRGB,
    bytesPerRow: 0, bitsPerPixel: 0
) else { exit(1) }

NSGraphicsContext.saveGraphicsState()
NSGraphicsContext.current = NSGraphicsContext(bitmapImageRep: bitmap)

func color(_ hex: Int, alpha: CGFloat = 1) -> NSColor {
    NSColor(srgbRed: CGFloat((hex >> 16) & 0xFF) / 255, green: CGFloat((hex >> 8) & 0xFF) / 255,
            blue: CGFloat(hex & 0xFF) / 255, alpha: alpha)
}

// macOS 图标栅格：1024 画布内 824 的圆角矩形主体，四周留出投影空间。
let body = NSRect(x: 100, y: 100, width: 824, height: 824)
let shape = NSBezierPath(roundedRect: body, xRadius: 186, yRadius: 186)
let shadow = NSShadow()
shadow.shadowColor = NSColor.black.withAlphaComponent(0.28)
shadow.shadowOffset = NSSize(width: 0, height: -12)
shadow.shadowBlurRadius = 28
NSGraphicsContext.saveGraphicsState()
shadow.set()
color(0x1F9D55).setFill()
shape.fill()
NSGraphicsContext.restoreGraphicsState()
// 左上亮、右下深，和界面里已连接的梯子标志一致。
NSGradient(starting: color(0x45C77B), ending: color(0x0D753C))?.draw(in: shape, angle: -45)
// 顶部一道柔和的高光。
NSGraphicsContext.saveGraphicsState()
shape.addClip()
NSGradient(starting: NSColor.white.withAlphaComponent(0.16), ending: NSColor.white.withAlphaComponent(0))?
    .draw(in: NSRect(x: body.minX, y: body.midY, width: body.width, height: body.height / 2), angle: -90)
NSGraphicsContext.restoreGraphicsState()

// 梯子：底部扶手间距宽、顶部收拢；四级横档。坐标系 y 向上，单位框里 0 在顶部。
let box = NSRect(x: 512 - 236, y: 512 - 236, width: 472, height: 472)
let bottomInset: CGFloat = 0.14, topInset: CGFloat = 0.26
func railX(_ y: CGFloat, left: Bool) -> CGFloat {
    let inset = topInset + (bottomInset - topInset) * y
    return box.minX + box.width * (left ? inset : 1 - inset)
}
func point(_ x: CGFloat, _ y: CGFloat) -> NSPoint { NSPoint(x: x, y: box.maxY - box.height * y) }

let ladder = NSBezierPath()
ladder.lineWidth = 50
ladder.lineCapStyle = .round
for index in 0..<4 {
    let y = 0.86 - CGFloat(index) * 0.72 / 3
    ladder.move(to: point(railX(y, left: true), y))
    ladder.line(to: point(railX(y, left: false), y))
}
for left in [true, false] {
    ladder.move(to: point(railX(1, left: left), 1))
    ladder.line(to: point(railX(0, left: left), 0))
}
let ladderShadow = NSShadow()
ladderShadow.shadowColor = NSColor.black.withAlphaComponent(0.22)
ladderShadow.shadowOffset = NSSize(width: 0, height: -8)
ladderShadow.shadowBlurRadius = 16
NSGraphicsContext.saveGraphicsState()
ladderShadow.set()
NSColor.white.setStroke()
ladder.stroke()
NSGraphicsContext.restoreGraphicsState()

NSGraphicsContext.restoreGraphicsState()
guard let png = bitmap.representation(using: .png, properties: [:]) else { exit(1) }
try png.write(to: URL(fileURLWithPath: CommandLine.arguments[1]))
