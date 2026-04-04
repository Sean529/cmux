package local

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestManager creates a SessionManager with a helper to create sessions using /bin/sh.
func newTestManager(t *testing.T) *SessionManager {
	t.Helper()
	sm := NewSessionManager()
	t.Cleanup(func() { sm.CloseAll() })
	return sm
}

func createTestSession(t *testing.T, sm *SessionManager, name string) *Session {
	t.Helper()
	sess, err := sm.Create(NewSessionParams{
		Name:  name,
		Shell: "/bin/sh",
		Cols:  80,
		Rows:  24,
	})
	if err != nil {
		t.Fatalf("failed to create session %q: %v", name, err)
	}
	return sess
}

// waitForOutput polls a pane's ring buffer until it contains the target string or times out.
func waitForOutput(t *testing.T, p *Pane, target string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data := p.ring.Bytes()
		if strings.Contains(string(data), target) {
			return string(data)
		}
		time.Sleep(20 * time.Millisecond)
	}
	data := p.ring.Bytes()
	t.Fatalf("timed out waiting for %q in ring buffer (got %d bytes: %q)", target, len(data), string(data))
	return ""
}

// -------------------------------------------------------------------------
// 1. Session lifecycle
// -------------------------------------------------------------------------

func TestSessionLifecycle_CreateListClose(t *testing.T) {
	sm := newTestManager(t)

	sess := createTestSession(t, sm, "test-session")

	// Verify it appears in list.
	list := sm.List()
	if len(list) != 1 {
		t.Fatalf("expected 1 session, got %d", len(list))
	}
	if list[0]["name"] != "test-session" {
		t.Fatalf("expected session name %q, got %q", "test-session", list[0]["name"])
	}

	// Close it.
	if !sm.Remove(sess.ID) {
		t.Fatal("Remove returned false for existing session")
	}

	// Verify gone.
	list = sm.List()
	if len(list) != 0 {
		t.Fatalf("expected 0 sessions after remove, got %d", len(list))
	}
}

func TestSessionLifecycle_CreateMultiple(t *testing.T) {
	sm := newTestManager(t)

	s1 := createTestSession(t, sm, "alpha")
	s2 := createTestSession(t, sm, "beta")
	s3 := createTestSession(t, sm, "gamma")

	list := sm.List()
	if len(list) != 3 {
		t.Fatalf("expected 3 sessions, got %d", len(list))
	}

	// Verify we can find each by name.
	for _, name := range []string{"alpha", "beta", "gamma"} {
		found, ok := sm.GetByName(name)
		if !ok {
			t.Fatalf("session %q not found by name", name)
		}
		if found.Name != name {
			t.Fatalf("expected name %q, got %q", name, found.Name)
		}
	}

	// IDs are distinct.
	if s1.ID == s2.ID || s2.ID == s3.ID {
		t.Fatal("session IDs are not unique")
	}
}

func TestSessionLifecycle_CloseNonExistent(t *testing.T) {
	sm := newTestManager(t)

	if sm.Remove("no-such-session") {
		t.Fatal("Remove returned true for non-existent session")
	}
}

// -------------------------------------------------------------------------
// 2. Ring buffer and replay
// -------------------------------------------------------------------------

func TestRingBuffer_WriteAndReplay(t *testing.T) {
	sm := newTestManager(t)
	sess := createTestSession(t, sm, "replay-test")

	pane := sess.ActivePane()
	if pane == nil {
		t.Fatal("no active pane")
	}

	// Write a command to the PTY and wait for output.
	if err := pane.WriteInput([]byte("echo HELLO_REPLAY\n")); err != nil {
		t.Fatalf("WriteInput: %v", err)
	}

	output := waitForOutput(t, pane, "HELLO_REPLAY", 5*time.Second)
	if !strings.Contains(output, "HELLO_REPLAY") {
		t.Fatalf("ring buffer does not contain expected output, got: %q", output)
	}

	// Attach a new client and verify replay contains the output.
	var buf bytes.Buffer
	fw := NewFrameWriter(&buf)
	replay := pane.Attach("test-client", fw)
	defer pane.Detach("test-client")

	if !strings.Contains(string(replay), "HELLO_REPLAY") {
		t.Fatalf("replay does not contain expected output, got %d bytes: %q", len(replay), string(replay))
	}
}

