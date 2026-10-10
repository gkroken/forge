#!/bin/bash
# cran-linux-upload.sh — upload the R packages a builder just compiled to
# forge's CRAN Linux binary repository (WORKPLAN-CRAN-LINUX.md), so the next
# install of each one on this platform is a binary.
#
# Run it in the builder, after renv::restore() / install.packages():
#
#   FORGE_URL=https://forge.example.com \
#   FORGE_TOKEN=forge_…            # write grant on the binary repo (scope it
#                                  # with selectors: bin/linux/noble-x86_64/**)
#   FORGE_BUILDER_DIGEST=sha256:…  # the builder image ID, as registered
#   scripts/cran-linux-upload.sh [--repo cran-linux] [--lib PATH] [--strict]
#
# The image ID is not visible from inside a container, so the pipeline passes
# it in: `docker image inspect -f '{{.Id}}' IMAGE`, the same value
# scripts/cran-linux-manifest.sh registered.
#
# For every package in the library (default: the first of .libPaths(), which
# is the project library under renv) that was not installed from GitHub, a
# git URL or a local path, it asks forge whether the binary for this platform exists and,
# if not, packs the installed directory (following renv's cache symlinks) and
# uploads it. Packages installed from GitHub or local paths are skipped:
# forge checks every binary against the source it serves, and has none for
# those. A 409 means another builder uploaded the same binary first.
#
# Uploading is best-effort: a failure is reported but does not fail the build
# (it only costs the next install a compile) unless --strict is given.
set -uo pipefail

repo=cran-linux lib="" strict=0
while [ $# -gt 0 ]; do
	case "$1" in
	--repo) repo=$2; shift 2 ;;
	--lib) lib=$2; shift 2 ;;
	--strict) strict=1; shift ;;
	*) echo "usage: $0 [--repo NAME] [--lib PATH] [--strict]" >&2; exit 2 ;;
	esac
done
: "${FORGE_URL:?FORGE_URL is required}" "${FORGE_TOKEN:?FORGE_TOKEN is required}" "${FORGE_BUILDER_DIGEST:?FORGE_BUILDER_DIGEST is required (the builder image ID)}"
command -v curl >/dev/null || { echo "cran-linux-upload: curl is required" >&2; exit 2; }
command -v Rscript >/dev/null || { echo "cran-linux-upload: Rscript is required" >&2; exit 2; }

codename=$(. /etc/os-release && echo "${VERSION_CODENAME:-}")
arch=$(uname -m)
rminor=$(Rscript -e 'cat(R.version$major, sub("\\..*", "", R.version$minor), sep = ".")')
key="$codename-$arch/$rminor"
base="${FORGE_URL%/}/repository/$repo/bin/linux/$codename-$arch/contrib/$rminor"

# One line per package: name, version, library directory, LinkingTo versions.
list=$(LIB="$lib" Rscript -e '
lib <- Sys.getenv("LIB"); if (!nzchar(lib)) lib <- .libPaths()[1]
ip <- installed.packages(lib.loc = lib, fields = c("RemoteType", "LinkingTo"))
all <- installed.packages()
for (i in seq_len(nrow(ip))) {
  p <- ip[i, ]
  if (identical(p[["Priority"]], "base")) next
  # renv and pak record where a package came from; anything but a package
  # repository (github, gitlab, git, url, local, …) has no source in forge.
  rt <- p[["RemoteType"]]
  if (!is.na(rt) && !rt %in% c("standard", "repository")) next
  lt <- ""
  if (!is.na(p[["LinkingTo"]])) {
    deps <- trimws(sub("\\(.*", "", strsplit(p[["LinkingTo"]], ",")[[1]]))
    have <- deps[deps %in% rownames(all)]
    lt <- paste(paste(have, all[have, "Version"]), collapse = ", ")
  }
  cat(p[["Package"]], p[["Version"]], p[["LibPath"]], lt, sep = "\t"); cat("\n")
}')

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
uploaded=0 present=0 raced=0 failed=0
while IFS=$'\t' read -r pkg ver libpath linkingto; do
	[ -n "$pkg" ] || continue
	url="$base/${pkg}_${ver}.tar.gz"
	code=$(curl -s -o /dev/null -w '%{http_code}' -I -H "Authorization: Bearer $FORGE_TOKEN" "$url")
	if [ "$code" = 200 ]; then present=$((present+1)); continue; fi
	tar -czhf "$tmp/$pkg.tar.gz" -C "$libpath" "$pkg" || { echo "cran-linux-upload: could not pack $pkg" >&2; failed=$((failed+1)); continue; }
	code=$(curl -s -o "$tmp/resp" -w '%{http_code}' -X PUT --data-binary @"$tmp/$pkg.tar.gz" \
		-H "Authorization: Bearer $FORGE_TOKEN" -H "X-Forge-Builder-Digest: $FORGE_BUILDER_DIGEST" \
		-H "X-Forge-Linking-To: $linkingto" "$url")
	case "$code" in
	201) uploaded=$((uploaded+1)); echo "uploaded $pkg $ver ($key)" ;;
	409) raced=$((raced+1)) ;;
	*) failed=$((failed+1)); echo "cran-linux-upload: $pkg $ver refused ($code): $(head -c 300 "$tmp/resp")" >&2 ;;
	esac
done <<<"$list"

echo "cran-linux-upload: $key — $uploaded uploaded, $present already present, $raced uploaded by another builder, $failed failed"
[ "$failed" -eq 0 ] || [ "$strict" -eq 0 ]
