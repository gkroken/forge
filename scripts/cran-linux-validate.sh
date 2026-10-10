#!/bin/bash
# cran-linux-validate.sh — live test of CRAN Linux binary uploads
# (WORKPLAN-CRAN-LINUX L1) against a fresh forge on :8099, with binaries built
# by real R in rocker/r-ver:4.6.1 and :4.4 containers, installed through
# forge's own group. Needs docker and network access to CRAN.
#
#   bash scripts/cran-linux-validate.sh
#
# Exits non-zero if any check fails. Each guard here was also shown to fail
# this script when removed (mutation-tested, per /format-live-test).
set -uo pipefail
REPO=$(cd "$(dirname "$0")/.." && pwd)
FT=${FT:-$(mktemp -d)}
B=http://localhost:8099
HD=http://host.docker.internal:8099
pass=0; fail=0
check() { # check NAME EXPECTED ACTUAL [detail]
	if [ "$2" = "$3" ]; then pass=$((pass+1)); echo "PASS  $1"; else fail=$((fail+1)); echo "FAIL  $1: want $2, got $3 ${4:-}"; fi
}

fuser -k 8099/tcp 2>/dev/null
mkdir -p "$FT/art" && (cd $REPO && go build -o "$FT/forge" ./cmd/forge) || exit 1
("$FT/forge" -addr :8099 -data "$FT/data" -auth >"$FT/log" 2>&1 &)
for i in $(seq 50); do curl -sf $B/healthz >/dev/null && break; sleep 0.3; done
TOK=$(grep -o 'forge_[A-Za-z0-9_-]*' "$FT/log" | head -1)
[ -n "$TOK" ] || { echo "no bootstrap token"; tail "$FT/log"; exit 1; }
A=(-H "Authorization: Bearer $TOK")

# Public reads so containers can install through forge.
for r in cran-hosted cran-public; do
	curl -s "${A[@]}" $B/api/v1/repos/$r | python3 -c 'import json,sys;d=json.load(sys.stdin);d["anonymousRead"]=True;print(json.dumps(d))' >"$FT/r.json"
	curl -s -o /dev/null -w "%{http_code}\n" "${A[@]}" -X PUT --data @"$FT/r.json" $B/api/v1/repos/$r
done >/dev/null
code=$(curl -s -o "$FT/out" -w "%{http_code}" "${A[@]}" -X POST $B/api/v1/repos -d \
	'{"name":"cran-linux","format":"cran","kind":"hosted","noOverwrite":true,"sourceRepo":"cran-public","anonymousRead":true}')
check "create cran-linux" 201 "$code" "$(cat $FT/out)"

# Register the key from the real builder image.
$REPO/scripts/cran-linux-manifest.sh rocker/r-ver:4.6.1 >"$FT/platform.json"
DIGEST=$(python3 -c 'import json;print(json.load(open("'$FT'/platform.json"))["builderDigest"])')
code=$(curl -s -o "$FT/out" -w "%{http_code}" "${A[@]}" -X PUT --data @"$FT/platform.json" $B/api/v1/cran/platforms/noble-x86_64/4.6)
check "register noble-x86_64/4.6" 200 "$code" "$(cat $FT/out)"

# Builder tokens: one scoped to this key, one scoped to another key.
mint() { curl -s "${A[@]}" -X POST $B/api/v1/tokens -d "$1" | python3 -c 'import json,sys;print(json.load(sys.stdin)["secret"])'; }
BT=$(mint '{"description":"builder noble","grants":[{"repo":"cran-linux","actions":["read","write"],"selectors":["bin/linux/noble-x86_64/**"]}]}')
OT=$(mint '{"description":"builder jammy","grants":[{"repo":"cran-linux","actions":["read","write"],"selectors":["bin/linux/jammy-x86_64/**"]}]}')

