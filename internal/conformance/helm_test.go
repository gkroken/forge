//go:build conformance

package conformance_test

import (
	"fmt"
	"testing"

	"forge/internal/conformance"
)

const helmImage = "alpine/helm:3"

// TestHelm_Hosted_PushPull creates a minimal chart, uploads it to the hosted
// Helm repository via the ChartMuseum-compatible POST /api/charts endpoint,
// then adds the repository to Helm and pulls the chart back to confirm the
// full upload-index-download round-trip.
func TestHelm_Hosted_PushPull(t *testing.T) {
	srv := conformance.StartForge(t)
	repo := srv.ContainerRepo("helm-hosted")

	conformance.RunScript(t, helmImage, fmt.Sprintf(`
set -e
REPO="%s"

apk add -q --no-cache curl

# Create and package a minimal chart (produces myapp-0.1.0.tgz).
helm create myapp
helm package myapp

# Upload to forge as a raw tgz body (forge reads r.Body directly, not multipart).
curl -sf -X POST \
  -H "Content-Type: application/gzip" \
  --data-binary @myapp-0.1.0.tgz \
  "${REPO}api/charts" | grep '"saved":true'

# index.yaml must list the uploaded chart.
curl -sf "${REPO}index.yaml" | grep 'name: myapp'

# Add the repository to Helm and pull the chart from a clean cache.
helm repo add forge "${REPO}"
helm repo update forge
helm pull forge/myapp --version 0.1.0 --destination /tmp/dl
test -f /tmp/dl/myapp-0.1.0.tgz
`, repo))
}

// TestHelm_Group_Resolve pushes a chart to the hosted Helm repository and
// verifies that it is visible and downloadable through the helm-public group
// repository (which fans out to helm-hosted).
func TestHelm_Group_Resolve(t *testing.T) {
	srv := conformance.StartForge(t)
	hosted := srv.ContainerRepo("helm-hosted")
	group := srv.ContainerRepo("helm-public")

	conformance.RunScript(t, helmImage, fmt.Sprintf(`
set -e
HOSTED="%s"
GROUP="%s"

apk add -q --no-cache curl

# Push a chart to the hosted repo.
helm create grpchart
helm package grpchart

curl -sf -X POST \
  -H "Content-Type: application/gzip" \
  --data-binary @grpchart-0.1.0.tgz \
  "${HOSTED}api/charts" | grep '"saved":true'

# The group's index.yaml must include the chart from its hosted member.
curl -sf "${GROUP}index.yaml" | grep 'name: grpchart'

# Pull through the group repository.
helm repo add forge-public "${GROUP}"
helm repo update forge-public
helm pull forge-public/grpchart --version 0.1.0 --destination /tmp/dl
test -f /tmp/dl/grpchart-0.1.0.tgz
`, hosted, group))
}
