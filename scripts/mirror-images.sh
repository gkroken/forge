#!/bin/bash
# mirror-images.sh — copy the third-party images forge's charts deploy into
# forge's registry, byte for byte (same digest, every architecture).
#
#   DEST=ghcr.io/gkroken scripts/mirror-images.sh        # needs crane, logged in
#
# Prints one mirrored reference per line (DEST/path:tag@digest), for the scan
# and signing steps that follow. Copying an image that is already there is a
# no-op, so this is safe to run on every build.
set -euo pipefail
: "${DEST:?DEST is required, e.g. ghcr.io/gkroken}"
list="$(dirname "$0")/../deploy/third-party-images.txt"
grep -vE '^\s*(#|$)' "$list" | while read -r path tag digest; do
	crane copy --platform all "docker.io/$path@$digest" "$DEST/$path:$tag" >&2
	got=$(crane digest "$DEST/$path:$tag")
	if [ "$got" != "$digest" ]; then
		echo "mirror-images: $DEST/$path:$tag is $got, expected $digest" >&2
		exit 1
	fi
	echo "$DEST/$path:$tag@$digest"
done
