package local

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ---------------------------------------------------------------------------
// Persistent state types — serializable snapshots of session structure.
// ---------------------------------------------------------------------------

// PersistentPane captures the metadata needed to recreate a pane.
type PersistentPane struct {
	ID        string `json:"id"`
	Shell     string `json:"shell"`
	Cols      int    `json:"cols"`
	Rows      int    `json:"rows"`
	CreatedAt string `json:"created_at"`
}

// PersistentWindow captures window metadata and its panes.
type PersistentWindow struct {
	ID           string           `json:"id"`
	Name         string           `json:"name"`
	ActivePaneID string           `json:"active_pane_id"`
	CreatedAt    string           `json:"created_at"`
	Panes        []PersistentPane `json:"panes"`
	NextID       uint64           `json:"next_id"`
}

// PersistentSession captures session metadata and its windows.
type PersistentSession struct {
	ID             string             `json:"id"`
	Name           string             `json:"name"`
	Shell          string             `json:"shell"`
	ActiveWindowID string             `json:"active_window_id"`
	CreatedAt      string             `json:"created_at"`
	Windows        []PersistentWindow `json:"windows"`
	NextID         uint64             `json:"next_id"`
}

// PersistentState is the top-level structure written to disk.
type PersistentState struct {
	Version   int                 `json:"version"`
	PID       int                 `json:"pid"`
	SavedAt   string              `json:"saved_at"`
	Sessions  []PersistentSession `json:"sessions"`
	NextID    uint64              `json:"next_id"`
}

const persistenceVersion = 1

// ---------------------------------------------------------------------------
// Snapshot helpers — extract serializable state from live objects.
// ---------------------------------------------------------------------------

// PersistSnapshot builds a PersistentState from the current SessionManager.
func (sm *SessionManager) PersistSnapshot() PersistentState {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	state := PersistentState{
		Version: persistenceVersion,
		PID:     os.Getpid(),
		SavedAt: time.Now().UTC().Format(time.RFC3339),
		NextID:  sm.nextID,
	}

	for _, sess := range sm.sessions {
		ps := persistSession(sess)
		if ps != nil {
			state.Sessions = append(state.Sessions, *ps)
		}
	}

	return state
}

func persistSession(s *Session) *PersistentSession {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.Status == SessionDead {
		return nil
	}

	ps := &PersistentSession{
		ID:             s.ID,
		Name:           s.Name,
		Shell:          s.Shell,
		ActiveWindowID: s.ActiveWindowID,
		CreatedAt:      s.CreatedAt.Format(time.RFC3339),
		NextID:         s.nextID,
	}

	for _, w := range s.windows {
		pw := persistWindow(w)
		if pw != nil {
			ps.Windows = append(ps.Windows, *pw)
		}
	}

	// Don't persist sessions with no live windows.
	if len(ps.Windows) == 0 {
		return nil
	}

	return ps
}

func persistWindow(w *Window) *PersistentWindow {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.Status == SessionDead {
		return nil
	}

	pw := &PersistentWindow{
		ID:           w.ID,
		Name:         w.Name,
		ActivePaneID: w.ActivePaneID,
		CreatedAt:    w.CreatedAt.Format(time.RFC3339),
		NextID:       w.nextID,
	}

	for _, p := range w.panes {
		p.mu.Lock()
		if p.Status == SessionDead {
			p.mu.Unlock()
			continue
		}
		pp := PersistentPane{
			ID:        p.ID,
			Shell:     p.Shell,
			Cols:      p.cols,
			Rows:      p.rows,
			CreatedAt: p.CreatedAt.Format(time.RFC3339),
		}
		p.mu.Unlock()
		pw.Panes = append(pw.Panes, pp)
	}

	// Don't persist windows with no live panes.
	if len(pw.Panes) == 0 {
		return nil
	}

	return pw
}

// ---------------------------------------------------------------------------
// Save / Load — atomic file I/O.
// ---------------------------------------------------------------------------

// SaveState writes the persistent state to filePath atomically.
// It writes to a temporary file in the same directory, then renames.
func SaveState(state PersistentState, filePath string) error {
	dir := filepath.Dir(filePath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".cmuxd-state-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("close temp file: %w", err)
	}

	if err := os.Rename(tmpPath, filePath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename temp to state file: %w", err)
	}

	return nil
}

// LoadState reads and deserializes the persistent state from filePath.
// Returns nil state and nil error if the file does not exist.
func LoadState(filePath string) (*PersistentState, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read state file: %w", err)
	}

	var state PersistentState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("unmarshal state: %w", err)
	}

	if state.Version != persistenceVersion {
		return nil, fmt.Errorf("unsupported state version %d (expected %d)", state.Version, persistenceVersion)
	}

	return &state, nil
}

