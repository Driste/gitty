#!/usr/bin/env bash
#
# MCP smoke test: starts the real `gitty mcp` — gitty finding and running the
# gitty-mcp plugin next to it — in a workspace holding a real project from
# gitlab.com, and talks to it over stdin and stdout the way an agent host
# does. Anonymous, so no token or secret is required.
#
# The client is this script: requests are piped in, one JSON message per line,
# and the answers are checked with grep. No MCP tooling is needed, which is
# the point — the protocol the server speaks is plain enough to drive by hand.
#
# Usage:
#   go build -o gitty . && go build -o gitty-mcp ./cmd/gitty-mcp && ./scripts/mcp-smoke.sh
#
# Override the project with GITTY_TEST_REPO.

set -euo pipefail

GITTY="${GITTY:-$PWD/gitty}"
REPO="${GITTY_TEST_REPO:-gitlab-examples/wayne-enterprises/wayne-aerospace/mission-control}"

[ -x "$GITTY" ] || { echo "gitty binary not found at $GITTY (build it first: go build -o gitty .)" >&2; exit 1; }
[ -x "$(dirname "$GITTY")/gitty-mcp" ] || {
  echo "gitty-mcp not found next to $GITTY (build it first: go build -o gitty-mcp ./cmd/gitty-mcp)" >&2; exit 1; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }
ok()   { echo "  ok: $*"; }

# answer <id> : the response line with that id.
answer() { grep -F "\"id\":$1," "$WORK/answers" || fail "no answer with id $1"$'\n'"$(cat "$WORK/answers")"; }

# expect <id> <what> <text> : the answer with that id contains the text.
expect() {
  case "$(answer "$1")" in
    *"$3"*) ok "$2" ;;
    *) fail "$2: expected to find: $3"$'\n'"in: $(answer "$1")" ;;
  esac
}

echo "== a workspace holding $REPO"
mkdir "$WORK/ws" && cd "$WORK/ws"
"$GITTY" init > /dev/null 2>&1 || fail "gitty init"
"$GITTY" fetch "$REPO" --anon > "$WORK/fetch.out" 2> "$WORK/fetch.err" \
  || fail "gitty fetch $REPO"$'\n'"$(cat "$WORK/fetch.out" "$WORK/fetch.err")"
grep -q "^clone $REPO\$" "$WORK/fetch.out" || fail "fetch did not clone $REPO: $(cat "$WORK/fetch.out")"
[ -d "$REPO/.git" ] || fail "$REPO is not where its namespace path puts it"
ok "fetched into its namespace path"

echo "== gitty mcp, from inside the checkout"
cd "$REPO"
code=0
"$GITTY" mcp --anon > "$WORK/answers" 2> "$WORK/server.err" <<JSON || code=$?
{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"mcp-smoke","version":"0"}}}
{"jsonrpc":"2.0","method":"notifications/initialized"}
{"jsonrpc":"2.0","id":2,"method":"tools/list"}
{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"gitty_context","arguments":{}}}
{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"gitty_search","arguments":{"pattern":".","max":1}}}
{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"gitty_read","arguments":{"project":"$REPO","path":"../outside"}}}
{"jsonrpc":"2.0","id":6,"method":"no/such/method"}
JSON
[ "$code" = 0 ] || fail "gitty mcp exited $code"$'\n'"$(cat "$WORK/server.err")"
ok "served until its input ended (exit 0)"

echo "== stdout is protocol and nothing else"
if grep -qv '^{"jsonrpc":"2.0",' "$WORK/answers"; then
  fail "stdout carried a line that is not a JSON-RPC message:"$'\n'"$(grep -v '^{"jsonrpc":"2.0",' "$WORK/answers")"
fi
[ "$(wc -l < "$WORK/answers")" = 6 ] || fail "expected 6 answers (the notification gets none), got $(wc -l < "$WORK/answers")"
ok "six requests, six one-line answers"

echo "== the answers"
expect 1 "handshake names the server"            '"serverInfo":{"name":"gitty-mcp"'
expect 1 "handshake agrees on the protocol"      '"protocolVersion":"2025-06-18"'
expect 2 "tool list offers gitty_context"        '"name":"gitty_context"'
expect 2 "tool list offers gitty_read"           '"name":"gitty_read"'
expect 3 "context is about the project"          "\"subject\":{\"kind\":\"project\",\"project\":\"$REPO\""
expect 3 "context gives its path on this machine" "\"path\":\"$WORK/ws/$REPO\""
expect 3 "context says what it rests on"         '"coverage":{"projects":'
expect 4 "search finds something in the checkout" "\"matches\":[{\"project\":\"$REPO\""
expect 5 "reading outside a checkout is refused" '"isError":true'
expect 6 "an unknown method is a protocol error" '"error":{"code":-32601'

echo "== gitty kept its own account on stderr"
grep -q "serving workspace $WORK/ws" "$WORK/server.err" || fail "stderr: $(cat "$WORK/server.err")"
ok "diagnostics on stderr"

echo
echo "mcp smoke test passed"
