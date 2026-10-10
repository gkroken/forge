#!/bin/bash
# cran-linux-serve-validate.sh — live test of serving CRAN Linux binaries on
# one URL (WORKPLAN-CRAN-LINUX L2) with real install.packages, renv and pak.
#
#   bash scripts/cran-linux-serve-validate.sh
#
# A builder installs packages through the group's __linux__ URL (from source,
# nothing is cached yet), packs and uploads them; then fresh client containers
# WITH THE COMPILER REMOVED install the same packages with each client — which
# can only succeed if forge served binaries. Clients that must get source
# (stock R, another R minor, another distro) run with the compiler present and
# are checked for a source install. Needs docker and network access to CRAN.
set -uo pipefail
REPO=$(cd "$(dirname "$0")/.." && pwd)
FT=${FT:-$(mktemp -d)}
B=http://localhost:8099
HD=http://host.docker.internal:8099
U=$HD/repository/cran-public/__linux__/noble/latest
IMG=forge-cranlinux-client:4.6.1
pass=0; fail=0
check() { if [ "$2" = "$3" ]; then pass=$((pass+1)); echo "PASS  $1"; else fail=$((fail+1)); echo "FAIL  $1: want $2, got $3 ${4:-}"; fi; }
run() { docker run --rm --add-host=host.docker.internal:host-gateway "$@"; }
NOCC='for b in gcc cc g++ c++ make; do p=$(command -v $b) && mv $p $p.hidden; done;'

