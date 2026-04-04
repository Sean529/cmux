#!/usr/bin/env bash
#
# End-to-end test for the cmuxd-local daemon and cmux-local CLI.
#
# Exercises the full daemon lifecycle via JSON-RPC over Unix socket:
#   - Session create / list / close
#   - Persistence across daemon restart
#   - Multiple sessions
#   - Resize
#
# Usage: bash daemon/local/test_e2e.sh

set -euo pipefail

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

PASS_COUNT=0
FAIL_COUNT=0
DAEMON_PID=""
TMPDIR_TEST=""

pass() {
  PASS_COUNT=$((PASS_COUNT + 1))
  echo "  PASS: $1"
}

fail() {
  FAIL_COUNT=$((FAIL_COUNT + 1))
  echo "  FAIL: $1"
  echo "        $2"
}

cleanup() {
  if [ -n "$DAEMON_PID" ] && kill -0 "$DAEMON_PID" 2>/dev/null; then
    kill "$DAEMON_PID" 2>/dev/null || true
    wait "$DAEMON_PID" 2>/dev/null || true
  fi
  if [ -n "$TMPDIR_TEST" ] && [ -d "$TMPDIR_TEST" ]; then
    rm -rf "$TMPDIR_TEST"
  fi
  # Clean up test socket and state file
  rm -f "$SOCKET_PATH" "$STATE_PATH" 2>/dev/null || true
}

trap cleanup EXIT

# Send an RPC request over the Unix socket and return the response.
# Uses a short-lived connection per request (like the CLI's rpcCall).
# All JSON construction and socket I/O is done in Python to avoid
# shell quoting issues with nested JSON.
rpc() {
  local method="$1"
  local params="${2+$2}"
  if [ -z "$params" ]; then params="{}"; fi
  local id="${3:-1}"
  # Write params to a temp file to avoid all shell quoting issues
  local params_file
  params_file="$(mktemp)"
  printf '%s' "$params" > "$params_file"
  python3 - "$SOCKET_PATH" "$method" "$id" "$params_file" << 'PYEOF'
import socket, json, sys, os

socket_path = sys.argv[1]
method = sys.argv[2]
req_id = int(sys.argv[3])
params_file = sys.argv[4]

with open(params_file) as f:
    params_str = f.read()
os.unlink(params_file)

sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
sock.settimeout(5)
try:
    sock.connect(socket_path)
except Exception as e:
    print(json.dumps({"ok": False, "error": {"code": "connect_failed", "message": str(e)}}))
    sys.exit(0)

try:
    params = json.loads(params_str) if params_str.strip() else {}
except json.JSONDecodeError:
    params = {}
request = json.dumps({"id": req_id, "method": method, "params": params})
sock.sendall((request + "\n").encode())

buf = b""
while True:
    try:
        chunk = sock.recv(4096)
    except socket.timeout:
        break
    if not chunk:
        break
    buf += chunk
    if b"\n" in buf:
        break

sock.close()

lines = buf.decode().strip().split("\n")
if lines and lines[0]:
    print(lines[0])
else:
    print(json.dumps({"ok": False, "error": {"code": "no_response", "message": "empty response"}}))
PYEOF
}

# Extract a field from JSON using python3 (portable, no jq dependency).
# Passes JSON via stdin to avoid shell quoting issues.
json_field() {
  local json="$1"
  local field="$2"
  echo "$json" | python3 -c "
import json, sys
data = json.loads(sys.stdin.read())
keys = sys.argv[1].split('.')
val = data
for k in keys:
    if isinstance(val, dict):
        val = val.get(k)
    else:
        val = None
        break
if val is None:
    sys.exit(1)
print(val)
" "$field" 2>/dev/null
}

# Count sessions from a session.list response.
count_sessions() {
  local json="$1"
  echo "$json" | python3 -c "
import json, sys
data = json.loads(sys.stdin.read())
sessions = data.get('result', {}).get('sessions', [])
print(len(sessions))
"
}

