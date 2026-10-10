#!/bin/bash
# check-chart-images.sh — every image forge's charts deploy must be forge's own
# or a third-party image listed in deploy/third-party-images.txt, pulled from
# ghcr.io/gkroken by its exact tag and digest. A chart value bumped without
# the list (so CI never mirrors, scans or signs it), or an image added
# unpinned, fails here.
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
allowed=$(grep -vE '^\s*(#|$)' "$root/deploy/third-party-images.txt" | awk '{print "ghcr.io/gkroken/" $1 ":" $2 "@" $3}')
helm dependency build "$root/deploy/helm/forge-stack" >/dev/null
images=$( { helm template forge "$root/deploy/helm/forge" --set 'waitFor[0].name=x' --set 'waitFor[0].host=x' --set 'waitFor[0].port=1'
            helm template forge-stack "$root/deploy/helm/forge-stack"; } |
	sed -n 's/^[[:space:]]*image:[[:space:]]*//p' | tr -d '"' | sort -u)
bad=0
for img in $images; do
	case "$img" in
	ghcr.io/gkroken/forge:*) continue ;;
	esac
	if ! grep -qxF "$img" <<<"$allowed"; then
		echo "check-chart-images: $img is not forge's own image or an exact entry of deploy/third-party-images.txt" >&2
		bad=1
	fi
done
[ "$bad" -eq 0 ] && echo "check-chart-images: $(wc -w <<<"$images") images, all forge's or mirrored by digest"
exit $bad
