package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	local "github.com/manaflow-ai/cmux/daemon/local"
	"golang.org/x/term"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return 2
	}

	switch args[0] {
	case "new":
		return cmdNew(args[1:])
	case "attach":
		return cmdAttach(args[1:])
	case "ls", "list":
		return cmdList(args[1:])
	case "kill":
		return cmdKill(args[1:])
	case "help", "--help", "-h":
		usage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "cmux-local: unknown command %q\n", args[0])
		usage()
		return 2
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "Usage: cmux-local <command> [options]")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Commands:")
	fmt.Fprintln(os.Stderr, "  new [-s name] [-- command args...]   Create a new session and attach")
	fmt.Fprintln(os.Stderr, "  attach [-t name|id]                  Attach to an existing session")
	fmt.Fprintln(os.Stderr, "  ls                                   List all sessions")
	fmt.Fprintln(os.Stderr, "  kill [-t name|id]                    Kill a session")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Detach hotkey: Ctrl-b d")
}

// --- RPC types (mirrors the daemon's protocol) ---

type rpcRequest struct {
	ID     any            `json:"id"`
	Method string         `json:"method"`
	Params map[string]any `json:"params"`
}

type rpcResponse struct {
	ID     any            `json:"id,omitempty"`
	OK     bool           `json:"ok"`
	Result map[string]any `json:"result,omitempty"`
	Error  *rpcError      `json:"error,omitempty"`
}

type rpcError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type rpcEvent struct {
	Event      string `json:"event"`
	SessionID  string `json:"session_id,omitempty"`
	DataBase64 string `json:"data_base64,omitempty"`
	ReplayDone bool   `json:"replay_done,omitempty"`
	Error      string `json:"error,omitempty"`
}

// --- Socket helpers ---

func socketPath() string {
	if p := os.Getenv("CMUX_LOCAL_SOCKET"); p != "" {
		return p
	}
	// Primary: private socket in ~/.local/state/cmux/
	primary := local.DefaultSocketPath()
	if _, err := os.Stat(primary); err == nil {
		return primary
	}
	// Fallback: legacy /tmp path (may be a symlink to the new location)
	legacy := local.LegacySocketPath()
	if _, err := os.Stat(legacy); err == nil {
		return legacy
	}
	// Default to primary even if it doesn't exist yet (daemon may start later)
	return primary
}

func dialDaemon() (net.Conn, error) {
	return net.DialTimeout("unix", socketPath(), 2*time.Second)
}

var nextReqID int

func sendRPC(conn net.Conn, method string, params map[string]any) error {
	nextReqID++
	req := rpcRequest{
		ID:     nextReqID,
		Method: method,
		Params: params,
	}
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	_, err = conn.Write(append(data, '\n'))
	return err
}

// readOneJSON reads a single newline-delimited JSON object from the reader
// and unmarshals it into dst.
func readOneJSON(reader *bufio.Reader, dst any) error {
	line, err := reader.ReadString('\n')
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(line), dst)
}

// rpcCall does a single request-response round trip.
func rpcCall(method string, params map[string]any) (map[string]any, error) {
	conn, err := dialDaemon()
	if err != nil {
		return nil, fmt.Errorf("cannot connect to daemon at %s: %w", socketPath(), err)
	}
	defer conn.Close()

	if err := sendRPC(conn, method, params); err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}

	reader := bufio.NewReader(conn)
	var resp rpcResponse
	if err := readOneJSON(reader, &resp); err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}
	if !resp.OK {
		if resp.Error != nil {
			return nil, fmt.Errorf("server error [%s]: %s", resp.Error.Code, resp.Error.Message)
		}
		return nil, fmt.Errorf("server returned error")
	}
	return resp.Result, nil
}

// --- Commands ---

func cmdNew(args []string) int {
	var name string
	var shell string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-s", "--name":
			if i+1 < len(args) {
				name = args[i+1]
				i++
			}
		case "--shell":
			if i+1 < len(args) {
				shell = args[i+1]
				i++
			}
		case "--":
			// Remaining args become the shell command
			if i+1 < len(args) {
				shell = strings.Join(args[i+1:], " ")
			}
			goto done
		}
	}
