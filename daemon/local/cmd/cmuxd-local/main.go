package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	local "github.com/manaflow-ai/cmux/daemon/local"
)

var version = "dev"

func main() {
	os.Exit(run())
}

func run() int {
	// Ensure the private state directory exists with 0700 permissions.
	if _, err := local.EnsureStateDir(); err != nil {
		fmt.Fprintf(os.Stderr, "cmuxd-local: %v\n", err)
		return 1
	}

	socketPath := defaultSocketPath()
	legacyPath := local.LegacySocketPath()

	// Clean up stale socket
	if _, err := os.Stat(socketPath); err == nil {
		// Try connecting — if it works, another daemon is running
		conn, err := net.Dial("unix", socketPath)
		if err == nil {
			conn.Close()
			fmt.Fprintf(os.Stderr, "cmuxd-local: another daemon is already running at %s\n", socketPath)
			return 1
		}
		// Stale socket, remove it
		_ = os.Remove(socketPath)
	}

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cmuxd-local: failed to listen on %s: %v\n", socketPath, err)
		return 1
	}
	defer listener.Close()
	defer os.Remove(socketPath)

	// Make socket accessible only to current user (owner read+write only)
	_ = os.Chmod(socketPath, 0600)

	// Create a backward-compatible symlink at the old /tmp path so older
	// clients can still connect during the transition period.
	_ = os.Remove(legacyPath) // remove stale socket or symlink
	if err := os.Symlink(socketPath, legacyPath); err != nil {
		log.Printf("warning: could not create legacy symlink at %s: %v", legacyPath, err)
	} else {
		defer os.Remove(legacyPath)
	}

	sm := local.NewSessionManager()
	defer sm.CloseAll()

	// --- State persistence ---
	statePath := local.DefaultStatePath()
	persister := local.NewStatePersister(sm, statePath)

	// Attempt to restore sessions from a previous daemon run.
	if savedState, err := local.LoadState(statePath); err != nil {
		log.Printf("warning: could not load persisted state: %v", err)
	} else if savedState != nil {
		// Only restore if the previous daemon is no longer running.
		if local.CheckStalePID(savedState) {
			log.Printf("previous daemon (pid %d) is still running, skipping restore", savedState.PID)
		} else {
			result := sm.RestoreSessions(savedState)
			if result.SessionCount > 0 {
				log.Printf("restored %d session(s), %d window(s), %d pane(s) from %s",
					result.SessionCount, result.WindowCount, result.PaneCount, statePath)
			}
			for _, e := range result.Errors {
				log.Printf("restore warning: %s", e)
			}
		}
	}

	persister.Start()
	defer persister.Stop()

	log.Printf("cmuxd-local %s listening on %s", version, socketPath)

	// Handle shutdown signals by closing the listener, which breaks the accept
	// loop and lets deferred cleanup (sm.CloseAll, listener.Close, os.Remove) run.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Println("shutting down...")
		listener.Close()
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return 0
			}
			log.Printf("accept error: %v", err)
			continue
		}
		go handleClient(conn, sm, persister)
	}
}

func defaultSocketPath() string {
	return local.DefaultSocketPath()
}

// clientState tracks per-connection state for a single client.
type clientState struct {
	conn         net.Conn
	reader       *bufio.Reader
	writer       *local.FrameWriter
	sm           *local.SessionManager
	persister    *local.StatePersister
	attachmentID string
	attachedSess *local.Session // non-nil while attached
	attachedPane *local.Pane    // non-nil while attached to a specific pane
}

func handleClient(conn net.Conn, sm *local.SessionManager, persister *local.StatePersister) {
	defer conn.Close()

	cs := &clientState{
		conn:         conn,
		reader:       bufio.NewReaderSize(conn, 64*1024),
		writer:       local.NewFrameWriter(conn),
		sm:           sm,
		persister:    persister,
		attachmentID: randomHex(8),
	}

	for {
		line, oversized, readErr := local.ReadRPCFrame(cs.reader)
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				cs.cleanupAttachment()
				return
			}
			log.Printf("client read error: %v", readErr)
			cs.cleanupAttachment()
			return
		}
		if oversized {
			_ = cs.writer.WriteResponse(local.ErrorResponse(nil, "invalid_request", "request frame exceeds maximum size"))
			continue
		}
		line = bytes.TrimSuffix(line, []byte{'\n'})
		line = bytes.TrimSuffix(line, []byte{'\r'})
		if len(line) == 0 {
			continue
		}

		var req local.RPCRequest
		if err := json.Unmarshal(line, &req); err != nil {
			_ = cs.writer.WriteResponse(local.ErrorResponse(nil, "invalid_request", "invalid JSON request"))
			continue
		}

		resp := cs.handleRequest(req)
		if err := cs.writer.WriteResponse(resp); err != nil {
			log.Printf("client write error: %v", err)
			cs.cleanupAttachment()
			return
		}
	}
}

