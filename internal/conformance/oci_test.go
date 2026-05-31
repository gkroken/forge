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

// craneImage is the Google go-containerregistry crane CLI — a daemon-free OCI
// client that complements oras with a different protocol implementation.
// :debug variant is busybox-based and has /bin/sh, required by RunScript.
// :latest is a scratch image with no shell and cannot be used with RunScript.
const craneImage = "gcr.io/go-containerregistry/crane:debug"

// TestOCI_Crane_PushPull copies a small public image (hello-world ~13 KB) from
// Docker Hub into forge's OCI registry using crane, then pulls it back to verify
// forge serves it correctly. Tests the OCI Distribution API from a second client.
func TestOCI_Crane_PushPull(t *testing.T) {
	if !conformance.IsReachable("https://index.docker.io/v2/") {
		t.Skip("Docker Hub not reachable (needed to source hello-world image)")
	}

	srv := conformance.StartForge(t)
	registry := srv.ContainerHost()

	conformance.RunScript(t, craneImage, fmt.Sprintf(`
set -e
REGISTRY="%s"

# Copy the smallest standard Docker image into forge.
crane copy --insecure hello-world:latest ${REGISTRY}/docker-hosted/crane-hello:latest

# Verify the manifest is present.
crane manifest --insecure ${REGISTRY}/docker-hosted/crane-hello:latest | grep schemaVersion

# Pull back to confirm forge serves the image correctly.
crane pull --insecure ${REGISTRY}/docker-hosted/crane-hello:latest /tmp/crane-hello.tar
test -f /tmp/crane-hello.tar
`, registry))
}

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

# oras rejects absolute paths; work from a temp directory with relative paths.
mkdir /tmp/push && cd /tmp/push
echo "forge conformance payload" > artifact.txt

# Push to forge (--plain-http: forge runs without TLS in eval mode).
oras push --plain-http "${IMAGE}" artifact.txt

# Pull back into a separate clean directory and verify content.
mkdir /tmp/pull
oras pull --plain-http --output /tmp/pull "${IMAGE}"
test -f /tmp/pull/artifact.txt
grep "forge conformance payload" /tmp/pull/artifact.txt
`, registry))
}
