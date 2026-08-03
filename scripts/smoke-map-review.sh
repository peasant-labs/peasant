#!/usr/bin/env bash
# smoke-map-review.sh — production-path smoke harness for the Map/Review REST
# surfaces documented in web/DESIGN_SYSTEM.md.
#
# Usage:
#   scripts/smoke-map-review.sh             non-strict: unwired routes print SKIP
#   scripts/smoke-map-review.sh --strict    integration gate: every check must PASS
#   scripts/smoke-map-review.sh --no-build  skip the build, use existing bin/peasant
#
# Pass 1 (port 9971, --mock-data-store=web,map,review): full jq shape assertions
# against the five §3 endpoints, using a projectHash discovered from
# GET /api/v1/sessions (sessions[].projectHash; falls back to a dummy hash that
# the mock provider echoes back).
#
# Pass 2 (port 9972, --mock-data-store=none): each endpoint must respond 2xx
# JSON (possibly empty data) OR an honest JSON 404 (unknown project) — never 5xx.
#
# Degraded pre-integration behaviour (non-strict only):
#   - mux-level 404 (plain-text body) => SKIP "route not wired"
#   - 503 nil-provider              => SKIP "provider not wired"
#   - build failure                 => fall back to `make go`, then stale binary
#   - stale binary rejecting map/review mock sections => restart without mocks
# --strict escalates every SKIP to FAIL.

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="$REPO_ROOT/bin/peasant"
PORT_MOCK=9971
PORT_REAL=9972
BASE_MOCK="http://localhost:$PORT_MOCK/api/v1"
BASE_REAL="http://localhost:$PORT_REAL/api/v1"
HEALTH_TRIES=40 # x 0.5s = 20s per server
# Review endpoints run one git diff per branch against the real repo (~15s on
# branch-heavy repos), so the per-request budget is generous.
CURL_MAX_TIME="${SMOKE_CURL_MAX_TIME:-45}"
# Syntactically valid 64-hex hash, unknown to any real store.
DUMMY_HASH="deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"

STRICT=0
DO_BUILD=1
for arg in "$@"; do
  case "$arg" in
    --strict) STRICT=1 ;;
    --no-build) DO_BUILD=0 ;;
    -h | --help)
      sed -n '2,24p' "$0" | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    *)
      echo "unknown flag: $arg (see --help)" >&2
      exit 2
      ;;
  esac
done

command -v curl >/dev/null 2>&1 || { echo "FATAL curl is required" >&2; exit 1; }
command -v jq >/dev/null 2>&1 || { echo "FATAL jq is required" >&2; exit 1; }

PASS=0
FAIL=0
SKIP=0
pass() {
  PASS=$((PASS + 1))
  echo "PASS  $*"
}
fail() {
  FAIL=$((FAIL + 1))
  echo "FAIL  $*"
}
skip() {
  if [ "$STRICT" -eq 1 ]; then
    fail "$* [strict: SKIP escalated]"
  else
    SKIP=$((SKIP + 1))
    echo "SKIP  $*"
  fi
}
fatal() {
  echo "FATAL $*" >&2
  exit 1
}

LOG_DIR="$(mktemp -d -t peasant-smoke.XXXXXX)"
PID_MOCK=""
PID_REAL=""

cleanup() {
  if [ -x "$BIN" ]; then
    "$BIN" web stop --port "$PORT_MOCK" >/dev/null 2>&1 || true
    "$BIN" web stop --port "$PORT_REAL" >/dev/null 2>&1 || true
  fi
  [ -n "$PID_MOCK" ] && kill "$PID_MOCK" >/dev/null 2>&1
  [ -n "$PID_REAL" ] && kill "$PID_REAL" >/dev/null 2>&1
  wait >/dev/null 2>&1
  return 0
}
trap cleanup EXIT

# ---------------------------------------------------------------------------
# HTTP helpers
# ---------------------------------------------------------------------------

HTTP_CODE=""
HTTP_BODY=""
# fetch <url> [curl extras...] — populates HTTP_CODE / HTTP_BODY (000 = no conn).
fetch() {
  local url=$1
  shift
  local out
  if ! out="$(curl -sS --max-time "$CURL_MAX_TIME" -w $'\n%{http_code}' "$@" "$url" 2>/dev/null)"; then
    out=$'\n000'
  fi
  HTTP_CODE="${out##*$'\n'}"
  HTTP_BODY="${out%$'\n'*}"
}