// RemoveState deletes the state file. It is not an error if the file does not exist.
func RemoveState(filePath string) error {
	err := os.Remove(filePath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// DefaultStateDir returns the private state directory for the daemon.
// Uses ~/.local/state/cmux/ (XDG-style), created with 0700 permissions.
func DefaultStateDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "/tmp"
	}
	return filepath.Join(home, ".local", "state", "cmux")
}

// DefaultStatePath returns the default path for the persistent state file.
// Uses ~/.local/state/cmux/daemon-local.json (XDG-style).
func DefaultStatePath() string {
	return filepath.Join(DefaultStateDir(), "daemon-local.json")
}

// DefaultSocketPath returns the path for the daemon's Unix socket.
// Uses ~/.local/state/cmux/daemon-local.sock — a private directory (0700)
// that is not vulnerable to symlink attacks, unlike /tmp.
func DefaultSocketPath() string {
	return filepath.Join(DefaultStateDir(), "daemon-local.sock")
}

// LegacySocketPath returns the old /tmp-based socket path for backward
// compatibility. The daemon creates a symlink here pointing to the new
// socket so that older clients can still connect during the transition.
func LegacySocketPath() string {
	return fmt.Sprintf("/tmp/cmux-local-%d.sock", os.Getuid())
}

// EnsureStateDir creates the state directory with 0700 permissions if it
// does not already exist. Returns the directory path.
func EnsureStateDir() (string, error) {
	dir := DefaultStateDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("create state directory: %w", err)
	}
	// Ensure permissions are correct even if directory already existed.
	if err := os.Chmod(dir, 0700); err != nil {
		return "", fmt.Errorf("set state directory permissions: %w", err)
	}
	return dir, nil
}

// ---------------------------------------------------------------------------
// Restore — recreate sessions from persisted state.
// ---------------------------------------------------------------------------

// RestoreResult summarizes what was restored.
type RestoreResult struct {
	SessionCount int
	WindowCount  int
	PaneCount    int
	Errors       []string
}

// RestoreSessions recreates session/window/pane structure from persisted state.
// Each pane gets a fresh PTY (the original processes are gone). Restored sessions
// keep their original IDs and names so the GUI can match them.
func (sm *SessionManager) RestoreSessions(state *PersistentState) RestoreResult {
	var result RestoreResult
	if state == nil || len(state.Sessions) == 0 {
		return result
	}

	sm.mu.Lock()
	// Advance the ID counter past any IDs in persisted state.
	if state.NextID > sm.nextID {
		sm.nextID = state.NextID
	}
	sm.mu.Unlock()

	for _, ps := range state.Sessions {
		sess, err := sm.restoreSession(ps)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("session %s (%s): %s", ps.ID, ps.Name, err))
			continue
		}
		if sess == nil {
			continue
		}
		result.SessionCount++
		result.WindowCount += sess.WindowCount()
		result.PaneCount += sess.TotalPaneCount()
	}

	return result
}

func (sm *SessionManager) restoreSession(ps PersistentSession) (*Session, error) {
	if len(ps.Windows) == 0 {
		return nil, nil
	}

	createdAt, err := time.Parse(time.RFC3339, ps.CreatedAt)
	if err != nil {
		createdAt = time.Now().UTC()
	}

	sess := &Session{
		ID:        ps.ID,
		Name:      ps.Name,
		Shell:     ps.Shell,
		CreatedAt: createdAt,
		Status:    SessionRunning,
		windows:   make(map[string]*Window),
		nextID:    ps.NextID,
	}

	for _, pw := range ps.Windows {
		w, err := restoreWindow(pw, sess.ID)
		if err != nil {
			// Log but continue — partial restore is better than none.
			continue
		}
		if w == nil {
			continue
		}
		sess.windows[w.ID] = w
	}

	if len(sess.windows) == 0 {
		return nil, fmt.Errorf("no windows could be restored")
	}

	// Restore active window. If the persisted active window wasn't restored, pick one.
	if _, ok := sess.windows[ps.ActiveWindowID]; ok {
		sess.ActiveWindowID = ps.ActiveWindowID
	} else {
		for id := range sess.windows {
			sess.ActiveWindowID = id
			break
		}
	}

	sm.mu.Lock()
	sm.sessions[sess.ID] = sess
	// Ensure nextID is past this session's numeric suffix.
	if n := extractIDNumber(sess.ID, "sess-"); n >= sm.nextID {
		sm.nextID = n + 1
	}
	sm.mu.Unlock()

	return sess, nil
}

