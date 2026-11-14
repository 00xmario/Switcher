// Install Switcher: a small native window around the installer's web pages
// (ui/), and the installer itself, which copies the Switcher.app it carries
// into Applications the way Switcher's own updater does: copy beside the
// destination, check the copy, then swap it in, so a failed install leaves the
// Switcher already there untouched.
//
// macOS asks once before this installer opens, because Switcher is not
// notarized. Having been opened, it clears the download flag on the app it
// installs, so Switcher itself opens without asking.
import AppKit
import Darwin
import WebKit

let switcherBundleID = "sh.switcher.app"
let launchAgentLabel = "sh.switcher.app"
let repoURL = URL(string: "https://github.com/00xmario/switcher#readme")!
let dashboardURL = URL(string: "http://127.0.0.1:8787/")!

// MARK: - Running commands

struct CommandResult {
    let status: Int32
    let out: String
    let err: String
    var ok: Bool { status == 0 }
    var text: String { [out, err].map { $0.trimmingCharacters(in: .whitespacesAndNewlines) }.filter { !$0.isEmpty }.joined(separator: "\n") }
}

/// Runs a program with arguments, never through a shell, and gives up after
/// the timeout. Output is read as it arrives, so a chatty program cannot
/// fill its pipe and stall.
func run(_ path: String, _ args: [String], env: [String: String]? = nil, timeout: TimeInterval = 60) -> CommandResult {
    let p = Process()
    p.executableURL = URL(fileURLWithPath: path)
    p.arguments = args
    if let env = env { p.environment = env }
    let outPipe = Pipe(), errPipe = Pipe()
    p.standardOutput = outPipe
    p.standardError = errPipe
    p.standardInput = FileHandle.nullDevice
    var out = Data(), err = Data()
    let group = DispatchGroup()
    group.enter()
    DispatchQueue.global().async { out = outPipe.fileHandleForReading.readDataToEndOfFile(); group.leave() }
    group.enter()
    DispatchQueue.global().async { err = errPipe.fileHandleForReading.readDataToEndOfFile(); group.leave() }
    let done = DispatchSemaphore(value: 0)
    p.terminationHandler = { _ in done.signal() }
    do {
        try p.run()
    } catch {
        try? outPipe.fileHandleForWriting.close()
        try? errPipe.fileHandleForWriting.close()
        group.wait()
        return CommandResult(status: -1, out: "", err: "\(path): \(error.localizedDescription)")
    }
    if done.wait(timeout: .now() + timeout) == .timedOut {
        p.terminate()
        _ = done.wait(timeout: .now() + 3)
        if p.isRunning { kill(p.processIdentifier, SIGKILL) }
        group.wait()
        return CommandResult(status: -2, out: String(decoding: out, as: UTF8.self), err: "\((path as NSString).lastPathComponent) did not finish within \(Int(timeout)) seconds")
    }
    group.wait()
    return CommandResult(status: p.terminationStatus, out: String(decoding: out, as: UTF8.self), err: String(decoding: err, as: UTF8.self))
}

// MARK: - Versions and bundles

