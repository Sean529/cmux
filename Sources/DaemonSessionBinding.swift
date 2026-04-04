import Foundation
#if DEBUG
import Bonsplit
#endif

/// Bridges a cmux workspace terminal to a daemon-managed PTY session.
/// Maintains a persistent socket connection for streaming PTY I/O.
///
/// Usage:
///   1. Create with a daemon session ID and output handler.
///   2. Call `attach(cols:rows:)` to connect to the daemon and start receiving output.
///   3. Call `sendInput(_:)` to forward keyboard input to the PTY.
///   4. Call `detach()` when the workspace closes — the PTY keeps running in the daemon.
///   5. On restore, create a new binding with the same session ID and `attach()` again.
final class DaemonSessionBinding: @unchecked Sendable {
    let sessionID: String
    let sessionName: String

    // All mutable state is protected by `lock`.
    private let lock = NSLock()
    private var _socketFD: Int32 = -1
    private var _readThread: Thread?
    private var _isAttached: Bool = false

    /// Monotonically increasing RPC request ID counter.
    private var _nextRPCID: Int = 1

    /// Pending RPC responses keyed by request ID. Completions are called from the
    /// read thread (or readJSONLine) when a response with a matching `id` arrives.
    private var _pendingResponses: [Int: (([String: Any]) -> Void)] = [:]

    /// Data left over from readJSONLine that must be consumed by the read loop.
    private var _pendingData = Data()

    /// Signaled by the read loop when it exits, so teardown can join.
    private let readThreadDone = DispatchSemaphore(value: 0)

    /// Whether the read thread has already been joined (waited on).
    /// Protected by `lock`. Prevents double-wait on `readThreadDone`.
    private var _readThreadJoined: Bool = false

    private let outputHandler: (Data) -> Void

    /// Primary socket path in the private state directory.
    private static var socketPath: String {
        NSHomeDirectory() + "/.local/state/cmux/daemon-local.sock"
    }

    /// Legacy /tmp socket path for backward compatibility.
    private static var legacySocketPath: String {
        "/tmp/cmux-local-\(getuid()).sock"
    }