func TestRingBuffer_DoesNotExceedSize(t *testing.T) {
	rb := NewRingBuffer(ringBufferSize)

	// Write 3MB of data into a 2MB ring buffer.
	chunk := bytes.Repeat([]byte("A"), 1024*1024)
	rb.Write(chunk) // 1MB
	rb.Write(chunk) // 2MB
	rb.Write(chunk) // 3MB total, but buffer should cap at 2MB

	data := rb.Bytes()
	if len(data) != ringBufferSize {
		t.Fatalf("expected ring buffer len %d, got %d", ringBufferSize, len(data))
	}
}

// -------------------------------------------------------------------------
// 3. Attach / detach
// -------------------------------------------------------------------------

func TestAttachDetach_SessionSurvivesDetach(t *testing.T) {
	sm := newTestManager(t)
	sess := createTestSession(t, sm, "attach-test")

	pane := sess.ActivePane()
	if pane == nil {
		t.Fatal("no active pane")
	}

	var buf bytes.Buffer
	fw := NewFrameWriter(&buf)
	pane.Attach("client-A", fw)

	if pane.AttachedCount() != 1 {
		t.Fatalf("expected 1 attached, got %d", pane.AttachedCount())
	}

	pane.Detach("client-A")

	if pane.AttachedCount() != 0 {
		t.Fatalf("expected 0 attached after detach, got %d", pane.AttachedCount())
	}

	// Session should still be alive.
	if sess.Status != SessionRunning {
		t.Fatal("session died after client detach")
	}
}

func TestAttachDetach_MultipleClients(t *testing.T) {
	sm := newTestManager(t)
	sess := createTestSession(t, sm, "multi-attach")

	pane := sess.ActivePane()
	if pane == nil {
		t.Fatal("no active pane")
	}

	var bufA, bufB bytes.Buffer
	fwA := NewFrameWriter(&bufA)
	fwB := NewFrameWriter(&bufB)

	pane.Attach("client-A", fwA)
	pane.Attach("client-B", fwB)

	if pane.AttachedCount() != 2 {
		t.Fatalf("expected 2 attached, got %d", pane.AttachedCount())
	}

	// Write something and verify both clients receive output events.
	if err := pane.WriteInput([]byte("echo MULTI_OUT\n")); err != nil {
		t.Fatalf("WriteInput: %v", err)
	}

	waitForOutput(t, pane, "MULTI_OUT", 5*time.Second)

	// Give a moment for fan-out writes to buffers.
	time.Sleep(100 * time.Millisecond)

	if bufA.Len() == 0 {
		t.Fatal("client A received no output")
	}
	if bufB.Len() == 0 {
		t.Fatal("client B received no output")
	}
}

func TestAttachDetach_DetachAllSessionPersists(t *testing.T) {
	sm := newTestManager(t)
	sess := createTestSession(t, sm, "detach-all")

	pane := sess.ActivePane()
	if pane == nil {
		t.Fatal("no active pane")
	}

	var buf1, buf2 bytes.Buffer
	pane.Attach("c1", NewFrameWriter(&buf1))
	pane.Attach("c2", NewFrameWriter(&buf2))

	pane.Detach("c1")
	pane.Detach("c2")

	if pane.AttachedCount() != 0 {
		t.Fatalf("expected 0 attached, got %d", pane.AttachedCount())
	}

	// Session and pane should still be running.
	if sess.Status != SessionRunning {
		t.Fatal("session died after all clients detached")
	}
	pane.mu.Lock()
	status := pane.Status
	pane.mu.Unlock()
	if status != SessionRunning {
		t.Fatal("pane died after all clients detached")
	}
}

// -------------------------------------------------------------------------
// 4. Window / pane management
// -------------------------------------------------------------------------