is_json() {
  jq -e . >/dev/null 2>&1 <<<"$1"
}

wait_healthy() { # <port> <pid>
  local port=$1 pid=$2 i
  for i in $(seq 1 "$HEALTH_TRIES"); do
    if curl -sf --max-time 2 "http://localhost:$port/api/v1/health" >/dev/null 2>&1; then
      return 0
    fi
    if ! kill -0 "$pid" 2>/dev/null; then
      return 1 # server process died (e.g. flag validation error)
    fi
    sleep 0.5
    : "$i"
  done
  return 1
}

# fetch_route <name> <url> [curl extras...] — strict-shape pass (pass 1).
# Sets ROUTE_OK=1 only on 2xx JSON; otherwise prints SKIP/FAIL itself.
ROUTE_OK=0
fetch_route() {
  local name=$1 url=$2
  shift 2
  ROUTE_OK=0
  fetch "$url" "$@"
  case "$HTTP_CODE" in
    2??)
      if is_json "$HTTP_BODY"; then
        ROUTE_OK=1
      else
        fail "$name: HTTP $HTTP_CODE but body is not JSON"
      fi
      ;;
    404)
      if is_json "$HTTP_BODY"; then
        skip "$name: honest 404 (project unknown to provider)"
      else
        skip "$name: route not wired (mux 404)"
      fi
      ;;
    503) skip "$name: 503 (provider not wired)" ;;
    000) fail "$name: no response (connection error)" ;;
    5??) fail "$name: HTTP $HTTP_CODE (server error)" ;;
    *) fail "$name: unexpected HTTP $HTTP_CODE" ;;
  esac
}

# assert_json <name> <description> <jq-expr> — asserts on the last HTTP_BODY.
assert_json() {
  local name=$1 desc=$2 expr=$3
  if jq -e "$expr" >/dev/null 2>&1 <<<"$HTTP_BODY"; then
    pass "$name: $desc"
  else
    fail "$name: $desc — jq failed: $expr"
  fi
}

# check_lenient <name> <url> [curl extras...] — liveness pass (pass 2):
# 2xx JSON or honest JSON 404 both PASS; mux 404 / 503 SKIP; 5xx always FAIL.
check_lenient() {
  local name=$1 url=$2
  shift 2
  fetch "$url" "$@"
  case "$HTTP_CODE" in
    2??)
      if is_json "$HTTP_BODY"; then
        pass "$name: HTTP $HTTP_CODE (JSON body)"
      else
        fail "$name: HTTP $HTTP_CODE but body is not JSON"
      fi
      ;;
    404)
      if is_json "$HTTP_BODY"; then
        pass "$name: honest JSON 404 (unknown project — acceptable)"
      else
        skip "$name: route not wired (mux 404)"
      fi
      ;;
    503) skip "$name: 503 (real provider not wired)" ;;
    000) fail "$name: no response (connection error)" ;;
    5??) fail "$name: HTTP $HTTP_CODE (server error — never acceptable)" ;;
    *) fail "$name: unexpected HTTP $HTTP_CODE" ;;
  esac
}

# discover_project_hash — reads HTTP_BODY of a /sessions response; first
# session carrying a non-empty projectHash wins (sessions[0] when populated).
discover_project_hash() {
  jq -r '[.sessions[]? | .projectHash? // empty | select(. != "")][0] // empty' <<<"$HTTP_BODY"
}

# ---------------------------------------------------------------------------
# Build
# ---------------------------------------------------------------------------

echo "== build =="
if [ "$DO_BUILD" -eq 1 ]; then
  if make -C "$REPO_ROOT" build >"$LOG_DIR/build.log" 2>&1; then
    pass "make build"
  elif [ "$STRICT" -eq 1 ]; then
    tail -20 "$LOG_DIR/build.log" >&2
    fatal "make build failed (log: $LOG_DIR/build.log)"
  elif make -C "$REPO_ROOT" go >"$LOG_DIR/build-go.log" 2>&1; then
    skip "make build failed; built Go binary via 'make go' (log: $LOG_DIR/build.log)"
  elif [ -x "$BIN" ]; then
    skip "build failed; falling back to existing $BIN (logs: $LOG_DIR)"
  else
    tail -20 "$LOG_DIR/build.log" >&2
    fatal "build failed and no existing binary at $BIN"
  fi
