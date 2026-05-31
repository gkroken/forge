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
# Remove the locally-packaged file so the test -f below proves the pull worked.
rm myapp-0.1.0.tgz
helm repo add forge "${REPO}"
helm repo update forge
helm pull forge/myapp --version 0.1.0
test -f myapp-0.1.0.tgz
`, repo))
}

// TestHelm_OCI_PushPull pushes a Helm chart to forge's OCI registry using the
// oci:// scheme (helm push / helm pull oci://) and verifies the full round-trip.
// This exercises forge's OCI Distribution API handler from the Helm client,
// which uses different media types (application/vnd.cncf.helm.*) from Docker images.
// Requires Helm 3.14+ for --plain-http support; alpine/helm:3 satisfies this.
func TestHelm_OCI_PushPull(t *testing.T) {
	srv := conformance.StartForge(t)
	registry := srv.ContainerHost()

	conformance.RunScript(t, helmImage, fmt.Sprintf(`
set -e
REGISTRY="%s"

apk add -q --no-cache curl

helm create oci-chart
helm package oci-chart

# Push to forge's OCI registry using the oci:// scheme.
# --plain-http: forge runs without TLS in eval mode (requires Helm 3.14+).
helm push oci-chart-0.1.0.tgz oci://${REGISTRY}/docker-hosted --plain-http

# Verify the manifest is accessible directly via the OCI API.
curl -sf "http://${REGISTRY}/v2/docker-hosted/oci-chart/manifests/0.1.0" \
  -H "Accept: application/vnd.oci.image.manifest.v1+json" | grep schemaVersion

# Pull back from forge and verify the tarball is present.
rm oci-chart-0.1.0.tgz
helm pull oci://${REGISTRY}/docker-hosted/oci-chart --version 0.1.0 --plain-http
test -f oci-chart-0.1.0.tgz
`, registry))
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
rm grpchart-0.1.0.tgz
helm repo add forge-public "${GROUP}"
helm repo update forge-public
helm pull forge-public/grpchart --version 0.1.0
test -f grpchart-0.1.0.tgz
`, hosted, group))
}
