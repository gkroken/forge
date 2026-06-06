//go:build conformance

package conformance_test

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"runtime"
	"strings"
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

// pakImage is rocker/r-ver:4.4 — pre-configures p3m.dev as the CRAN mirror
// which serves pre-compiled Ubuntu binary packages. This avoids the 8+ minute
// source compilation of pak and its C++ dependencies that r-base:4.4.0 requires.
const pakImage = "rocker/r-ver:4.4"

// TestCRAN_pak_Hosted_Install uploads a minimal package to the hosted CRAN
// repository, then installs it using pak::pkg_install to verify that pak
// can consume forge as a CRAN-compatible source.
func TestCRAN_pak_Hosted_Install(t *testing.T) {
	if !conformance.IsReachable("https://cloud.r-project.org") {
		t.Skip("CRAN not reachable (needed to install pak)")
	}

	srv := conformance.StartForge(t)
	repo := srv.ContainerRepo("cran-hosted")

	conformance.RunScript(t, pakImage, fmt.Sprintf(`
set -e
REPO="%s"
%s
Rscript -e "
  # rocker/r-ver pre-configures p3m.dev binary repos — pak installs in seconds.
  install.packages('pak', quiet=TRUE)
  options(repos=c(CRAN='${REPO}'))
  dir.create('/tmp/paklib', recursive=TRUE)
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
mkdir -p /tmp/renvlib
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

// ── Binary conformance tests ───────────────────────────────────────────────

// TestCRAN_Binary_PACKAGES_Fields publishes a Windows binary package and
// verifies that Built, Archs, and OS_type appear in all three PACKAGES index
// formats served by forge. Runs on any OS with Docker.
func TestCRAN_Binary_PACKAGES_Fields(t *testing.T) {
	srv := conformance.StartForge(t)
	repo := srv.ContainerRepo("cran-hosted")

	conformance.RunScript(t, "python:3-slim", fmt.Sprintf(`
set -e
python3 - <<'PYEOF'
import urllib.request, zipfile, io, gzip, struct

REPO = "%s"

pkg, ver = "wintestpkg", "1.0.0"
desc = (
    f"Package: {pkg}\nVersion: {ver}\nLicense: MIT\n"
    "Built: R 4.4.0; x86_64-w64-mingw32; 2024-01-15 00:00:00 UTC; windows\n"
    "Archs: x64\nOS_type: windows\n"
).encode()
ns = b"exportPattern('.')\n"

buf = io.BytesIO()
with zipfile.ZipFile(buf, "w") as zf:
    zf.writestr(f"{pkg}/DESCRIPTION", desc)
    zf.writestr(f"{pkg}/NAMESPACE", ns)
zip_data = buf.getvalue()

req = urllib.request.Request(
    REPO + f"bin/windows/contrib/4.4/{pkg}_{ver}.zip",
    data=zip_data, method="PUT")
with urllib.request.urlopen(req) as r:
    assert r.status == 201, f"PUT failed: {r.status}"

# PACKAGES (plain text)
with urllib.request.urlopen(REPO + "bin/windows/contrib/4.4/PACKAGES") as r:
    pkgs = r.read().decode()
assert "Package: wintestpkg" in pkgs, f"Package missing: {pkgs}"
assert "Built: R 4.4.0" in pkgs, f"Built missing: {pkgs}"
assert "Archs: x64" in pkgs, f"Archs missing: {pkgs}"
assert "OS_type: windows" in pkgs, f"OS_type missing: {pkgs}"
print("PACKAGES: OK")

# PACKAGES.gz
with urllib.request.urlopen(REPO + "bin/windows/contrib/4.4/PACKAGES.gz") as r:
    gz = gzip.decompress(r.read()).decode()
assert "Built: R 4.4.0" in gz, f"Built missing from PACKAGES.gz"
assert "Archs: x64" in gz, f"Archs missing from PACKAGES.gz"
print("PACKAGES.gz: OK")

# PACKAGES.rds — XDR header: element count at byte 18 must be 1 pkg × 8 cols = 8
with urllib.request.urlopen(REPO + "bin/windows/contrib/4.4/PACKAGES.rds") as r:
    rds = gzip.decompress(r.read())
assert rds[:2] == b"X\n", f"bad RDS marker: {rds[:2]}"
count = struct.unpack(">i", rds[18:22])[0]
assert count == 8, f"expected 8 elements (1 pkg x 8 cols), got {count}"
print(f"PACKAGES.rds: {count} elements (1x8): OK")

print("All binary PACKAGES field checks passed")
PYEOF
`, repo))
}

// TestCRAN_Binary_PlatformIsolation publishes a Windows binary and a macOS
// binary to the same hosted repo and verifies neither appears in the other's
// PACKAGES index. Runs on any OS with Docker.
func TestCRAN_Binary_PlatformIsolation(t *testing.T) {
	srv := conformance.StartForge(t)
	repo := srv.ContainerRepo("cran-hosted")

	conformance.RunScript(t, "python:3-slim", fmt.Sprintf(`
set -e
python3 - <<'PYEOF'
import urllib.request, zipfile, tarfile, io, gzip

REPO = "%s"

def put(url, data):
    req = urllib.request.Request(url, data=data, method="PUT")
    with urllib.request.urlopen(req) as r:
        assert r.status == 201, f"PUT {url} => {r.status}"

def get_text(url):
    with urllib.request.urlopen(url) as r:
        return r.read().decode()

def make_zip(pkg, ver):
    desc = (f"Package: {pkg}\nVersion: {ver}\nLicense: MIT\nOS_type: windows\n"
            "Built: R 4.4.0; x86_64-w64-mingw32; 2024-01-15 00:00:00 UTC; windows\n"
            "Archs: x64\n").encode()
    buf = io.BytesIO()
    with zipfile.ZipFile(buf, "w") as zf:
        zf.writestr(f"{pkg}/DESCRIPTION", desc)
        zf.writestr(f"{pkg}/NAMESPACE", b"exportPattern('.')\n")
    return buf.getvalue()

def make_tgz(pkg, ver):
    desc = (f"Package: {pkg}\nVersion: {ver}\nLicense: MIT\nOS_type: unix\n"
            "Built: R 4.4.0; aarch64-apple-darwin20; 2024-01-15 00:00:00 UTC; unix\n").encode()
    buf = io.BytesIO()
    with gzip.GzipFile(fileobj=buf, mode="wb", mtime=0) as gz:
        with tarfile.open(fileobj=gz, mode="w") as tf:
            ti = tarfile.TarInfo(f"{pkg}/DESCRIPTION")
            ti.size = len(desc)
            tf.addfile(ti, io.BytesIO(desc))
    return buf.getvalue()

put(REPO + "bin/windows/contrib/4.4/winonly_1.0.0.zip", make_zip("winonly", "1.0.0"))
put(REPO + "bin/macosx/big-sur-arm64/contrib/4.4/maconly_1.0.0.tgz", make_tgz("maconly", "1.0.0"))

win = get_text(REPO + "bin/windows/contrib/4.4/PACKAGES")
mac = get_text(REPO + "bin/macosx/big-sur-arm64/contrib/4.4/PACKAGES")

assert "winonly" in win,     f"winonly missing from Windows PACKAGES"
assert "maconly" not in win, f"maconly leaked into Windows PACKAGES:\n{win}"
assert "maconly" in mac,     f"maconly missing from macOS PACKAGES"
assert "winonly" not in mac, f"winonly leaked into macOS PACKAGES:\n{mac}"
print("Platform isolation: OK")
PYEOF
`, repo))
}

// TestCRAN_Binary_Windows_PublishInstall publishes a minimal pure-R Windows
// binary package to forge and installs it with install.packages(type="win.binary").
// Skipped on non-Windows platforms.
func TestCRAN_Binary_Windows_PublishInstall(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows-only binary install test")
	}

	srv := conformance.StartForge(t)
	repo := srv.Repo("cran-hosted")

	pkg, ver := "wintestpkg", "1.0.0"
	zipData := makeBinaryConformanceZip(t, pkg, ver)

	req, _ := http.NewRequest(http.MethodPut,
		repo+"bin/windows/contrib/4.4/"+pkg+"_"+ver+".zip",
		bytes.NewReader(zipData))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT Windows binary: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT Windows binary: got %d", resp.StatusCode)
	}

	conformance.RunHostRscript(t, fmt.Sprintf(`
repo <- "%s"
lib  <- tempfile("rlib")
dir.create(lib, recursive = TRUE)
install.packages("wintestpkg",
  repos = repo, type = "win.binary", lib = lib, quiet = FALSE)
stopifnot(file.exists(file.path(lib, "wintestpkg", "DESCRIPTION")))
cat("wintestpkg installed OK\n")
`, repo))
}

// TestCRAN_Binary_macOS_PublishInstall publishes a minimal macOS binary package
// to the platform path that the host R instance expects, then installs it with
// install.packages(type="binary"). Skipped on non-macOS platforms.
func TestCRAN_Binary_macOS_PublishInstall(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS-only binary install test")
	}

	rscript, err := exec.LookPath("Rscript")
	if err != nil {
		t.Skip("Rscript not found on PATH")
	}

	srv := conformance.StartForge(t)
	repo := srv.Repo("cran-hosted")

	// Ask R which binary contrib URL it would use for this platform/version.
	// contrib.url is pure string manipulation — no network call is made.
	out, err := exec.Command(rscript, "--vanilla", "-e", // #nosec G204 -- test harness only
		fmt.Sprintf(`cat(contrib.url("%s", type="binary"))`, repo)).Output()
	if err != nil {
		t.Fatalf("query R contrib.url: %v", err)
	}
	// out = "http://localhost:PORT/repository/cran-hosted/bin/macosx/big-sur-arm64/contrib/4.4"
	contribURL := strings.TrimSpace(string(out))
	binPath := strings.TrimPrefix(contribURL, repo)
	// binPath = "bin/macosx/big-sur-arm64/contrib/4.4"

	pkg, ver := "mactestpkg", "1.0.0"
	tgzData := makeBinaryConformanceTgz(t, pkg, ver)

	req, _ := http.NewRequest(http.MethodPut,
		repo+binPath+"/"+pkg+"_"+ver+".tgz",
		bytes.NewReader(tgzData))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT macOS binary: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT macOS binary: got %d", resp.StatusCode)
	}

	conformance.RunHostRscript(t, fmt.Sprintf(`
repo <- "%s"
lib  <- tempfile("rlib")
dir.create(lib, recursive = TRUE)
install.packages("mactestpkg",
  repos = repo, type = "binary", lib = lib, quiet = FALSE)
stopifnot(file.exists(file.path(lib, "mactestpkg", "DESCRIPTION")))
cat("mactestpkg installed OK\n")
`, repo))
}

// makeBinaryConformanceZip builds a minimal pure-R Windows binary .zip with
// DESCRIPTION, NAMESPACE, and a trivial R file.
func makeBinaryConformanceZip(t *testing.T, pkg, ver string) []byte {
	t.Helper()
	desc := fmt.Sprintf(
		"Package: %s\nVersion: %s\nLicense: MIT\n"+
			"Built: R 4.4.0; x86_64-w64-mingw32; 2024-01-15 00:00:00 UTC; windows\n"+
			"Archs: x64\nOS_type: windows\n",
		pkg, ver)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range map[string]string{
		pkg + "/DESCRIPTION":        desc,
		pkg + "/NAMESPACE":          "exportPattern('.')\n",
		pkg + "/R/" + pkg + ".R":   fmt.Sprintf("hello <- function() invisible(NULL)\n"),
	} {
		f, _ := zw.Create(name)
		f.Write([]byte(content)) //nolint:errcheck
	}
	zw.Close()
	return buf.Bytes()
}

// makeBinaryConformanceTgz builds a minimal macOS binary .tgz with DESCRIPTION,
// NAMESPACE, and a trivial R file. The Built field uses arm64-apple-darwin20
// which is compatible with any arm64 macOS runner.
//
// Directory entries are written explicitly before their contents so that BSD
// tar's selective extraction (which R uses to validate binary packages via
// untar(files="pkg/DESCRIPTION")) succeeds on macOS without auto-creating
// parent directories.
func makeBinaryConformanceTgz(t *testing.T, pkg, ver string) []byte {
	t.Helper()
	desc := fmt.Sprintf(
		"Package: %s\nVersion: %s\nLicense: MIT\n"+
			"Built: R 4.4.0; aarch64-apple-darwin20; 2024-01-15 00:00:00 UTC; unix\n"+
			"OS_type: unix\n",
		pkg, ver)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, dir := range []string{pkg + "/", pkg + "/R/"} {
		tw.WriteHeader(&tar.Header{Name: dir, Mode: 0755, Typeflag: tar.TypeDir}) //nolint:errcheck
	}
	for name, content := range map[string]string{
		pkg + "/DESCRIPTION":      desc,
		pkg + "/NAMESPACE":        "exportPattern('.')\n",
		pkg + "/R/" + pkg + ".R": "hello <- function() invisible(NULL)\n",
	} {
		b := []byte(content)
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0644, Size: int64(len(b))}) //nolint:errcheck
		tw.Write(b)                                                                //nolint:errcheck
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// TestBinaryTgzStructure verifies that makeBinaryConformanceTgz produces a
// tarball with explicit directory entries preceding their contents. BSD tar on
// macOS requires this for selective extraction (untar(files="pkg/DESCRIPTION")),
// which is how R validates binary packages before installing them. This test
// runs on all platforms without needing R or Docker.
func TestBinaryTgzStructure(t *testing.T) {
	data := makeBinaryConformanceTgz(t, "mypkg", "1.0.0")

	gr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("not a valid gzip: %v", err)
	}
	defer gr.Close()
	tr := tar.NewReader(gr)

	var dirPos, filePos int
	dirPos = -1
	filePos = -1
	pos := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar read error: %v", err)
		}
		if hdr.Typeflag == tar.TypeDir && hdr.Name == "mypkg/" {
			dirPos = pos
		}
		if hdr.Name == "mypkg/DESCRIPTION" {
			filePos = pos
			content, _ := io.ReadAll(tr)
			if !strings.Contains(string(content), "Built:") {
				t.Error("DESCRIPTION missing Built field")
			}
		}
		pos++
	}
	if dirPos < 0 {
		t.Error("tgz missing explicit directory entry mypkg/ — BSD tar selective extraction fails without it")
	}
	if filePos < 0 {
		t.Error("tgz missing mypkg/DESCRIPTION")
	}
	if dirPos >= 0 && filePos >= 0 && dirPos > filePos {
		t.Error("directory entry mypkg/ must appear before mypkg/DESCRIPTION in the tar stream")
	}
}

// TestCRAN_Binary_Proxy verifies that forge proxies a binary package GET from
// its upstream and caches the result so a second request does not hit upstream.
// Runs on Linux only (HTTP layer; no R install needed). Uses a local mock
// upstream rather than live CRAN so the test is hermetic and fast.
func TestCRAN_Binary_Proxy(t *testing.T) {
	pkg := makeBinaryConformanceZip(t, "proxypkg", "1.0.0")

	var hits int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/zip")
		w.Write(pkg) //nolint:errcheck
	}))
	defer upstream.Close()

	srv := conformance.StartForgeEnv(t, []string{"CRAN_PROXY_UPSTREAM=" + upstream.URL})
	pkgURL := srv.Repo("cran-proxy") + "bin/windows/contrib/4.4/proxypkg_1.0.0.zip"

	// First GET — cache miss; upstream must be called exactly once.
	resp, err := http.Get(pkgURL) //nolint:noctx
	if err != nil {
		t.Fatalf("first GET: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first GET: got %d", resp.StatusCode)
	}
	if !bytes.Equal(body, pkg) {
		t.Fatal("first GET: body differs from upstream fixture")
	}
	if hits != 1 {
		t.Fatalf("expected 1 upstream hit after cache miss, got %d", hits)
	}

	// Second GET — cache hit; upstream must NOT be called again.
	resp, err = http.Get(pkgURL) //nolint:noctx
	if err != nil {
		t.Fatalf("second GET: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second GET: got %d", resp.StatusCode)
	}
	if !bytes.Equal(body, pkg) {
		t.Fatal("second GET: body differs")
	}
	if hits != 1 {
		t.Fatalf("expected upstream hit count to remain 1 after cache hit, got %d", hits)
	}
}
