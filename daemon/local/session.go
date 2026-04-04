package local

import (
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

// Status types for sessions, windows, and panes.
type SessionStatus string

const (
	SessionRunning SessionStatus = "running"
	SessionDead    SessionStatus = "dead"
)

const ringBufferSize = 2 * 1024 * 1024 // 2MB

// ---------------------------------------------------------------------------
// Pane — each pane owns a PTY, ring buffer, and output fan-out.
// ---------------------------------------------------------------------------

// Pane represents a single PTY within a window.
type Pane struct {
	ID        string
	Shell     string
	CreatedAt time.Time
	Status    SessionStatus
	Pid       int
	ExitCode  int

	cols int
	rows int

	cmd  *exec.Cmd
	ptmx *os.File
	ring *RingBuffer

	mu      sync.Mutex
	clients map[string]*FrameWriter // attachment_id -> writer

	// IDs of parent session and window, used in events.
	sessionID string
	windowID  string

	done chan struct{}
}

// newPane spawns a new PTY pane.
func newPane(id, sessionID, windowID, shell string, cols, rows int, env map[string]string) (*Pane, error) {
	if shell == "" {
		shell = os.Getenv("SHELL")
		if shell == "" {
			shell = "/bin/sh"
		}
	}
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}

	cmd := exec.Command(shell)
	cmd.Env = buildEnv(env)

	winSize := &pty.Winsize{
		Cols: uint16(cols),
		Rows: uint16(rows),
	}
	ptmx, err := pty.StartWithSize(cmd, winSize)
	if err != nil {
		return nil, fmt.Errorf("failed to start pty: %w", err)
	}

	p := &Pane{
		ID:        id,
		Shell:     shell,
		CreatedAt: time.Now().UTC(),
		Status:    SessionRunning,
		Pid:       cmd.Process.Pid,
		cols:      cols,
		rows:      rows,
		cmd:       cmd,
		ptmx:      ptmx,
		ring:      NewRingBuffer(ringBufferSize),
		clients:   make(map[string]*FrameWriter),
		sessionID: sessionID,
		windowID:  windowID,
		done:      make(chan struct{}),
	}

	go p.readLoop()
	go p.waitLoop()

	return p, nil
}

// readLoop reads PTY output, stores it in the ring buffer, and fans out to attached clients.
func (p *Pane) readLoop() {
	buf := make([]byte, 32768)
	for {
		n, err := p.ptmx.Read(buf)
		if n > 0 {
			data := make([]byte, n)
			copy(data, buf[:n])

			p.ring.Write(data)

			// Collect clients under lock, then write outside lock to avoid
			// blocking all pane operations if a client connection is slow.
			p.mu.Lock()
			clients := make([]*FrameWriter, 0, len(p.clients))
			for _, fw := range p.clients {
				clients = append(clients, fw)
			}
			p.mu.Unlock()

			encoded := base64.StdEncoding.EncodeToString(data)
			for _, fw := range clients {
				_ = fw.WriteEvent(RPCEvent{
					Event:      "pty.output",
					SessionID:  p.sessionID,
					PaneID:     p.ID,
					DataBase64: encoded,
				})
			}
		}
		if err != nil {
			return
		}
	}
}

// waitLoop waits for the child process to exit and updates pane status.
func (p *Pane) waitLoop() {
	err := p.cmd.Wait()
	p.mu.Lock()
	p.Status = SessionDead
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			p.ExitCode = exitErr.ExitCode()
		} else {
			p.ExitCode = -1
		}
	}
	for _, fw := range p.clients {
		_ = fw.WriteEvent(RPCEvent{
			Event:     "pane.exited",
			SessionID: p.sessionID,
			PaneID:    p.ID,
		})
		// Also emit session.exited for backward compatibility with older clients.
		_ = fw.WriteEvent(RPCEvent{
			Event:     "session.exited",
			SessionID: p.sessionID,
		})
	}
	p.mu.Unlock()
	close(p.done)
}

// WriteInput sends raw bytes to the PTY.
func (p *Pane) WriteInput(data []byte) error {
	_, err := p.ptmx.Write(data)
	return err
}

