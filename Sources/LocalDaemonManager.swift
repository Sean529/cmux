import Foundation

/// Manages the lifecycle of the cmuxd-local daemon process.
/// The daemon owns PTYs and enables session detach/reattach.
/// The daemon is intentionally NOT killed on app quit so sessions persist.
@MainActor
final class LocalDaemonManager: ObservableObject {
    static let shared = LocalDaemonManager()

    @Published private(set) var isRunning: Bool = false
    @Published private(set) var daemonVersion: String?
    @Published private(set) var isLaunchAgentInstalled: Bool = false

    /// Sessions that are running in the daemon but not currently attached to any workspace.
    /// Updated periodically when the daemon is running.
    @Published private(set) var detachedSessions: [[String: Any]] = []

    /// Number of detached sessions, suitable for badge/indicator display.
    var detachedSessionCount: Int { detachedSessions.count }

    private var daemonProcess: Process?
    private var refreshTask: Task<Void, Never>?

    nonisolated private static let launchAgentLabel = "com.cmux.daemon-local"

    nonisolated static var launchAgentPlistPath: String {
        NSHomeDirectory() + "/Library/LaunchAgents/com.cmux.daemon-local.plist"
    }

    /// Private state directory for the daemon (~/.local/state/cmux/).
    /// Matches the Go daemon's DefaultStateDir().
    nonisolated static var stateDir: String {
        NSHomeDirectory() + "/.local/state/cmux"
    }

    /// Primary socket path in the private state directory.
    nonisolated static var socketPath: String {
        stateDir + "/daemon-local.sock"
    }

    /// Legacy /tmp socket path for backward compatibility.
    nonisolated static var legacySocketPath: String {
        "/tmp/cmux-local-\(getuid()).sock"
    }

    /// Returns the best available socket path: primary if it exists,
    /// otherwise falls back to the legacy /tmp path.
    nonisolated static var resolvedSocketPath: String {
        let primary = socketPath
        if FileManager.default.fileExists(atPath: primary) {
            return primary
        }
        let legacy = legacySocketPath
        if FileManager.default.fileExists(atPath: legacy) {
            return legacy
        }
        return primary
    }

    private init() {
        // Check if the launch agent plist is already installed.
        isLaunchAgentInstalled = FileManager.default.fileExists(atPath: Self.launchAgentPlistPath)
    }

    // MARK: - Public API

    /// Start the daemon if not already running. Safe to call multiple times.
    /// Async to avoid blocking the main thread while waiting for the daemon to become ready.
    ///
    /// If a launchd agent is installed, checks `launchctl list` for the daemon
    /// and kicks it via `launchctl kickstart` if needed. Falls back to direct
    /// process spawn when no launch agent is configured.
    func ensureRunning() async {
        // Fast path: daemon already confirmed running.
        if isRunning, Self.probeSync() {
            return
        }

        // Check if an external daemon (or launchd-managed) is already listening.
        if Self.probeSync() {
            isRunning = true
            await fetchVersion()
            return
        }

        // If a launchd agent is installed, use launchctl to start/restart it.
        if isLaunchAgentInstalled {
            let started = await ensureRunningViaLaunchd()
            if started {
                return
            }
            // Launchd start failed — fall through to direct spawn as a fallback.
        }

        // Direct spawn: launch a new daemon process.
        guard let binaryPath = locateBinary() else {
            isRunning = false
            return
        }

        let process = Process()
        process.executableURL = URL(fileURLWithPath: binaryPath)
        process.arguments = []
        // Detach stdout/stderr so the app doesn't capture daemon output.
        process.standardOutput = FileHandle.nullDevice
        process.standardError = FileHandle.nullDevice
        process.standardInput = FileHandle.nullDevice

        do {
            try process.run()
        } catch {
            isRunning = false
            return
        }

        daemonProcess = process

        // Wait up to 3 seconds for the socket to appear and become reachable.
        // Uses Task.sleep instead of Thread.sleep to avoid blocking the main thread.
        // Probe runs off-main via Task.detached to keep the main thread free.
        var connected = false
        for _ in 0..<30 {
            try? await Task.sleep(nanoseconds: 100_000_000) // 0.1 seconds
            let probeResult = await Task.detached { Self.probeSync() }.value
            if probeResult {
                connected = true
                break
            }
        }

        isRunning = connected
        if connected {
            await fetchVersion()
        }
    }

