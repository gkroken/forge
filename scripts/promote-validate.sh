#!/usr/bin/env bash
# promote-validate.sh — live validation of promotion (copy) + immutable repos
# against a running forge binary (no network needed).
#
# Proves, through a running forge binary:
#   promote — a component+version published to a hosted "staging" repo is
#             promoted (copied) into a hosted "release" repo of the same format;
#             the target artifact is byte-for-byte identical, re-downloadable,
#             and carries a provenance record ("promoted from staging@sha256…");
#             the promotion lands in the audit log + forge_promotions_total.
#   immutable — an immutable release repo refuses a re-promote of an existing
#             version (409), refuses a direct client re-publish (409), and
#             refuses a soft-delete (409); a brand-new version still publishes.
#   quota   — a promote into a target that is at/over quota is refused (507).
#   integrity — a full verify of the promoted target reports zero findings (the
#             copy is self-contained; provenance is not an integrity input).
#
# Requires: curl, python3, cmp, a built ./forge (go build -o forge ./cmd/forge).
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PORT=18083
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

contains() { if echo "$2" | grep -q "$3"; then echo "  PASS: $1"; PASS=$((PASS+1)); else echo "  FAIL: $1 (missing: $3)"; FAIL=$((FAIL+1)); fi; }
eq()       { if [[ "$2" == "$3" ]]; then echo "  PASS: $1 ($2)"; PASS=$((PASS+1)); else echo "  FAIL: $1 (want $3, got $2)"; FAIL=$((FAIL+1)); fi; }
code()     { curl -s -o /dev/null -w '%{http_code}' "$@"; }
mkrepo()   { curl -s -o /dev/null -X POST -H 'Content-Type: application/json' -d "$1" "$BASE/api/v1/repos"; }
promote()  { curl -s -X POST -H 'Content-Type: application/json' -d "$2" "$BASE/api/v1/repos/$1/promote"; }
pcode()    { curl -s -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d "$2" "$BASE/api/v1/repos/$1/promote"; }

# ── start forge ───────────────────────────────────────────────────────────────
"$ROOT/forge" -addr ":$PORT" -data "$WORK/data" >"$WORK/forge.log" 2>&1 &
FORGE_PID=$!
for i in $(seq 1 50); do curl -sf "$BASE/healthz" >/dev/null && break; sleep 0.2; done
curl -sf "$BASE/healthz" >/dev/null || { echo "forge did not start"; cat "$WORK/forge.log"; exit 1; }

# ── repos: maven staging → release; an immutable release; a tiny-quota release ─
mkrepo '{"name":"mvn-staging","format":"maven","kind":"hosted"}'
mkrepo '{"name":"mvn-release","format":"maven","kind":"hosted"}'
mkrepo '{"name":"mvn-locked","format":"maven","kind":"hosted","immutable":true}'
mkrepo '{"name":"mvn-full","format":"maven","kind":"hosted","quotaGB":0.000002}'
mkrepo '{"name":"npm-staging","format":"npm","kind":"hosted"}'
mkrepo '{"name":"npm-release","format":"npm","kind":"hosted"}'

head -c 1500 /dev/urandom > "$WORK/app.jar"
echo '<project><modelVersion>4.0.0</modelVersion><groupId>com.example</groupId><artifactId>app</artifactId><version>1.0.0</version></project>' > "$WORK/app.pom"
S="$BASE/repository/mvn-staging/com/example/app/1.0.0"

echo "== seed: publish com.example:app 1.0.0 to staging =="
eq "PUT jar" "$(code -X PUT --data-binary @"$WORK/app.jar" "$S/app-1.0.0.jar")" 201
eq "PUT pom" "$(code -X PUT --data-binary @"$WORK/app.pom" "$S/app-1.0.0.pom")" 201

echo "== promote: staging → release =="
R=$(promote mvn-release '{"sourceRepo":"mvn-staging","component":"com.example:app","version":"1.0.0"}')
contains "promote ok"             "$R" '"promoted":true'
contains "response names source"  "$R" '"sourceRepo":"mvn-staging"'
contains "response carries digest" "$R" '"sourceDigest"'
SRCDGST=$(python3 -c 'import hashlib,sys;print(hashlib.sha256(open(sys.argv[1],"rb").read()).hexdigest())' "$WORK/app.jar")
contains "digest == source jar sha256" "$R" "$SRCDGST"

echo "== promote: target artifact is byte-for-byte identical + re-downloadable =="
curl -s "$BASE/repository/mvn-release/com/example/app/1.0.0/app-1.0.0.jar" -o "$WORK/dl.jar"
if cmp -s "$WORK/app.jar" "$WORK/dl.jar"; then echo "  PASS: promoted jar byte-identical"; PASS=$((PASS+1)); else echo "  FAIL: promoted jar differs"; FAIL=$((FAIL+1)); fi
eq "target maven-metadata regenerated" "$(code "$BASE/repository/mvn-release/com/example/app/maven-metadata.xml")" 200

