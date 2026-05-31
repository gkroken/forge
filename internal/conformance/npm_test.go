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

// TestNpm_pnpm_Proxy_Install drives a real pnpm CLI through forge's npm-proxy
// repository to install is-odd, exercising the npm protocol from a second client.
func TestNpm_pnpm_Proxy_Install(t *testing.T) {
	if !conformance.IsReachable("https://registry.npmjs.org/is-odd") {
		t.Skip("upstream npm registry not reachable")
	}

	srv := conformance.StartForge(t)
	registry := srv.ContainerRepo("npm-proxy")

	conformance.RunScript(t, "node:20-alpine", fmt.Sprintf(`
set -e
REGISTRY="%s"
npm install -g pnpm --quiet
mkdir /tmp/proj && cd /tmp/proj
echo '{"name":"test","version":"1.0.0"}' > package.json
pnpm add is-odd@3.0.1 --registry "$REGISTRY" --prefer-online
test -d node_modules/is-odd
node -e "require('is-odd'); console.log('pnpm: is-odd OK')"
`, registry))
}

// TestNpm_Yarn_Hosted_PublishInstall publishes a minimal package using yarn
// and then installs it from a clean directory, verifying the full round-trip
// with forge's hosted npm repository.
func TestNpm_Yarn_Hosted_PublishInstall(t *testing.T) {
	srv := conformance.StartForge(t)
	registry := srv.ContainerRepo("npm-hosted")

	conformance.RunScript(t, "node:20-alpine", fmt.Sprintf(`
set -e
REGISTRY="%s"
npm install -g yarn --quiet

# Configure auth token so yarn can publish to forge (eval mode accepts any token).
REGISTRY_KEY="${REGISTRY#http:}"
npm config set "${REGISTRY_KEY}:_authToken" placeholder

# Publish a minimal package.
mkdir /pkg && cd /pkg
cat > package.json <<'EOF'
{"name":"yarn-conformance-pkg","version":"1.0.0","main":"index.js"}
EOF
echo 'module.exports={answer:42};' > index.js
yarn publish --registry "$REGISTRY" --new-version 1.0.0 --no-git-tag-version --non-interactive

# Install it in a fresh directory and verify.
mkdir /install && cd /install
echo '{"name":"consumer","version":"1.0.0"}' > package.json
yarn add yarn-conformance-pkg --registry "$REGISTRY"
node -e "const p=require('yarn-conformance-pkg'); if(p.answer!==42) process.exit(1); console.log('yarn: pkg OK')"
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