    /// Attempt to start the daemon via launchd. Returns true if the daemon
    /// becomes reachable after kickstarting the agent.
    private func ensureRunningViaLaunchd() async -> Bool {
        // Check if launchd already knows about the service by listing it.
        let isLoaded = await Task.detached {
            Self.launchdServiceIsLoaded()
        }.value

        if !isLoaded {
            // The plist exists but hasn't been bootstrapped. Bootstrap it now.
            let bootstrapSuccess = await Task.detached {
                Self.runLaunchctl(arguments: ["bootstrap", "gui/\(getuid())", Self.launchAgentPlistPath])
            }.value
            if !bootstrapSuccess {
                return false
            }
        }

        // Kickstart the service in case it's not running (idempotent if already running).
        _ = await Task.detached {
            Self.runLaunchctl(arguments: ["kickstart", "gui/\(getuid())/\(Self.launchAgentLabel)"])
        }.value

        // Wait up to 3 seconds for the daemon to become reachable.
        var connected = false
        for _ in 0..<30 {
            try? await Task.sleep(nanoseconds: 100_000_000)
            let probeResult = await Task.detached { Self.probeSync() }.value
            if probeResult {
                connected = true
                break
            }
        }

        isRunning = connected
        if connected {
            await fetchVersion()
        }
        return connected
    }

    /// Check if the daemon socket is reachable (nonisolated, safe to call from any thread).
    nonisolated static func probeSync() -> Bool {
        guard let response = rpcSync(method: "ping") else { return false }
        return response["ok"] as? Bool == true
    }

    /// Send an RPC request to the daemon and return the parsed response.
    /// Nonisolated — performs blocking socket I/O, must NOT be called on the main thread.
    /// Use `rpcAsync` from MainActor contexts instead.
    nonisolated static func rpcSync(method: String, params: [String: Any] = [:]) -> [String: Any]? {
        let request: [String: Any] = [
            "id": 1,
            "method": method,
            "params": params,
        ]
        guard JSONSerialization.isValidJSONObject(request),
              let data = try? JSONSerialization.data(withJSONObject: request, options: []),
              let payload = String(data: data, encoding: .utf8) else {
            return nil
        }
        guard let raw = sendSocketCommand(payload, socketPath: resolvedSocketPath, timeout: 5.0) else { return nil }
        guard let responseData = raw.data(using: .utf8),
              let parsed = try? JSONSerialization.jsonObject(with: responseData, options: []) as? [String: Any] else {
            return nil
        }
        return parsed
    }

    /// Async wrapper that dispatches the blocking RPC to a background thread.
    /// Safe to call from MainActor contexts.
    func rpcAsync(method: String, params: [String: Any] = [:]) async -> [String: Any]? {
        await Task.detached {
            Self.rpcSync(method: method, params: params)
        }.value
    }

    /// List active sessions from the daemon (nonisolated, blocking).
    nonisolated static func listSessionsSync() -> [[String: Any]] {
        guard let response = rpcSync(method: "session.list") else { return [] }
        guard response["ok"] as? Bool == true,
              let result = response["result"] as? [String: Any],
              let sessions = result["sessions"] as? [[String: Any]] else {
            return []
        }
        return sessions
    }

    /// Async wrapper for listing sessions, safe to call from MainActor.
    func listSessionsAsync() async -> [[String: Any]] {
        await Task.detached {
            Self.listSessionsSync()
        }.value
    }

