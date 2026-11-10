import AppKit
import UserNotifications

func fail(_ message: String) -> Never {
    FileHandle.standardError.write(Data((message + "\n").utf8))
    exit(1)
}

func runMenuActionRegressionFixtures() {
    func response(_ status: Int) -> HTTPURLResponse {
        HTTPURLResponse(url: URL(string: "https://fixture.invalid/state")!, statusCode: status,
            httpVersion: nil, headerFields: nil)!
    }
    func fixtureState(_ update: UpdateInfo? = nil) -> AppState {
        AppState(accounts: [], order: [], hidden: [], version: "fixture-1",
            update: update, menu_usage_bars: true, reset_notifications: true)
    }
    var requests: [URLRequest] = []
    var completions: [(Data?, URLResponse?, Error?) -> Void] = []
    var errors: [String] = []
    var saved: [ResetAlertLedger] = []
    var runtime = MenuRuntime()
    runtime.request = { request, completion in requests.append(request); completions.append(completion) }
    runtime.deviceToken = { "fixture-device-token" }
    runtime.saveLedger = { saved.append($0) }
    runtime.showError = { title, message in errors.append(title + ": " + message) }
    let delegate = AppDelegate(runtime: runtime)
    delegate.applyMenuState(fixtureState(UpdateInfo(latest: "fixture-1", update_available: false)))
    let stalePoll = delegate.beginStateRequest()
    delegate.runUpdate()
    guard requests.count == 1, requests[0].url?.path == "/api/update/check",
          requests[0].httpMethod == "POST",
          requests[0].value(forHTTPHeaderField: "Authorization") == "Bearer fixture-device-token" else {
        fail("Check for updates must request authenticated metadata without installation")
    }
    completions.removeFirst()(Data(#"{"status":"ok","update":{"latest":"fixture-2","update_available":true}}"#.utf8), response(200), nil)
    guard delegate.cachedState?.update?.update_available == true,
          !delegate.acceptStateResponse(fixtureState(), serial: stalePoll) else {
        fail("A successful update check must refresh the install action")
    }
    func menuTexts(_ menu: NSMenu) -> [String] {
        func walk(_ view: NSView) -> [String] {
            ((view as? NSTextField).map { [$0.stringValue] } ?? []) + ((view as? NSButton).map { [$0.title] } ?? [])
                + view.subviews.flatMap(walk)
        }
        return menu.items.compactMap(\.view).flatMap(walk)
    }
    guard menuTexts(delegate.menu).contains("Install update"),
          !menuTexts(delegate.menu).contains(where: { $0.hasPrefix("Update available") }) else {
        fail("An available update needs no note beside its Install button")
    }
    let upToDate = AppDelegate(runtime: runtime)
    upToDate.applyMenuState(fixtureState(UpdateInfo(latest: "fixture-1", update_available: false)))
    requests.removeAll(); completions.removeAll()
    upToDate.runUpdate()
    completions.removeFirst()(Data(#"{"status":"ok","update":{"latest":"fixture-1","update_available":false}}"#.utf8), response(200), nil)
    guard upToDate.updateCheckNote == "Up to date", menuTexts(upToDate.menu).contains("Up to date"),
          !menuTexts(upToDate.menu).contains("No newer release available") else {
        fail("A check without a newer release must say so in its button, not in a row of its own")
    }
    upToDate.menuDidClose(upToDate.menu)
    guard upToDate.updateCheckNote == nil, menuTexts(upToDate.menu).contains("Check for updates") else {
        fail("The update check note must go when the menu closes")
    }
    requests.removeAll(); completions.removeAll()
    delegate.runUpdate()
    guard requests.count == 1, requests[0].url?.path == "/api/update" else { fail("An explicit available update must use installation") }
    completions.removeFirst()(Data(#"{"error":{"message":"fixture active update"}}"#.utf8), response(409), nil)
    guard errors.last?.contains("fixture active update") == true else { fail("Native update HTTP errors were swallowed") }
    requests.removeAll(); completions.removeAll()
    delegate.refreshUsage()
    delegate.refreshUsage()
    guard requests.count == 1, requests[0].url?.path == "/api/usage/refresh" else { fail("Native usage refresh must deduplicate while pending") }
    completions.removeFirst()(Data(#"{"error":{"message":"fixture refresh rejected"}}"#.utf8), response(500), nil)
    guard errors.last?.contains("fixture refresh rejected") == true else { fail("Native refresh HTTP errors were swallowed") }
    requests.removeAll(); completions.removeAll()
    delegate.refreshUsage()
    guard requests.count == 1 else { fail("Failed refresh did not release the retry control") }
    completions.removeFirst()(nil, nil, URLError(.timedOut))
    guard errors.last?.localizedLowercase.contains("timed out") == true else { fail("Native refresh timeout was not reported") }

    let alert = ResetAlert(id: "fixture-reset", accountID: "fixture-account", providerID: "codex",
        provider: "Codex", window: "Weekly", date: Date().addingTimeInterval(-60))
    let ledger = ResetAlertLedger(alerts: [alert], deliveredIDs: [])
    delegate.restoreResetLedger(ledger)
    delegate.cachedState = nil
    saved.removeAll(); requests.removeAll()
    delegate.deliverDueResetAlerts()
    guard delegate.resetAlertLedger == ledger, saved.isEmpty, requests.isEmpty else {
        fail("Unknown startup state erased persisted alerts or attempted delivery")
    }

    var launched = 0, terminated = 0, quitCalls = 0
    var exits: [(UUID) -> Void] = []
    var tasks: [(TimeInterval, () -> Void)] = []
    runtime.request = { _, completion in completion(nil, nil, URLError(.cannotConnectToHost)) }
    runtime.schedule = { delay, task in tasks.append((delay, task)) }
    runtime.launchServer = { exit in
        launched += 1; exits.append(exit)
        return OwnedServer(id: UUID(), terminate: { terminated += 1 })
    }
    runtime.terminateApplication = { quitCalls += 1 }
    let owned = AppDelegate(runtime: runtime)
    owned.ensureServerRunning()
    guard launched == 1, let initialID = owned.serverProcess?.id else { fail("Fixture initial child was not owned") }
    owned.applyMenuState(fixtureState())
    exits[0](initialID)
    guard owned.cachedState == nil else { fail("Owned child exit left stale account cards") }
    for delay in [1.0, 2.0, 4.0] {
        guard let index = tasks.firstIndex(where: { $0.0 == delay }) else { fail("Missing bounded recovery delay \(delay)") }
        tasks.remove(at: index).1()
        guard let id = owned.serverProcess?.id else { fail("Recovery did not own its replacement child") }
        exits[0](initialID)
        guard owned.serverProcess?.id == id else { fail("A stale child exit discarded its replacement") }
        exits.last!(id)
    }
    guard launched == 4, !tasks.contains(where: { [1.0, 2.0, 4.0].contains($0.0) }), terminated == 0 else {
        fail("Crash recovery was unbounded or terminated a process")
    }

    tasks.removeAll(); launched = 0; exits.removeAll()
    let quitting = AppDelegate(runtime: runtime)
    quitting.ensureServerRunning()
    guard let quitID = quitting.serverProcess?.id else { fail("Quit fixture missing owned child") }
    quitting.quit()
    exits[0](quitID)
    for (_, task) in tasks { task() }
    guard launched == 1, terminated == 1, quitCalls == 1 else { fail("Deliberate quit restarted its server") }

    tasks.removeAll(); launched = 0; terminated = 0; quitCalls = 0; exits.removeAll()
    let queuedQuit = AppDelegate(runtime: runtime)
    queuedQuit.ensureServerRunning()
    guard let queuedID = queuedQuit.serverProcess?.id else { fail("Queued quit fixture missing its child") }
    exits[0](queuedID)
    queuedQuit.quit()
    for (_, task) in tasks { task() }
    guard launched == 1, terminated == 0, quitCalls == 1 else { fail("Quit failed to fence queued recovery") }

    tasks.removeAll(); launched = 0; terminated = 0; exits.removeAll()
    let systemQuit = AppDelegate(runtime: runtime)
    systemQuit.ensureServerRunning()
    guard let systemID = systemQuit.serverProcess?.id else { fail("System quit fixture missing its child") }
    systemQuit.applicationWillTerminate(Notification(name: NSApplication.willTerminateNotification))
    exits[0](systemID)
    for (_, task) in tasks { task() }
    guard launched == 1, terminated == 1 else { fail("Application termination failed to stop only its owned child") }

    var externalRuntime = runtime
    externalRuntime.request = { _, completion in completion(nil, response(401), nil) }
    let external = AppDelegate(runtime: externalRuntime)
    launched = 0; terminated = 0
    external.ensureServerRunning()
    external.quit()
    guard launched == 0, terminated == 0 else { fail("An existing external listener was spawned over or terminated") }

    var delayedProbe: ((Data?, URLResponse?, Error?) -> Void)?
    var probingRuntime = runtime
    probingRuntime.request = { _, completion in delayedProbe = completion }
    let probing = AppDelegate(runtime: probingRuntime)
    launched = 0; tasks.removeAll()
    probing.ensureServerRunning()
    probing.quit()
    delayedProbe?(nil, nil, URLError(.cannotConnectToHost))
    for (_, task) in tasks { task() }
    guard launched == 0 else { fail("A probe completed after quit and launched a child") }

    let inert = MenuRuntime()
    var inertError: Error?
    inert.request(URLRequest(url: URL(string: "https://fixture.invalid/")!)) { _, _, error in inertError = error }
    guard (inertError as NSError?)?.domain == "SwitcherFixture", inert.deviceToken() == nil,
          !inert.alertsEnabled(), (try? inert.loadLedger())?.alerts.isEmpty == true else { fail("Fixture runtime defaults were not inert") }
    do { _ = try inert.launchServer { _ in fail("Inert runtime launched a child") }; fail("Inert launch did not refuse process creation") }
    catch { guard (error as NSError).domain == "SwitcherFixture" else { fail("Unexpected inert launch error") } }
    print("Native update check, refresh errors, unknown reset ledger, bounded owned recovery, and deliberate quit: OK")
}

// Update badge and install progress: the menu bar mark announces a waiting
// release, and an install draws its progress from the server's reports.
func runUpdateBadgeAndProgressFixtures() {
    let mark = NSImage(size: NSSize(width: 18, height: 18), flipped: false) { rect in
        NSColor.black.setFill(); rect.fill(); return true
    }
    let badged = badgedStatusImage(mark)
    let bitmap = NSBitmapImageRep(bitmapDataPlanes: nil, pixelsWide: Int(badged.size.width * 4), pixelsHigh: 72,
        bitsPerSample: 8, samplesPerPixel: 4, hasAlpha: true, isPlanar: false, colorSpaceName: .deviceRGB,
        bytesPerRow: 0, bitsPerPixel: 0)!
    bitmap.size = badged.size
    NSGraphicsContext.saveGraphicsState()
    NSGraphicsContext.current = NSGraphicsContext(bitmapImageRep: bitmap)
    badged.draw(in: NSRect(origin: .zero, size: badged.size))
    NSGraphicsContext.restoreGraphicsState()
    func alpha(_ x: CGFloat, _ y: CGFloat) -> CGFloat {   // points, origin bottom left
        bitmap.colorAt(x: Int(x * 4), y: Int((badged.size.height - y) * 4))?.alphaComponent ?? -1
    }
    let center = CGPoint(x: badged.size.width - 4.5, y: 4.5)
    guard badged.isTemplate, badged.size.width > 18, badged.size.height == 18,
          alpha(3, 15) > 0.99,                                  // the mark itself
          alpha(center.x - 4.9, center.y) < 0.01,               // the gap around the badge
          alpha(center.x - 3, center.y - 1.5) > 0.99,           // the disc
          alpha(center.x, center.y) < 0.01,                     // the arrow's stem
          alpha(center.x, center.y + 2.4) < 0.01 else {         // the arrow's head
        fail("The update badge must be a disc with an arrow cut out, set apart from the mark")
    }

    func response(_ status: Int) -> HTTPURLResponse {
        HTTPURLResponse(url: URL(string: "https://fixture.invalid/update")!, statusCode: status,
            httpVersion: nil, headerFields: nil)!
    }
    func fixtureState(_ update: UpdateInfo?, version: String = "fixture-1") -> AppState {
        AppState(accounts: [], order: [], hidden: [], version: version,
            update: update, menu_usage_bars: true, reset_notifications: false)
    }
    var requests: [URLRequest] = []
    var completions: [(Data?, URLResponse?, Error?) -> Void] = []
    var tasks: [(TimeInterval, () -> Void)] = []
    var errors: [String] = []
    var runtime = MenuRuntime()
    runtime.request = { request, completion in requests.append(request); completions.append(completion) }
    runtime.schedule = { delay, task in tasks.append((delay, task)) }
    runtime.showError = { title, message in errors.append(title + ": " + message) }
    runtime.deviceToken = { "fixture-device-token" }
    func runTask(_ delay: TimeInterval) {
        guard let index = tasks.firstIndex(where: { $0.0 == delay }) else { fail("Missing scheduled task after \(delay)s") }
        tasks.remove(at: index).1()
    }

    let delegate = AppDelegate(runtime: runtime)
    delegate.applyMenuState(fixtureState(UpdateInfo(latest: "v2.0.0", update_available: true)))
    guard delegate.statusBadge == "v2.0.0" else { fail("A waiting release must badge the menu bar mark") }
    delegate.applyMenuState(fixtureState(UpdateInfo(latest: "fixture-1", update_available: false)))
    guard delegate.statusBadge == nil else { fail("The badge must go once no newer release waits") }
    delegate.applyMenuState(fixtureState(UpdateInfo(latest: "v2.0.0", update_available: true)))

    delegate.runUpdate()
    guard requests.count == 1, requests[0].url?.path == "/api/update", requests[0].httpMethod == "POST",
          let card = delegate.installProgressView, card.percentText == "0%",
          delegate.menu.items.contains(where: { $0.view?.subviews.contains(card) == true }) else {
        fail("Installing must show the progress card in the menu")
    }
    let install = completions.removeFirst()
    runTask(0.4)
    guard requests.count == 2, requests[1].url?.path == "/api/update", requests[1].httpMethod == "GET",
          requests[1].value(forHTTPHeaderField: "Authorization") == "Bearer fixture-device-token" else {
        fail("An install must poll its progress")
    }
    completions.removeFirst()(Data(#"{"update_available":true,"install":{"step":"downloading","fraction":0.4,"done":4000000,"total":8000000}}"#.utf8), response(200), nil)
    let trackWidth = menuWidth - 2 * edgeInset - 2 * cardInset
    guard card.percentText == "40%", card.detailText == "Downloading · 4.0 of 8.0 MB",
          abs(card.fillWidth - trackWidth * 0.4) < 0.5 else {
        fail("A progress report must move the bar and say what is happening: \(card.percentText) \(card.detailText) \(card.fillWidth)")
    }
    runTask(0.4)
    completions.removeFirst()(nil, nil, URLError(.timedOut))
    guard card.percentText == "40%", tasks.contains(where: { $0.0 == 0.4 }) else { fail("A failed progress read must keep polling") }
    runTask(0.4)
    let inFlight = completions.removeFirst()
    install(Data(#"{"status":"ok"}"#.utf8), response(200), nil)
    guard card.percentText == "100%", card.detailText == "Restarting Switcher…" else {
        fail("A finished install must show the restart")
    }
    inFlight(Data(#"{"install":{"step":"installing","fraction":0.9}}"#.utf8), response(200), nil)
    guard card.percentText == "100%" else { fail("A poll answered before the install finished must not move the bar off the restart") }
    let before = requests.count
    runTask(0.4)
    guard requests.count == before else { fail("Progress polling must stop once Switcher restarts") }
    runTask(1.5)
    completions.removeLast()(try! JSONEncoder().encode(fixtureState(nil, version: "2.0.0")), response(200), nil)
    guard delegate.installProgress == nil, delegate.installProgressView == nil else {
        fail("The progress card must go once the new version answers")
    }

    requests.removeAll(); completions.removeAll(); tasks.removeAll()
    let dropped = AppDelegate(runtime: runtime)
    dropped.applyMenuState(fixtureState(UpdateInfo(latest: "v2.0.0", update_available: true)))
    dropped.runUpdate()
    completions.removeFirst()(nil, nil, URLError(.networkConnectionLost))
    guard dropped.installProgress?.step == "starting" else { fail("A dropped install request must not claim the restart") }
    runTask(0.4)
    completions.removeFirst()(Data(#"{"install":{"step":"downloading","fraction":0.5}}"#.utf8), response(200), nil)
    guard dropped.installProgressView?.percentText == "50%" else { fail("After a dropped request the bar must keep following the server") }

    requests.removeAll(); completions.removeAll(); tasks.removeAll()
    let failing = AppDelegate(runtime: runtime)
    failing.applyMenuState(fixtureState(UpdateInfo(latest: "v2.0.0", update_available: true)))
    failing.runUpdate()
    completions.removeFirst()(Data(#"{"error":"fixture install failed"}"#.utf8), response(500), nil)
    guard failing.installProgress == nil, failing.installProgressView == nil,
          errors.last?.contains("fixture install failed") == true else {
        fail("A failed install must drop the progress card and report the error")
    }
    runTask(0.4)
    guard requests.count == 1 else { fail("A failed install must stop polling") }
    print("Update badge, install progress polling, restart and failure: OK")
}

// Keep awake: chips start and stop sessions, the lid toggle patches the
// setting, and a replaced app relaunches itself.
func runKeepAwakeFixtures() {
    var requests: [URLRequest] = []
    var completions: [(Data?, URLResponse?, Error?) -> Void] = []
    func response(_ status: Int) -> HTTPURLResponse {
        HTTPURLResponse(url: URL(string: "https://fixture.invalid/awake")!, statusCode: status,
            httpVersion: nil, headerFields: nil)!
    }
    func json(_ request: URLRequest) -> [String: Any] {
        guard let body = request.httpBody,
              let object = try? JSONSerialization.jsonObject(with: body) as? [String: Any] else { return [:] }
        return object
    }
    func views<T: NSView>(_ type: T.Type, in menu: NSMenu) -> [T] {
        func walk(_ view: NSView) -> [T] { ((view as? T).map { [$0] } ?? []) + view.subviews.flatMap(walk) }
        return menu.items.compactMap(\.view).flatMap(walk)
    }
    func texts(in menu: NSMenu) -> [String] { views(NSTextField.self, in: menu).map(\.stringValue) }
    // Answers the newest keep-awake request; state refreshes stay pending.
    func answerAwake(_ body: String) {
        guard let index = requests.lastIndex(where: { $0.url?.path == "/api/awake" }) else { fail("No keep-awake request to answer") }
        let completion = completions[index]
        requests.remove(at: index); completions.remove(at: index)
        completion(Data(body.utf8), response(200), nil)
    }
    func chip(_ title: String, in menu: NSMenu) -> ChipButton {
        guard let chip = views(ChipButton.self, in: menu).first(where: { $0.title == title }) else {
            fail("Keep-awake chip \(title) missing")
        }
        return chip
    }

    var runtime = MenuRuntime()
    runtime.request = { request, completion in requests.append(request); completions.append(completion) }
    runtime.deviceToken = { "fixture-device-token" }
    let delegate = AppDelegate(runtime: runtime)
    let off = AwakeState(supported: true, active: false, lid_ready: false, allow_display_sleep: false,
        lid_closed: false, battery_floor: 10)
    delegate.applyMenuState(AppState(accounts: [], order: [], hidden: [], version: "fixture-1",
        update: nil, menu_usage_bars: true, reset_notifications: false, awake: off))
    guard texts(in: delegate.menu).contains("Keep awake"),
          texts(in: delegate.menu).contains("Keep the Mac awake"),
          texts(in: delegate.menu).contains("Keep going with the lid closed"),
          texts(in: delegate.menu).contains("Asks for your password once"),
          views(ChipButton.self, in: delegate.menu).map(\.title) == ["15m", "30m", "1h", "2h", "4h", "8h", "∞", "Until…"] else {
        fail("Keep-awake section is incomplete: \(texts(in: delegate.menu))")
    }

    chip("1h", in: delegate.menu).onClicked?()
    guard requests.count == 1, requests[0].url?.path == "/api/awake", requests[0].httpMethod == "POST",
          json(requests[0])["on"] as? Bool == true, json(requests[0])["minutes"] as? Int == 60,
          requests[0].value(forHTTPHeaderField: "Content-Type") == "application/json" else {
        fail("A duration chip must start keep awake for its minutes")
    }
    let until = Date().addingTimeInterval(3600).timeIntervalSince1970
    answerAwake(#"{"supported":true,"active":true,"until":\#(until),"lid_ready":false,"battery_floor":10}"#)
    guard chip("1h", in: delegate.menu).selected, !chip("∞", in: delegate.menu).selected,
          texts(in: delegate.menu).contains(where: { $0.hasPrefix("Until ") && $0.hasSuffix("left") }) else {
        fail("A running timed session must show its chip and the time left")
    }
    requests.removeAll(); completions.removeAll()
    chip("1h", in: delegate.menu).onClicked?()
    guard requests.count == 1, json(requests[0])["on"] as? Bool == false else {
        fail("Clicking the running chip again must stop keep awake")
    }
    answerAwake(#"{"supported":true,"active":false,"lid_ready":false,"battery_floor":10}"#)
    guard !views(ChipButton.self, in: delegate.menu).contains(where: \.selected) else { fail("A stopped session kept a chip lit") }

    requests.removeAll(); completions.removeAll()
    chip("Until…", in: delegate.menu).onClicked?()
    let times = views(ChipButton.self, in: delegate.menu).map(\.title)
    guard requests.isEmpty, times.count == 8, times.last == "Back" else {
        fail("Until… must offer seven clock times and Back, without a request: \(times)")
    }
    chip(times[0], in: delegate.menu).onClicked?()
    guard requests.count == 1, let at = json(requests[0])["until"] as? Int, at > Int(Date().timeIntervalSince1970) else {
        fail("A clock time must start keep awake until that time")
    }
    answerAwake(#"{"supported":true,"active":false,"lid_ready":false,"battery_floor":10}"#)
    requests.removeAll(); completions.removeAll()

    guard let lidTitle = views(NSTextField.self, in: delegate.menu).first(where: { $0.stringValue == "Keep going with the lid closed" }),
          let lid = lidTitle.superview?.subviews.compactMap({ $0 as? ToggleSwitch }).last else { fail("Lid toggle missing") }
    lid.isOn = true
    lid.onChanged?(true)
    guard requests.count == 1, requests[0].httpMethod == "PATCH", json(requests[0])["lid_closed"] as? Bool == true,
          requests[0].timeoutInterval > 60 else {
        fail("The first closed-lid use must patch the setting and wait for the password prompt")
    }
    answerAwake(#"{"supported":true,"active":true,"lid_active":true,"lid_ready":true,"lid_closed":true}"#)
    guard texts(in: delegate.menu).contains("Sleep fully disabled. Mind the power"),
          texts(in: delegate.menu).contains("Active until you turn it off"),
          chip("∞", in: delegate.menu).selected else {
        fail("An active closed lid must warn about power")
    }
    guard awakeNotice(AwakeState(supported: true, active: false, ended: "battery"))?.contains("low battery") == true,
          awakeNotice(AwakeState(supported: true, active: true, ended: "battery")) == nil else {
        fail("Keep-awake endings were not explained")
    }

    // The header closes and opens the section, says what runs while closed,
    // and the choice is remembered.
    var saved: [(String, Bool)] = []
    var remembering = runtime
    remembering.saveFlag = { key, value in saved.append((key, value)) }
    let folding = AppDelegate(runtime: remembering)
    let timed = AwakeState(supported: true, active: true, until: Date().timeIntervalSince1970 + 2 * 3600 + 120,
        lid_ready: true, allow_display_sleep: false, lid_closed: false, battery_floor: 10)
    folding.applyMenuState(AppState(accounts: [], order: [], hidden: [], version: "fixture-1",
        update: nil, menu_usage_bars: true, reset_notifications: false, awake: timed))
    func header(_ menu: NSMenu) -> TapArea {
        guard let head = menu.items.compactMap({ $0.view as? TapArea })
            .first(where: { $0.subviews.contains { ($0 as? NSTextField)?.stringValue == "Keep awake" } }) else {
            fail("Keep-awake header is not a click target")
        }
        return head
    }
    guard !folding.awakeCollapsed, !views(ChipButton.self, in: folding.menu).isEmpty else { fail("Keep awake must start open") }
    header(folding.menu).onClicked?()
    guard folding.awakeCollapsed, views(ChipButton.self, in: folding.menu).isEmpty,
          saved.last.map({ $0.0 == "keepAwakeCollapsed" && $0.1 }) == true,
          let summary = texts(in: folding.menu).first(where: { $0.hasPrefix("On · ") }), summary.hasSuffix(" left"),
          !texts(in: folding.menu).contains("Keep going with the lid closed") else {
        fail("Closing keep awake must hide the card, remember it and say what runs: \(texts(in: folding.menu))")
    }
    header(folding.menu).onClicked?()
    guard !folding.awakeCollapsed, saved.last.map({ !$0.1 }) == true,
          views(ChipButton.self, in: folding.menu).map(\.title).first == "15m" else {
        fail("Opening keep awake must bring the card back")
    }
    var closedBefore = runtime
    closedBefore.loadFlag = { $0 == "keepAwakeCollapsed" }
    let reopened = AppDelegate(runtime: closedBefore)
    reopened.applyMenuState(AppState(accounts: [], order: [], hidden: [], version: "fixture-1",
        update: nil, menu_usage_bars: true, reset_notifications: false, awake: off))
    guard reopened.awakeCollapsed, views(ChipButton.self, in: reopened.menu).isEmpty,
          texts(in: reopened.menu).contains("Off") else { fail("A closed keep awake must stay closed on the next launch") }
    guard awakeSummary(AwakeState(supported: true, active: true)).text == "On",
          awakeSummary(AwakeState(supported: true, active: false)).text == "Off",
          awakeSummary(AwakeState(supported: true, active: true, error: "fixture")).text == "Needs attention" else {
        fail("Closed keep-awake summaries are wrong")
    }

    // Unsupported servers show no section.
    delegate.applyMenuState(AppState(accounts: [], order: [], hidden: [], version: "fixture-1",
        update: nil, menu_usage_bars: true, reset_notifications: false, awake: nil))
    guard !texts(in: delegate.menu).contains("Keep awake") else { fail("Keep awake shown without support") }

    var relaunches = 0, terminations = 0
    var replaced = MenuRuntime()
    replaced.launchedAppVersion = { "1.11.0" }
    replaced.diskAppVersion = { "1.12.0" }
    replaced.relaunchApp = { relaunches += 1 }
    replaced.terminateApplication = { terminations += 1 }
    let old = AppDelegate(runtime: replaced)
    old.relaunchIfReplaced()
    old.relaunchIfReplaced()
    guard relaunches == 1, terminations == 1 else { fail("A replaced app must relaunch exactly once") }
    var current = MenuRuntime()
    current.launchedAppVersion = { "1.12.0" }
    current.diskAppVersion = { "1.12.0" }
    current.relaunchApp = { relaunches += 1 }
    AppDelegate(runtime: current).relaunchIfReplaced()
    guard relaunches == 1 else { fail("An unchanged app must not relaunch") }
    print("Keep awake chips, closed lid, and relaunch after update: OK")
}

// The Advanced submenu offers working without the paired host and handing
// the tools back; each item sends its request, handing back asks first, and
// a handed-back Mac does not offer its host.
func runHostAndHandBackFixtures() {
    var requests: [URLRequest] = []
    var completions: [(Data?, URLResponse?, Error?) -> Void] = []
    func views<T: NSView>(_ type: T.Type, in menu: NSMenu) -> [T] {
        func walk(_ view: NSView) -> [T] { ((view as? T).map { [$0] } ?? []) + view.subviews.flatMap(walk) }
        return menu.items.compactMap(\.view).flatMap(walk)
    }
    func json(_ request: URLRequest) -> [String: Any] {
        guard let body = request.httpBody,
              let object = try? JSONSerialization.jsonObject(with: body) as? [String: Any] else { return [:] }
        return object
    }
    func state(_ remote: RemoteInfo?, handedBack: Bool) -> AppState {
        AppState(accounts: [], order: [], hidden: [], version: "fixture-1", update: nil, menu_usage_bars: true,
            reset_notifications: true, remote: remote, handed_back: handedBack)
    }
    var confirmations = 0
    var runtime = MenuRuntime()
    runtime.request = { request, completion in requests.append(request); completions.append(completion) }
    runtime.deviceToken = { "fixture-device-token" }
    var askedHosts: [String?] = []
    runtime.confirmHandBack = { _, host in confirmations += 1; askedHosts.append(host); return confirmations > 1 }
    var hostAsks = 0
    runtime.confirmHostUpdate = { host, release in hostAsks += 1; return host == "Studio" && release == "1.22.0" }
    let delegate = AppDelegate(runtime: runtime)

    func advanced(_ menu: NSMenu) -> NSMenu {
        guard let submenu = menu.items.first(where: { $0.title == "Advanced" })?.submenu else { fail("Advanced submenu missing") }
        return submenu
    }
    func item(_ title: String, in menu: NSMenu) -> NSMenuItem {
        guard let found = advanced(menu).items.first(where: { $0.title == title }) else {
            fail("Advanced item \(title) missing: \(advanced(menu).items.map(\.title))")
        }
        return found
    }
    func choose(_ found: NSMenuItem) {
        guard let action = found.action else { fail("\(found.title) has no action") }
        _ = (found.target as AnyObject?)?.perform(action, with: found)
    }

    delegate.applyMenuState(state(nil, handedBack: false))
    let unpaired = views(NSTextField.self, in: delegate.menu).map(\.stringValue)
    guard unpaired.contains("Start at login"), unpaired.contains("Reset alerts"),
          !unpaired.contains(where: { $0.hasPrefix("Use ") || $0.hasPrefix("Hand back") }),
          advanced(delegate.menu).items.map(\.title) == ["Hand Back to Providers…"] else {
        fail("An unpaired Mac keeps only Start at login on show and offers handing back under Advanced: \(unpaired)")
    }

    delegate.applyMenuState(state(RemoteInfo(role: "client", host: "Studio", connected: false), handedBack: false))
    let useHost = item("Use Studio's accounts", in: delegate.menu)
    guard useHost.state == .on, useHost.isEnabled, advanced(delegate.menu).items.last?.title == "Hand Back to Providers…" else {
        fail("A Mac using its host shows the host checked and offers handing back")
    }
    choose(useHost)
    guard let last = requests.last, last.url?.path == "/api/remote/use-host", last.httpMethod == "POST",
          json(last)["enabled"] as? Bool == false else {
        fail("Turning the host off must ask the server to work locally")
    }

    delegate.applyMenuState(state(RemoteInfo(role: "local", host: "Studio"), handedBack: false))
    guard item("Use Studio's accounts", in: delegate.menu).state == .off else { fail("A host not in use shows unchecked") }
    requests.removeAll(); completions.removeAll()
    choose(item("Hand Back to Providers…", in: delegate.menu))
    guard confirmations == 1, requests.isEmpty else { fail("Declining the question must hand nothing back") }
    choose(item("Hand Back to Providers…", in: delegate.menu))
    guard confirmations == 2, let handBack = requests.first(where: { $0.url?.path == "/api/handback" }),
          json(handBack)["enabled"] as? Bool == true else {
        fail("Handing back must ask, then post the hand-back")
    }

    // Handed back, the host waits: using it would contradict the hand-back.
    delegate.applyMenuState(state(RemoteInfo(role: "client", host: "Studio", connected: true), handedBack: true))
    let waiting = item("Use Studio's accounts", in: delegate.menu)
    guard !waiting.isEnabled, waiting.state == .off else { fail("A handed-back Mac must not offer or show its host in use") }
    requests.removeAll(); completions.removeAll()
    choose(item("Set Switcher Up Again…", in: delegate.menu))
    guard confirmations == 3, let setUp = requests.first(where: { $0.url?.path == "/api/handback" }),
          json(setUp)["enabled"] as? Bool == false else {
        fail("Setting up again must ask too, then post the setup")
    }
    guard askedHosts.first == "Studio" else { fail("The question must name the paired host it leaves alone") }
    let question = handBackConfirmation(on: true, host: "Studio")
    guard question.informativeText.hasPrefix("Only this Mac's Switcher changes; Studio's Switcher stays as it is."),
          handBackConfirmation(on: false, host: nil).messageText == "Set Switcher up again on this Mac?" else {
        fail("Both questions must say only this Mac changes: \(question.informativeText)")
    }
    // A Mac using its host offers to update the host's Switcher, after asking.
    delegate.applyMenuState(state(RemoteInfo(role: "client", host: "Studio", connected: true,
        host_update: HostUpdateInfo(current: "1.21.0", latest: "v1.22.0", update_available: true, remote_update: true)), handedBack: false))
    requests.removeAll(); completions.removeAll()
    guard let hostButton = views(NSButton.self, in: delegate.menu).first(where: { $0.title == "Update Studio to 1.22.0" }) else {
        fail("A Mac using its host must offer to update it")
    }
    hostButton.performClick(nil)
    guard hostAsks == 1, let hostUpdate = requests.first(where: { $0.url?.path == "/api/remote/host-update" }),
          json(hostUpdate)["action"] as? String == "install", hostUpdate.timeoutInterval >= 600 else {
        fail("Updating the host must ask, then ask this Mac's server to have the host install")
    }
    delegate.applyMenuState(state(RemoteInfo(role: "local", host: "Studio",
        host_update: HostUpdateInfo(latest: "v1.22.0", update_available: true, remote_update: true)), handedBack: false))
    guard !views(NSButton.self, in: delegate.menu).contains(where: { $0.title.hasPrefix("Update Studio") }) else {
        fail("Only a Mac using its host offers the host's update in the menu")
    }
    delegate.applyMenuState(state(RemoteInfo(role: "client", host: "Studio", connected: true,
        host_update: HostUpdateInfo(current: "1.19.0", latest: "v1.22.0", update_available: true, remote_update: false)), handedBack: false))
    guard !views(NSButton.self, in: delegate.menu).contains(where: { $0.title.hasPrefix("Update Studio") }) else {
        fail("A host too old to take updates from here must not be offered one")
    }
    print("Paired host and hand-back switches: OK")
}

// Live state: the menu waits for Switcher to say its state changed, reads it
// then, and falls back to its timers on an older server or errors.
func runLiveStateFixtures() {
    func response(_ status: Int) -> HTTPURLResponse {
        HTTPURLResponse(url: URL(string: "https://fixture.invalid/changes")!, statusCode: status,
            httpVersion: nil, headerFields: nil)!
    }
    func since(_ request: URLRequest) -> String? {
        guard request.url?.path == "/api/state/changes", let url = request.url,
              let items = URLComponents(url: url, resolvingAgainstBaseURL: false)?.queryItems else { return nil }
        return items.first(where: { $0.name == "since" })?.value
    }
    var requests: [URLRequest] = []
    var completions: [(Data?, URLResponse?, Error?) -> Void] = []
    var tasks: [(TimeInterval, () -> Void)] = []
    var runtime = MenuRuntime()
    runtime.request = { request, completion in requests.append(request); completions.append(completion) }
    runtime.schedule = { delay, task in tasks.append((delay, task)) }
    runtime.deviceToken = { "fixture-device-token" }
    func runTask(_ delay: TimeInterval) {
        guard let index = tasks.firstIndex(where: { $0.0 == delay }) else { fail("Missing scheduled task after \(delay)s") }
        tasks.remove(at: index).1()
    }
    let delegate = AppDelegate(runtime: runtime)
    delegate.watchStateChanges()
    guard requests.count == 1, since(requests[0]) == "", requests[0].timeoutInterval > 25,
          requests[0].value(forHTTPHeaderField: "Authorization") == "Bearer fixture-device-token" else {
        fail("The menu must wait for changes with its device token and a deadline past the server's wait")
    }
    completions[0](Data(#"{"etag":"tag-1","changed":true}"#.utf8), response(200), nil)
    guard requests.count == 3, requests[1].url?.path == "/api/state", since(requests[2]) == "tag-1" else {
        fail("A change must read the state and wait again from the new tag")
    }
    let state = AppState(accounts: [], order: [], hidden: [], version: "fixture-live",
        update: nil, menu_usage_bars: true, reset_notifications: false)
    completions[1](try! JSONEncoder().encode(state), response(200), nil)
    guard delegate.cachedState?.version == "fixture-live" else { fail("The read after a change must reach the cache") }
    completions[2](Data(#"{"etag":"tag-1","changed":false}"#.utf8), response(200), nil)
    guard requests.count == 4, since(requests[3]) == "tag-1" else {
        fail("A wait that timed out must wait again without reading the state")
    }
    completions[3](Data("404 page not found".utf8), response(404), nil)
    guard requests.count == 4, tasks.contains(where: { $0.0 == 300 }) else {
        fail("An older server must leave the menu to its timers for a while")
    }
    runTask(300)
    guard requests.count == 5, since(requests[4]) == "tag-1" else { fail("The route must be tried again later") }
    completions[4](nil, nil, URLError(.cannotConnectToHost))
    runTask(1)
    completions[5](Data("{}".utf8), response(500), nil)
    guard tasks.contains(where: { $0.0 == 2 }) else { fail("Errors must back off") }
    runTask(2)
    guard requests.count == 7 else { fail("The wait must resume after its backoff") }
    // After a wake a fresh wait replaces the one that may hang.
    delegate.watchStateChanges()
    completions[6](Data(#"{"etag":"stale","changed":true}"#.utf8), response(200), nil)
    guard requests.count == 8, delegate.liveEtag == "tag-1" else { fail("The replaced wait's answer must be ignored") }
    completions[7](Data(#"{"etag":"tag-2","changed":false}"#.utf8), response(200), nil)
    guard delegate.liveEtag == "tag-2", requests.count == 9 else { fail("The fresh wait must carry on") }
    print("Live state wait, older server and backoff: OK")
}

func runLoadAndLimitFixtures() {
    var requests: [URLRequest] = []
    var completions: [(Data?, URLResponse?, Error?) -> Void] = []
    func response(_ status: Int) -> HTTPURLResponse {
        HTTPURLResponse(url: URL(string: "https://fixture.invalid/load")!, statusCode: status,
            httpVersion: nil, headerFields: nil)!
    }
    func views<T: NSView>(_ type: T.Type, in menu: NSMenu) -> [T] {
        func walk(_ view: NSView) -> [T] { ((view as? T).map { [$0] } ?? []) + view.subviews.flatMap(walk) }
        return menu.items.compactMap(\.view).flatMap(walk)
    }
    func texts(in menu: NSMenu) -> [String] { views(NSTextField.self, in: menu).map(\.stringValue) }
    func itemIndex(_ text: String, in menu: NSMenu) -> Int? {
        menu.items.firstIndex { item in
            guard let view = item.view else { return false }
            func walk(_ v: NSView) -> Bool { ((v as? NSTextField)?.stringValue == text) || v.subviews.contains(where: walk) }
            return walk(view)
        }
    }
    func json(_ request: URLRequest) -> [String: Any] {
        guard let body = request.httpBody,
              let object = try? JSONSerialization.jsonObject(with: body) as? [String: Any] else { return [:] }
        return object
    }
    var runtime = MenuRuntime()
    runtime.request = { request, completion in requests.append(request); completions.append(completion) }
    runtime.deviceToken = { "fixture-device-token" }
    var errors: [String] = []
    runtime.showError = { title, message in errors.append(title + ": " + message) }
    let now = Date().timeIntervalSince1970
    let weekly = Usage(available: true, windows: [
        UsageWindow(label: "Session", used_percent: 20, resets_at: now + 3600),
        UsageWindow(label: "Weekly", used_percent: 92, resets_at: now + 4 * 86400)])
    var kept = Account(id: "claude-kept", provider: "claude", email: "kept@example.test", plan: "claude_max_20x",
        active: true, exhausted_until: nil, usage: weekly, reset_credits: nil)
    kept.usage_limit = 10
    kept.at_limit = true
    let free = Account(id: "claude-free", provider: "claude", email: "free@example.test", plan: "claude_max_20x",
        active: false, exhausted_until: nil, usage: weekly, reset_credits: nil)
    let sessionOnly = Account(id: "grok-1", provider: "grok", email: "s@example.test", plan: nil, active: true,
        exhausted_until: nil, usage: Usage(available: true, windows: [UsageWindow(label: "Session", used_percent: 1, resets_at: nil)]),
        reset_credits: nil)
    let awake = AwakeState(supported: true, active: false)
    func state(_ accounts: [Account], remote: RemoteInfo? = nil) -> AppState {
        AppState(accounts: accounts, order: ["claude", "grok"], hidden: [], version: "fixture-1", update: nil,
            menu_usage_bars: true, reset_notifications: false, awake: awake, remote: remote)
    }
    let delegate = AppDelegate(runtime: runtime)
    delegate.applyMenuState(state([kept, free, sessionOnly], remote: RemoteInfo(role: "client", host: "k12", connected: true)))

    // Usage limits: the account at its limit says so and marks its weekly bar.
    guard texts(in: delegate.menu).contains("At limit") else { fail("An account at its usage limit must say so: \(texts(in: delegate.menu))") }
    let buttons = views(LimitButton.self, in: delegate.menu)
    guard buttons.count == 2, buttons.filter(\.pinned).count == 1 else {
        fail("Accounts with a weekly window offer a limit (shown while one is set); a session-only account does not")
    }
    guard views(NSView.self, in: delegate.menu).contains(where: { $0.toolTip == "Switcher moves away at 10% left" }) else {
        fail("The weekly bar must mark where the limit moves Switcher away")
    }
    for row in views(ClickableRow.self, in: delegate.menu) {
        let labels = row.subviews.filter { $0 is NSTextField || $0 is BlurredLabel }
        for button in row.subviews.compactMap({ $0 as? LimitButton }) {
            guard labels.allSatisfy({ !$0.frame.intersects(button.frame) }) else { fail("The limit button overlaps the account's title") }
        }
    }
    guard let pinned = buttons.first(where: \.pinned) else { fail("No limit button for the limited account") }
    pinned.onClicked?()
    guard delegate.limitEditingID == "claude-kept" else { fail("The limit button must open the account's choices") }
    // The limit chips come first; Keep awake's follow further down.
    let chips = Array(views(ChipButton.self, in: delegate.menu).prefix(5))
    guard chips.map(\.title) == ["Off", "10%", "20%", "30%", "50%"], chips.first(where: { $0.title == "10%" })?.selected == true,
          texts(in: delegate.menu).contains("Switches away at 10% left, keeping it for the chat apps") else {
        fail("Limit choices are incomplete: \(chips.map(\.title))")
    }
    requests.removeAll(); completions.removeAll()
    chips.first(where: { $0.title == "20%" })?.onClicked?()
    guard requests.count == 1, requests[0].url?.path == "/api/accounts/claude-kept", requests[0].httpMethod == "PATCH",
          json(requests[0])["usage_limit"] as? Int == 20,
          requests[0].value(forHTTPHeaderField: "Authorization") == "Bearer fixture-device-token" else {
        fail("A limit chip must save the limit for its account")
    }
    completions.removeFirst()(Data(#"{"status":"ok"}"#.utf8), response(200), nil)
    guard delegate.limitEditingID == nil, texts(in: delegate.menu).contains("Switches away at 20% left · saved on k12") else {
        fail("A saved limit closes the choices and says where it was saved: \(texts(in: delegate.menu))")
    }
    pinned.onClicked?()
    delegate.menuDidClose(delegate.menu)
    guard delegate.limitEditingID == nil, views(ChipButton.self, in: delegate.menu).allSatisfy({ !["Off", "50%"].contains($0.title) }) else {
        fail("Open limit choices must close with the menu")
    }

    // The load card: three rings above Keep awake, the host's while its
    // accounts are used.
    requests.removeAll(); completions.removeAll()
    delegate.fetchLoadAsync()
    delegate.fetchLoadAsync()
    guard requests.count == 1, requests[0].url?.path == "/api/system", requests[0].httpMethod == "GET", delegate.loadInFlight else {
        fail("The load card must read /api/system, one reading at a time")
    }
    let gb = 1_073_741_824.0
    completions.removeFirst()(Data(#"{"machine":"k12","remote":true,"platform":"linux","available":true,"cpu":23,"cores":16,"memory":72,"memory_used":\#(22.9 * gb),"memory_total":\#(31.8 * gb),"gpu":94}"#.utf8), response(200), nil)
    guard !delegate.loadInFlight else { fail("An answered reading must let the next one go") }
    guard let host = itemIndex("k12", in: delegate.menu), let keepAwake = itemIndex("Keep awake", in: delegate.menu), host < keepAwake else {
        fail("The load card must sit above Keep awake: \(texts(in: delegate.menu))")
    }
    let gauges = views(LoadGaugeView.self, in: delegate.menu)
    guard gauges.map(\.kind) == ["cpu", "memory", "gpu"], gauges[0].value == 23, gauges[0].valueText == "23%",
          gauges[0].detailText == "16 cores", gauges[1].detailText == "22.9 of 31.8 GB", gauges[2].value == 94,
          abs(gauges[0].arcEnd - 0.23) < 0.001, texts(in: delegate.menu).contains("Host load") else {
        fail("Load rings are wrong: \(gauges.map { ($0.kind, $0.valueText, $0.detailText) })")
    }
    // The card opens the load page; the rings let the click through.
    guard let card = gauges[0].superview, gauges[0].hitTest(NSPoint(x: 5, y: 5)) == nil,
          let link = card.subviews.first as? ClickableRow, link.frame == card.bounds else {
        fail("The load card must be one link behind its rings")
    }
    link.onClicked?()
    guard delegate.openedLoadPage == 1 else { fail("Clicking the load card must open the load page") }
    // A new reading of the same shape moves the rings without a new card.
    var next = delegate.systemLoad!
    next.cpu = 64
    delegate.applyLoad(next)
    guard views(LoadGaugeView.self, in: delegate.menu).first === gauges[0], gauges[0].value == 64, abs(gauges[0].arcEnd - 0.64) < 0.001 else {
        fail("A new reading must move the existing rings")
    }
    next.gpu = nil
    delegate.applyLoad(next)
    guard let gpu = views(LoadGaugeView.self, in: delegate.menu).last, gpu.value == nil, gpu.detailText == "No reading", gpu.valueText == "–" else {
        fail("A missing graphics reading must say so")
    }
    delegate.applyLoad(SystemLoad(available: false, machine: "k12", remote: true, reason: "update"))
    guard views(LoadGaugeView.self, in: delegate.menu).isEmpty, texts(in: delegate.menu).contains("Update k12 to see its load") else {
        fail("An older host must be named with what to do")
    }
    delegate.applyLoad(SystemLoad(available: true, machine: "mac", remote: false, cpu: 5, cores: 8, memory: 40, gpu: 2))
    guard texts(in: delegate.menu).contains("This Mac"), views(LoadGaugeView.self, in: delegate.menu).count == 3 else {
        fail("This Mac's own load must be titled This Mac")
    }
    guard loadLevelColor(69) == accentColor, loadLevelColor(70) == warnColor, loadLevelColor(90) == strainedColor,
          gigabytesText(128 * gb) == "128", gigabytesText(16 * gb) == "16.0" else {
        fail("Load levels or sizes are wrong")
    }
    // An older server has no readings: the card goes.
    requests.removeAll(); completions.removeAll()
    delegate.fetchLoadAsync()
    completions.removeFirst()(Data("404 page not found".utf8), response(404), nil)
    guard delegate.systemLoad == nil, views(LoadGaugeView.self, in: delegate.menu).isEmpty, itemIndex("This Mac", in: delegate.menu) == nil else {
        fail("A server without load readings must show no card")
    }
    guard errors.isEmpty else { fail("Unexpected errors: \(errors)") }
    print("Load card and usage limits: OK")
}

@main
struct MenuBarLayoutTest {
    static func main() {
        _ = NSApplication.shared
        runMenuActionRegressionFixtures()
        runUpdateBadgeAndProgressFixtures()
        runKeepAwakeFixtures()
        runHostAndHandBackFixtures()
        runLiveStateFixtures()
        runLoadAndLimitFixtures()
        guard planNames["free"] == "Free" else { fail("Free accounts must not display a paid tier") }
        guard planLabel("go") == "Go", planLabel("plus") == "Plus", planLabel("team") == "Team",
              planLabel("business") == "Business", planLabel("self_serve_business") == "Self Serve Business",
              planLabel(nil) == nil, planLabel(" ") == nil else {
            fail("Codex plans must be named, and an unknown plan must still show")
        }
        let goAccount = Account(id: "codex-go", provider: "codex", email: "go@example.test", plan: "go",
            active: false, exhausted_until: nil, usage: nil, reset_credits: nil)
        func labels(_ view: NSView) -> [String] {
            ((view as? NSTextField).map { [$0.stringValue] } ?? []) + view.subviews.flatMap(labels)
        }
        guard labels(AppDelegate().providerCard(providerID: "codex", accounts: [goAccount],
            contentWidth: menuWidth - 2 * edgeInset, showUsageBars: true)).contains("· Go") else {
            fail("A Go account's row must show its plan")
        }
        guard planNames["claude_max_5x"] == "Max 5x",
              planNames["claude_max_20x"] == "Max 20x",
              planNames["claude_max"] == "Max" else { fail("Claude Max tiers must not be inferred from a generic max flag") }
        for phase in ["idle", "running", "success", "error"] {
            let head = sectionHead("claude", contentWidth: menuWidth - 2 * edgeInset, syncPhase: phase)
            guard let button = head.subviews.compactMap({ $0 as? ClaudeSyncButton }).first,
                  button.isEnabled == (phase != "running"),
                  button.toolTip?.contains("Sync Claude Code sessions between accounts") == true else {
                fail("Claude sync button missing or has incorrect state")
            }
            let labels = head.subviews.compactMap { $0 as? NSTextField }
            guard labels.allSatisfy({ !$0.frame.intersects(button.frame) && $0.frame.maxX < button.frame.minX }) else { fail("Claude sync button must follow its title without overlapping") }
        }
        let syncConfirmation = claudeSyncConfirmation()
        guard syncConfirmation.messageText == "Sync Claude Code sessions?",
              syncConfirmation.informativeText.contains("close and reopen Claude Desktop"),
              syncConfirmation.informativeText.contains("background"),
              syncConfirmation.buttons.map(\.title) == ["Cancel", "Sync sessions"],
              syncConfirmation.buttons[0].keyEquivalent == "\r",
              syncConfirmation.buttons[1].keyEquivalent.isEmpty else { fail("Sync confirmation must explain interruption and default to Cancel") }
        guard !sectionHead("codex", contentWidth: menuWidth - 2 * edgeInset).subviews.contains(where: { $0 is ClaudeSyncButton }) else {
            fail("Claude sync action appeared on another provider")
        }
        let now = Date().timeIntervalSince1970
        var cases: [[UsageWindow]] = [
            [UsageWindow(label: "Weekly", used_percent: 3,
                resets_at: now + 5 * 86400 + 3 * 3600)],
            [
                UsageWindow(label: "Go · Session", used_percent: 4,
                    resets_at: now + 4 * 3600),
                UsageWindow(label: "Go · Weekly", used_percent: 47,
                    resets_at: now + 3 * 86400 + 2 * 3600),
                UsageWindow(label: "Go · Monthly", used_percent: 65,
                    resets_at: now + 12 * 86400 + 8 * 3600),
            ],
        ]
        if let raw = ProcessInfo.processInfo.environment["SWITCHER_USAGE_WINDOWS"],
           let data = raw.data(using: .utf8),
           let live = try? JSONDecoder().decode([[UsageWindow]].self, from: data) {
            cases.append(contentsOf: live)
        }
        let contentWidth = menuWidth - 2 * edgeInset
        for windows in cases {
            let lineHeight: CGFloat = 15
            let rowHeight = CGFloat(9 + 15 + 4 + 8) + CGFloat(windows.count) * lineHeight
            let titleY = rowHeight - 9 - 15
            let textX = cardInset
            for showBars in [true, false] {
                let layout = usageLayout(windows: windows, titleY: titleY,
                    lineWidth: contentWidth - textX - cardInset,
                    contentWidth: contentWidth, textX: textX,
                    cardInset: cardInset, lineHeight: lineHeight, titleGap: 4,
                    showBars: showBars)
                for line in layout.lines {
                    guard line.value.width >= textWidth("100% left", font: NSFont.monospacedDigitSystemFont(ofSize: 11, weight: .regular)) else {
                        fail("Percentage value clipped")
                    }
                    guard !line.label.intersects(layout.checkmark) && !line.value.intersects(layout.checkmark) else {
                        fail("Checkmark overlaps a usage line")
                    }
                    if let reset = line.resetText, let frame = line.reset {
                        let needed = textWidth(reset, font: NSFont.systemFont(ofSize: 10.5))
                        guard frame.width >= needed else {
                            fail("Truncated reset: \(reset), need \(needed), got \(frame.width)")
                        }
                        guard frame.minX >= line.value.maxX + 5 && frame.maxX <= contentWidth - cardInset else {
                            fail("Reset overlaps value or escapes card inset")
                        }
                        guard !frame.intersects(layout.checkmark) else {
                            fail("Reset overlaps active checkmark")
                        }
                    } else if line.resetText != nil || line.reset != nil {
                        fail("Reset text and frame disagree")
                    }
                    if showBars {
                        guard let bar = line.bar, bar.minX >= line.label.maxX + 5,
                              line.value.minX >= bar.maxX + 5,
                              bar.maxY <= line.label.maxY else {
                            fail("Inline usage bar overlaps a label or percentage")
                        }
                    } else if line.bar != nil {
                        fail("Usage bar present when disabled")
                    }
                }
                guard layout.checkmark.minY >= titleY && layout.checkmark.maxY <= titleY + 15 else {
                    fail("Checkmark is outside the account title line")
                }
            }
        }
        let noReset = usageLayout(windows: [UsageWindow(label: "Fable", used_percent: 0, resets_at: nil)],
            titleY: 27, lineWidth: contentWidth - 2 * cardInset, contentWidth: contentWidth,
            textX: cardInset, cardInset: cardInset, lineHeight: 15, titleGap: 4, showBars: true)
        guard noReset.lines[0].reset == nil && noReset.lines[0].resetText == nil else {
            fail("A window without a reset timestamp acquired a countdown")
        }
        guard exactResetTime(nil) == nil, exactResetTime(now - 10) == nil,
              let exact = exactResetTime(now + 3600), exact.contains("Provider-reported reset"),
              exact.contains(" (UTC") else {
            fail("Exact reset hover lacks local time zone or appears for an absent reset")
        }
        let longLabel = usageLayout(windows: [UsageWindow(
            label: "Very long window label from upstream", used_percent: 0,
            resets_at: now + 12 * 86400)], titleY: 27,
            lineWidth: contentWidth - 2 * cardInset, contentWidth: contentWidth,
            textX: cardInset, cardInset: cardInset, lineHeight: 15, titleGap: 4,
            showBars: true)
        guard longLabel.lines[0].bar == nil &&
              longLabel.lines[0].value.width >= textWidth("100% left", font: NSFont.monospacedDigitSystemFont(ofSize: 11, weight: .regular)) else {
            fail("Long label did not preserve the percentage column")
        }
        let base = Account(id: "a", provider: "codex", email: "private@example.com", plan: nil,
            active: true, exhausted_until: nil, usage: Usage(available: true, windows: [
                UsageWindow(label: "Session", used_percent: 25, resets_at: now + 3600),
                UsageWindow(label: "Weekly", used_percent: 0, resets_at: now + 7200),
                UsageWindow(label: "Past", used_percent: 50, resets_at: now - 5),
                UsageWindow(label: "Unknown", used_percent: 50, resets_at: nil),
            ]), reset_credits: ResetCredits(count: 1))
        var nativeAccount = base
        nativeAccount.native_switch_available = true
        nativeAccount.native_active = false
        guard !nativeAccount.selected else { fail("Proxy selection was mislabeled as a native login") }
        nativeAccount.native_active = true
        guard nativeAccount.selected else { fail("Observed native selection was not displayed") }
        let nativeDelegate = AppDelegate()
        func fieldTexts(_ view: NSView) -> [String] {
            (view as? NSTextField).map { [$0.stringValue] } ?? view.subviews.flatMap(fieldTexts)
        }
        nativeAccount.native_active = false
        let proxyFields = fieldTexts(nativeDelegate.providerCard(providerID: "claude", accounts: [nativeAccount], contentWidth: contentWidth, showUsageBars: true))
        guard proxyFields.contains("Claude Code idle · Proxy active") else { fail("Native menu hid the independent proxy selection") }
        nativeAccount.native_active = true
        let bothFields = fieldTexts(nativeDelegate.providerCard(providerID: "claude", accounts: [nativeAccount], contentWidth: contentWidth, showUsageBars: true))
        guard bothFields.contains("Claude Code active · Proxy active") else { fail("Native menu conflated matching native/proxy selections") }
        guard bankedResetText(base.reset_credits) == "⚡ 1 banked",
              bankedResetText(nil) == nil,
              bankedResetText(ResetCredits(count: 0)) == nil else {
            fail("Banked reset count missing or shown for an empty balance")
        }
        func state(_ accounts: [Account], _ enabled: Bool) -> AppState {
            AppState(accounts: accounts, order: nil, hidden: nil, version: nil,
                update: nil, menu_usage_bars: nil, reset_notifications: enabled)
        }
        // A click reuses the prepared menu. Updating one provider preserves
        // the other provider's views and hover targets.
        let otherProvider = Account(id: "claude-fixture", provider: "claude", email: "fixture@example.test",
            plan: "claude_max_5x", active: false, exhausted_until: nil, usage: base.usage, reset_credits: nil)
        let delegate = AppDelegate()
        let initial = state([base, otherProvider], false)
        delegate.applyMenuState(initial)
        let items = delegate.menu.items.map(ObjectIdentifier.init)
        func providerViews() -> [NSView] {
            delegate.menu.items.compactMap(\.view).filter { container in
                container.subviews.contains { $0 is HoverCard && $0.subviews.contains(where: { $0 is ClickableRow }) }
            }
        }
        let cards = providerViews()
        guard cards.count == 2 else { fail("Missing provider fixture cards") }
        for _ in 0..<10 { delegate.prepareMenuForOpening() }
        delegate.applyMenuState(initial)
        guard delegate.menu.items.map(ObjectIdentifier.init) == items else { fail("Unchanged menu was rebuilt on open or poll") }
        let changedAccount = Account(id: base.id, provider: base.provider, email: base.email, plan: base.plan,
            active: base.active, exhausted_until: nil, usage: Usage(available: true,
                windows: [UsageWindow(label: "Session", used_percent: 7, resets_at: now + 3600)]), reset_credits: nil)
        delegate.applyMenuState(state([changedAccount, otherProvider], false))
        let nextCards = providerViews()
        guard nextCards[0] !== cards[0], nextCards[1] === cards[1] else { fail("Provider patch rebuilt unrelated cards") }
        delegate.applyMenuState(state([changedAccount, otherProvider], true))
        delegate.applyNotificationPermission(ResetAlertPermission(message: "Fixture permission granted", canSend: true))
        func subviews(_ view: NSView) -> [NSView] { [view] + view.subviews.flatMap(subviews) }
        let bottom = delegate.menu.items.compactMap(\.view).flatMap(subviews)
        guard bottom.contains(where: { ($0 as? NSTextField)?.stringValue == "Fixture permission granted" }),
              bottom.contains(where: { ($0 as? NSButton)?.title == "Send test" && ($0 as? NSButton)?.isEnabled == true }) else {
            fail("Permission change while closed left stale prepared controls")
        }
        let expiry = Date().timeIntervalSince1970 + 1
        let parked = Account(id: base.id, provider: base.provider, email: base.email, plan: base.plan,
            active: false, exhausted_until: expiry, usage: base.usage, reset_credits: nil)
        delegate.applyMenuState(state([parked, otherProvider], false))
        Thread.sleep(forTimeInterval: max(0, expiry - Date().timeIntervalSince1970 + 0.02))
        let changedOther = Account(id: otherProvider.id, provider: otherProvider.provider, email: otherProvider.email,
            plan: "claude_max_20x", active: false, exhausted_until: nil, usage: otherProvider.usage, reset_credits: nil)
        delegate.applyMenuState(state([parked, changedOther], false))
        func hasExpiredBadge(_ view: NSView) -> Bool {
            if let field = view as? NSTextField, field.stringValue == "Out of usage" { return true }
            return view.subviews.contains(where: hasExpiredBadge)
        }
        guard !providerViews().contains(where: hasExpiredBadge) else { fail("Unrelated provider patch lost an expired exhaustion deadline") }
        func field(_ text: String, in view: NSView) -> NSTextField? {
            if let field = view as? NSTextField, field.stringValue == text { return field }
            return view.subviews.lazy.compactMap { field(text, in: $0) }.first
        }
        var relogin = changedOther
        relogin.health = AccountHealth(condition: "needs_relogin")
        var stale = changedAccount
        stale.health = AccountHealth(condition: "usage_stale")
        delegate.applyMenuState(state([stale, relogin], false))
        guard providerViews().contains(where: { field("Sign in again", in: $0) != nil }),
              providerViews().contains(where: { field("Usage stale", in: $0) != nil }) else {
            fail("Accounts that cannot refresh or have old usage were shown as current")
        }
        let olderPoll = delegate.beginStateRequest()
        let newerNotificationCheck = delegate.beginStateRequest()
        guard delegate.acceptStateResponse(initial, serial: newerNotificationCheck),
              !delegate.acceptStateResponse(state([], false), serial: olderPoll),
              delegate.cachedState == initial else { fail("Old poll replaced a newer notification-state check") }
        let oldNotificationCheck = delegate.beginStateRequest()
        let newPoll = delegate.beginStateRequest()
        guard delegate.acceptStateResponse(initial, serial: newPoll),
              !delegate.acceptStateResponse(state([], false), serial: oldNotificationCheck) else {
            fail("Old notification-state check replaced a newer poll")
        }
        let alerts = desiredResetAlerts(state([base], true), now: Date(timeIntervalSince1970: now))
        guard alerts.map(\.window) == ["Session", "Weekly"], !alerts[0].id.contains("private") else {
            fail("Reset planner omitted a zero-used window or included past/unknown windows or private identity")
        }
        let unused = Account(id: "unused-codex", provider: "codex", email: "", plan: nil,
            active: false, exhausted_until: nil, usage: Usage(available: true,
                windows: [UsageWindow(label: "Weekly", used_percent: 0,
                    resets_at: now + 3600)]), reset_credits: nil)
        guard desiredResetAlerts(state([unused], true), now: Date(timeIntervalSince1970: now)).count == 1 else {
            fail("A zero-used Codex window reset without an opted-in alert")
        }
        guard resetAlertPermission(.authorized, alertSetting: .enabled,
                  alertStyle: .banner, centerSetting: .enabled).canSend,
              resetAlertPermission(.authorized, alertSetting: .disabled,
                  alertStyle: .none, centerSetting: .enabled).message.contains("Notification Center"),
              !resetAlertPermission(.authorized, alertSetting: .disabled,
                  alertStyle: .none, centerSetting: .disabled).canSend,
              !resetAlertPermission(.denied, alertSetting: .disabled,
                  alertStyle: .none, centerSetting: .disabled).canSend,
              resetAlertPermission(.provisional, alertSetting: .enabled,
                  alertStyle: .none, centerSetting: .enabled).message.contains("quietly") else {
            fail("macOS banner, quiet, or blocked delivery was reported incorrectly")
        }
        // The server can poll Codex again just as the reset rolls over. Its
        // new snapshot has a zeroed window and next week's reset timestamp,
        // before the menu's delivery timer has fired for the old timestamp.
        let before = Account(id: "codex-reset", provider: "codex", email: "private@example.com", plan: nil,
            active: true, exhausted_until: nil, usage: Usage(available: true,
                windows: [UsageWindow(label: "Weekly", used_percent: 25,
                    resets_at: now + 3600)]), reset_credits: nil)
        let planned = desiredResetAlerts(state([before], true), now: Date(timeIntervalSince1970: now))
        guard planned.count == 1 else { fail("Expected one Codex reset alert before rollover") }
        let rolled = Account(id: before.id, provider: "codex", email: "", plan: nil,
            active: true, exhausted_until: nil, usage: Usage(available: true,
                windows: [UsageWindow(label: "Weekly", used_percent: 0,
                    resets_at: now + 3600 + 7 * 86400)]), reset_credits: nil)
        let afterReset = Date(timeIntervalSince1970: now + 3602)
        let retained = reconciledResetAlerts(planned, state: state([rolled], true), now: afterReset)
        let afterDeliveryCheck = reconciledResetAlerts(retained, state: state([rolled], true),
            now: afterReset.addingTimeInterval(1))
        let rechecked = reconciledResetAlerts(
            reconciledResetAlerts(planned, state: state([before], true), now: afterReset),
            state: state([rolled], true), now: afterReset.addingTimeInterval(1))
        let afterSleep = reconciledResetAlerts(planned, state: state([rolled], true),
            now: afterReset.addingTimeInterval(5 * 60))
        let id = planned[0].id
        let survivesRefresh = retained.contains { $0.id == id }
        let survivesSecondCheck = afterDeliveryCheck.contains { $0.id == id } &&
            rechecked.contains { $0.id == id }
        let survivesSleep = afterSleep.contains { $0.id == id }
        guard survivesRefresh && survivesSecondCheck && survivesSleep else {
            fail("Codex reset alert lost: refresh=\(survivesRefresh), server recheck=\(survivesSecondCheck), delayed timer=\(survivesSleep)")
        }
        guard dueResetAlerts(retained, now: afterReset, delivered: [], sending: []).map(\.id) == [id],
              dueResetAlerts(retained, now: afterReset, delivered: [id], sending: []).isEmpty,
              dueResetAlerts(retained, now: afterReset, delivered: [], sending: [id]).isEmpty else {
            fail("Reset alert was not due exactly once")
        }
        let nextDay = afterReset.addingTimeInterval(12 * 3600)
        guard reconciledResetAlerts(planned, state: state([rolled], true), now: nextDay).contains(where: { $0.id == id }),
              dueResetAlerts(planned, now: nextDay, delivered: [], sending: []).count == 1,
              !reconciledResetAlerts(planned, state: state([rolled], true),
                  now: afterReset.addingTimeInterval(25 * 3600)).contains(where: { $0.id == id }) else {
            fail("Sleeping past a reset lost bounded catch-up or delivered a stale alert")
        }
        guard reconciledResetAlerts(planned, state: state([], true), now: afterReset).isEmpty,
              reconciledResetAlerts(planned, state: state([rolled], false), now: afterReset).isEmpty else {
            fail("Removed accounts or disabled notifications kept a reset alert")
        }
        let missingWindow = Account(id: before.id, provider: "codex", email: "", plan: nil,
            active: true, exhausted_until: nil, usage: Usage(available: true, windows: []), reset_credits: nil)
        guard reconciledResetAlerts(planned, state: state([missingWindow], true), now: afterReset).isEmpty else {
            fail("A removed usage window kept its reset alert")
        }
        let unavailable = Account(id: before.id, provider: "codex", email: "", plan: nil,
            active: true, exhausted_until: nil, usage: Usage(available: false, windows: nil), reset_credits: nil)
        guard reconciledResetAlerts(planned, state: state([unavailable], true), now: afterReset).map(\.id) == [id] else {
            fail("A temporary usage outage erased a scheduled reset")
        }
        let shortlyBefore = Date(timeIntervalSince1970: now + 3600 - 90)
        guard reconciledResetAlerts(planned, state: state([rolled], true), now: shortlyBefore).contains(where: { $0.id == id }),
              dueResetAlerts(planned, now: shortlyBefore, delivered: [], sending: []).isEmpty else {
            fail("A near-reset upstream rollover was not retained until its reported time")
        }
        let farBefore = Date(timeIntervalSince1970: now + 3600 - 10 * 60)
        guard !reconciledResetAlerts(planned, state: state([rolled], true), now: farBefore).contains(where: { $0.id == id }) else {
            fail("An old reset stayed scheduled after an early upstream change")
        }
        let corrected = Account(id: before.id, provider: "codex", email: "", plan: nil,
            active: true, exhausted_until: nil, usage: Usage(available: true,
                windows: [UsageWindow(label: "Weekly", used_percent: 25,
                    resets_at: now + 4000)]), reset_credits: nil)
        let revised = reconciledResetAlerts(planned, state: state([corrected], true),
            now: Date(timeIntervalSince1970: now + 10))
        guard revised.count == 1, revised[0].id != id else {
            fail("An upstream reset correction kept the obsolete alert")
        }
        let other = Account(id: "second-codex", provider: "codex", email: "", plan: nil,
            active: false, exhausted_until: nil, usage: Usage(available: true,
                windows: [UsageWindow(label: "Weekly", used_percent: 0,
                    resets_at: now + 3600)]), reset_credits: nil)
        let twoPlans = desiredResetAlerts(state([before, other], true), now: Date(timeIntervalSince1970: now))
        let otherRolled = Account(id: other.id, provider: "codex", email: "", plan: nil,
            active: false, exhausted_until: nil, usage: Usage(available: true,
                windows: [UsageWindow(label: "Weekly", used_percent: 0,
                    resets_at: now + 3600 + 7 * 86400)]), reset_credits: nil)
        let twoDue = dueResetAlerts(reconciledResetAlerts(twoPlans,
            state: state([rolled, otherRolled], true), now: afterReset),
            now: afterReset, delivered: [], sending: [])
        guard Set(twoDue.map(\.accountID)) == Set([before.id, other.id]),
              Set(twoDue.map(\.id)).count == 2 else {
            fail("Two Codex accounts resetting together did not produce separate alerts")
        }
        var pending = twoDue
        var deliveredIDs = Set<String>()
        recordDeliveredResetAlert(twoDue[0].id, alerts: &pending, delivered: &deliveredIDs)
        guard pending.map(\.id) == [twoDue[1].id],
              dueResetAlerts(pending, now: afterReset, delivered: deliveredIDs, sending: []).map(\.id) == [twoDue[1].id] else {
            fail("Delivering one account's alert erased or repeated another account's alert")
        }
        let ledgerURL = FileManager.default.temporaryDirectory
            .appendingPathComponent("switcher-reset-test-" + UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: ledgerURL) }
        let ledgerFile = ledgerURL.appendingPathComponent("reset-alerts.json")
        do {
            try FileManager.default.createDirectory(at: ledgerURL, withIntermediateDirectories: true)
            try saveResetAlertLedger(ResetAlertLedger(alerts: planned, deliveredIDs: []), at: ledgerFile)
            let restored = try loadResetAlertLedger(at: ledgerFile)
            guard reconciledResetAlerts(restored.alerts, state: state([rolled], true), now: afterReset).contains(where: { $0.id == id }),
                  restored.deliveredIDs.isEmpty else { fail("A menu restart lost the pending reset") }
            let raw = try Data(contentsOf: ledgerFile)
            let permissions = try FileManager.default.attributesOfItem(atPath: ledgerFile.path)[.posixPermissions] as? NSNumber
            guard !String(decoding: raw, as: UTF8.self).contains("private@example.com"),
                  permissions?.intValue == 0o600 else { fail("Reset ledger revealed identity or was not private") }
            try saveResetAlertLedger(ResetAlertLedger(alerts: retained, deliveredIDs: [id]), at: ledgerFile)
            let delivered = try loadResetAlertLedger(at: ledgerFile)
            guard dueResetAlerts(delivered.alerts, now: afterReset, delivered: Set(delivered.deliveredIDs),
                sending: []).isEmpty else { fail("A menu restart repeated a delivered notification") }
        } catch {
            fail("Reset ledger persistence failed: \(error)")
        }
        let shifted = Account(id: "a", provider: "codex", email: base.email, plan: nil,
            active: true, exhausted_until: nil, usage: Usage(available: true,
                windows: [UsageWindow(label: "Session", used_percent: 25, resets_at: now + 3700)]), reset_credits: nil)
        let newAlerts = desiredResetAlerts(state([shifted], true), now: Date(timeIntervalSince1970: now))
        guard newAlerts.count == 1, newAlerts[0].id != alerts[0].id,
              desiredResetAlerts(state([], true)).isEmpty,
              desiredResetAlerts(state([base], false)).isEmpty else {
            fail("Reset alert shift, deletion, or disable planning failed")
        }
        let many = (0..<40).map { i in
            Account(id: "account-\(i)", provider: "codex", email: "", plan: nil,
                active: false, exhausted_until: nil, usage: Usage(available: true,
                    windows: [UsageWindow(label: "Session", used_percent: 1, resets_at: now + Double(40-i)*60)]), reset_credits: nil)
        }
        let capped = desiredResetAlerts(state(many, true), now: Date(timeIntervalSince1970: now))
        guard capped.count == 32, capped[0].id.contains("account-39") else {
            fail("Reset planner did not choose the earliest 32 windows")
        }
        guard logoUsesTemplate("copilot"), logoUsesTemplate("grok"), !logoUsesTemplate("gemini"),
              let copilot = NSImage(contentsOfFile: "build/macos/logos/copilot.svg") else {
            fail("Copilot logo must load as a tintable template")
        }
        copilot.isTemplate = logoUsesTemplate("copilot")
        func logoBrightness(_ tint: NSColor) -> CGFloat {
            let view = NSImageView(frame: NSRect(x: 0, y: 0, width: 22, height: 22))
            view.image = copilot
            view.contentTintColor = tint
            guard let rep = renderBitmap(view) else { fail("Copilot mark did not render") }
            var total: CGFloat = 0
            var pixels = 0
            for y in 0..<rep.pixelsHigh {
                for x in 0..<rep.pixelsWide {
                    guard let color = rep.colorAt(x: x, y: y)?.usingColorSpace(.deviceRGB),
                          color.alphaComponent > 0.5 else { continue }
                    total += (color.redComponent + color.greenComponent + color.blueComponent) / 3
                    pixels += 1
                }
            }
            guard pixels > 0 else { fail("Copilot mark has no visible pixels") }
            return total / CGFloat(pixels)
        }
        guard logoBrightness(Palette.dark.ink) > logoBrightness(Palette.light.ink) + 0.35 else {
            fail("Copilot mark did not follow light and dark menu colours")
        }
        print("Menu bar layout, reset alerts, and Copilot theme tint: OK")
    }
}
