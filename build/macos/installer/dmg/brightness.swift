// Prints the mean brightness (0 dark … 1 white) of a rectangle of a PNG, so
// CI can tell the disk image window drew its dark background rather than
// Finder's plain white.
//
//   swift brightness.swift PICTURE.png LEFT TOP RIGHT BOTTOM
import AppKit

let a = CommandLine.arguments
guard a.count == 6, let image = NSImage(contentsOfFile: a[1]), let tiff = image.tiffRepresentation,
      let rep = NSBitmapImageRep(data: tiff), let l = Int(a[2]), let t = Int(a[3]), let r = Int(a[4]), let b = Int(a[5]) else {
    FileHandle.standardError.write("usage: brightness PICTURE LEFT TOP RIGHT BOTTOM\n".data(using: .utf8)!)
    exit(2)
}
var sum = 0.0, n = 0.0
for y in stride(from: t, to: min(b, rep.pixelsHigh), by: 4) {
    for x in stride(from: l, to: min(r, rep.pixelsWide), by: 4) {
        guard let c = rep.colorAt(x: x, y: y)?.usingColorSpace(.sRGB) else { continue }
        sum += (c.redComponent + c.greenComponent + c.blueComponent) / 3
        n += 1
    }
}
print(String(format: "%.3f", n > 0 ? sum / n : 1))
