import XCTest

#if canImport(cmux_DEV)
@testable import cmux_DEV
#elseif canImport(cmux)
@testable import cmux
#endif

// MARK: - LocalDaemonManager Path Computation Tests

final class LocalDaemonManagerPathTests: XCTestCase {
    func testStateDirIsUnderHomeDotLocalState() {
        let stateDir = LocalDaemonManager.stateDir
        let home = NSHomeDirectory()
        XCTAssertEqual(stateDir, home + "/.local/state/cmux")
    }

    func testSocketPathIsInsideStateDir() {
        let socketPath = LocalDaemonManager.socketPath
        let stateDir = LocalDaemonManager.stateDir
        XCTAssertTrue(socketPath.hasPrefix(stateDir + "/"))
        XCTAssertTrue(socketPath.hasSuffix(".sock"))
    }

    func testLegacySocketPathContainsUID() {
        let legacyPath = LocalDaemonManager.legacySocketPath
        let uid = getuid()
        XCTAssertTrue(legacyPath.hasPrefix("/tmp/"))
        XCTAssertTrue(legacyPath.contains("\(uid)"))
        XCTAssertTrue(legacyPath.hasSuffix(".sock"))
    }

    func testLaunchAgentPlistPathIsUnderHomeLibraryLaunchAgents() {
        let plistPath = LocalDaemonManager.launchAgentPlistPath
        let home = NSHomeDirectory()
        XCTAssertTrue(plistPath.hasPrefix(home + "/Library/LaunchAgents/"))
        XCTAssertTrue(plistPath.hasSuffix(".plist"))
    }

    func testResolvedSocketPathFallsThroughToPrimaryWhenNeitherExists() {
        // When neither primary nor legacy socket exists on disk,
        // resolvedSocketPath should return the primary path.
        // (In a test environment, neither daemon socket should exist.)
        let resolved = LocalDaemonManager.resolvedSocketPath
        let primary = LocalDaemonManager.socketPath
        let legacy = LocalDaemonManager.legacySocketPath

        // If neither file exists, resolved should equal primary.
        let fm = FileManager.default
        if !fm.fileExists(atPath: primary) && !fm.fileExists(atPath: legacy) {
            XCTAssertEqual(resolved, primary)
        } else if fm.fileExists(atPath: primary) {
            XCTAssertEqual(resolved, primary)
        } else {
            XCTAssertEqual(resolved, legacy)
        }
    }

    func testSocketPathAndLegacyPathAreDifferent() {
        // Primary and legacy should never collide.
        XCTAssertNotEqual(LocalDaemonManager.socketPath, LocalDaemonManager.legacySocketPath)
    }
}

// MARK: - LaunchAgentError Tests

final class LaunchAgentErrorTests: XCTestCase {
    func testErrorDescriptionReturnsAssociatedMessage() {
        let cases: [(LaunchAgentError, String)] = [
            (.binaryNotFound("not found"), "not found"),
            (.templateNotFound("no template"), "no template"),
            (.writeFailed("write error"), "write error"),
            (.bootstrapFailed("bootstrap error"), "bootstrap error"),
        ]

        for (error, expected) in cases {
            XCTAssertEqual(error.errorDescription, expected)
        }
    }
}

// MARK: - DaemonSessionError Tests

final class DaemonSessionErrorTests: XCTestCase {
    func testAllCasesHaveNonEmptyDescriptions() {
        let errors: [DaemonSessionError] = [
            .socketCreateFailed,
            .socketPathTooLong,
            .connectFailed(ECONNREFUSED),
            .serializationFailed,
            .writeFailed,
            .readFailed,
            .parseFailed,
            .attachFailed("timeout"),
        ]

        for error in errors {
            let description = error.errorDescription
            XCTAssertNotNil(description)
            XCTAssertFalse(description?.isEmpty ?? true, "Error \(error) should have a non-empty description")
        }
    }

    func testConnectFailedIncludesErrno() {
        let error = DaemonSessionError.connectFailed(61)
        XCTAssertTrue(error.errorDescription?.contains("61") ?? false)
    }

    func testAttachFailedIncludesMessage() {
        let error = DaemonSessionError.attachFailed("session not found")
        XCTAssertTrue(error.errorDescription?.contains("session not found") ?? false)
    }
}

// MARK: - DaemonBridgeError Tests