func (cs *clientState) cleanupAttachment() {
	if cs.attachedPane != nil {
		cs.attachedPane.Detach(cs.attachmentID)
		cs.attachedPane = nil
	}
	if cs.attachedSess != nil {
		cs.attachedSess = nil
	}
}

func (cs *clientState) handleRequest(req local.RPCRequest) local.RPCResponse {
	if req.Method == "" {
		return local.ErrorResponse(req.ID, "invalid_request", "method is required")
	}

	switch req.Method {
	case "hello":
		return local.OKResponse(req.ID, map[string]any{
			"name":    "cmuxd-local",
			"version": version,
			"capabilities": []string{
				"session.pty",
				"session.detach",
				"session.replay",
				"window.management",
				"pane.management",
			},
		})

	case "ping":
		return local.OKResponse(req.ID, map[string]any{"pong": true})

	case "session.new":
		return cs.handleSessionNew(req)
	case "session.list":
		return cs.handleSessionList(req)
	case "session.attach":
		return cs.handleSessionAttach(req)
	case "session.detach":
		return cs.handleSessionDetach(req)
	case "session.resize":
		return cs.handleSessionResize(req)
	case "session.close":
		return cs.handleSessionClose(req)

	case "window.new":
		return cs.handleWindowNew(req)
	case "window.list":
		return cs.handleWindowList(req)
	case "window.close":
		return cs.handleWindowClose(req)

	case "pane.split":
		return cs.handlePaneSplit(req)
	case "pane.close":
		return cs.handlePaneClose(req)
	case "pane.focus":
		return cs.handlePaneFocus(req)

	case "pty.input":
		return cs.handlePtyInput(req)

	default:
		return local.ErrorResponse(req.ID, "method_not_found", fmt.Sprintf("unknown method %q", req.Method))
	}
}

// ---------------------------------------------------------------------------
// Session methods
// ---------------------------------------------------------------------------

func (cs *clientState) handleSessionNew(req local.RPCRequest) local.RPCResponse {
	name, _ := local.GetStringParam(req.Params, "name")
	shell, _ := local.GetStringParam(req.Params, "shell")
	cols, _ := local.GetIntParam(req.Params, "cols")
	rows, _ := local.GetIntParam(req.Params, "rows")
	env, _ := local.GetStringMapParam(req.Params, "env")

	sess, err := cs.sm.Create(local.NewSessionParams{
		Name:  name,
		Shell: shell,
		Cols:  cols,
		Rows:  rows,
		Env:   env,
	})
	if err != nil {
		return local.ErrorResponse(req.ID, "create_failed", err.Error())
	}

	cs.persister.NotifyChange()
	return local.OKResponse(req.ID, sess.Snapshot())
}

func (cs *clientState) handleSessionList(req local.RPCRequest) local.RPCResponse {
	return local.OKResponse(req.ID, map[string]any{
		"sessions": cs.sm.List(),
	})
}

func (cs *clientState) handleSessionAttach(req local.RPCRequest) local.RPCResponse {
	sessionID, ok := local.GetStringParam(req.Params, "session_id")
	if !ok || sessionID == "" {
		return local.ErrorResponse(req.ID, "invalid_params", "session.attach requires session_id")
	}

	sess, ok := cs.sm.FindByNameOrID(sessionID)
	if !ok {
		return local.ErrorResponse(req.ID, "not_found", "session not found")
	}

	// Determine which pane to attach to.
	var pane *local.Pane
	if paneID, ok := local.GetStringParam(req.Params, "pane_id"); ok && paneID != "" {
		p, _, found := sess.FindPane(paneID)
		if !found {
			return local.ErrorResponse(req.ID, "not_found", "pane not found")
		}
		pane = p
	} else {
		pane = sess.ActivePane()
		if pane == nil {
			return local.ErrorResponse(req.ID, "not_found", "session has no active pane")
		}
	}

	// If cols/rows provided, resize the target pane.
	if cols, ok := local.GetIntParam(req.Params, "cols"); ok && cols > 0 {
		if rows, ok := local.GetIntParam(req.Params, "rows"); ok && rows > 0 {
			_ = pane.Resize(cols, rows)
		}
	}

	// Detach from any previous attachment on this connection.
	cs.cleanupAttachment()

	// Attach to the pane.
	replay := pane.Attach(cs.attachmentID, cs.writer)
	cs.attachedSess = sess
	cs.attachedPane = pane

	// Build response with window_id and pane_id.
	result := sess.Snapshot()
	result["window_id"] = sess.GetActiveWindowID()
	result["pane_id"] = pane.ID

	resp := local.OKResponse(req.ID, result)

	// Queue replay event.
	go func() {
		_ = cs.writer.WriteEvent(local.RPCEvent{
			Event:      "pty.replay",
			SessionID:  sess.ID,
			PaneID:     pane.ID,
			DataBase64: base64.StdEncoding.EncodeToString(replay),
			ReplayDone: true,
		})
	}()

	return resp
}