    /// Stop the daemon. Only call on explicit user request — NOT on app quit.
    func stop() async {
        refreshTask?.cancel()
        refreshTask = nil
        await rpcAsync(method: "shutdown")
        daemonProcess?.terminate()
        daemonProcess = nil
        isRunning = false
        daemonVersion = nil
        detachedSessions = []
    }

    /// Refresh the list of detached (running but not attached) sessions from the daemon.
    /// Fetches sessions off-main, then updates the published property on MainActor.
    func refreshDetachedSessions() async {
        let all = await Task.detached {
            Self.listSessionsSync()
        }.value
        detachedSessions = all.filter { session in
            let attached = session["attached"] as? Int ?? 0
            let status = session["status"] as? String ?? ""
            return attached == 0 && status == "running"
        }
    }

    /// Begin periodic polling of detached sessions. Called once after daemon startup.
    func startPeriodicRefresh() {
        guard refreshTask == nil else { return }
        // Do an initial refresh.
        Task { await refreshDetachedSessions() }
        // Start a detached loop that polls every 5 seconds.
        refreshTask = Task.detached { [weak self] in
            while !Task.isCancelled {
                try? await Task.sleep(nanoseconds: 5_000_000_000)
                guard !Task.isCancelled else { return }
                let sessions = Self.listSessionsSync()
                let detached = sessions.filter { session in
                    let attached = session["attached"] as? Int ?? 0
                    let status = session["status"] as? String ?? ""
                    return attached == 0 && status == "running"
                }
                await MainActor.run { [weak self] in
                    self?.detachedSessions = detached
                }
            }
        }
    }

    // MARK: - Launch Agent Management

    /// Install a launchd user agent so the daemon auto-starts on login.
    /// Copies the plist template, substitutes the binary path, and bootstraps the agent.
    /// Runs file I/O and launchctl off the main thread.
    func installLaunchAgent() async throws {
        guard let binaryPath = locateBinary() else {
            throw LaunchAgentError.binaryNotFound(
                String(localized: "daemon.launchAgent.error.binaryNotFound",
                       defaultValue: "Could not find the cmuxd-local binary to install the launch agent.")
            )
        }

        let home = NSHomeDirectory()
        let plistDestination = Self.launchAgentPlistPath

        // Read the plist template from the app bundle or the source tree.
        guard let templateURL = Bundle.main.url(forResource: "com.cmux.daemon-local", withExtension: "plist"),
              let templateData = try? Data(contentsOf: templateURL),
              var templateString = String(data: templateData, encoding: .utf8) else {
            throw LaunchAgentError.templateNotFound(
                String(localized: "daemon.launchAgent.error.templateNotFound",
                       defaultValue: "Could not find the launch agent plist template in the app bundle.")
            )
        }

        // Substitute placeholders.
        templateString = templateString
            .replacingOccurrences(of: "__CMUXD_LOCAL_BINARY_PATH__", with: binaryPath)
            .replacingOccurrences(of: "__HOME__", with: home)

        // Write the plist and bootstrap — all off-main.
        try await Task.detached {
            let launchAgentsDir = home + "/Library/LaunchAgents"
            let fm = FileManager.default

            // Ensure ~/Library/LaunchAgents exists.
            if !fm.fileExists(atPath: launchAgentsDir) {
                try fm.createDirectory(atPath: launchAgentsDir, withIntermediateDirectories: true)
            }

            // If already installed, bootout the old one first (ignore errors).
            if fm.fileExists(atPath: plistDestination) {
                _ = Self.runLaunchctl(arguments: ["bootout", "gui/\(getuid())/\(Self.launchAgentLabel)"])
                try? fm.removeItem(atPath: plistDestination)
            }

            // Write the substituted plist.
            guard let plistData = templateString.data(using: .utf8) else {
                throw LaunchAgentError.writeFailed(
                    String(localized: "daemon.launchAgent.error.writeFailed",
                           defaultValue: "Failed to write the launch agent plist file.")
                )
            }
            try plistData.write(to: URL(fileURLWithPath: plistDestination))

            // Bootstrap the agent.
            let success = Self.runLaunchctl(arguments: ["bootstrap", "gui/\(getuid())", plistDestination])
            if !success {
                // Clean up on failure.
                try? fm.removeItem(atPath: plistDestination)
                throw LaunchAgentError.bootstrapFailed(
                    String(localized: "daemon.launchAgent.error.bootstrapFailed",
                           defaultValue: "Failed to bootstrap the launch agent with launchctl.")
                )
            }
        }.value

        isLaunchAgentInstalled = true
    }

