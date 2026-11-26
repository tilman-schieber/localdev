#!/usr/bin/env bash
# End-to-end smoke test: real daemon, real processes. Needs python3 and curl.
# Usage: scripts/smoke.sh [path/to/localdev]
set -euo pipefail

L=$(cd "$(dirname "${1:-./localdev}")" && pwd)/$(basename "${1:-./localdev}")
export LOCALDEV_HOME=$(mktemp -d) LOCALDEV_PORT=7791
P=$LOCALDEV_PORT
WORK=$(mktemp -d)
trap '"$L" daemon stop >/dev/null 2>&1 || true; cat "$LOCALDEV_HOME/daemon.log" 2>/dev/null | tail -20' EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }
step() { echo "--- $*"; }
get() { curl -sS -H "Host: $1.localhost:$P" "http://127.0.0.1:$P$2"; }
json() { python3 -c "import sys,json; print(json.load(sys.stdin)$1)"; }

cat >"$WORK/echo.py" <<'EOF'
import os, socketserver, http.server
class S(http.server.HTTPServer):
    def server_bind(self):  # skip the getfqdn lookup, which is slow on macOS
        socketserver.TCPServer.server_bind(self)
        self.server_name, self.server_port = "localhost", self.server_address[1]
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        b = self.path.encode()
        self.send_response(200); self.send_header("Content-Length", str(len(b))); self.end_headers(); self.wfile.write(b)
    def log_message(self, *a): pass
S(("127.0.0.1", int(os.environ["PORT"])), H).serve_forever()
EOF
cd "$WORK"

step "run prints the URL"
url=$("$L" run web -- "python3 $WORK/echo.py")
[ "$url" = "http://web.localhost:$P" ] || fail "unexpected url $url"

step "proxy forwards requests and query strings verbatim"
got=$(get web '/App.svelte?svelte&type=style&lang.css')
[ "$got" = '/App.svelte?svelte&type=style&lang.css' ] || fail "query mangled: $got"
[ "$(get api.web /x)" = /x ] || fail "subdomain routing"

step "exit codes"
set +e
"$L" url nosuch >/dev/null 2>&1; [ $? = 3 ] || fail "not found should exit 3"
"$L" run crash -- 'exit 7' >/dev/null 2>&1; [ $? = 5 ] || fail "crash should exit 5"
set -e
"$L" rm crash >/dev/null 2>&1

step "discover sees the registered app"
"$L" discover --all --json | json '' | grep -q "'registered_as': 'web'" || fail "discover"

step "start on first visit after daemon restart"
"$L" daemon stop >/dev/null
"$L" daemon start >/dev/null
[ "$("$L" info web --json | json '["status"]')" = idle ] || fail "web should be idle"
[ "$(get web /lazy)" = /lazy ] || fail "request did not start the app"
[ "$("$L" info web --json | json '["status"]')" = running ] || fail "web should be running"

step "explicitly stopped apps stay down"
"$L" stop web >/dev/null 2>&1
code=$(curl -s -o /dev/null -w '%{http_code}' -H "Host: web.localhost:$P" "http://127.0.0.1:$P/")
[ "$code" = 502 ] || fail "stopped app answered $code"

step "orphans of a killed daemon are cleaned up"
"$L" run orphan -- "python3 $WORK/echo.py & wait" >/dev/null
port=$("$L" info orphan --json | json '["port"]')
kill -9 "$("$L" daemon status --json | json '["pid"]')"
sleep 1
curl -s "http://127.0.0.1:$port/" >/dev/null || echo "(child already gone after daemon death)"
"$L" daemon start >/dev/null
sleep 1
if curl -s -m 2 "http://127.0.0.1:$port/" >/dev/null; then fail "orphan still listening on $port"; fi

step "localdev.json: run with no arguments starts the project's apps"
mkdir -p "$WORK/proj/sub"
printf '{"projweb": {"command": "python3 %s/echo.py", "ready_path": "/"}}\n' "$WORK" >"$WORK/proj/localdev.json"
url=$(cd "$WORK/proj/sub" && "$L" run 2>/dev/null)
[ "$url" = "http://projweb.localhost:$P" ] || fail "project run printed '$url'"
[ "$(get projweb /p)" = /p ] || fail "project app not proxied"
[ "$("$L" info projweb --json | json '["cwd"]')" = "$WORK/proj" ] || fail "project cwd"
set +e
(cd "$WORK/proj" && "$L" run --port 9 >/dev/null 2>&1); [ $? = 2 ] || fail "flags with localdev.json should be a usage error"
set -e

step "logs -f streams through the API"
"$L" logs projweb -f -n 5 >"$WORK/follow.out" 2>&1 &  # macOS has no timeout(1)
sleep 2; kill $! 2>/dev/null || true; wait $! 2>/dev/null || true
grep -q "starting" "$WORK/follow.out" || fail "logs -f printed: $(cat "$WORK/follow.out")"

step "logs are capped while the app runs"
"$L" run noisy --no-wait -- "python3 -c 'import sys
for i in range(160000): sys.stdout.write(\"x\" * 99 + \"\\n\")
sys.stdout.flush()
import time; time.sleep(60)'" >/dev/null
sleep 5
log=$("$L" info noisy --json | json '["log_file"]')
size=$(wc -c <"$log")
[ "$size" -lt $((10 * 1024 * 1024)) ] || fail "log not capped ($size bytes)"
[ -s "$log.1" ] || fail "rotated log missing"

echo "OK"
