# cmux local daemon

PTY-based detach/reattach daemon, similar to tmux. Manages local terminal sessions
that persist when clients disconnect.

## Build

```bash
go build -o cmuxd-local ./cmd/cmuxd-local/
go build -o cmux-local  ./cmd/cmux-local/
```

## Usage

Start the daemon:

```bash
./cmuxd-local
```

The daemon listens on `~/.local/state/cmux/daemon-local.sock`. A backward-compatible
symlink is created at `/tmp/cmux-local-<uid>.sock` for older clients.

Create a session and attach:

```bash
./cmux-local new -s dev
```

Detach with `Ctrl-b d`.

List sessions:

```bash
./cmux-local ls
```

Reattach:

```bash
./cmux-local attach -t dev
```

Kill a session:

```bash
./cmux-local kill -t dev
```

## Protocol

Uses newline-delimited JSON-RPC over a Unix domain socket. Same framing as the
remote daemon.

### RPC methods

| Method           | Description                          |
|------------------|--------------------------------------|
| `hello`          | Handshake, return capabilities       |
| `ping`           | Health check                         |
| `session.new`    | Create a new PTY session             |
| `session.list`   | List all sessions                    |
| `session.attach` | Attach to a session (starts streaming) |
| `session.detach` | Detach from a session                |
| `session.resize` | Resize a session's PTY               |
| `session.close`  | Kill a session                       |
| `pty.input`      | Send keyboard input to a session     |

### Events (server push)

| Event            | Description                          |
|------------------|--------------------------------------|
| `pty.replay`     | Ring buffer replay on attach         |
| `pty.output`     | Live PTY output                      |
| `session.exited` | Child process exited                 |

## Architecture

- `session.go` — Session and PTY lifecycle management via `github.com/creack/pty`
- `ringbuffer.go` — Fixed-size circular buffer (2MB) for output replay
- `rpc.go` — JSON-RPC framing, helpers shared by daemon and library consumers
- `cmd/cmuxd-local/` — Daemon server binary
- `cmd/cmux-local/` — CLI client binary