    /// Returns the best available socket path: primary if it exists,
    /// otherwise falls back to the legacy /tmp path.
    private static var resolvedSocketPath: String {
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

    init(sessionID: String, sessionName: String, outputHandler: @escaping (Data) -> Void) {
        self.sessionID = sessionID
        self.sessionName = sessionName
        self.outputHandler = outputHandler
    }

    deinit {
        // Properly tear down via close() which synchronizes with the read thread.
        close()
    }

    // MARK: - Locked accessors

    private var isAttached: Bool {
        get { lock.lock(); defer { lock.unlock() }; return _isAttached }
        set { lock.lock(); defer { lock.unlock() }; _isAttached = newValue }
    }

    private var socketFD: Int32 {
        get { lock.lock(); defer { lock.unlock() }; return _socketFD }
        set { lock.lock(); defer { lock.unlock() }; _socketFD = newValue }
    }

    /// Allocate a unique RPC ID. Must be called without holding `lock`.
    func allocateRPCID() -> Int {
        lock.lock()
        let id = _nextRPCID
        _nextRPCID += 1
        lock.unlock()
        return id
    }

    /// Register a pending response handler for the given RPC ID.
    /// The completion will be called from the read thread or from `readJSONLine`.
    private func registerPendingResponse(id: Int, completion: @escaping ([String: Any]) -> Void) {
        lock.lock()
        _pendingResponses[id] = completion
        lock.unlock()
    }

    /// Remove and return the pending response handler for the given ID, if any.
    private func takePendingResponse(id: Int) -> (([String: Any]) -> Void)? {
        lock.lock()
        let handler = _pendingResponses.removeValue(forKey: id)
        lock.unlock()
        return handler
    }

    /// Cancel all pending responses (called during teardown).
    private func cancelAllPendingResponses() {
        lock.lock()
        let handlers = _pendingResponses
        _pendingResponses.removeAll()
        lock.unlock()
        // Signal nil-equivalent (empty dict) so waiters unblock.
        for (_, handler) in handlers {
            handler([:])
        }
    }

    // MARK: - Public API

    /// Attach to the daemon session — sends `session.attach` RPC, then enters
    /// streaming mode receiving `pty.replay` and `pty.output` events.
    func attach(cols: Int, rows: Int) throws {
        lock.lock()
        guard !_isAttached else { lock.unlock(); return }
        lock.unlock()

        let fd = try Self.connectSocket()

        lock.lock()
        _socketFD = fd
        lock.unlock()

        // Send session.attach RPC
        let rpcID = allocateRPCID()
        let request: [String: Any] = [
            "id": rpcID,
            "method": "session.attach",
            "params": [
                "session_id": sessionID,
                "cols": cols,
                "rows": rows,
            ],
        ]

        try sendJSON(request, on: fd)
        let (response, remainder) = try readJSONLine(from: fd)

        guard response["ok"] as? Bool == true else {
            lock.lock()
            _socketFD = -1
            lock.unlock()
            Darwin.close(fd)
            let message = (response["error"] as? [String: Any])?["message"] as? String ?? "attach failed"
            throw DaemonSessionError.attachFailed(message)
        }

        lock.lock()
        _isAttached = true
        _pendingData = remainder
        lock.unlock()

        startReadThread()
    }

    /// Detach from the session. The daemon keeps the PTY alive.
    func detach() {
        lock.lock()
        guard _isAttached else { lock.unlock(); return }
        _isAttached = false
        let fd = _socketFD
        let thread = _readThread
        lock.unlock()

        // Send detach RPC and wait briefly for acknowledgment.
        if fd >= 0 {
            let rpcID = allocateRPCID()
            let sem = DispatchSemaphore(value: 0)
            registerPendingResponse(id: rpcID) { _ in
                sem.signal()
            }
            let request: [String: Any] = [
                "id": rpcID,
                "method": "session.detach",
                "params": ["session_id": sessionID],
            ]
            if (try? sendJSON(request, on: fd)) != nil {
                // Wait up to 2 seconds for the response before proceeding with teardown.
                _ = sem.wait(timeout: .now() + 2.0)
            }
            // Clean up in case the response never arrived.
            _ = takePendingResponse(id: rpcID)
        }

        // Unblock the read() call by shutting down the socket.
        if fd >= 0 {
            shutdown(fd, SHUT_RDWR)
        }

        // Cancel the thread and wait for it to exit. Only one caller joins.
        thread?.cancel()
        lock.lock()
        let shouldJoin = thread != nil && !_readThreadJoined
        if shouldJoin { _readThreadJoined = true }
        lock.unlock()
        if shouldJoin {
            readThreadDone.wait()
        }

        // Now safe to close the FD — read loop has exited.
        lock.lock()
        if _socketFD >= 0 {
            Darwin.close(_socketFD)
            _socketFD = -1
        }
        _readThread = nil
        lock.unlock()
    }

    /// Send keyboard input to the PTY (fire-and-forget).
    func sendInput(_ data: Data) {
        lock.lock()
        guard _isAttached, _socketFD >= 0 else { lock.unlock(); return }
        let fd = _socketFD
        lock.unlock()

        let base64 = data.base64EncodedString()
        let rpcID = allocateRPCID()
        let request: [String: Any] = [
            "id": rpcID,
            "method": "pty.input",
            "params": [
                "session_id": sessionID,
                "data_base64": base64,
            ],
        ]
        try? sendJSON(request, on: fd)
    }

    /// Resize the PTY.
    func resize(cols: Int, rows: Int) {
        lock.lock()
        guard _isAttached, _socketFD >= 0 else { lock.unlock(); return }
        let fd = _socketFD
        lock.unlock()

        let rpcID = allocateRPCID()
        let request: [String: Any] = [
            "id": rpcID,
            "method": "session.resize",
            "params": [
                "session_id": sessionID,
                "cols": cols,
                "rows": rows,
            ],
        ]
        try? sendJSON(request, on: fd)
    }

    /// Close the session entirely (kills the PTY in the daemon).
    func close() {
        lock.lock()
        let wasAttached = _isAttached
        _isAttached = false
        let fd = _socketFD
        let thread = _readThread
        lock.unlock()

        // Send close RPC and wait briefly for acknowledgment.
        if fd >= 0 && wasAttached {
            let rpcID = allocateRPCID()
            let sem = DispatchSemaphore(value: 0)
            registerPendingResponse(id: rpcID) { _ in
                sem.signal()
            }
            let request: [String: Any] = [
                "id": rpcID,
                "method": "session.close",
                "params": ["session_id": sessionID],
            ]
            if (try? sendJSON(request, on: fd)) != nil {
                // Wait up to 2 seconds for the response before proceeding with teardown.
                _ = sem.wait(timeout: .now() + 2.0)
            }
            // Clean up in case the response never arrived.
            _ = takePendingResponse(id: rpcID)
        }

        // Unblock the read() call.
        if fd >= 0 {
            shutdown(fd, SHUT_RDWR)
        }

        // Cancel the thread and wait for it to exit. Only one caller joins.
        thread?.cancel()
        lock.lock()
        let shouldJoin = thread != nil && !_readThreadJoined
        if shouldJoin { _readThreadJoined = true }
        lock.unlock()
        if shouldJoin {
            readThreadDone.wait()
        }

        // Now safe to close the FD.
        lock.lock()
        if _socketFD >= 0 {
            Darwin.close(_socketFD)
            _socketFD = -1
        }
        _readThread = nil
        lock.unlock()
    }

    // MARK: - Private

    private static func connectSocket() throws -> Int32 {
        let fd = socket(AF_UNIX, SOCK_STREAM, 0)
        guard fd >= 0 else {
            throw DaemonSessionError.socketCreateFailed
        }

        // Disable SIGPIPE
        var noSigPipe: Int32 = 1
        _ = withUnsafePointer(to: &noSigPipe) { ptr in
            setsockopt(fd, SOL_SOCKET, SO_NOSIGPIPE, ptr, socklen_t(MemoryLayout<Int32>.size))
        }

        // Set send/receive timeouts (5 seconds) to avoid permanent hangs.
        var socketTimeout = timeval(tv_sec: 5, tv_usec: 0)
        _ = withUnsafePointer(to: &socketTimeout) { ptr in
            setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, ptr, socklen_t(MemoryLayout<timeval>.size))
        }
        _ = withUnsafePointer(to: &socketTimeout) { ptr in
            setsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, ptr, socklen_t(MemoryLayout<timeval>.size))
        }

