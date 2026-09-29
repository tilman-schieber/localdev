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
import os, http.server
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        b = self.path.encode()
        self.send_response(200); self.send_header("Content-Length", str(len(b))); self.end_headers(); self.wfile.write(b)
    def log_message(self, *a): pass
http.server.HTTPServer(("127.0.0.1", int(os.environ["PORT"])), H).serve_forever()
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
[ "$("$L" info web --json | json '["status"]')" = stopped ] || fail "web should be stopped"
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
