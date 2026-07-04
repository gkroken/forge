#!/usr/bin/env bash
# nexus-migrate-validate.sh — end-to-end validation of the Nexus migration
# against a real sonatype/nexus3 container.
#
# Flow: start Nexus → seed repos/content/security via its REST API (maven curl
# PUTs, npm/helm/r via the components upload API, docker via a real `docker
# push` to the connector port) → start a fresh forge → plan → apply → assert
# counts match and every hosted repo's FULL integrity verify comes back intact.
#
# Requires: docker, curl, jq, tar, gzip. Ports: 8081+5001 (nexus), 8080 (forge).
set -euo pipefail

NEXUS_NAME=${NEXUS_NAME:-nexus-mig}
NEXUS_URL=http://localhost:8081
FORGE_URL=http://localhost:8080
FORGE_DATA=${FORGE_DATA:-/tmp/forge-mig-demo}
WORK=$(mktemp -d)
PASS=0; FAIL=0

say()  { printf '\n\033[1m== %s ==\033[0m\n' "$*"; }
ok()   { PASS=$((PASS+1)); printf '  \033[32mPASS\033[0m: %s\n' "$*"; }
bad()  { FAIL=$((FAIL+1)); printf '  \033[31mFAIL\033[0m: %s\n' "$*"; }
check(){ if eval "$1"; then ok "$2"; else bad "$2"; fi; }

cleanup() {
  [ -n "${FORGE_PID:-}" ] && kill "$FORGE_PID" 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT

# ── 1. Nexus up ────────────────────────────────────────────────────────────────
say "starting Nexus (this takes a couple of minutes on first boot)"
if ! docker ps --format '{{.Names}}' | grep -qx "$NEXUS_NAME"; then
  docker rm -f "$NEXUS_NAME" 2>/dev/null || true
  docker run -d --name "$NEXUS_NAME" -p 8081:8081 -p 5001:5001 sonatype/nexus3 >/dev/null
fi
for i in $(seq 1 90); do
  if curl -sf "$NEXUS_URL/service/rest/v1/status" >/dev/null 2>&1; then break; fi
  sleep 5
  [ "$i" = 90 ] && { echo "nexus did not come up"; exit 1; }
done
NXPW=$(docker exec "$NEXUS_NAME" cat /nexus-data/admin.password 2>/dev/null || echo admin123)
NX="-u admin:$NXPW"
echo "  nexus admin password: $NXPW"

# Accept the EULA if this edition demands one (Community Edition ≥3.77).
EULA=$(curl -sf $NX "$NEXUS_URL/service/rest/v1/system/eula" || true)
if [ -n "$EULA" ] && echo "$EULA" | jq -e '.accepted == false' >/dev/null 2>&1; then
  echo "$EULA" | jq '.accepted = true' | \
    curl -sf $NX -X POST -H 'Content-Type: application/json' -d @- \
      "$NEXUS_URL/service/rest/v1/system/eula" >/dev/null && echo "  EULA accepted"
fi

# ── 2. source repositories ─────────────────────────────────────────────────────
say "creating source repositories"
mkrepo() { # $1 recipe path, $2 json body, $3 name
  code=$(curl -s -o /dev/null -w '%{http_code}' $NX -X POST \
    -H 'Content-Type: application/json' -d "$2" \
    "$NEXUS_URL/service/rest/v1/repositories/$1")
  case "$code" in 201) echo "  created $3";; 400) echo "  $3 already exists";; *) echo "  $3: HTTP $code"; esac
}
STORAGE='"storage":{"blobStoreName":"default","strictContentTypeValidation":true,"writePolicy":"ALLOW"}'
mkrepo npm/hosted  "{\"name\":\"npm-internal\",\"online\":true,$STORAGE}" npm-internal
mkrepo helm/hosted "{\"name\":\"helm-charts\",\"online\":true,$STORAGE}" helm-charts
mkrepo r/hosted    "{\"name\":\"r-packages\",\"online\":true,$STORAGE}" r-packages
mkrepo docker/hosted "{\"name\":\"docker-apps\",\"online\":true,$STORAGE,\"docker\":{\"v1Enabled\":false,\"forceBasicAuth\":true,\"httpPort\":5001}}" docker-apps
mkrepo npm/proxy   "{\"name\":\"npm-mirror\",\"online\":true,$STORAGE,\"proxy\":{\"remoteUrl\":\"https://registry.npmjs.org\",\"contentMaxAge\":1440,\"metadataMaxAge\":1440},\"negativeCache\":{\"enabled\":true,\"timeToLive\":1440},\"httpClient\":{\"blocked\":false,\"autoBlock\":true}}" npm-mirror
mkrepo maven/group "{\"name\":\"maven-all\",\"online\":true,$STORAGE,\"group\":{\"memberNames\":[\"maven-releases\"]}}" maven-all

