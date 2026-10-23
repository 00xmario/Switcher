// Switcher menu bar app: supervises the bundled switcher server and shows
// per-account usage in a custom-drawn dropdown (cards, toggles, logos).
// Single-file Swift, compiled with swiftc (no Xcode project).
import AppKit
import UserNotifications

let hubURL = URL(string: "http://127.0.0.1:8787")!

// The server can rotate this device token while the app is running.
func currentDeviceToken() -> String? {
    #if SWITCHER_LAYOUT_TEST
    return nil
    #else
    guard let raw = try? String(contentsOfFile: NSHomeDirectory() + "/.switcher/local-token", encoding: .utf8) else { return nil }
    let token = raw.trimmingCharacters(in: .whitespacesAndNewlines)
    return token.count == 64 ? token : nil
    #endif
}

// The menu app and server share this local preference file. Consult it at
// delivery time as well as the cached state so a web toggle takes effect
// without waiting for the menu app's next state poll.
func resetAlertsEnabledOnDisk() -> Bool {
    #if SWITCHER_LAYOUT_TEST
    return false
    #else
    let path = NSHomeDirectory() + "/.switcher/settings.json"
    guard let data = try? Data(contentsOf: URL(fileURLWithPath: path)),
          let object = try? JSONSerialization.jsonObject(with: data) as? [String: Any] else { return false }
    return object["reset_notifications"] as? Bool == true
    #endif
}
let launchAgentLabel = "sh.switcher.app"
let launchAgentPath = NSHomeDirectory() + "/Library/LaunchAgents/" + launchAgentLabel + ".plist"
let menuWidth: CGFloat = 340
let edgeInset: CGFloat = 16      // menu-level padding (left + right)
let cardInset: CGFloat = 14      // padding inside cards

struct UsageWindow: Codable, Equatable {
    let label: String
    let used_percent: Int
    let resets_at: Double?
}

struct Usage: Codable, Equatable {
    let available: Bool
    let windows: [UsageWindow]?
}

struct ResetCredits: Codable, Equatable {
    let count: Int
}

func bankedResetText(_ credits: ResetCredits?) -> String? {
    guard let count = credits?.count, count > 0 else { return nil }
    return "⚡ \(count) banked"
}

struct Account: Codable, Equatable {
    let id: String
    let provider: String
    let email: String
    let plan: String?
    let active: Bool
    let exhausted_until: Double?
    let usage: Usage?
    let reset_credits: ResetCredits?
    var native_switch_available: Bool? = nil
    var native_active: Bool? = nil
    var health: AccountHealth? = nil
    var selected: Bool { native_switch_available == true ? native_active == true : active }
}

struct AccountHealth: Codable, Equatable {
    let condition: String?
}

struct UpdateInfo: Codable, Equatable {
    let latest: String?
    let update_available: Bool?
}

// InstallProgress mirrors the server's update.Progress: the step an install
// is on, how far along it is (0…1) and the bytes downloaded so far.
struct InstallProgress: Codable, Equatable {
    let step: String
    let fraction: Double
    var done: Int64? = nil
    var total: Int64? = nil
}

struct UpdateProgressResponse: Decodable {
    let install: InstallProgress?
}

func installStepText(_ progress: InstallProgress) -> String {
    switch progress.step {
    case "downloading":
        guard let total = progress.total, total > 0 else { return "Downloading…" }
        let megabytes = { (bytes: Int64) in String(format: "%.1f", Double(bytes) / 1_000_000) }
        return "Downloading · \(megabytes(progress.done ?? 0)) of \(megabytes(total)) MB"
    case "checking": return "Checking the new version"
    case "installing": return "Installing"
    case "restarting": return "Restarting Switcher…"
    default: return "Starting…"
    }
}

// AwakeState mirrors the server's keep-awake state (internal/awake).
struct AwakeState: Codable, Equatable {
    let supported: Bool
    let active: Bool
    var until: Double? = nil
    var lid_active: Bool? = nil
    var lid_ready: Bool? = nil
    var allow_display_sleep: Bool? = nil
    var lid_closed: Bool? = nil
    var battery_floor: Int? = nil
    var ended: String? = nil
    var error: String? = nil
}

struct AppState: Codable, Equatable {
    let accounts: [Account]
    let order: [String]?
    let hidden: [String]?
    let version: String?
    let update: UpdateInfo?
    let menu_usage_bars: Bool?
    let reset_notifications: Bool?
    var awake: AwakeState? = nil

    func withUpdate(_ info: UpdateInfo) -> AppState {
        AppState(accounts: accounts, order: order, hidden: hidden, version: version,
            update: info, menu_usage_bars: menu_usage_bars, reset_notifications: reset_notifications, awake: awake)
    }

    func withAwake(_ state: AwakeState) -> AppState {
        AppState(accounts: accounts, order: order, hidden: hidden, version: version,
            update: update, menu_usage_bars: menu_usage_bars, reset_notifications: reset_notifications, awake: state)
    }
}

// Keep-awake chips: label and minutes, 0 meaning until turned off.
let awakePresets: [(label: String, minutes: Int)] = [
    ("15m", 15), ("30m", 30), ("1h", 60), ("2h", 120), ("4h", 240), ("8h", 480), ("∞", 0),
]

private let awakeTimeFormatter: DateFormatter = {
    let formatter = DateFormatter()
    formatter.dateStyle = .none
    formatter.timeStyle = .short
    return formatter
}()

func awakeClock(_ date: Date) -> String { awakeTimeFormatter.string(from: date) }

func awakeStatusText(_ state: AwakeState) -> String {
    guard state.active else { return "Keep the Mac awake" }
    guard let until = state.until else { return "Active until you turn it off" }
    let left = resetRemaining(until) ?? "under a minute"
    return "Until \(awakeClock(Date(timeIntervalSince1970: until))) · \(left) left"
}

// What the closed Keep awake header says, and whether it reads as on.
func awakeSummary(_ state: AwakeState) -> (text: String, on: Bool) {
    if let error = state.error, !error.isEmpty { return ("Needs attention", false) }
    guard state.active else { return ("Off", false) }
    guard let until = state.until else { return ("On", true) }
    return ("On · " + (resetRemaining(until) ?? "under a minute") + " left", true)
}

func awakeLidSubtitle(_ state: AwakeState) -> String {
    if state.lid_active == true { return "Sleep fully disabled. Mind the power" }
    if state.lid_closed == true {
        if !state.active { return "Starts with keep awake" }
        return (state.error ?? "").isEmpty ? "Sleep is already off, set by another app" : "The closed lid did not apply"
    }
    return state.lid_ready == true ? "Closing the lid sleeps the Mac" : "Asks for your password once"
}

func awakeNotice(_ state: AwakeState) -> String? {
    if let error = state.error, !error.isEmpty { return error }
    guard !state.active else { return nil }
    switch state.ended {
    case "time": return "Time is up. The Mac sleeps normally again."
    case "battery": return "Stopped at low battery. The Mac sleeps normally again."
    default: return nil
    }
}

// The next whole hours, for "Until…".
func awakeUntilChoices(now: Date = Date(), count: Int = 7) -> [Date] {
    let calendar = Calendar.current
    guard let hour = calendar.dateInterval(of: .hour, for: now)?.start else { return [] }
    return (1...count).compactMap { calendar.date(byAdding: .hour, value: $0, to: hour) }
}

private let resetPrefix = "sh.switcher.reset."
private let resetCatchupInterval: TimeInterval = 24 * 3600
private let resetRolloverLead: TimeInterval = 5 * 60

struct ResetAlert: Codable, Equatable {
    let id: String
    let accountID: String
    let providerID: String
    let provider: String
    let window: String
    let date: Date
}

struct ResetAlertLedger: Codable, Equatable {
    let alerts: [ResetAlert]
    let deliveredIDs: [String]
}

struct ResetAlertPermission: Equatable {
    let message: String
    let canSend: Bool
    var short: String = ""   // the few words the menu's alerts row has room for
}

func resetAlertPermission(_ status: UNAuthorizationStatus, alertSetting: UNNotificationSetting,
                          alertStyle: UNAlertStyle, centerSetting: UNNotificationSetting) -> ResetAlertPermission {
    switch status {
    case .authorized where alertSetting == .enabled && alertStyle != .none:
        return ResetAlertPermission(message: "macOS allows banners (Focus may silence)", canSend: true, short: "Banners allowed")
    case .authorized where centerSetting == .enabled:
        return ResetAlertPermission(message: "Reset alerts go to Notification Center", canSend: true, short: "Notification Center only")
    case .authorized:
        return ResetAlertPermission(message: "Reset alerts hidden in macOS Settings", canSend: false, short: "Hidden in macOS Settings")
    case .provisional where centerSetting == .enabled:
        return ResetAlertPermission(message: "Reset alerts delivered quietly by macOS", canSend: true, short: "Delivered quietly")
    case .provisional:
        return ResetAlertPermission(message: "Reset alerts hidden in macOS Settings", canSend: false, short: "Hidden in macOS Settings")
    case .denied:
        return ResetAlertPermission(message: "Reset alerts blocked in macOS Settings", canSend: false, short: "Blocked in macOS Settings")
    case .notDetermined:
        return ResetAlertPermission(message: "Waiting for notification permission", canSend: false, short: "Waiting for permission")
    @unknown default:
        return ResetAlertPermission(message: "Check macOS notification permission", canSend: false, short: "Check macOS Settings")
    }
}

func resetAlertPermission(_ settings: UNNotificationSettings) -> ResetAlertPermission {
    resetAlertPermission(settings.authorizationStatus, alertSetting: settings.alertSetting,
        alertStyle: settings.alertStyle, centerSetting: settings.notificationCenterSetting)
}

func sendTestResetAlert(completion: @escaping (String?) -> Void) {
    #if SWITCHER_LAYOUT_TEST
    completion("Notification delivery disabled in fixtures")
    #else
    guard resetAlertsEnabledOnDisk() else {
        completion("Enable reset alerts in Switcher Settings first")
        return
    }
    let center = UNUserNotificationCenter.current()
    center.getNotificationSettings { settings in
        let permission = resetAlertPermission(settings)
        guard permission.canSend, resetAlertsEnabledOnDisk() else {
            completion(permission.canSend ? "Reset alerts were turned off" : permission.message)
            return
        }
        let content = UNMutableNotificationContent()
        content.title = "Switcher reset alert test"
        content.body = "Switcher can send macOS notifications."
        content.sound = .default
        center.add(UNNotificationRequest(identifier: "sh.switcher.test." + UUID().uuidString,
            content: content, trigger: nil)) { error in completion(error?.localizedDescription) }
    }
    #endif
}

func loadResetAlertLedger(at url: URL) throws -> ResetAlertLedger {
    guard FileManager.default.fileExists(atPath: url.path) else {
        return ResetAlertLedger(alerts: [], deliveredIDs: [])
    }
    let decoder = JSONDecoder()
    decoder.dateDecodingStrategy = .secondsSince1970
    return try decoder.decode(ResetAlertLedger.self, from: Data(contentsOf: url))
}

func saveResetAlertLedger(_ ledger: ResetAlertLedger, at url: URL) throws {
    let encoder = JSONEncoder()
    encoder.dateEncodingStrategy = .secondsSince1970
    try encoder.encode(ledger).write(to: url, options: .atomic)
    try FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: url.path)
}

func desiredResetAlerts(_ state: AppState, now: Date = Date()) -> [ResetAlert] {
    guard state.reset_notifications == true else { return [] }
    let end = now.addingTimeInterval(30 * 86400)
    var alerts: [ResetAlert] = []
    for account in state.accounts where account.usage?.available == true {
        for (index, window) in (account.usage?.windows ?? []).enumerated() {
            guard let seconds = window.resets_at else { continue }
            let date = Date(timeIntervalSince1970: seconds)
            guard date > now && date <= end else { continue }
            alerts.append(ResetAlert(id: resetPrefix + account.id + ".\(index).\(Int(seconds))",
                accountID: account.id, providerID: account.provider,
                provider: providerNames[account.provider] ?? account.provider, window: window.label, date: date))
        }
    }
    return Array(alerts.sorted { $0.date == $1.date ? $0.id < $1.id : $0.date < $1.date }.prefix(32))
}

// A provider can roll a used window to the next period before the menu's
// timer runs. Keep an alert near its reported time while the same account
// and window still exist, even if usage is already back to zero.
func reconciledResetAlerts(_ previous: [ResetAlert], state: AppState, now: Date) -> [ResetAlert] {
    guard state.reset_notifications == true else { return [] }
    let upcoming = desiredResetAlerts(state, now: now)
    let carried = previous.filter { alert in
        let delay = now.timeIntervalSince(alert.date)
        guard delay >= -resetRolloverLead && delay < resetCatchupInterval,
              let account = state.accounts.first(where: { $0.id == alert.accountID && $0.provider == alert.providerID }) else {
            return false
        }
        // An unavailable usage response cannot revoke an already planned
        // reset. A successful response that removed the window can.
        guard account.usage?.available == true else { return true }
        return (account.usage?.windows ?? []).contains { $0.label == alert.window }
    }
    let carriedIDs = Set(carried.map(\.id))
    return Array((carried + upcoming.filter { !carriedIDs.contains($0.id) }).prefix(32))
}

func dueResetAlerts(_ alerts: [ResetAlert], now: Date, delivered: Set<String>, sending: Set<String>) -> [ResetAlert] {
    alerts.filter { alert in
        let delay = now.timeIntervalSince(alert.date)
        return delay >= 0 && delay < resetCatchupInterval && !delivered.contains(alert.id) && !sending.contains(alert.id)
    }
}

func recordDeliveredResetAlert(_ id: String, alerts: inout [ResetAlert], delivered: inout Set<String>) {
    delivered.insert(id)
    alerts.removeAll { $0.id == id }
}

let providerNames = ["codex": "Codex", "claude": "Claude", "grok": "Grok", "opencode": "OpenCode",
    "antigravity": "Antigravity", "gemini": "Gemini", "copilot": "Copilot"]
let planNames = ["claude_max_5x": "Max 5x", "claude_max_20x": "Max 20x", "claude_max": "Max", "claude_pro": "Pro",
    "max": "Max", "pro": "Pro 20x", "prolite": "Pro 5x", "plus": "Plus", "free": "Free",
    "copilot_pro": "Pro", "copilot_pro_plus": "Pro+", "copilot_business": "Business", "copilot_enterprise": "Enterprise",
    "copilot_free": "Free"]

// Palette mirrors the web app's light and dark tokens (web/style.css).
// Layer colours cannot be dynamic NSColors, so the menu picks a palette from
// the effective appearance every time it is rebuilt.
struct Palette {
    let ink: NSColor
    let dim: NSColor
    let accent: NSColor
    let surface: NSColor
    let border: NSColor
    let hover: NSColor
    let dark: Bool

