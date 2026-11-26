# localdev

A small local development service registry and reverse proxy. One daemon gives each local web app
a stable hostname and forwards requests to the app's real port:

- `http://localhost`: dashboard of registered apps, with start/stop/restart, live logs, and discovery
- `http://myapp.localhost`: proxied to the app's port, including WebSockets/HMR and SSE/streaming
- a CLI and a JSON API built for scripts and coding agents

It works the same for npm, bun, Python, Rust, Go, or anything else that listens on a port.
It is a single static binary with no dependencies beyond the Go standard library.

## Install

Prebuilt binary (Linux and macOS, x86_64 and arm64):

```sh
mkdir -p ~/.local/bin && curl -fsSL \
  "https://github.com/tilman-schieber/localdev/releases/latest/download/localdev_$(uname -s)_$(uname -m).tar.gz" \
  | tar -xz -C ~/.local/bin localdev
```

Or with Go 1.22+:

```sh
go install github.com/tilman-schieber/localdev@latest
```

Linux and macOS only. Windows isn't supported.

`*.localhost` resolves to loopback in Chrome, Firefox, Safari, and curl, and on Linux with
systemd-resolved, with no `/etc/hosts` edits.

### Install with a coding agent

Paste this into Claude Code or another coding agent:

> Install localdev from https://github.com/tilman-schieber/localdev: put the release binary for my
> OS and architecture on my PATH, following the README's install section. Then save the repo's `skills/localdev/SKILL.md` as
> `~/.claude/skills/localdev/SKILL.md`. For other agents, add it to their instructions file instead.
> Verify with `localdev version`.

### Port 80

For clean URLs without a port, the daemon listens on port 80. If it isn't allowed to bind port 80,
it falls back to **7777** and every URL carries `:7777`. The CLI always prints the correct URL.
To allow port 80 on Linux, pick one:

```sh
# allow unprivileged ports from 80 upward (persists across reboots)
echo 'net.ipv4.ip_unprivileged_port_start=80' | sudo tee /etc/sysctl.d/50-localdev.conf
sudo sysctl --system

# or grant only this binary (redo after every rebuild)
sudo setcap cap_net_bind_service=+ep ~/.local/bin/localdev
```

To pin a port instead, set `LOCALDEV_PORT=7777` in your shell profile.

## Usage

```sh
cd ~/src/myapp

# Managed app: localdev starts the command with $PORT set, waits until it listens, prints the URL.
localdev run myapp -- 'npm run dev -- --port $PORT'
# -> http://myapp.localhost

# Unmanaged app: you run the server, localdev only proxies to it.
localdev register api --port 8000

localdev list
localdev logs myapp -f
localdev open myapp
localdev stop myapp
localdev unregister api
```

- Omit the name and localdev uses the app registered for the current directory, or else the
  directory's name.
- `run` is idempotent. It restarts the app only when the definition (command, cwd, env) changed.
- A command that ignores `$PORT` still works in most cases: localdev finds the port the process
  group actually listens on and proxies there.
- The process inherits the environment of the CLI call that started it, so PATH and version
  managers work as expected. It runs in its own process group, and stopping it signals the whole
  tree.
- Stopping the daemon (`localdev daemon stop`) stops all managed apps. Definitions persist in
  `~/.config/localdev/apps.json`.
- Apps start on first visit. After a reboot or daemon restart, the first request to a managed
  app's URL starts it: browsers see a "starting…" page that reloads itself, and other clients wait
  up to 30s. These apps show as `idle`. Apps you stopped explicitly, and apps that crashed, stay
  down until started again.
- Idle apps stop themselves if you want: `--idle-stop 2h` stops an app after two hours without
  requests, and its next request starts it again. An open tab with a live WebSocket (HMR) counts
  as use. Set a default for all apps with `LOCALDEV_IDLE_STOP=2h` or `daemon run --idle-stop 2h`,
  and opt one app out with `--idle-stop off`.
- `run` waits until the port accepts connections. Some dev servers do that before they can serve
  a page; `--ready-path /` makes localdev wait for a real HTTP response (any status below 500).
- If the daemon is killed without stopping its apps (for example with SIGKILL), the next daemon
  stops the leftover process groups before it starts.
- Each app's log is capped at 10 MB while it runs. Older output moves to `<name>.log.1`, which
  also holds the previous run's output after a restart.

### Project file

Commit a `localdev.json` to a repo, and `localdev run` with no arguments starts its apps:

```json
{
  "web": { "command": "npm run dev -- --port $PORT", "ready_path": "/" },
  "api": { "command": "go run .", "cwd": "api", "env": { "DEBUG": "1" }, "idle_stop": "1h" }
}
```

- Each key is an app name. The fields match the API: `command`, `port`, `cwd` (relative to the
  file), `env`, `rewrite_host`, `idle_stop`, `ready_path`. Unknown fields are an error.
- localdev looks for the file in the current directory and its parents.
- `localdev run web` starts just that app. A command after `--` ignores the file.

### Discovery (optional)

```sh
localdev discover          # one-shot: unregistered HTTP servers listening on localhost
localdev discover --all    # include non-HTTP and registered listeners
```

For continuous discovery, run the daemon with `localdev daemon run --discover`, or set
`LOCALDEV_DISCOVER=1` before the daemon auto-starts. The dashboard then lists unregistered
servers, and you can register them with one click. It is off by default.

### Running the daemon

Any command starts the daemon in the background on demand. Its log is
`~/.local/state/localdev/daemon.log`. To run it under systemd instead:

```ini
# ~/.config/systemd/user/localdev.service
[Unit]
Description=localdev

[Service]
ExecStart=%h/.local/bin/localdev daemon run
Restart=on-failure

[Install]
WantedBy=default.target
```

```sh
systemctl --user enable --now localdev
```

## For coding agents

- [`docs/agents.md`](docs/agents.md) is the machine-oriented reference: workflow, JSON shapes,
  exit codes, and the HTTP API. It is also built into the binary: `localdev agent-help`, or
  `GET http://localhost/api/docs`.
- [`skills/localdev/SKILL.md`](skills/localdev/SKILL.md) is a drop-in agent skill. For Claude Code:
  `cp -r skills/localdev ~/.claude/skills/`. For other agents, paste its body into `AGENTS.md`.

## Security

The daemon listens only on 127.0.0.1 and ::1. The API accepts only `localhost`/`127.0.0.1` Host
headers, which blocks DNS rebinding. State-changing requests need an `X-Localdev` header, which
browsers can't send cross-origin without a CORS preflight, and localdev never grants one. So web
pages can't register commands. Any local process running as your user can, just as it could run
those commands directly.

## License

MIT, see [LICENSE](LICENSE).

## Layout

| file | contents |
|---|---|
| `main.go` | CLI |
| `client.go` | API client, daemon auto-start |
| `daemon.go` | daemon lifecycle, HTTP API |
| `proxy.go` | host-based reverse proxy, error pages |
| `process.go` | managed processes, port allocation |
| `discover*.go` | listener discovery (`/proc` on Linux, `lsof` on macOS) |
| `registry.go` | persistent app definitions, paths |
| `dashboard.html` | dashboard, embedded into the binary |