# ── 3. seed content ────────────────────────────────────────────────────────────
say "seeding maven (curl PUTs incl. sha1 sidecars)"
for V in 1.0 1.1; do
  printf 'fake jar bytes %s' "$V" > "$WORK/app-$V.jar"
  cat > "$WORK/app-$V.pom" <<EOF
<project><modelVersion>4.0.0</modelVersion><groupId>org.acme</groupId>
<artifactId>app</artifactId><version>$V</version></project>
EOF
  for f in app-$V.jar app-$V.pom; do
    # 201 on first upload; maven-releases rejects redeploys (400) — idempotent.
    curl -s -o /dev/null $NX --upload-file "$WORK/$f" "$NEXUS_URL/repository/maven-releases/org/acme/app/$V/$f" || true
    sha1sum "$WORK/$f" | cut -d' ' -f1 > "$WORK/$f.sha1"
    curl -s -o /dev/null $NX --upload-file "$WORK/$f.sha1" "$NEXUS_URL/repository/maven-releases/org/acme/app/$V/$f.sha1" || true
  done
done
ok "maven seeded (2 versions × jar+pom+sha1)"

say "seeding npm (components upload API)"
mknpm() { # $1 version
  local d="$WORK/npm-$1"; mkdir -p "$d/package"
  printf '{"name":"acme-lib","version":"%s","description":"migration seed"}' "$1" > "$d/package/package.json"
  tar -czf "$WORK/acme-lib-$1.tgz" -C "$d" package
  curl -s -o /dev/null $NX -F "npm.asset=@$WORK/acme-lib-$1.tgz" \
    "$NEXUS_URL/service/rest/v1/components?repository=npm-internal" || true
}
mknpm 1.0.0; mknpm 1.1.0; ok "npm seeded (acme-lib 1.0.0 + 1.1.0)"

say "seeding helm"
mkdir -p "$WORK/web"
printf 'name: web\nversion: 1.2.3\napiVersion: v2\ndescription: seed chart\n' > "$WORK/web/Chart.yaml"
tar -czf "$WORK/web-1.2.3.tgz" -C "$WORK" web
curl -s -o /dev/null $NX -F "helm.asset=@$WORK/web-1.2.3.tgz" \
  "$NEXUS_URL/service/rest/v1/components?repository=helm-charts" || true
ok "helm seeded (web-1.2.3)"

say "seeding r/cran"
mkdir -p "$WORK/seedpkg"
printf 'Package: seedpkg\nVersion: 2.0.0\nLicense: MIT\nTitle: Seed\nDescription: Seed.\nAuthor: t\nMaintainer: t <t@t>\n' > "$WORK/seedpkg/DESCRIPTION"
tar -czf "$WORK/seedpkg_2.0.0.tar.gz" -C "$WORK" seedpkg
curl -s -o /dev/null $NX -F "r.asset=@$WORK/seedpkg_2.0.0.tar.gz" -F "r.asset.pathId=src/contrib" \
  "$NEXUS_URL/service/rest/v1/components?repository=r-packages" || true
ok "cran seeded (seedpkg_2.0.0)"

say "seeding docker (real docker push to the connector port)"
docker pull -q busybox:latest >/dev/null
docker tag busybox:latest localhost:5001/acme/app:v1
echo "$NXPW" | docker login localhost:5001 -u admin --password-stdin >/dev/null 2>&1
docker push -q localhost:5001/acme/app:v1 >/dev/null || true
ok "docker seeded (acme/app:v1)"