    static let light = Palette(
        ink: NSColor(hex: 0x17191d), dim: NSColor(hex: 0x7a818c), accent: NSColor(hex: 0x2f9e5f),
        surface: NSColor(hex: 0xffffff), border: NSColor(hex: 0xe4e4e8),
        hover: NSColor.black.withAlphaComponent(0.05), dark: false)
    static let dark = Palette(
        ink: NSColor(hex: 0xf5f5f5), dim: NSColor(hex: 0xa3a3a3), accent: NSColor(hex: 0x3fb96f),
        surface: NSColor(hex: 0x151515), border: NSColor(hex: 0x242424),
        hover: NSColor.white.withAlphaComponent(0.06), dark: true)

    static func current() -> Palette {
        let appearance = NSApp?.effectiveAppearance ?? NSAppearance.currentDrawing()
        return appearance.bestMatch(from: [.darkAqua, .aqua]) == .darkAqua ? dark : light
    }
}

extension NSColor {
    convenience init(hex: Int) {
        self.init(red: CGFloat((hex >> 16) & 0xff) / 255, green: CGFloat((hex >> 8) & 0xff) / 255,
            blue: CGFloat(hex & 0xff) / 255, alpha: 1)
    }
}

var palette = Palette.current()
var inkColor: NSColor { palette.ink }
var dimColor: NSColor { palette.dim }
var accentColor: NSColor { palette.accent }
var surfaceColor: NSColor { palette.surface }
var cardBorderColor: NSColor { palette.border }
let warnColor = NSColor(red: 0.85, green: 0.64, blue: 0.31, alpha: 1)

func label(_ text: String, font: NSFont, color: NSColor) -> NSTextField {
    let field = NSTextField(labelWithString: text)
    field.font = font
    field.textColor = color
    field.lineBreakMode = .byTruncatingTail
    field.alignment = .left
    return field
}

let centeredParagraph: NSParagraphStyle = {
    let style = NSMutableParagraphStyle()
    style.alignment = .center
    return style
}()

func cardView(width: CGFloat, height: CGFloat) -> NSView {
    let card = HoverCard(frame: NSRect(x: 0, y: 0, width: width, height: height))
    card.wantsLayer = true
    card.layer?.backgroundColor = surfaceColor.cgColor
    card.layer?.borderColor = cardBorderColor.cgColor
    card.layer?.borderWidth = 1
    card.layer?.cornerRadius = 10
    card.layer?.masksToBounds = true
    return card
}

func sectionHead(_ providerID: String, contentWidth: CGFloat, syncPhase: String = "idle", onSync: (() -> Void)? = nil) -> NSView {
    let head = NSView(frame: NSRect(x: 0, y: 0, width: menuWidth, height: 32))
    let logo = NSImageView(frame: NSRect(x: edgeInset, y: 5, width: 22, height: 22))
    logo.imageScaling = .scaleProportionallyUpOrDown
    logo.image = providerLogoImage(providerID)
    logo.contentTintColor = inkColor
    let name = label(providerNames[providerID] ?? providerID,
        font: NSFont.systemFont(ofSize: 13, weight: .semibold), color: inkColor)
    let nameWidth = providerID == "claude"
        ? ceil(textWidth(name.stringValue, font: name.font!)) : contentWidth - 32
    name.frame = NSRect(x: edgeInset + 22 + 10, y: 8, width: nameWidth, height: 16)
    head.addSubview(logo)
    head.addSubview(name)
    if providerID == "claude" {
        let button = ClaudeSyncButton(phase: syncPhase, onSync: onSync)
        button.frame = NSRect(x: name.frame.maxX + 6, y: 3, width: 26, height: 26)
        head.addSubview(button)
    }
    return head
}

// A native secondary button with a small progress indicator. Unlike the
// usage refresh animation, its busy state lasts for the actual request.
final class ClaudeSyncButton: NSButton {
    private let onSync: (() -> Void)?
    private var tracking: NSTrackingArea?
    init(phase: String, onSync: (() -> Void)?) {
        self.onSync = onSync
        super.init(frame: NSRect(x: 0, y: 0, width: 26, height: 26))
        isBordered = false
        title = ""
        setAccessibilityLabel("Sync Claude Code sessions between accounts")
        toolTip = "Sync Claude Code sessions between accounts\nConfirmation required: this closes and reopens Claude Desktop."
        target = self
        action = #selector(clicked)
        wantsLayer = true
        layer?.cornerRadius = 7
        let symbol = phase == "success" ? "checkmark" : phase == "error" ? "exclamationmark.circle" : "arrow.left.arrow.right"
        image = NSImage(systemSymbolName: symbol, accessibilityDescription: nil)
        symbolConfiguration = NSImage.SymbolConfiguration(pointSize: 13, weight: .medium)
        contentTintColor = phase == "success" ? accentColor : phase == "error" ? NSColor.systemRed : dimColor
        isEnabled = phase != "running"
        if phase == "running" {
            image = nil
            let spinner = NSProgressIndicator(frame: NSRect(x: 6, y: 6, width: 14, height: 14))
            spinner.style = .spinning
            spinner.controlSize = .small
            spinner.startAnimation(nil)
            addSubview(spinner)
            setAccessibilityValue("Syncing")
        }
    }
    required init?(coder: NSCoder) { fatalError("unused") }
    override func updateTrackingAreas() {
        super.updateTrackingAreas()
        if let tracking = tracking { removeTrackingArea(tracking) }
        tracking = NSTrackingArea(rect: bounds, options: [.mouseEnteredAndExited, .activeAlways, .inVisibleRect], owner: self)
        if let tracking = tracking { addTrackingArea(tracking) }
    }
    override func mouseEntered(with event: NSEvent) { if isEnabled { layer?.backgroundColor = palette.hover.cgColor } }
    override func mouseExited(with event: NSEvent) { layer?.backgroundColor = NSColor.clear.cgColor }
    @objc private func clicked() { onSync?() }
}

struct ClaudeSyncResponse: Decodable {
    let detail: String?
    let error: String?
    let details: String?
    let result: SyncResult?
    struct SyncResult: Decodable { let backup: String? }
}

struct AccountActivationResponse: Decodable {
    let error: String?
    let details: String?
    let backup: String?
    let native: NativeResult?
    struct NativeResult: Decodable { let message: String? }
}

func claudeSyncConfirmation() -> NSAlert {
    let alert = NSAlert()
    alert.messageText = "Sync Claude Code sessions?"
    alert.informativeText = "This will close and reopen Claude Desktop, even if it is running in the background. Finish any active work first. Switcher will back up the Desktop session indexes and copy only missing session pointers between accounts. Existing records and conversation transcripts stay unchanged."
    alert.addButton(withTitle: "Cancel")
    alert.addButton(withTitle: "Sync sessions")
    alert.buttons[0].keyEquivalent = "\r"
    alert.buttons[1].keyEquivalent = ""
    return alert
}

// Copilot's single-path mark and Grok's glyph follow the current palette;
// other provider marks retain their own colours.
func logoUsesTemplate(_ providerID: String) -> Bool {
    providerID == "grok" || providerID == "copilot"
}

// providerLogoImage loads the same provider marks used by the web app.
func providerLogoImage(_ providerID: String) -> NSImage? {
    let key = "\(providerID)|\(palette.dark)" as NSString
    if let cached = menuLogoCache.object(forKey: key) { return cached }
    guard let url = Bundle.main.url(forResource: providerID, withExtension: "svg") else {
        return nil
    }
    guard let image = NSImage(contentsOf: url) else { return nil }
    image.isTemplate = logoUsesTemplate(providerID)
    let result = providerID == "opencode" && palette.dark
        ? invertedImage(image, size: NSSize(width: 22, height: 22)) ?? image : image
    menuLogoCache.setObject(result, forKey: key)
    return result
}

private let menuImageContext = CIContext()
private let menuLogoCache: NSCache<NSString, NSImage> = {
    let cache = NSCache<NSString, NSImage>(); cache.countLimit = 32; return cache
}()
private let menuBlurCache: NSCache<NSString, NSImage> = {
    let cache = NSCache<NSString, NSImage>(); cache.countLimit = 128; cache.totalCostLimit = 8 * 1024 * 1024; return cache
}()
private let menuTextWidths: NSCache<NSString, NSNumber> = {
    let cache = NSCache<NSString, NSNumber>(); cache.countLimit = 512; return cache
}()
private let menuResetFormatter = DateFormatter()
private var menuResetFormatKey = ""

// invertedImage renders an image and inverts its colours (dark mode
// counterpart of the web app's CSS invert on the OpenCode mark).
func invertedImage(_ image: NSImage, size: NSSize) -> NSImage? {
    let view = NSImageView(frame: NSRect(origin: .zero, size: size))
    view.image = image
    view.imageScaling = .scaleProportionallyUpOrDown
    guard let rep = renderBitmap(view), let cg = rep.cgImage,
          let filter = CIFilter(name: "CIColorInvert") else { return nil }
    let input = CIImage(cgImage: cg)
    filter.setValue(input, forKey: kCIInputImageKey)
    // Invert colour only; keep the original alpha so transparent areas stay transparent.
    guard let inverted = filter.outputImage,
          let masked = CIFilter(name: "CIBlendWithAlphaMask", parameters: [
              kCIInputImageKey: inverted, kCIInputBackgroundImageKey: CIImage.empty(), kCIInputMaskImageKey: input,
          ])?.outputImage,
          let result = menuImageContext.createCGImage(masked, from: input.extent) else { return nil }
    return NSImage(cgImage: result, size: size)
}

// resetRemaining renders the countdown until a usage window resets, the
// same formatting the web app uses: 38m, 6h 12m, or 2d 3h. Nil when there
// is no reset time or it has already passed.
func resetRemaining(_ untilUnix: Double?) -> String? {
    guard let until = untilUnix, until > 0 else { return nil }
    let ms = until * 1000 - Date().timeIntervalSince1970 * 1000
    if ms <= 0 { return nil }
    let m = Int(ms / 60000)
    if m < 60 { return "\(m)m" }
    let h = m / 60
    if h < 24 { return "\(h)h \(m % 60)m" }
    let d = h / 24
    return "\(d)d \(h % 24)h"
}

func exactResetTime(_ untilUnix: Double?) -> String? {
    guard let untilUnix = untilUnix, untilUnix.isFinite,
          untilUnix > Date().timeIntervalSince1970 else { return nil }
    let formatter = menuResetFormatter
    let formatKey = Locale.current.identifier + "|" + TimeZone.current.identifier
    if menuResetFormatKey != formatKey {
        formatter.locale = .current
        formatter.dateStyle = .full
        formatter.timeStyle = .full
        formatter.timeZone = .current
        menuResetFormatKey = formatKey
    }
    let date = Date(timeIntervalSince1970: untilUnix)
    let offset = TimeZone.current.secondsFromGMT(for: date)
    let sign = offset >= 0 ? "+" : "-"
    let hours = abs(offset) / 3600
    let minutes = abs(offset) % 3600 / 60
    return "Provider-reported reset: " + formatter.string(from: date) +
        String(format: " (UTC%@%02d:%02d)", sign, hours, minutes)
}

// textWidth measures the frame width a label needs for a string, using a
// real NSTextField so the field's own padding is included.
func textWidth(_ text: String, font: NSFont) -> CGFloat {
    let key = String(reflecting: [text, font.fontName, String(describing: font.pointSize),
        String(describing: font.fontDescriptor.object(forKey: .traits))]) as NSString
    if let cached = menuTextWidths.object(forKey: key) { return CGFloat(cached.doubleValue) }
    let probe = NSTextField(labelWithString: text)
    probe.font = font
    probe.sizeToFit()
    let width = ceil(probe.frame.width) + 2
    menuTextWidths.setObject(NSNumber(value: Double(width)), forKey: key)
    return width
}

struct UsageLineLayout {
    let label: NSRect
    let value: NSRect
    let reset: NSRect?
    let resetText: String?
    let bar: NSRect?
}

struct UsageLayout {
    let lines: [UsageLineLayout]
    let checkmark: NSRect
}

// Layout is shared by rendering and a geometry check. All frames use the
// account row's coordinates so title actions and usage lines can be checked
// for collisions before views are created.
func usageLayout(windows: [UsageWindow], titleY: CGFloat,
                 lineWidth: CGFloat, contentWidth: CGFloat, textX: CGFloat,
                 cardInset: CGFloat, lineHeight: CGFloat, titleGap: CGFloat,
                 showBars: Bool) -> UsageLayout {
    let usageFont = NSFont.systemFont(ofSize: 11)
    let valueFont = NSFont.monospacedDigitSystemFont(ofSize: 11, weight: .regular)
    let resetFont = NSFont.systemFont(ofSize: 10.5)
    let resetTexts = windows.map { resetRemaining($0.resets_at).map { "↻ " + $0 } }
    let resetColumn = resetTexts.compactMap { $0 }
        .map { textWidth($0, font: resetFont) }.max() ?? 0
    let resetSpace = resetColumn > 0 ? resetColumn + 12 : 0
    let minValueWidth = textWidth("100% left", font: valueFont)
    let maxLabelWidth = max(0, lineWidth - 5 - resetSpace - minValueWidth)
    let labelColumn = min(windows.map { textWidth($0.label + ":", font: usageFont) }.max() ?? 0,
                          maxLabelWidth)
    // A tiny indicator fits before the percentage without increasing row
    // height. If a provider supplies a longer window label, keep the text
    // legible and omit the bar for that account instead of squeezing it.
    let barWidth: CGFloat = 38
    let barGap: CGFloat = 8
    let valueSpace = lineWidth - labelColumn - 5 - resetSpace
    let barsFit = showBars && valueSpace >= barWidth + barGap + minValueWidth
    var usageY = titleY - titleGap - lineHeight
    let lines = resetTexts.map { reset -> UsageLineLayout in
        let valueX = textX + labelColumn + 5 + (barsFit ? barWidth + barGap : 0)
        let line = UsageLineLayout(
            label: NSRect(x: textX, y: usageY, width: labelColumn, height: lineHeight),
            value: NSRect(x: valueX, y: usageY,
                          width: valueSpace - (barsFit ? barWidth + barGap : 0),
                          height: lineHeight),
            reset: reset.map { _ in NSRect(x: contentWidth - cardInset - resetColumn,
                                          y: usageY + 0.5, width: resetColumn, height: lineHeight) },
            resetText: reset,
            bar: barsFit ? NSRect(x: textX + labelColumn + 5, y: usageY + (lineHeight - 3) / 2,
                                  width: barWidth, height: 3) : nil)
        usageY -= lineHeight
        return line
    }
    // Keep the active mark on the account title line, above every usage
    // row, rather than centering it over a countdown.
    let checkmark = NSRect(x: contentWidth - cardInset - 14, y: titleY + 0.5,
                           width: 14, height: 14)
    return UsageLayout(lines: lines, checkmark: checkmark)
}

// ClickableRow is one account line. Hover state is driven by the card
// that contains it (see HoverCard), so rows can never get out of sync.
final class ClickableRow: NSView {
    var onClicked: (() -> Void)?
    var onHover: ((Bool) -> Void)?
    private(set) var hovered = false

    override init(frame: NSRect) {
        super.init(frame: frame)
        wantsLayer = true
        layer?.cornerRadius = 8
        layer?.masksToBounds = true
    }