else
  echo "      (build skipped via --no-build)"
fi
[ -x "$BIN" ] || fatal "missing binary: $BIN (run make build)"

# ---------------------------------------------------------------------------
# Pass 1 — mock server: full shape assertions
# ---------------------------------------------------------------------------

echo
echo "== pass 1: mock data (port $PORT_MOCK, --mock-data-store=web,map,review) =="
"$BIN" web stop --port "$PORT_MOCK" >/dev/null 2>&1 || true
"$BIN" web start --port "$PORT_MOCK" --foreground --no-browser \
  --mock-data-store=web,map,review >"$LOG_DIR/server-mock.log" 2>&1 &
PID_MOCK=$!
sleep 1
if ! wait_healthy "$PORT_MOCK" "$PID_MOCK"; then
  if [ "$STRICT" -eq 1 ]; then
    tail -10 "$LOG_DIR/server-mock.log" >&2
    fatal "mock server failed to start (log: $LOG_DIR/server-mock.log)"
  fi
  # Pre-integration binaries reject the map/review mock sections; degrade to a
  # plain start so health/sessions can still be verified.
  skip "server with map,review mock sections failed to start — retrying without mocks (log: $LOG_DIR/server-mock.log)"
  kill "$PID_MOCK" >/dev/null 2>&1
  "$BIN" web stop --port "$PORT_MOCK" >/dev/null 2>&1 || true
  "$BIN" web start --port "$PORT_MOCK" --foreground --no-browser \
    >"$LOG_DIR/server-mock-fallback.log" 2>&1 &
  PID_MOCK=$!
  sleep 1
  if ! wait_healthy "$PORT_MOCK" "$PID_MOCK"; then
    tail -10 "$LOG_DIR/server-mock-fallback.log" >&2
    fatal "server failed to start on port $PORT_MOCK"
  fi
fi

# Health + sessions: must PASS even pre-integration.
fetch "$BASE_MOCK/health"
assert_json "health" "status == ok" '.status == "ok"'
fetch "$BASE_MOCK/sessions"
assert_json "sessions" "sessions is an array" '.sessions | type == "array"'

PROJECT_HASH="$(discover_project_hash)"
if [ -n "$PROJECT_HASH" ]; then
  pass "projectHash discovery: $PROJECT_HASH"
else
  # The mock provider echoes any hash, so a dummy still
  # exercises the mocked endpoints.
  skip "projectHash discovery: no session carries projectHash — using dummy hash"
  PROJECT_HASH="$DUMMY_HASH"
fi

# 1. Map graph
NODE_ID=""
fetch_route "map-graph" "$BASE_MOCK/map/$PROJECT_HASH"
if [ "$ROUTE_OK" -eq 1 ]; then
  assert_json "map-graph" "nodes non-empty" '.nodes | length > 0'
  assert_json "map-graph" "every node has layer>=0 and order>=0" \
    '(.nodes // []) | all(.layer >= 0 and .order >= 0)'
  assert_json "map-graph" "parsedLanguages is an array" '.parsedLanguages | type == "array"'
  assert_json "map-graph" "violations is an array" '.violations | type == "array"'
  NODE_ID="$(jq -r '.nodes[0].id // empty' <<<"$HTTP_BODY")"
fi

# 2. Node detail (first node id from the graph)
if [ -n "$NODE_ID" ]; then
  fetch_route "map-node" "$BASE_MOCK/map/$PROJECT_HASH/node" -G --data-urlencode "path=$NODE_ID"
  if [ "$ROUTE_OK" -eq 1 ]; then
    assert_json "map-node" "shapedBy is an array" '.shapedBy | type == "array"'
    assert_json "map-node" "recordedFiles <= totalFiles" '.recordedFiles <= .totalFiles'
  fi
else
  skip "map-node: no node id available from graph"
fi

# 3. Tasks
fetch_route "map-tasks" "$BASE_MOCK/map/$PROJECT_HASH/tasks"
if [ "$ROUTE_OK" -eq 1 ]; then
  assert_json "map-tasks" "tasks is an array" '.tasks | type == "array"'
  assert_json "map-tasks" "each task has sessionId+entryIndex+title" \
    '(.tasks // []) | all((.sessionId | type == "string") and (.entryIndex | type == "number") and (.title | type == "string"))'