# ── 4. security model ──────────────────────────────────────────────────────────
say "seeding security (selector, privileges, role, users)"
curl -s $NX -X POST -H 'Content-Type: application/json' -d \
  '{"name":"acme-only","description":"","expression":"path =~ \"^/org/acme/.*\""}' \
  "$NEXUS_URL/service/rest/v1/security/content-selectors" >/dev/null || true
curl -s $NX -X POST -H 'Content-Type: application/json' -d \
  '{"name":"acme-scoped","description":"","format":"maven2","actions":["READ","ADD"],"repository":"maven-releases","contentSelector":"acme-only"}' \
  "$NEXUS_URL/service/rest/v1/security/privileges/repository-content-selector" >/dev/null || true
curl -s $NX -X POST -H 'Content-Type: application/json' -d \
  '{"id":"developers","name":"Developers","description":"dev team","privileges":["nx-repository-view-maven2-maven-releases-add","nx-repository-view-*-*-read","acme-scoped"],"roles":[]}' \
  "$NEXUS_URL/service/rest/v1/security/roles" >/dev/null || true
curl -s $NX -X POST -H 'Content-Type: application/json' -d \
  '{"userId":"alice","firstName":"Alice","lastName":"Ash","emailAddress":"alice@acme.test","password":"alicepw-123","status":"active","roles":["developers"]}' \
  "$NEXUS_URL/service/rest/v1/security/users" >/dev/null || true
curl -s $NX -X POST -H 'Content-Type: application/json' -d \
  '{"userId":"bob","firstName":"Bob","lastName":"Berg","emailAddress":"bob@acme.test","password":"bobpw-12345","status":"active","roles":["developers","nx-anonymous"]}' \
  "$NEXUS_URL/service/rest/v1/security/users" >/dev/null || true
ok "security seeded"

# ── 5. forge up (with auth: the migration API is admin-gated) ──────────────────
say "starting a fresh forge (-auth)"
rm -rf "$FORGE_DATA"
"$(dirname "$0")/../forge" -addr :8080 -auth -data "$FORGE_DATA" > "$WORK/forge.log" 2>&1 &
FORGE_PID=$!
for i in $(seq 1 20); do curl -sf "$FORGE_URL/healthz" >/dev/null 2>&1 && break; sleep 0.5; done
TOKEN=$(grep -o 'forge_[0-9a-f]\{64\}' "$WORK/forge.log" | head -1)
[ -n "$TOKEN" ] || { echo "no bootstrap token found in forge log"; exit 1; }
FA=(-H "Authorization: Bearer $TOKEN")
# Unauthenticated access to the migration API must be refused.
check "[ \"$(curl -s -o /dev/null -w '%{http_code}' $FORGE_URL/api/v1/migration)\" = 401 ]" \
  "migration API requires admin auth"

# ── 6. plan ─────────────────────────────────────────────────────────────────────
say "plan"
curl -sf "${FA[@]}" -X POST -H 'Content-Type: application/json' -d \
  "{\"url\":\"$NEXUS_URL\",\"username\":\"admin\",\"password\":\"$NXPW\",\"includeSecurity\":true}" \
  "$FORGE_URL/api/v1/migration/plan" > "$WORK/plan.json"
jq -r '.repos[] | "  \(.source) [\(.sourceFormat)/\(.sourceType)] -> \(.action) \(.reason // "")"' "$WORK/plan.json"
check "jq -e '[.repos[] | select(.action==\"create\" or .action==\"exists\")] | length >= 7' $WORK/plan.json >/dev/null" \
  "plan migrates >=7 repos"
check "jq -e '.repos[] | select(.source==\"npm-mirror\") | .upstream == \"https://registry.npmjs.org\"' $WORK/plan.json >/dev/null" \
  "proxy upstream carried over"
check "jq -e '.repos[] | select(.source==\"docker-apps\") | .assets >= 1' $WORK/plan.json >/dev/null" \
  "docker-apps inventory counted"