        var addr = sockaddr_un()
        memset(&addr, 0, MemoryLayout<sockaddr_un>.size)
        addr.sun_family = sa_family_t(AF_UNIX)

        let maxLen = MemoryLayout.size(ofValue: addr.sun_path)
        let pathBytes = Array(resolvedSocketPath.utf8CString)
        guard pathBytes.count <= maxLen else {
            Darwin.close(fd)
            throw DaemonSessionError.socketPathTooLong
        }
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
        guard connectResult == 0 else {
            Darwin.close(fd)
            throw DaemonSessionError.connectFailed(errno)
        }

        return fd
    }

    private func sendJSON(_ object: [String: Any], on fd: Int32) throws {
        guard JSONSerialization.isValidJSONObject(object),
              let data = try? JSONSerialization.data(withJSONObject: object, options: []),
              var payload = String(data: data, encoding: .utf8) else {
            throw DaemonSessionError.serializationFailed
        }
        payload += "\n"

        let success = payload.withCString { cString in
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
        guard success else {
            throw DaemonSessionError.writeFailed
        }
    }

    /// Read a single JSON line from the socket. Returns the parsed JSON and any
    /// remaining data that was read beyond the first newline, so the caller can
    /// feed it into the read loop without data loss.
    private func readJSONLine(from fd: Int32) throws -> ([String: Any], Data) {
        var accumulated = Data()
        var buffer = [UInt8](repeating: 0, count: 4096)

        while true {
            let count = read(fd, &buffer, buffer.count)
            if count <= 0 {
                throw DaemonSessionError.readFailed
            }
            accumulated.append(contentsOf: buffer[0..<count])

            // Scan for newline byte (0x0A).
            if let newlineIndex = accumulated.firstIndex(of: 0x0A) {
                let lineData = accumulated[accumulated.startIndex..<newlineIndex]
                let remainder = accumulated[accumulated.index(after: newlineIndex)...]

                guard let parsed = try? JSONSerialization.jsonObject(with: lineData, options: []) as? [String: Any] else {
                    throw DaemonSessionError.parseFailed
                }
                return (parsed, Data(remainder))
            }
        }
    }

    private func startReadThread() {
        let thread = Thread { [weak self] in
            self?.readLoop()
        }
        thread.name = "DaemonSessionBinding.read.\(sessionID.prefix(8))"
        thread.qualityOfService = .userInteractive

        lock.lock()
        _readThread = thread
        lock.unlock()

        thread.start()
    }

    private func readLoop() {
        defer {
            cancelAllPendingResponses()
            readThreadDone.signal()
        }

        var buffer = [UInt8](repeating: 0, count: 8192)
        var accumulated = Data()

        // Consume any data left over from readJSONLine.
        lock.lock()
        if !_pendingData.isEmpty {
            accumulated = _pendingData
            _pendingData = Data()
        }
        lock.unlock()

        // Process any complete lines already in the pending data.
        processCompleteLines(&accumulated)

        while true {
            // Check attached state and grab FD under lock.
            lock.lock()
            let attached = _isAttached
            let fd = _socketFD
            lock.unlock()

            guard attached, fd >= 0, !Thread.current.isCancelled else { break }

            let count = read(fd, &buffer, buffer.count)
            if count <= 0 {
                // Connection closed, shutdown, or error — mark detached.
                lock.lock()
                _isAttached = false
                lock.unlock()
                break
            }

            accumulated.append(contentsOf: buffer[0..<count])
            processCompleteLines(&accumulated)
        }
    }

    /// Extract and process all complete JSON lines (terminated by 0x0A) from the
    /// accumulated data buffer, leaving any incomplete trailing data in place.
    private func processCompleteLines(_ accumulated: inout Data) {
        while let newlineIndex = accumulated.firstIndex(of: 0x0A) {
            let lineData = accumulated[accumulated.startIndex..<newlineIndex]
            accumulated = Data(accumulated[accumulated.index(after: newlineIndex)...])

            guard !lineData.isEmpty,
                  let event = try? JSONSerialization.jsonObject(with: lineData, options: []) as? [String: Any] else {
                continue
            }

            processStreamEvent(event)
        }
    }

    private func processStreamEvent(_ event: [String: Any]) {
        // Check if this is a response to a pending RPC request.
        if let responseID = event["id"] as? Int,
           let handler = takePendingResponse(id: responseID) {
            handler(event)
            return
        }

        // Handle pty.replay (ring buffer replay on attach) and pty.output (live output).
        let method = event["method"] as? String
        let params = event["params"] as? [String: Any]

        switch method {
        case "pty.replay", "pty.output":
            guard let base64 = params?["data_base64"] as? String,
                  let data = Data(base64Encoded: base64) else {
                return
            }
            outputHandler(data)

        default:
            // Ignore unknown events (e.g., pty.exit).
            break
        }
    }
}