done:

	cols, rows := terminalSize()

	params := map[string]any{
		"cols": cols,
		"rows": rows,
	}
	if name != "" {
		params["name"] = name
	}
	if shell != "" {
		params["shell"] = shell
	}

	result, err := rpcCall("session.new", params)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cmux-local: %v\n", err)
		return 1
	}

	sessionID, _ := result["session_id"].(string)
	if sessionID == "" {
		fmt.Fprintln(os.Stderr, "cmux-local: session created but no session_id returned")
		return 1
	}

	fmt.Fprintf(os.Stderr, "[session %s created]\n", sessionID)
	return attachToSession(sessionID, cols, rows)
}

func cmdAttach(args []string) int {
	var target string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-t", "--target":
			if i+1 < len(args) {
				target = args[i+1]
				i++
			}
		default:
			if target == "" {
				target = args[i]
			}
		}
	}

	if target == "" {
		fmt.Fprintln(os.Stderr, "cmux-local attach: requires -t <name|id>")
		return 2
	}

	cols, rows := terminalSize()
	return attachToSession(target, cols, rows)
}

func cmdList(args []string) int {
	result, err := rpcCall("session.list", nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cmux-local: %v\n", err)
		return 1
	}

	sessionsRaw, ok := result["sessions"]
	if !ok {
		fmt.Println("No sessions.")
		return 0
	}

	sessions, ok := sessionsRaw.([]any)
	if !ok || len(sessions) == 0 {
		fmt.Println("No sessions.")
		return 0
	}

	for _, raw := range sessions {
		s, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		id, _ := s["session_id"].(string)
		name, _ := s["name"].(string)
		status, _ := s["status"].(string)
		pid := jsonInt(s["pid"])
		attached := jsonInt(s["attached"])
		createdAt, _ := s["created_at"].(string)
		windowCount := jsonInt(s["window_count"])
		paneCount := jsonInt(s["pane_count"])

		attachStr := "detached"
		if attached > 0 {
			attachStr = fmt.Sprintf("attached (%d)", attached)
		}

		fmt.Printf("%-12s %-16s %-10s pid=%-8d %dW/%dP  %s  %s\n",
			id, name, status, pid, windowCount, paneCount, attachStr, createdAt)
	}
	return 0
}