# Extract session_id from session.new or session.list result.
get_session_id() {
  local json="$1"
  echo "$json" | python3 -c "
import json, sys
data = json.loads(sys.stdin.read())
print(data['result']['session_id'])
"
}

# Get a field from a specific session in a list response by name.
get_session_field_by_name() {
  local json="$1"
  local name="$2"
  local field="$3"
  echo "$json" | python3 -c "
import json, sys
data = json.loads(sys.stdin.read())
name = sys.argv[1]
field = sys.argv[2]
sessions = data.get('result', {}).get('sessions', [])
for s in sessions:
    if s.get('name') == name:
        print(s.get(field, ''))
        break
" "$name" "$field"
}

reset_state() {
  rm -f "$SOCKET_PATH" "$STATE_PATH" 2>/dev/null || true
}

wait_for_socket() {
  local max_wait=5
  local waited=0
  while [ ! -S "$SOCKET_PATH" ] && [ $waited -lt $max_wait ]; do
    sleep 0.2
    waited=$((waited + 1))
  done
  if [ ! -S "$SOCKET_PATH" ]; then
    echo "ERROR: daemon socket did not appear at $SOCKET_PATH within ${max_wait}s"
    return 1
  fi
}

start_daemon() {
  "$DAEMON_BIN" &
  DAEMON_PID=$!
  wait_for_socket
}

stop_daemon() {
  if [ -n "$DAEMON_PID" ] && kill -0 "$DAEMON_PID" 2>/dev/null; then
    kill "$DAEMON_PID" 2>/dev/null || true
    wait "$DAEMON_PID" 2>/dev/null || true
  fi
  DAEMON_PID=""
  # Give the OS a moment to release the socket
  sleep 0.3
}

# ---------------------------------------------------------------------------
# Setup
# ---------------------------------------------------------------------------

echo "=== cmuxd-local E2E Test Suite ==="
echo ""

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

TMPDIR_TEST="$(mktemp -d /tmp/cmux-e2e-XXXXXX)"
BIN_DIR="$TMPDIR_TEST/bin"
mkdir -p "$BIN_DIR"

# Use a test-specific state directory so we don't disturb the user's real state.
export XDG_STATE_HOME="$TMPDIR_TEST/state"
TEST_STATE_DIR="$TMPDIR_TEST/state/cmux"
mkdir -p "$TEST_STATE_DIR"

# These need to match what the Go code uses. The Go code uses
# ~/.local/state/cmux/ (hardcoded via os.UserHomeDir). To isolate,
# we override HOME so the daemon writes to our temp dir.
export HOME="$TMPDIR_TEST/home"
mkdir -p "$HOME/.local/state/cmux"

SOCKET_PATH="$HOME/.local/state/cmux/daemon-local.sock"
STATE_PATH="$HOME/.local/state/cmux/daemon-local.json"

echo "Building binaries..."
(cd "$SCRIPT_DIR" && go build -o "$BIN_DIR/cmuxd-local" ./cmd/cmuxd-local) || {
  echo "FATAL: failed to build cmuxd-local"
  exit 1
}
(cd "$SCRIPT_DIR" && go build -o "$BIN_DIR/cmux-local" ./cmd/cmux-local) || {
  echo "FATAL: failed to build cmux-local"
  exit 1
}

DAEMON_BIN="$BIN_DIR/cmuxd-local"
CLI_BIN="$BIN_DIR/cmux-local"

echo "Binaries built at $BIN_DIR"
echo "Socket: $SOCKET_PATH"
echo "State:  $STATE_PATH"
echo ""

# ---------------------------------------------------------------------------
# Test 1: Session lifecycle (create, list, close)
# ---------------------------------------------------------------------------

echo "--- Test 1: Session Lifecycle ---"

reset_state
start_daemon