fi

# 4. Review list
BRANCH=""
fetch_route "review-list" "$BASE_MOCK/review/$PROJECT_HASH"
if [ "$ROUTE_OK" -eq 1 ]; then
  assert_json "review-list" "changes is an array" '.changes | type == "array"'
  assert_json "review-list" "defaultBranch is a string" '.defaultBranch | type == "string"'
  BRANCH="$(jq -r '.changes[0].branch // empty' <<<"$HTTP_BODY")"
fi

# 5. Change detail (first change's branch)
if [ -n "$BRANCH" ]; then
  fetch_route "review-change" "$BASE_MOCK/review/$PROJECT_HASH/change" -G --data-urlencode "branch=$BRANCH"
  if [ "$ROUTE_OK" -eq 1 ]; then
    assert_json "review-change" "slice.nodes is an array" '.slice.nodes | type == "array"'
    assert_json "review-change" "work is an array" '.work | type == "array"'
    assert_json "review-change" "caption facts newEdges+violations present" \
      '(.newEdges | type == "array") and (.violations | type == "array")'
  fi
else
  skip "review-change: no branch available from review list"
fi

"$BIN" web stop --port "$PORT_MOCK" >/dev/null 2>&1 || true
kill "$PID_MOCK" >/dev/null 2>&1
wait "$PID_MOCK" 2>/dev/null
PID_MOCK=""

# ---------------------------------------------------------------------------
# Pass 2 — real data: endpoints must respond, never 5xx
# ---------------------------------------------------------------------------

echo
echo "== pass 2: real data (port $PORT_REAL, --mock-data-store=none) =="
"$BIN" web stop --port "$PORT_REAL" >/dev/null 2>&1 || true
"$BIN" web start --port "$PORT_REAL" --foreground --no-browser \
  --mock-data-store=none >"$LOG_DIR/server-real.log" 2>&1 &
PID_REAL=$!
sleep 1
if ! wait_healthy "$PORT_REAL" "$PID_REAL"; then
  tail -10 "$LOG_DIR/server-real.log" >&2
  fatal "real-data server failed to start on port $PORT_REAL (log: $LOG_DIR/server-real.log)"
fi

fetch "$BASE_REAL/health"
assert_json "health(real)" "status == ok" '.status == "ok"'
fetch "$BASE_REAL/sessions"
assert_json "sessions(real)" "sessions is an array" '.sessions | type == "array"'

REAL_HASH="$(discover_project_hash)"
if [ -n "$REAL_HASH" ]; then
  pass "projectHash discovery(real): $REAL_HASH"
else
  # Unknown hash => an honest JSON 404 is the acceptable answer per contract.
  skip "projectHash discovery(real): none found — using dummy hash (honest 404 expected)"
  REAL_HASH="$DUMMY_HASH"
fi

check_lenient "map-graph(real)" "$BASE_REAL/map/$REAL_HASH"
REAL_NODE_ID="$(jq -r '.nodes[0].id // empty' <<<"$HTTP_BODY" 2>/dev/null)"
check_lenient "map-node(real)" "$BASE_REAL/map/$REAL_HASH/node" -G --data-urlencode "path=${REAL_NODE_ID:-internal}"
check_lenient "map-tasks(real)" "$BASE_REAL/map/$REAL_HASH/tasks"
check_lenient "review-list(real)" "$BASE_REAL/review/$REAL_HASH"
REAL_BRANCH="$(jq -r '.changes[0].branch // empty' <<<"$HTTP_BODY" 2>/dev/null)"
check_lenient "review-change(real)" "$BASE_REAL/review/$REAL_HASH/change" -G --data-urlencode "branch=${REAL_BRANCH:-peasant-smoke/nonexistent}"

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------

echo
MODE="non-strict"
[ "$STRICT" -eq 1 ] && MODE="strict"
echo "== summary ($MODE): $PASS passed, $FAIL failed, $SKIP skipped =="
if [ "$FAIL" -gt 0 ]; then
  echo "logs: $LOG_DIR"
  exit 1
fi
exit 0