// Resize changes the PTY window size.
func (p *Pane) Resize(cols, rows int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cols = cols
	p.rows = rows
	return pty.Setsize(p.ptmx, &pty.Winsize{
		Cols: uint16(cols),
		Rows: uint16(rows),
	})
}

// Attach registers a client writer. Returns current ring buffer contents for replay.
// The snapshot and registration are done under the same lock so no PTY output
// is lost between the snapshot and the client becoming visible to readLoop.
func (p *Pane) Attach(attachmentID string, fw *FrameWriter) []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	replay := p.ring.Bytes() // ring.Bytes() acquires ring.mu internally (leaf lock)
	p.clients[attachmentID] = fw
	return replay
}

// Detach removes a client from the pane.
func (p *Pane) Detach(attachmentID string) {
	p.mu.Lock()
	delete(p.clients, attachmentID)
	p.mu.Unlock()
}

// AttachedCount returns how many clients are currently attached.
func (p *Pane) AttachedCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.clients)
}

// Close kills the child process and closes the PTY.
func (p *Pane) Close() {
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Signal(syscall.SIGHUP)
		go func() {
			timer := time.NewTimer(3 * time.Second)
			defer timer.Stop()
			select {
			case <-p.done:
			case <-timer.C:
				_ = p.cmd.Process.Kill()
			}
		}()
	}
	_ = p.ptmx.Close()
}

// Snapshot returns a JSON-serializable map of pane state.
func (p *Pane) Snapshot() map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()

	return map[string]any{
		"pane_id":    p.ID,
		"shell":      p.Shell,
		"pid":        p.Pid,
		"status":     string(p.Status),
		"exit_code":  p.ExitCode,
		"created_at": p.CreatedAt.Format(time.RFC3339),
		"cols":       p.cols,
		"rows":       p.rows,
		"attached":   len(p.clients),
	}
}

// ---------------------------------------------------------------------------
// Window — logical grouping of panes.
// ---------------------------------------------------------------------------

// Window represents a logical grouping of panes within a session.
type Window struct {
	ID           string
	Name         string
	CreatedAt    time.Time
	Status       SessionStatus
	ActivePaneID string

	mu     sync.Mutex
	panes  map[string]*Pane
	nextID uint64
}

// newWindow creates an empty window.
func newWindow(id, name string) *Window {
	if name == "" {
		name = id
	}
	return &Window{
		ID:        id,
		Name:      name,
		CreatedAt: time.Now().UTC(),
		Status:    SessionRunning,
		panes:     make(map[string]*Pane),
		nextID:    1,
	}
}

// AddPane creates a new pane in this window.
func (w *Window) AddPane(sessionID, shell string, cols, rows int, env map[string]string) (*Pane, error) {
	w.mu.Lock()
	paneID := fmt.Sprintf("%s.pane-%d", w.ID, w.nextID)
	w.nextID++
	w.mu.Unlock()

	p, err := newPane(paneID, sessionID, w.ID, shell, cols, rows, env)
	if err != nil {
		return nil, err
	}

	w.mu.Lock()
	w.panes[p.ID] = p
	if w.ActivePaneID == "" {
		w.ActivePaneID = p.ID
	}
	w.mu.Unlock()

	return p, nil
}

// GetPane returns a pane by ID.
func (w *Window) GetPane(paneID string) (*Pane, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	p, ok := w.panes[paneID]
	return p, ok
}

// ActivePane returns the active pane, or nil.
func (w *Window) ActivePane() *Pane {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.panes[w.ActivePaneID]
}

// RemovePane removes a pane from the window and closes it.
func (w *Window) RemovePane(paneID string) bool {
	w.mu.Lock()
	p, ok := w.panes[paneID]
	if ok {
		delete(w.panes, paneID)
		// If the active pane was removed, pick another.
		if w.ActivePaneID == paneID {
			w.ActivePaneID = ""
			for id, pp := range w.panes {
				if pp.Status == SessionRunning {
					w.ActivePaneID = id
					break
				}
			}
			// If no running pane, just pick any.
			if w.ActivePaneID == "" {
				for id := range w.panes {
					w.ActivePaneID = id
					break
				}
			}
		}
		// If no panes left, mark window dead.
		if len(w.panes) == 0 {
			w.Status = SessionDead
		}
	}
	w.mu.Unlock()
	if ok {
		p.Close()
	}
	return ok
}

