//go:build conformance

package conformance_test

import (
	"fmt"
	"testing"

	"forge/internal/conformance"
)

// TestPyPI_Twine_Publish_Pip_Install drives the two real clients end to end:
// twine uploads a wheel and an sdist, pip resolves and installs from forge's
// simple index, and the installed module is imported.
//
// The project is deliberately named "My.Package". PEP 503 says pip will ask for
// it as "my-package", setuptools will name the files "my_package-1.0.0…", and
// forge has to agree with both. Three spellings of one name is where a private
// index usually breaks, so it is what this test pins down.
func TestPyPI_Twine_Publish_Pip_Install(t *testing.T) {
	srv := conformance.StartForge(t)
	repo := srv.ContainerRepo("pypi-hosted")

	conformance.RunScript(t, "python:3-slim", fmt.Sprintf(`
set -e
REPO="%s"

# setuptools is installed globally too, for the --no-build-isolation step below.
pip install --quiet --disable-pip-version-check build twine setuptools

mkdir -p /work/forgedemo && cd /work
cat > pyproject.toml <<'EOF'
[build-system]
requires = ["setuptools>=61"]
build-backend = "setuptools.build_meta"

[project]
name = "My.Package"
version = "1.0.0"
requires-python = ">=3.8"

[tool.setuptools]
packages = ["forgedemo"]
EOF
echo 'def hello(): return "from forge"' > forgedemo/__init__.py

python -m build >/dev/null
ls dist/
# setuptools writes the files under its own spelling, not the project's.
test -f dist/my_package-1.0.0-py3-none-any.whl

# --- publish with real twine ------------------------------------------------
TWINE_USERNAME=forge TWINE_PASSWORD=forge \
  twine upload --disable-progress-bar --repository-url "$REPO" dist/*
echo "twine upload: OK"

# --- the simple index, as pip sees it ---------------------------------------
python3 - <<PYEOF
import urllib.request

REPO = "$REPO"

def get(url):
    with urllib.request.urlopen(url) as r:
        assert r.status == 200, f"{url} => {r.status}"
        return r.read().decode()

index = get(REPO + "simple/")
assert 'href="my-package/"' in index, f"normalized name missing from index:\n{index}"
print("simple index: OK")

# Every spelling PEP 503 calls equivalent must reach the same page.
pages = {}
for spelling in ("My.Package", "my-package", "my_package", "MY__PACKAGE"):
    page = get(REPO + "simple/" + spelling + "/")
    assert "my_package-1.0.0-py3-none-any.whl" in page, f"wheel missing for {spelling}"
    assert "my_package-1.0.0.tar.gz" in page, f"sdist missing for {spelling}"
    assert "#sha256=" in page, f"hash fragment missing for {spelling}"
    assert 'data-requires-python="&gt;=3.8"' in page or 'data-requires-python=">=3.8"' in page, \
        f"requires-python missing for {spelling}:\n{page}"
    pages[spelling] = page
assert len(set(pages.values())) == 1, "equivalent spellings served different pages"
print("PEP 503 normalization across 4 spellings: OK")
PYEOF

# --- install with real pip --------------------------------------------------
# Asked for under the original spelling; served under the normalized one.
# --trusted-host is pip's own plain-HTTP policy, not a forge requirement; a
# real deployment sits behind TLS and needs neither flag.
pip install --quiet --disable-pip-version-check --trusted-host host.docker.internal \
  --index-url "${REPO}simple/" --no-deps "My.Package==1.0.0"
python3 -c "import forgedemo; assert forgedemo.hello() == 'from forge', forgedemo.hello()"
echo "pip install + import: OK"

# The sdist is served too, so a source-only install resolves. --no-build-isolation
# keeps pip from looking up its own build deps (setuptools) in forge's index,
# which only holds this one project.
pip download --quiet --disable-pip-version-check --trusted-host host.docker.internal \
  --no-deps --no-binary :all: --no-build-isolation \
  --index-url "${REPO}simple/" -d /tmp/dl "my-package==1.0.0"
test -f /tmp/dl/my_package-1.0.0.tar.gz
echo "sdist download: OK"

echo "All PyPI conformance checks passed"
`, repo))
}

