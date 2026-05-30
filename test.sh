#!/usr/bin/env bash
# End-to-end smoke test for forge. Starts the server, exercises every format,
# and prints PASS/FAIL per check.
set -u
BASE="http://localhost:8080"
PASS=0; FAIL=0
ok()   { echo "  PASS: $1"; PASS=$((PASS+1)); }
bad()  { echo "  FAIL: $1"; FAIL=$((FAIL+1)); }
check(){ if echo "$2" | grep -q "$3"; then ok "$1"; else bad "$1 (got: $(echo "$2" | head -c120))"; fi; }

rm -rf ./data
./forge -addr :8080 -data ./data >/tmp/forge.log 2>&1 &
SRV=$!
trap "kill $SRV 2>/dev/null" EXIT
sleep 1

echo "== service index =="
check "root lists repositories" "$(curl -s $BASE/)" "maven-hosted"

echo "== MAVEN hosted =="
# Deploy a jar + pom the way `mvn deploy` would (PUT to the layout path).
echo "fake-jar-bytes" > /tmp/lib.jar
cat > /tmp/lib.pom <<'EOF'
<project><modelVersion>4.0.0</modelVersion>
<groupId>com.acme</groupId><artifactId>lib</artifactId><version>1.2.0</version></project>
EOF
curl -s -X PUT --data-binary @/tmp/lib.jar $BASE/repository/maven-hosted/com/acme/lib/1.2.0/lib-1.2.0.jar >/dev/null
curl -s -X PUT --data-binary @/tmp/lib.pom $BASE/repository/maven-hosted/com/acme/lib/1.2.0/lib-1.2.0.pom >/dev/null
# Second version, to prove metadata aggregation.
curl -s -X PUT --data-binary @/tmp/lib.jar $BASE/repository/maven-hosted/com/acme/lib/1.3.0/lib-1.3.0.jar >/dev/null
check "download jar" "$(curl -s $BASE/repository/maven-hosted/com/acme/lib/1.2.0/lib-1.2.0.jar)" "fake-jar-bytes"
META=$(curl -s $BASE/repository/maven-hosted/com/acme/lib/maven-metadata.xml)
check "metadata lists v1.2.0" "$META" "1.2.0"
check "metadata lists v1.3.0" "$META" "1.3.0"
check "metadata latest=1.3.0" "$META" "<latest>1.3.0</latest>"
# Synthesized sha1 sidecar should match a locally computed sha1.
WANT=$(sha1sum /tmp/lib.jar | cut -d' ' -f1)
GOT=$(curl -s $BASE/repository/maven-hosted/com/acme/lib/1.2.0/lib-1.2.0.jar.sha1)
check "synthesized .sha1 matches" "$GOT" "$WANT"

echo "== HELM hosted =="
# Build a real chart tarball: webapp/Chart.yaml
mkdir -p /tmp/webapp
cat > /tmp/webapp/Chart.yaml <<'EOF'
apiVersion: v2
name: webapp
version: 0.4.1
appVersion: "2.0"
description: A demo web application chart
EOF
tar -czf /tmp/webapp-0.4.1.tgz -C /tmp webapp
curl -s -X POST --data-binary @/tmp/webapp-0.4.1.tgz $BASE/repository/helm-hosted/api/charts >/dev/null
IDX=$(curl -s $BASE/repository/helm-hosted/index.yaml)
check "index.yaml has chart" "$IDX" "webapp"
check "index.yaml has version" "$IDX" "0.4.1"
check "index.yaml has digest" "$IDX" "digest:"
check "download chart tgz" "$(curl -s -o /tmp/dl.tgz -w '%{http_code}' $BASE/repository/helm-hosted/webapp-0.4.1.tgz)" "200"
check "chart api lists" "$(curl -s $BASE/repository/helm-hosted/api/charts)" "webapp"

echo "== NPM hosted =="
# Construct a publish payload (what `npm publish` PUTs).
printf 'tarball-contents-here' > /tmp/pkg.tgz
B64=$(base64 -w0 /tmp/pkg.tgz)
cat > /tmp/publish.json <<EOF
{
  "_id": "mypkg",
  "name": "mypkg",
  "dist-tags": { "latest": "1.0.0" },
  "versions": {
    "1.0.0": { "name": "mypkg", "version": "1.0.0", "dist": { "shasum": "abc" } }
  },
  "_attachments": {
    "mypkg-1.0.0.tgz": { "content_type": "application/octet-stream", "data": "$B64", "length": 21 }
  }
}
EOF
curl -s -X PUT -H "Content-Type: application/json" --data-binary @/tmp/publish.json $BASE/repository/npm-hosted/mypkg >/dev/null
PKM=$(curl -s $BASE/repository/npm-hosted/mypkg)
check "packument has version" "$PKM" "1.0.0"
check "tarball url points at forge" "$PKM" "repository/npm-hosted/mypkg/-/mypkg-1.0.0.tgz"
check "download published tarball" "$(curl -s $BASE/repository/npm-hosted/mypkg/-/mypkg-1.0.0.tgz)" "tarball-contents-here"

echo "== NPM proxy (LIVE upstream registry.npmjs.org) =="
PROXY=$(curl -s $BASE/repository/npm-proxy/is-odd)
if echo "$PROXY" | grep -q '"versions"'; then
  check "proxied packument fetched" "$PROXY" "is-odd"
  check "proxy rewrote tarball url" "$PROXY" "repository/npm-proxy/is-odd/-/"
  # Pull a real tarball through the proxy and verify it's a gzip.
  curl -s -o /tmp/isodd.tgz $BASE/repository/npm-proxy/is-odd/-/is-odd-3.0.1.tgz
  check "proxied tarball is gzip" "$(file /tmp/isodd.tgz)" "gzip"
else
  echo "  SKIP: upstream npm registry not reachable from this network"
fi

echo "== CRAN hosted =="
mkdir -p /tmp/cranpkg
cat > /tmp/cranpkg/DESCRIPTION <<'EOF'
Package: mathutils
Version: 0.2.0
Imports: stats
License: MIT
Title: Math Utilities
EOF
tar -czf /tmp/mathutils_0.2.0.tar.gz -C /tmp cranpkg
curl -s -X PUT --data-binary @/tmp/mathutils_0.2.0.tar.gz $BASE/repository/cran-hosted/src/contrib/mathutils_0.2.0.tar.gz >/dev/null
PKGS=$(curl -s $BASE/repository/cran-hosted/src/contrib/PACKAGES)
check "PACKAGES has package" "$PKGS" "Package: mathutils"
check "PACKAGES has version" "$PKGS" "Version: 0.2.0"
check "PACKAGES.gz served" "$(curl -s -o /tmp/p.gz -w '%{http_code}' $BASE/repository/cran-hosted/src/contrib/PACKAGES.gz)" "200"

echo
echo "==================== RESULTS: $PASS passed, $FAIL failed ===================="
exit $FAIL
