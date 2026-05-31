//go:build conformance

package conformance_test

import (
	"fmt"
	"testing"

	"forge/internal/conformance"
)

// orasImage is the OCI Registry AS Storage CLI — a lightweight OCI client that
// pushes and pulls arbitrary artifacts without requiring a Docker daemon.
const orasImage = "ghcr.io/oras-project/oras:v1.1.0"

// TestOCI_Hosted_PushPull pushes a small artifact to forge's OCI registry
// using oras and then pulls it back into a clean output directory.
//
// forge routes OCI at /v2/{repo-name}/{image}/… (not under /repository/).
// The registry authority is host.docker.internal:PORT and the image reference
// is docker-hosted/{name}:{tag}.
func TestOCI_Hosted_PushPull(t *testing.T) {
	srv := conformance.StartForge(t)
	// ContainerHost returns "host.docker.internal:PORT" — the OCI registry root.
	registry := srv.ContainerHost()

	conformance.RunScript(t, orasImage, fmt.Sprintf(`
set -e
REGISTRY="%s"
IMAGE="${REGISTRY}/docker-hosted/conformance-artifact:v1"

# Create a small test artifact.
echo "forge conformance payload" > /tmp/artifact.txt

# Push to forge (--plain-http: forge runs without TLS in eval mode).
oras push --plain-http "${IMAGE}" /tmp/artifact.txt

# Pull back into a clean output directory and verify content.
mkdir /tmp/pulled
oras pull --plain-http --output /tmp/pulled "${IMAGE}"
test -f /tmp/pulled/artifact.txt
grep "forge conformance payload" /tmp/pulled/artifact.txt
`, registry))
}
