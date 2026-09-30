---
name: localdev
description: Start, register, and reach local development servers (npm, bun, vite, next, django, rails, cargo, go, etc.) at stable http://<name>.localhost URLs using the localdev CLI. Use when you need to run a dev server, get the URL of a running local app, check its logs, or stop it, instead of running the server in the foreground or guessing ports.
---

# localdev

`localdev` runs dev servers in the background under one daemon and gives each one a stable URL,
`http://<name>.localhost`, proxied to its real port (WebSockets/HMR work). Use it instead of
starting servers in the foreground, backgrounding them with `&`, or guessing `localhost:3000`.

## Start a dev server and get its URL

From the project directory:

```sh
localdev run <name> -- '<dev command using $PORT>'
```

The command blocks until the server accepts connections, then prints **only the URL** on stdout.
Examples:

```sh
localdev run web -- 'npm run dev -- --port $PORT'        # vite / next / astro
localdev run api -- 'uv run uvicorn app:app --port $PORT'
localdev run site -- 'bun run dev --port $PORT'
localdev run svc -- 'PORT=$PORT cargo run'
localdev run docs -- 'python3 -m http.server $PORT'
```

- Always single-quote the command so `$PORT` reaches localdev unexpanded.
- `<name>` must be lowercase letters, digits, and dashes. If you omit it, localdev uses the app
  registered for the current directory, or else the directory's name.
- `run` is idempotent, so re-running it is safe: it restarts only when the command, cwd, or env
  changed.
- If a server ignores `$PORT`, localdev usually finds the port it opened anyway.
- **Astro:** `astro dev` detects coding agents and backgrounds itself, so the command exits at once
  and localdev reports a failed start. Set `ASTRO_DEV_BACKGROUND=1` to keep it in the foreground:
  `localdev run site -- 'ASTRO_DEV_BACKGROUND=1 npx astro dev --port $PORT'`. If Astro/Vite
  rejects the `*.localhost` Host, add `--allowed-hosts <name>.localhost` or pass `--rewrite-host`.
- Capture the URL with `URL=$(localdev run web -- '...')`, or use `--json` for the full app object.

## If you already started the server yourself

```sh
localdev register <name> --port 5173     # proxy only; prints the URL
```

## Everyday commands

```sh
localdev url [name]            # print the URL (never hardcode it; the daemon port may not be 80)
localdev list --json           # all apps with status: running|starting|stopped|exited|down
localdev logs [name] -n 80     # server output (stdout+stderr of the current run)
localdev restart [name]        # force a restart
localdev stop [name]           # stop when you're done; the definition stays registered
localdev discover              # find unregistered servers on localhost
```

## Failure handling

Exit codes:
- 0: ok
- 2: bad arguments or name
- 3: no such app
- 4: daemon unavailable
- 5: the server exited during startup, or its fixed port is taken
- 6: timed out waiting for the port (default 60s; change it with `--timeout 3m`)

On 5 and 6, the last 30 log lines are already on stderr. Read them, fix the problem, and run
`localdev run` again. For slow first builds, pass `--timeout`. To return right away, pass
`--no-wait` and use `localdev wait <name>` later.

Full reference, including the HTTP API: `localdev agent-help`.