check "jq -e '.security.roles[] | select(.id==\"developers\") | .grants | length >= 2' $WORK/plan.json >/dev/null" \
  "developers role translated to grants"
check "jq -e '.security.roles[] | select(.id==\"developers\") | .grants[] | select(.selectors==[\"org/acme/**\"])' $WORK/plan.json >/dev/null" \
  "content selector became forge selector org/acme/**"

# ── 7. apply + wait ─────────────────────────────────────────────────────────────
say "apply"
curl -sf "${FA[@]}" -X POST "$FORGE_URL/api/v1/migration/apply" >/dev/null
for i in $(seq 1 120); do
  curl -sf "${FA[@]}" "$FORGE_URL/api/v1/migration" > "$WORK/st.json"
  S=$(jq -r '.run.status // "queued"' "$WORK/st.json")
  { [ "$S" = complete ] || [ "$S" = failed ]; } && break
  sleep 1
done
jq -r '.repos[] | "  \(.repo): \(.status) migrated=\(.migrated) skipped=\(.skipped) failed=\(.failed) source=\(.sourceAssets)"' "$WORK/st.json"
check "[ \"$S\" = complete ]" "migration run complete"
check "jq -e '[.repos[] | select(.failed > 0)] | length == 0' $WORK/st.json >/dev/null" "zero asset failures"
check "jq -e '[.repos[] | select(.status==\"complete\")] | length >= 7' $WORK/st.json >/dev/null" "all repos complete"

# counts match: migrated+skipped == sourceAssets for every hosted repo
check "jq -e '[.repos[] | select(.sourceAssets>0) | select(.migrated+.skipped < .sourceAssets)] | length == 0' $WORK/st.json >/dev/null" \
  "counts match source on every content repo"

# ── 8. integrity verify (the acceptance tool) ──────────────────────────────────
say "integrity verify (full) per migrated hosted repo"
for R in maven-releases npm-internal helm-charts r-packages docker-apps; do
  for i in $(seq 1 60); do
    curl -sf "${FA[@]}" "$FORGE_URL/api/v1/repos/$R/verify" > "$WORK/rep.json"
    RS=$(jq -r .status "$WORK/rep.json")
    { [ "$RS" = complete ] || [ "$RS" = failed ]; } && break
    sleep 1
  done
  FINDINGS=$(jq -r '.totalFindings // "?"' "$WORK/rep.json")
  check "[ \"$RS\" = complete ] && [ \"$FINDINGS\" = 0 ]" "$R verify: intact (findings=$FINDINGS, status=$RS)"
done

# ── 9. migrated security is live ───────────────────────────────────────────────
say "migrated permissions"
curl -sf "${FA[@]}" "$FORGE_URL/api/v1/roles" > "$WORK/roles.json"
check "jq -e '.custom[] | select(.name==\"developers\") | .grants | length >= 2' $WORK/roles.json >/dev/null" \
  "developers role exists in forge with grants"
curl -sf "${FA[@]}" "$FORGE_URL/api/v1/users" > "$WORK/users.json"
check "jq -e '.[] | select(.username==\"alice\") | .disabled == true' $WORK/users.json >/dev/null" \
  "alice imported (disabled, awaiting password)"

# ── 10. resume is idempotent ────────────────────────────────────────────────────
say "re-apply (resume)"
curl -sf "${FA[@]}" -X POST "$FORGE_URL/api/v1/migration/apply" >/dev/null
for i in $(seq 1 120); do
  curl -sf "${FA[@]}" "$FORGE_URL/api/v1/migration" > "$WORK/st2.json"
  S=$(jq -r '.run.status // "queued"' "$WORK/st2.json")
  [ "$S" = complete ] && break
  sleep 1
done
check "jq -e '[.repos[] | select(.sourceAssets>0) | select(.migrated != 0)] | length == 0' $WORK/st2.json >/dev/null" \
  "resume re-copied nothing (all skipped)"
check "jq -e '[.repos[] | select(.failed>0)] | length == 0' $WORK/st2.json >/dev/null" "resume had zero failures"

say "RESULTS: $PASS passed, $FAIL failed"
[ "$FAIL" = 0 ]