func (cs *clientState) handleSessionDetach(req local.RPCRequest) local.RPCResponse {
	sessionID, ok := local.GetStringParam(req.Params, "session_id")
	if !ok || sessionID == "" {
		return local.ErrorResponse(req.ID, "invalid_params", "session.detach requires session_id")
	}

	sess, ok := cs.sm.FindByNameOrID(sessionID)
	if !ok {
		return local.ErrorResponse(req.ID, "not_found", "session not found")
	}

	sess.Detach(cs.attachmentID)
	if cs.attachedSess == sess {
		cs.attachedSess = nil
		cs.attachedPane = nil
	}

	return local.OKResponse(req.ID, map[string]any{
		"session_id": sess.ID,
		"detached":   true,
	})
}

func (cs *clientState) handleSessionResize(req local.RPCRequest) local.RPCResponse {
	sessionID, ok := local.GetStringParam(req.Params, "session_id")
	if !ok || sessionID == "" {
		return local.ErrorResponse(req.ID, "invalid_params", "session.resize requires session_id")
	}
	cols, ok := local.GetIntParam(req.Params, "cols")
	if !ok || cols <= 0 {
		return local.ErrorResponse(req.ID, "invalid_params", "session.resize requires cols > 0")
	}
	rows, ok := local.GetIntParam(req.Params, "rows")
	if !ok || rows <= 0 {
		return local.ErrorResponse(req.ID, "invalid_params", "session.resize requires rows > 0")
	}

	sess, ok := cs.sm.FindByNameOrID(sessionID)
	if !ok {
		return local.ErrorResponse(req.ID, "not_found", "session not found")
	}

	// If pane_id specified, resize that pane; otherwise resize active pane.
	if paneID, ok := local.GetStringParam(req.Params, "pane_id"); ok && paneID != "" {
		p, _, found := sess.FindPane(paneID)
		if !found {
			return local.ErrorResponse(req.ID, "not_found", "pane not found")
		}
		if err := p.Resize(cols, rows); err != nil {
			return local.ErrorResponse(req.ID, "resize_failed", err.Error())
		}
	} else {
		if err := sess.Resize(cols, rows); err != nil {
			return local.ErrorResponse(req.ID, "resize_failed", err.Error())
		}
	}

	return local.OKResponse(req.ID, sess.Snapshot())
}

func (cs *clientState) handleSessionClose(req local.RPCRequest) local.RPCResponse {
	sessionID, ok := local.GetStringParam(req.Params, "session_id")
	if !ok || sessionID == "" {
		return local.ErrorResponse(req.ID, "invalid_params", "session.close requires session_id")
	}

	sess, ok := cs.sm.FindByNameOrID(sessionID)
	if !ok {
		return local.ErrorResponse(req.ID, "not_found", "session not found")
	}

	// Detach us if we're attached to this session.
	if cs.attachedSess == sess {
		cs.attachedSess = nil
		cs.attachedPane = nil
	}

	removed := cs.sm.Remove(sess.ID)
	if !removed {
		return local.ErrorResponse(req.ID, "not_found", "session not found")
	}

	cs.persister.NotifyChange()
	return local.OKResponse(req.ID, map[string]any{
		"session_id": sess.ID,
		"closed":     true,
	})
}

// ---------------------------------------------------------------------------
// Window methods
// ---------------------------------------------------------------------------

