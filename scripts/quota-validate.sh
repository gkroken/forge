#!/usr/bin/env bash
# quota-validate.sh — live validation of storage quota enforcement + soft-delete
# against a running forge binary (no network needed).
#
# Proves, through a running forge binary:
#   quota — a hosted repo with a tiny quota accepts publishes until it fills,
#           then refuses the next one with 507 + a JSON body naming used/quota
#           bytes; the refusal lands in the audit log and the
#           forge_quota_blocked_total / forge_repo_quota_used_ratio metrics;
#           a proxy repo with the same tiny quota is never blocked.
#   trash — the admin component delete soft-deletes (frees quota immediately,
#           blob moved under _trash/), the version is restorable and purgeable,
#           and cleanup/hard paths still free disk.
#
# Requires: curl, python3, a built ./forge binary (go build -o forge ./cmd/forge).
# Uses its own port + data dir; cleans up on exit.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PORT=18082
BASE="http://localhost:$PORT"
WORK="$(mktemp -d)"
FORGE_PID=""
PASS=0
FAIL=0

cleanup() {
  [[ -n "$FORGE_PID" ]] && kill "$FORGE_PID" 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT

contains() { # contains <label> <haystack> <needle>
  if echo "$2" | grep -q "$3"; then echo "  PASS: $1"; PASS=$((PASS+1));
  else echo "  FAIL: $1 (missing: $3)"; FAIL=$((FAIL+1)); fi
}
eq() { # eq <label> <got> <want>
  if [[ "$2" == "$3" ]]; then echo "  PASS: $1 ($2)"; PASS=$((PASS+1));
  else echo "  FAIL: $1 (want $3, got $2)"; FAIL=$((FAIL+1)); fi
}
code() { curl -s -o /dev/null -w '%{http_code}' "$@"; }

# ── start forge ───────────────────────────────────────────────────────────────
"$ROOT/forge" -addr ":$PORT" -data "$WORK/data" >"$WORK/forge.log" 2>&1 &
FORGE_PID=$!
for i in $(seq 1 50); do curl -sf "$BASE/healthz" >/dev/null && break; sleep 0.2; done
curl -sf "$BASE/healthz" >/dev/null || { echo "forge did not start"; cat "$WORK/forge.log"; exit 1; }

# ── repos: hosted maven + proxy npm, each with a ~2147-byte quota ─────────────
QGB=0.000002   # 0.000002 GB ≈ 2147 bytes
mkrepo() { curl -s -o /dev/null -X POST -H 'Content-Type: application/json' -d "$1" "$BASE/api/v1/repos"; }
mkrepo "{\"name\":\"q-mvn\",\"format\":\"maven\",\"kind\":\"hosted\",\"quotaGB\":$QGB}"
mkrepo "{\"name\":\"q-npm-proxy\",\"format\":\"npm\",\"kind\":\"proxy\",\"upstream\":\"https://registry.npmjs.org\",\"quotaGB\":$QGB}"

head -c 800 /dev/urandom > "$WORK/j.jar"
J="$BASE/repository/q-mvn/com/example/app"

echo "== quota: publishes accepted until the repo fills, then 507 =="
eq "publish 1.0.0" "$(code -X PUT --data-binary @"$WORK/j.jar" "$J/1.0.0/app-1.0.0.jar")" 201
sleep 0.6
eq "publish 2.0.0" "$(code -X PUT --data-binary @"$WORK/j.jar" "$J/2.0.0/app-2.0.0.jar")" 201
sleep 0.6
eq "publish 3.0.0 refused" "$(code -X PUT --data-binary @"$WORK/j.jar" "$J/3.0.0/app-3.0.0.jar")" 507
BODY=$(curl -s -X PUT --data-binary @"$WORK/j.jar" "$J/3.0.0/app-3.0.0.jar")
contains "507 body names the error" "$BODY" 'storage quota exceeded'
contains "507 body carries quotaBytes" "$BODY" 'quotaBytes'

echo "== quota: governance surfaces =="
M=$(curl -s "$BASE/metrics")
contains "forge_quota_blocked_total set" "$M" 'forge_quota_blocked_total{repo="q-mvn"}'
contains "forge_repo_quota_used_ratio set" "$M" 'forge_repo_quota_used_ratio{repo="q-mvn"}'
AUD=$(curl -s "$BASE/api/v1/audit?limit=200")
contains "audit row for the quota block" "$AUD" 'quota: blocked write to q-mvn'

echo "== quota: proxy cache-fills are never blocked =="
# Force cached content into the over-quota proxy; a cache-fill must not 507.
eq "proxy fetch not blocked by quota" "$(code "$BASE/repository/q-npm-proxy/is-odd")" 200

echo "== soft-delete: admin delete → trash frees quota =="
DEL=$(curl -s -X DELETE "$BASE/api/v1/repos/q-mvn/component?name=com.example:app&version=1.0.0")
contains "delete returns a trashId" "$DEL" 'trashId'
sleep 0.6
TRASH=$(curl -s "$BASE/api/v1/repos/q-mvn/trash")
N=$(echo "$TRASH" | python3 -c "import sys,json;print(len(json.load(sys.stdin)['trash']))")
eq "one item in trash" "$N" 1
contains "trashed blob moved under _trash/" "$TRASH" '_trash/'
eq "republish now succeeds (quota freed)" "$(code -X PUT --data-binary @"$WORK/j.jar" "$J/3.0.0/app-3.0.0.jar")" 201

echo "== soft-delete: restore round-trips the artifact =="
ID=$(echo "$TRASH" | python3 -c "import sys,json;print(json.load(sys.stdin)['trash'][0]['id'])")
eq "restore accepted" "$(code -X POST "$BASE/api/v1/repos/q-mvn/trash/restore?id=$ID")" 200
eq "restored jar downloads" "$(code "$J/1.0.0/app-1.0.0.jar")" 200
N=$(curl -s "$BASE/api/v1/repos/q-mvn/trash" | python3 -c "import sys,json;print(len(json.load(sys.stdin)['trash']))")
eq "trash empty after restore" "$N" 0

echo "== soft-delete: purge hard-deletes and frees disk =="
curl -s -o /dev/null -X DELETE "$BASE/api/v1/repos/q-mvn/component?name=com.example:app&version=2.0.0"
ID=$(curl -s "$BASE/api/v1/repos/q-mvn/trash" | python3 -c "import sys,json;print(json.load(sys.stdin)['trash'][0]['id'])")
PURGE=$(curl -s -X POST "$BASE/api/v1/repos/q-mvn/trash/purge?id=$ID")
contains "purge reports freedBytes" "$PURGE" 'freedBytes'
eq "purged version 404s" "$(code "$J/2.0.0/app-2.0.0.jar")" 404
test -z "$(find "$WORK/data/blobs/_trash" -type f 2>/dev/null)" \
  && { echo "  PASS: no trash blob files remain on disk"; PASS=$((PASS+1)); } \
  || { echo "  FAIL: trash blob files still on disk"; FAIL=$((FAIL+1)); }

echo "== integrity stays honest with a blob in trash =="
curl -s -o /dev/null -X DELETE "$BASE/api/v1/repos/q-mvn/component?name=com.example:app&version=3.0.0"
curl -s -o /dev/null -X POST "$BASE/api/v1/repos/q-mvn/verify?mode=full"
sleep 2
VER=$(curl -s "$BASE/api/v1/repos/q-mvn/verify")
F=$(echo "$VER" | python3 -c "import sys,json;print(json.load(sys.stdin).get('totalFindings'))")
eq "verify reports no orphan findings for trashed blob" "$F" 0

echo
echo "==================== RESULTS: $PASS passed, $FAIL failed ===================="
[[ "$FAIL" -eq 0 ]]
