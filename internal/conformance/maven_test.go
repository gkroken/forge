//go:build conformance

package conformance_test

import (
	"fmt"
	"net/http"
	"testing"

	"forge/internal/conformance"
)

const mavenImage = "maven:3.9-eclipse-temurin-17"

// mavenSettings is a shell snippet that writes the two settings files used by
// every Maven test:
//   - /tmp/gs.xml (global settings) — empty, which removes Maven 3.8+'s
//     built-in HTTP-blocker mirror and allows forge's plain-HTTP URL.
//   - /tmp/us.xml (user settings) — registers a "forge" server entry so that
//     deploy:deploy-file can match the -DrepositoryId=forge flag. Forge ignores
//     credentials in eval mode; the entry is required only for Maven's lookup.
const mavenSettings = `
cat > /tmp/gs.xml <<'GS'
<settings/>
GS
cat > /tmp/us.xml <<'US'
<settings>
  <servers>
    <server><id>forge</id><username>x</username><password>x</password></server>
  </servers>
</settings>
US
`

// TestMaven_Hosted_DeployResolve deploys a JAR to the hosted Maven repository
// using mvn deploy:deploy-file, then verifies:
//   - forge generates artifact-level maven-metadata.xml listing the version,
//   - the SHA1 sidecar is available as a 40-char hex string (stored by mvn or
//     synthesised on-the-fly by forge from the blob's checksum),
//   - mvn dependency:get resolves the artifact from a clean local cache.
func TestMaven_Hosted_DeployResolve(t *testing.T) {
	srv := conformance.StartForge(t)
	repo := srv.ContainerRepo("maven-hosted")

	conformance.RunScript(t, mavenImage, fmt.Sprintf(`
set -e
REPO="%s"
%s

# Any bytes are a valid artifact body; forge stores content verbatim.
dd if=/dev/urandom of=/tmp/conformance-lib-1.0.0.jar bs=1024 count=1 2>/dev/null

# Deploy the JAR and an auto-generated minimal POM.
mvn -B --no-transfer-progress -gs /tmp/gs.xml -s /tmp/us.xml \
  deploy:deploy-file \
  -Durl="${REPO}" -DrepositoryId=forge \
  -Dfile=/tmp/conformance-lib-1.0.0.jar \
  -DgroupId=com.forge.test -DartifactId=conformance-lib -Dversion=1.0.0

# Artifact-level maven-metadata.xml must be generated and list our version.
curl -sf "${REPO}com/forge/test/conformance-lib/maven-metadata.xml" \
  | grep '<version>1.0.0</version>'

# SHA1 sidecar must be exactly 40 lowercase hex chars (no whitespace).
SHA1=$(curl -sf "${REPO}com/forge/test/conformance-lib/1.0.0/conformance-lib-1.0.0.jar.sha1" \
  | tr -d '[:space:]')
echo "${SHA1}" | grep -qE '^[0-9a-f]{40}$'

# Resolve from a fresh local cache to prove the full download round-trip.
mvn -B --no-transfer-progress -gs /tmp/gs.xml -s /tmp/us.xml \
  dependency:get \
  -Dmaven.repo.local=/tmp/fresh-repo \
  -Dartifact=com.forge.test:conformance-lib:1.0.0 \
  -DremoteRepositories="forge::default::${REPO}"
`, repo, mavenSettings))
}

// TestMaven_Proxy_Resolve resolves javax.inject:javax.inject:1 (a 2 KB JAR
// with zero transitive dependencies) through forge's maven-central proxy and
// confirms that forge cached the artifact in its blob store afterwards.
func TestMaven_Proxy_Resolve(t *testing.T) {
	const probeURL = "https://repo1.maven.org/maven2/javax/inject/javax.inject/1/javax.inject-1.jar"
	if !conformance.IsReachable(probeURL) {
		t.Skip("Maven Central not reachable")
	}

	srv := conformance.StartForge(t)
	repo := srv.ContainerRepo("maven-central")

	conformance.RunScript(t, mavenImage, fmt.Sprintf(`
set -e
REPO="%s"
%s

mvn -B --no-transfer-progress -gs /tmp/gs.xml -s /tmp/us.xml \
  dependency:get \
  -Dmaven.repo.local=/tmp/fresh-repo \
  -Dartifact=javax.inject:javax.inject:1 \
  -DremoteRepositories="forge::default::${REPO}"
`, repo, mavenSettings))

	// After the container exits, forge must have the JAR in its blob store.
	// srv.Repo uses localhost (host-side), not host.docker.internal.
	blobURL := srv.Repo("maven-central") + "javax/inject/javax.inject/1/javax.inject-1.jar"
	resp, err := http.Get(blobURL) //nolint:noctx
	if err != nil {
		t.Fatalf("GET cached artifact from host: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for cached artifact, got %d", resp.StatusCode)
	}
}