    required init?(coder: NSCoder) { fatalError("unused") }

    func setHovered(_ value: Bool) {
        guard value != hovered else { return }
        hovered = value
        layer?.backgroundColor = value ? palette.hover.cgColor : NSColor.clear.cgColor
        onHover?(value)
    }

    override func mouseDown(with event: NSEvent) {
        if let handler = onClicked { handler() }
    }
}

// HoverCard holds account rows. Hover is not driven by AppKit tracking
// areas (menus drop and reorder enter/exit events); the delegate polls the
// cursor while the menu is open and calls updateHover.
final class HoverCard: NSView {
    private func rows() -> [ClickableRow] { subviews.compactMap { $0 as? ClickableRow } }

    func updateHover(screenPoint: NSPoint) {
        guard let window = window else { return }
        let point = convert(window.convertPoint(fromScreen: screenPoint), from: nil)
        for row in rows() { row.setHovered(row.frame.contains(point)) }
    }

    func clearHover() { rows().forEach { $0.setHovered(false) } }
}

// renderBitmap draws a view into a Retina bitmap so it can be turned into a
// static image (used for the blurred email and the spinner icon).
func renderBitmap(_ view: NSView) -> NSBitmapImageRep? {
    let size = view.bounds.size
    guard let rep = NSBitmapImageRep(bitmapDataPlanes: nil, pixelsWide: Int(size.width * 2), pixelsHigh: Int(size.height * 2),
        bitsPerSample: 8, samplesPerPixel: 4, hasAlpha: true, isPlanar: false,
        colorSpaceName: .deviceRGB, bytesPerRow: 0, bitsPerPixel: 0) else { return nil }
    rep.size = size
    view.cacheDisplay(in: view.bounds, to: rep)
    return rep
}

// blurredImage renders a label once through CIGaussianBlur. Same privacy
// pattern as the web app: unreadable until hovered.
let blurRadius: Double = 5

func blurredImage(of field: NSTextField) -> NSImage? {
    let key = String(reflecting: [field.stringValue, String(describing: field.bounds.size),
        field.font?.fontName ?? "", String(describing: field.font?.pointSize),
        String(describing: field.font?.fontDescriptor.object(forKey: .traits)),
        String(describing: field.textColor?.usingColorSpace(.deviceRGB)), String(palette.dark), "scale=2"]) as NSString
    if let cached = menuBlurCache.object(forKey: key) { return cached }
    guard let rep = renderBitmap(field), let cg = rep.cgImage else { return nil }
    let input = CIImage(cgImage: cg)
    guard let clamp = CIFilter(name: "CIAffineClamp"), let blur = CIFilter(name: "CIGaussianBlur") else { return nil }
    clamp.setValue(input, forKey: kCIInputImageKey)
    clamp.setValue(CGAffineTransform.identity, forKey: kCIInputTransformKey)
    blur.setValue(clamp.outputImage, forKey: kCIInputImageKey)
    blur.setValue(blurRadius * 2, forKey: kCIInputRadiusKey) // radius in 2x pixels
    guard let output = blur.outputImage?.cropped(to: input.extent),
          let result = menuImageContext.createCGImage(output, from: input.extent) else { return nil }
    let image = NSImage(cgImage: result, size: field.bounds.size)
    menuBlurCache.setObject(image, forKey: key, cost: result.bytesPerRow * result.height)
    return image
}

// BlurredLabel stacks a blurred snapshot above the real label and
// cross-fades layer opacity on hover. Layer animations are used on purpose:
// AppKit's animator() stalls inside the menu tracking run loop.
final class BlurredLabel: NSView {
    private let veil = NSImageView()
    private let field: NSTextField

    init(field: NSTextField) {
        self.field = field
        super.init(frame: field.frame)
        wantsLayer = true
        field.frame.origin = .zero
        field.wantsLayer = true
        addSubview(field)
        veil.frame = bounds
        veil.imageScaling = .scaleNone
        veil.image = blurredImage(of: field)
        veil.wantsLayer = true
        addSubview(veil)
        field.layer?.opacity = 0
    }

    required init?(coder: NSCoder) { fatalError("unused") }

    func reveal(_ show: Bool) {
        fade(veil.layer, to: show ? 0 : 1)
        fade(field.layer, to: show ? 1 : 0)
    }

    private func fade(_ layer: CALayer?, to target: Float) {
        guard let layer = layer else { return }
        let animation = CABasicAnimation(keyPath: "opacity")
        animation.fromValue = layer.presentation()?.opacity ?? layer.opacity
        animation.toValue = target
        animation.duration = 0.18
        animation.timingFunction = CAMediaTimingFunction(name: .easeOut)
        layer.removeAnimation(forKey: "fade")
        layer.opacity = target
        layer.add(animation, forKey: "fade")
    }
}

// ToggleSwitch is a custom on/off control drawn in the app accent. NSSwitch
// cannot be tinted and renders grey inside menus (inactive window style).
final class ToggleSwitch: NSView {
    var isOn: Bool { didSet { render(animated: true) } }
    var onChanged: ((Bool) -> Void)?
    private let knob = CALayer()

    init(isOn: Bool) {
        self.isOn = isOn
        super.init(frame: NSRect(x: 0, y: 0, width: 40, height: 24))
        wantsLayer = true
        layer?.cornerRadius = 12
        knob.frame = CGRect(x: 2, y: 2, width: 20, height: 20)
        knob.cornerRadius = 10
        knob.backgroundColor = NSColor.white.cgColor
        knob.shadowColor = NSColor.black.cgColor
        knob.shadowOpacity = 0.18
        knob.shadowRadius = 1.5
        knob.shadowOffset = CGSize(width: 0, height: -0.5)
        layer?.addSublayer(knob)
        render(animated: false)
    }

    required init?(coder: NSCoder) { fatalError("unused") }

    private func render(animated: Bool) {
        CATransaction.begin()
        CATransaction.setDisableActions(!animated)
        CATransaction.setAnimationDuration(0.18)
        layer?.backgroundColor = (isOn ? accentColor : NSColor.systemGray.withAlphaComponent(0.35)).cgColor
        knob.frame.origin.x = isOn ? bounds.width - 22 : 2
        CATransaction.commit()
    }

    override func mouseDown(with event: NSEvent) {
        isOn.toggle()
        onChanged?(isOn)
    }
}

// ChipButton is one keep-awake choice: a pill that fills with the accent
// while it is the running choice.
final class ChipButton: NSView {
    var onClicked: (() -> Void)?
    let title: String
    let selected: Bool

    init(title: String, selected: Bool, frame: NSRect) {
        self.title = title
        self.selected = selected
        super.init(frame: frame)
        wantsLayer = true
        layer?.cornerRadius = frame.height / 2
        layer?.backgroundColor = (selected ? accentColor
            : (palette.dark ? NSColor.white.withAlphaComponent(0.08) : NSColor.black.withAlphaComponent(0.06))).cgColor
        let text = label(title, font: NSFont.systemFont(ofSize: 12, weight: .medium), color: selected ? .white : inkColor)
        text.alignment = .center
        let height = ceil(text.intrinsicContentSize.height)
        text.frame = NSRect(x: 2, y: (frame.height - height) / 2, width: frame.width - 4, height: height)
        addSubview(text)
        setAccessibilityRole(.button)
        setAccessibilityLabel(title)
    }

    required init?(coder: NSCoder) { fatalError("unused") }

    override func hitTest(_ point: NSPoint) -> NSView? { frame.contains(point) ? self : nil }

    override func mouseDown(with event: NSEvent) { onClicked?() }
}

// TapArea turns a group of labels into one clickable control.
final class TapArea: NSView {
    var onClicked: (() -> Void)?

    override func hitTest(_ point: NSPoint) -> NSView? { frame.contains(point) ? self : nil }

    override func mouseDown(with event: NSEvent) { onClicked?() }
}

// SpinnerButton shows an SF Symbol in a plain CALayer (so AppKit never
// resets its anchor point) and spins it around its center while an action
// runs.
final class SpinnerButton: NSView {
    var onClicked: (() -> Void)?
    private let iconLayer = CALayer()
    private var busy = false

    init(symbol: String, size: CGFloat, color: NSColor) {
        super.init(frame: NSRect(x: 0, y: 0, width: size, height: size))
        wantsLayer = true
        let config = NSImage.SymbolConfiguration(pointSize: size * 0.58, weight: .medium)
            .applying(NSImage.SymbolConfiguration(paletteColors: [color]))
        let symbolView = NSImageView(frame: bounds)
        symbolView.image = NSImage(systemSymbolName: symbol, accessibilityDescription: nil)?
            .withSymbolConfiguration(config)
        if let rep = renderBitmap(symbolView) {
            iconLayer.contents = rep.cgImage
        }
        iconLayer.contentsScale = 2
        iconLayer.contentsGravity = .resizeAspect
        iconLayer.frame = bounds
        layer?.addSublayer(iconLayer)
        toolTip = "Refresh usage"
    }

    required init?(coder: NSCoder) { fatalError("unused") }

    override func mouseDown(with event: NSEvent) {
        guard !busy else { return }
        onClicked?()
    }

    func setBusy(_ value: Bool) {
        busy = value
        #if SWITCHER_LAYOUT_TEST
        iconLayer.removeAnimation(forKey: "spin")
        #else
        if value && !NSWorkspace.shared.accessibilityDisplayShouldReduceMotion { spin() }
        else { iconLayer.removeAnimation(forKey: "spin") }
        #endif
    }

    func spin() {
        let rotation = CABasicAnimation(keyPath: "transform.rotation.z")
        rotation.fromValue = 0
        rotation.toValue = -2 * Double.pi
        rotation.duration = 0.8
        rotation.repeatCount = .infinity
        iconLayer.add(rotation, forKey: "spin")
    }
}

// UpdateProgressView is the install card: the release on its way, the step
// it is on and a bar that eases toward each report, with a soft shine
// running along the filled part while the work goes on. Layer animations
// are used because AppKit's animator() stalls in the menu's run loop.
final class UpdateProgressView: NSView {
    static let height: CGFloat = 66
    private let percent = label("", font: NSFont.monospacedDigitSystemFont(ofSize: 12, weight: .medium), color: dimColor)
    private let detail = label("", font: NSFont.systemFont(ofSize: 11), color: dimColor)
    private let track = CALayer()
    private let fill = CAGradientLayer()
    private let shine = CAGradientLayer()
    private(set) var progress: InstallProgress

    init(width: CGFloat, release: String?, progress: InstallProgress) {
        self.progress = progress
        super.init(frame: NSRect(x: 0, y: 0, width: width, height: Self.height))
        wantsLayer = true
        layer?.backgroundColor = surfaceColor.cgColor
        layer?.borderColor = cardBorderColor.cgColor
        layer?.borderWidth = 1
        layer?.cornerRadius = 10
        let inner = width - 2 * cardInset
        let titleText = release.map { "Updating to \($0)" } ?? "Updating Switcher"
        let title = label(titleText, font: NSFont.systemFont(ofSize: 12.5, weight: .semibold), color: inkColor)
        title.frame = NSRect(x: cardInset, y: 40, width: inner - 52, height: 16)
        addSubview(title)
        percent.alignment = .right
        percent.frame = NSRect(x: width - cardInset - 48, y: 40, width: 48, height: 16)
        addSubview(percent)

        track.frame = CGRect(x: cardInset, y: 29, width: inner, height: 6)
        track.cornerRadius = 3
        track.masksToBounds = true
        track.backgroundColor = (palette.dark ? NSColor.white.withAlphaComponent(0.1)
            : NSColor.black.withAlphaComponent(0.07)).cgColor
        layer?.addSublayer(track)
        fill.frame = CGRect(x: 0, y: 0, width: track.bounds.height, height: track.bounds.height)
        fill.cornerRadius = 3
        fill.masksToBounds = true
        fill.startPoint = CGPoint(x: 0, y: 0.5)
        fill.endPoint = CGPoint(x: 1, y: 0.5)
        fill.colors = [accentColor.blended(withFraction: 0.12, of: .black) ?? accentColor,
            accentColor.blended(withFraction: 0.22, of: .white) ?? accentColor].map(\.cgColor)
        track.addSublayer(fill)
        shine.frame = CGRect(x: -56, y: 0, width: 56, height: track.bounds.height)
        shine.startPoint = CGPoint(x: 0, y: 0.5)
        shine.endPoint = CGPoint(x: 1, y: 0.5)
        shine.colors = [0, 0.5, 0].map { NSColor.white.withAlphaComponent($0).cgColor }
        fill.addSublayer(shine)

        detail.frame = NSRect(x: cardInset, y: 9, width: inner, height: 14)
        addSubview(detail)
        setAccessibilityElement(true)
        setAccessibilityRole(.progressIndicator)
        setAccessibilityLabel(titleText)
        show(progress, animated: false)
        #if !SWITCHER_LAYOUT_TEST
        if !NSWorkspace.shared.accessibilityDisplayShouldReduceMotion { startShine() }
        #endif
    }

    required init?(coder: NSCoder) { fatalError("unused") }

    var fillWidth: CGFloat { fill.frame.width }
    var percentText: String { percent.stringValue }
    var detailText: String { detail.stringValue }

    func show(_ progress: InstallProgress, animated: Bool = true) {
        self.progress = progress
        let fraction = min(max(progress.fraction, 0), 1)
        percent.stringValue = "\(Int((fraction * 100).rounded()))%"
        detail.stringValue = installStepText(progress)
        detail.toolTip = detail.stringValue
        setAccessibilityValue(detail.stringValue + ", \(percent.stringValue)")
        CATransaction.begin()
        CATransaction.setDisableActions(!animated)
        CATransaction.setAnimationDuration(0.45)
        CATransaction.setAnimationTimingFunction(CAMediaTimingFunction(name: .easeOut))
        fill.frame.size.width = max(track.bounds.height, track.bounds.width * CGFloat(fraction))
        CATransaction.commit()
    }

    // The shine crosses the whole track; the fill clips it, so it only
    // shows on the part already done and rests a beat before the next pass.
    private func startShine() {
        let run = CABasicAnimation(keyPath: "position.x")
        run.fromValue = -shine.bounds.width / 2
        run.toValue = track.bounds.width + shine.bounds.width / 2
        run.duration = 1.8
        run.repeatCount = .infinity
        run.timingFunction = CAMediaTimingFunction(name: .easeInEaseOut)
        shine.add(run, forKey: "shine")
    }
}

struct OwnedServer {
    let id: UUID
    let terminate: () -> Void
}

private func onMain(_ action: @escaping () -> Void) {
    if Thread.isMainThread { action() } else { DispatchQueue.main.async(execute: action) }
}

private func fixtureRuntimeError() -> NSError {
    NSError(domain: "SwitcherFixture", code: 1,
        userInfo: [NSLocalizedDescriptionKey: "Runtime actions disabled in fixtures"])
}