func TestWindowPaneManagement_AddWindow(t *testing.T) {
	sm := newTestManager(t)
	sess := createTestSession(t, sm, "win-test")

	// Should start with 1 window, 1 pane.
	if sess.WindowCount() != 1 {
		t.Fatalf("expected 1 window, got %d", sess.WindowCount())
	}
	if sess.TotalPaneCount() != 1 {
		t.Fatalf("expected 1 pane, got %d", sess.TotalPaneCount())
	}

	// Add a second window.
	w2, p2, err := sess.AddWindow("second", "/bin/sh", 80, 24, nil)
	if err != nil {
		t.Fatalf("AddWindow: %v", err)
	}
	if w2 == nil || p2 == nil {
		t.Fatal("AddWindow returned nil window or pane")
	}

	if sess.WindowCount() != 2 {
		t.Fatalf("expected 2 windows, got %d", sess.WindowCount())
	}

	// ListWindows should return 2 snapshots.
	winList := sess.ListWindows()
	if len(winList) != 2 {
		t.Fatalf("expected 2 window snapshots, got %d", len(winList))
	}
}

func TestWindowPaneManagement_SplitPane(t *testing.T) {
	sm := newTestManager(t)
	sess := createTestSession(t, sm, "split-test")

	w := sess.ActiveWindow()
	if w == nil {
		t.Fatal("no active window")
	}

	if w.PaneCount() != 1 {
		t.Fatalf("expected 1 pane, got %d", w.PaneCount())
	}

	// "Split" = add another pane to the same window.
	p2, err := w.AddPane(sess.ID, "/bin/sh", 80, 24, nil)
	if err != nil {
		t.Fatalf("AddPane: %v", err)
	}
	if p2 == nil {
		t.Fatal("AddPane returned nil")
	}

	if w.PaneCount() != 2 {
		t.Fatalf("expected 2 panes after split, got %d", w.PaneCount())
	}
}

func TestWindowPaneManagement_ClosePane(t *testing.T) {
	sm := newTestManager(t)
	sess := createTestSession(t, sm, "close-pane-test")

	w := sess.ActiveWindow()
	if w == nil {
		t.Fatal("no active window")
	}

	// Add a second pane.
	p2, err := w.AddPane(sess.ID, "/bin/sh", 80, 24, nil)
	if err != nil {
		t.Fatalf("AddPane: %v", err)
	}

	if w.PaneCount() != 2 {
		t.Fatalf("expected 2 panes, got %d", w.PaneCount())
	}

	// Remove the second pane.
	if !w.RemovePane(p2.ID) {
		t.Fatal("RemovePane returned false")
	}

	if w.PaneCount() != 1 {
		t.Fatalf("expected 1 pane after removal, got %d", w.PaneCount())
	}

	// Verify the removed pane ID is gone.
	if _, ok := w.GetPane(p2.ID); ok {
		t.Fatal("removed pane still found in window")
	}
}

// -------------------------------------------------------------------------
// 5. Persistence
// -------------------------------------------------------------------------

func TestPersistence_SnapshotSaveLoadRoundtrip(t *testing.T) {
	sm := newTestManager(t)

	createTestSession(t, sm, "persist-A")
	createTestSession(t, sm, "persist-B")

	// Take snapshot.
	snapshot := sm.PersistSnapshot()
	if len(snapshot.Sessions) != 2 {
		t.Fatalf("expected 2 sessions in snapshot, got %d", len(snapshot.Sessions))
	}
	if snapshot.Version != persistenceVersion {
		t.Fatalf("expected version %d, got %d", persistenceVersion, snapshot.Version)
	}

	// Save to temp file.
	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "state.json")

	if err := SaveState(snapshot, statePath); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	// Verify file exists.
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("state file not found: %v", err)
	}

	// Load it back.
	loaded, err := LoadState(statePath)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if loaded == nil {
		t.Fatal("LoadState returned nil")
	}

	if len(loaded.Sessions) != 2 {
		t.Fatalf("expected 2 sessions after load, got %d", len(loaded.Sessions))
	}

	// Verify session names survived the roundtrip.
	names := make(map[string]bool)
	for _, s := range loaded.Sessions {
		names[s.Name] = true
	}
	if !names["persist-A"] || !names["persist-B"] {
		t.Fatalf("session names not preserved: %v", names)
	}
}