// MARK: - FIFO Bridge

/// Bridges daemon PTY I/O to a ghostty terminal surface using named pipes (FIFOs).
///
/// Architecture:
///   - Output FIFO: daemon PTY output → FIFO → `cat` (ghostty surface command) → ghostty renderer
///   - Input FIFO: ghostty key events → surface child stdin → `cat` → FIFO → daemon PTY input
///
/// The ghostty surface runs `/bin/sh -c 'cat > <input_fifo> & cat <output_fifo>'`
/// which creates a bidirectional bridge without modifying ghostty internals:
///   - `cat <output_fifo>` reads daemon output from the FIFO and writes to stdout (rendered by ghostty)
///   - `cat > <input_fifo>` captures keyboard input from stdin and writes to the input FIFO
///   - A reader thread on the input FIFO forwards data to `DaemonSessionBinding.sendInput()`
final class DaemonPTYBridge {
    let outputFIFOPath: String
    let inputFIFOPath: String

    /// Private temp directory containing the FIFOs (created with 0700 permissions).
    private let fifoDirectory: String

    /// The command to use as the ghostty surface's `initialCommand`.
    /// Ghostty spawns this instead of the default shell.
    var surfaceCommand: String {
        // The shell command sets up bidirectional I/O:
        //   1. Background: read stdin and write to input FIFO (forwards keyboard to daemon)
        //   2. Foreground: read output FIFO and write to stdout (displays daemon output)
        //
        // `cat > input_fifo` runs in background reading the surface's stdin (keyboard input
        // from ghostty's PTY) and writing to the input FIFO. `cat output_fifo` reads daemon
        // output from the output FIFO and writes to stdout for ghostty to render.
        //
        // When the output FIFO is closed (bridge teardown), `cat` exits, the shell exits,
        // and ghostty sees the child process terminate.
        //
        // FIFO paths are under a mkdtemp directory with sanitized session IDs, so they
        // contain only safe characters. We still shell-escape them for defense in depth.
        let escapedInput = shellEscape(inputFIFOPath)
        let escapedOutput = shellEscape(outputFIFOPath)
        return "/bin/sh -c 'cat >\(escapedInput) & cat \(escapedOutput)'"
    }