func bundleVersion(_ app: URL) -> String? {
    let plist = app.appendingPathComponent("Contents/Info.plist")
    guard let data = try? Data(contentsOf: plist),
          let dict = try? PropertyListSerialization.propertyList(from: data, format: nil) as? [String: Any] else { return nil }
    guard let version = dict["CFBundleShortVersionString"] as? String,
          version.range(of: #"^[0-9]+(\.[0-9]+){0,3}$"#, options: .regularExpression) != nil else { return nil }
    return version
}

func bundleID(_ app: URL) -> String? {
    let plist = app.appendingPathComponent("Contents/Info.plist")
    guard let data = try? Data(contentsOf: plist),
          let dict = try? PropertyListSerialization.propertyList(from: data, format: nil) as? [String: Any] else { return nil }
    return dict["CFBundleIdentifier"] as? String
}

/// compareVersions orders dotted release numbers numerically: 1.9 < 1.10.
func compareVersions(_ a: String, _ b: String) -> Int {
    let x = a.split(separator: ".").map { Int($0) ?? 0 }
    let y = b.split(separator: ".").map { Int($0) ?? 0 }
    for i in 0..<max(x.count, y.count) {
        let l = i < x.count ? x[i] : 0, r = i < y.count ? y[i] : 0
        if l != r { return l < r ? -1 : 1 }
    }
    return 0
}

/// The download flag macOS puts on everything that came from a browser.
let quarantineAttribute = "com.apple.quarantine"

/// clearQuarantine removes the download flag from every file in the tree,
/// links included, and reports any file that still carries it.
func clearQuarantine(_ root: URL) -> [String] {
    var left: [String] = []
    func clear(_ path: String) {
        if removexattr(path, quarantineAttribute, XATTR_NOFOLLOW) != 0 && errno != ENOATTR {
            left.append(path)
        } else if getxattr(path, quarantineAttribute, nil, 0, 0, XATTR_NOFOLLOW) >= 0 {
            left.append(path)
        }
    }
    clear(root.path)
    if let walker = FileManager.default.enumerator(atPath: root.path) {
        for case let rel as String in walker { clear(root.appendingPathComponent(rel).path) }
    }
    return left
}

func shellQuote(_ s: String) -> String { "'" + s.replacingOccurrences(of: "'", with: "'\\''") + "'" }
func appleScriptEscape(_ s: String) -> String { s.replacingOccurrences(of: "\\", with: "\\\\").replacingOccurrences(of: "\"", with: "\\\"") }

// MARK: - Switcher's processes

/// A running Switcher process: the menu bar app or its server. It is found
/// by the path of the executable the kernel runs, not by its arguments, and
/// known by its process ID and start time, so a process ID reused by another
/// program is never signalled.
struct SwitcherProcess: Hashable {
    let pid: pid_t
    let started: UInt64
}

private func executablePath(_ pid: pid_t) -> String? {
    var buf = [CChar](repeating: 0, count: Int(MAXPATHLEN) * 4)
    guard proc_pidpath(pid, &buf, UInt32(buf.count)) > 0 else { return nil }
    return String(cString: buf)
}

/// startTime is when the process started, in microseconds; nil once it has
/// exited, zombies included (status 5, SZOMB).
private func startTime(_ pid: pid_t) -> UInt64? {
    var info = proc_bsdinfo()
    let size = Int32(MemoryLayout<proc_bsdinfo>.size)
    guard proc_pidinfo(pid, PROC_PIDTBSDINFO, 0, &info, size) == size, info.pbi_status != 5 else { return nil }
    return info.pbi_start_tvsec &* 1_000_000 &+ info.pbi_start_tvusec
}

/// arguments reads a process's command line (KERN_PROCARGS2).
private func arguments(_ pid: pid_t) -> [String]? {
    var mib: [Int32] = [CTL_KERN, KERN_PROCARGS2, pid]
    var size = 0
    guard sysctl(&mib, 3, nil, &size, nil, 0) == 0, size > 4 else { return nil }
    var buf = [UInt8](repeating: 0, count: size)
    guard sysctl(&mib, 3, &buf, &size, nil, 0) == 0, size > 4 else { return nil }
    let argc = Int(buf.withUnsafeBytes { $0.loadUnaligned(as: Int32.self) })
    var i = 4
    while i < size && buf[i] != 0 { i += 1 }  // the executable path
    while i < size && buf[i] == 0 { i += 1 }  // its padding
    var args: [String] = []
    while args.count < argc && i < size {
        let start = i
        while i < size && buf[i] != 0 { i += 1 }
        args.append(String(decoding: buf[start..<i], as: UTF8.self))
        i += 1
    }
    return args
}

/// switcherProcesses lists this user's running Switcher menu bar apps and
/// servers, from any copy of the app: an executable named Switcher, or
/// SwitcherServer serving (no command, a flag, or "serve"), in the
/// Contents/MacOS of a bundle whose identifier is Switcher's. A switcher
/// command someone is running, which is the same binary, is left alone.
func switcherProcesses() -> Set<SwitcherProcess> {
    var pids = [pid_t](repeating: 0, count: 8192)
    let n = proc_listallpids(&pids, Int32(pids.count * MemoryLayout<pid_t>.size))
    guard n > 0 else { return [] }
    var found = Set<SwitcherProcess>()
    for pid in pids.prefix(Int(n)) where pid > 0 && pid != getpid() {
        // Only processes this user may signal.
        guard kill(pid, 0) == 0, let path = executablePath(pid),
              let r = path.range(of: ".app/Contents/MacOS/", options: .backwards) else { continue }
        let name = path[r.upperBound...]
        guard name == "Switcher" || name == "SwitcherServer" else { continue }
        guard bundleID(URL(fileURLWithPath: String(path[..<r.lowerBound]) + ".app")) == switcherBundleID else { continue }
        if name == "SwitcherServer", let args = arguments(pid), args.count > 1 {
            let first = args[1]
            guard first.hasPrefix("-") || first == "serve" else { continue }
        }
        guard let started = startTime(pid) else { continue }
        found.insert(SwitcherProcess(pid: pid, started: started))
    }
    return found
}

/// alive is false once the process has exited, or its ID now belongs to
/// another process.
private func alive(_ p: SwitcherProcess) -> Bool { kill(p.pid, 0) == 0 && startTime(p.pid) == p.started }

// MARK: - The installer

struct InstallOptions {
    var login = true
    var cli = true
    var skill = true
    var launch = true
    var cliDir: String? = nil
}

struct InstallError: Error {
    var title = "Switcher was not installed"
    let message: String
    var detail = ""
    /// changed is true once the new app is in place; until then the Mac is
    /// as it was, and the Switcher that ran still runs.
    var changed = false
    var calm = ""
}

struct Existing {
    let url: URL
    let version: String
}

final class Installer {
    let payload: URL
    let version: String
    let home: URL
    let candidates: [URL]
    /// Extra environment for the commands the installer runs (a test home).
    let commandEnv: [String: String]
    private(set) var existing: Existing?
    private(set) var running = false

    init(payload: URL, home: URL, candidates: [URL], commandEnv: [String: String] = [:]) {
        self.payload = payload
        self.version = bundleVersion(payload) ?? "?"
        self.home = home
        self.candidates = candidates
        self.commandEnv = commandEnv
        survey()
    }

    /// survey finds the installed Switcher: of the copies in the candidate
    /// folders, the newest is the one to update (an older one elsewhere is
    /// left as it is), so the installer never downgrades the one in use.
    func survey() {
        existing = nil
        for dir in candidates {
            let app = dir.appendingPathComponent("Switcher.app")
            guard bundleID(app) == switcherBundleID else { continue }
            // A Switcher whose version cannot be read (a development build)
            // counts as older than any release.
            let found = Existing(url: app, version: bundleVersion(app) ?? "0")
            if existing == nil || compareVersions(found.version, existing!.version) > 0 { existing = found }
        }
        running = !switcherProcesses().isEmpty
    }

    /// relation says what installing would do: a fresh install, an update,
    /// the same version again, or a downgrade, which the installer refuses.
    var relation: String {
        guard let e = existing else { return "fresh" }
        switch compareVersions(e.version, version) {
        case ..<0: return "update"
        case 0: return "same"
        default: return "newer"
        }
    }

    /// destination is where Switcher.app goes: over the one already there,
    /// else the first Applications folder this user may write.
    var destination: URL {
        if let e = existing { return e.url }
        let parent = candidates.first { FileManager.default.isWritableFile(atPath: $0.path) } ?? candidates.last!
        return parent.appendingPathComponent("Switcher.app")
    }

    var launchAgentURL: URL { home.appendingPathComponent("Library/LaunchAgents/\(launchAgentLabel).plist") }
    var loginEnabled: Bool { FileManager.default.fileExists(atPath: launchAgentURL.path) }

    func environment() -> [String: Any] {
        let dest = destination
        let parent = dest.deletingLastPathComponent()
        var env: [String: Any] = [
            "version": version,
            "relation": relation,
            "destination": parent.path,
            "destinationName": parent.path == home.appendingPathComponent("Applications").path ? "Applications in your home folder" : parent.lastPathComponent,
            "login": existing == nil ? true : loginEnabled,
            "running": running,
        ]
        if let e = existing { env["existing"] = ["version": e.version, "path": e.url.path] as [String: Any] }
        return env
    }

    /// plan lists the steps in the order they run. The running Switcher is
    /// stopped only after the new one is in place, and the new one starts
    /// right after, before the set-up steps.
    func plan(_ o: InstallOptions) -> [String] {
        var steps = ["copy", "check", "place"]
        if running { steps.append("stop") }
        if o.launch { steps.append("launch") }
        if o.cli { steps.append("cli") }
        if o.skill && relation == "fresh" { steps.append("skill") }
        if o.login || loginEnabled { steps.append("login") }
        return steps
    }

    private func commandEnvironment() -> [String: String] {
        var env = ProcessInfo.processInfo.environment
        // An app opened from Finder gets launchd's short PATH. The folders a
        // login shell adds come from /etc/paths, and Homebrew's when it is
        // installed, so the switcher command's "on your PATH" answer matches
        // what Terminal will see.
        var path = (env["PATH"] ?? "/usr/bin:/bin:/usr/sbin:/sbin").split(separator: ":").map(String.init)
        var files = ["/etc/paths"]
        if let extra = try? FileManager.default.contentsOfDirectory(atPath: "/etc/paths.d") { files += extra.sorted().map { "/etc/paths.d/" + $0 } }
        var dirs: [String] = []
        for f in files {
            for line in ((try? String(contentsOfFile: f, encoding: .utf8)) ?? "").split(separator: "\n") {
                dirs.append(line.trimmingCharacters(in: .whitespaces))
            }
        }
        if FileManager.default.isExecutableFile(atPath: "/opt/homebrew/bin/brew") { dirs.append("/opt/homebrew/bin") }
        for dir in dirs where !dir.isEmpty && !path.contains(dir) { path.append(dir) }
        env["PATH"] = path.joined(separator: ":")
        for (k, v) in commandEnv { env[k] = v }
        return env
    }

    // swiftlint:disable:next function_body_length
    func install(_ o: InstallOptions, report: @escaping (String, String, String?) -> Void) throws -> [String: Any] {
        let fm = FileManager.default
        if relation == "newer" {
            throw InstallError(title: "You already have a newer Switcher", message: "This Mac has Switcher \(existing!.version); this installer has \(version).")
        }
        guard bundleID(payload) == switcherBundleID, version != "?" else {
            throw InstallError(message: "This installer does not contain a complete Switcher.", detail: payload.path)
        }
        let steps = plan(o)
        let dest = destination
        let parent = dest.deletingLastPathComponent()
        // Something else called Switcher.app is never replaced.
        if existing == nil && fm.fileExists(atPath: dest.path) {
            throw InstallError(message: "\(parent.path) already holds an app called Switcher.app that is not Switcher. Move it elsewhere, then try again.")
        }
        let env = commandEnvironment()
        // ~/Applications may not exist yet; made first, it is this user's own
        // and needs no administrator.
        if !fm.fileExists(atPath: parent.path) {
            do { try fm.createDirectory(at: parent, withIntermediateDirectories: true) } catch {
                throw InstallError(message: "Could not create \(parent.path).", detail: error.localizedDescription)
            }
        }
        let canWrite = fm.isWritableFile(atPath: parent.path) && (!fm.fileExists(atPath: dest.path) || fm.isWritableFile(atPath: dest.path))

        // 1. Copy beside the destination, so the swap is a rename on the same
        // disk. Where this user cannot write, the copy waits in a private
        // temporary folder for the administrator step.
        report("copy", "active", nil)
        let stageParent = canWrite ? parent : URL(fileURLWithPath: NSTemporaryDirectory())
        var template = Array(stageParent.appendingPathComponent(".switcher-install-XXXXXX").path.utf8CString)
        guard let made = mkdtemp(&template) else {
            throw InstallError(message: "Could not prepare the install in \(stageParent.path).", detail: String(cString: strerror(errno)))
        }
        let stage = URL(fileURLWithPath: String(cString: made))
        defer { try? fm.removeItem(at: stage) }
        let staged = stage.appendingPathComponent("Switcher.app")
        // --noqtn: the copy does not inherit this download's flag.
        let copy = run("/usr/bin/ditto", ["--noqtn", payload.path, staged.path], timeout: 300)
        guard copy.ok else { throw InstallError(message: "Could not copy Switcher.", detail: copy.text) }
        let flagged = clearQuarantine(staged)
        guard flagged.isEmpty else {
            throw InstallError(message: "macOS's download flag could not be cleared from the copy.", detail: flagged.prefix(5).joined(separator: "\n"))
        }
        report("copy", "done", nil)

        // 2. The copy is what will run: check its version, its seal and that
        // its server starts, as the updater does.
        report("check", "active", nil)
        guard bundleVersion(staged) == version else {
            throw InstallError(message: "The copied app is not Switcher \(version).", detail: staged.path)
        }
        let seal = run("/usr/bin/codesign", ["--verify", "--deep", "--strict", staged.path], timeout: 120)
        guard seal.ok else {
            throw InstallError(message: "The copy of Switcher in this installer did not pass its check, so it was not installed.", detail: seal.text)
        }
        let smoke = run(staged.appendingPathComponent("Contents/MacOS/SwitcherServer").path, ["version"], env: env, timeout: 20)
        guard smoke.ok, smoke.out.trimmingCharacters(in: .whitespacesAndNewlines) == "switcher \(version)" else {
            throw InstallError(message: "Switcher \(version) did not start on this Mac.", detail: smoke.text)
        }
        report("check", "done", nil)

        // 3. Swap the new app in while the old one keeps running, as the
        // updater does: its processes keep their open files. Whatever goes
        // wrong here, the Switcher in use is still running.
        var toStop = switcherProcesses()
        report("place", "active", nil)
        var aside: URL?
        if canWrite {
            aside = try swap(staged, into: dest)
        } else {
            try adminSwap(staged, into: dest)
        }
        // The old app's folder goes once its processes are gone; no copy of
        // it is kept, whatever happens next.
        defer { if let a = aside { try? fm.removeItem(at: a) } }
        report("place", "done", nil)
        existing = Existing(url: dest, version: version)
        // A Switcher started meanwhile, during a password prompt say, or a
        // server the old menu bar app restarted, stops too.
        toStop.formUnion(switcherProcesses())

        var result: [String: Any] = ["version": version, "path": dest.path]
        var warnings: [String] = []
        let server = dest.appendingPathComponent("Contents/MacOS/SwitcherServer").path

        // From here on Switcher is installed; what follows is set-up, and a
        // step that fails is reported without undoing the install.
        // 4. Stop every Switcher that ran before, from any copy: one Switcher
        // serves this user, and the new one starts only once they are gone.
        var stopped = true
        if steps.contains("stop") || !toStop.isEmpty {
            report("stop", "active", nil)
            stopped = stop(Array(toStop))
            report("stop", stopped ? "done" : "failed", nil)
            if !stopped {
                warnings.append("The previous Switcher did not quit and still runs. Quit it from its menu, then open Switcher from Applications.")
            }
        }
        if steps.contains("launch") {
            report("launch", "active", nil)
            if stopped {
                let (ok, warning) = launch(dest, server: server, env: env, after: Set(toStop.map(\.pid)))
                result["running"] = ok
                if let w = warning { warnings.append(w) }
                report("launch", ok ? "done" : "failed", nil)
            } else {
                result["running"] = false
                report("launch", "skipped", nil)
            }
        }
        if steps.contains("cli") {
            report("cli", "active", nil)
            var args = ["install-cli", "--json"]
            if let dir = o.cliDir { args += ["--dir", dir] }
            let r = run(server, args, env: env, timeout: 30)
            if r.ok, let data = r.out.data(using: .utf8),
               let json = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
               let path = json["path"] as? String {
                let dir = (path as NSString).deletingLastPathComponent
                let onPath = json["on_path"] as? Bool ?? false
                let shown = dir.hasPrefix(home.path + "/") ? "~" + dir.dropFirst(home.path.count) : dir
                result["cli"] = ["path": path, "dir": shown, "onPath": onPath,
                                 "hint": "echo 'export PATH=\"\(dir):$PATH\"' >> ~/.zshrc"] as [String: Any]
                report("cli", "done", shown)
            } else {
                warnings.append("The switcher command was not added: " + (r.text.isEmpty ? "unknown error" : r.text))
                report("cli", "failed", nil)
            }
        }
        if steps.contains("skill") {
            report("skill", "active", nil)
            let r = run(server, ["skill", "install", "--on", "here"], env: env, timeout: 30)
            result["skill"] = r.ok
            if !r.ok { warnings.append("The skill for your agents was not added; run: switcher skill install") }
            report("skill", r.ok ? "done" : "failed", nil)
        }
        if steps.contains("login") {
            report("login", "active", nil)
            do {
                try setLogin(o.login, app: dest)
                result["login"] = o.login
                report("login", o.login ? "done" : "skipped", o.login ? nil : "Off")
            } catch {
                warnings.append("Open at login was not set: \(error.localizedDescription)")
                report("login", "failed", nil)
            }
        }
        result["warnings"] = warnings
        return result
    }

    /// stop ends the given processes the way Switcher's updater does:
    /// SIGTERM, which the menu bar app and the server handle by shutting down
    /// cleanly, and SIGKILL only for one still there after ten seconds.
    /// Signals need no permission prompt, unlike asking an app to quit.
    private func stop(_ processes: [SwitcherProcess]) -> Bool {
        for p in processes where alive(p) { kill(p.pid, SIGTERM) }
        if waitGone(processes, seconds: 10) { return true }
        for p in processes where alive(p) { kill(p.pid, SIGKILL) }
        return waitGone(processes, seconds: 5)
    }

    private func waitGone(_ processes: [SwitcherProcess], seconds: Int) -> Bool {
        for _ in 0..<(seconds * 4) {
            if !processes.contains(where: alive) { return true }
            usleep(250_000)
        }
        return !processes.contains(where: alive)
    }

    /// swap renames the new app into place and returns where the old one
    /// went, for removal once it has stopped.
    private func swap(_ staged: URL, into dest: URL) throws -> URL? {
        let fm = FileManager.default
        // The old app moves aside in its own folder and is removed once the
        // new one is in place; no copy of it is kept.
        let aside = dest.deletingLastPathComponent().appendingPathComponent(".Switcher.app.previous")
        if fm.fileExists(atPath: aside.path) {
            do { try fm.removeItem(at: aside) } catch {
                throw InstallError(message: "Could not clear \(aside.path).", detail: error.localizedDescription)
            }
        }
        let had = fm.fileExists(atPath: dest.path)
        if had {
            guard rename(dest.path, aside.path) == 0 else {
                throw InstallError(message: "Could not move the current Switcher aside.", detail: String(cString: strerror(errno)))
            }
        }
        if rename(staged.path, dest.path) != 0 {
            let why = String(cString: strerror(errno))
            if had && rename(aside.path, dest.path) != 0 {
                throw InstallError(message: "The new Switcher could not be put in place, and the previous one is at \(aside.path); move it back to \(dest.path).",
                                   detail: why, changed: true, calm: "The Switcher you had is still running.")
            }
            throw InstallError(message: "The new Switcher could not be put in place.", detail: why)
        }
        return had ? aside : nil
    }

    /// adminSwap installs into a folder only an administrator may change.
    /// macOS shows its own password prompt; the installer never sees the
    /// password. The Switcher in use keeps running meanwhile.
    private func adminSwap(_ staged: URL, into dest: URL) throws {
        let next = dest.path + ".switcher-new"
        let aside = dest.deletingLastPathComponent().appendingPathComponent(".Switcher.app.previous").path
        let q = shellQuote
        let script = """
            /bin/rm -rf \(q(next)) \(q(aside)) || exit 1
            /usr/bin/ditto \(q(staged.path)) \(q(next)) || { /bin/rm -rf \(q(next)); exit 1; }
            if [ -e \(q(dest.path)) ]; then /bin/mv \(q(dest.path)) \(q(aside)) || { /bin/rm -rf \(q(next)); exit 1; }; fi
            if ! /bin/mv \(q(next)) \(q(dest.path)); then
              if [ -e \(q(aside)) ] && ! /bin/mv \(q(aside)) \(q(dest.path)); then echo SWITCHER_RESTORE_FAILED >&2; exit 3; fi
              /bin/rm -rf \(q(next)); exit 1
            fi
            /bin/rm -rf \(q(aside))
            """
        let apple = "do shell script \"\(appleScriptEscape(script))\" with prompt \"Install Switcher needs an administrator to put Switcher in \(appleScriptEscape(dest.deletingLastPathComponent().lastPathComponent)).\" with administrator privileges"
        let r = run("/usr/bin/osascript", ["-e", apple], timeout: 300)
        if !r.ok {
            if r.text.contains("SWITCHER_RESTORE_FAILED") {
                throw InstallError(message: "The new Switcher could not be put in place, and the previous one is at \(aside); move it back to \(dest.path).",
                                   detail: r.text, changed: true, calm: "The Switcher you had is still running.")
            }
            if r.text.contains("-128") { throw InstallError(message: "The administrator password was not entered.") }
            throw InstallError(message: "Switcher could not be put in place.", detail: r.text)
        }
    }

    /// setLogin writes or removes the same login item Switcher's own menu
    /// writes, pointing at the installed app.
    func setLogin(_ on: Bool, app: URL) throws {
        let fm = FileManager.default
        if !on {
            if fm.fileExists(atPath: launchAgentURL.path) { try fm.removeItem(at: launchAgentURL) }
            return
        }
        try fm.createDirectory(at: launchAgentURL.deletingLastPathComponent(), withIntermediateDirectories: true)
        let plist: [String: Any] = [
            "Label": launchAgentLabel,
            "ProgramArguments": [app.appendingPathComponent("Contents/MacOS/Switcher").path],
            "RunAtLoad": true,
        ]
        let data = try PropertyListSerialization.data(fromPropertyList: plist, format: .xml, options: 0)
        try data.write(to: launchAgentURL, options: .atomic)
    }

    private func launch(_ app: URL, server: String, env: [String: String], after old: Set<pid_t>) -> (Bool, String?) {
        let deadline = Date().addingTimeInterval(25)
        // macOS may list a menu bar app that just quit for a moment; opening
        // then would only find the old one.
        while Date() < deadline && NSRunningApplication.runningApplications(withBundleIdentifier: switcherBundleID).contains(where: { old.contains($0.processIdentifier) }) {
            usleep(200_000)
        }
        let opened = DispatchSemaphore(value: 0)
        var openError: Error?
        let config = NSWorkspace.OpenConfiguration()
        config.activates = false
        config.addsToRecentItems = false
        NSWorkspace.shared.openApplication(at: app, configuration: config) { _, error in
            openError = error
            opened.signal()
        }
        _ = opened.wait(timeout: .now() + 15)
        if let e = openError { return (false, "Switcher is installed but did not open: \(e.localizedDescription)") }
        var seen = ""
        while Date() < deadline {
            let r = run(server, ["status", "--json"], env: env, timeout: 3)
            if r.ok, let data = r.out.data(using: .utf8),
               let json = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
               let v = json["version"] as? String {
                if v == version { return (true, nil) }
                seen = v
            }
            usleep(500_000)
        }
        if !seen.isEmpty {
            return (false, "Another Switcher (\(seen)) still answers on this Mac; quit it, then open Switcher from Applications.")
        }
        return (false, "Switcher has not answered yet; open it from Applications if it does not appear in the menu bar.")
    }
}

// MARK: - Where the installer itself runs from

/// originalBundleURL is where this installer really is: macOS runs a
/// downloaded app from a randomized read-only copy (App Translocation).
func originalBundleURL() -> URL {
    let url = Bundle.main.bundleURL
    guard url.path.contains("/AppTranslocation/") else { return url }
    typealias Original = @convention(c) (CFURL, UnsafeMutablePointer<Unmanaged<CFError>?>?) -> Unmanaged<CFURL>?
    guard let lib = dlopen("/System/Library/Frameworks/Security.framework/Security", RTLD_LAZY),
          let sym = dlsym(lib, "SecTranslocateCreateOriginalPathForURL") else { return url }
    let original = unsafeBitCast(sym, to: Original.self)
    return original(url as CFURL, nil)?.takeRetainedValue() as URL? ?? url
}

/// diskImageMount is the mount point of the disk image this installer was
/// opened from, if it was.
func diskImageMount() -> String? {
    var fs = statfs()
    guard statfs(originalBundleURL().path, &fs) == 0 else { return nil }
    let mount = withUnsafePointer(to: &fs.f_mntonname) { $0.withMemoryRebound(to: CChar.self, capacity: Int(MAXPATHLEN)) { String(cString: $0) } }
    guard mount.hasPrefix("/Volumes/") else { return nil }
    let info = run("/usr/bin/hdiutil", ["info", "-plist"], timeout: 20)
    guard info.ok, let data = info.out.data(using: .utf8),
          let plist = try? PropertyListSerialization.propertyList(from: data, format: nil) as? [String: Any],
          let images = plist["images"] as? [[String: Any]] else { return nil }
    for image in images {
        for entity in image["system-entities"] as? [[String: Any]] ?? [] where entity["mount-point"] as? String == mount {
            return mount
        }
    }
    return nil
}

/// pageURL is one of the installer's pages, with a query for the CI
/// snapshots; nothing is force-unwrapped, so the path inside "Install
/// Switcher.app" with its space always loads.
func pageURL(_ file: URL, query: String) -> URL {
    guard !query.isEmpty, let withQuery = URL(string: "?" + query, relativeTo: file)?.absoluteURL else { return file }
    return withQuery
}

// MARK: - The window

/// A transparent strip along the top that drags the window, since the web
/// page under it takes every click.
final class DragStrip: NSView {
    override func mouseDown(with event: NSEvent) { window?.performDrag(with: event) }
    override var mouseDownCanMoveWindow: Bool { true }
}

/// Holds the page's message handler weakly: WebKit retains handlers.
final class WeakHandler: NSObject, WKScriptMessageHandler {
    weak var target: WKScriptMessageHandler?
    init(_ target: WKScriptMessageHandler) { self.target = target }
    func userContentController(_ c: WKUserContentController, didReceive m: WKScriptMessage) { target?.userContentController(c, didReceive: m) }
}

final class AppDelegate: NSObject, NSApplicationDelegate, NSWindowDelegate, WKScriptMessageHandler, WKNavigationDelegate, WKUIDelegate {
    var window: NSWindow!
    var webView: WKWebView!
    let ui = Bundle.main.resourceURL!.appendingPathComponent("ui", isDirectory: true)
    var installer: Installer!
    var installing = false
    var installed = false
    let query: String
    let bridge: Bool
    /// --autotest DIR, for CI: the window installs with the default options
    /// as soon as its page is up, writes the outcome and a picture of the
    /// last screen to DIR, and quits. A flag, not an environment variable,
    /// so nothing set for a double-clicked app can start an install.
    let autotest: URL?
    /// The current install's events, sent again to a page that reloads.
    var events: [[String: Any]] = []
    var envAtStart: [String: Any] = [:]
    var reloads = 0

    init(query: String = "", bridge: Bool = true, autotest: URL? = nil) {
        self.query = query
        self.bridge = bridge
        self.autotest = autotest
    }

    func applicationDidFinishLaunching(_ note: Notification) {
        let home = URL(fileURLWithPath: NSHomeDirectory())
        installer = Installer(
            payload: Bundle.main.resourceURL!.appendingPathComponent("Switcher.app"),
            home: home,
            candidates: [URL(fileURLWithPath: "/Applications"), home.appendingPathComponent("Applications")])
        buildMenu()

        let config = WKWebViewConfiguration()
        config.websiteDataStore = .nonPersistent()
        config.preferences.javaScriptCanOpenWindowsAutomatically = false
        config.suppressesIncrementalRendering = true
        if bridge { config.userContentController.add(WeakHandler(self), name: "installer") }

        let size = NSSize(width: 880, height: 600)
        window = NSWindow(contentRect: NSRect(origin: .zero, size: size), styleMask: [.titled, .closable, .miniaturizable, .fullSizeContentView], backing: .buffered, defer: false)
        window.title = "Install Switcher"
        window.titlebarAppearsTransparent = true
        window.titleVisibility = .hidden
        window.appearance = NSAppearance(named: .darkAqua)
        window.backgroundColor = NSColor(srgbRed: 0.02, green: 0.043, blue: 0.031, alpha: 1)
        window.isReleasedWhenClosed = false
        window.standardWindowButton(.zoomButton)?.isHidden = true
        window.delegate = self

        webView = WKWebView(frame: NSRect(origin: .zero, size: size), configuration: config)
        webView.autoresizingMask = [.width, .height]
        webView.navigationDelegate = self
        webView.uiDelegate = self
        webView.allowsMagnification = false
        webView.allowsBackForwardNavigationGestures = false
        // Shown once the page has drawn, so the window never flashes white.
        webView.alphaValue = 0
        window.contentView!.addSubview(webView)
        let strip = DragStrip(frame: NSRect(x: 80, y: size.height - 28, width: size.width - 80, height: 28))
        strip.autoresizingMask = [.width, .minYMargin]
        window.contentView!.addSubview(strip)

        webView.loadFileURL(pageURL(ui.appendingPathComponent("index.html"), query: query), allowingReadAccessTo: ui)
        window.center()
        window.makeKeyAndOrderFront(nil)
        NSApp.activate(ignoringOtherApps: true)
    }

    func buildMenu() {
        let main = NSMenu()
        let appItem = NSMenuItem()
        main.addItem(appItem)
        let appMenu = NSMenu()
        appMenu.addItem(withTitle: "Hide Install Switcher", action: #selector(NSApplication.hide(_:)), keyEquivalent: "h")
        appMenu.addItem(.separator())
        appMenu.addItem(withTitle: "Quit Install Switcher", action: #selector(NSApplication.terminate(_:)), keyEquivalent: "q")
        appItem.submenu = appMenu
        let editItem = NSMenuItem()
        main.addItem(editItem)
        let edit = NSMenu(title: "Edit")
        edit.addItem(withTitle: "Copy", action: #selector(NSText.copy(_:)), keyEquivalent: "c")
        edit.addItem(withTitle: "Select All", action: #selector(NSText.selectAll(_:)), keyEquivalent: "a")
        editItem.submenu = edit
        let windowItem = NSMenuItem()
        main.addItem(windowItem)
        let win = NSMenu(title: "Window")
        win.addItem(withTitle: "Minimize", action: #selector(NSWindow.performMiniaturize(_:)), keyEquivalent: "m")
        win.addItem(withTitle: "Close", action: #selector(NSWindow.performClose(_:)), keyEquivalent: "w")
        windowItem.submenu = win
        NSApp.mainMenu = main
    }

    // Only the installer's own pages load; every link and new window is
    // refused, and the few outside pages it offers open in the browser.
    func webView(_ webView: WKWebView, decidePolicyFor action: WKNavigationAction, decisionHandler: @escaping (WKNavigationActionPolicy) -> Void) {
        let url = action.request.url
        let ours = url?.isFileURL == true && url!.standardizedFileURL.path.hasPrefix(ui.standardizedFileURL.path + "/")
        let load = action.navigationType == .other || action.navigationType == .reload
        decisionHandler(ours && load && action.targetFrame?.isMainFrame == true ? .allow : .cancel)
    }

    func webView(_ webView: WKWebView, createWebViewWith configuration: WKWebViewConfiguration, for action: WKNavigationAction, windowFeatures: WKWindowFeatures) -> WKWebView? { nil }

    func webView(_ webView: WKWebView, didFinish navigation: WKNavigation!) {
        NSAnimationContext.runAnimationGroup { ctx in
            ctx.duration = 0.35
            webView.animator().alphaValue = 1
        }
    }

    // The page's process can end (memory pressure, a WebKit crash): the page
    // loads again, and its "ready" brings back the install's progress.
    func webViewWebContentProcessDidTerminate(_ webView: WKWebView) {
        reloads += 1
        if reloads <= 3 { webView.reload() }
    }

    func send(_ event: [String: Any]) {
        var event = event
        if let type = event["type"] as? String, ["plan", "step", "done", "error"].contains(type) {
            event["seq"] = events.count + 1
            events.append(event)
        }
        deliver(event)
    }

    private func deliver(_ event: [String: Any]) {
        webView.callAsyncJavaScript("window.installer.receive(e)", arguments: ["e": event], in: nil, in: .page, completionHandler: nil)
    }

    func userContentController(_ c: WKUserContentController, didReceive message: WKScriptMessage) {
        // Messages come only from the installer's own page, which is the
        // only page this view loads; still, each one is checked.
        guard message.frameInfo.isMainFrame, message.frameInfo.request.url?.isFileURL == true,
              let body = message.body as? [String: Any], let type = body["type"] as? String else { return }
        switch type {
        case "ready":
            if installing || installed {
                // A page that loaded again mid-install: the same facts as
                // before, then everything that has happened since.
                deliver(["type": "env", "env": envAtStart])
                for e in events { deliver(e) }
                return
            }
            installer.survey()
            envAtStart = installer.environment()
            deliver(["type": "env", "env": envAtStart])
            if autotest != nil { startInstall(InstallOptions()) }
        case "install":
            guard !installing else { return }
            let raw = body["options"] as? [String: Any] ?? [:]
            var o = InstallOptions()
            o.login = raw["login"] as? Bool ?? true
            o.cli = raw["cli"] as? Bool ?? true
            o.skill = raw["skill"] as? Bool ?? true
            startInstall(o)
        case "open":
            switch body["target"] as? String {
            case "dashboard": NSWorkspace.shared.open(dashboardURL)
            case "repo": NSWorkspace.shared.open(repoURL)
            case "app":
                if let app = installer.existing?.url { NSWorkspace.shared.openApplication(at: app, configuration: NSWorkspace.OpenConfiguration(), completionHandler: nil) }
            default: break
            }
        case "copy":
            if let text = body["text"] as? String, text.count <= 4096 {
                NSPasteboard.general.clearContents()
                NSPasteboard.general.setString(text, forType: .string)
            }
        case "finish":
            finish()
        case "quit":
            NSApp.terminate(nil)
        default:
            break
        }
    }

    func startInstall(_ o: InstallOptions) {
        installing = true
        events = []
        installer.survey()
        send(["type": "plan", "steps": installer.plan(o)])
        let installer = self.installer!
        DispatchQueue.global(qos: .userInitiated).async {
            // The page paces the steps it shows; the work never waits for it.
            let report: (String, String, String?) -> Void = { id, state, detail in
                var e: [String: Any] = ["type": "step", "id": id, "state": state]
                if let d = detail { e["detail"] = d }
                DispatchQueue.main.async { self.send(e) }
            }
            do {
                let result = try installer.install(o, report: report)
                DispatchQueue.main.async {
                    self.installing = false
                    self.installed = true
                    self.send(["type": "done", "result": result])
                    self.autotestFinish(["ok": true, "result": result])
                }
            } catch let e as InstallError {
                DispatchQueue.main.async {
                    self.installing = false
                    self.send(["type": "error", "title": e.title, "message": e.message, "detail": e.detail, "changed": e.changed, "calm": e.calm])
                    self.autotestFinish(["ok": false, "message": e.message, "detail": e.detail])
                }
            } catch {
                DispatchQueue.main.async {
                    self.installing = false
                    self.send(["type": "error", "message": "Something went wrong.", "detail": error.localizedDescription, "changed": false])
                    self.autotestFinish(["ok": false, "message": error.localizedDescription])
                }
            }
        }
    }

    func autotestFinish(_ outcome: [String: Any]) {
        guard let dir = autotest else { return }
        try? FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        if let data = try? JSONSerialization.data(withJSONObject: outcome, options: [.prettyPrinted, .sortedKeys, .withoutEscapingSlashes]) {
            try? data.write(to: dir.appendingPathComponent("result.json"))
        }
        DispatchQueue.main.asyncAfter(deadline: .now() + 3.5) {
            self.webView.takeSnapshot(with: nil) { image, _ in
                if let tiff = image?.tiffRepresentation, let rep = NSBitmapImageRep(data: tiff),
                   let png = rep.representation(using: .png, properties: [:]) {
                    try? png.write(to: dir.appendingPathComponent("last-screen.png"))
                }
                NSApp.terminate(nil)
            }
        }
    }

    /// finish closes the installer and, when it ran from its disk image,
    /// ejects that once the installer has quit.
    func finish() {
        if installed, let mount = diskImageMount() {
            let p = Process()
            p.executableURL = URL(fileURLWithPath: "/bin/sh")
            // The installer must have quit first; Finder may hold the volume
            // a moment longer, so a second try forces it, as the updater does.
            p.arguments = ["-c", "sleep 2; /usr/bin/hdiutil detach -quiet \"$1\" || { sleep 3; /usr/bin/hdiutil detach -quiet -force \"$1\"; } || true", "sh", mount]
            try? p.run()
        }
        NSApp.terminate(nil)
    }

    func windowShouldClose(_ sender: NSWindow) -> Bool {
        if installing { NSSound.beep(); return false }
        return true
    }

    func applicationShouldTerminate(_ sender: NSApplication) -> NSApplication.TerminateReply {
        installing ? .terminateCancel : .terminateNow
    }

    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool { true }
}

// MARK: - Checks run in CI

/// installTest installs this installer's Switcher into a scratch folder with
/// a scratch home, the same code path as the window, and prints each step.
func installTest(_ args: [String]) -> Int32 {
    guard args.count >= 4 else {
        FileHandle.standardError.write("usage: --install-test <applications> <home> <cli-dir>\n".data(using: .utf8)!)
        return 2
    }
    let apps = URL(fileURLWithPath: args[1]), home = URL(fileURLWithPath: args[2])
    let installer = Installer(payload: Bundle.main.resourceURL!.appendingPathComponent("Switcher.app"), home: home, candidates: [apps],
                              commandEnv: ["HOME": home.path, "CLAUDE_CONFIG_DIR": home.appendingPathComponent(".claude").path])
    var o = InstallOptions()
    o.launch = false
    o.cliDir = args[3]
    let env = installer.environment()
    print(String(decoding: try! JSONSerialization.data(withJSONObject: ["env": env, "plan": installer.plan(o)] as [String: Any], options: [.sortedKeys, .withoutEscapingSlashes]), as: UTF8.self))
    do {
        let result = try installer.install(o) { id, state, detail in print("step \(id) \(state)\(detail.map { " " + $0 } ?? "")") }
        print(String(decoding: try! JSONSerialization.data(withJSONObject: ["result": result], options: [.sortedKeys, .withoutEscapingSlashes]), as: UTF8.self))
        return 0
    } catch let e as InstallError {
        print("error: \(e.message)\n\(e.detail)")
        return 1
    } catch {
        print("error: \(error)")
        return 1
    }
}

/// Snapshots renders installer screens to PNG files, for review in CI.
final class Snapshots: NSObject, NSApplicationDelegate, WKNavigationDelegate {
    let out: URL
    var jobs: [(String, String, Double, CGFloat)]
    var window: NSWindow!
    var webView: WKWebView!
    let base: URL
    var job: (name: String, wait: Double, zoom: CGFloat, size: NSSize)!
    var finished = false
    var tries = 0

    init(out: URL, page: URL, jobs: [(String, String, Double, CGFloat)]) {
        self.out = out
        self.base = page
        self.jobs = jobs
    }

    func applicationDidFinishLaunching(_ note: Notification) {
        next()
    }

    func next() {
        guard !jobs.isEmpty else { NSApp.terminate(nil); return }
        let (name, query, wait, zoom) = jobs.removeFirst()
        FileHandle.standardError.write("snapshot \(name): loading\n".data(using: .utf8)!)
        let size = base.lastPathComponent == "background.html" ? NSSize(width: 720, height: 440) : NSSize(width: 880, height: 600)
        job = (name, wait, zoom, size)
        finished = false
        tries = 0
        let config = WKWebViewConfiguration()
        config.websiteDataStore = .nonPersistent()
        // On the screen and at its own size: WebKit stops drawing a page
        // whose window is hidden, and the snapshot asks for the scale.
        window = NSWindow(contentRect: NSRect(origin: NSPoint(x: 40, y: 80), size: size), styleMask: [.borderless], backing: .buffered, defer: false)
        window.appearance = NSAppearance(named: .darkAqua)
        webView = WKWebView(frame: NSRect(origin: .zero, size: size), configuration: config)
        webView.navigationDelegate = self
        window.contentView = webView
        window.orderFrontRegardless()
        webView.loadFileURL(pageURL(base, query: query), allowingReadAccessTo: base.deletingLastPathComponent())
        let started = name
        DispatchQueue.main.asyncAfter(deadline: .now() + wait + 30) {
            if !self.finished && self.job.name == started { print("snapshot \(started) never came back"); exit(3) }
        }
    }

    // The page has loaded: wait for its fonts and two drawn frames, then the
    // job's own time for animations, before the picture is taken.
    func webView(_ webView: WKWebView, didFinish navigation: WKNavigation!) {
        webView.callAsyncJavaScript("await document.fonts.ready; await new Promise(r => requestAnimationFrame(() => requestAnimationFrame(r)));",
                                    arguments: [:], in: nil, in: .page) { _ in
            DispatchQueue.main.asyncAfter(deadline: .now() + self.job.wait) { self.capture() }
        }
    }

    func capture() {
        let (name, _, zoom, size) = job!
        let cfg = WKSnapshotConfiguration()
        cfg.snapshotWidth = NSNumber(value: Double(size.width * zoom))
        webView.takeSnapshot(with: cfg) { image, error in
            let pw = Int(size.width * zoom), ph = Int(size.height * zoom)
            guard let image = image,
                  let rep = NSBitmapImageRep(bitmapDataPlanes: nil, pixelsWide: pw, pixelsHigh: ph, bitsPerSample: 8, samplesPerPixel: 4,
                                             hasAlpha: true, isPlanar: false, colorSpaceName: .deviceRGB, bytesPerRow: 0, bitsPerPixel: 0) else {
                print("snapshot \(name) failed: \(error?.localizedDescription ?? "no image")")
                exit(3)
            }
            // Exactly the pixels asked for, whatever the screen's scale.
            rep.size = NSSize(width: pw, height: ph)
            NSGraphicsContext.saveGraphicsState()
            NSGraphicsContext.current = NSGraphicsContext(bitmapImageRep: rep)
            image.draw(in: NSRect(x: 0, y: 0, width: pw, height: ph))
            NSGraphicsContext.restoreGraphicsState()
            // A picture of one flat colour is a page not yet drawn: take it
            // again rather than ship a blank background.
            if Snapshots.flat(rep) {
                self.tries += 1
                guard self.tries < 10 else { print("snapshot \(name) stayed blank"); exit(3) }
                FileHandle.standardError.write("snapshot \(name): blank, taking it again\n".data(using: .utf8)!)
                DispatchQueue.main.asyncAfter(deadline: .now() + 1) { self.capture() }
                return
            }
            self.finished = true
            guard let png = rep.representation(using: .png, properties: [:]),
                  (try? png.write(to: self.out.appendingPathComponent(name + ".png"))) != nil else {
                print("snapshot \(name) could not be written")
                exit(3)
            }
            print("wrote \(name).png \(pw)x\(ph)")
            self.window.orderOut(nil)
            self.next()
        }
    }

    /// flat is true when a grid of samples across the picture is all one
    /// colour.
    static func flat(_ rep: NSBitmapImageRep) -> Bool {
        guard let first = rep.colorAt(x: 0, y: 0) else { return true }
        for y in stride(from: 0, to: rep.pixelsHigh, by: max(1, rep.pixelsHigh / 24)) {
            for x in stride(from: 0, to: rep.pixelsWide, by: max(1, rep.pixelsWide / 24)) {
                if rep.colorAt(x: x, y: y) != first { return false }
            }
        }
        return true
    }
}

@main
struct InstallSwitcher {
    static func main() {
        let args = CommandLine.arguments
        if let i = args.firstIndex(of: "--install-test") {
            exit(installTest(Array(args[i...])))
        }
        let app = NSApplication.shared
        if args.contains("--snapshot") || args.contains("--autotest") {
            // In CI an exception must end the run with its reason, not be
            // swallowed by AppKit and leave a window that does nothing.
            UserDefaults.standard.set(true, forKey: "NSApplicationCrashOnExceptions")
        }
        if let i = args.firstIndex(of: "--snapshot"), i + 1 < args.count {
            // --snapshot <out> [page]: the installer's screens, or a page
            // such as the disk image background.
            let out = URL(fileURLWithPath: args[i + 1])
            try? FileManager.default.createDirectory(at: out, withIntermediateDirectories: true)
            let ui = Bundle.main.resourceURL!.appendingPathComponent("ui")
            if i + 2 < args.count {
                let page = URL(fileURLWithPath: args[i + 2])
                let name = page.deletingPathExtension().lastPathComponent
                let delegate = Snapshots(out: out, page: page, jobs: [(name, "", 1.5, 1), (name + "@2x", "", 1.5, 2)])
                app.delegate = delegate
                app.setActivationPolicy(.accessory)
                withExtendedLifetime(delegate) { app.run() }
                return
            }
            let delegate = Snapshots(out: out, page: ui.appendingPathComponent("index.html"), jobs: [
                ("1-welcome", "", 3.6, 2), ("2-tour-switch", "screen=tour&slide=0", 2.6, 2), ("3-tour-limits", "screen=tour&slide=1", 4.6, 2),
                ("4-tour-quota", "screen=tour&slide=2", 2.6, 2), ("5-tour-share", "screen=tour&slide=3", 3, 2), ("6-tour-agents", "screen=tour&slide=4", 7, 2),
                ("7-setup", "screen=setup", 1.6, 2), ("8-why", "screen=setup&sheet=1", 1.6, 2), ("9-install", "screen=install", 4, 2),
                ("10-done", "screen=done", 2, 2), ("11-error", "screen=error", 1.6, 2), ("12-update", "existing=1.32.0", 3.6, 2), ("13-newer", "existing=9.0.0", 1.6, 2),
            ])
            app.delegate = delegate
            app.setActivationPolicy(.accessory)
            withExtendedLifetime(delegate) { app.run() }
            return
        }
        // NSApplication holds its delegate weakly: it must outlive run().
        var autotest: URL?
        if let i = args.firstIndex(of: "--autotest"), i + 1 < args.count { autotest = URL(fileURLWithPath: args[i + 1]) }
        let delegate = AppDelegate(autotest: autotest)
        app.delegate = delegate
        app.setActivationPolicy(.regular)
        withExtendedLifetime(delegate) { app.run() }
    }
}
