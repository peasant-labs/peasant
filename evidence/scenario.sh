#!/usr/bin/env bash
# Scenario for peasant PR #721 / issue #682:
# harvest a synthetic session, delete its sessions row (FKs off) so its
# content is orphaned, then run `harvest verify --content`.
# Usage: scenario.sh <peasant-binary> <label>
set -u
BIN="$1"; LABEL="$2"
EVID="$(cd "$(dirname "$0")" && pwd)"
ROOT="$EVID/home-$LABEL${ROOT_SUFFIX:-}"
ID="aaaaaaaa-0000-4000-8000-000000000682"
rm -rf "$ROOT"
mkdir -p "$ROOT/projects/-tmp-proj" "$ROOT/cfgdir"
python3 - "$ROOT" "$ID" <<'PY'
import json, sys
root, sid = sys.argv[1], sys.argv[2]
lines = []
for i in range(6):
    lines.append(json.dumps({"type":"user","message":{"role":"user","content":f"request {i}"},"uuid":f"u-{i}","sessionId":sid,"timestamp":f"2026-05-30T04:{i:02d}:00.000Z","cwd":"/tmp/proj","version":"1.0","userType":"external"}))
    lines.append(json.dumps({"type":"assistant","message":{"role":"assistant","model":"claude-test","content":[{"type":"text","text":f"response {i}"}]},"uuid":f"a-{i}","parentUuid":f"u-{i}","sessionId":sid,"timestamp":f"2026-05-30T04:{i:02d}:30.000Z","cwd":"/tmp/proj","version":"1.0"}))
open(f"{root}/projects/-tmp-proj/{sid}.jsonl","w").write("\n".join(lines)+"\n")
PY
cat > "$ROOT/config.yaml" <<EOF
version: 1
sources:
  claude-code:
    enabled: true
    paths:
      - $ROOT/projects
  opencode: {enabled: false}
  cursor: {enabled: false}
  codex: {enabled: false}
  strike: {enabled: false}
  pi: {enabled: false}
output:
  basePath: $ROOT/out
EOF
P() { "$BIN" harvest --data-dir "$ROOT" --config-dir "$ROOT/cfgdir" --config "$ROOT/config.yaml" "$@"; }
DB="$ROOT/peasant/peasant.db"

echo "=== [$LABEL] binary: $BIN"
"$BIN" --version 2>/dev/null || true
echo "\$ peasant harvest   # synthetic session $ID"
P; echo "exit=$?"
echo "\$ peasant harvest verify --content   # clean harvest"
P verify --content; echo "exit=$?"
echo "\$ sqlite3 DB \"PRAGMA foreign_keys=OFF; DELETE FROM sessions WHERE session_id='$ID'\""
sqlite3 "$DB" "PRAGMA foreign_keys=OFF; DELETE FROM sessions WHERE session_id='$ID';"
echo "sessions rows left: $(sqlite3 "$DB" "SELECT count(*) FROM sessions WHERE session_id='$ID';")"
echo "content bodies left for that session: $(sqlite3 "$DB" "SELECT count(*) FROM session_entry_bodies WHERE session_id='$ID';")"
echo "\$ peasant harvest verify --content   # content is now orphaned"
P verify --content; echo "exit=$?"
if [ "$LABEL" = "AFTER" ]; then
  echo "\$ peasant harvest --force --session $ID   # the printed recovery"
  P --force --session "$ID"; echo "exit=$?"
  echo "\$ peasant harvest verify --content   # after recovery"
  P verify --content; echo "exit=$?"
fi