    /// Escape a string for safe embedding inside a single-quoted shell context.
    /// Ends the current single-quote, inserts an escaped literal single-quote,
    /// then reopens the single-quote: ' -> '\''
    func shellEscape(_ path: String) -> String {
        path.replacingOccurrences(of: "'", with: "'\\''")
    }

    private let lock = NSLock()
    private var _outputFD: Int32 = -1
    private var _inputReadThread: Thread?
    private var _isTornDown = false
    private var _inputReaderJoined = false
    private weak var binding: DaemonSessionBinding?

    /// Signaled by the input reader thread when it exits, so teardown can join.
    private let inputReaderDone = DispatchSemaphore(value: 0)

    /// Activation timeout in seconds. If the output FIFO cannot be opened within
    /// this duration (e.g., the surface process failed to launch), activate gives up.
    private static let activationTimeoutSeconds: Int = 10

    /// Sanitize a session ID to only contain safe characters (alphanumeric, dash,
    /// underscore). All other characters are replaced with underscores.
    static func sanitizeSessionID(_ sessionID: String) -> String {
        let allowed = CharacterSet.alphanumerics.union(CharacterSet(charactersIn: "-_"))
        return String(sessionID.unicodeScalars.map { allowed.contains($0) ? Character($0) : Character("_") })
    }

    /// Create a private temporary directory with 0700 permissions for FIFO files.
    /// Uses mkdtemp for atomicity. Returns the directory path.
    static func createPrivateTempDirectory(uid: uid_t, sanitizedID: String) -> String? {
        let parentDir = "/tmp/cmux-\(uid)"

        // Ensure parent directory exists with 0700 permissions.
        var isDir: ObjCBool = false
        if !FileManager.default.fileExists(atPath: parentDir, isDirectory: &isDir) {
            mkdir(parentDir, 0o700)
        }

        // Use mkdtemp to create a unique subdirectory.
        let template = parentDir + "/bridge-\(sanitizedID)-XXXXXX"
        var templateBytes = Array(template.utf8CString)
        guard let result = mkdtemp(&templateBytes) else {
            return nil
        }
        return String(cString: result)
    }

    /// Create a FIFO bridge for the given daemon session.
    /// - Parameter sessionID: Used to generate unique FIFO paths. Sanitized internally.
    /// - Parameter binding: The daemon session binding to forward input to.
    /// - Throws: If FIFO creation fails.
    init(sessionID: String, binding: DaemonSessionBinding) throws {
        let sanitized = Self.sanitizeSessionID(String(sessionID.prefix(12)))
        guard let dir = Self.createPrivateTempDirectory(uid: getuid(), sanitizedID: sanitized) else {
            throw DaemonBridgeError.fifoDirectoryCreationFailed
        }
        self.fifoDirectory = dir
        self.outputFIFOPath = dir + "/out.fifo"
        self.inputFIFOPath = dir + "/in.fifo"
        self.binding = binding
        try createFIFOs()
    }