echo "== provenance: surfaced on the target's component detail =="
D=$(curl -s "$BASE/ui/browse/mvn-release/detail?pkg=com.example:app&ver=1.0.0")
contains "detail has provenance"        "$D" '"provenance"'
contains "provenance names source repo" "$D" '"sourceRepo":"mvn-staging"'

echo "== governance: audit + metric =="
contains "audit records the promotion" "$(curl -s "$BASE/api/v1/audit?limit=50")" 'promote: mvn-staging'
contains "forge_promotions_total set"  "$(curl -s "$BASE/metrics")" 'forge_promotions_total{repo="mvn-release"}'

echo "== immutable: re-promote of an existing version is refused (409) =="
promote mvn-locked '{"sourceRepo":"mvn-staging","component":"com.example:app","version":"1.0.0"}' >/dev/null
eq "first promote to locked ok" "$(code "$BASE/repository/mvn-locked/com/example/app/1.0.0/app-1.0.0.jar")" 200
eq "re-promote same version 409" "$(pcode mvn-locked '{"sourceRepo":"mvn-staging","component":"com.example:app","version":"1.0.0"}')" 409

echo "== immutable: direct client overwrite refused, new version still allowed =="
eq "re-publish existing jar 409" "$(code -X PUT --data-binary @"$WORK/app.jar" "$BASE/repository/mvn-locked/com/example/app/1.0.0/app-1.0.0.jar")" 409
eq "publish NEW version 201"     "$(code -X PUT --data-binary @"$WORK/app.jar" "$BASE/repository/mvn-locked/com/example/app/2.0.0/app-2.0.0.jar")" 201

echo "== immutable: soft-delete refused (409) =="
eq "delete on immutable 409" "$(code -X DELETE "$BASE/api/v1/repos/mvn-locked/component?name=com.example:app&version=1.0.0")" 409

echo "== quota: promote into an at-quota target is refused (507) =="
head -c 1900 /dev/urandom > "$WORK/big.jar"
BS="$BASE/repository/mvn-staging/com/example/big/1.0.0"
code -X PUT --data-binary @"$WORK/big.jar" "$BS/big-1.0.0.jar" >/dev/null
# fill mvn-full to its ~2147-byte quota first, then a promote must 507
code -X PUT --data-binary @"$WORK/big.jar" "$BASE/repository/mvn-full/com/example/x/1.0.0/x-1.0.0.jar" >/dev/null
sleep 0.6
eq "promote over quota 507" "$(pcode mvn-full '{"sourceRepo":"mvn-staging","component":"com.example:big","version":"1.0.0"}')" 507

echo "== npm: promote copies the tarball + regenerates the packument =="
TAR=$(python3 - "$WORK" <<'PY'
import base64,gzip,io,json,os,sys,hashlib
work=sys.argv[1]
raw=b"fake npm tarball payload for leftpad 1.2.3"
open(os.path.join(work,"tar.bin"),"wb").write(raw)
b64=base64.b64encode(raw).decode()
doc={"_id":"leftpad","name":"leftpad","dist-tags":{"latest":"1.2.3"},
     "versions":{"1.2.3":{"name":"leftpad","version":"1.2.3","dist":{"tarball":"http://x/leftpad-1.2.3.tgz"}}},
     "_attachments":{"leftpad-1.2.3.tgz":{"data":b64}}}
open(os.path.join(work,"pub.json"),"w").write(json.dumps(doc))
print(hashlib.sha256(raw).hexdigest())
PY
)
eq "npm publish to staging" "$(code -X PUT -H 'Content-Type: application/json' --data-binary @"$WORK/pub.json" "$BASE/repository/npm-staging/leftpad")" 201
RN=$(promote npm-release '{"sourceRepo":"npm-staging","component":"leftpad","version":"1.2.3"}')
contains "npm promote ok" "$RN" '"promoted":true'
contains "npm digest == tarball sha256" "$RN" "$TAR"
contains "packument served from target" "$(curl -s "$BASE/repository/npm-release/leftpad")" '"1.2.3"'

echo "== integrity: a full verify of the promoted target is clean =="
curl -s -o /dev/null -X POST "$BASE/api/v1/repos/mvn-release/verify?mode=full"
for i in $(seq 1 40); do
  V=$(curl -s "$BASE/api/v1/repos/mvn-release/verify")
  echo "$V" | grep -q '"status":"complete"' && break
  sleep 0.25
done
contains "verify completed" "$V" '"status":"complete"'
contains "verify found zero findings" "$V" '"totalFindings":0'

echo
echo "──────────────────────────────────────────"
echo "  PASS: $PASS   FAIL: $FAIL"
[[ "$FAIL" -eq 0 ]] || exit 1
