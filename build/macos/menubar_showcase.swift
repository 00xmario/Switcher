import AppKit

// Draws the menu bar's dropdown from made-up accounts into PNG files for
// the README: the real menu views, in dark and light, and with an account's
// usage limit choices open. Nothing here is anyone's real data, and it never
// talks to a running Switcher. Run with `make showcase-menu`.
@main
struct MenuShowcase {
    static func main() {
        let app = NSApplication.shared
        let out = CommandLine.arguments.dropFirst().first ?? "menu-shots"
        try? FileManager.default.createDirectory(atPath: out, withIntermediateDirectories: true)
        for (theme, name) in [("dark", NSAppearance.Name.darkAqua), ("light", NSAppearance.Name.aqua)] {
            app.appearance = NSAppearance(named: name)
            for scene in ["menu", "limit"] {
                let delegate = AppDelegate(runtime: offline())
                delegate.applyMenuState(demoState())
                delegate.applyLoad(SystemLoad(available: true, machine: "MacBook Pro", remote: false, platform: "darwin",
                    cpu: 23, cores: 12, memory: 61, memory_used: 14.6 * 1_073_741_824, memory_total: 24 * 1_073_741_824, gpu: 18))
                if scene == "limit" { delegate.toggleLimitChoices("claude-work") }
                render(delegate.menu, appearance: NSAppearance(named: name)!, to: "\(out)/menu-\(scene)-\(theme)")
            }
        }
        print("Menu showcase written to \(out)")
    }

    // A runtime that answers nothing, so the menu shows only what it is given.
    static func offline() -> MenuRuntime {
        var runtime = MenuRuntime()
        runtime.request = { _, _ in }
        runtime.deviceToken = { "" }
        // Keep awake folded, as most menus show it.
        runtime.loadFlag = { $0 == awakeCollapsedKey }
        return runtime
    }

    static func demoState() -> AppState {
        let now = Date().timeIntervalSince1970
        func w(_ label: String, left: Int, _ seconds: Double) -> UsageWindow {
            UsageWindow(label: label, used_percent: 100 - left, resets_at: now + seconds)
        }
        let h = 3600.0, d = 86400.0
        let accounts = [
            Account(id: "claude-you", provider: "claude", email: "you@example.com", plan: "claude_max_20x", active: true,
                exhausted_until: nil, usage: Usage(available: true, windows: [w("Weekly", left: 58, 3 * d + 3 * h), w("Fable", left: 71, 3 * d + 3 * h)]),
                reset_credits: nil, native_switch_available: true, native_active: true),
            Account(id: "claude-work", provider: "claude", email: "work@example.com", plan: "claude_max_5x", active: false,
                exhausted_until: nil, usage: Usage(available: true, windows: [w("Session", left: 81, 46 * 60), w("Weekly", left: 23, d + 6 * h), w("Fable", left: 64, d + 6 * h)]),
                reset_credits: nil, native_switch_available: true, native_active: false, usage_limit: 10),
            Account(id: "codex-you", provider: "codex", email: "you@example.com", plan: "prolite", active: true,
                exhausted_until: nil, usage: Usage(available: true, windows: [w("5 hour", left: 78, 3 * h + 240), w("Weekly", left: 53, 4 * d + 23 * h)]),
                reset_credits: ResetCredits(count: 2)),
            Account(id: "codex-side", provider: "codex", email: "side@example.com", plan: "plus", active: false,
                exhausted_until: nil, usage: Usage(available: true, windows: [w("5 hour", left: 92, 4 * h + 3540), w("Weekly", left: 9, 2 * d + 5 * h)]),
                reset_credits: nil, usage_limit: 10, at_limit: true),
        ]
        return AppState(accounts: accounts, order: ["claude", "codex"], hidden: [], version: "1.30.0",
            update: UpdateInfo(latest: "v1.30.0", update_available: false), menu_usage_bars: true, reset_notifications: false,
            awake: AwakeState(supported: true, active: false))
    }