# Verify daemon is reachable with a ping
RESP=$(rpc "ping")
OK=$(json_field "$RESP" "ok" || echo "false")
if [ "$OK" = "True" ] || [ "$OK" = "true" ]; then
  pass "daemon responds to ping"
else
  fail "daemon ping" "response: $RESP"
fi

# Create a session
RESP=$(rpc "session.new" '{"name":"test1","cols":80,"rows":24}')
OK=$(json_field "$RESP" "ok" || echo "false")
if [ "$OK" = "True" ] || [ "$OK" = "true" ]; then
  SESSION1_ID=$(get_session_id "$RESP")
  pass "session.new created session $SESSION1_ID"
else
  fail "session.new" "response: $RESP"
  SESSION1_ID=""
fi

# List sessions - should have 1
RESP=$(rpc "session.list")
COUNT=$(count_sessions "$RESP")
if [ "$COUNT" = "1" ]; then
  pass "session.list shows 1 session"
else
  fail "session.list count" "expected 1, got $COUNT"
fi

# Close the session
if [ -n "$SESSION1_ID" ]; then
  RESP=$(rpc "session.close" "{\"session_id\":\"$SESSION1_ID\"}")
  OK=$(json_field "$RESP" "ok" || echo "false")
  if [ "$OK" = "True" ] || [ "$OK" = "true" ]; then
    pass "session.close succeeded"
  else
    fail "session.close" "response: $RESP"
  fi
fi

# List again - should be 0
RESP=$(rpc "session.list")
COUNT=$(count_sessions "$RESP")
if [ "$COUNT" = "0" ]; then
  pass "session.list shows 0 after close"
else
  fail "session.list after close" "expected 0, got $COUNT"
fi

stop_daemon
echo ""

# ---------------------------------------------------------------------------
# Test 2: Persistence across daemon restart
# ---------------------------------------------------------------------------

echo "--- Test 2: Persistence ---"

reset_state
start_daemon

# Create a session
RESP=$(rpc "session.new" '{"name":"persist-me","cols":80,"rows":24}')
PERSIST_ID=$(get_session_id "$RESP")
pass "created session $PERSIST_ID for persistence test"

# Wait a moment for async persistence
sleep 0.5

# Verify state file exists
if [ -f "$STATE_PATH" ]; then
  pass "state file exists at $STATE_PATH"
else
  fail "state file" "not found at $STATE_PATH"
fi

# Kill daemon (SIGTERM triggers graceful shutdown with final save)
stop_daemon

# Verify state file still exists after daemon exit
if [ -f "$STATE_PATH" ]; then
  pass "state file persists after daemon shutdown"
else
  fail "state file after shutdown" "not found"
fi

# Remove the stale socket if it exists (daemon cleanup may have removed it)
rm -f "$SOCKET_PATH" 2>/dev/null || true

# Restart daemon - it should restore the session
start_daemon

RESP=$(rpc "session.list")
COUNT=$(count_sessions "$RESP")
if [ "$COUNT" = "1" ]; then
  pass "session restored after daemon restart (1 session)"
else
  fail "session restore" "expected 1 session, got $COUNT. Response: $RESP"
fi

# Verify the restored session has the right name
RESTORED_NAME=$(get_session_field_by_name "$RESP" "persist-me" "name")
if [ "$RESTORED_NAME" = "persist-me" ]; then
  pass "restored session has correct name 'persist-me'"
else
  fail "restored session name" "expected 'persist-me', got '$RESTORED_NAME'"
fi

# Clean up the session
RESP=$(rpc "session.close" "{\"session_id\":\"$PERSIST_ID\"}")
stop_daemon
echo ""

# ---------------------------------------------------------------------------
# Test 3: Multiple sessions
# ---------------------------------------------------------------------------

echo "--- Test 3: Multiple Sessions ---"

reset_state
start_daemon