func cmdKill(args []string) int {
	var target string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-t", "--target":
			if i+1 < len(args) {
				target = args[i+1]
				i++
			}
		default:
			if target == "" {
				target = args[i]
			}
		}
	}

	if target == "" {
		fmt.Fprintln(os.Stderr, "cmux-local kill: requires -t <name|id>")
		return 2
	}

	_, err := rpcCall("session.close", map[string]any{"session_id": target})
	if err != nil {
		fmt.Fprintf(os.Stderr, "cmux-local: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "[session %s killed]\n", target)
	return 0
}

// --- Attach mode ---

func attachToSession(sessionID string, cols, rows int) int {
	conn, err := dialDaemon()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cmux-local: cannot connect to daemon: %v\n", err)
		return 1
	}
	defer conn.Close()

	// Send session.attach
	if err := sendRPC(conn, "session.attach", map[string]any{
		"session_id": sessionID,
		"cols":       cols,
		"rows":       rows,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "cmux-local: failed to send attach: %v\n", err)
		return 1
	}

	reader := bufio.NewReader(conn)

	// Read attach response
	var resp rpcResponse
	if err := readOneJSON(reader, &resp); err != nil {
		fmt.Fprintf(os.Stderr, "cmux-local: failed to read attach response: %v\n", err)
		return 1
	}
	if !resp.OK {
		if resp.Error != nil {
			fmt.Fprintf(os.Stderr, "cmux-local: attach failed [%s]: %s\n", resp.Error.Code, resp.Error.Message)
		} else {
			fmt.Fprintln(os.Stderr, "cmux-local: attach failed")
		}
		return 1
	}

	// Resolve actual session_id from response (in case we attached by name)
	if result := resp.Result; result != nil {
		if sid, ok := result["session_id"].(string); ok {
			sessionID = sid
		}
	}

	// Set terminal to raw mode
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		fmt.Fprintf(os.Stderr, "cmux-local: failed to set raw mode: %v\n", err)
		return 1
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	// Mutex to serialize concurrent writes to conn (SIGWINCH + stdin goroutines).
	var connMu sync.Mutex

	// Channel to signal detach or session exit (buffered for all senders).
	doneCh := make(chan int, 3)

	// Read events from daemon (pty.replay, pty.output, session.exited)
	go func() {
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				doneCh <- 1
				return
			}

			// Try to parse as event
			var event rpcEvent
			if err := json.Unmarshal([]byte(line), &event); err != nil {
				// Might be an RPC response (e.g., for pty.input); ignore
				continue
			}

			switch event.Event {
			case "pty.replay", "pty.output":
				if event.DataBase64 != "" {
					data, err := base64.StdEncoding.DecodeString(event.DataBase64)
					if err == nil {
						os.Stdout.Write(data)
					}
				}
			case "session.exited":
				fmt.Fprintf(os.Stderr, "\r\n[session %s exited]\r\n", sessionID)
				doneCh <- 0
				return
			}
		}
	}()

	// Handle SIGWINCH
	winchCh := make(chan os.Signal, 1)
	signal.Notify(winchCh, syscall.SIGWINCH)
	go func() {
		for range winchCh {
			c, r := terminalSize()
			connMu.Lock()
			_ = sendRPC(conn, "session.resize", map[string]any{
				"session_id": sessionID,
				"cols":       c,
				"rows":       r,
			})
			connMu.Unlock()
		}
	}()
	defer signal.Stop(winchCh)

	// Read stdin and send as pty.input; detect Ctrl-b d for detach
	go func() {
		buf := make([]byte, 4096)
		var pendingCtrlB bool // true when previous read ended with Ctrl-b

		for {
			n, err := os.Stdin.Read(buf)
			if err != nil {
				doneCh <- 1
				return
			}

			data := buf[:n]

			// Build the output to send, handling the Ctrl-b d detach sequence.
			// We scan byte-by-byte, deferring any trailing Ctrl-b.
			var toSend []byte
			hadPending := pendingCtrlB
			pendingCtrlB = false

			for i := 0; i < len(data); i++ {
				b := data[i]
				if hadPending {
					hadPending = false
					if b == 'd' {
						// Detach hotkey: Ctrl-b d
						// Send any accumulated bytes first
						connMu.Lock()
						if len(toSend) > 0 {
							_ = sendRPC(conn, "pty.input", map[string]any{
								"session_id":  sessionID,
								"data_base64": base64.StdEncoding.EncodeToString(toSend),
							})
						}
						_ = sendRPC(conn, "session.detach", map[string]any{
							"session_id": sessionID,
						})
						connMu.Unlock()
						doneCh <- 0
						return
					}
					// Not 'd' — flush the deferred Ctrl-b
					toSend = append(toSend, 0x02)
				}

				if b == 0x02 {
					// Defer this Ctrl-b — it might be the start of Ctrl-b d
					hadPending = true
					continue
				}
				toSend = append(toSend, b)
			}

			// If the buffer ended with a Ctrl-b, defer it to the next read
			pendingCtrlB = hadPending

			if len(toSend) > 0 {
				connMu.Lock()
				_ = sendRPC(conn, "pty.input", map[string]any{
					"session_id":  sessionID,
					"data_base64": base64.StdEncoding.EncodeToString(toSend),
				})
				connMu.Unlock()
			}
		}
	}()

	code := <-doneCh

	// Restore terminal before printing
	term.Restore(int(os.Stdin.Fd()), oldState)

	if code == 0 {
		fmt.Fprintf(os.Stderr, "[detached from %s]\n", sessionID)
	}
	return code
}

func terminalSize() (int, int) {
	w, h, err := term.GetSize(int(os.Stdin.Fd()))
	if err != nil {
		return 80, 24
	}
	return w, h
}

func jsonInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	default:
		return 0
	}
}
