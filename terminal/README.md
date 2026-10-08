# pi-pocket web terminal

A small Go daemon baked into the pi-pocket image. It serves a
[ghostty-web](https://github.com/coder/ghostty-web) terminal frontend and
bridges one WebSocket connection to one PTY shell running in the same
container, so a browser gets a real shell with the workspace, home directory,
environment, and devtools pi-pocket itself uses.

This is intentionally not a pi-pocket extension: extensions can add tools,
prompt sections, hooks, and tasks, but they have no web-UI or HTTP-route
surface, and a terminal needs both plus a PTY backend.

## Layout

- `main.go`, `config.go`, `server.go` — daemon (env-only config, HTTP routes,
  owner-token auth, in-memory sessions, WebSocket↔PTY bridge).
- `web/index.html`, `web/terminal.js`, `web/terminal.css` — the frontend.
- `web/static/ghostty-web.js`, `web/static/ghostty-vt.wasm` — vendored from
  `ghostty-web@0.4.0` (MIT, see `GHOSTTY-WEB-LICENSE`). No `FitAddon` exists
  in 0.4.0, so `terminal.js` fits the grid itself by measuring the cell.

## Protocol

Mirrors the ghostty-web demo framing:

- Page → server: raw text is shell stdin; `{"type":"resize","cols":N,"rows":N}`
  resizes; `{"type":"ping"}` is a keepalive the server ignores (idle sockets
  look dead to routers and load balancers).
- Server → page: raw PTY bytes (binary); `{"type":"exit","code":N}` ends the
  shell, then the socket closes.

## Authentication

The daemon reads `$PI_POCKET_DIR/config.json` on every check, so an owner
token rotation needs no restart. `GET /?token=` verifies (constant-time)
and sets a random `__Host-pi-terminal` session cookie (Secure, HttpOnly,
SameSite Lax, 12 h), then redirects to the clean URL. The cookie alone opens
the page and `/ws`; `/ws` also accepts `?token=` directly. `/healthz` is
public. Without a token on disk every check fails closed.

## Environment

| Variable | Default | Description |
| --- | --- | --- |
| `TERMINAL_PORT` | `8081` | Listen port. |
| `TERMINAL_FRAME_ANCESTORS` | empty (deny framing) | Extra `frame-ancestors` origin, e.g. the portal origin for iframe embedding. |
| `PI_POCKET_DIR` | `/workspace/home/.pi-pocket` | Where `config.json` with the owner token lives. |
| `TERMINAL_SHELL` / `TERMINAL_SHELL_ARGS` | `bash` / empty | Shell for PTY sessions (space-separated args). |
| `TERMINAL_CWD` | `/workspace/repos` | Session working directory, falling back to `$HOME`. |

Sessions are capped (16 concurrent shells, 128 stored ids) and shells inherit
the daemon environment, including provider keys the entrypoint exports.

## Security notes

- Keystrokes are never logged; only connects, disconnects, and exit codes.
- A human typing in the terminal bypasses Lancet Guard, exactly like the
  owner's own `!`-commands. Only the owner token opens it.
- The frontend needs `wasm-unsafe-eval` in `script-src` to compile the
  vendored WASM parser. No SharedArrayBuffer is used, so no COOP/COEP dance.

## Development

```bash
cd terminal
go test -race ./...
```

`TERMINAL_SHELL=cat` turns the daemon into an echo server, which is what the
WebSocket tests use.