func (cs *clientState) handleWindowNew(req local.RPCRequest) local.RPCResponse {
	sessionID, ok := local.GetStringParam(req.Params, "session_id")
	if !ok || sessionID == "" {
		return local.ErrorResponse(req.ID, "invalid_params", "window.new requires session_id")
	}

	sess, ok := cs.sm.FindByNameOrID(sessionID)
	if !ok {
		return local.ErrorResponse(req.ID, "not_found", "session not found")
	}

	name, _ := local.GetStringParam(req.Params, "name")
	shell, _ := local.GetStringParam(req.Params, "shell")
	cols, _ := local.GetIntParam(req.Params, "cols")
	rows, _ := local.GetIntParam(req.Params, "rows")

	w, _, err := sess.AddWindow(name, shell, cols, rows, nil)
	if err != nil {
		return local.ErrorResponse(req.ID, "create_failed", err.Error())
	}

	cs.persister.NotifyChange()
	return local.OKResponse(req.ID, w.Snapshot())
}

func (cs *clientState) handleWindowList(req local.RPCRequest) local.RPCResponse {
	sessionID, ok := local.GetStringParam(req.Params, "session_id")
	if !ok || sessionID == "" {
		return local.ErrorResponse(req.ID, "invalid_params", "window.list requires session_id")
	}

	sess, ok := cs.sm.FindByNameOrID(sessionID)
	if !ok {
		return local.ErrorResponse(req.ID, "not_found", "session not found")
	}

	return local.OKResponse(req.ID, map[string]any{
		"session_id": sess.ID,
		"windows":    sess.ListWindows(),
	})
}

func (cs *clientState) handleWindowClose(req local.RPCRequest) local.RPCResponse {
	sessionID, ok := local.GetStringParam(req.Params, "session_id")
	if !ok || sessionID == "" {
		return local.ErrorResponse(req.ID, "invalid_params", "window.close requires session_id")
	}
	windowID, ok := local.GetStringParam(req.Params, "window_id")
	if !ok || windowID == "" {
		return local.ErrorResponse(req.ID, "invalid_params", "window.close requires window_id")
	}

	sess, ok := cs.sm.FindByNameOrID(sessionID)
	if !ok {
		return local.ErrorResponse(req.ID, "not_found", "session not found")
	}

	// If we're attached to a pane in this window, clean up.
	if cs.attachedPane != nil && cs.attachedSess == sess {
		if _, w, found := sess.FindPane(cs.attachedPane.ID); found && w.ID == windowID {
			cs.attachedPane.Detach(cs.attachmentID)
			cs.attachedPane = nil
			cs.attachedSess = nil
		}
	}

	removed := sess.RemoveWindow(windowID)
	if !removed {
		return local.ErrorResponse(req.ID, "not_found", "window not found")
	}

	cs.persister.NotifyChange()
	return local.OKResponse(req.ID, map[string]any{
		"session_id": sess.ID,
		"window_id":  windowID,
		"closed":     true,
	})
}

// ---------------------------------------------------------------------------
// Pane methods
// ---------------------------------------------------------------------------

func (cs *clientState) handlePaneSplit(req local.RPCRequest) local.RPCResponse {
	sessionID, ok := local.GetStringParam(req.Params, "session_id")
	if !ok || sessionID == "" {
		return local.ErrorResponse(req.ID, "invalid_params", "pane.split requires session_id")
	}
	windowID, ok := local.GetStringParam(req.Params, "window_id")
	if !ok || windowID == "" {
		return local.ErrorResponse(req.ID, "invalid_params", "pane.split requires window_id")
	}

	sess, ok := cs.sm.FindByNameOrID(sessionID)
	if !ok {
		return local.ErrorResponse(req.ID, "not_found", "session not found")
	}

	w, ok := sess.GetWindow(windowID)
	if !ok {
		return local.ErrorResponse(req.ID, "not_found", "window not found")
	}

	shell, _ := local.GetStringParam(req.Params, "shell")
	cols, _ := local.GetIntParam(req.Params, "cols")
	rows, _ := local.GetIntParam(req.Params, "rows")
	direction, _ := local.GetStringParam(req.Params, "direction")

	p, err := w.AddPane(sess.ID, shell, cols, rows, nil)
	if err != nil {
		return local.ErrorResponse(req.ID, "create_failed", err.Error())
	}

	cs.persister.NotifyChange()

	result := p.Snapshot()
	result["window_id"] = w.ID
	result["session_id"] = sess.ID
	if direction != "" {
		result["direction"] = direction
	}

	return local.OKResponse(req.ID, result)
}