// Close closes all panes in the window.
func (w *Window) Close() {
	w.mu.Lock()
	panes := make([]*Pane, 0, len(w.panes))
	for _, p := range w.panes {
		panes = append(panes, p)
	}
	w.panes = make(map[string]*Pane)
	w.Status = SessionDead
	w.mu.Unlock()

	for _, p := range panes {
		p.Close()
	}
}

// UpdateStatus checks all panes and marks window dead if all panes are dead.
func (w *Window) UpdateStatus() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.panes) == 0 {
		w.Status = SessionDead
		return
	}
	allDead := true
	for _, p := range w.panes {
		p.mu.Lock()
		running := p.Status == SessionRunning
		p.mu.Unlock()
		if running {
			allDead = false
			break
		}
	}
	if allDead {
		w.Status = SessionDead
	}
}

// PaneCount returns the number of panes.
func (w *Window) PaneCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.panes)
}

// Snapshot returns a JSON-serializable map of window state.
func (w *Window) Snapshot() map[string]any {
	w.mu.Lock()
	defer w.mu.Unlock()

	panes := make([]map[string]any, 0, len(w.panes))
	for _, p := range w.panes {
		panes = append(panes, p.Snapshot())
	}

	return map[string]any{
		"window_id":      w.ID,
		"name":           w.Name,
		"status":         string(w.Status),
		"created_at":     w.CreatedAt.Format(time.RFC3339),
		"active_pane_id": w.ActivePaneID,
		"pane_count":     len(w.panes),
		"panes":          panes,
	}
}

// ---------------------------------------------------------------------------
// Session — contains windows.
// ---------------------------------------------------------------------------

// Session represents a terminal session containing one or more windows.
type Session struct {
	ID             string
	Name           string
	Shell          string
	CreatedAt      time.Time
	Status         SessionStatus
	ActiveWindowID string

	mu      sync.Mutex
	windows map[string]*Window
	nextID  uint64
}

// AddWindow creates a new window in this session with one initial pane.
func (s *Session) AddWindow(name, shell string, cols, rows int, env map[string]string) (*Window, *Pane, error) {
	s.mu.Lock()
	winID := fmt.Sprintf("%s.win-%d", s.ID, s.nextID)
	s.nextID++
	s.mu.Unlock()

	w := newWindow(winID, name)

	p, err := w.AddPane(s.ID, shell, cols, rows, env)
	if err != nil {
		return nil, nil, err
	}

	s.mu.Lock()
	s.windows[w.ID] = w
	if s.ActiveWindowID == "" {
		s.ActiveWindowID = w.ID
	}
	s.mu.Unlock()

	return w, p, nil
}

// GetWindow returns a window by ID.
func (s *Session) GetWindow(windowID string) (*Window, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.windows[windowID]
	return w, ok
}

// ActiveWindow returns the active window, or nil.
func (s *Session) ActiveWindow() *Window {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.windows[s.ActiveWindowID]
}

// GetActiveWindowID returns the active window ID. Thread-safe.
func (s *Session) GetActiveWindowID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ActiveWindowID
}

// ActivePane returns the active pane of the active window, or nil.
func (s *Session) ActivePane() *Pane {
	w := s.ActiveWindow()
	if w == nil {
		return nil
	}
	return w.ActivePane()
}

// FindPane searches all windows for a pane by ID.
func (s *Session) FindPane(paneID string) (*Pane, *Window, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, w := range s.windows {
		if p, ok := w.GetPane(paneID); ok {
			return p, w, true
		}
	}
	return nil, nil, false
}

// SetActivePaneByID sets the active window and pane to match the given pane ID.
func (s *Session) SetActivePaneByID(paneID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, w := range s.windows {
		if _, ok := w.GetPane(paneID); ok {
			s.ActiveWindowID = w.ID
			w.mu.Lock()
			w.ActivePaneID = paneID
			w.mu.Unlock()
			return true
		}
	}
	return false
}