// Only these adapters reach HTTP, private files, processes, or modal UI.
// Layout fixtures start with inert adapters and opt into synthetic ones.
struct MenuRuntime {
    var request: (URLRequest, @escaping (Data?, URLResponse?, Error?) -> Void) -> Void = { request, completion in
        #if SWITCHER_LAYOUT_TEST
        completion(nil, nil, fixtureRuntimeError())
        #else
        let configuration = URLSessionConfiguration.ephemeral
        configuration.timeoutIntervalForRequest = request.timeoutInterval
        configuration.timeoutIntervalForResource = request.timeoutInterval
        configuration.httpCookieStorage = nil
        configuration.urlCache = nil
        let session = URLSession(configuration: configuration)
        session.dataTask(with: request) { data, response, error in
            completion(data, response, error)
            session.finishTasksAndInvalidate()
        }.resume()
        #endif
    }
    var deviceToken: () -> String? = currentDeviceToken
    var alertsEnabled: () -> Bool = resetAlertsEnabledOnDisk
    var loadLedger: () throws -> ResetAlertLedger = {
        #if SWITCHER_LAYOUT_TEST
        return ResetAlertLedger(alerts: [], deliveredIDs: [])
        #else
        return try loadResetAlertLedger(at: URL(fileURLWithPath: NSHomeDirectory() + "/.switcher/reset-alerts.json"))
        #endif
    }
    var saveLedger: (ResetAlertLedger) throws -> Void = { ledger in
        #if !SWITCHER_LAYOUT_TEST
        try saveResetAlertLedger(ledger, at: URL(fileURLWithPath: NSHomeDirectory() + "/.switcher/reset-alerts.json"))
        #endif
    }
    var schedule: (TimeInterval, @escaping () -> Void) -> Void = { delay, task in
        #if !SWITCHER_LAYOUT_TEST
        DispatchQueue.main.asyncAfter(deadline: .now() + delay, execute: task)
        #endif
    }
    var launchServer: (@escaping (UUID) -> Void) throws -> OwnedServer = { exited in
        #if SWITCHER_LAYOUT_TEST
        throw fixtureRuntimeError()
        #else
        let server = URL(fileURLWithPath: Bundle.main.bundlePath + "/Contents/MacOS/SwitcherServer")
        let logDir = NSHomeDirectory() + "/.switcher"
        try FileManager.default.createDirectory(atPath: logDir, withIntermediateDirectories: true)
        let logPath = logDir + "/server.log"
        if !FileManager.default.fileExists(atPath: logPath) {
            FileManager.default.createFile(atPath: logPath, contents: nil, attributes: [.posixPermissions: 0o600])
        }
        try FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: logPath)
        let process = Process(), id = UUID()
        process.executableURL = server
        process.arguments = ["--port", "8787"]
        if let handle = FileHandle(forWritingAtPath: logPath) {
            handle.seekToEndOfFile()
            process.standardOutput = handle
            process.standardError = handle
        }
        process.terminationHandler = { _ in onMain { exited(id) } }
        try process.run()
        return OwnedServer(id: id, terminate: { if process.isRunning { process.terminate() } })
        #endif
    }
    var showError: (String, String) -> Void = { title, detail in
        #if !SWITCHER_LAYOUT_TEST
        let alert = NSAlert()
        alert.messageText = title
        alert.informativeText = detail
        alert.addButton(withTitle: "OK")
        alert.runModal()
        #endif
    }
    var terminateApplication: () -> Void = {
        #if !SWITCHER_LAYOUT_TEST
        NSApp.terminate(nil)
        #endif
    }
    // The version this app was launched as, and the version of the app now
    // on disk. They differ once an update replaced Switcher.app.
    var launchedAppVersion: () -> String? = {
        #if SWITCHER_LAYOUT_TEST
        return nil
        #else
        guard Bundle.main.bundlePath.hasSuffix(".app") else { return nil }
        return Bundle.main.object(forInfoDictionaryKey: "CFBundleShortVersionString") as? String
        #endif
    }
    var diskAppVersion: () -> String? = {
        #if SWITCHER_LAYOUT_TEST
        return nil
        #else
        let url = URL(fileURLWithPath: Bundle.main.bundlePath + "/Contents/Info.plist")
        guard let data = try? Data(contentsOf: url),
              let plist = try? PropertyListSerialization.propertyList(from: data, format: nil) as? [String: Any] else { return nil }
        return plist["CFBundleShortVersionString"] as? String
        #endif
    }
    // Small per-Mac menu preferences, such as a closed section.
    var loadFlag: (String) -> Bool = { key in
        #if SWITCHER_LAYOUT_TEST
        return false
        #else
        return UserDefaults.standard.bool(forKey: key)
        #endif
    }
    var saveFlag: (String, Bool) -> Void = { key, value in
        #if !SWITCHER_LAYOUT_TEST
        UserDefaults.standard.set(value, forKey: key)
        #endif
    }
    // relaunchApp opens the app again once this process has exited.
    var relaunchApp: () -> Void = {
        #if !SWITCHER_LAYOUT_TEST
        let process = Process()
        process.executableURL = URL(fileURLWithPath: "/bin/sh")
        process.arguments = ["-c", "while kill -0 \"$1\" 2>/dev/null; do sleep 0.2; done; exec /usr/bin/open \"$0\"",
            Bundle.main.bundlePath, String(ProcessInfo.processInfo.processIdentifier)]
        try? process.run()
        #endif
    }
}

func menuHTTPResult(_ data: Data?, response: URLResponse?, error: Error?) -> Result<Data, Error> {
    if let error = error {
        let failure = error as NSError
        if failure.domain == NSURLErrorDomain, failure.code == URLError.timedOut.rawValue {
            return .failure(NSError(domain: NSURLErrorDomain, code: failure.code,
                userInfo: [NSLocalizedDescriptionKey: "Switcher request timed out. Try again."]))
        }
        return .failure(error)
    }
    guard let response = response as? HTTPURLResponse else {
        return .failure(NSError(domain: "SwitcherHTTP", code: 0,
            userInfo: [NSLocalizedDescriptionKey: "Could not reach Switcher"]))
    }
    guard (200..<300).contains(response.statusCode) else {
        let body = data.flatMap { try? JSONSerialization.jsonObject(with: $0) as? [String: Any] }
        let message = body?["error"] as? String ?? (body?["error"] as? [String: Any])?["message"] as? String
        let backup = body?["backup"] as? String ?? (body?["result"] as? [String: Any])?["backup"] as? String
        let detail = [message, body?["details"] as? String, backup.flatMap { $0.isEmpty ? nil : "Backup: " + $0 }]
            .compactMap { $0 }.joined(separator: "\n")
        return .failure(NSError(domain: "SwitcherHTTP", code: response.statusCode,
            userInfo: [NSLocalizedDescriptionKey: detail.isEmpty ? "Switcher returned HTTP \(response.statusCode)" : detail]))
    }
    return .success(data ?? Data())
}

// badgedStatusImage puts the update badge on the menu bar mark: a disc at
// the lower right with an up arrow cut out of it, set apart from the mark
// by a thin gap. The image grows a little to the right, so the badge
// covers less of the S. Everything stays one template image, so macOS
// tints the badge with the mark for a light or dark menu bar.
func badgedStatusImage(_ mark: NSImage) -> NSImage {
    let size = NSSize(width: mark.size.width + mark.size.height * 0.3, height: mark.size.height)
    let image = NSImage(size: size, flipped: false) { rect in
        guard let context = NSGraphicsContext.current?.cgContext else { return false }
        mark.draw(in: NSRect(origin: .zero, size: mark.size))
        let radius = rect.height * 0.25
        let center = CGPoint(x: rect.maxX - radius, y: rect.minY + radius)
        func disc(_ r: CGFloat) -> CGRect { CGRect(x: center.x - r, y: center.y - r, width: 2 * r, height: 2 * r) }
        context.setBlendMode(.clear)
        context.fillEllipse(in: disc(radius + rect.height * 0.07))
        context.setBlendMode(.normal)
        context.setFillColor(NSColor.black.cgColor)
        context.fillEllipse(in: disc(radius))
        let arm = radius * 0.5, rise = radius * 0.55
        context.move(to: CGPoint(x: center.x, y: center.y - rise))
        context.addLine(to: CGPoint(x: center.x, y: center.y + rise))
        context.move(to: CGPoint(x: center.x - arm, y: center.y + rise - arm))
        context.addLine(to: CGPoint(x: center.x, y: center.y + rise))
        context.addLine(to: CGPoint(x: center.x + arm, y: center.y + rise - arm))
        context.setBlendMode(.clear)
        context.setLineWidth(rect.height * 0.08)
        context.setLineCap(.round)
        context.setLineJoin(.round)
        context.strokePath()
        return true
    }
    image.isTemplate = true
    return image
}

let awakeCollapsedKey = "keepAwakeCollapsed"

final class AppDelegate: NSObject, NSApplicationDelegate, NSMenuDelegate, UNUserNotificationCenterDelegate {
    #if SWITCHER_LAYOUT_TEST
    let statusItem: NSStatusItem? = nil
    #else
    let statusItem: NSStatusItem? = NSStatusBar.system.statusItem(withLength: NSStatusItem.variableLength)
    #endif
    let menu = NSMenu()
    let runtime: MenuRuntime
    var serverProcess: OwnedServer?
    var cachedState: AppState? { didSet { refreshStatusIcon() } }
    var hoverCards: [HoverCard] = []
    var hoverTimer: Timer?
    var menuOpen = false
    private var stateRequest = 0
    private var providerItems: [String: NSMenuItem] = [:]
    private var providerHovers: [String: HoverCard] = [:]
    private var countdownLabels: [(NSTextField, Double)] = []
    private var nextExhaustionChange = Date.distantFuture
    private var claudeSyncPhase = "idle"
    private var claudeSyncDetail = ""
    private var claudeSyncGeneration = 0
    private var claudeSyncConfirming = false
    private var switchingAccountID: String?
    private var nativeSwitchMessage = ""
    private var resetAlerts: [ResetAlert] = []
    private var deliveredResetIDs = Set<String>()
    private var sendingResetIDs = Set<String>()
    private var permissionRequested = false
    private var notificationPermission: ResetAlertPermission?
    private var savedResetLedger: ResetAlertLedger?
    private var serverStarting = false
    private var quitting = false
    private var recoveryGeneration = 0
    private var recoveryAttempts = 0
    private var stableRecoveryGeneration: Int?
    private var usageRefreshRunning = false
    private var refreshButton: SpinnerButton?
    private var updateActionRunning = false
    private var updateGeneration = 0
    private(set) var installProgress: InstallProgress?   // set while an install runs
    private var installRelease: String?
    private(set) var installProgressView: UpdateProgressView?
    private var menuActionMessage = ""     // stays until the next action
    private var menuNote = ""              // a success note, gone when the menu closes
    private(set) var updateCheckNote: String?  // shown in the update button until the menu closes
    private var awakeBusy = false
    private var awakeChoice: String?        // the chip that started the running session
    private var awakeShowingTimes = false   // "Until…" shows clock times instead of durations
    private var awakeOptionsOpen = false
    private(set) var awakeCollapsed = false
    private var awakeStatusLabel: NSTextField?
    private var awakeSummaryLabel: NSTextField?
    private var relaunching = false
    private var terminationSignal: DispatchSourceSignal?
    private var statusMark: NSImage?
    private(set) var statusBadge: String?   // the release the menu bar badge announces

    init(runtime: MenuRuntime = MenuRuntime()) {
        self.runtime = runtime
        super.init()
        awakeCollapsed = runtime.loadFlag(awakeCollapsedKey)
    }

    var resetAlertLedger: ResetAlertLedger {
        ResetAlertLedger(alerts: resetAlerts, deliveredIDs: deliveredResetIDs.sorted())
    }

    func restoreResetLedger(_ ledger: ResetAlertLedger) {
        resetAlerts = ledger.alerts
        deliveredResetIDs = Set(ledger.deliveredIDs)
        savedResetLedger = ledger
    }

    private func persistResetAlerts() {
        let ledger = ResetAlertLedger(alerts: resetAlerts, deliveredIDs: deliveredResetIDs.sorted())
        guard ledger != savedResetLedger else { return }
        do {
            try runtime.saveLedger(ledger)
            savedResetLedger = ledger
        } catch {
            NSLog("Switcher could not save reset alerts: %@", error.localizedDescription)
        }
    }

    private func clearResetAlerts() {
        if !resetAlerts.isEmpty || !deliveredResetIDs.isEmpty || !sendingResetIDs.isEmpty {
            resetAlerts = []
            deliveredResetIDs.removeAll()
            sendingResetIDs.removeAll()
            persistResetAlerts()
        }
    }

    func userNotificationCenter(_ center: UNUserNotificationCenter, willPresent notification: UNNotification,
                                withCompletionHandler completionHandler: @escaping (UNNotificationPresentationOptions) -> Void) {
        completionHandler([.banner, .sound])
    }

    func applicationDidFinishLaunching(_ notification: Notification) {
        #if !SWITCHER_LAYOUT_TEST
        UNUserNotificationCenter.current().delegate = self
        // The updater stops the app with SIGTERM before opening the new one.
        // Quit properly, so the owned server stops and restores sleep.
        signal(SIGTERM, SIG_IGN)
        let termination = DispatchSource.makeSignalSource(signal: SIGTERM, queue: .main)
        termination.setEventHandler { [weak self] in self?.quit() }
        termination.resume()
        terminationSignal = termination
        ensureServerRunning()
        do {
            restoreResetLedger(try runtime.loadLedger())
        } catch {
            NSLog("Switcher could not load reset alerts: %@", error.localizedDescription)
        }
        // The Switcher mark as a template image, so macOS tints it for the
        // menu bar's appearance.
        if let mark = Bundle.main.image(forResource: "MenubarTemplate") {
            mark.isTemplate = true
            mark.size = NSSize(width: 18, height: 18)
            statusMark = mark
            statusItem?.button?.image = mark
        } else {
            statusItem?.button?.title = "⇄"
        }
        statusItem?.button?.toolTip = "Switcher"
        refreshStatusIcon()
        menu.autoenablesItems = false
        statusItem?.menu = menu
        menu.delegate = self
        rebuildMenu()
        fetchStateAsync()
        // Keep the cache warm so the menu opens instantly with recent data.
        let warm = Timer(timeInterval: 60, repeats: true) { [weak self] _ in self?.fetchStateAsync() }
        RunLoop.main.add(warm, forMode: .common)
        let alerts = Timer(timeInterval: 15, repeats: true) { [weak self] _ in self?.deliverDueResetAlerts() }
        RunLoop.main.add(alerts, forMode: .common)
        #endif
    }

    // The menu bar mark carries a badge while a newer release waits.
    func refreshStatusIcon() {
        let update = cachedState?.update
        let badge = update?.update_available == true ? (update?.latest ?? "") : nil
        guard badge != statusBadge else { return }
        statusBadge = badge
        guard let button = statusItem?.button else { return }
        button.toolTip = badge.map { $0.isEmpty ? "Switcher · Update available" : "Switcher · \($0) available" } ?? "Switcher"
        guard let mark = statusMark else { return }
        let image = badge == nil ? mark : badgedStatusImage(mark)
        image.accessibilityDescription = badge == nil ? "Switcher" : "Switcher, update available"
        button.image = image
    }