func TestPersistence_DeadSessionsFiltered(t *testing.T) {
	sm := newTestManager(t)

	sess := createTestSession(t, sm, "will-die")

	// Kill the session by closing it (marks it dead).
	sess.Close()

	snapshot := sm.PersistSnapshot()
	if len(snapshot.Sessions) != 0 {
		t.Fatalf("expected 0 sessions in snapshot (dead filtered), got %d", len(snapshot.Sessions))
	}
}

func TestPersistence_AtomicWrite(t *testing.T) {
	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "atomic-state.json")

	state := PersistentState{
		Version: persistenceVersion,
		PID:     os.Getpid(),
		SavedAt: time.Now().UTC().Format(time.RFC3339),
		Sessions: []PersistentSession{
			{
				ID:    "sess-1",
				Name:  "test",
				Shell: "/bin/sh",
			},
		},
		NextID: 2,
	}

	if err := SaveState(state, statePath); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	// Verify no temp files left behind.
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}

	// Verify final file is valid JSON.
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var check PersistentState
	if err := json.Unmarshal(data, &check); err != nil {
		t.Fatalf("saved file is not valid JSON: %v", err)
	}
	if check.Version != persistenceVersion {
		t.Fatalf("expected version %d, got %d", persistenceVersion, check.Version)
	}
}

func TestPersistence_LoadNonExistent(t *testing.T) {
	loaded, err := LoadState("/tmp/no-such-file-exists-12345.json")
	if err != nil {
		t.Fatalf("LoadState of nonexistent file should return nil error, got: %v", err)
	}
	if loaded != nil {
		t.Fatal("LoadState of nonexistent file should return nil state")
	}
}

func TestPersistence_RestoreSessions(t *testing.T) {
	// Create sessions and snapshot them.
	sm1 := newTestManager(t)
	createTestSession(t, sm1, "restore-A")
	createTestSession(t, sm1, "restore-B")
	snapshot := sm1.PersistSnapshot()

	// Save and reload.
	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "restore-state.json")
	if err := SaveState(snapshot, statePath); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	loaded, err := LoadState(statePath)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}

	// Restore into a fresh manager.
	sm2 := NewSessionManager()
	t.Cleanup(func() { sm2.CloseAll() })
	result := sm2.RestoreSessions(loaded)

	if result.SessionCount != 2 {
		t.Fatalf("expected 2 restored sessions, got %d", result.SessionCount)
	}
	if len(result.Errors) > 0 {
		t.Fatalf("restore errors: %v", result.Errors)
	}

	// Verify names are preserved.
	list := sm2.List()
	if len(list) != 2 {
		t.Fatalf("expected 2 sessions in restored manager, got %d", len(list))
	}
	names := make(map[string]bool)
	for _, s := range list {
		names[s["name"].(string)] = true
	}
	if !names["restore-A"] || !names["restore-B"] {
		t.Fatalf("restored session names not preserved: %v", names)
	}
}

// -------------------------------------------------------------------------
// 6. Resize
// -------------------------------------------------------------------------

func TestResize_UpdatesDimensions(t *testing.T) {
	sm := newTestManager(t)
	sess := createTestSession(t, sm, "resize-test")

	pane := sess.ActivePane()
	if pane == nil {
		t.Fatal("no active pane")
	}

	// Verify initial dimensions via snapshot.
	snap := pane.Snapshot()
	if snap["cols"] != 80 || snap["rows"] != 24 {
		t.Fatalf("expected initial 80x24, got %vx%v", snap["cols"], snap["rows"])
	}

	// Resize.
	if err := pane.Resize(120, 40); err != nil {
		t.Fatalf("Resize: %v", err)
	}

	// Verify new dimensions.
	snap = pane.Snapshot()
	if snap["cols"] != 120 || snap["rows"] != 40 {
		t.Fatalf("expected 120x40 after resize, got %vx%v", snap["cols"], snap["rows"])
	}
}

