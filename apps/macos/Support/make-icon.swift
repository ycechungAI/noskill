// Draws the app icon (the word "skill" crossed out, for no-skill, on an indigo squircle) and
// writes Support/AppIcon.icns.
// Run: swift apps/macos/Support/make-icon.swift [preview.png]
import AppKit

let support = URL(fileURLWithPath: CommandLine.arguments[0]).deletingLastPathComponent()
let iconset = FileManager.default.temporaryDirectory.appendingPathComponent("NoskillIcon.iconset")
try? FileManager.default.removeItem(at: iconset)
try FileManager.default.createDirectory(at: iconset, withIntermediateDirectories: true)

func rgb(_ r: CGFloat, _ g: CGFloat, _ b: CGFloat, _ a: CGFloat = 1) -> NSColor {
    NSColor(srgbRed: r / 255, green: g / 255, blue: b / 255, alpha: a)
}

func draw(_ px: Int) -> NSBitmapImageRep {
    let rep = NSBitmapImageRep(bitmapDataPlanes: nil, pixelsWide: px, pixelsHigh: px, bitsPerSample: 8,
                               samplesPerPixel: 4, hasAlpha: true, isPlanar: false,
                               colorSpaceName: .deviceRGB, bytesPerRow: 0, bitsPerPixel: 0)!
    NSGraphicsContext.saveGraphicsState()
    NSGraphicsContext.current = NSGraphicsContext(bitmapImageRep: rep)
    let s = CGFloat(px) / 1024  // design on a 1024 grid
    func pt(_ x: CGFloat, _ y: CGFloat) -> NSPoint { NSPoint(x: x * s, y: y * s) }
    func circle(_ c: NSPoint, _ r: CGFloat) -> NSBezierPath {
        NSBezierPath(ovalIn: NSRect(x: c.x - r * s, y: c.y - r * s, width: 2 * r * s, height: 2 * r * s))
    }

    // macOS icon grid: an 824 squircle centred on the 1024 canvas, with a soft drop shadow.
    let tile = NSRect(x: 100 * s, y: 100 * s, width: 824 * s, height: 824 * s)
    let shape = NSBezierPath(roundedRect: tile, xRadius: 185 * s, yRadius: 185 * s)
    NSGraphicsContext.saveGraphicsState()
    let shadow = NSShadow()
    shadow.shadowColor = NSColor.black.withAlphaComponent(0.35)
    shadow.shadowBlurRadius = 24 * s
    shadow.shadowOffset = NSSize(width: 0, height: -10 * s)
    shadow.set()
    rgb(40, 30, 110).setFill()
    shape.fill()
    NSGraphicsContext.restoreGraphicsState()
    NSGradient(colors: [rgb(92, 70, 230), rgb(38, 24, 104)])!.draw(in: shape, angle: -90)

    NSGraphicsContext.saveGraphicsState()
    shape.addClip()
    // A soft light from the top.
    NSGradient(colors: [rgb(255, 255, 255, 0.18), rgb(255, 255, 255, 0)])!
        .draw(in: NSRect(x: 100 * s, y: 560 * s, width: 824 * s, height: 364 * s), angle: -90)

    // The word "skill"…
    let base = NSFont.systemFont(ofSize: 300 * s, weight: .heavy)
    let font = base.fontDescriptor.withDesign(.rounded).flatMap { NSFont(descriptor: $0, size: 300 * s) } ?? base
    let word = NSAttributedString(string: "skill", attributes: [.font: font, .foregroundColor: NSColor.white, .kern: -6 * s])
    let size = word.size()
    let origin = NSPoint(x: (1024 * s - size.width) / 2, y: (1024 * s - size.height) / 2 + 10 * s)
    word.draw(at: origin)

    // …crossed out: a thick red stroke, slightly tilted, with a dark red edge so it reads at 16 px.
    let mid = origin.y + size.height * 0.42
    let strike = NSBezierPath()
    strike.move(to: NSPoint(x: origin.x - 30 * s, y: mid - 14 * s))
    strike.line(to: NSPoint(x: origin.x + size.width + 30 * s, y: mid + 14 * s))
    strike.lineCapStyle = .round
    strike.lineWidth = 34 * s
    rgb(150, 16, 24).setStroke()
    strike.stroke()
    strike.lineWidth = 24 * s
    rgb(255, 59, 48).setStroke()
    strike.stroke()
    NSGraphicsContext.restoreGraphicsState()
    return rep
}

for (pts, scales) in [(16, [1, 2]), (32, [1, 2]), (128, [1, 2]), (256, [1, 2]), (512, [1, 2])] {
    for sc in scales {
        let name = "icon_\(pts)x\(pts)" + (sc == 2 ? "@2x" : "") + ".png"
        try draw(pts * sc).representation(using: .png, properties: [:])!.write(to: iconset.appendingPathComponent(name))
    }
}
if CommandLine.arguments.count > 1 {
    try draw(1024).representation(using: .png, properties: [:])!.write(to: URL(fileURLWithPath: CommandLine.arguments[1]))
}
let p = Process()
p.executableURL = URL(fileURLWithPath: "/usr/bin/iconutil")
p.arguments = ["-c", "icns", iconset.path, "-o", support.appendingPathComponent("AppIcon.icns").path]
try p.run()
p.waitUntilExit()
print(p.terminationStatus == 0 ? "Wrote Support/AppIcon.icns" : "iconutil failed")