    deinit {
        teardown()
    }

    // MARK: - Setup

    private func createFIFOs() throws {
        // Remove stale FIFOs from a previous session (should not exist in mkdtemp dir,
        // but be defensive).
        unlink(outputFIFOPath)
        unlink(inputFIFOPath)

        // Create FIFOs with owner-only permissions.
        guard mkfifo(outputFIFOPath, 0o600) == 0 else {
            // Clean up the directory since we can't proceed.
            rmdir(fifoDirectory)
            throw DaemonBridgeError.mkfifoFailed(outputFIFOPath, errno)
        }
        guard mkfifo(inputFIFOPath, 0o600) == 0 else {
            // Clean up the output FIFO and directory.
            unlink(outputFIFOPath)
            rmdir(fifoDirectory)
            throw DaemonBridgeError.mkfifoFailed(inputFIFOPath, errno)
        }
    }

    /// Open the output FIFO for writing and start the input reader thread.
    /// Must be called AFTER the terminal surface has been created and its child
    /// process has opened the FIFO for reading (otherwise `open` blocks).
    ///
    /// This method is designed to be called from a background thread since
    /// opening a FIFO blocks until the other end opens it too.
    ///
    /// Uses O_WRONLY|O_NONBLOCK with a retry loop and timeout to avoid blocking
    /// a GCD thread forever if the surface process fails to launch.
    func activate() {
        lock.lock()
        guard !_isTornDown else { lock.unlock(); return }
        lock.unlock()

        // Open the output FIFO for writing with a timeout. O_WRONLY|O_NONBLOCK
        // returns ENXIO if the read end isn't open yet, so we retry in a loop.
        let deadline = DispatchTime.now() + .seconds(Self.activationTimeoutSeconds)
        var outFD: Int32 = -1

        while DispatchTime.now() < deadline {
            // Check for teardown between retries.
            lock.lock()
            if _isTornDown { lock.unlock(); return }
            lock.unlock()

            outFD = open(outputFIFOPath, O_WRONLY | O_NONBLOCK)
            if outFD >= 0 {
                break
            }
            if errno == ENXIO {
                // Read end not open yet; wait 50ms and retry.
                usleep(50_000)
                continue
            }
            // Some other error (e.g., file deleted by teardown).
            return
        }

        guard outFD >= 0 else {
            // Timed out waiting for surface to open the FIFO.
            teardown()
            return
        }

        // Clear O_NONBLOCK now that the FIFO is connected, so writes block normally.
        let flags = fcntl(outFD, F_GETFL)
        if flags >= 0 {
            _ = fcntl(outFD, F_SETFL, flags & ~O_NONBLOCK)
        }

        // Disable SIGPIPE on this fd. FIFOs use fcntl, not setsockopt.
        _ = fcntl(outFD, F_SETNOSIGPIPE, 1)

        // Re-check teardown under lock after the potentially long open.
        // If teardown() ran while we were waiting, close the FD to avoid a leak.
        lock.lock()
        if _isTornDown {
            lock.unlock()
            Darwin.close(outFD)
            return
        }
        _outputFD = outFD
        lock.unlock()

        startInputReaderThread()
    }

    // MARK: - Output (daemon → surface)

    /// Write raw PTY output data to the output FIFO.
    /// Called from the `DaemonSessionBinding` read thread.
    ///
    /// On write failure (broken pipe, etc.), logs the error and triggers teardown
    /// to prevent the bridge from entering a zombie state.
    func writeOutput(_ data: Data) {
        lock.lock()
        let fd = _outputFD
        guard fd >= 0, !_isTornDown else { lock.unlock(); return }
        lock.unlock()

        var writeFailed = false

        data.withUnsafeBytes { rawBuffer in
            guard var ptr = rawBuffer.baseAddress else { return }
            var remaining = rawBuffer.count
            while remaining > 0 {
                let written = write(fd, ptr, remaining)
                if written <= 0 {
                    writeFailed = true
                    break
                }
                remaining -= written
                ptr = ptr.advanced(by: written)
            }
        }

        if writeFailed {
            #if DEBUG
            dlog("DaemonPTYBridge.writeOutput: write failed (errno \(errno)), triggering teardown")
            #endif
            teardown()
        }
    }