# Client = builder image: rocker plus renv (CRAN source, pure R) and pak (its
# own binary repo). No third-party R package binaries are involved.
docker build -q -t $IMG - >/dev/null <<'EOF' || exit 1
FROM rocker/r-ver:4.6.1
RUN apt-get update -qq && apt-get install -y -qq --no-install-recommends curl >/dev/null && rm -rf /var/lib/apt/lists/*
RUN Rscript -e 'install.packages("renv", repos = "https://cloud.r-project.org")' \
 && Rscript -e 'install.packages("pak", repos = sprintf("https://r-lib.github.io/p/pak/stable/%s/%s/%s", .Platform$pkgType, R.Version()$os, R.Version()$arch))'
EOF

fuser -k 8099/tcp 2>/dev/null
mkdir -p "$FT/art" && (cd "$REPO" && go build -o "$FT/forge" ./cmd/forge) || exit 1
("$FT/forge" -addr :8099 -data "$FT/data" -auth >"$FT/log" 2>&1 &)
for i in $(seq 50); do curl -sf $B/healthz >/dev/null && break; sleep 0.3; done
TOK=$(grep -o 'forge_[A-Za-z0-9_-]*' "$FT/log" | head -1)
A=(-H "Authorization: Bearer $TOK")
api() { curl -s -o "$FT/out" -w "%{http_code}" "${A[@]}" "$@"; }
for r in cran-hosted cran-proxy; do
	curl -s "${A[@]}" $B/api/v1/repos/$r | python3 -c 'import json,sys;d=json.load(sys.stdin);d["anonymousRead"]=True;print(json.dumps(d))' >"$FT/r.json"
	api -X PUT --data @"$FT/r.json" $B/api/v1/repos/$r >/dev/null
done
check "create cran-linux" 201 "$(api -X POST $B/api/v1/repos -d '{"name":"cran-linux","format":"cran","kind":"hosted","noOverwrite":true,"sourceRepo":"cran-public","anonymousRead":true}')" "$(cat $FT/out)"
curl -s "${A[@]}" $B/api/v1/repos/cran-public | python3 -c 'import json,sys;d=json.load(sys.stdin);d["anonymousRead"]=True;d["members"]=d["members"]+["cran-linux"];print(json.dumps(d))' >"$FT/g.json"
check "add cran-linux to cran-public" 200 "$(api -X PUT --data @"$FT/g.json" $B/api/v1/repos/cran-public)" "$(cat $FT/out)"
"$REPO/scripts/cran-linux-manifest.sh" $IMG >"$FT/platform.json"
DIGEST=$(python3 -c 'import json;print(json.load(open("'$FT'/platform.json"))["builderDigest"])')
check "register the client image's key" 200 "$(api -X PUT --data @"$FT/platform.json" $B/api/v1/cran/platforms/noble-x86_64/4.6)"
BT=$(curl -s "${A[@]}" -X POST $B/api/v1/tokens -d '{"description":"builder","grants":[{"repo":"cran-linux","actions":["read","write"],"selectors":["bin/linux/noble-x86_64/**"]}]}' |
	python3 -c 'import json,sys;print(json.load(sys.stdin)["secret"])')

# --- Builder: first install compiles; the helper uploads. -------------------
helper() { run -v "$REPO/scripts":/s:ro -v "$FT/art":/out -e FORGE_URL=$HD -e FORGE_TOKEN=$BT -e FORGE_BUILDER_DIGEST=$DIGEST $IMG bash -c "$1"; }
helper "mkdir /lib1 && Rscript -e 'options(repos=c(CRAN=\"$U\")); install.packages(c(\"jsonlite\",\"praise\"), lib=\"/lib1\")' >/out/build.log 2>&1
	/s/cran-linux-upload.sh --lib /lib1 >/out/upload.log 2>&1; echo \$? >/out/upload.rc
	cd /lib1 && for p in jsonlite; do v=\$(Rscript -e \"cat(as.character(packageVersion('\$p', lib.loc='/lib1')))\"); tar -czhf /out/\${p}_\$v.tar.gz \$p; done
	/s/cran-linux-upload.sh --lib /lib1 >/out/upload2.log 2>&1
	chown -R $(id -u) /out"
check "builder compiled jsonlite (no binary yet)" 1 "$(grep -c "installing \*source\* package .jsonlite" "$FT/art/build.log")"
check "helper uploaded both" 1 "$(grep -c '2 uploaded, 0 already present, 0 uploaded by another builder, 0 failed' "$FT/art/upload.log")" "$(cat "$FT/art/upload.log")"
check "helper exits 0" 0 "$(cat "$FT/art/upload.rc")"
check "second run finds both present" 1 "$(grep -c '0 uploaded, 2 already present' "$FT/art/upload2.log")" "$(cat "$FT/art/upload2.log")"
# A renv builder: the project library is symlinks into renv's cache, and the
# helper's default library is that project library.
helper "mkdir /p && cd /p && echo 'library(glue)' > app.R && Rscript -e 'options(repos=c(CRAN=\"$U\")); renv::init(restart=FALSE)' >/out/renv-build.log 2>&1
	/s/cran-linux-upload.sh >/out/upload-renv.log 2>&1; chown -R $(id -u) /out"
check "renv builder: helper uploads glue through renv's symlinked library" 1 "$(grep -c '^uploaded glue ' "$FT/art/upload-renv.log")" "$(cat "$FT/art/upload-renv.log")"
check "renv builder: nothing refused" 1 "$(grep -c ' 0 failed$' "$FT/art/upload-renv.log")" "$(cat "$FT/art/upload-renv.log")"
out=$(run $IMG bash -c "$NOCC Rscript -e 'options(repos=c(CRAN=\"$U\")); install.packages(\"glue\"); cat(\"LOADED\", as.character(glue::glue(\"{1+1}\")), \"\\n\")' 2>&1")
check "client installs the renv-built glue as a binary" 1 "$(echo "$out" | grep -c 'installing \*binary\* package .glue')" "$out"
check "renv-built glue loads" 1 "$(echo "$out" | grep -c 'LOADED 2')"

JS=$(cd "$FT/art" && ls jsonlite_*.tar.gz)

# --- The URL itself. ---------------------------------------------------------
hdr() { curl -s -o /dev/null -D - -A "$1" "$B/repository/cran-public/__linux__/noble/latest/src/contrib/$JS" | tr -d '\r' | grep -i "^$2:" | cut -d' ' -f2-; }
check "rocker UA gets binary" binary "$(hdr 'R/4.6.1 R (4.6.1 x86_64-pc-linux-gnu x86_64 linux-gnu)' x-forge-package-type)"
check "stock R UA gets source" source "$(hdr 'libcurl/8.5.0' x-forge-package-type)"
check "response varies on User-Agent" User-Agent "$(hdr 'libcurl/8.5.0' vary)"
check "pak index lists jsonlite for pak" 1 "$(curl -s -A 'R (4.6.1 x86_64-pc-linux-gnu x86_64 linux-gnu)' $B/repository/cran-public/latest/bin/linux/4.6-noble/contrib/4.6/PACKAGES | grep -c '^Package: jsonlite$')"
check "pak index empty for stock R" 0 "$(curl -s -A 'libcurl/8.5.0' $B/repository/cran-public/latest/bin/linux/4.6-noble/contrib/4.6/PACKAGES | grep -c '^Package:')"

# --- Clients with no compiler: only binaries can install. -------------------
LOAD='cat("LOADED", jsonlite::toJSON(1:2), praise::praise("${EXCLAMATION}!") != "", "\n")'
out=$(run $IMG bash -c "$NOCC Rscript -e 'options(repos=c(CRAN=\"$U\")); install.packages(c(\"jsonlite\",\"praise\")); $LOAD' 2>&1")
check "install.packages: both installed as binaries" 2 "$(echo "$out" | grep -c 'installing \*binary\* package')" "$out"
check "install.packages: jsonlite loads" 1 "$(echo "$out" | grep -c 'LOADED \[1,2\] TRUE')"
out=$(run $IMG bash -c "$NOCC Rscript -e 'options(repos=c(CRAN=\"$U\")); pak::pkg_install(c(\"jsonlite\",\"praise\")); $LOAD' 2>&1 | sed 's/\x1b\[[0-9;?]*[a-zA-Z]//g'")
check "pak: both fetched as binaries" 2 "$(echo "$out" | grep -c 'Got .*(x86_64-pc-linux-gnu-ubuntu-24.04)')" "$out"
check "pak: jsonlite loads" 1 "$(echo "$out" | grep -c 'LOADED \[1,2\] TRUE')"
# renv: snapshot a project that uses the URL, restore it in a fresh container.
run -v "$FT":/out $IMG bash -c "mkdir /p && cd /p && echo 'library(jsonlite); library(praise)' > app.R &&
	Rscript -e 'options(repos=c(CRAN=\"$U\")); renv::init(restart=FALSE)' >/dev/null 2>&1; cp renv.lock /out/; chown $(id -u) /out/renv.lock"
# renv keeps lockfiles platform-neutral: it records PPM's source form of the
# URL ({group}/latest) and at restore asks /repository/__api__/status whether
# the client's distro has binaries before using the __linux__ URL again
# (docs/notes/cran-linux-clients.md §2). No client setting is involved.
check "lockfile records the source form {group}/latest" 1 "$(grep -c "\"$HD/repository/cran-public/latest\"" "$FT/renv.lock")"
check "status endpoint lists noble with binaries" 1 "$(curl -s $B/repository/__api__/status | python3 -c 'import json,sys;print(sum(1 for d in json.load(sys.stdin)["distros"] if d["binaryURL"]=="noble" and d["binaries"]))')"
out=$(run -v "$FT/renv.lock":/l/renv.lock:ro $IMG bash -c "$NOCC mkdir /p && cp /l/renv.lock /p && cd /p && Rscript -e 'renv::restore(prompt=FALSE); $LOAD' 2>&1 | sed 's/\x1b\[[0-9;?]*[a-zA-Z]//g'")
check "renv::restore: both installed as binaries" 2 "$(echo "$out" | grep -c 'installed binary')" "$out"
check "renv::restore: jsonlite loads" 1 "$(echo "$out" | grep -c 'LOADED \[1,2\] TRUE')"

# --- Clients that must get source (compiler present). ------------------------
out=$(run $IMG Rscript --vanilla -e "options(repos=c(CRAN='$U')); install.packages('jsonlite')" 2>&1)
check "stock install.packages (no R UA) gets source" 1 "$(echo "$out" | grep -c 'installing \*source\* package')"
out=$(run rocker/r-ver:4.4 Rscript -e "options(repos=c(CRAN='$U')); install.packages('jsonlite')" 2>&1)
check "R 4.4 client gets source" 1 "$(echo "$out" | grep -c 'installing \*source\* package')"
out=$(run $IMG Rscript -e "options(repos=c(CRAN='$HD/repository/cran-public/__linux__/jammy/latest')); install.packages('jsonlite')" 2>&1)
check "other distro path gets source" 1 "$(echo "$out" | grep -c 'installing \*source\* package')"

echo; echo "passed $pass, failed $fail"
[ -n "${KEEP:-}" ] || fuser -k 8099/tcp 2>/dev/null
[ $fail -eq 0 ]
