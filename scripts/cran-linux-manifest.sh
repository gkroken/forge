#!/bin/bash
# cran-linux-manifest.sh — print the CRAN Linux platform registration for a
# builder image (WORKPLAN-CRAN-LINUX.md, internal/cranplatform).
#
#   scripts/cran-linux-manifest.sh IMAGE > platform.json
#   curl -X PUT -H "Authorization: Bearer $ADMIN" --data @platform.json \
#        "$FORGE/api/v1/cran/platforms/$(jq -r .key platform.json)"
#
# or paste the object into the cranPlatforms section of the config file.
#
# Runs on a host with docker. The key, R platform, glibc version and library
# manifest are read inside IMAGE; builderDigest is the image ID
# (docker image inspect -f '{{.Id}}') — identical on every host for the same
# image, and what the upload helper must send as X-Forge-Builder-Digest.
#
# The manifest is every soname the dynamic loader can find in the image:
# `ldconfig -p`, plus the directories R adds to LD_LIBRARY_PATH at startup
# (etc/ldpaths) — ldconfig does not list R's own lib directory, and every
# package with compiled code needs libR.so from it.
set -euo pipefail

if [ "${1:-}" = "--inside" ]; then
	codename=$(. /etc/os-release && echo "${VERSION_CODENAME:-}")
	[ -n "$codename" ] || { echo "no VERSION_CODENAME in /etc/os-release" >&2; exit 1; }
	arch=$(uname -m)
	rminor=$(Rscript -e 'cat(R.version$major, sub("\\..*", "", R.version$minor), sep = ".")')
	rplatform=$(Rscript -e 'cat(R.version$platform)')
	glibc=$(getconf GNU_LIBC_VERSION | awk '{print $2}')
	libs=$(
		{
			ldconfig -p | awk 'NR > 1 { print $1 }'
			Rscript -e 'cat(strsplit(Sys.getenv("LD_LIBRARY_PATH"), ":")[[1]], sep = "\n")' |
				while read -r d; do if [ -d "$d" ]; then ls -1 "$d"; fi; done
		} | grep -E '^[A-Za-z0-9._+-]+\.so(\.[0-9A-Za-z._-]+)?$' | sort -u
	)
	printf '{\n  "key": "%s-%s/%s",\n  "rPlatform": "%s",\n  "builderImage": "%s",\n  "builderDigest": "%s",\n  "maxGlibc": "%s",\n  "libraries": [\n' \
		"$codename" "$arch" "$rminor" "$rplatform" "$2" "$3" "$glibc"
	printf '%s\n' "$libs" | sed 's/.*/    "&"/; $!s/$/,/'
	printf '  ]\n}\n'
	exit 0
fi

image=${1:?usage: $0 IMAGE}
digest=$(docker image inspect -f '{{.Id}}' "$image" 2>/dev/null) || {
	docker pull -q "$image" >/dev/null
	digest=$(docker image inspect -f '{{.Id}}' "$image")
}
# The script goes in as an argument, not on stdin: Rscript would swallow the
# rest of a script that bash is still reading from stdin.
docker run --rm --entrypoint bash "$image" -c "$(cat "$0")" manifest --inside "$image" "$digest"