func restoreWindow(pw PersistentWindow, sessionID string) (*Window, error) {
	if len(pw.Panes) == 0 {
		return nil, nil
	}

	createdAt, err := time.Parse(time.RFC3339, pw.CreatedAt)
	if err != nil {
		createdAt = time.Now().UTC()
	}

	w := &Window{
		ID:        pw.ID,
		Name:      pw.Name,
		CreatedAt: createdAt,
		Status:    SessionRunning,
		panes:     make(map[string]*Pane),
		nextID:    pw.NextID,
	}

	for _, pp := range pw.Panes {
		p, err := restorePane(pp, sessionID, w.ID)
		if err != nil {
			continue
		}
		w.panes[p.ID] = p
	}

	if len(w.panes) == 0 {
		return nil, fmt.Errorf("no panes could be restored")
	}

	// Restore active pane.
	if _, ok := w.panes[pw.ActivePaneID]; ok {
		w.ActivePaneID = pw.ActivePaneID
	} else {
		for id := range w.panes {
			w.ActivePaneID = id
			break
		}
	}

	return w, nil
}

func restorePane(pp PersistentPane, sessionID, windowID string) (*Pane, error) {
	// Create a fresh PTY with the same shell and dimensions.
	p, err := newPane(pp.ID, sessionID, windowID, pp.Shell, pp.Cols, pp.Rows, nil)
	if err != nil {
		return nil, fmt.Errorf("restore pane %s: %w", pp.ID, err)
	}

	// Preserve the original creation time.
	if t, err := time.Parse(time.RFC3339, pp.CreatedAt); err == nil {
		p.CreatedAt = t
	}

	return p, nil
}

// extractIDNumber parses the numeric suffix from an ID like "sess-3" or "sess-3.win-2".
func extractIDNumber(id, prefix string) uint64 {
	// Handle compound IDs like "sess-3.win-2" — we want the "sess-3" part.
	if idx := strings.Index(id, "."); idx >= 0 {
		id = id[:idx]
	}
	after := strings.TrimPrefix(id, prefix)
	if after == id {
		return 0
	}
	n, err := strconv.ParseUint(after, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// ---------------------------------------------------------------------------
// StatePersister — periodic and event-driven state saving.
// ---------------------------------------------------------------------------

// StatePersister manages automatic state persistence for a SessionManager.
type StatePersister struct {
	sm       *SessionManager
	filePath string
	mu       sync.Mutex
	stopCh   chan struct{}
	stopped  bool
}

// NewStatePersister creates a persister that will save state for the given
// SessionManager to filePath.
func NewStatePersister(sm *SessionManager, filePath string) *StatePersister {
	return &StatePersister{
		sm:       sm,
		filePath: filePath,
		stopCh:   make(chan struct{}),
	}
}

// Start begins periodic auto-saving every 30 seconds.
func (sp *StatePersister) Start() {
	go sp.periodicSave()
}

// Stop halts the periodic saver and performs a final save.
func (sp *StatePersister) Stop() {
	sp.mu.Lock()
	if sp.stopped {
		sp.mu.Unlock()
		return
	}
	sp.stopped = true
	close(sp.stopCh)
	sp.mu.Unlock()

	// Final save.
	sp.Save()
}

// Save writes the current state to disk. Safe to call concurrently.
func (sp *StatePersister) Save() {
	state := sp.sm.PersistSnapshot()

	// If there are no sessions, remove the state file instead of writing empty state.
	if len(state.Sessions) == 0 {
		_ = RemoveState(sp.filePath)
		return
	}

	if err := SaveState(state, sp.filePath); err != nil {
		// Log but don't fail — persistence is best-effort.
		fmt.Fprintf(os.Stderr, "cmuxd-local: save state: %v\n", err)
	}
}

// NotifyChange should be called after any structural change (session/window/pane
// create or close). It triggers an asynchronous save.
func (sp *StatePersister) NotifyChange() {
	go sp.Save()
}

func (sp *StatePersister) periodicSave() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			sp.Save()
		case <-sp.stopCh:
			return
		}
	}
}

// ---------------------------------------------------------------------------
// PID lock — prevent two daemons from using the same state file.
// ---------------------------------------------------------------------------

// CheckStalePID checks whether the PID in the state file refers to a running
// process. Returns true if the previous daemon is still alive.
func CheckStalePID(state *PersistentState) bool {
	if state == nil || state.PID == 0 {
		return false
	}
	proc, err := os.FindProcess(state.PID)
	if err != nil {
		return false
	}
	// On Unix, FindProcess always succeeds. Send signal 0 to check liveness.
	err = proc.Signal(syscall.Signal(0))
	return err == nil
}