    /// Uninstall the launchd user agent. Stops the daemon via launchctl and removes the plist.
    /// Runs launchctl and file removal off the main thread.
    func uninstallLaunchAgent() async throws {
        let plistPath = Self.launchAgentPlistPath

        try await Task.detached {
            // Bootout the service (stops the daemon if running).
            _ = Self.runLaunchctl(arguments: ["bootout", "gui/\(getuid())/\(Self.launchAgentLabel)"])

            // Remove the plist file.
            let fm = FileManager.default
            if fm.fileExists(atPath: plistPath) {
                try fm.removeItem(atPath: plistPath)
            }
        }.value

        isLaunchAgentInstalled = false
    }

    // MARK: - Private

    /// Check if the launchd service is currently loaded (known to launchd).
    nonisolated private static func launchdServiceIsLoaded() -> Bool {
        let process = Process()
        process.executableURL = URL(fileURLWithPath: "/bin/launchctl")
        process.arguments = ["print", "gui/\(getuid())/\(launchAgentLabel)"]
        process.standardOutput = FileHandle.nullDevice
        process.standardError = FileHandle.nullDevice
        process.standardInput = FileHandle.nullDevice

        do {
            try process.run()
            process.waitUntilExit()
            return process.terminationStatus == 0
        } catch {
            return false
        }
    }

    /// Run a launchctl command with the given arguments. Returns true on success.
    @discardableResult
    nonisolated private static func runLaunchctl(arguments: [String]) -> Bool {
        let process = Process()
        process.executableURL = URL(fileURLWithPath: "/bin/launchctl")
        process.arguments = arguments
        process.standardOutput = FileHandle.nullDevice
        process.standardError = FileHandle.nullDevice
        process.standardInput = FileHandle.nullDevice

        do {
            try process.run()
            process.waitUntilExit()
            return process.terminationStatus == 0
        } catch {
            return false
        }
    }

    private func fetchVersion() async {
        guard let response = await rpcAsync(method: "hello") else { return }
        if let result = response["result"] as? [String: Any],
           let version = result["version"] as? String {
            daemonVersion = version
        }
    }

    /// Locate the cmuxd-local binary, checking bundled, user-installed, and project paths.
    private func locateBinary() -> String? {
        let candidates: [String] = [
            // 1. Bundled inside the app
            Bundle.main.resourcePath.map { $0 + "/cmuxd-local" },
            // 2. User-installed
            NSHomeDirectory() + "/.cmux/bin/cmuxd-local",
            // 3. Homebrew (Apple Silicon)
            "/opt/homebrew/bin/cmuxd-local",
            // 4. Homebrew (Intel)
            "/usr/local/bin/cmuxd-local",
        ].compactMap { $0 }

        for path in candidates {
            if FileManager.default.isExecutableFile(atPath: path) {
                return path
            }
        }
        return nil
    }

