#!/usr/bin/env bash
# depguard-validate.sh — live validation of dependency-confusion protection
# against the REAL public registries (registry.npmjs.org, repo1.maven.org).
#
# Proves, through a running forge binary:
#   npm  — a name owned by the hosted group member (is-odd, which also exists
#          upstream) never reaches upstream via the group: the group packument
#          carries only the hosted version and upstream-only versions 404;
#          a claimed-but-unpublished name (@acme/*, left-pad) is refused with
#          403 + claim attribution, on the group AND straight on the proxy;
#          unclaimed names (is-number) still proxy through fine;
#          switching the guard off restores the merged/fall-through behaviour.
#   maven— same matrix for com/acme/** vs junit via a maven group.
#   plus — the block lands in the audit log and the
#          forge_depguard_blocked_total metric.
#
# Requires: curl, a built ./forge binary (go build -o forge ./cmd/forge),
# outbound network. Uses its own port + data dir; cleans up on exit.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PORT=18081
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
lacks() { # lacks <label> <haystack> <needle>
  if echo "$2" | grep -q "$3"; then echo "  FAIL: $1 (unexpectedly contains: $3)"; FAIL=$((FAIL+1));
  else echo "  PASS: $1"; PASS=$((PASS+1)); fi
}
status() { # status <label> <expected-code> <url>
  local got; got=$(curl -s -o /dev/null -w '%{http_code}' "$3")
  if [[ "$got" == "$2" ]]; then echo "  PASS: $1 ($got)"; PASS=$((PASS+1));
  else echo "  FAIL: $1 (want $2, got $got)"; FAIL=$((FAIL+1)); fi
}

# ── start forge ───────────────────────────────────────────────────────────────
"$ROOT/forge" -addr ":$PORT" -data "$WORK/data" >"$WORK/forge.log" 2>&1 &
FORGE_PID=$!
for i in $(seq 1 50); do curl -sf "$BASE/healthz" >/dev/null && break; sleep 0.2; done
curl -sf "$BASE/healthz" >/dev/null || { echo "forge did not start"; cat "$WORK/forge.log"; exit 1; }

# ── repos: hosted with claims + live proxy + group, npm and maven ────────────
mkrepo() { curl -s -o /dev/null -X POST -H 'Content-Type: application/json' -d "$1" "$BASE/api/v1/repos"; }
mkrepo '{"name":"dg-npm-hosted","format":"npm","kind":"hosted","claims":["@acme/**","left-pad"]}'
mkrepo '{"name":"dg-npm-proxy","format":"npm","kind":"proxy","upstream":"https://registry.npmjs.org"}'
mkrepo '{"name":"dg-npm-group","format":"npm","kind":"group","members":["dg-npm-hosted","dg-npm-proxy"]}'
mkrepo '{"name":"dg-mvn-hosted","format":"maven","kind":"hosted","claims":["com/acme/**"]}'
mkrepo '{"name":"dg-mvn-proxy","format":"maven","kind":"proxy","upstream":"https://repo1.maven.org/maven2"}'
mkrepo '{"name":"dg-mvn-group","format":"maven","kind":"group","members":["dg-mvn-hosted","dg-mvn-proxy"]}'

# Publish is-odd 0.0.1-local into the hosted member. is-odd EXISTS upstream
# (latest 3.0.1) — the exact dependency-confusion setup.
printf 'local-tarball' > "$WORK/pkg.tgz"
B64=$(base64 -w0 "$WORK/pkg.tgz")
cat > "$WORK/publish.json" <<EOF
{"_id":"is-odd","name":"is-odd","dist-tags":{"latest":"0.0.1-local"},
 "versions":{"0.0.1-local":{"name":"is-odd","version":"0.0.1-local","dist":{"shasum":"abc"}}},
 "_attachments":{"is-odd-0.0.1-local.tgz":{"content_type":"application/octet-stream","data":"$B64","length":13}}}
EOF
curl -s -o /dev/null -X PUT -H 'Content-Type: application/json' --data-binary @"$WORK/publish.json" "$BASE/repository/dg-npm-hosted/is-odd"