    // Opening draws from the cache immediately (no network on the main
    // thread), then refreshes in the background and patches the menu only if
    // the data changed. A poll drives hover while open.
    func menuWillOpen(_ menu: NSMenu) {
        menuOpen = true
        prepareMenuForOpening()
        fetchStateAsync()
        hoverTimer?.invalidate()
        let timer = Timer(timeInterval: 0.05, repeats: true) { [weak self] _ in self?.pollHover() }
        RunLoop.main.add(timer, forMode: .common)
        hoverTimer = timer
    }

    // The menu is built ahead of the click. Only time-sensitive labels need
    // attention here; expensive image rendering stays off the opening path.
    func prepareMenuForOpening(now: Date = Date()) {
        if menu.items.isEmpty || palette.dark != Palette.current().dark || now >= nextExhaustionChange {
            rebuildMenu()
            return
        }
        for (label, reset) in countdownLabels {
            guard let remaining = resetRemaining(reset) else { label.isHidden = true; continue }
            let text = "↻ " + remaining
            if textWidth(text, font: label.font!) > label.frame.width {
                rebuildMenu()
                return
            }
            label.isHidden = false
            label.stringValue = text
            label.toolTip = exactResetTime(reset)
        }
        refreshAwakeStatus()
    }

    // Compare only the data the menu actually decodes. Health timestamps and
    // other web-only API fields cannot force a menu rebuild.
    func applyMenuState(_ state: AppState) {
        let previous = cachedState
        cachedState = state
        if menuActionMessage == "Switcher server stopped" || menuActionMessage == "Could not start Switcher server" {
            menuActionMessage = ""
        }
        if Date() >= nextExhaustionChange { rebuildMenu(); return }
        guard previous != state else { return }
        guard let previous = previous, previous.order == state.order, previous.hidden == state.hidden,
              previous.version == state.version, previous.update == state.update,
              previous.menu_usage_bars == state.menu_usage_bars,
              previous.reset_notifications == state.reset_notifications,
              previous.awake == state.awake,
              palette.dark == Palette.current().dark else { rebuildMenu(); return }
        let oldProviders = Set(previous.accounts.map(\.provider))
        let newProviders = Set(state.accounts.map(\.provider))
        guard oldProviders == newProviders else { rebuildMenu(); return }
        for providerID in providerItems.keys {
            let accounts = state.accounts.filter { $0.provider == providerID }
            if accounts == previous.accounts.filter({ $0.provider == providerID }) { continue }
            let card = providerCard(providerID: providerID, accounts: accounts,
                contentWidth: menuWidth - 2 * edgeInset, showUsageBars: state.menu_usage_bars ?? true)
            providerItems[providerID]?.view = centered(card)
            providerHovers[providerID] = card as? HoverCard
        }
        hoverCards = Array(providerHovers.values)
        // Discard references to labels in replaced provider cards.
        countdownLabels.removeAll { label, _ in
            !providerHovers.values.contains { label.isDescendant(of: $0) }
        }
        updateExhaustionDeadline()
    }

    private func updateExhaustionDeadline() {
        let now = Date()
        nextExhaustionChange = (cachedState?.accounts ?? []).compactMap { $0.exhausted_until }
            .map { Date(timeIntervalSince1970: $0) }.filter { $0 > now }.min() ?? .distantFuture
    }

    func applyNotificationPermission(_ permission: ResetAlertPermission) {
        guard notificationPermission != permission else { return }
        notificationPermission = permission
        // The prepared menu must be ready on the next click, even when the
        // permission result arrived while it was closed.
        rebuildMenu()
    }

    func menuDidClose(_ menu: NSMenu) {
        menuOpen = false
        hoverTimer?.invalidate()
        hoverTimer = nil
        hoverCards.forEach { $0.clearHover() }
        // The menu opens on durations again, not on a half-made time choice,
        // and without the notes of the last visit.
        if awakeShowingTimes || !menuNote.isEmpty || updateCheckNote != nil {
            awakeShowingTimes = false
            menuNote = ""
            updateCheckNote = nil
            rebuildMenu()
        }
    }

    func pollHover() {
        let point = NSEvent.mouseLocation
        hoverCards.forEach { $0.updateHover(screenPoint: point) }
        refreshAwakeStatus()
    }

    // The keep-awake countdown changes without new server data.
    private func refreshAwakeStatus() {
        guard let awake = cachedState?.awake else { return }
        if let label = awakeStatusLabel {
            let text = awakeStatusText(awake)
            if label.stringValue != text { label.stringValue = text }
        }
        if let label = awakeSummaryLabel {
            let text = awakeSummary(awake).text
            if label.stringValue != text { label.stringValue = text }
        }
    }

    // fetchStateAsync refreshes the cache off the main thread. The menu is
    // rebuilt only when the JSON differs from what is currently shown.
    func beginStateRequest() -> Int {
        stateRequest += 1
        return stateRequest
    }

    @discardableResult
    func acceptStateResponse(_ state: AppState, serial: Int) -> Bool {
        guard serial == stateRequest, !quitting else { return false }
        applyMenuState(state)
        if serverProcess != nil, recoveryAttempts > 0, stableRecoveryGeneration != recoveryGeneration {
            let generation = recoveryGeneration
            stableRecoveryGeneration = generation
            runtime.schedule(60) { [weak self] in
                guard let self = self, !self.quitting, self.serverProcess != nil,
                      self.cachedState != nil, self.recoveryGeneration == generation else { return }
                self.recoveryAttempts = 0
            }
        }
        return true
    }

    private func performRequest(_ request: URLRequest, completion: @escaping (Data?, URLResponse?, Error?) -> Void) {
        runtime.request(request) { data, response, error in
            onMain { completion(data, response, error) }
        }
    }

    private func makeRequest(path: String, method: String = "GET", body: [String: Any]? = nil,
                             timeout: TimeInterval = 5) -> URLRequest {
        var request = URLRequest(url: hubURL.appendingPathComponent(path))
        request.httpMethod = method
        request.timeoutInterval = timeout
        if let body = body, let data = try? JSONSerialization.data(withJSONObject: body) {
            request.httpBody = data
            request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        }
        if let token = runtime.deviceToken() { request.setValue("Bearer " + token, forHTTPHeaderField: "Authorization") }
        return request
    }

    func fetchStateAsync() {
        guard !quitting else { return }
        let serial = beginStateRequest()
        let request = makeRequest(path: "api/state")
        performRequest(request) { [weak self] data, response, _ in
            guard let self = self, !self.quitting, serial == self.stateRequest else { return }
            if (response as? HTTPURLResponse)?.statusCode == 401,
               request.value(forHTTPHeaderField: "Authorization") != self.runtime.deviceToken().map({ "Bearer " + $0 }) {
                self.fetchStateAsync()
                return
            }
            guard let data = data,
                  (response as? HTTPURLResponse)?.statusCode == 200,
                  let decoded = try? JSONDecoder().decode(AppState.self, from: data) else {
                self.cachedState = nil
                self.rebuildMenu()
                return
            }
            guard self.acceptStateResponse(decoded, serial: serial) else { return }
            if self.menuOpen { self.prepareMenuForOpening() }
            self.updateResetAlerts()
            self.relaunchIfReplaced()
        }
    }

    // Persist upcoming and recently due windows. Nothing is pre-queued in
    // macOS, so disabling alerts still takes effect before delivery.
    func updateResetAlerts() {
        guard let state = cachedState else { return }
        if state.reset_notifications != true {
            clearResetAlerts()
            return
        }
        let now = Date()
        resetAlerts = reconciledResetAlerts(resetAlerts, state: state, now: now)
        deliveredResetIDs.formIntersection(Set(resetAlerts.map(\.id)))
        persistResetAlerts()
        #if !SWITCHER_LAYOUT_TEST
        let center = UNUserNotificationCenter.current()
        center.getNotificationSettings { [weak self] settings in
            DispatchQueue.main.async {
                guard let self = self else { return }
                let permission = resetAlertPermission(settings)
                self.applyNotificationPermission(permission)
                guard settings.authorizationStatus == .notDetermined,
                      self.cachedState?.reset_notifications == true, !self.permissionRequested else { return }
                self.permissionRequested = true
                center.requestAuthorization(options: [.alert, .sound]) { [weak self] _, _ in
                    center.getNotificationSettings { result in
                        DispatchQueue.main.async {
                            self?.applyNotificationPermission(resetAlertPermission(result))
                        }
                    }
                }
            }
        }
        #endif
    }

    func deliverDueResetAlerts() {
        guard let cachedState = cachedState else { return }
        guard cachedState.reset_notifications == true, runtime.alertsEnabled() else {
            clearResetAlerts()
            return
        }
        // The state poll refreshes this permission every minute. Do not
        // hammer the server for a due alert while macOS blocks banners.
        guard notificationPermission?.canSend == true else { return }
        let now = Date()
        let due = dueResetAlerts(resetAlerts, now: now, delivered: deliveredResetIDs, sending: sendingResetIDs)
        guard !due.isEmpty else { return }
        let serial = beginStateRequest()
        // Read the server's current preference immediately before delivery.
        // If it is unavailable, skip the alert rather than risk showing one
        // after the user turned the setting off in the web app.
        let request = makeRequest(path: "api/state")
        performRequest(request) { [weak self] data, response, _ in
            guard let data = data, (response as? HTTPURLResponse)?.statusCode == 200,
                  let state = try? JSONDecoder().decode(AppState.self, from: data) else { return }
            guard let self = self, !self.quitting else { return }
            guard self.acceptStateResponse(state, serial: serial) else { return }
            self.updateResetAlerts()
            guard state.reset_notifications == true, self.runtime.alertsEnabled() else { return }
            let valid = Set(self.resetAlerts.map(\.id))
            let ready = due.filter { valid.contains($0.id) && !self.deliveredResetIDs.contains($0.id) && !self.sendingResetIDs.contains($0.id) }
            guard !ready.isEmpty else { return }
            for alert in ready { self.sendingResetIDs.insert(alert.id) }
            self.presentResetAlerts(ready)
        }
    }

    private func presentResetAlerts(_ due: [ResetAlert]) {
        #if SWITCHER_LAYOUT_TEST
        for alert in due { sendingResetIDs.remove(alert.id) }
        #else
        UNUserNotificationCenter.current().getNotificationSettings { [weak self] settings in
            DispatchQueue.main.async {
                guard let self = self else { return }
                guard resetAlertPermission(settings).canSend,
                      self.cachedState?.reset_notifications == true, self.runtime.alertsEnabled() else {
                    for alert in due { self.sendingResetIDs.remove(alert.id) }
                    return
                }
                for alert in due {
                    let content = UNMutableNotificationContent()
                    content.title = "\(alert.provider) usage window"
                    content.body = "Provider-reported reset time reached for \(alert.window)."
                    content.sound = .default
                    guard self.runtime.alertsEnabled() else { self.sendingResetIDs.remove(alert.id); continue }
                    UNUserNotificationCenter.current().add(UNNotificationRequest(identifier: alert.id,
                        content: content, trigger: nil)) { [weak self] error in
                        DispatchQueue.main.async {
                            guard let self = self else { return }
                            self.sendingResetIDs.remove(alert.id)
                            if let error = error {
                                NSLog("Switcher reset alert delivery failed: %@", error.localizedDescription)
                            } else {
                                recordDeliveredResetAlert(alert.id, alerts: &self.resetAlerts,
                                    delivered: &self.deliveredResetIDs)
                                self.persistResetAlerts()
                            }
                        }
                    }
                }
            }
        }
        #endif
    }

    // An HTTP response, including 401, means something already owns the port.
    // Recovery never terminates or adopts that external listener.
    func ensureServerRunning(recovering: Bool = false) {
        guard !quitting, !serverStarting, serverProcess == nil else { return }
        serverStarting = true
        let generation = recoveryGeneration
        performRequest(makeRequest(path: "api/state", timeout: 1.5)) { [weak self] _, response, _ in
            guard let self = self, !self.quitting, generation == self.recoveryGeneration else { return }
            self.serverStarting = false
            if response is HTTPURLResponse { return }
            do {
                self.serverProcess = try self.runtime.launchServer { [weak self] id in
                    onMain { self?.ownedServerExited(id) }
                }
                let id = self.serverProcess?.id
                self.runtime.schedule(0.8) { [weak self] in
                    guard let self = self, !self.quitting, self.serverProcess?.id == id else { return }
                    self.fetchStateAsync()
                }
            } catch {
                self.menuActionMessage = "Could not start Switcher server"
                self.rebuildMenu()
                NSLog("Switcher could not start server: %@", error.localizedDescription)
                if recovering { self.scheduleOwnedRecovery() }
            }
        }
    }

    private func ownedServerExited(_ id: UUID) {
        guard !quitting, serverProcess?.id == id else { return }
        serverProcess = nil
        recoveryGeneration += 1
        stableRecoveryGeneration = nil
        _ = beginStateRequest()
        cachedState = nil
        menuActionMessage = "Switcher server stopped"
        rebuildMenu()
        scheduleOwnedRecovery()
    }

    private func scheduleOwnedRecovery() {
        guard !quitting, recoveryAttempts < 3 else { return }
        let delay = pow(2, Double(recoveryAttempts))
        recoveryAttempts += 1
        let generation = recoveryGeneration
        runtime.schedule(delay) { [weak self] in
            guard let self = self, !self.quitting, self.recoveryGeneration == generation else { return }
            self.ensureServerRunning(recovering: true)
        }
    }

    func post(path: String, method: String = "POST", body: [String: Any]? = nil, timeout: TimeInterval = 90,
              completion: @escaping (Result<Data, Error>) -> Void) {
        func attempt(retry: Bool) {
            let request = makeRequest(path: path, method: method, body: body, timeout: timeout)
            performRequest(request) { [weak self] data, response, error in
                guard let self = self, !self.quitting else { return }
                if retry, (response as? HTTPURLResponse)?.statusCode == 401,
                   request.value(forHTTPHeaderField: "Authorization") != self.runtime.deviceToken().map({ "Bearer " + $0 }) {
                    attempt(retry: false)
                    return
                }
                completion(menuHTTPResult(data, response: response, error: error))
            }
        }
        attempt(retry: true)
    }

    // MARK: menu construction