final class DaemonBridgeErrorTests: XCTestCase {
    func testMkfifoFailedIncludesPathAndErrno() {
        let error = DaemonBridgeError.mkfifoFailed("/tmp/test.fifo", 13)
        let description = error.errorDescription ?? ""
        XCTAssertTrue(description.contains("/tmp/test.fifo"))
        XCTAssertTrue(description.contains("13"))
    }

    func testFifoDirectoryCreationFailedHasDescription() {
        let error = DaemonBridgeError.fifoDirectoryCreationFailed
        XCTAssertNotNil(error.errorDescription)
        XCTAssertFalse(error.errorDescription?.isEmpty ?? true)
    }
}

// MARK: - DaemonSessionBinding Tests

final class DaemonSessionBindingTests: XCTestCase {
    func testInitStoresSessionIDAndName() {
        let binding = DaemonSessionBinding(
            sessionID: "abc-123",
            sessionName: "my-session",
            outputHandler: { _ in }
        )

        XCTAssertEqual(binding.sessionID, "abc-123")
        XCTAssertEqual(binding.sessionName, "my-session")
    }

    func testAllocateRPCIDIsMonotonicallyIncreasing() {
        let binding = DaemonSessionBinding(
            sessionID: "test",
            sessionName: "test",
            outputHandler: { _ in }
        )

        let id1 = binding.allocateRPCID()
        let id2 = binding.allocateRPCID()
        let id3 = binding.allocateRPCID()

        XCTAssertEqual(id1, 1)
        XCTAssertEqual(id2, 2)
        XCTAssertEqual(id3, 3)
    }

    func testAllocateRPCIDStartsFromOneOnFreshBinding() {
        let binding = DaemonSessionBinding(
            sessionID: "fresh",
            sessionName: "fresh",
            outputHandler: { _ in }
        )

        XCTAssertEqual(binding.allocateRPCID(), 1)
    }

    func testAllocateRPCIDIsThreadSafe() {
        let binding = DaemonSessionBinding(
            sessionID: "concurrent",
            sessionName: "concurrent",
            outputHandler: { _ in }
        )

        let iterations = 100
        let expectation = self.expectation(description: "all IDs allocated")
        expectation.expectedFulfillmentCount = iterations

        var collectedIDs = [Int]()
        let lock = NSLock()

        for _ in 0..<iterations {
            DispatchQueue.global().async {
                let id = binding.allocateRPCID()
                lock.lock()
                collectedIDs.append(id)
                lock.unlock()
                expectation.fulfill()
            }
        }

        waitForExpectations(timeout: 5)

        // All IDs should be unique.
        let uniqueIDs = Set(collectedIDs)
        XCTAssertEqual(uniqueIDs.count, iterations, "All RPC IDs should be unique")

        // IDs should cover the range 1...iterations.
        XCTAssertEqual(uniqueIDs.min(), 1)
        XCTAssertEqual(uniqueIDs.max(), iterations)
    }
}

// MARK: - DaemonPTYBridge Tests

final class DaemonPTYBridgeTests: XCTestCase {
    func testSanitizeSessionIDStripsUnsafeCharacters() {
        // Alphanumeric, dash, and underscore should be preserved.
        XCTAssertEqual(
            DaemonPTYBridge.sanitizeSessionID("abc-123_DEF"),
            "abc-123_DEF"
        )
    }

    func testSanitizeSessionIDReplacesSlashesAndSpaces() {
        XCTAssertEqual(
            DaemonPTYBridge.sanitizeSessionID("path/to session"),
            "path_to_session"
        )
    }

    func testSanitizeSessionIDReplacesDotsAndSpecialCharacters() {
        XCTAssertEqual(
            DaemonPTYBridge.sanitizeSessionID("a.b@c#d$e%f"),
            "a_b_c_d_e_f"
        )
    }

    func testSanitizeSessionIDHandlesEmptyString() {
        XCTAssertEqual(DaemonPTYBridge.sanitizeSessionID(""), "")
    }

    func testSanitizeSessionIDHandlesUnicodeCharacters() {
        // Unicode letters outside ASCII alphanumerics should be replaced.
        let result = DaemonPTYBridge.sanitizeSessionID("cafe\u{0301}")
        // "cafe" is ASCII, \u{0301} (combining acute) is not alphanumeric
        XCTAssertFalse(result.contains("\u{0301}"))
    }

    func testSanitizeSessionIDPreservesAllDigits() {
        XCTAssertEqual(
            DaemonPTYBridge.sanitizeSessionID("0123456789"),
            "0123456789"
        )
    }

