#!/usr/bin/env bash
# ECS smoke for the node runtime (add-coolify-hosting task 2.6; moved with the
# hosting-agent-into-bridge merge: same checks, new `node run` entry).
#
# Runs ON the ECS node. Verifies what can be verified before the API-side
# work endpoints (task 3.2) exist:
#   1. build + unit tests (includes the real git-clone-through-CGI path)
#   2. fail-loud boot: a missing INTERNAL_SECRET must exit 1 immediately
#   3. resilient run: with an unreachable API the agent logs poll failures,
#      stays alive, and exits 0 on SIGTERM
#
# The full chain (R2 materialize -> loopback git http -> Coolify pull) is
# exercised by group 8 e2e once 3.2+ land; this script is the node-local
# pre-flight.
#
# Prereqs: go >= 1.22, git.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
NODE_DIR="$REPO_ROOT/node"
BIN="$(mktemp -d)/open-agents-bridge"
PASS=0
FAIL=0

ok()   { echo "PASS: $*"; PASS=$((PASS + 1)); }
bad()  { echo "FAIL: $*"; FAIL=$((FAIL + 1)); }

echo "== stage 1: build + unit tests =="
(cd "$REPO_ROOT" && go build -o "$BIN" ./cmd/open-agents-bridge && go test ./node/...)
ok "build + unit tests"

echo "== stage 2: missing INTERNAL_SECRET must fail loud =="
if INTERNAL_SECRET= API_BASE_URL=https://api.example.com "$BIN" node run >/dev/null 2>/tmp/hosting-agent-smoke.err \
   && test $? -ne 1; then
  bad "agent started without INTERNAL_SECRET"
else
  grep -q "INTERNAL_SECRET" /tmp/hosting-agent-smoke.err \
    && ok "exited 1 naming INTERNAL_SECRET" \
    || bad "exited nonzero but message did not name INTERNAL_SECRET"
fi

echo "== stage 3: unreachable API -> stay alive, SIGTERM -> exit 0 =="
LOG=/tmp/hosting-agent-smoke.log
: > "$LOG"
CACHE="$(mktemp -d)"
INTERNAL_SECRET=smoke-secret \
API_BASE_URL=http://127.0.0.1:9 \
CACHE_DIR="$CACHE" \
WORK_POLL_INTERVAL_SEC=1 \
WATERMARK_INTERVAL_SEC=1 \
CADDY_ADMIN_ADDR= \
  "$BIN" node run >"$LOG" 2>&1 &
AGENT_PID=$!

sleep 3
if kill -0 "$AGENT_PID" 2>/dev/null; then
  ok "survived 3s of unreachable API (best-effort loops)"
else
  bad "died on unreachable API (loops must be best-effort)"
fi
grep -q "work poll failed" "$LOG" \
  && ok "poll failures are logged, not silent" \
  || bad "no poll-failure log line after 3s"

kill -TERM "$AGENT_PID"
wait "$AGENT_PID" && ok "clean exit on SIGTERM" || bad "nonzero exit on SIGTERM"

echo
echo "smoke summary: $PASS passed, $FAIL failed"
test "$FAIL" -eq 0