// TestPyPI_Proxy_Pip_Install drives real pip against a proxy repo pointed at
// pypi.org. The index and the files live on different hosts upstream, so the
// thing worth proving is that pip follows forge's rewritten links, gets real
// bytes back, and that the second install is served from cache.
func TestPyPI_Proxy_Pip_Install(t *testing.T) {
	srv := conformance.StartForge(t)
	repo := srv.ContainerRepo("pypi-proxy")

	conformance.RunScript(t, "python:3-slim", fmt.Sprintf(`
set -e
REPO="%s"
PIP="pip install --quiet --disable-pip-version-check --trusted-host host.docker.internal --index-url ${REPO}simple/"

python3 - <<PYEOF
import urllib.request
page = urllib.request.urlopen("${REPO}simple/six/").read().decode()
assert "six-" in page, "no files listed for six"
# Every link must come back through forge, or pip downloads around the cache
# and the proxy is decorative.
assert "files.pythonhosted.org" not in page, "rewritten page still points at upstream"
assert "/repository/pypi-proxy/packages/six/" in page, f"links not rewritten:\n{page[:400]}"
print("proxied index rewritten: OK")
PYEOF

# First install: cold cache, forge fetches from pypi.org.
$PIP --target /tmp/a six==1.16.0
python3 -c "import sys; sys.path.insert(0, '/tmp/a'); import six; print('import six:', six.__version__)"

# Second install: the bytes now come from forge's cache.
$PIP --target /tmp/b six==1.16.0
test -f /tmp/b/six.py
echo "second install (cached): OK"

# The 45 MB root index is deliberately refused rather than proxied.
code=$(python3 -c "
import urllib.request, urllib.error
try:
    urllib.request.urlopen('${REPO}simple/')
    print(200)
except urllib.error.HTTPError as e:
    print(e.code)
")
test "$code" = "501" || { echo "root index returned $code, want 501"; exit 1; }
echo "root index refused: OK"

echo "All PyPI proxy conformance checks passed"
`, repo))
}

// TestPyPI_Group_ShadowsUpstream is the dependency-confusion scenario with real
// pip: an internal package is published under a name that also exists on
// pypi.org, and the group must serve the internal one. It also checks that a
// package only upstream has still resolves through the same URL, because a
// group that shadows by breaking upstream resolution is no use.
func TestPyPI_Group_ShadowsUpstream(t *testing.T) {
	srv := conformance.StartForge(t)
	hosted := srv.ContainerRepo("pypi-hosted")
	group := srv.ContainerRepo("pypi-public")

	conformance.RunScript(t, "python:3-slim", fmt.Sprintf(`
set -e
HOSTED="%s"
GROUP="%s"

pip install --quiet --disable-pip-version-check build twine setuptools

# An internal package deliberately named after a real pypi.org project.
mkdir -p /work/six_internal && cd /work
cat > pyproject.toml <<'EOF'
[build-system]
requires = ["setuptools>=61"]
build-backend = "setuptools.build_meta"

[project]
name = "six"
version = "99.0.0"

[tool.setuptools]
packages = ["six_internal"]
EOF
echo 'ORIGIN = "internal"' > six_internal/__init__.py
python -m build --wheel >/dev/null

TWINE_USERNAME=forge TWINE_PASSWORD=forge \
  twine upload --disable-progress-bar --repository-url "$HOSTED" dist/*
echo "internal 'six' published: OK"

# Through the group, pip must resolve the internal package, not pypi.org's.
pip install --quiet --disable-pip-version-check --trusted-host host.docker.internal \
  --index-url "${GROUP}simple/" --no-deps --target /tmp/g six
python3 - <<'PYEOF'
import sys
sys.path.insert(0, "/tmp/g")
import six_internal
assert six_internal.ORIGIN == "internal", six_internal.ORIGIN
try:
    import six  # the real pypi.org package would provide this module
    raise SystemExit("upstream six was installed — the group did not shadow it")
except ImportError:
    pass
print("group shadowed upstream 'six': OK")
PYEOF

# A project only upstream has must still resolve through the same URL.
pip install --quiet --disable-pip-version-check --trusted-host host.docker.internal \
  --index-url "${GROUP}simple/" --no-deps --target /tmp/u iniconfig
test -d /tmp/u/iniconfig || test -f /tmp/u/iniconfig.py
echo "upstream-only project through the group: OK"

echo "All PyPI group conformance checks passed"
`, hosted, group))
}