    /// Connect to the Unix domain socket, send a command, and read the first response line.
    /// Nonisolated static method — safe to call from any thread.
    nonisolated private static func sendSocketCommand(_ command: String, socketPath: String, timeout: TimeInterval) -> String? {
        let fd = socket(AF_UNIX, SOCK_STREAM, 0)
        guard fd >= 0 else { return nil }
        defer { close(fd) }

        // Configure timeouts
        let normalizedTimeout = max(timeout, 0)
        let seconds = floor(normalizedTimeout)
        let microseconds = (normalizedTimeout - seconds) * 1_000_000
        var socketTimeout = timeval(tv_sec: Int(seconds), tv_usec: Int32(microseconds.rounded()))
        _ = withUnsafePointer(to: &socketTimeout) { ptr in
            setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, ptr, socklen_t(MemoryLayout<timeval>.size))
        }
        _ = withUnsafePointer(to: &socketTimeout) { ptr in
            setsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, ptr, socklen_t(MemoryLayout<timeval>.size))
        }

        // Disable SIGPIPE
        var noSigPipe: Int32 = 1
        _ = withUnsafePointer(to: &noSigPipe) { ptr in
            setsockopt(fd, SOL_SOCKET, SO_NOSIGPIPE, ptr, socklen_t(MemoryLayout<Int32>.size))
        }

        // Connect to Unix socket
        var addr = sockaddr_un()
        memset(&addr, 0, MemoryLayout<sockaddr_un>.size)
        addr.sun_family = sa_family_t(AF_UNIX)

        let maxLen = MemoryLayout.size(ofValue: addr.sun_path)
        let pathBytes = Array(socketPath.utf8CString)
        guard pathBytes.count <= maxLen else { return nil }
        withUnsafeMutablePointer(to: &addr.sun_path) { ptr in
            let raw = UnsafeMutableRawPointer(ptr).assumingMemoryBound(to: CChar.self)
            memset(raw, 0, maxLen)
            for index in 0..<pathBytes.count {
                raw[index] = pathBytes[index]
            }
        }

        let pathOffset = MemoryLayout<sockaddr_un>.offset(of: \.sun_path) ?? 0
        let addrLen = socklen_t(pathOffset + pathBytes.count)
        addr.sun_len = UInt8(min(Int(addrLen), 255))

        let connectResult = withUnsafePointer(to: &addr) { ptr in
            ptr.withMemoryRebound(to: sockaddr.self, capacity: 1) { sockaddrPtr in
                connect(fd, sockaddrPtr, addrLen)
            }
        }
        guard connectResult == 0 else { return nil }

        // Write the command + newline
        let payload = command + "\n"
        let wroteAll = payload.withCString { cString in
            var remaining = strlen(cString)
            var pointer = UnsafeRawPointer(cString)
            while remaining > 0 {
                let written = write(fd, pointer, remaining)
                if written <= 0 { return false }
                remaining -= written
                pointer = pointer.advanced(by: written)
            }
            return true
        }
        guard wroteAll else { return nil }

        // Read response until newline
        var buffer = [UInt8](repeating: 0, count: 4096)
        var response = ""

        while true {
            let count = read(fd, &buffer, buffer.count)
            if count < 0 {
                let readErrno = errno
                if readErrno == EAGAIN || readErrno == EWOULDBLOCK {
                    break
                }
                return nil
            }
            if count == 0 {
                break
            }
            if let chunk = String(bytes: buffer[0..<count], encoding: .utf8) {
                response.append(chunk)
                if let newlineIndex = response.firstIndex(of: "\n") {
                    return String(response[..<newlineIndex])
                }
            }
        }

        let trimmed = response.trimmingCharacters(in: .whitespacesAndNewlines)
        return trimmed.isEmpty ? nil : trimmed
    }
}

// MARK: - Launch Agent Errors

enum LaunchAgentError: Error, LocalizedError {
    case binaryNotFound(String)
    case templateNotFound(String)
    case writeFailed(String)
    case bootstrapFailed(String)

    var errorDescription: String? {
        switch self {
        case .binaryNotFound(let msg): return msg
        case .templateNotFound(let msg): return msg
        case .writeFailed(let msg): return msg
        case .bootstrapFailed(let msg): return msg
        }
    }
}
