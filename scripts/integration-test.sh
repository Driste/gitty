#!/usr/bin/env bash
#
# Integration smoke test: drives a real gitty binary against a small public
# group on gitlab.com, anonymously, so no token or secret is required.
#
# This is the counterpart to the hermetic e2e suite in e2e_test.go: those tests
# prove the logic against a fake GitLab, this one proves gitty still agrees
# with the real API and a real git transport.
#
# Usage:
#   go build -o gitty . && ./scripts/integration-test.sh
#
# Override the target with GITTY_TEST_GROUP / GITTY_TEST_PARENT / GITTY_TEST_REPO.

set -euo pipefail

GITTY="${GITTY:-$PWD/gitty}"
PARENT="${GITTY_TEST_PARENT:-gitlab-examples/wayne-enterprises}"
GROUP="${GITTY_TEST_GROUP:-gitlab-examples/wayne-enterprises/wayne-aerospace}"
REPO="${GITTY_TEST_REPO:-$GROUP/mission-control}"

if [ ! -x "$GITTY" ]; then
  echo "gitty binary not found at $GITTY (build it first: go build -o gitty .)" >&2
  exit 1
fi

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
cd "$WORK"

fail() { echo "FAIL: $*" >&2; exit 1; }
ok()   { echo "  ok: $*"; }

# run <expected-exit> <label> -- <args...>  : runs gitty, captures streams.
# Sets OUT and ERR to the captured stdout/stderr.
run() {
  local want="$1" label="$2"; shift 3
  local code=0
  "$GITTY" "$@" >"$WORK/out" 2>"$WORK/err" || code=$?
  OUT="$(cat "$WORK/out")"; ERR="$(cat "$WORK/err")"
  if [ "$code" != "$want" ]; then
    echo "FAIL: $label: exit $code, want $want" >&2
    echo "--- stdout ---" >&2; echo "$OUT" >&2
    echo "--- stderr ---" >&2; echo "$ERR" >&2
    exit 1
  fi
  ok "$label (exit $code)"
}

contains() {
  case "$2" in
    *"$1"*) ok "found: $1" ;;
    *) fail "expected to find: $1"$'\n'"got:"$'\n'"$2" ;;
  esac
}

echo "== target: $GROUP (repo: $REPO)"

echo "== version"
run 0 "gitty version" -- version
[ -n "$OUT" ] || fail "version printed nothing"

echo "== init"
run 0 "gitty init --http" -- init --http
[ -f .gitty/config ] || fail ".gitty/config was not created"

echo "== ls (remote inventory, no git, no clone)"
run 0 "gitty ls --nested" -- ls --path="$PARENT" --nested --anon
contains "project $REPO new" "$OUT"
contains "summary groups=" "$OUT"
[ ! -d "$PARENT" ] || fail "ls must not create anything in the workspace"

echo "== ls --format=json parses"
run 0 "gitty ls --format=json" -- ls --path="$PARENT" --nested --anon --format=json
echo "$OUT" | python3 -c 'import json,sys; d=json.load(sys.stdin); assert d["summary"]["projects"] > 0, d' \
  || fail "ls --format=json did not produce a usable report"
ok "json report parsed"

echo "== dry run"
run 0 "gitty sync --dry-run" -- sync --path="$GROUP" --anon --dry-run
contains "plan clone $REPO" "$OUT"
DRY="$OUT"
[ ! -d "$GROUP" ] || fail "dry run created directories"

echo "== real sync (clone)"
run 0 "gitty sync" -- sync --path="$GROUP" --anon
contains "clone $REPO" "$OUT"
contains "summary cloned=1 pulled=0 skipped=0 errors=0" "$OUT"
REAL="$OUT"
[ -d "$REPO/.git" ] || fail "$REPO was not cloned"

echo "== dry-run parity (constitution: plan output matches real actions)"
if [ "$(printf '%s\n' "$DRY" | sed 's/^plan //')" != "$REAL" ]; then
  echo "FAIL: dry-run output does not match the real run" >&2
  diff <(printf '%s\n' "$DRY" | sed 's/^plan //') <(printf '%s\n' "$REAL") >&2 || true
  exit 1
fi
ok "dry run matches the real run"

echo "== greppable stdout (every line is a stable event)"
printf '%s\n' "$REAL" | grep -qvE '^(clone|pull|group|project|reclone|skip|status|error|plan|summary) ' \
  && fail "stdout contained a non-event line" || ok "all stdout lines are events"

echo "== re-sync (fast-forward pull, idempotent)"
run 0 "gitty sync (again)" -- sync --path="$GROUP" --anon
contains "pull $REPO" "$OUT"
contains "summary cloned=0 pulled=1 skipped=0 errors=0" "$OUT"

echo "== status"
run 0 "gitty status" -- status
contains "status $REPO branch=" "$OUT"
contains "dirty=false" "$OUT"
contains "summary repos=1" "$OUT"

echo "== ls now reports the repo as present"
run 0 "gitty ls (after sync)" -- ls --path="$GROUP" --anon
contains "project $REPO present" "$OUT"

echo "== exit codes"
run 1 "unknown group exits 1" -- sync --path="$PARENT/definitely-not-a-real-group" --anon
run 2 "missing token exits 2" -- sync --path="$GROUP"

echo
echo "integration smoke test passed"