# Create 3 sessions
RESP=$(rpc "session.new" '{"name":"alpha","cols":80,"rows":24}' 1)
ALPHA_ID=$(get_session_id "$RESP")
RESP=$(rpc "session.new" '{"name":"beta","cols":80,"rows":24}' 2)
BETA_ID=$(get_session_id "$RESP")
RESP=$(rpc "session.new" '{"name":"gamma","cols":80,"rows":24}' 3)
GAMMA_ID=$(get_session_id "$RESP")

pass "created 3 sessions: alpha=$ALPHA_ID beta=$BETA_ID gamma=$GAMMA_ID"

# List should show 3
RESP=$(rpc "session.list" '{}' 4)
COUNT=$(count_sessions "$RESP")
if [ "$COUNT" = "3" ]; then
  pass "session.list shows 3 sessions"
else
  fail "session.list 3 sessions" "expected 3, got $COUNT"
fi

# Close beta
RESP=$(rpc "session.close" "{\"session_id\":\"$BETA_ID\"}" 5)
OK=$(json_field "$RESP" "ok" || echo "false")
if [ "$OK" = "True" ] || [ "$OK" = "true" ]; then
  pass "closed beta session"
else
  fail "close beta" "response: $RESP"
fi

# List should show 2
RESP=$(rpc "session.list" '{}' 6)
COUNT=$(count_sessions "$RESP")
if [ "$COUNT" = "2" ]; then
  pass "session.list shows 2 after closing beta"
else
  fail "session.list after close beta" "expected 2, got $COUNT"
fi

# Verify alpha and gamma still exist but beta is gone
ALPHA_CHECK=$(get_session_field_by_name "$RESP" "alpha" "name")
GAMMA_CHECK=$(get_session_field_by_name "$RESP" "gamma" "name")
BETA_CHECK=$(get_session_field_by_name "$RESP" "beta" "name")
if [ "$ALPHA_CHECK" = "alpha" ] && [ "$GAMMA_CHECK" = "gamma" ] && [ -z "$BETA_CHECK" ]; then
  pass "alpha and gamma remain, beta is gone"
else
  fail "session check after close" "alpha=$ALPHA_CHECK gamma=$GAMMA_CHECK beta=$BETA_CHECK"
fi

# Cleanup
rpc "session.close" "{\"session_id\":\"$ALPHA_ID\"}" 7 >/dev/null
rpc "session.close" "{\"session_id\":\"$GAMMA_ID\"}" 8 >/dev/null

stop_daemon
echo ""

# ---------------------------------------------------------------------------
# Test 4: Resize
# ---------------------------------------------------------------------------

echo "--- Test 4: Resize ---"

reset_state
start_daemon

RESP=$(rpc "session.new" '{"name":"resize-test","cols":80,"rows":24}' 1)
RESIZE_ID=$(get_session_id "$RESP")
pass "created session $RESIZE_ID for resize test"

# Resize via RPC
RESP=$(rpc "session.resize" "{\"session_id\":\"$RESIZE_ID\",\"cols\":120,\"rows\":40}" 3)
OK=$(json_field "$RESP" "ok" || echo "false")
if [ "$OK" = "True" ] || [ "$OK" = "true" ]; then
  pass "session.resize succeeded"
else
  fail "session.resize" "response: $RESP"
fi