func (cs *clientState) handlePaneClose(req local.RPCRequest) local.RPCResponse {
	sessionID, ok := local.GetStringParam(req.Params, "session_id")
	if !ok || sessionID == "" {
		return local.ErrorResponse(req.ID, "invalid_params", "pane.close requires session_id")
	}
	windowID, ok := local.GetStringParam(req.Params, "window_id")
	if !ok || windowID == "" {
		return local.ErrorResponse(req.ID, "invalid_params", "pane.close requires window_id")
	}
	paneID, ok := local.GetStringParam(req.Params, "pane_id")
	if !ok || paneID == "" {
		return local.ErrorResponse(req.ID, "invalid_params", "pane.close requires pane_id")
	}

	sess, ok := cs.sm.FindByNameOrID(sessionID)
	if !ok {
		return local.ErrorResponse(req.ID, "not_found", "session not found")
	}

	w, ok := sess.GetWindow(windowID)
	if !ok {
		return local.ErrorResponse(req.ID, "not_found", "window not found")
	}

	// If we're attached to this pane, clean up.
	if cs.attachedPane != nil && cs.attachedPane.ID == paneID {
		cs.attachedPane.Detach(cs.attachmentID)
		cs.attachedPane = nil
		cs.attachedSess = nil
	}

	removed := w.RemovePane(paneID)
	if !removed {
		return local.ErrorResponse(req.ID, "not_found", "pane not found")
	}

	cs.persister.NotifyChange()
	return local.OKResponse(req.ID, map[string]any{
		"session_id": sess.ID,
		"window_id":  w.ID,
		"pane_id":    paneID,
		"closed":     true,
	})
}

func (cs *clientState) handlePaneFocus(req local.RPCRequest) local.RPCResponse {
	sessionID, ok := local.GetStringParam(req.Params, "session_id")
	if !ok || sessionID == "" {
		return local.ErrorResponse(req.ID, "invalid_params", "pane.focus requires session_id")
	}
	paneID, ok := local.GetStringParam(req.Params, "pane_id")
	if !ok || paneID == "" {
		return local.ErrorResponse(req.ID, "invalid_params", "pane.focus requires pane_id")
	}

	sess, ok := cs.sm.FindByNameOrID(sessionID)
	if !ok {
		return local.ErrorResponse(req.ID, "not_found", "session not found")
	}

	if !sess.SetActivePaneByID(paneID) {
		return local.ErrorResponse(req.ID, "not_found", "pane not found")
	}

	return local.OKResponse(req.ID, map[string]any{
		"session_id": sess.ID,
		"pane_id":    paneID,
		"focused":    true,
	})
}

// ---------------------------------------------------------------------------
// PTY input
// ---------------------------------------------------------------------------

func (cs *clientState) handlePtyInput(req local.RPCRequest) local.RPCResponse {
	dataBase64, ok := local.GetStringParam(req.Params, "data_base64")
	if !ok {
		return local.ErrorResponse(req.ID, "invalid_params", "pty.input requires data_base64")
	}
	data, err := base64.StdEncoding.DecodeString(dataBase64)
	if err != nil {
		return local.ErrorResponse(req.ID, "invalid_params", "data_base64 must be valid base64")
	}

	// If pane_id is provided, write directly to that pane.
	if paneID, ok := local.GetStringParam(req.Params, "pane_id"); ok && paneID != "" {
		sessionID, _ := local.GetStringParam(req.Params, "session_id")
		if sessionID == "" {
			return local.ErrorResponse(req.ID, "invalid_params", "pty.input with pane_id requires session_id")
		}
		sess, ok := cs.sm.FindByNameOrID(sessionID)
		if !ok {
			return local.ErrorResponse(req.ID, "not_found", "session not found")
		}
		p, _, found := sess.FindPane(paneID)
		if !found {
			return local.ErrorResponse(req.ID, "not_found", "pane not found")
		}
		if err := p.WriteInput(data); err != nil {
			return local.ErrorResponse(req.ID, "write_failed", err.Error())
		}
		return local.OKResponse(req.ID, map[string]any{"written": len(data)})
	}

	// Fallback: use session_id, route to active pane.
	sessionID, ok := local.GetStringParam(req.Params, "session_id")
	if !ok || sessionID == "" {
		return local.ErrorResponse(req.ID, "invalid_params", "pty.input requires session_id or pane_id")
	}

	sess, ok := cs.sm.FindByNameOrID(sessionID)
	if !ok {
		return local.ErrorResponse(req.ID, "not_found", "session not found")
	}

	if err := sess.WriteInput(data); err != nil {
		return local.ErrorResponse(req.ID, "write_failed", err.Error())
	}

	return local.OKResponse(req.ID, map[string]any{"written": len(data)})
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