func TestResize_ViaSession(t *testing.T) {
	sm := newTestManager(t)
	sess := createTestSession(t, sm, "resize-sess")

	// Resize via session (delegates to active pane).
	if err := sess.Resize(200, 50); err != nil {
		t.Fatalf("session Resize: %v", err)
	}

	pane := sess.ActivePane()
	snap := pane.Snapshot()
	if snap["cols"] != 200 || snap["rows"] != 50 {
		t.Fatalf("expected 200x50 after session resize, got %vx%v", snap["cols"], snap["rows"])
	}
}

// -------------------------------------------------------------------------
// Additional edge cases
// -------------------------------------------------------------------------

func TestSession_FindPane(t *testing.T) {
	sm := newTestManager(t)
	sess := createTestSession(t, sm, "find-pane")

	pane := sess.ActivePane()
	if pane == nil {
		t.Fatal("no active pane")
	}

	// FindPane should locate the pane.
	found, w, ok := sess.FindPane(pane.ID)
	if !ok {
		t.Fatal("FindPane did not find active pane")
	}
	if found.ID != pane.ID {
		t.Fatalf("expected pane %s, got %s", pane.ID, found.ID)
	}
	if w == nil {
		t.Fatal("FindPane returned nil window")
	}

	// Non-existent pane.
	_, _, ok = sess.FindPane("no-such-pane")
	if ok {
		t.Fatal("FindPane should not find non-existent pane")
	}
}

func TestSession_RemoveWindow(t *testing.T) {
	sm := newTestManager(t)
	sess := createTestSession(t, sm, "remove-win")

	// Add a second window.
	w2, _, err := sess.AddWindow("extra", "/bin/sh", 80, 24, nil)
	if err != nil {
		t.Fatalf("AddWindow: %v", err)
	}

	if sess.WindowCount() != 2 {
		t.Fatalf("expected 2 windows, got %d", sess.WindowCount())
	}

	// Remove the second window.
	if !sess.RemoveWindow(w2.ID) {
		t.Fatal("RemoveWindow returned false")
	}

	if sess.WindowCount() != 1 {
		t.Fatalf("expected 1 window after removal, got %d", sess.WindowCount())
	}
}

func TestSession_FindByNameOrID(t *testing.T) {
	sm := newTestManager(t)
	sess := createTestSession(t, sm, "lookup-test")

	// Find by name.
	found, ok := sm.FindByNameOrID("lookup-test")
	if !ok || found.ID != sess.ID {
		t.Fatal("FindByNameOrID by name failed")
	}

	// Find by ID.
	found, ok = sm.FindByNameOrID(sess.ID)
	if !ok || found.ID != sess.ID {
		t.Fatal("FindByNameOrID by ID failed")
	}

	// Not found.
	_, ok = sm.FindByNameOrID("nonexistent")
	if ok {
		t.Fatal("FindByNameOrID should fail for nonexistent")
	}
}

func TestWindow_RemoveLastPane_MarksWindowDead(t *testing.T) {
	sm := newTestManager(t)
	sess := createTestSession(t, sm, "last-pane")

	w := sess.ActiveWindow()
	if w == nil {
		t.Fatal("no active window")
	}

	pane := w.ActivePane()
	if pane == nil {
		t.Fatal("no active pane")
	}

	// Remove the only pane.
	w.RemovePane(pane.ID)

	if w.PaneCount() != 0 {
		t.Fatalf("expected 0 panes, got %d", w.PaneCount())
	}
	if w.Status != SessionDead {
		t.Fatalf("expected window status dead, got %s", w.Status)
	}
}

func TestRingBuffer_EmptyBytes(t *testing.T) {
	rb := NewRingBuffer(1024)
	data := rb.Bytes()
	if len(data) != 0 {
		t.Fatalf("expected empty ring buffer, got %d bytes", len(data))
	}
}

func TestRingBuffer_Len(t *testing.T) {
	rb := NewRingBuffer(1024)
	if rb.Len() != 0 {
		t.Fatalf("expected 0, got %d", rb.Len())
	}

	rb.Write([]byte("hello"))
	if rb.Len() != 5 {
		t.Fatalf("expected 5, got %d", rb.Len())
	}

	// Write more than capacity.
	rb.Write(bytes.Repeat([]byte("x"), 2000))
	if rb.Len() != 1024 {
		t.Fatalf("expected 1024 (capped), got %d", rb.Len())
	}
}