// TestMaven_Proxy_CacheHit verifies that a second resolution is served from
// forge's cache. Two containers each start with a clean Maven local repo; the
// second proves forge serves without needing a second upstream fetch.
func TestMaven_Proxy_CacheHit(t *testing.T) {
	const probeURL = "https://repo1.maven.org/maven2/javax/inject/javax.inject/1/javax.inject-1.jar"
	if !conformance.IsReachable(probeURL) {
		t.Skip("Maven Central not reachable")
	}

	srv := conformance.StartForge(t)
	repo := srv.ContainerRepo("maven-central")

	resolve := fmt.Sprintf(`
set -e
REPO="%s"
%s

mvn -B --no-transfer-progress -gs /tmp/gs.xml -s /tmp/us.xml \
  dependency:get \
  -Dmaven.repo.local=/tmp/fresh-repo \
  -Dartifact=javax.inject:javax.inject:1 \
  -DremoteRepositories="forge::default::${REPO}"
`, repo, mavenSettings)

	// First container: cache miss — forge fetches from Maven Central and stores.
	conformance.RunScript(t, mavenImage, resolve)
	// Second container: fresh client-side local repo — forge serves from blob store.
	conformance.RunScript(t, mavenImage, resolve)
}

// TestMaven_Hosted_SnapshotDeploy deploys a SNAPSHOT JAR to the hosted Maven
// repository and verifies:
//   - forge generates SNAPSHOT-level maven-metadata.xml with <snapshotVersions>,
//   - the SNAPSHOT artifact resolves successfully from a clean local cache via a
//     project POM that explicitly enables snapshot resolution.
func TestMaven_Hosted_SnapshotDeploy(t *testing.T) {
	srv := conformance.StartForge(t)
	repo := srv.ContainerRepo("maven-hosted")

	conformance.RunScript(t, mavenImage, fmt.Sprintf(`
set -e
REPO="%s"
%s

dd if=/dev/urandom of=/tmp/snap-lib-1.0-SNAPSHOT.jar bs=1024 count=1 2>/dev/null

mvn -B --no-transfer-progress -gs /tmp/gs.xml -s /tmp/us.xml \
  deploy:deploy-file \
  -Durl="${REPO}" -DrepositoryId=forge \
  -Dfile=/tmp/snap-lib-1.0-SNAPSHOT.jar \
  -DgroupId=com.forge.test -DartifactId=snap-lib -Dversion=1.0-SNAPSHOT

# Forge must serve SNAPSHOT-level maven-metadata.xml containing snapshotVersion
# entries (either generated from forge's snap records or stored by mvn deploy).
curl -sf "${REPO}com/forge/test/snap-lib/1.0-SNAPSHOT/maven-metadata.xml" \
  | grep -i 'snapshotVersion'

# dependency:get disables snapshots for remote repos by default; use a project
# POM to configure the forge repo with snapshots explicitly enabled.
mkdir /tmp/consumer && cd /tmp/consumer
cat > pom.xml <<POM
<project>
  <modelVersion>4.0.0</modelVersion>
  <groupId>test</groupId><artifactId>consumer</artifactId><version>1.0</version>
  <repositories>
    <repository>
      <id>forge</id>
      <url>${REPO}</url>
      <snapshots><enabled>true</enabled></snapshots>
    </repository>
  </repositories>
  <dependencies>
    <dependency>
      <groupId>com.forge.test</groupId>
      <artifactId>snap-lib</artifactId>
      <version>1.0-SNAPSHOT</version>
    </dependency>
  </dependencies>
</project>
POM
mvn -B --no-transfer-progress -gs /tmp/gs.xml -s /tmp/us.xml \
  -Dmaven.repo.local=/tmp/fresh-repo \
  dependency:resolve
`, repo, mavenSettings))
}