    func testCreatePrivateTempDirectoryReturnsValidPath() {
        let uid = getuid()
        guard let dir = DaemonPTYBridge.createPrivateTempDirectory(uid: uid, sanitizedID: "test") else {
            XCTFail("Expected temp directory to be created")
            return
        }
        defer {
            rmdir(dir)
            // Also try to remove parent (may fail if other dirs exist, that's fine).
            rmdir("/tmp/cmux-\(uid)")
        }

        XCTAssertTrue(dir.hasPrefix("/tmp/cmux-\(uid)/"))
        XCTAssertTrue(dir.contains("bridge-test-"))

        var isDir: ObjCBool = false
        XCTAssertTrue(FileManager.default.fileExists(atPath: dir, isDirectory: &isDir))
        XCTAssertTrue(isDir.boolValue)
    }

    func testCreatePrivateTempDirectoryHasRestrictedPermissions() {
        let uid = getuid()
        guard let dir = DaemonPTYBridge.createPrivateTempDirectory(uid: uid, sanitizedID: "perms") else {
            XCTFail("Expected temp directory to be created")
            return
        }
        defer {
            rmdir(dir)
            rmdir("/tmp/cmux-\(uid)")
        }

        let parentDir = "/tmp/cmux-\(uid)"
        guard let attrs = try? FileManager.default.attributesOfItem(atPath: parentDir) else {
            XCTFail("Expected to read parent directory attributes")
            return
        }
        let permissions = (attrs[.posixPermissions] as? Int) ?? 0
        // 0700 = 448 decimal
        XCTAssertEqual(permissions & 0o777, 0o700, "Parent directory should have 0700 permissions")
    }

    func testCreatePrivateTempDirectoryCreatesUniqueDirectories() {
        let uid = getuid()
        guard let dir1 = DaemonPTYBridge.createPrivateTempDirectory(uid: uid, sanitizedID: "unique"),
              let dir2 = DaemonPTYBridge.createPrivateTempDirectory(uid: uid, sanitizedID: "unique") else {
            XCTFail("Expected temp directories to be created")
            return
        }
        defer {
            rmdir(dir1)
            rmdir(dir2)
            rmdir("/tmp/cmux-\(uid)")
        }

        XCTAssertNotEqual(dir1, dir2, "mkdtemp should produce unique directories each time")
    }

    func testBridgeInitCreatesFIFOs() throws {
        let binding = DaemonSessionBinding(
            sessionID: "fifo-test-123",
            sessionName: "test",
            outputHandler: { _ in }
        )

        let bridge = try DaemonPTYBridge(sessionID: "fifo-test-123", binding: binding)
        defer { bridge.teardown() }

        // Verify FIFOs exist on disk.
        var isDir: ObjCBool = false
        XCTAssertTrue(FileManager.default.fileExists(atPath: bridge.outputFIFOPath, isDirectory: &isDir))
        XCTAssertFalse(isDir.boolValue)
        XCTAssertTrue(FileManager.default.fileExists(atPath: bridge.inputFIFOPath, isDirectory: &isDir))
        XCTAssertFalse(isDir.boolValue)

        // Verify FIFO paths are in expected location.
        XCTAssertTrue(bridge.outputFIFOPath.hasSuffix("/out.fifo"))
        XCTAssertTrue(bridge.inputFIFOPath.hasSuffix("/in.fifo"))
    }

    func testBridgeFIFOsHaveOwnerOnlyPermissions() throws {
        let binding = DaemonSessionBinding(
            sessionID: "perms-test",
            sessionName: "test",
            outputHandler: { _ in }
        )

        let bridge = try DaemonPTYBridge(sessionID: "perms-test", binding: binding)
        defer { bridge.teardown() }

        for path in [bridge.outputFIFOPath, bridge.inputFIFOPath] {
            guard let attrs = try? FileManager.default.attributesOfItem(atPath: path) else {
                XCTFail("Expected to read FIFO attributes for \(path)")
                continue
            }
            let permissions = (attrs[.posixPermissions] as? Int) ?? 0
            // 0600 = 384 decimal
            XCTAssertEqual(
                permissions & 0o777,
                0o600,
                "FIFO at \(path) should have 0600 permissions"
            )
        }
    }