echo "== npm: hosted-owned name never reaches upstream (auto-derive) =="
PKM=$(curl -s "$BASE/repository/dg-npm-group/is-odd")
contains "group packument has hosted version" "$PKM" '0\.0\.1-local'
lacks    "group packument has NO upstream versions" "$PKM" '"3\.0\.1"'
status   "upstream-only version 404s via group" 404 "$BASE/repository/dg-npm-group/is-odd/-/is-odd-3.0.1.tgz"
contains "hosted tarball serves via group" "$(curl -s "$BASE/repository/dg-npm-group/is-odd/-/is-odd-0.0.1-local.tgz")" 'local-tarball'

echo "== npm: claimed-but-unpublished names are refused with attribution =="
R=$(curl -s "$BASE/repository/dg-npm-group/@acme/newpkg")
status   "claimed scope 403s via group" 403 "$BASE/repository/dg-npm-group/@acme/newpkg"
contains "403 names the claim" "$R" '@acme/\*\*'
contains "403 names the owner" "$R" 'dg-npm-hosted'
status   "claimed name 403s straight on the proxy" 403 "$BASE/repository/dg-npm-proxy/left-pad"
status   "claimed tarball 403s straight on the proxy" 403 "$BASE/repository/dg-npm-proxy/left-pad/-/left-pad-1.3.0.tgz"

echo "== npm: unclaimed names still proxy fine =="
PKM=$(curl -s "$BASE/repository/dg-npm-group/is-number")
if echo "$PKM" | grep -q '"versions"'; then
  contains "unclaimed packument via group" "$PKM" 'is-number'
  status   "unclaimed packument via proxy" 200 "$BASE/repository/dg-npm-proxy/is-number"
  status   "unclaimed tarball via group" 200 "$BASE/repository/dg-npm-group/is-number/-/is-number-7.0.0.tgz"
else
  echo "  SKIP: upstream npm registry not reachable from this network"
fi

echo "== npm: guard opt-out restores merge/fall-through =="
curl -s -o /dev/null -X PUT -H 'Content-Type: application/json' \
  -d '{"name":"dg-npm-group","format":"npm","kind":"group","members":["dg-npm-hosted","dg-npm-proxy"],"depConfusionGuard":false}' \
  "$BASE/api/v1/repos/dg-npm-group"
PKM=$(curl -s "$BASE/repository/dg-npm-group/is-odd")
contains "guard off: upstream versions merge again" "$PKM" '"3\.0\.1"'
curl -s -o /dev/null -X PUT -H 'Content-Type: application/json' \
  -d '{"name":"dg-npm-group","format":"npm","kind":"group","members":["dg-npm-hosted","dg-npm-proxy"]}' \
  "$BASE/api/v1/repos/dg-npm-group"

echo "== maven: same matrix through a maven group vs Maven Central =="
printf 'hosted-jar-bytes' > "$WORK/app.jar"
curl -s -o /dev/null -X PUT --data-binary @"$WORK/app.jar" "$BASE/repository/dg-mvn-hosted/com/acme/app/1.0.0/app-1.0.0.jar"
contains "hosted artifact serves via group" "$(curl -s "$BASE/repository/dg-mvn-group/com/acme/app/1.0.0/app-1.0.0.jar")" 'hosted-jar-bytes'
status   "upstream-only version of owned artifact 404s via group" 404 "$BASE/repository/dg-mvn-group/com/acme/app/9.9/app-9.9.jar"
status   "claimed unpublished artifact 403s via group" 403 "$BASE/repository/dg-mvn-group/com/acme/other/1.0/other-1.0.jar"
MD=$(curl -s "$BASE/repository/dg-mvn-group/com/acme/app/maven-metadata.xml")
contains "group metadata lists hosted version" "$MD" '1\.0\.0'
status   "unclaimed artifact proxies from Maven Central via group" 200 "$BASE/repository/dg-mvn-group/junit/junit/4.13.2/junit-4.13.2.pom"

echo "== governance surfaces =="
contains "block recorded in audit log" "$(curl -s "$BASE/api/v1/audit?limit=100")" 'dep-guard: blocked'
contains "forge_depguard_blocked_total exported" "$(curl -s "$BASE/metrics")" 'forge_depguard_blocked_total'

echo
echo "==================== RESULTS: $PASS passed, $FAIL failed ===================="
[[ $FAIL -eq 0 ]]