# Build binaries the way a builder would: install through forge's group
# (so from source), then pack the installed directories with links followed.
build() { # build IMAGE OUTDIR
	mkdir -p "$2"
	docker run --rm --add-host=host.docker.internal:host-gateway -v "$2":/out "$1" bash -c "
		mkdir /lib1 && Rscript -e 'options(repos=c(CRAN=\"$HD/repository/cran-public/\")); install.packages(c(\"jsonlite\",\"praise\"), lib=\"/lib1\")' >/out/install.log 2>&1
		cd /lib1 && for p in jsonlite praise; do v=\$(Rscript -e \"cat(as.character(packageVersion('\$p', lib.loc='/lib1')))\"); tar -czhf /out/\${p}_\$v.tar.gz \$p; done
		chown -R $(id -u) /out"
}
build rocker/r-ver:4.6.1 "$FT/art/46"
build rocker/r-ver:4.4 "$FT/art/44"
JS=$(cd "$FT/art/46" && ls jsonlite_*.tar.gz); PR=$(cd "$FT/art/46" && ls praise_*.tar.gz)
check "builder compiled jsonlite from forge source" 1 "$(grep -c "installing \*source\* package .jsonlite" "$FT/art/46/install.log")"

P=$B/repository/cran-linux/bin/linux/noble-x86_64/contrib/4.6
put() { # put TOKEN DIGEST FILE URL
	local h=(); [ -n "$1" ] && h+=(-H "Authorization: Bearer $1"); [ -n "$2" ] && h+=(-H "X-Forge-Builder-Digest: $2")
	curl -s -o "$FT/out" -w "%{http_code}" "${h[@]}" -X PUT --data-binary @"$3" "$4"
}
check "upload real jsonlite binary" 201 "$(put "$BT" "$DIGEST" "$FT/art/46/$JS" $P/$JS)" "$(cat $FT/out)"
check "upload real praise binary (pure R)" 201 "$(put "$BT" "$DIGEST" "$FT/art/46/$PR" $P/$PR)" "$(cat $FT/out)"
check "re-upload is 409" 409 "$(put "$BT" "$DIGEST" "$FT/art/46/$JS" $P/$JS)"
check "no token is 401" 401 "$(put "" "$DIGEST" "$FT/art/46/$JS" $P/$JS)"
check "token for another key is 403" 403 "$(put "$OT" "$DIGEST" "$FT/art/46/$JS" $P/$JS)"
check "missing digest is 400" 400 "$(put "$BT" "" "$FT/art/46/$JS" $P/$JS)"
check "other builder image is 403" 403 "$(put "$BT" "sha256:$(printf '0%.0s' {1..64})" "$FT/art/46/$JS" $P/$JS)"
# A real R 4.4 build of the same package, presented as 4.6.
J44=$(cd "$FT/art/44" && ls jsonlite_*.tar.gz)
curl -s "${A[@]}" -X DELETE -o /dev/null $P/$JS
check "R 4.4 build under the 4.6 key is 400" 400 "$(put "$BT" "$DIGEST" "$FT/art/44/$J44" $P/$JS)" "$(cat $FT/out)"
grep -q "R 4.6" "$FT/out" && check "  ...and says why" yes yes || check "  ...and says why" yes no "$(cat $FT/out)"
check "unregistered key is 403" 403 "$(put "$TOK" "$DIGEST" "$FT/art/46/$JS" $B/repository/cran-linux/bin/linux/noble-x86_64/contrib/4.5/$JS)"
check ".tgz under bin/linux is refused" 404 "$(put "$TOK" "$DIGEST" "$FT/art/46/$JS" $P/${JS%.tar.gz}.tgz)"
# Same package, tampered DESCRIPTION (built from "different source").
mkdir -p "$FT/tamper" && tar -xzf "$FT/art/46/$JS" -C "$FT/tamper" && sed -i 's/^License: .*/License: GPL-3/' "$FT/tamper/jsonlite/DESCRIPTION" && tar -czf "$FT/tamper.tar.gz" -C "$FT/tamper" jsonlite
check "different source is 400" 400 "$(put "$BT" "$DIGEST" "$FT/tamper.tar.gz" $P/$JS)"
# Source tarball uploaded as a binary.
curl -s -o "$FT/src.tar.gz" $B/repository/cran-public/src/contrib/$JS
check "source tarball as binary is 400" 400 "$(put "$BT" "$DIGEST" "$FT/src.tar.gz" $P/$JS)"
check "re-upload after removal is 201" 201 "$(put "$BT" "$DIGEST" "$FT/art/46/$JS" $P/$JS)" "$(cat $FT/out)"

# The index lists it with Built; the stored bytes install as a binary with no compiler.
check "index lists jsonlite" 1 "$(curl -s $P/PACKAGES | grep -c '^Package: jsonlite')"
check "index carries Built" 1 "$(curl -s $P/PACKAGES | awk -v RS= '/Package: jsonlite/' | grep -c '^Built: R 4.6.1; x86_64-pc-linux-gnu')"
out=$(docker run --rm --add-host=host.docker.internal:host-gateway rocker/r-ver:4.6.1 bash -c "
	for b in gcc cc make; do p=\$(command -v \$b) && mv \$p \$p.hidden; done
	Rscript -e 'download.file(\"$HD/repository/cran-linux/bin/linux/noble-x86_64/contrib/4.6/$JS\", \"/tmp/$JS\", quiet=TRUE)' && R CMD INSTALL /tmp/$JS 2>&1 | grep -o 'installing \*binary\* package'; Rscript -e 'cat(jsonlite::toJSON(1:2))'")
check "stored binary installs without a compiler and loads" "installing*binary*package[1,2]" "$(echo $out | tr -d ' ')" "$out"

# Trash: soft-delete, re-upload, restore is refused (409), purge works.
PRV=${PR#praise_}; PRV=${PRV%.tar.gz}
code=$(curl -s -o /dev/null -w "%{http_code}" "${A[@]}" -X DELETE "$B/api/v1/repos/cran-linux/component?name=praise&version=$PRV")
check "soft-delete binary-only version" 200 "$code"
ID=$(curl -s "${A[@]}" $B/api/v1/repos/cran-linux/trash | python3 -c 'import json,sys;print(json.load(sys.stdin)["trash"][0]["id"])')
check "binary gone after soft-delete" 404 "$(curl -s -o /dev/null -w '%{http_code}' $P/$PR)"
check "re-upload after soft-delete" 201 "$(put "$BT" "$DIGEST" "$FT/art/46/$PR" $P/$PR)"
check "restore over the newer upload is 409" 409 "$(curl -s -o /dev/null -w '%{http_code}' "${A[@]}" -X POST "$B/api/v1/repos/cran-linux/trash/restore?id=$ID")"

echo; echo "passed $pass, failed $fail"
fuser -k 8099/tcp 2>/dev/null
[ $fail -eq 0 ]
