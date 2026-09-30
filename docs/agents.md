# localdev: guide for coding agents

localdev gives every local web app a stable URL `http://<name>.localhost[:<daemon-port>]`
and reverse-proxies it (HTTP, WebSockets/HMR, SSE/streaming) to the app's real port.
A single daemon serves the proxy, a dashboard at `http://localhost[:port]`, and a JSON API.
The daemon is started automatically by any CLI command that needs it.

Never hardcode URLs. Always get them from `localdev url` or from `run`'s output, because the
daemon port can be 80 or a fallback (7777), and app ports can change.

## Recommended workflow

Run these from the project's directory:

```sh
# 1. Start the dev server under localdev (idempotent). Blocks until the port accepts
#    connections, then prints ONLY the URL on stdout. Progress/errors go to stderr.
URL=$(localdev run myapp -- 'npm run dev -- --port $PORT')

# 2. Use it.
curl -s "$URL/api/health"

# 3. Check output when something looks wrong.
localdev logs myapp -n 50

# 4. Stop it when you are done (the definition stays registered).
localdev stop myapp
```

- `$PORT` is set for the command. Single-quote the command so your shell does not expand `$PORT`.
  Arguments after `--` are joined with spaces and run by `/bin/sh -c`, as with ssh.
- If the command ignores `$PORT` and listens elsewhere (e.g. Vite's 5173), localdev detects the
  port its process group listens on and proxies there. Passing `$PORT` explicitly is still best.
- The command must stay in the foreground. A server that daemonizes itself exits right away and
  `run` fails with exit 5. Astro's `astro dev` does this when it detects a coding agent; set
  `ASTRO_DEV_BACKGROUND=1` to keep it in the foreground:
  `localdev run site -- 'ASTRO_DEV_BACKGROUND=1 npx astro dev --port $PORT'`.
- `run` is idempotent. If the app is already running with the same definition, nothing is restarted.
  If the command, cwd, env, or fixed port changed, it restarts. Re-running `run` is always safe.
- If you omit the name, localdev uses the app registered for the current directory, or else
  the directory's name sanitized to `[a-z0-9-]`.
- The command inherits the caller's environment (PATH, version managers, etc.) plus `PORT`,
  `LOCALDEV_NAME`, and `LOCALDEV_URL`. Use `--env K=V` for extra variables that should persist.

Server you already run yourself (localdev only proxies, it does not manage the process):

```sh
localdev register myapp --port 3000   # prints the URL
```

## Commands

Add `--json` anywhere before `--` to get machine-readable stdout. On failure with `--json`,
stderr carries `{"error": "...", "code": "..."}`.

| Command | Effect | stdout (human) |
|---|---|---|
| `run [name] [--port N] [--cwd D] [--env K=V] [--rewrite-host] [--no-preview] [--no-wait] [--timeout 60s] -- CMD` | upsert a managed app and ensure it runs | URL |
| `run [name]` | start an existing app (no redefinition) | URL |
| `register [name] --port N` / `--cmd CMD` | upsert a definition, don't start | URL |
| `unregister <name>` | stop and remove | nothing |
| `list` | all apps | table |
| `info [name]` | one app | key/value |
| `url [name]` | | URL |
| `start [name]` / `restart [name]` | ensure running / force restart, waits for readiness | URL |
| `stop [name]` | stop the process group (SIGTERM, SIGKILL after 5s) | nothing |
| `wait [name] [--timeout 60s]` | wait until the port accepts connections | URL |
| `logs [name] [-n 100] [-f]` | combined stdout/stderr of the current run (file capped at 10 MB, older output in `<log_file>.1`) | log text |
| `discover [--all]` | unregistered HTTP listeners on localhost (does not start the daemon) | table |
| `daemon start\|stop\|status\|run` | manage the daemon (`run` = foreground) | |
| `agent-help` | print this document | |

`register` and `run -- CMD` replace the whole definition. Flags you leave out go back to their
defaults, and `cwd` defaults to the current directory. Without `--port`, a managed app gets an
auto-assigned port (4100–4999) that stays stable across restarts when it's free.

## Exit codes

| code | meaning |
|---|---|
| 0 | success |
| 1 | other error (e.g. app not managed) |
| 2 | usage error / invalid name or arguments |
| 3 | app not found |
| 4 | daemon unavailable (only when `LOCALDEV_NO_AUTOSTART=1` or it failed to start) |
| 5 | app failed to start: the process exited, or its fixed port is already in use. The last 30 log lines go to stderr |
| 6 | timed out waiting for the app to listen. The last 30 log lines go to stderr |

## App object (`--json`, API)

```json
{
  "name": "myapp",
  "url": "http://myapp.localhost",
  "status": "running",
  "managed": true,
  "port": 4100,
  "port_auto": true,
  "command": "npm run dev -- --port $PORT",
  "cwd": "/home/me/src/myapp",
  "env": {"FOO": "1"},
  "rewrite_host": false,
  "no_preview": false,
  "pid": 12345,
  "exit_code": 1,
  "started_at": "2026-01-01T12:00:00Z",
  "stale": false,
  "log_file": "/home/me/.local/state/localdev/logs/myapp.log",
  "created_at": "...", "updated_at": "..."
}
```

`status` is one of:
- `running`: the port accepts connections.
- `starting`: the process is alive but not listening yet.
- `stopped`: stopped or never started.
- `exited`: the process ended on its own; see `exit_code`.
- `down`: an unmanaged app that isn't listening.

`stale: true` means the definition changed since the process started, so run `restart` to apply it.
`pid` is the process group id.

## HTTP API

Base URL: `http://localhost[:port]`. Get the port from `localdev daemon status --json`, or from
`port` in `$XDG_STATE_HOME/localdev/daemon.json`. The API answers only when the Host header is
`localhost`, `127.0.0.1`, or `[::1]`. **Every non-GET request must send the header
`X-Localdev: <anything>`**, which blocks cross-site requests from browsers.

| Method & path | Body | Response |
|---|---|---|
| `GET /api/health` | | `{service:"localdev", version, pid, port, url, started_at, discover, config_file}` |
| `GET /api/docs` | | this document (markdown) |
| `GET /api/apps` | | `[App]` |
| `GET /api/apps/{name}` | | `App` or 404 |
| `PUT /api/apps/{name}` | `{port?, command?, cwd? (absolute), env?, rewrite_host?, no_preview?}`, at least port or command | `App`, 201 if created. Never starts or restarts anything |
| `DELETE /api/apps/{name}` | | `{ok, name}`, stops the process first |
| `POST /api/apps/{name}/start` | optional `{env: ["K=V", ...]}` (the caller's environment for the process) | `App`. Idempotent; restarts only if stale |
| `POST /api/apps/{name}/restart` | same | `App` |
| `POST /api/apps/{name}/stop` | | `App` |
| `GET /api/apps/{name}/logs?tail=200` | | text/plain |
| `GET /api/discover` | | `[{port, addr, pid, process, command, cwd, http, registered_as}]` (fresh scan) |
| `GET /api/discover?cached=1` | | latest scan from continuous discovery (`daemon run --discover`) |
| `POST /api/shutdown` | | stops all managed apps and exits |

Errors: `{"error": "message", "code": "not_found|bad_request|not_managed|start_failed|forbidden|..."}`.
`start` returns immediately. To wait for readiness, poll `GET /api/apps/{name}` until `status`
is `running`, and treat `exited` or `stopped` as failure.

## Proxy behavior

- `name.localhost` and any subdomain `*.name.localhost` go to the app's port. The proxy connects to
  127.0.0.1 and falls back to ::1.
- The Host header is passed through unchanged (`name.localhost`). `X-Forwarded-Host`,
  `X-Forwarded-Proto`, and `X-Forwarded-For` are set. If a dev server rejects the Host, register
  with `--rewrite-host` to send `localhost:<port>` instead.
- The dashboard shows a live iframe preview of each running app. Register with `--no-preview` to
  turn it off for one app, for example when it is heavy or has side effects on load.
- A managed app that hasn't run since the daemon started (after a reboot, say) is started by
  the first request to its URL. Non-browser requests wait up to 30s for it to listen. Apps that
  were stopped explicitly or exited stay down, so `localdev stop` is respected.
- An unknown app returns 404. An app that's down returns 502 (503 while starting), with an HTML
  page for browsers and a one-line text body otherwise.

## Environment

- `LOCALDEV_PORT`: daemon port for both daemon and CLI. The default is 80, falling back to 7777.
- `LOCALDEV_HOME`: put all state in one directory. The default is `$XDG_CONFIG_HOME/localdev` for
  `apps.json` and `$XDG_STATE_HOME/localdev` for logs and daemon state.
- `LOCALDEV_NO_AUTOSTART=1`: fail with exit 4 instead of starting the daemon.
- `LOCALDEV_DISCOVER=1`: an auto-started daemon runs continuous discovery.