// RemoveWindow removes a window and closes all its panes.
func (s *Session) RemoveWindow(windowID string) bool {
	s.mu.Lock()
	w, ok := s.windows[windowID]
	if ok {
		delete(s.windows, windowID)
		if s.ActiveWindowID == windowID {
			s.ActiveWindowID = ""
			for id, ww := range s.windows {
				if ww.Status == SessionRunning {
					s.ActiveWindowID = id
					break
				}
			}
			if s.ActiveWindowID == "" {
				for id := range s.windows {
					s.ActiveWindowID = id
					break
				}
			}
		}
		if len(s.windows) == 0 {
			s.Status = SessionDead
		}
	}
	s.mu.Unlock()
	if ok {
		w.Close()
	}
	return ok
}

// ListWindows returns snapshots of all windows.
func (s *Session) ListWindows() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]map[string]any, 0, len(s.windows))
	for _, w := range s.windows {
		result = append(result, w.Snapshot())
	}
	return result
}

// WindowCount returns the number of windows.
func (s *Session) WindowCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.windows)
}

// TotalPaneCount returns the total number of panes across all windows.
func (s *Session) TotalPaneCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	total := 0
	for _, w := range s.windows {
		total += w.PaneCount()
	}
	return total
}

// Backward-compatible methods that delegate to the active pane.

// WriteInput sends input to the active pane.
func (s *Session) WriteInput(data []byte) error {
	p := s.ActivePane()
	if p == nil {
		return fmt.Errorf("no active pane")
	}
	return p.WriteInput(data)
}

// Resize changes the active pane's PTY window size.
func (s *Session) Resize(cols, rows int) error {
	p := s.ActivePane()
	if p == nil {
		return fmt.Errorf("no active pane")
	}
	return p.Resize(cols, rows)
}

// Attach attaches to the active pane. Returns replay data.
func (s *Session) Attach(attachmentID string, fw *FrameWriter) []byte {
	p := s.ActivePane()
	if p == nil {
		return nil
	}
	return p.Attach(attachmentID, fw)
}

// Detach detaches from all panes in this session for the given attachment.
func (s *Session) Detach(attachmentID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, w := range s.windows {
		w.mu.Lock()
		for _, p := range w.panes {
			p.Detach(attachmentID)
		}
		w.mu.Unlock()
	}
}

// AttachedCount returns the number of clients attached to the active pane.
func (s *Session) AttachedCount() int {
	p := s.ActivePane()
	if p == nil {
		return 0
	}
	return p.AttachedCount()
}

// Close kills all panes in all windows.
func (s *Session) Close() {
	s.mu.Lock()
	windows := make([]*Window, 0, len(s.windows))
	for _, w := range s.windows {
		windows = append(windows, w)
	}
	s.windows = make(map[string]*Window)
	s.Status = SessionDead
	s.mu.Unlock()

	for _, w := range windows {
		w.Close()
	}
}

// Snapshot returns a JSON-serializable map of session state.
func (s *Session) Snapshot() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Compute total attached across all panes.
	totalAttached := 0
	attachmentSet := make(map[string]struct{})
	for _, w := range s.windows {
		w.mu.Lock()
		for _, p := range w.panes {
			p.mu.Lock()
			for id := range p.clients {
				attachmentSet[id] = struct{}{}
			}
			p.mu.Unlock()
		}
		w.mu.Unlock()
	}
	totalAttached = len(attachmentSet)

	attachments := make([]string, 0, len(attachmentSet))
	for id := range attachmentSet {
		attachments = append(attachments, id)
	}

	// Get first pane's PID for backward compat.
	pid := 0
	var exitCode int
	if w, ok := s.windows[s.ActiveWindowID]; ok {
		if p := w.ActivePane(); p != nil {
			pid = p.Pid
			exitCode = p.ExitCode
		}
	}

	// Compute counts.
	windowCount := len(s.windows)
	paneCount := 0
	for _, w := range s.windows {
		paneCount += w.PaneCount()
	}

	// Get active pane ID.
	activePaneID := ""
	if w, ok := s.windows[s.ActiveWindowID]; ok {
		activePaneID = w.ActivePaneID
	}

	return map[string]any{
		"session_id":       s.ID,
		"name":             s.Name,
		"shell":            s.Shell,
		"pid":              pid,
		"status":           string(s.Status),
		"exit_code":        exitCode,
		"created_at":       s.CreatedAt.Format(time.RFC3339),
		"attached":         totalAttached,
		"attachments":      attachments,
		"window_count":     windowCount,
		"pane_count":       paneCount,
		"active_window_id": s.ActiveWindowID,
		"active_pane_id":   activePaneID,
	}
}