# Verify the resize took effect via window.list which includes pane dimensions.
# First, get the active_window_id from the resize response.
WIN_ID=$(echo "$RESP" | python3 -c "
import json, sys
data = json.loads(sys.stdin.read())
print(data.get('result', {}).get('active_window_id', ''))
" 2>/dev/null)

RESP2=$(rpc "window.list" "{\"session_id\":\"$RESIZE_ID\"}" 4)
DIMS=$(echo "$RESP2" | python3 -c "
import json, sys
data = json.loads(sys.stdin.read())
windows = data.get('result', {}).get('windows', [])
for w in windows:
    for p in w.get('panes', []):
        print(p.get('cols', 'N/A'), p.get('rows', 'N/A'))
        break
    break
" 2>/dev/null)

NEW_COLS=$(echo "$DIMS" | awk '{print $1}')
NEW_ROWS=$(echo "$DIMS" | awk '{print $2}')

if [ "$NEW_COLS" = "120" ]; then
  pass "resize updated cols to 120"
else
  fail "resize cols" "expected 120, got $NEW_COLS"
fi

if [ "$NEW_ROWS" = "40" ]; then
  pass "resize updated rows to 40"
else
  fail "resize rows" "expected 40, got $NEW_ROWS"
fi

rpc "session.close" "{\"session_id\":\"$RESIZE_ID\"}" 5 >/dev/null

stop_daemon
echo ""

# ---------------------------------------------------------------------------
# Test 5: CLI ls command
# ---------------------------------------------------------------------------

echo "--- Test 5: CLI ls ---"

reset_state
start_daemon

# Create two sessions via RPC
rpc "session.new" '{"name":"cli-a","cols":80,"rows":24}' 1 >/dev/null
rpc "session.new" '{"name":"cli-b","cols":80,"rows":24}' 2 >/dev/null

# Use the CLI binary to list (it reads the same socket)
export CMUX_LOCAL_SOCKET="$SOCKET_PATH"
CLI_OUT=$("$CLI_BIN" ls 2>&1 || true)

# Verify both sessions appear in CLI output
if echo "$CLI_OUT" | grep -q "cli-a" && echo "$CLI_OUT" | grep -q "cli-b"; then
  pass "cmux-local ls shows both sessions"
else
  fail "cmux-local ls" "output: $CLI_OUT"
fi

stop_daemon
echo ""

# ---------------------------------------------------------------------------
# Test 6: Error handling
# ---------------------------------------------------------------------------

echo "--- Test 6: Error Handling ---"

reset_state
start_daemon

# Close a non-existent session
RESP=$(rpc "session.close" '{"session_id":"no-such-id"}' 1)
OK=$(json_field "$RESP" "ok" || echo "false")
ERR_CODE=$(json_field "$RESP" "error.code" || echo "unknown")
if [ "$OK" = "False" ] || [ "$OK" = "false" ]; then
  pass "closing non-existent session returns error ($ERR_CODE)"
else
  fail "close non-existent" "expected error, got ok. Response: $RESP"
fi

# Call unknown method
RESP=$(rpc "no.such.method" '{}' 2)
OK=$(json_field "$RESP" "ok" || echo "false")
if [ "$OK" = "False" ] || [ "$OK" = "false" ]; then
  pass "unknown method returns error"
else
  fail "unknown method" "expected error. Response: $RESP"
fi

# Resize with invalid params
RESP=$(rpc "session.resize" '{"session_id":"fake","cols":-1,"rows":0}' 3)
OK=$(json_field "$RESP" "ok" || echo "false")
if [ "$OK" = "False" ] || [ "$OK" = "false" ]; then
  pass "resize with invalid params returns error"
else
  fail "resize invalid params" "expected error. Response: $RESP"
fi

stop_daemon
echo ""

# ---------------------------------------------------------------------------
# Test 7: hello / capability check
# ---------------------------------------------------------------------------

echo "--- Test 7: Hello ---"

reset_state
start_daemon

RESP=$(rpc "hello" '{}' 1)
OK=$(json_field "$RESP" "ok" || echo "false")
if [ "$OK" = "True" ] || [ "$OK" = "true" ]; then
  VERSION=$(json_field "$RESP" "result.version" || echo "unknown")
  NAME=$(json_field "$RESP" "result.name" || echo "unknown")
  pass "hello response: name=$NAME version=$VERSION"
else
  fail "hello" "response: $RESP"
fi

stop_daemon
echo ""

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------

echo "==========================================="
echo " Results: $PASS_COUNT passed, $FAIL_COUNT failed"
echo "==========================================="

if [ "$FAIL_COUNT" -gt 0 ]; then
  exit 1
fi
exit 0