    func testBridgeTeardownRemovesFIFOsAndDirectory() throws {
        let binding = DaemonSessionBinding(
            sessionID: "teardown-test",
            sessionName: "test",
            outputHandler: { _ in }
        )

        let bridge = try DaemonPTYBridge(sessionID: "teardown-test", binding: binding)
        let outputPath = bridge.outputFIFOPath
        let inputPath = bridge.inputFIFOPath
        // The directory is the parent of the FIFO paths.
        let dirPath = (outputPath as NSString).deletingLastPathComponent

        // Verify files exist before teardown.
        XCTAssertTrue(FileManager.default.fileExists(atPath: outputPath))
        XCTAssertTrue(FileManager.default.fileExists(atPath: inputPath))

        bridge.teardown()

        // After teardown, FIFOs and directory should be removed.
        XCTAssertFalse(FileManager.default.fileExists(atPath: outputPath))
        XCTAssertFalse(FileManager.default.fileExists(atPath: inputPath))
        XCTAssertFalse(FileManager.default.fileExists(atPath: dirPath))
    }

    func testBridgeTeardownIsIdempotent() throws {
        let binding = DaemonSessionBinding(
            sessionID: "idempotent-test",
            sessionName: "test",
            outputHandler: { _ in }
        )

        let bridge = try DaemonPTYBridge(sessionID: "idempotent-test", binding: binding)

        // Calling teardown multiple times should not crash or error.
        bridge.teardown()
        bridge.teardown()
        bridge.teardown()
    }

    func testSurfaceCommandContainsFIFOPaths() throws {
        let binding = DaemonSessionBinding(
            sessionID: "cmd-test",
            sessionName: "test",
            outputHandler: { _ in }
        )

        let bridge = try DaemonPTYBridge(sessionID: "cmd-test", binding: binding)
        defer { bridge.teardown() }

        let command = bridge.surfaceCommand
        XCTAssertTrue(command.hasPrefix("/bin/sh -c '"))
        XCTAssertTrue(command.contains(bridge.inputFIFOPath))
        XCTAssertTrue(command.contains(bridge.outputFIFOPath))
        // Should contain the bidirectional cat piping.
        XCTAssertTrue(command.contains("cat >"))
        XCTAssertTrue(command.contains("cat "))
        XCTAssertTrue(command.contains("&"))
    }

    func testSurfaceCommandShellEscapesSingleQuotes() throws {
        let binding = DaemonSessionBinding(
            sessionID: "escape-test",
            sessionName: "test",
            outputHandler: { _ in }
        )

        let bridge = try DaemonPTYBridge(sessionID: "escape-test", binding: binding)
        defer { bridge.teardown() }

        // The shellEscape method should handle single quotes properly.
        let escaped = bridge.shellEscape("it's a test")
        XCTAssertEqual(escaped, "it'\\''s a test")
    }

    func testShellEscapeNoOpForSafePaths() throws {
        let binding = DaemonSessionBinding(
            sessionID: "safe-path-test",
            sessionName: "test",
            outputHandler: { _ in }
        )

        let bridge = try DaemonPTYBridge(sessionID: "safe-path-test", binding: binding)
        defer { bridge.teardown() }

        let safePath = "/tmp/cmux-501/bridge-abc123/out.fifo"
        XCTAssertEqual(bridge.shellEscape(safePath), safePath)
    }

    func testBridgeSessionIDIsTruncatedToTwelveCharacters() throws {
        let binding = DaemonSessionBinding(
            sessionID: "abcdefghijklmnopqrstuvwxyz",
            sessionName: "test",
            outputHandler: { _ in }
        )

        let bridge = try DaemonPTYBridge(
            sessionID: "abcdefghijklmnopqrstuvwxyz",
            binding: binding
        )
        defer { bridge.teardown() }

        // The FIFO directory should contain the first 12 characters of the sanitized ID.
        let dirPath = (bridge.outputFIFOPath as NSString).deletingLastPathComponent
        let dirName = (dirPath as NSString).lastPathComponent
        XCTAssertTrue(dirName.hasPrefix("bridge-abcdefghijkl-"),
                       "Directory name should use truncated session ID: \(dirName)")
        // Should NOT contain characters beyond position 12.
        XCTAssertFalse(dirName.contains("mnop"),
                        "Directory name should not contain characters past 12: \(dirName)")
    }
}

// MARK: - Session Persistence Daemon ID Tests

