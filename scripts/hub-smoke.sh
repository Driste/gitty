#!/usr/bin/env bash
#
# Hub smoke test: starts the real `gitty up` — gitty finding and running the
# gitty-up plugin next to it — for a small public group on gitlab.com, tells
# it what a real checkout says with `gitty share`, and then talks to it over
# HTTP the way an agent's host does. Anonymous, so no token or secret is
# required.
#
# The client is this script: requests are sent with curl and the answers
# checked with grep. No MCP tooling is needed, which is the point — the
# protocol the hub speaks is plain enough to drive by hand.
#
# Usage:
#   go build -o gitty . && go build -o gitty-up ./cmd/gitty-up && ./scripts/hub-smoke.sh
#
# Override the target with GITTY_TEST_PARENT / GITTY_TEST_GROUP / GITTY_TEST_REPO.

set -euo pipefail

GITTY="${GITTY:-$PWD/gitty}"
PARENT="${GITTY_TEST_PARENT:-gitlab-examples/wayne-enterprises}"
GROUP="${GITTY_TEST_GROUP:-gitlab-examples/wayne-enterprises/wayne-aerospace}"
REPO="${GITTY_TEST_REPO:-$GROUP/mission-control}"

[ -x "$GITTY" ] || { echo "gitty binary not found at $GITTY (build it first: go build -o gitty .)" >&2; exit 1; }
[ -x "$(dirname "$GITTY")/gitty-up" ] || {
  echo "gitty-up not found next to $GITTY (build it first: go build -o gitty-up ./cmd/gitty-up)" >&2; exit 1; }

WORK="$(mktemp -d)"
HUB_PID=""
cleanup() {
  [ -n "$HUB_PID" ] && kill "$HUB_PID" 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT

fail() { echo "FAIL: $*" >&2; [ -f "$WORK/hub.err" ] && sed 's/^/  hub: /' "$WORK/hub.err" >&2; exit 1; }
ok()   { echo "  ok: $*"; }
has()  { grep -qF -- "$2" <<<"$1" || fail "$3: expected to find '$2' in:
$1"; ok "$3"; }

echo "== a workspace with one real project in it"
mkdir "$WORK/ws" && cd "$WORK/ws"
"$GITTY" init --url=https://gitlab.com --verify=false >/dev/null
"$GITTY" sync "$GROUP" --anon >"$WORK/sync.out" 2>"$WORK/sync.err" || fail "sync: $(cat "$WORK/sync.err")"
has "$(cat "$WORK/sync.out")" "clone $REPO" "the project is checked out"

echo "== gitty up, for the group it lives in"
"$GITTY" up --url=https://gitlab.com --anon --no-auth --group="$PARENT" \
  --listen=127.0.0.1:0 --data="$WORK/data" 2>"$WORK/hub.err" &
HUB_PID=$!
HUB=""
for _ in $(seq 1 120); do
  HUB="$(sed -n 's|.*serving MCP at \(http://[^/ ]*\)/mcp.*|\1|p' "$WORK/hub.err" | head -1)"
  [ -n "$HUB" ] && grep -q " listed " "$WORK/hub.err" && break
  kill -0 "$HUB_PID" 2>/dev/null || fail "gitty up exited"
  sleep 0.5
done
[ -n "$HUB" ] && grep -q " listed " "$WORK/hub.err" || fail "gitty up did not list the instance in time"
ok "listening at $HUB: $(grep ' listed ' "$WORK/hub.err" | sed 's/^[0-9:]* //')"

echo "== gitty share tells it what the checkout says"
OUT="$(GITTY_HUB="$HUB" "$GITTY" share --anon --dry-run 2>"$WORK/share.err")" || fail "share --dry-run: $(cat "$WORK/share.err")"
has "$OUT" "plan share $REPO" "a dry run says what would be sent"
OUT="$(GITTY_HUB="$HUB" "$GITTY" share --anon 2>"$WORK/share.err")" || fail "share: $(cat "$WORK/share.err")"
has "$OUT" "share $REPO" "the checkout was shared"
has "$OUT" "errors=0" "nothing failed"
OUT="$(GITTY_HUB="$HUB" "$GITTY" share --anon 2>/dev/null)"
has "$OUT" "skip $REPO known" "the second time there is nothing new to say"

# call <tool> <json arguments>: one request of the 2026-07-28 revision.
call() {
  curl -sS -X POST "$HUB/mcp" \
    -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
    -H 'MCP-Protocol-Version: 2026-07-28' -H 'Mcp-Method: tools/call' -H "Mcp-Name: $1" \
    -d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"$1\",\"arguments\":$2,\"_meta\":{\"io.modelcontextprotocol/protocolVersion\":\"2026-07-28\",\"io.modelcontextprotocol/clientCapabilities\":{}}}}"
}

echo "== an agent walks the tree"
OUT="$(curl -sS -X POST "$HUB/mcp" -H 'Content-Type: application/json' \
  -H 'MCP-Protocol-Version: 2026-07-28' -H 'Mcp-Method: server/discover' \
  -d '{"jsonrpc":"2.0","id":0,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}')"
has "$OUT" '"supportedVersions":["2026-07-28"' "the server says what it speaks"
has "$(call gitty_tree '{}')" "${GROUP##*/}/" "the tree shows the group"
has "$(call gitty_browse "{\"path\":\"$GROUP\"}")" "${REPO##*/}" "the group lists the project"
OUT="$(call gitty_outline "{\"path\":\"$REPO\"}")"
has "$OUT" "Its repository was read at" "the outline rests on what the repository says"
has "$OUT" "gitty fetch $REPO" "and says how to get it"

echo "== and leaves something for the next one"
OUT="$(call gitty_note "{\"path\":\"$REPO\",\"kind\":\"caveat\",\"text\":\"Recorded by the smoke test.\"}")"
has "$OUT" "Recorded as #1" "a note is recorded"
has "$(call gitty_outline "{\"path\":\"$REPO\"}")" "Recorded by the smoke test." "and is there for the next reader"

echo "== older clients are still answered"
OUT="$(curl -sS -X POST "$HUB/mcp" -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"smoke","version":"0"}}}')"
has "$OUT" '"protocolVersion":"2025-06-18"' "the 2025 handshake"
CODE="$(curl -sS -o /dev/null -w '%{http_code}' "$HUB/mcp")"
[ "$CODE" = "405" ] || fail "GET /mcp answered $CODE, want 405"
ok "no stream, no session"

echo
echo "hub smoke test passed"