    // render lays the menu's items out as the open menu shows them, item
    // views as they are and plain items as macOS draws them, and saves them
    // drawn through their layers (the cards' colours live in the layers).
    static func render(_ menu: NSMenu, appearance: NSAppearance, to base: String) {
        let width = menuWidth
        var rows: [NSView] = []
        for item in menu.items {
            if let view = item.view {
                item.view = nil
                rows.append(view)
            } else if item.isSeparatorItem {
                let line = NSView(frame: NSRect(x: 0, y: 0, width: width, height: 11))
                let rule = NSView(frame: NSRect(x: 14, y: 5, width: width - 28, height: 1))
                rule.wantsLayer = true
                rule.layer?.backgroundColor = NSColor.separatorColor.cgColor
                line.addSubview(rule)
                rows.append(line)
            } else {
                let row = NSView(frame: NSRect(x: 0, y: 0, width: width, height: 24))
                let title = NSTextField(labelWithString: item.title)
                title.font = NSFont.menuFont(ofSize: 13)
                title.textColor = .labelColor
                title.frame = NSRect(x: 14, y: 3, width: width - 80, height: 17)
                row.addSubview(title)
                let trailing = item.submenu != nil ? "›" : (item.keyEquivalent.isEmpty ? "" : "⌘" + item.keyEquivalent.uppercased())
                if !trailing.isEmpty {
                    let key = NSTextField(labelWithString: trailing)
                    key.font = NSFont.menuFont(ofSize: item.submenu != nil ? 16 : 13)
                    key.textColor = .secondaryLabelColor
                    key.alignment = .right
                    key.frame = NSRect(x: width - 74, y: item.submenu != nil ? 1 : 3, width: 60, height: 19)
                    row.addSubview(key)
                }
                rows.append(row)
            }
        }
        let height = rows.reduce(CGFloat(10)) { $0 + $1.frame.height }
        let container = FlippedView(frame: NSRect(x: 0, y: 0, width: width, height: height))
        container.wantsLayer = true
        container.appearance = appearance
        var y: CGFloat = 5
        for row in rows {
            row.frame.origin = NSPoint(x: (width - row.frame.width) / 2, y: y)
            container.addSubview(row)
            y += row.frame.height
        }
        let window = NSWindow(contentRect: container.frame, styleMask: [.borderless], backing: .buffered, defer: false)
        window.appearance = appearance
        window.isOpaque = false
        window.backgroundColor = .clear
        window.contentView = container
        container.layoutSubtreeIfNeeded()
        window.displayIfNeeded()
        container.display()
        save(layered(container, scale: 2), to: base + ".png")
    }

    static func layered(_ view: NSView, scale: CGFloat) -> NSBitmapImageRep? {
        guard let layer = view.layer,
              let rep = NSBitmapImageRep(bitmapDataPlanes: nil, pixelsWide: Int(view.bounds.width * scale),
                  pixelsHigh: Int(view.bounds.height * scale), bitsPerSample: 8, samplesPerPixel: 4, hasAlpha: true,
                  isPlanar: false, colorSpaceName: .deviceRGB, bytesPerRow: 0, bitsPerPixel: 0),
              let context = NSGraphicsContext(bitmapImageRep: rep) else { return nil }
        let cg = context.cgContext
        cg.scaleBy(x: scale, y: scale)
        // Layers of a flipped view draw top-down.
        cg.translateBy(x: 0, y: view.bounds.height)
        cg.scaleBy(x: 1, y: -1)
        NSGraphicsContext.saveGraphicsState()
        NSGraphicsContext.current = context
        layer.render(in: cg)
        NSGraphicsContext.restoreGraphicsState()
        return rep
    }

    static func save(_ rep: NSBitmapImageRep?, to path: String) {
        guard let data = rep?.representation(using: .png, properties: [:]) else {
            FileHandle.standardError.write(Data("could not draw \(path)\n".utf8))
            return
        }
        try? data.write(to: URL(fileURLWithPath: path))
        print("wrote \(path)")
    }
}

final class FlippedView: NSView {
    override var isFlipped: Bool { true }
}