final class SessionPersistenceDaemonTests: XCTestCase {
    func testDaemonSessionIDRoundTripSerializesCorrectly() throws {
        let workspace = SessionWorkspaceSnapshot(
            processTitle: "Terminal",
            customTitle: nil,
            customColor: nil,
            isPinned: false,
            currentDirectory: "/tmp",
            focusedPanelId: nil,
            layout: .pane(SessionPaneLayoutSnapshot(panelIds: [], selectedPanelId: nil)),
            panels: [],
            statusEntries: [],
            logEntries: [],
            progress: nil,
            gitBranch: nil,
            daemonSessionID: "daemon-abc-123"
        )

        let encoder = JSONEncoder()
        encoder.outputFormatting = [.sortedKeys]
        let data = try encoder.encode(workspace)

        let decoded = try JSONDecoder().decode(SessionWorkspaceSnapshot.self, from: data)
        XCTAssertEqual(decoded.daemonSessionID, "daemon-abc-123")
    }

    func testDaemonSessionIDNilRoundTrip() throws {
        let workspace = SessionWorkspaceSnapshot(
            processTitle: "Terminal",
            customTitle: nil,
            customColor: nil,
            isPinned: false,
            currentDirectory: "/tmp",
            focusedPanelId: nil,
            layout: .pane(SessionPaneLayoutSnapshot(panelIds: [], selectedPanelId: nil)),
            panels: [],
            statusEntries: [],
            logEntries: [],
            progress: nil,
            gitBranch: nil,
            daemonSessionID: nil
        )

        let encoder = JSONEncoder()
        let data = try encoder.encode(workspace)

        let decoded = try JSONDecoder().decode(SessionWorkspaceSnapshot.self, from: data)
        XCTAssertNil(decoded.daemonSessionID)
    }

    func testDaemonSessionIDDecodesFromLegacyJSONWithoutField() throws {
        // Simulate JSON from before daemonSessionID was added — the field should
        // decode as nil without errors.
        let json = """
        {
            "processTitle": "Terminal",
            "isPinned": false,
            "currentDirectory": "/tmp",
            "layout": {
                "type": "pane",
                "pane": { "panelIds": [], "selectedPanelId": null }
            },
            "panels": [],
            "statusEntries": [],
            "logEntries": []
        }
        """.data(using: .utf8)!

        let decoded = try JSONDecoder().decode(SessionWorkspaceSnapshot.self, from: json)
        XCTAssertNil(decoded.daemonSessionID)
        XCTAssertEqual(decoded.processTitle, "Terminal")
    }

    func testFullSnapshotRoundTripWithDaemonSessionID() throws {
        let workspace = SessionWorkspaceSnapshot(
            processTitle: "zsh",
            customTitle: "Dev",
            customColor: "#FF0000",
            isPinned: true,
            currentDirectory: "/Users/test",
            focusedPanelId: nil,
            layout: .pane(SessionPaneLayoutSnapshot(panelIds: [], selectedPanelId: nil)),
            panels: [],
            statusEntries: [],
            logEntries: [],
            progress: nil,
            gitBranch: nil,
            daemonSessionID: "sess-42"
        )

        let tabManager = SessionTabManagerSnapshot(
            selectedWorkspaceIndex: 0,
            workspaces: [workspace]
        )

        let window = SessionWindowSnapshot(
            frame: SessionRectSnapshot(x: 0, y: 0, width: 800, height: 600),
            display: nil,
            tabManager: tabManager,
            sidebar: SessionSidebarSnapshot(isVisible: true, selection: .tabs, width: 200)
        )

        let snapshot = AppSessionSnapshot(
            version: SessionSnapshotSchema.currentVersion,
            createdAt: Date().timeIntervalSince1970,
            windows: [window]
        )

        // Write to temp file and reload.
        let tempDir = FileManager.default.temporaryDirectory
            .appendingPathComponent("cmux-daemon-session-tests-\(UUID().uuidString)", isDirectory: true)
        try FileManager.default.createDirectory(at: tempDir, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: tempDir) }

        let fileURL = tempDir.appendingPathComponent("session.json")
        XCTAssertTrue(SessionPersistenceStore.save(snapshot, fileURL: fileURL))

        let loaded = SessionPersistenceStore.load(fileURL: fileURL)
        XCTAssertNotNil(loaded)
        XCTAssertEqual(
            loaded?.windows.first?.tabManager.workspaces.first?.daemonSessionID,
            "sess-42"
        )
        XCTAssertEqual(
            loaded?.windows.first?.tabManager.workspaces.first?.customTitle,
            "Dev"
        )
    }
}
