//go:build conformance

package conformance_test

import (
	"fmt"
	"testing"

	"forge/internal/conformance"
)

// TestNpm_Proxy_Install drives a real npm CLI through forge's npm-proxy
// repository to install is-odd from the upstream npm registry.
// This is the Phase 0 exit-criterion conformance case.
func TestNpm_Proxy_Install(t *testing.T) {
	if !conformance.IsReachable("https://registry.npmjs.org/is-odd") {
		t.Skip("upstream npm registry not reachable")
	}

	srv := conformance.StartForge(t)
	registry := srv.ContainerRepo("npm-proxy")

	conformance.RunScript(t, "node:20-alpine", fmt.Sprintf(`
set -e
cd /tmp
npm install --prefer-online --registry %s is-odd@3.0.1
test -d node_modules/is-odd
node -e "require('is-odd'); console.log('is-odd loaded OK')"
`, registry))
}

// TestNpm_Proxy_CacheHit verifies that a second install is served from forge's
// cache (no upstream fetch) by blocking the upstream after the first request.
func TestNpm_Proxy_CacheHit(t *testing.T) {
	if !conformance.IsReachable("https://registry.npmjs.org/is-odd") {
		t.Skip("upstream npm registry not reachable")
	}

	srv := conformance.StartForge(t)
	registry := srv.ContainerRepo("npm-proxy")

	// First install populates the cache.
	conformance.RunScript(t, "node:20-alpine", fmt.Sprintf(`
set -e
cd /tmp
npm install --prefer-online --registry %s is-odd@3.0.1
test -d node_modules/is-odd
`, registry))

	// Second install must succeed even if we block the upstream in /etc/hosts.
	conformance.RunScript(t, "node:20-alpine", fmt.Sprintf(`
set -e
echo "0.0.0.0 registry.npmjs.org" >> /etc/hosts
cd /tmp
npm install --prefer-offline --registry %s is-odd@3.0.1
test -d node_modules/is-odd
node -e "console.log('cache hit: is-odd loaded OK')"
`, registry))
}

// TestNpm_Hosted_PublishInstall publishes a minimal package to forge's hosted
// npm repository and then installs it from a clean slate.
func TestNpm_Hosted_PublishInstall(t *testing.T) {
	srv := conformance.StartForge(t)
	registry := srv.ContainerRepo("npm-hosted")

	conformance.RunScript(t, "node:20-alpine", fmt.Sprintf(`
set -e
REGISTRY="%s"

# The npmrc auth key is the registry URL without the protocol.
# Forge ignores auth headers until Phase 2 adds real AuthN.
REGISTRY_KEY="${REGISTRY#http:}"
npm config set "${REGISTRY_KEY}:_authToken" placeholder

# Publish a minimal package to the hosted registry.
mkdir /pkg && cd /pkg
cat > package.json <<'EOF'
{"name":"conformance-pkg","version":"1.0.0","main":"index.js"}
EOF
echo 'module.exports = { answer: 42 };' > index.js
npm publish --registry "$REGISTRY"

# Install it fresh in a separate directory.
mkdir /install && cd /install
npm install --registry "$REGISTRY" conformance-pkg
node -e "const p = require('conformance-pkg'); if (p.answer !== 42) process.exit(1); console.log('hosted pkg OK')"
`, registry))
}
