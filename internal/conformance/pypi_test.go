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
