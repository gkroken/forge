//go:build conformance

package conformance_test

import (
	"fmt"
	"testing"

	"forge/internal/conformance"
)

const cranImage = "r-base:4.4.0"

// TestCRAN_Hosted_PublishInstall uploads a minimal pure-R source package to
// the hosted CRAN repository, verifies that forge generates a correct PACKAGES
// index listing the package, and then installs it with R's install.packages
// from a clean library directory.
func TestCRAN_Hosted_PublishInstall(t *testing.T) {
	srv := conformance.StartForge(t)
	repo := srv.ContainerRepo("cran-hosted")

	conformance.RunScript(t, cranImage, fmt.Sprintf(`
set -e
REPO="%s"

# curl is needed for the PUT upload (r-base is Debian-based).
apt-get update -qq && apt-get install -y -qq --no-install-recommends curl

# Build a minimal pure-R source package (no C code; no compilation needed).
mkdir -p /tmp/mypackage/R

cat > /tmp/mypackage/DESCRIPTION <<'DESC'
Package: mypackage
Version: 1.0.0
Title: Conformance Test Package
Description: A minimal pure-R package for forge conformance testing.
License: MIT
Encoding: UTF-8
DESC

echo 'greet <- function() cat("hello from mypackage\n")' > /tmp/mypackage/R/greet.R
# R 4.x requires a NAMESPACE file; exportPattern exports all visible functions.
echo "exportPattern('.')" > /tmp/mypackage/NAMESPACE

cd /tmp && tar czf mypackage_1.0.0.tar.gz mypackage/

# Upload to the hosted repo via a raw-body PUT (forge reads r.Body directly).
curl -sf -X PUT \
  --data-binary @mypackage_1.0.0.tar.gz \
  "${REPO}src/contrib/mypackage_1.0.0.tar.gz"

# PACKAGES index must list the uploaded package (generated from meta records).
curl -sf "${REPO}src/contrib/PACKAGES" | grep 'Package: mypackage'

# Install from forge into a fresh library directory and call a function.
mkdir -p /tmp/rlib
Rscript -e "
  install.packages('mypackage', repos='${REPO}', type='source',
                   lib='/tmp/rlib', quiet=TRUE)
  library(mypackage, lib.loc='/tmp/rlib')
  greet()
"
`, repo))
}

// cranBuildScript is the shell portion that creates and uploads a minimal
// pure-R source package to a hosted CRAN repo. Requires $REPO to be set.
// Used by the pak and renv tests so they can share the upload step.
const cranBuildScript = `
apt-get update -qq && apt-get install -y -qq --no-install-recommends curl

mkdir -p /tmp/mypackage/R
cat > /tmp/mypackage/DESCRIPTION <<'DESC'
Package: mypackage
Version: 1.0.0
Title: Conformance Test Package
Description: Minimal pure-R package for forge conformance testing.
License: MIT
Encoding: UTF-8
DESC
echo 'greet <- function() cat("hello from mypackage\n")' > /tmp/mypackage/R/greet.R
echo "exportPattern('.')" > /tmp/mypackage/NAMESPACE
cd /tmp && tar czf mypackage_1.0.0.tar.gz mypackage/
curl -sf -X PUT --data-binary @mypackage_1.0.0.tar.gz \
  "${REPO}src/contrib/mypackage_1.0.0.tar.gz"
`

// TestCRAN_pak_Hosted_Install uploads a minimal package to the hosted CRAN
// repository, then installs it using pak::pkg_install to verify that pak
// can consume forge as a CRAN-compatible source.
func TestCRAN_pak_Hosted_Install(t *testing.T) {
	if !conformance.IsReachable("https://cloud.r-project.org") {
		t.Skip("CRAN not reachable (needed to install pak)")
	}

	srv := conformance.StartForge(t)
	repo := srv.ContainerRepo("cran-hosted")

	conformance.RunScript(t, cranImage, fmt.Sprintf(`
set -e
REPO="%s"
%s
Rscript -e "
  install.packages('pak', repos='https://cloud.r-project.org', quiet=TRUE)
  options(repos=c(CRAN='${REPO}'))
  pak::pkg_install('mypackage', lib='/tmp/paklib', ask=FALSE)
  library(mypackage, lib.loc='/tmp/paklib')
  greet()
"
`, repo, cranBuildScript))
}

// TestCRAN_renv_Hosted_Install uploads a minimal package to the hosted CRAN
// repository, then installs it using renv::install to verify that renv
// can consume forge as a CRAN-compatible source.
func TestCRAN_renv_Hosted_Install(t *testing.T) {
	if !conformance.IsReachable("https://cloud.r-project.org") {
		t.Skip("CRAN not reachable (needed to install renv)")
	}

	srv := conformance.StartForge(t)
	repo := srv.ContainerRepo("cran-hosted")

	conformance.RunScript(t, cranImage, fmt.Sprintf(`
set -e
REPO="%s"
%s
Rscript -e "
  install.packages('renv', repos='https://cloud.r-project.org', quiet=TRUE)
  options(repos=c(CRAN='${REPO}'))
  renv::install('mypackage', library='/tmp/renvlib', prompt=FALSE)
  library(mypackage, lib.loc='/tmp/renvlib')
  greet()
"
`, repo, cranBuildScript))
}

// TestCRAN_Proxy_Download fetches a small pure-R package tarball through
// forge's cran-proxy repository using R's download.file, verifying that the
// proxy correctly fetches and caches the artifact from upstream CRAN.
//
// Note: forge's CRAN proxy serves individual tarballs on demand but generates
// the PACKAGES index from locally cached artifacts only. This test uses
// download.file (direct tarball fetch) rather than install.packages to
// exercise the proxy without requiring a pre-populated PACKAGES index.
func TestCRAN_Proxy_Download(t *testing.T) {
	const probeURL = "https://cran.r-project.org/src/contrib/R.methodsS3_1.8.2.tar.gz"
	if !conformance.IsReachable(probeURL) {
		t.Skip("CRAN not reachable")
	}

	srv := conformance.StartForge(t)
	repo := srv.ContainerRepo("cran-proxy")

	conformance.RunScript(t, cranImage, fmt.Sprintf(`
set -e
REPO="%s"

# R.methodsS3 is a pure-R package (~28 KB) with minimal dependencies.
# download.file bypasses PACKAGES discovery and directly tests proxy fetch+cache.
Rscript -e "
  dest <- '/tmp/R.methodsS3_1.8.2.tar.gz'
  download.file(
    paste0('${REPO}', 'src/contrib/R.methodsS3_1.8.2.tar.gz'),
    dest, quiet = TRUE
  )
  stopifnot(file.exists(dest), file.size(dest) > 1000L)
  cat('CRAN proxy download OK\n')
"
`, repo))
}