    func rebuildMenu() {
        menu.removeAllItems()
        providerItems.removeAll()
        providerHovers.removeAll()
        countdownLabels.removeAll()
        updateExhaustionDeadline()
        let contentWidth = menuWidth - 2 * edgeInset

        // Header: app icon with name and version beside it, block centered.
        palette = Palette.current()
        let state = cachedState
        hoverCards = []
        let headerView = NSView(frame: NSRect(x: 0, y: 0, width: menuWidth, height: 58))
        let nameFont = NSFont.systemFont(ofSize: 14, weight: .semibold)
        let versionFont = NSFont.systemFont(ofSize: 11)
        let versionText = "v" + (state?.version ?? "0.0.0")
        let textColumn = max(textWidth("Switcher", font: nameFont), textWidth(versionText, font: versionFont))
        let blockWidth = 36 + 10 + textColumn
        let blockX = (menuWidth - blockWidth) / 2
        let logo = NSImageView(frame: NSRect(x: blockX, y: 11, width: 36, height: 36))
        logo.image = Bundle.main.image(forResource: "AppIcon") ?? NSApp.applicationIconImage
        headerView.addSubview(logo)
        let nameField = label("Switcher", font: nameFont, color: inkColor)
        nameField.frame = NSRect(x: blockX + 46, y: 30, width: textColumn, height: 17)
        headerView.addSubview(nameField)
        let versionField = label(versionText, font: versionFont, color: dimColor)
        versionField.frame = NSRect(x: blockX + 46, y: 14, width: textColumn, height: 14)
        headerView.addSubview(versionField)
        // Manual usage refresh, trailing edge of the header.
        let refresh = SpinnerButton(symbol: "arrow.clockwise", size: 26, color: dimColor)
        refresh.frame.origin = NSPoint(x: menuWidth - edgeInset - 26, y: 16)
        refresh.onClicked = { [weak self] in self?.refreshUsage() }
        refresh.setBusy(usageRefreshRunning)
        refreshButton = refresh
        headerView.addSubview(refresh)
        menu.addItem(menuItemWithView(headerView))

        if state == nil {
            let row = NSView(frame: NSRect(x: 0, y: 0, width: menuWidth, height: 36))
            let field = label("Server unreachable", font: NSFont.systemFont(ofSize: 12.5), color: dimColor)
            field.frame = NSRect(x: edgeInset, y: 10, width: contentWidth, height: 16)
            row.addSubview(field)
            menu.addItem(menuItemWithView(row))
        } else {
            let visible = (state?.order ?? ["codex", "claude", "grok", "opencode"])
                .filter { !(state?.hidden ?? []).contains($0) }
                .filter { providerID in
                    !(state?.accounts ?? []).filter { $0.provider == providerID }.isEmpty
                }
            for (position, providerID) in visible.enumerated() {
                if position > 0 {
                    menu.addItem(menuItemWithView(spacerView(height: 12)))
                }
                let accounts = (state?.accounts ?? []).filter { $0.provider == providerID }
                menu.addItem(menuItemWithView(sectionHead(providerID, contentWidth: contentWidth,
                    syncPhase: claudeSyncPhase, onSync: { [weak self] in self?.syncClaudeSessions() })))
                if providerID == "claude", !nativeSwitchMessage.isEmpty {
                    let row = NSView(frame: NSRect(x: 0, y: 0, width: menuWidth, height: 40))
                    let note = label(nativeSwitchMessage, font: NSFont.systemFont(ofSize: 11), color: dimColor)
                    note.maximumNumberOfLines = 2
                    note.frame = NSRect(x: edgeInset, y: 5, width: contentWidth, height: 32)
                    note.toolTip = nativeSwitchMessage
                    row.addSubview(note)
                    menu.addItem(menuItemWithView(row))
                }
                if providerID == "claude" && (claudeSyncPhase == "success" || claudeSyncPhase == "error") {
                    let feedback = NSView(frame: NSRect(x: 0, y: 0, width: menuWidth, height: 48))
                    if claudeSyncPhase == "error" {
                        let details = NSButton(title: "Sync failed · Details", target: self, action: #selector(showClaudeSyncError))
                        details.isBordered = false
                        details.contentTintColor = .systemRed
                        details.frame = NSRect(x: edgeInset, y: 10, width: contentWidth, height: 26)
                        feedback.addSubview(details)
                    } else {
                        let text = label("Claude sessions synced\n" + claudeSyncDetail,
                            font: NSFont.systemFont(ofSize: 11), color: dimColor)
                        text.maximumNumberOfLines = 2
                        text.frame = NSRect(x: edgeInset, y: 5, width: contentWidth, height: 38)
                        text.toolTip = claudeSyncDetail
                        feedback.addSubview(text)
                    }
                    menu.addItem(menuItemWithView(feedback))
                }
                let card = providerCard(providerID: providerID, accounts: accounts,
                    contentWidth: contentWidth, showUsageBars: state?.menu_usage_bars ?? true)
                let item = menuItemWithView(centered(card))
                providerItems[providerID] = item
                providerHovers[providerID] = card as? HoverCard
                menu.addItem(item)
            }
        }

        awakeStatusLabel = nil
        awakeSummaryLabel = nil
        if let awake = state?.awake, awake.supported {
            menu.addItem(menuItemWithView(spacerView(height: 12)))
            menu.addItem(menuItemWithView(awakeHead(awake, contentWidth: contentWidth)))
            if !awakeCollapsed {
                menu.addItem(menuItemWithView(centered(awakeCard(awake, contentWidth: contentWidth))))
            }
        }

        menu.addItem(.separator())

        // Bottom actions: equal-width buttons, text vertically centered.
        let installAvailable = state?.update?.update_available == true
        let updateTitle = installProgress != nil ? "Installing…" : updateActionRunning ? "Working…"
            : installAvailable ? "Install update" : updateCheckNote ?? "Check for updates"
        let buttonWidth = (menuWidth - 2 * edgeInset - 10) / 2
        let actionRow = NSView(frame: NSRect(x: 0, y: 0, width: menuWidth, height: 46))
        let webCard = cardView(width: buttonWidth, height: 38)
        webCard.frame.origin = NSPoint(x: edgeInset, y: 4)
        let webButton = NSButton(title: "Open web app", target: self, action: #selector(openWebApp))
        webButton.isBordered = false
        webButton.font = NSFont.systemFont(ofSize: 13, weight: .medium)
        webButton.frame = NSRect(x: 0, y: 9, width: buttonWidth, height: 20)
        webButton.alignment = .center
        webButton.attributedTitle = NSAttributedString(string: webButton.title, attributes: [
            .font: NSFont.systemFont(ofSize: 13, weight: .medium), .foregroundColor: inkColor,
            .paragraphStyle: centeredParagraph])
        webCard.addSubview(webButton)
        actionRow.addSubview(webCard)
        let updateCard = cardView(width: buttonWidth, height: 38)
        updateCard.frame.origin = NSPoint(x: edgeInset + buttonWidth + 10, y: 4)
        let updateButton = NSButton(title: updateTitle,
            target: self, action: #selector(runUpdate))
        updateButton.isEnabled = !updateActionRunning
        updateButton.isBordered = false
        updateButton.font = NSFont.systemFont(ofSize: 13, weight: .medium)
        updateButton.frame = NSRect(x: 0, y: 9, width: buttonWidth, height: 20)
        updateButton.alignment = .center
        updateButton.attributedTitle = NSAttributedString(string: updateButton.title, attributes: [
            .font: NSFont.systemFont(ofSize: 13, weight: .medium),
            .foregroundColor: updateTitle == updateCheckNote ? accentColor : inkColor,
            .paragraphStyle: centeredParagraph])
        updateCard.addSubview(updateButton)
        actionRow.addSubview(updateCard)
        menu.addItem(menuItemWithView(actionRow))
        installProgressView = nil
        if let progress = installProgress {
            let card = UpdateProgressView(width: contentWidth, release: installRelease, progress: progress)
            installProgressView = card
            menu.addItem(menuItemWithView(centered(card, verticalPadding: 4)))
        } else if !(menuActionMessage.isEmpty ? menuNote : menuActionMessage).isEmpty {
            let message = menuActionMessage.isEmpty ? menuNote : menuActionMessage
            let feedback = NSView(frame: NSRect(x: 0, y: 0, width: menuWidth, height: 26))
            let text = label(message, font: NSFont.systemFont(ofSize: 11), color: dimColor)
            text.frame = NSRect(x: edgeInset, y: 5, width: contentWidth, height: 16)
            text.toolTip = message
            feedback.addSubview(text)
            menu.addItem(menuItemWithView(feedback))
        }

        menu.addItem(menuItemWithView(centered(settingsCard(contentWidth: contentWidth,
            alerts: state?.reset_notifications == true), verticalPadding: 4)))

        let quitItem = NSMenuItem(title: "Quit Switcher", action: #selector(quit), keyEquivalent: "q")
        quitItem.target = self
        menu.addItem(quitItem)
    }

    // Row metrics: top pad, title, gap, usage lines, bottom pad.
    let rowTopPad: CGFloat = 9
    let titleHeight: CGFloat = 15
    let titleUsageGap: CGFloat = 4
    let usageLineHeight: CGFloat = 15
    let rowBottomPad: CGFloat = 8

    func providerCard(providerID: String, accounts: [Account], contentWidth: CGFloat,
                      showUsageBars: Bool) -> NSView {
        let height = accounts.reduce(CGFloat(0)) { $0 + rowHeight(for: $1) }
        let card = cardView(width: contentWidth, height: height)
        if let hover = card as? HoverCard { hoverCards.append(hover) }
        // Column widths shared by every row in this card (measured with the
        // medium weight, the wider of the two title weights).
        let measureFont = NSFont.systemFont(ofSize: 12.5, weight: .medium)
        let planColumn = accounts
            .compactMap { $0.plan.flatMap { planNames[$0] } }
            .map { textWidth("· " + $0, font: measureFont) }.max() ?? 0
        let emailColumn = accounts.map { textWidth($0.email, font: measureFont) }.max() ?? 0
        var y = height
        for (index, account) in accounts.enumerated() {
            let rowH = rowHeight(for: account)
            y -= rowH

            let row = ClickableRow(frame: NSRect(x: 0, y: y, width: contentWidth, height: rowH))
            row.onClicked = { [weak self] in
                self?.activateAccount(account)
            }

            // Email + plan. Plans sit in one column per card: the email
            // column is as wide as the widest email that still leaves room
            // for the widest plan; longer emails truncate with an ellipsis.
            let textX = cardInset
            let planText = account.plan.flatMap { planNames[$0] }.map { "· " + $0 } ?? ""
            let bankedText = bankedResetText(account.reset_credits) ?? ""
            let bankedFont = NSFont.systemFont(ofSize: 10.5)
            let bankedWidth = bankedText.isEmpty ? CGFloat(0) : textWidth(bankedText, font: bankedFont)
            let exhausted = !account.selected && (account.exhausted_until.map { $0 > Date().timeIntervalSince1970 } ?? false)
            // Cached bars stay on screen when checks fail, so say when they
            // are old or the account can no longer refresh at all.
            let badge: (text: String, color: NSColor, tip: String)?
            switch account.health?.condition {
            case "needs_relogin":
                badge = ("Sign in again", warnColor, "This account's login no longer refreshes; use Relogin in Switcher")
            case _ where exhausted:
                badge = ("Out of usage", warnColor, "")
            case "usage_stale" where account.usage?.windows?.isEmpty == false,
                 "usage_unavailable" where account.usage?.windows?.isEmpty == false:
                badge = ("Usage stale", dimColor, "Usage could not be checked recently; these numbers may be old")
            default:
                badge = nil
            }
            let titleFont = NSFont.systemFont(ofSize: 12.5, weight: account.selected ? .medium : .regular)
            let titleColor = account.selected ? accentColor : inkColor
            let trailingWidth = (badge != nil ? 90 + (account.selected ? 22 : 0) : (account.selected ? 14 : 0)) +
                (bankedWidth > 0 ? bankedWidth + 8 : 0)
            let titleWidth = contentWidth - textX - cardInset - trailingWidth - 8
            let titleY = rowH - rowTopPad - titleHeight
            let emailWidth = planColumn > 0 ? max(0, min(emailColumn, titleWidth - planColumn - 6)) : max(0, min(emailColumn, titleWidth))
            let emailField = label(account.email, font: titleFont, color: titleColor)
            emailField.frame = NSRect(x: textX, y: titleY, width: emailWidth, height: titleHeight)
            let email = BlurredLabel(field: emailField)
            row.addSubview(email)
            row.onHover = { hovering in email.reveal(hovering) }
            if !planText.isEmpty {
                let plan = label(planText, font: titleFont, color: titleColor)
                plan.frame = NSRect(x: textX + emailWidth + 6, y: titleY, width: planColumn, height: titleHeight)
                row.addSubview(plan)
            }
            if bankedWidth > 0 {
                let banked = label(bankedText, font: bankedFont, color: warnColor)
                banked.alignment = .right
                banked.toolTip = "Banked usage-limit resets available"
                banked.frame = NSRect(x: contentWidth - cardInset - (account.selected ? 22 : 0) - bankedWidth,
                                      y: titleY, width: bankedWidth, height: titleHeight)
                row.addSubview(banked)
            }

            let nativeSelectionBlock: CGFloat = account.native_switch_available == true ? titleUsageGap + usageLineHeight : 0
            if nativeSelectionBlock > 0 {
                let selection = label("Claude Code \(account.native_active == true ? "active" : "idle") · Proxy \(account.active ? "active" : "idle")",
                    font: NSFont.systemFont(ofSize: 10.5), color: dimColor)
                selection.frame = NSRect(x: textX, y: titleY - titleUsageGap - usageLineHeight,
                    width: contentWidth - textX - cardInset, height: usageLineHeight)
                row.addSubview(selection)
            }

            // Stable label and percentage columns, with resets trailing.
            let usageFont = NSFont.systemFont(ofSize: 11)
            let valueFont = NSFont.monospacedDigitSystemFont(ofSize: 11, weight: .regular)
            let resetFont = NSFont.systemFont(ofSize: 10.5)
            let windows = account.usage?.windows ?? []
            let layout = usageLayout(windows: windows, titleY: titleY,
                lineWidth: contentWidth - textX - cardInset, contentWidth: contentWidth, textX: textX,
                cardInset: cardInset, lineHeight: usageLineHeight, titleGap: titleUsageGap + nativeSelectionBlock,
                showBars: showUsageBars)
            for (window, line) in zip(windows, layout.lines) {
                let left = max(0, min(100, 100 - window.used_percent))
                if let barFrame = line.bar {
                    let track = NSView(frame: barFrame)
                    track.wantsLayer = true
                    track.layer?.backgroundColor = dimColor.withAlphaComponent(0.24).cgColor
                    track.layer?.cornerRadius = 1.5
                    track.layer?.masksToBounds = true
                    if left > 0 {
                        let fill = NSView(frame: NSRect(x: 0, y: 0,
                            width: barFrame.width * CGFloat(left) / 100, height: barFrame.height))
                        fill.wantsLayer = true
                        fill.layer?.backgroundColor = accentColor.cgColor
                        fill.layer?.cornerRadius = 1.5
                        track.addSubview(fill)
                    }
                    row.addSubview(track)
                }
                let name = label(window.label + ":", font: usageFont, color: dimColor)
                name.frame = line.label
                row.addSubview(name)
                let value = label("\(left)% left", font: valueFont, color: dimColor)
                value.frame = line.value
                row.addSubview(value)
                if let reset = line.resetText, let frame = line.reset {
                    let resetLabel = label(reset, font: resetFont, color: dimColor)
                    resetLabel.toolTip = exactResetTime(window.resets_at)
                    if let timestamp = window.resets_at { countdownLabels.append((resetLabel, timestamp)) }
                    resetLabel.alignment = .right
                    resetLabel.frame = frame
                    row.addSubview(resetLabel)
                }
            }

            if account.selected {
                let check = NSImageView(frame: layout.checkmark)
                check.image = NSImage(systemSymbolName: "checkmark", accessibilityDescription: nil)
                check.contentTintColor = accentColor
                check.toolTip = account.native_switch_available == true ? "Claude Code native login active" : "Proxy account active"
                row.addSubview(check)
            }
            if let badge {
                let note = label(badge.text, font: NSFont.systemFont(ofSize: 10.5), color: badge.color)
                note.alignment = .right
                if !badge.tip.isEmpty { note.toolTip = badge.tip }
                note.frame = NSRect(x: contentWidth - cardInset - 90 - (account.selected ? 22 : 0) - (bankedWidth > 0 ? bankedWidth + 8 : 0),
                                    y: titleY, width: 90, height: 14)
                row.addSubview(note)
            }

            if index < accounts.count - 1 {
                let divider = NSView(frame: NSRect(x: cardInset, y: y, width: contentWidth - 2 * cardInset, height: 1))
                divider.wantsLayer = true
                divider.layer?.backgroundColor = cardBorderColor.cgColor
                card.addSubview(divider)
            }
            card.addSubview(row)
        }
        return card
    }

    func rowHeight(for account: Account) -> CGFloat {
        let windows = CGFloat(account.usage?.windows?.count ?? 0)
        let usageBlock = windows > 0 ? titleUsageGap + windows * usageLineHeight : 0
        let selectionBlock = account.native_switch_available == true ? titleUsageGap + usageLineHeight : 0
        return rowTopPad + titleHeight + selectionBlock + usageBlock + rowBottomPad
    }

    // Start at login and, while they are on, reset alerts: one card with a
    // compact row each. The alerts row says in a few words what macOS
    // allows; the full sentence is its tooltip.
    func settingsCard(contentWidth: CGFloat, alerts: Bool) -> NSView {
        let row: CGFloat = 34, inner = contentWidth - 2 * cardInset
        let card = cardView(width: contentWidth, height: alerts ? 2 * row + 1 : row)
        let top = card.frame.height - row
        let login = label("Start at login", font: NSFont.systemFont(ofSize: 13), color: inkColor)
        login.frame = NSRect(x: cardInset, y: top + 9, width: inner - 48, height: 16)
        card.addSubview(login)
        let toggle = ToggleSwitch(isOn: startAtLoginEnabled())
        toggle.frame.origin = NSPoint(x: contentWidth - cardInset - 40, y: top + 5)
        toggle.onChanged = { [weak self] _ in self?.toggleStartAtLogin() }
        card.addSubview(toggle)
        guard alerts else { return card }

        let divider = NSView(frame: NSRect(x: cardInset, y: row, width: inner, height: 1))
        divider.wantsLayer = true
        divider.layer?.backgroundColor = cardBorderColor.cgColor
        card.addSubview(divider)
        let titleFont = NSFont.systemFont(ofSize: 13)
        let title = label("Reset alerts", font: titleFont, color: inkColor)
        let titleWidth = ceil(textWidth(title.stringValue, font: titleFont))
        title.frame = NSRect(x: cardInset, y: 9, width: titleWidth, height: 16)
        card.addSubview(title)
        let permission = notificationPermission
        let canSend = permission?.canSend == true
        let testFont = NSFont.systemFont(ofSize: 12.5, weight: .medium)
        let testWidth = ceil(textWidth("Send test", font: testFont)) + 8
        let test = NSButton(title: "Send test", target: self, action: #selector(testResetAlert))
        test.isBordered = false
        test.isEnabled = canSend
        test.attributedTitle = NSAttributedString(string: test.title, attributes: [
            .font: testFont, .foregroundColor: canSend ? accentColor : dimColor])
        test.frame = NSRect(x: contentWidth - cardInset - testWidth + 4, y: 8, width: testWidth, height: 18)
        test.setAccessibilityLabel("Send a test reset alert")
        card.addSubview(test)
        let statusText = permission.map { $0.short.isEmpty ? $0.message : $0.short } ?? "Checking permission"
        let status = label(statusText, font: NSFont.systemFont(ofSize: 11.5), color: canSend || permission == nil ? dimColor : warnColor)
        let statusX = title.frame.maxX + 8
        status.frame = NSRect(x: statusX, y: 8.5, width: test.frame.minX - 8 - statusX, height: 15)
        status.toolTip = permission?.message
        card.addSubview(status)
        return card
    }

    // MARK: keep awake

    // The section header opens and closes the section. Closed, it still
    // says whether the Mac is being kept awake.
    func awakeHead(_ state: AwakeState, contentWidth: CGFloat) -> NSView {
        let head = TapArea(frame: NSRect(x: 0, y: 0, width: menuWidth, height: 32))
        let icon = NSImageView(frame: NSRect(x: edgeInset, y: 5, width: 22, height: 22))
        icon.image = NSImage(systemSymbolName: "moon.zzz.fill", accessibilityDescription: nil)?
            .withSymbolConfiguration(NSImage.SymbolConfiguration(pointSize: 15, weight: .medium))
        icon.contentTintColor = inkColor
        head.addSubview(icon)
        let nameFont = NSFont.systemFont(ofSize: 13, weight: .semibold)
        let name = label("Keep awake", font: nameFont, color: inkColor)
        name.frame = NSRect(x: edgeInset + 22 + 10, y: 8, width: ceil(textWidth(name.stringValue, font: nameFont)), height: 16)
        head.addSubview(name)
        let chevron = NSImageView(frame: NSRect(x: menuWidth - edgeInset - 12, y: 10, width: 12, height: 12))
        chevron.image = NSImage(systemSymbolName: awakeCollapsed ? "chevron.right" : "chevron.down", accessibilityDescription: nil)?
            .withSymbolConfiguration(NSImage.SymbolConfiguration(pointSize: 10, weight: .semibold))
        chevron.contentTintColor = dimColor
        head.addSubview(chevron)
        if awakeCollapsed {
            let summary = awakeSummary(state)
            let attention = state.error.map { !$0.isEmpty } ?? false
            let field = label(summary.text, font: NSFont.systemFont(ofSize: 12, weight: .medium),
                color: attention ? warnColor : summary.on ? accentColor : dimColor)
            field.alignment = .right
            field.frame = NSRect(x: name.frame.maxX + 10, y: 8, width: chevron.frame.minX - 8 - name.frame.maxX - 10, height: 16)
            field.toolTip = attention ? state.error : awakeStatusText(state)
            head.addSubview(field)
            awakeSummaryLabel = field
        }
        head.toolTip = awakeCollapsed ? "Show keep awake" : "Hide keep awake"
        head.setAccessibilityRole(.disclosureTriangle)
        head.setAccessibilityLabel("Keep awake")
        head.setAccessibilityValue(awakeCollapsed ? 0 : 1)
        head.onClicked = { [weak self] in self?.toggleAwakeSection() }
        return head
    }

    func toggleAwakeSection() {
        awakeCollapsed.toggle()
        awakeShowingTimes = false
        runtime.saveFlag(awakeCollapsedKey, awakeCollapsed)
        rebuildMenu()
    }

    // The chip that matches the running session, if any.
    func selectedAwakeChip(_ state: AwakeState) -> String? {
        guard state.active else { return nil }
        if state.until == nil { return "∞" }
        return awakeChoice == "∞" ? nil : awakeChoice
    }

    func awakeCard(_ state: AwakeState, contentWidth: CGFloat) -> NSView {
        let inner = contentWidth - 2 * cardInset
        let chipGap: CGFloat = 8, chipHeight: CGFloat = 26
        let chipWidth = floor((inner - 3 * chipGap) / 4)
        let notice = awakeNotice(state)
        // Heights, top to bottom.
        let top: CGFloat = 12, status: CGFloat = 24, chips = 2 * chipHeight + chipGap, hint: CGFloat = 14,
            options: CGFloat = 22, option: CGFloat = 30, lid: CGFloat = 34, bottom: CGFloat = 12
        var height = top + status + 12 + chips + 8 + hint + 8 + options
        if awakeOptionsOpen { height += 2 * option }
        height += 10 + 1 + 10 + lid
        if notice != nil { height += 6 + 28 }
        height += bottom
        let card = cardView(width: contentWidth, height: height)
        var y = height - top

        // Status line and the main switch: on means until turned off.
        y -= status
        let statusField = label(awakeStatusText(state), font: NSFont.systemFont(ofSize: 13), color: inkColor)
        statusField.frame = NSRect(x: cardInset, y: y + 4, width: inner - 48, height: 17)
        card.addSubview(statusField)
        awakeStatusLabel = statusField
        let main = ToggleSwitch(isOn: state.active)
        main.frame.origin = NSPoint(x: contentWidth - cardInset - 40, y: y)
        main.onChanged = { [weak self] on in
            if on { self?.startAwake(minutes: 0, choice: "∞") } else { self?.stopAwake() }
        }
        card.addSubview(main)
        y -= 12

        // Two rows of four chips: durations, or clock times for "Until…".
        let selected = selectedAwakeChip(state)
        var chipSpecs: [(String, Bool, () -> Void)] = []
        if awakeShowingTimes {
            for date in awakeUntilChoices() {
                let title = awakeClock(date)
                chipSpecs.append((title, false, { [weak self] in
                    self?.awakeShowingTimes = false
                    self?.startAwake(until: date)
                }))
            }
            chipSpecs.append(("Back", false, { [weak self] in
                self?.awakeShowingTimes = false
                self?.rebuildMenu()
            }))
        } else {
            for preset in awakePresets {
                let isOn = selected == preset.label
                chipSpecs.append((preset.label, isOn, { [weak self] in
                    if isOn { self?.stopAwake() } else { self?.startAwake(minutes: preset.minutes, choice: preset.label) }
                }))
            }
            let untilOn = selected == "until"
            chipSpecs.append(("Until…", untilOn, { [weak self] in
                if untilOn { self?.stopAwake(); return }
                self?.awakeShowingTimes = true
                self?.rebuildMenu()
            }))
        }
        for (index, spec) in chipSpecs.enumerated() {
            let row = CGFloat(index / 4), column = CGFloat(index % 4)
            let frame = NSRect(x: cardInset + column * (chipWidth + chipGap),
                               y: y - chipHeight - row * (chipHeight + chipGap), width: chipWidth, height: chipHeight)
            let chip = ChipButton(title: spec.0, selected: spec.1, frame: frame)
            chip.onClicked = spec.2
            card.addSubview(chip)
        }
        y -= chips + 8

        y -= hint
        let hintField = label(awakeShowingTimes ? "Keep the Mac awake until this time" : "Click a chip to start. Click it again to stop",
            font: NSFont.systemFont(ofSize: 11), color: dimColor)
        hintField.frame = NSRect(x: cardInset, y: y, width: inner, height: hint)
        card.addSubview(hintField)
        y -= 8

        // Options disclosure.
        y -= options
        let disclosure = TapArea(frame: NSRect(x: cardInset - 4, y: y, width: inner + 8, height: options))
        let chevron = NSImageView(frame: NSRect(x: 4, y: 5, width: 12, height: 12))
        chevron.image = NSImage(systemSymbolName: awakeOptionsOpen ? "chevron.down" : "chevron.right", accessibilityDescription: nil)?
            .withSymbolConfiguration(NSImage.SymbolConfiguration(pointSize: 10, weight: .semibold))
        chevron.contentTintColor = dimColor
        disclosure.addSubview(chevron)
        let optionsField = label("Options", font: NSFont.systemFont(ofSize: 12.5, weight: .medium), color: dimColor)
        optionsField.frame = NSRect(x: 22, y: 3, width: 120, height: 16)
        disclosure.addSubview(optionsField)
        disclosure.onClicked = { [weak self] in
            guard let self = self else { return }
            self.awakeOptionsOpen.toggle()
            self.rebuildMenu()
        }
        card.addSubview(disclosure)
        if awakeOptionsOpen {
            let floor = state.battery_floor ?? 10
            let rows: [(String, Bool, (Bool) -> Void)] = [
                ("Allow the display to sleep", state.allow_display_sleep == true,
                 { [weak self] on in self?.patchAwake(["allow_display_sleep": on]) }),
                ("Stop below \(floor > 0 ? floor : 10)% battery", floor > 0,
                 { [weak self] on in self?.patchAwake(["battery_floor": on ? 10 : 0]) }),
            ]
            for row in rows {
                y -= option
                let field = label(row.0, font: NSFont.systemFont(ofSize: 12.5), color: inkColor)
                field.frame = NSRect(x: cardInset, y: y + 7, width: inner - 50, height: 16)
                card.addSubview(field)
                let toggle = ToggleSwitch(isOn: row.1)
                toggle.frame.origin = NSPoint(x: contentWidth - cardInset - 40, y: y + 3)
                toggle.onChanged = row.2
                card.addSubview(toggle)
            }
        }

        y -= 10 + 1
        let divider = NSView(frame: NSRect(x: cardInset, y: y, width: inner, height: 1))
        divider.wantsLayer = true
        divider.layer?.backgroundColor = cardBorderColor.cgColor
        card.addSubview(divider)
        y -= 10

        // Keep going with the lid closed.
        y -= lid
        let lidTitle = label("Keep going with the lid closed", font: NSFont.systemFont(ofSize: 13), color: inkColor)
        lidTitle.frame = NSRect(x: cardInset, y: y + 17, width: inner - 48, height: 17)
        card.addSubview(lidTitle)
        let lidSub = label(awakeLidSubtitle(state), font: NSFont.systemFont(ofSize: 11), color: dimColor)
        lidSub.frame = NSRect(x: cardInset, y: y + 1, width: inner - 48, height: 14)
        lidSub.toolTip = lidSub.stringValue
        card.addSubview(lidSub)
        let lidToggle = ToggleSwitch(isOn: state.lid_closed == true)
        lidToggle.frame.origin = NSPoint(x: contentWidth - cardInset - 40, y: y + 5)
        lidToggle.onChanged = { [weak self] on in self?.setLidClosed(on, ready: state.lid_ready == true) }
        card.addSubview(lidToggle)

        if let notice = notice {
            y -= 6 + 28
            let field = label(notice, font: NSFont.systemFont(ofSize: 11), color: warnColor)
            field.maximumNumberOfLines = 2
            field.lineBreakMode = .byWordWrapping
            field.toolTip = notice
            field.frame = NSRect(x: cardInset, y: y, width: inner, height: 28)
            card.addSubview(field)
        }
        return card
    }

    func startAwake(minutes: Int, choice: String) {
        sendAwake(["on": true, "minutes": minutes], choice: choice)
    }

    func startAwake(until: Date) {
        sendAwake(["on": true, "until": Int(until.timeIntervalSince1970)], choice: "until")
    }

    func stopAwake() {
        sendAwake(["on": false], choice: nil)
    }

    private func sendAwake(_ body: [String: Any], choice: String?) {
        guard !quitting, !awakeBusy else { return }
        awakeBusy = true
        post(path: "api/awake", body: body, timeout: 20) { [weak self] result in
            guard let self = self else { return }
            self.awakeBusy = false
            if case .success = result { self.awakeChoice = choice }
            self.applyAwakeResult(result, title: "Could not change keep awake")
        }
    }

    func patchAwake(_ body: [String: Any]) {
        guard !quitting, !awakeBusy else { return }
        awakeBusy = true
        post(path: "api/awake", method: "PATCH", body: body, timeout: 20) { [weak self] result in
            guard let self = self else { return }
            self.awakeBusy = false
            self.applyAwakeResult(result, title: "Could not change keep awake")
        }
    }

    // The first closed-lid use shows macOS's administrator prompt. The menu
    // closes so the prompt has focus.
    func setLidClosed(_ on: Bool, ready: Bool) {
        guard !quitting, !awakeBusy else { return }
        awakeBusy = true
        let prompts = on && !ready
        if prompts {
            menu.cancelTracking()
            menuActionMessage = "Waiting for the administrator password…"
            rebuildMenu()
        }
        post(path: "api/awake", method: "PATCH", body: ["lid_closed": on], timeout: prompts ? 330 : 20) { [weak self] result in
            guard let self = self else { return }
            self.awakeBusy = false
            if prompts { self.menuActionMessage = "" }
            if case .failure(let error) = result, error.localizedDescription.contains("not entered") {
                self.menuActionMessage = "The closed lid needs the administrator password once"
                self.fetchStateAsync()
                return
            }
            self.applyAwakeResult(result, title: "Could not set up the closed lid")
        }
    }

    private func applyAwakeResult(_ result: Result<Data, Error>, title: String) {
        switch result {
        case .success(let data):
            if let awake = try? JSONDecoder().decode(AwakeState.self, from: data), let state = cachedState {
                if !awake.active { awakeChoice = nil }
                cachedState = state.withAwake(awake)
            }
            rebuildMenu()
            fetchStateAsync()
        case .failure(let error):
            runtime.showError(title, error.localizedDescription)
            rebuildMenu()
            fetchStateAsync()
        }
    }

    // MARK: actions

    func activateAccount(_ account: Account) {
        guard !quitting, switchingAccountID == nil else { return }
        switchingAccountID = account.id
        if account.native_switch_available == true {
            nativeSwitchMessage = "Switching Claude Code login…"
            rebuildMenu()
        }
        post(path: "api/accounts/\(account.id)/activate") { [weak self] result in
            guard let self = self else { return }
            self.switchingAccountID = nil
            switch result {
            case .success(let data):
                let body = try? JSONDecoder().decode(AccountActivationResponse.self, from: data)
                self.nativeSwitchMessage = body?.native?.message ?? ""
            case .failure(let error):
                self.nativeSwitchMessage = ""
                self.runtime.showError("Could not switch account", error.localizedDescription)
            }
            self.rebuildMenu()
            self.fetchStateAsync()
            if !self.nativeSwitchMessage.isEmpty {
                let message = self.nativeSwitchMessage
                self.runtime.schedule(8) { [weak self] in
                    guard let self = self, !self.quitting, self.nativeSwitchMessage == message else { return }
                    self.nativeSwitchMessage = ""
                    self.rebuildMenu()
                }
            }
        }
    }

    func syncClaudeSessions() {
        #if !SWITCHER_LAYOUT_TEST
        guard !quitting, claudeSyncPhase != "running", !claudeSyncConfirming else { return }
        claudeSyncConfirming = true
        menu.cancelTracking()
        NSApp.activate(ignoringOtherApps: true)
        let confirmed = claudeSyncConfirmation().runModal() == .alertSecondButtonReturn
        claudeSyncConfirming = false
        guard confirmed else { return }
        claudeSyncGeneration += 1
        let generation = claudeSyncGeneration
        claudeSyncPhase = "running"
        claudeSyncDetail = ""
        rebuildMenu()
        post(path: "api/claude/sync") { [weak self] result in
            guard let self = self else { return }
            switch result {
            case .success(let data):
                let body = try? JSONDecoder().decode(ClaudeSyncResponse.self, from: data)
                self.claudeSyncPhase = body?.detail == nil ? "error" : "success"
                self.claudeSyncDetail = body?.detail ?? "Invalid session sync response"
            case .failure(let error):
                self.claudeSyncPhase = "error"
                self.claudeSyncDetail = error.localizedDescription
            }
            self.rebuildMenu()
            if self.claudeSyncPhase == "success" {
                self.runtime.schedule(6) { [weak self] in
                    guard let self = self, !self.quitting, self.claudeSyncPhase == "success",
                          self.claudeSyncGeneration == generation else { return }
                    self.claudeSyncPhase = "idle"
                    self.rebuildMenu()
                }
            }
        }
        #endif
    }

    @objc func showClaudeSyncError() {
        runtime.showError("Claude session sync did not complete", claudeSyncDetail)
    }

    @objc func refreshUsage() {
        guard !quitting, !usageRefreshRunning else { return }
        usageRefreshRunning = true
        refreshButton?.setBusy(true)
        post(path: "api/usage/refresh") { [weak self] result in
            guard let self = self else { return }
            self.usageRefreshRunning = false
            self.refreshButton?.setBusy(false)
            switch result {
            case .success:
                self.menuNote = "Usage refresh accepted"
                self.fetchStateAsync()
            case .failure(let error):
                self.menuActionMessage = "Usage refresh failed"
                self.runtime.showError("Could not refresh usage", error.localizedDescription)
            }
            self.rebuildMenu()
        }
    }

    @objc func openWebApp() {
        #if !SWITCHER_LAYOUT_TEST
        NSWorkspace.shared.open(hubURL)
        #endif
    }

    @objc func runUpdate() {
        guard !quitting, !updateActionRunning else { return }
        let install = cachedState?.update?.update_available == true
        updateActionRunning = true
        updateGeneration += 1
        let generation = updateGeneration
        let oldVersion = cachedState?.version
        menuActionMessage = install ? "" : "Checking for updates…"
        if install {
            installProgress = InstallProgress(step: "starting", fraction: 0)
            installRelease = cachedState?.update?.latest
            pollInstallProgress(generation: generation)
        }
        rebuildMenu()
        post(path: install ? "api/update" : "api/update/check", timeout: install ? 660 : 30) { [weak self] result in
            guard let self = self else { return }
            if install {
                if case .failure(let error) = result, (error as NSError).domain == "SwitcherHTTP" {
                    self.finishUpdateFailure(error, install: true)
                } else {
                    // A dropped request may be the restart or a lost
                    // connection; only an answer proves the install done.
                    if case .success = result {
                        self.showInstallProgress(InstallProgress(step: "restarting", fraction: 1))
                    }
                    self.awaitUpdatedServer(oldVersion: oldVersion, generation: generation, remaining: 20)
                }
                return
            }
            switch result {
            case .success(let data):
                struct CheckResponse: Decodable { let update: UpdateInfo? }
                let flat = try? JSONDecoder().decode(UpdateInfo.self, from: data)
                let nested = (try? JSONDecoder().decode(CheckResponse.self, from: data))?.update
                guard let info = flat?.update_available != nil ? flat : nested, info.update_available != nil else {
                    self.finishUpdateFailure(NSError(domain: "SwitcherHTTP", code: 0,
                        userInfo: [NSLocalizedDescriptionKey: "Invalid update metadata response"]), install: false)
                    return
                }
                self.updateActionRunning = false
                // An available update turns the button into Install update
                // and badges the menu bar mark; nothing else needs saying.
                self.menuActionMessage = ""
                self.menuNote = info.latest == nil && info.update_available != true ? "No update metadata available" : ""
                self.updateCheckNote = info.latest != nil && info.update_available != true ? "Up to date" : nil
                _ = self.beginStateRequest()
                if let state = self.cachedState { self.applyMenuState(state.withUpdate(info)) }
                else { self.fetchStateAsync() }
                self.rebuildMenu()
            case .failure(let error): self.finishUpdateFailure(error, install: false)
            }
        }
    }

    // While an install runs, the menu reads its progress a few times a
    // second.
    private func pollInstallProgress(generation: Int) {
        runtime.schedule(0.4) { [weak self] in
            guard let self = self, !self.quitting, self.updateGeneration == generation,
                  let current = self.installProgress, current.step != "restarting" else { return }
            self.performRequest(self.makeRequest(path: "api/update", timeout: 2)) { [weak self] data, response, _ in
                guard let self = self, !self.quitting, self.updateGeneration == generation else { return }
                if (response as? HTTPURLResponse)?.statusCode == 200, let data = data,
                   let progress = (try? JSONDecoder().decode(UpdateProgressResponse.self, from: data))?.install {
                    self.showInstallProgress(progress)
                }
                self.pollInstallProgress(generation: generation)
            }
        }
    }

    // The bar follows the server, but nothing moves it off the restart: a
    // poll answered before the install finished can arrive after it.
    func showInstallProgress(_ progress: InstallProgress) {
        guard let current = installProgress, current.step != "restarting" else { return }
        installProgress = progress
        installProgressView?.show(progress)
    }

    private func finishUpdateFailure(_ error: Error, install: Bool) {
        updateActionRunning = false
        installProgress = nil
        menuActionMessage = install ? "Update installation failed" : "Update check failed"
        rebuildMenu()
        runtime.showError(install ? "Could not install update" : "Could not check for updates", error.localizedDescription)
    }

    private func awaitUpdatedServer(oldVersion: String?, generation: Int, remaining: Int) {
        guard !quitting, updateGeneration == generation else { return }
        guard remaining > 0 else {
            finishUpdateFailure(NSError(domain: NSURLErrorDomain, code: URLError.timedOut.rawValue,
                userInfo: [NSLocalizedDescriptionKey: "Timed out waiting for the updated server"]), install: true)
            return
        }
        runtime.schedule(1.5) { [weak self] in
            guard let self = self, !self.quitting, self.updateGeneration == generation else { return }
            let serial = self.beginStateRequest()
            self.performRequest(self.makeRequest(path: "api/state")) { [weak self] data, response, _ in
                guard let self = self, !self.quitting, self.updateGeneration == generation else { return }
                if (response as? HTTPURLResponse)?.statusCode == 200, let data = data,
                   let state = try? JSONDecoder().decode(AppState.self, from: data),
                   let version = state.version, version != oldVersion {
                    self.updateActionRunning = false
                    self.installProgress = nil
                    self.menuNote = "Updated to \(version)"
                    _ = self.acceptStateResponse(state, serial: serial)
                    self.rebuildMenu()
                } else {
                    self.awaitUpdatedServer(oldVersion: oldVersion, generation: generation, remaining: remaining - 1)
                }
            }
        }
    }

    @objc func toggleStartAtLogin() {
        #if !SWITCHER_LAYOUT_TEST
        if startAtLoginEnabled() {
            try? FileManager.default.removeItem(atPath: launchAgentPath)
        } else {
            let exe = Bundle.main.bundlePath + "/Contents/MacOS/Switcher"
            let plist: [String: Any] = [
                "Label": launchAgentLabel,
                "ProgramArguments": [exe],
                "RunAtLoad": true,
            ]
            let dir = NSHomeDirectory() + "/Library/LaunchAgents"
            try? FileManager.default.createDirectory(atPath: dir, withIntermediateDirectories: true)
            if let data = try? PropertyListSerialization.data(fromPropertyList: plist, format: .xml, options: 0) {
                try? data.write(to: URL(fileURLWithPath: launchAgentPath))
            }
        }
        #endif
    }

    @objc func testResetAlert() {
        guard cachedState?.reset_notifications == true else { return }
        sendTestResetAlert { error in
            if let error = error { NSLog("Switcher test alert failed: %@", error) }
        }
    }

    private func stopOwnedServerForQuit() {
        guard !quitting else { return }
        quitting = true
        recoveryGeneration += 1
        updateGeneration += 1
        _ = beginStateRequest()
        serverProcess?.terminate()
        serverProcess = nil
    }

    func applicationWillTerminate(_ notification: Notification) {
        stopOwnedServerForQuit()
    }

    @objc func quit() {
        stopOwnedServerForQuit()
        runtime.terminateApplication()
    }

    // An update replaced Switcher.app while this process kept running the
    // old code. Reopen from disk, which also restarts an owned server.
    func relaunchIfReplaced() {
        guard !quitting, !relaunching, let launched = runtime.launchedAppVersion(),
              let disk = runtime.diskAppVersion(), disk != launched else { return }
        relaunching = true
        NSLog("Switcher %@ was replaced by %@ on disk; relaunching", launched, disk)
        runtime.relaunchApp()
        quit()
    }

    func startAtLoginEnabled() -> Bool {
        #if SWITCHER_LAYOUT_TEST
        return false
        #else
        return FileManager.default.fileExists(atPath: launchAgentPath)
        #endif
    }
}

// MARK: - top-level view builders

func menuItemWithView(_ view: NSView) -> NSMenuItem {
    let item = NSMenuItem()
    item.view = view
    item.isEnabled = false
    return item
}


func spacerView(height: CGFloat) -> NSView {
    NSView(frame: NSRect(x: 0, y: 0, width: menuWidth, height: height))
}

// centered wraps a card in a full menu-width container so NSMenu cannot
// pin it to the left edge. Equal edgeInset on both sides.
func centered(_ card: NSView, verticalPadding: CGFloat = 0) -> NSView {
    let container = NSView(frame: NSRect(x: 0, y: 0, width: menuWidth, height: card.frame.height + 2 * verticalPadding))
    card.frame.origin = NSPoint(x: (menuWidth - card.frame.width) / 2, y: verticalPadding)
    container.addSubview(card)
    return container
}

#if !SWITCHER_LAYOUT_TEST
@main
struct SwitcherApp {
    static func main() {
        if CommandLine.arguments.dropFirst() == ["--notification-status"] {
            let done = DispatchSemaphore(value: 0)
            UNUserNotificationCenter.current().getNotificationSettings { settings in
                let status = resetAlertPermission(settings)
                FileHandle.standardOutput.write(Data((status.message + "\n").utf8))
                done.signal()
            }
            guard done.wait(timeout: .now() + 5) == .success else {
                FileHandle.standardError.write(Data("Could not read macOS notification settings\n".utf8))
                exit(1)
            }
            return
        }
        if CommandLine.arguments.dropFirst() == ["--test-notification"] {
            let done = DispatchSemaphore(value: 0)
            sendTestResetAlert { error in
                if let error = error {
                    FileHandle.standardError.write(Data((error + "\n").utf8))
                    exit(1)
                }
                FileHandle.standardOutput.write(Data("Reset test alert accepted by macOS\n".utf8))
                done.signal()
            }
            guard done.wait(timeout: .now() + 5) == .success else {
                FileHandle.standardError.write(Data("Reset test alert timed out\n".utf8))
                exit(1)
            }
            return
        }
        let app = NSApplication.shared
        let delegate = AppDelegate()
        app.delegate = delegate
        app.setActivationPolicy(.accessory) // menu bar app: no Dock icon
        withExtendedLifetime(delegate) { app.run() }
    }
}
#endif
