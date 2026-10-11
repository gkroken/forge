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
#   scripts/cran-linux-manifest.sh --digest IMAGE
#
# prints only builderDigest: what the upload helper must send as
# X-Forge-Builder-Digest.
#
# Runs on a host with docker. The key, R platform, glibc version and library
# manifest are read inside IMAGE. builderDigest is the image's CONFIG digest —
# the config.digest its registry manifest names, so the same on every host.
# It is not always the image ID: `docker image inspect -f '{{.Id}}'` is the
# config digest on Docker's classic image store but the manifest or index
# digest on the containerd image store (the default from Docker Engine 29), so
# a registration and an upload computed on the two kinds of host would never
# match. On the containerd store the digest is read from `docker save`'s
# manifest.json instead, which names the config on both stores.
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

# config_digest IMAGE prints IMAGE's config digest (see above).
config_digest() {
	# Captured, not piped into grep -q: under pipefail, grep exiting at its
	# first match can fail docker with SIGPIPE, and that would read as
	# "classic store" and print the wrong digest. Checked explicitly for the
	# same reason: set -e does not reach inside the caller's $(…).
	local status
	status=$(docker info -f '{{json .DriverStatus}}') || {
		echo "cran-linux-manifest: docker info failed, so the image store (and the digest) is unknown" >&2
		return 1
	}
	if [[ $status != *io.containerd.snapshotter* ]]; then
		docker image inspect -f '{{.Id}}' "$1"
		return
	fi
	local manifest configs
	manifest=$(docker save "$1" | tar -xO manifest.json) || {
		echo "cran-linux-manifest: docker save $1 failed — is it present locally for this host's platform?" >&2
		return 1
	}
	configs=$(printf '%s' "$manifest" | grep -oE '"Config":"(blobs/sha256/)?[0-9a-f]{64}(\.json)?"' |
		grep -oE '[0-9a-f]{64}')
	if [ "$(printf '%s\n' "$configs" | grep -c .)" != 1 ]; then
		echo "cran-linux-manifest: docker save's manifest.json for $1 does not name exactly one config: $manifest" >&2
		return 1
	fi
	echo "sha256:$configs"
}

digest_only=0
if [ "${1:-}" = "--digest" ]; then digest_only=1; shift; fi
image=${1:?usage: $0 [--digest] IMAGE}
docker image inspect "$image" >/dev/null 2>&1 || docker pull -q "$image" >/dev/null
digest=$(config_digest "$image")
if [ "$digest_only" = 1 ]; then
	echo "$digest"
	exit 0
fi
# The script goes in as an argument, not on stdin: Rscript would swallow the
# rest of a script that bash is still reading from stdin.
docker run --rm --entrypoint bash "$image" -c "$(cat "$0")" manifest --inside "$image" "$digest"