// ---------------------------------------------------------------------------
// SessionManager
// ---------------------------------------------------------------------------

// SessionManager owns all sessions and provides thread-safe access.
type SessionManager struct {
	mu       sync.Mutex
	sessions map[string]*Session
	nextID   uint64
}

// NewSessionManager creates a new manager.
func NewSessionManager() *SessionManager {
	return &SessionManager{
		sessions: make(map[string]*Session),
		nextID:   1,
	}
}

// NewSessionParams are the parameters for creating a new session.
type NewSessionParams struct {
	Name  string
	Shell string
	Cols  int
	Rows  int
	Env   map[string]string
}

// Create spawns a new session with one window containing one pane.
func (sm *SessionManager) Create(params NewSessionParams) (*Session, error) {
	shell := params.Shell
	if shell == "" {
		shell = os.Getenv("SHELL")
		if shell == "" {
			shell = "/bin/sh"
		}
	}

	sm.mu.Lock()
	id := fmt.Sprintf("sess-%d", sm.nextID)
	sm.nextID++
	sm.mu.Unlock()

	name := params.Name
	if name == "" {
		name = id
	}

	sess := &Session{
		ID:        id,
		Name:      name,
		Shell:     shell,
		CreatedAt: time.Now().UTC(),
		Status:    SessionRunning,
		windows:   make(map[string]*Window),
		nextID:    1,
	}

	_, _, err := sess.AddWindow("", shell, params.Cols, params.Rows, params.Env)
	if err != nil {
		return nil, err
	}

	sm.mu.Lock()
	sm.sessions[id] = sess
	sm.mu.Unlock()

	return sess, nil
}

func buildEnv(extra map[string]string) []string {
	env := os.Environ()
	hasterm := false
	for _, e := range env {
		if len(e) > 5 && e[:5] == "TERM=" {
			hasterm = true
			break
		}
	}
	if !hasterm {
		env = append(env, "TERM=xterm-256color")
	}
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return env
}

// Get returns a session by ID. Thread-safe.
func (sm *SessionManager) Get(id string) (*Session, bool) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	s, ok := sm.sessions[id]
	return s, ok
}

// GetByName returns the first session matching the given name. Thread-safe.
func (sm *SessionManager) GetByName(name string) (*Session, bool) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	for _, s := range sm.sessions {
		if s.Name == name {
			return s, true
		}
	}
	return nil, false
}

// FindByNameOrID finds a session by name first, then by ID.
func (sm *SessionManager) FindByNameOrID(target string) (*Session, bool) {
	if s, ok := sm.GetByName(target); ok {
		return s, true
	}
	return sm.Get(target)
}

// List returns snapshots of all sessions.
func (sm *SessionManager) List() []map[string]any {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	result := make([]map[string]any, 0, len(sm.sessions))
	for _, s := range sm.sessions {
		result = append(result, s.Snapshot())
	}
	return result
}

// Remove removes a session from the manager and closes it.
func (sm *SessionManager) Remove(id string) bool {
	sm.mu.Lock()
	s, ok := sm.sessions[id]
	if ok {
		delete(sm.sessions, id)
	}
	sm.mu.Unlock()
	if ok {
		s.Close()
	}
	return ok
}

// CloseAll closes all sessions. Called on daemon shutdown.
func (sm *SessionManager) CloseAll() {
	sm.mu.Lock()
	sessions := make([]*Session, 0, len(sm.sessions))
	for id, s := range sm.sessions {
		sessions = append(sessions, s)
		delete(sm.sessions, id)
	}
	sm.mu.Unlock()
	for _, s := range sessions {
		s.Close()
	}
}