    // MARK: - Input (surface → daemon)

    private func startInputReaderThread() {
        let thread = Thread { [weak self] in
            self?.inputReadLoop()
        }
        thread.name = "DaemonPTYBridge.inputReader"
        thread.qualityOfService = .userInteractive

        lock.lock()
        _inputReadThread = thread
        lock.unlock()

        thread.start()
    }

    private func inputReadLoop() {
        defer {
            inputReaderDone.signal()
        }

        // Open the input FIFO for reading. By the time this runs, the surface's
        // `cat > <input_fifo>` (background) should already have the write end open
        // because activate() waits for the foreground `cat <output_fifo>` first, and
        // the shell starts the background process before the foreground one.
        // If the writer hasn't connected yet, this blocks briefly until it does.
        //
        // Teardown sends EOF by opening and immediately closing the write end,
        // which unblocks any pending read().
        let inFD = open(inputFIFOPath, O_RDONLY)
        guard inFD >= 0 else { return }
        defer { Darwin.close(inFD) }

        var buffer = [UInt8](repeating: 0, count: 4096)
        while !Thread.current.isCancelled {
            let count = read(inFD, &buffer, buffer.count)
            if count <= 0 { break }
            let data = Data(buffer[0..<count])
            binding?.sendInput(data)
        }
    }

    // MARK: - Teardown

    func teardown() {
        lock.lock()
        guard !_isTornDown else { lock.unlock(); return }
        _isTornDown = true
        let outFD = _outputFD
        _outputFD = -1
        let thread = _inputReadThread
        _inputReadThread = nil
        let shouldJoinInput = thread != nil && !_inputReaderJoined
        if shouldJoinInput { _inputReaderJoined = true }
        lock.unlock()

        // Close the output FIFO write end — causes `cat` to see EOF and exit.
        if outFD >= 0 {
            Darwin.close(outFD)
        }

        // Cancel the input reader thread and unblock it.
        // If the reader is blocked on read(), opening the write end and closing
        // it delivers EOF. If the reader is still blocked on open() (rare race),
        // the O_NONBLOCK open may fail with ENXIO; the subsequent unlink ensures
        // the thread exits once the kernel releases the path reference.
        thread?.cancel()
        let kickFD = open(inputFIFOPath, O_WRONLY | O_NONBLOCK)
        if kickFD >= 0 {
            Darwin.close(kickFD)
        }

        // Wait for the input reader thread to finish.
        if shouldJoinInput {
            inputReaderDone.wait()
        }

        // Clean up FIFO files and private temp directory.
        unlink(outputFIFOPath)
        unlink(inputFIFOPath)
        rmdir(fifoDirectory)
    }
}

// MARK: - Bridge Errors

enum DaemonBridgeError: Error, LocalizedError {
    case fifoDirectoryCreationFailed
    case mkfifoFailed(String, Int32)

    var errorDescription: String? {
        switch self {
        case .fifoDirectoryCreationFailed:
            return "Failed to create private temp directory for FIFO bridge"
        case .mkfifoFailed(let path, let code):
            return "mkfifo failed for \(path) (errno \(code))"
        }
    }
}

// MARK: - Errors

enum DaemonSessionError: Error, LocalizedError {
    case socketCreateFailed
    case socketPathTooLong
    case connectFailed(Int32)
    case serializationFailed
    case writeFailed
    case readFailed
    case parseFailed
    case attachFailed(String)

    var errorDescription: String? {
        switch self {
        case .socketCreateFailed: return "Failed to create Unix socket"
        case .socketPathTooLong: return "Daemon socket path too long"
        case .connectFailed(let code): return "Failed to connect to daemon (errno \(code))"
        case .serializationFailed: return "Failed to serialize RPC request"
        case .writeFailed: return "Failed to write to daemon socket"
        case .readFailed: return "Failed to read from daemon socket"
        case .parseFailed: return "Failed to parse daemon response"
        case .attachFailed(let msg): return "Daemon attach failed: \(msg)"
        }
    }
}
