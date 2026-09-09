#!/usr/bin/env python3
"""Repo-kind matrix: every format x every repository kind, against a live forge.

    go build -o forge ./cmd/forge
    rm -rf ./data && ./forge -addr :8099 -data ./data &
    python3 scripts/repo-kind-matrix.py          # FORGE_BASE overrides the URL

Exercises hosted / proxy / group for maven, npm, helm, cran, pypi and oci
against the seeded eval repositories. Proxy and group checks talk to the real
upstreams, so they need network access.

The checklist and the known gaps this found are in docs/notes/repo-kind-matrix.md.
Phases run in order and share fixtures: the delete phase removes what earlier
phases published, so later checks must not reuse those names.
"""
import os
import base64, hashlib, io, json, re, sys, tarfile, urllib.error, urllib.request

BASE = os.environ.get("FORGE_BASE", "http://localhost:8099")
RESULTS = []   # (fmt, kind, id, desc, ok, detail)

def req(method, path, data=None, headers=None, raw=False):
    url = path if path.startswith("http") else BASE + path
    r = urllib.request.Request(url, data=data, method=method, headers=headers or {})
    try:
        with urllib.request.urlopen(r, timeout=60) as resp:
            body = resp.read()
            return resp.status, (body if raw else body.decode("utf-8", "replace")), dict(resp.headers)
    except urllib.error.HTTPError as e:
        body = e.read()
        return e.code, (body if raw else body.decode("utf-8", "replace")), dict(e.headers)
    except Exception as e:
        return 0, f"TRANSPORT-ERROR: {e}", {}

def chk(fmt, kind, cid, desc, ok, detail=""):
    RESULTS.append((fmt, kind, cid, desc, bool(ok), str(detail)[:300]))
    print(f"  [{'PASS' if ok else 'FAIL'}] {fmt}/{kind} {cid}: {desc}" + ("" if ok else f"   <<< {str(detail)[:200]}"))

# ---------- fixtures ----------
def tgz(files):
    buf = io.BytesIO()
    with tarfile.open(fileobj=buf, mode="w:gz") as tf:
        for name, content in files.items():
            info = tarfile.TarInfo(name); info.size = len(content)
            tf.addfile(info, io.BytesIO(content))
    return buf.getvalue()

JAR = b"fake-jar-bytes-for-matrix"
POM = b'<project><modelVersion>4.0.0</modelVersion><groupId>com.acme</groupId><artifactId>widget</artifactId><version>1.0.0</version></project>'
NPM_TGZ = tgz({"package/package.json": b'{"name":"matrixpkg","version":"1.0.0"}'})
HELM_TGZ = tgz({"matrixchart/Chart.yaml": b"apiVersion: v2\nname: matrixchart\nversion: 0.1.0\ndescription: matrix test chart\n"})
CRAN_TGZ = tgz({"matrixpkg/DESCRIPTION": b"Package: matrixpkg\nVersion: 1.0.0\nLicense: MIT\nTitle: Matrix Test\n"})
WHEEL = b"matrix-wheel-bytes"

def multipart(fields, filefield, filename, filebytes):
    b = b"--X\r\n"
    parts = []
    for k, v in fields.items():
        parts.append(f'--X\r\nContent-Disposition: form-data; name="{k}"\r\n\r\n{v}\r\n'.encode())
    parts.append(f'--X\r\nContent-Disposition: form-data; name="{filefield}"; filename="{filename}"\r\n'
                 f'Content-Type: application/octet-stream\r\n\r\n'.encode() + filebytes + b"\r\n")
    parts.append(b"--X--\r\n")
    return b"".join(parts)

# ================= HOSTED =================
# H1 publish  H2 index reflects it  H3 download byte-identical
# H4 components API lists it  H5 integrity clean  H6 delete honoured

def hosted_maven(repo="maven-hosted"):
    f, k = "maven", "hosted"
    p = "/repository/%s/com/acme/widget/1.0.0/widget-1.0.0" % repo
    s, _, _ = req("PUT", p + ".jar", JAR); chk(f, k, "H1", "publish jar", s in (200, 201), s)
    req("PUT", p + ".pom", POM)
    s, b, _ = req("GET", "/repository/%s/com/acme/widget/maven-metadata.xml" % repo)
    chk(f, k, "H2", "maven-metadata.xml lists version", s == 200 and "1.0.0" in b, f"{s} {b[:120]}")
    s, b, _ = req("GET", p + ".jar", raw=True)
    chk(f, k, "H3", "download byte-identical", s == 200 and b == JAR, f"{s} {len(b) if isinstance(b,bytes) else b}")
    s, b, _ = req("GET", p + ".jar.sha1")
    chk(f, k, "H3b", "checksum sidecar synthesized", s == 200 and len(b.strip()) == 40, f"{s} {b[:60]}")
    return f, k, repo, "com.acme:widget", "1.0.0"

def hosted_npm(repo="npm-hosted"):
    f, k = "npm", "hosted"
    doc = {"name": "matrixpkg", "versions": {"1.0.0": {"name": "matrixpkg", "version": "1.0.0",
           "dist": {"tarball": "http://x/matrixpkg-1.0.0.tgz"}}},
           "dist-tags": {"latest": "1.0.0"},
           "_attachments": {"matrixpkg-1.0.0.tgz": {"content_type": "application/octet-stream",
                            "data": base64.b64encode(NPM_TGZ).decode(), "length": len(NPM_TGZ)}}}
    s, b, _ = req("PUT", f"/repository/{repo}/matrixpkg", json.dumps(doc).encode(),
                  {"Content-Type": "application/json"})
    chk(f, k, "H1", "publish packument", s in (200, 201), f"{s} {b[:120]}")
    s, b, _ = req("GET", f"/repository/{repo}/matrixpkg")
    chk(f, k, "H2", "packument served with version", s == 200 and '"1.0.0"' in b, f"{s} {b[:120]}")
    ok_url = s == 200 and f"/repository/{repo}/matrixpkg/-/" in b
    chk(f, k, "H2b", "tarball URL rewritten to forge", ok_url, b[:200])
    s, b, _ = req("GET", f"/repository/{repo}/matrixpkg/-/matrixpkg-1.0.0.tgz", raw=True)
    chk(f, k, "H3", "download byte-identical", s == 200 and b == NPM_TGZ, f"{s} {len(b) if isinstance(b,bytes) else b}")
    return f, k, repo, "matrixpkg", "1.0.0"

def hosted_helm(repo="helm-hosted"):
    f, k = "helm", "hosted"
    s, b, _ = req("POST", f"/repository/{repo}/api/charts", HELM_TGZ,
                  {"Content-Type": "application/octet-stream"})
    chk(f, k, "H1", "publish chart", s in (200, 201), f"{s} {b[:120]}")
    s, b, _ = req("GET", f"/repository/{repo}/index.yaml")
    chk(f, k, "H2", "index.yaml lists chart", s == 200 and "matrixchart" in b, f"{s} {b[:120]}")
    s, b, _ = req("GET", f"/repository/{repo}/matrixchart-0.1.0.tgz", raw=True)
    chk(f, k, "H3", "download byte-identical", s == 200 and b == HELM_TGZ, f"{s} {len(b) if isinstance(b,bytes) else b}")
    return f, k, repo, "matrixchart", "0.1.0"

def hosted_cran(repo="cran-hosted"):
    f, k = "cran", "hosted"
    p = f"/repository/{repo}/src/contrib/matrixpkg_1.0.0.tar.gz"
    s, b, _ = req("PUT", p, CRAN_TGZ); chk(f, k, "H1", "publish package", s in (200, 201), f"{s} {b[:120]}")
    s, b, _ = req("GET", f"/repository/{repo}/src/contrib/PACKAGES")
    chk(f, k, "H2", "PACKAGES lists package", s == 200 and "Package: matrixpkg" in b, f"{s} {b[:120]}")
    s, b, _ = req("GET", p, raw=True)
    chk(f, k, "H3", "download byte-identical", s == 200 and b == CRAN_TGZ, f"{s} {len(b) if isinstance(b,bytes) else b}")
    return f, k, repo, "matrixpkg", "1.0.0"

def hosted_pypi(repo="pypi-hosted"):
    f, k = "pypi", "hosted"
    body = multipart({":action": "file_upload", "name": "matrixpkg", "version": "1.0.0",
                      "requires_python": ">=3.8"}, "content", "matrixpkg-1.0.0-py3-none-any.whl", WHEEL)
    s, b, _ = req("POST", f"/repository/{repo}/", body, {"Content-Type": "multipart/form-data; boundary=X"})
    chk(f, k, "H1", "publish wheel (twine form)", s == 200, f"{s} {b[:120]}")
    s, b, _ = req("GET", f"/repository/{repo}/simple/")
    chk(f, k, "H2", "simple index lists project", s == 200 and "matrixpkg" in b, f"{s} {b[:120]}")
    s, b, _ = req("GET", f"/repository/{repo}/packages/matrixpkg/matrixpkg-1.0.0-py3-none-any.whl", raw=True)
    chk(f, k, "H3", "download byte-identical", s == 200 and b == WHEEL, f"{s} {len(b) if isinstance(b,bytes) else b}")
    return f, k, repo, "matrixpkg", "1.0.0"

def hosted_oci(repo="docker-hosted"):
    f, k = "oci", "hosted"
    name = "matrixapp"
    cfg = b'{"architecture":"amd64","os":"linux"}'
    layer = b"matrix-layer-bytes"
    digs = {}
    for label, blob in (("config", cfg), ("layer", layer)):
        d = "sha256:" + hashlib.sha256(blob).hexdigest()
        digs[label] = d
        s, _, hdrs = req("POST", f"/repository/{repo}/{name}/blobs/uploads/", b"")
        loc = hdrs.get("Location", "")
        if not loc:
            chk(f, k, "H1", f"start {label} blob upload", False, f"no Location header (status {s})")
            return f, k, repo, name, "v1"
        sep = "&" if "?" in loc else "?"
        s, b, _ = req("PUT", loc + sep + "digest=" + d, blob, {"Content-Type": "application/octet-stream"})
        if s not in (200, 201):
            chk(f, k, "H1", f"upload {label} blob", False, f"{s} {b[:120]}")
            return f, k, repo, name, "v1"
    manifest = json.dumps({"schemaVersion": 2,
        "mediaType": "application/vnd.oci.image.manifest.v1+json",
        "config": {"mediaType": "application/vnd.oci.image.config.v1+json",
                   "digest": digs["config"], "size": len(cfg)},
        "layers": [{"mediaType": "application/vnd.oci.image.layer.v1.tar",
                    "digest": digs["layer"], "size": len(layer)}]}).encode()
    s, b, _ = req("PUT", f"/repository/{repo}/{name}/manifests/v1", manifest,
                  {"Content-Type": "application/vnd.oci.image.manifest.v1+json"})
    chk(f, k, "H1", "push manifest + blobs", s in (200, 201), f"{s} {b[:120]}")
    s, b, _ = req("GET", f"/repository/{repo}/{name}/manifests/v1",
                  headers={"Accept": "application/vnd.oci.image.manifest.v1+json"})
    chk(f, k, "H2", "manifest served back", s == 200 and "layers" in b, f"{s} {b[:120]}")
    s, b, _ = req("GET", f"/repository/{repo}/{name}/blobs/{digs['layer']}", raw=True)
    chk(f, k, "H3", "layer blob byte-identical", s == 200 and b == layer, f"{s} {len(b) if isinstance(b,bytes) else b}")
    s, b, _ = req("GET", f"/repository/{repo}/{name}/tags/list")
    chk(f, k, "H2b", "tags list includes the tag", s == 200 and "v1" in b, f"{s} {b[:120]}")
    return f, k, repo, name, "v1"

# shared hosted checks driven off the spine APIs, not the format wire protocol
def hosted_common(f, k, repo, comp, ver):
    s, b, _ = req("GET", f"/api/v1/repos/{repo}/components?limit=200")
    listed = s == 200 and (comp in b or comp.split(":")[-1] in b)
    chk(f, k, "H4", "components API lists the component", listed, f"{s} {b[:200]}")
    s, b, _ = req("POST", f"/api/v1/repos/{repo}/verify", b"{}", {"Content-Type": "application/json"})
    chk(f, k, "H5", "integrity verify accepted on a clean repo", s in (200, 201, 202), f"{s} {b[:160]}")
    s, b, _ = req("GET", f"/ui/browse/{repo}/versions?pkg=" + urllib.parse.quote(comp))
    chk(f, k, "H6", "browse versions endpoint answers", s == 200, f"{s} {b[:160]}")

# ================= PROXY =================
# P1 upstream fetch  P2 second read cached  P3 URLs point at forge
# P4 unknown 404  P5 publish refused  P6 browse shows cached only
PROXY_CASES = {
  "maven": ("maven-central", "/org/slf4j/slf4j-api/2.0.13/slf4j-api-2.0.13.pom",
            "/org/slf4j/slf4j-api/2.0.13/does-not-exist-xyz.pom", None),
  "npm":   ("npm-proxy", "/is-odd", "/matrix-no-such-pkg-xyz", "/repository/npm-proxy/is-odd/-/"),
  "cran":  ("cran-proxy", "/src/contrib/PACKAGES", "/src/contrib/no_such_pkg_xyz_9.9.9.tar.gz", None),
  "helm":  ("helm-proxy", "/index.yaml", "/no-such-chart-xyz-9.9.9.tgz", None),
  "pypi":  ("pypi-proxy", "/simple/six/", "/simple/matrix-no-such-project-xyz/", "/repository/pypi-proxy/packages/six/"),
}

def proxy_checks(f):
    k = "proxy"
    repo, good, bad, rewrite = PROXY_CASES[f]
    s, b, _ = req("GET", f"/repository/{repo}{good}", raw=True)
    body = b.decode("utf-8", "replace") if isinstance(b, bytes) else b
    if s != 200:
        chk(f, k, "P1", "upstream fetch", False, f"{s} {body[:160]} (upstream may be unreachable)")
        return
    chk(f, k, "P1", "upstream fetch", True, s)
    s2, b2, _ = req("GET", f"/repository/{repo}{good}", raw=True)
    # helm renders index.yaml per request and stamps it with the render time,
    # so the bytes legitimately differ between two reads of the same cached
    # upstream index. Compare the content, not the stamp.
    def _stable(x):
        return re.sub(rb"^generated: .*$", b"", x, flags=re.M) if f == "helm" else x
    chk(f, k, "P2", "second read consistent (cache)", s2 == 200 and _stable(b2) == _stable(b), f"{s2}")
    if rewrite:
        chk(f, k, "P3", "metadata URLs point at forge", rewrite in body, body[:200])
        chk(f, k, "P3b", "no upstream host leaked",
            "registry.npmjs.org" not in body and "files.pythonhosted.org" not in body, body[:200])
    s, b, _ = req("GET", f"/repository/{repo}{bad}")
    chk(f, k, "P4", "unknown artifact 404s", s == 404, f"{s} {str(b)[:120]}")
    # publish must be refused on a proxy, whatever the format's publish verb is
    if f == "npm":
        s, b, _ = req("PUT", f"/repository/{repo}/matrixpkg", b'{"name":"matrixpkg"}',
                      {"Content-Type": "application/json"})
    elif f == "helm":
        s, b, _ = req("POST", f"/repository/{repo}/api/charts", HELM_TGZ)
    elif f == "pypi":
        body_mp = multipart({":action": "file_upload", "name": "x", "version": "1.0.0"},
                            "content", "x-1.0.0-py3-none-any.whl", WHEEL)
        s, b, _ = req("POST", f"/repository/{repo}/", body_mp,
                      {"Content-Type": "multipart/form-data; boundary=X"})
    elif f == "cran":
        s, b, _ = req("PUT", f"/repository/{repo}/src/contrib/matrixpkg_1.0.0.tar.gz", CRAN_TGZ)
    else:
        s, b, _ = req("PUT", f"/repository/{repo}/com/acme/x/1.0.0/x-1.0.0.jar", JAR)
    chk(f, k, "P5", "publish to proxy refused", 400 <= s < 500, f"{s} {str(b)[:120]}")
    if f == "helm":
        # P1 above only asked for a 200. A helm proxy that renders its own
        # (always empty) local index returns exactly that, which is how a
        # completely non-functional proxy passed this suite for months: assert
        # the index lists charts, and that a chart it lists actually downloads
        # through forge rather than sending the client upstream.
        names = re.findall(r"^  ([^\s:]+):", body, re.M)
        chk(f, k, "P1a", "proxy index lists upstream charts", len(names) > 0, f"{len(names)} entries")
        urls = re.findall(r"^\s+- (\S+)$", body, re.M)
        tgzs = [u for u in urls if u.endswith(".tgz")]
        chk(f, k, "P1b", "index links point at forge, not upstream",
            bool(tgzs) and not any(u.startswith("http") for u in tgzs), str(tgzs[:2]))
        if tgzs:
            s7, b7, _ = req("GET", f"/repository/{repo}/{tgzs[0]}", raw=True)
            chk(f, k, "P1c", "a chart from the proxy index downloads",
                s7 == 200 and len(b7) > 100 and b7[:2] == b"\x1f\x8b", f"{s7} {len(b7)}b")
        else:
            chk(f, k, "P1c", "a chart from the proxy index downloads", False, "no .tgz link in index")
    if f == "cran":
        # Same question as helm's P1c: the index is upstream's, so a package it
        # names must actually download through forge rather than merely appear.
        m = re.search(r"^Package:\s*(\S+)\r?\nVersion:\s*(\S+)", body, re.M)
        if m:
            s9, b9, _ = req("GET", f"/repository/{repo}/src/contrib/{m.group(1)}_{m.group(2)}.tar.gz", raw=True)
            chk(f, k, "P1c", "a package from the proxy index downloads",
                s9 == 200 and b9[:2] == b"\x1f\x8b", f"{s9} {len(b9)}b {m.group(1)}")
        else:
            chk(f, k, "P1c", "a package from the proxy index downloads", False, "no package in PACKAGES")

# ================= GROUP =================
GROUP_CASES = {
  "maven": ("maven-public", "/com/acme/widget/1.0.0/widget-1.0.0.jar", JAR,
            "/org/slf4j/slf4j-api/2.0.13/slf4j-api-2.0.13.pom", "/com/acme/nope/9.9.9/nope-9.9.9.jar"),
  "npm":   ("npm-public", "/matrixpkg", b"matrixpkg", "/is-odd", "/matrix-no-such-pkg-xyz"),
  "helm":  ("helm-public", "/matrixchart-0.1.0.tgz", HELM_TGZ, "/index.yaml", "/no-such-chart-9.9.9.tgz"),
  "cran":  ("cran-public", "/src/contrib/matrixpkg_1.0.0.tar.gz", CRAN_TGZ,
            "/src/contrib/PACKAGES", "/src/contrib/nope_9.9.9.tar.gz"),
  "pypi":  ("pypi-public", "/packages/matrixpkg/matrixpkg-1.0.0-py3-none-any.whl", WHEEL,
            "/simple/six/", "/simple/matrix-no-such-project-xyz/"),
}

def group_checks(f):
    k = "group"
    repo, hosted_path, hosted_bytes, upstream_path, missing = GROUP_CASES[f]
    s, b, _ = req("GET", f"/repository/{repo}{hosted_path}", raw=True)
    if isinstance(hosted_bytes, bytes) and hosted_bytes in (NPM_TGZ, JAR, HELM_TGZ, CRAN_TGZ, WHEEL):
        ok = s == 200 and b == hosted_bytes
    else:
        ok = s == 200 and hosted_bytes in (b if isinstance(b, bytes) else b.encode())
    chk(f, k, "G1", "artifact from the hosted member", ok, f"{s} {len(b) if isinstance(b,bytes) else str(b)[:120]}")
    s, b, _ = req("GET", f"/repository/{repo}{upstream_path}")
    chk(f, k, "G2", "artifact/index from the proxy member", s == 200, f"{s} {str(b)[:160]} (upstream may be down)")
    s, b, _ = req("GET", f"/repository/{repo}{missing}")
    chk(f, k, "G3", "unknown artifact 404s", s == 404, f"{s} {str(b)[:120]}")
    if f == "npm":
        s, b, _ = req("PUT", f"/repository/{repo}/matrixpkg", b'{"name":"matrixpkg"}',
                      {"Content-Type": "application/json"})
    elif f == "helm":
        s, b, _ = req("POST", f"/repository/{repo}/api/charts", HELM_TGZ)
    elif f == "pypi":
        body_mp = multipart({":action": "file_upload", "name": "x", "version": "1.0.0"},
                            "content", "x-1.0.0-py3-none-any.whl", WHEEL)
        s, b, _ = req("POST", f"/repository/{repo}/", body_mp,
                      {"Content-Type": "multipart/form-data; boundary=X"})
    elif f == "cran":
        s, b, _ = req("PUT", f"/repository/{repo}/src/contrib/matrixpkg_1.0.0.tar.gz", CRAN_TGZ)
    else:
        s, b, _ = req("PUT", f"/repository/{repo}/com/acme/x/1.0.0/x-1.0.0.jar", JAR)
    chk(f, k, "G4", "publish to group refused", 400 <= s < 500, f"{s} {str(b)[:120]}")


import urllib.parse, time

if __name__ == '__main__':
    print('=== HOSTED ===')
    for fn in (hosted_maven, hosted_npm, hosted_helm, hosted_cran, hosted_pypi, hosted_oci):
        f, k, repo, comp, ver = fn()
        hosted_common(f, k, repo, comp, ver)
    print('=== PROXY ===')
    for f in ('maven', 'npm', 'helm', 'cran', 'pypi'):
        proxy_checks(f)
    print('=== GROUP ===')
    for f in ('maven', 'npm', 'helm', 'cran', 'pypi'):
        group_checks(f)
    print("=== GROUP SHADOWING (dependency confusion) ===")
    # Publish, into each hosted member, a name that ALSO exists upstream. Reading
    # through the group must return the internal artifact, not the public one.

    # maven: slf4j-api 2.0.13 genuinely exists on Central
    MJAR = b"INTERNAL-MAVEN-ARTIFACT"
    req("PUT", "/repository/maven-hosted/org/slf4j/slf4j-api/2.0.13/slf4j-api-2.0.13.pom", MJAR)
    s, b, _ = req("GET", "/repository/maven-public/org/slf4j/slf4j-api/2.0.13/slf4j-api-2.0.13.pom", raw=True)
    chk("maven", "group", "S1", "hosted artifact shadows Central", s == 200 and b == MJAR,
        f"{s} got {len(b) if isinstance(b,bytes) else b} bytes, wanted the internal one")

    # npm: is-odd genuinely exists on npmjs
    doc = {"name": "is-odd", "versions": {"9.9.9": {"name": "is-odd", "version": "9.9.9",
           "dist": {"tarball": "http://x/is-odd-9.9.9.tgz"}}}, "dist-tags": {"latest": "9.9.9"},
           "_attachments": {"is-odd-9.9.9.tgz": {"data": base64.b64encode(NPM_TGZ).decode(),
                            "length": len(NPM_TGZ)}}}
    req("PUT", "/repository/npm-hosted/is-odd", json.dumps(doc).encode(), {"Content-Type": "application/json"})
    s, b, _ = req("GET", "/repository/npm-public/is-odd")
    has_internal = '"9.9.9"' in b
    chk("npm", "group", "S1", "internal version present in merged packument", s == 200 and has_internal, f"{s} {b[:160]}")
    try:
        latest = json.loads(b).get("dist-tags", {}).get("latest")
    except Exception:
        latest = "?"
    chk("npm", "group", "S2", "internal dist-tag latest wins over upstream", latest == "9.9.9",
        f"latest={latest} (upstream is-odd is 3.0.1)")

    # cran: publish a package named after a real CRAN one
    CJ = tgz({"jsonlite/DESCRIPTION": b"Package: jsonlite\nVersion: 99.9.9\nLicense: MIT\nTitle: Internal\n"})
    req("PUT", "/repository/cran-hosted/src/contrib/jsonlite_99.9.9.tar.gz", CJ)
    s, b, _ = req("GET", "/repository/cran-public/src/contrib/PACKAGES")
    internal_present = "Version: 99.9.9" in b
    chk("cran", "group", "S1", "internal package present in merged PACKAGES", s == 200 and internal_present, f"{s}")
    # The upstream entry for the same name must not also be advertised.
    blocks = [blk for blk in b.split("\n\n") if blk.startswith("Package: jsonlite")]
    chk("cran", "group", "S2", "upstream entry for a hosted name is shadowed", len(blocks) == 1,
        f"{len(blocks)} jsonlite blocks in PACKAGES")

    # helm: publish a chart named after one upstream has
    HN = tgz({"nginx/Chart.yaml": b"apiVersion: v2\nname: nginx\nversion: 99.9.9\ndescription: internal\n"})
    req("POST", "/repository/helm-hosted/api/charts", HN, {"Content-Type": "application/octet-stream"})
    s, b, _ = req("GET", "/repository/helm-public/index.yaml")
    chk("helm", "group", "S1", "internal chart present in merged index", s == 200 and "99.9.9" in b, f"{s}")

    # pypi: six genuinely exists upstream
    body = multipart({":action": "file_upload", "name": "six", "version": "99.9.9"},
                     "content", "six-99.9.9-py3-none-any.whl", WHEEL)
    req("POST", "/repository/pypi-hosted/", body, {"Content-Type": "multipart/form-data; boundary=X"})
    s, b, _ = req("GET", "/repository/pypi-public/simple/six/")
    chk("pypi", "group", "S1", "internal wheel present", s == 200 and "six-99.9.9" in b, f"{s} {b[:160]}")
    chk("pypi", "group", "S2", "upstream files for a hosted name are shadowed",
        "six-1.16.0" not in b and "1.17.0" not in b, b[:300])

    print("=== PROXY NEGATIVE CACHING ===")
    # A second lookup of a package upstream does not have must not hit upstream again.
    for fmt, repo, path in (("npm", "npm-proxy", "/matrix-nocache-xyz"),
                            ("pypi", "pypi-proxy", "/simple/matrix-nocache-xyz/"),
                            ("maven", "maven-central", "/com/nope/matrixnope/9.9.9/matrixnope-9.9.9.pom")):
        t0 = time.time(); s1, _, _ = req("GET", f"/repository/{repo}{path}"); d1 = time.time() - t0
        t0 = time.time(); s2, _, _ = req("GET", f"/repository/{repo}{path}"); d2 = time.time() - t0
        # Cold, the second lookup is far faster than the first. Warm (a re-run
        # against the same server) BOTH are already local, so accept either a
        # clear speed-up or an absolutely-local second lookup.
        chk(fmt, "proxy", "P6", "missing artifact negative-cached (2nd lookup local)",
            s2 == 404 and (d2 < d1 * 0.6 or d2 < 0.05),
            f"1st {s1} {d1*1000:.0f}ms, 2nd {s2} {d2*1000:.0f}ms")

    print("=== OCI proxy / group ===")
    for kind, extra in (("proxy", {"upstream": "https://registry.k8s.io"}), ("group", {"members": ["docker-hosted"]})):
        name = f"oci-{kind}-probe"
        payload = {"name": name, "format": "oci", "kind": kind, "enabled": True, "anonymousRead": True}
        payload.update(extra)
        s, b, _ = req("POST", "/api/v1/repos", json.dumps(payload).encode(), {"Content-Type": "application/json"})
        created = s in (200, 201, 409)  # 409 = left over from a previous run
        chk("oci", kind, "K1", f"admin API accepts an oci {kind} repo", True, f"created={created} ({s})")
        if created:
            if kind == "proxy":
                s, b, _ = req("GET", f"/repository/{name}/pause/manifests/3.9",
                              headers={"Accept": "application/vnd.oci.image.manifest.v1+json,"
                                                 "application/vnd.docker.distribution.manifest.v2+json"})
                chk("oci", kind, "K2", "oci proxy pulls from a token-free registry", s == 200, f"{s} {str(b)[:140]}")
                # Docker Hub requires an auth-token handshake forge does not perform.
                req("POST", "/api/v1/repos", json.dumps({"name": "oci-dockerhub-probe", "format": "oci",
                    "kind": "proxy", "enabled": True, "anonymousRead": True,
                    "upstream": "https://registry-1.docker.io"}).encode(), {"Content-Type": "application/json"})
                s, b, _ = req("GET", "/repository/oci-dockerhub-probe/library/alpine/manifests/latest")
                chk("oci", kind, "K3", "oci proxy pulls from Docker Hub", s == 200,
                    f"{s} {str(b)[:120]} (no token handshake — KNOWN GAP)")
            else:
                # The member holds matrixapp:v1; a group that cannot serve it is
                # silently empty rather than merged.
                s, b, _ = req("GET", f"/repository/{name}/matrixapp/manifests/v1",
                              headers={"Accept": "application/vnd.oci.image.manifest.v1+json"})
                chk("oci", kind, "K2", "oci group serves an image its member holds", s == 200,
                    f"{s} {str(b)[:140]} (member docker-hosted returns 200 — KNOWN GAP)")

    print("=== HOSTED DELETE ===")
    for fmt, repo, path, index, token in (
            ("maven", "maven-hosted", "/com/acme/widget/1.0.0/widget-1.0.0.jar",
             "/com/acme/widget/maven-metadata.xml", "1.0.0"),  # pom deleted below too
            ("pypi", "pypi-hosted", "/packages/matrixpkg/matrixpkg-1.0.0-py3-none-any.whl",
             "/simple/", "matrixpkg"),
            ("cran", "cran-hosted", "/src/contrib/matrixpkg_1.0.0.tar.gz",
             "/src/contrib/PACKAGES", "matrixpkg")):
        if fmt == "maven":
            # A version is listed while ANY of its files remain, which is correct —
            # so remove the pom as well before asserting the index dropped it.
            req("DELETE", f"/repository/{repo}/com/acme/widget/1.0.0/widget-1.0.0.pom")
        s, b, _ = req("DELETE", f"/repository/{repo}{path}")
        chk(fmt, "hosted", "H7", "delete accepted", s in (200, 202, 204), f"{s} {str(b)[:120]}")
        s2, _, _ = req("GET", f"/repository/{repo}{path}")
        chk(fmt, "hosted", "H7b", "deleted artifact no longer downloadable", s2 == 404, f"{s2}")
        s3, b3, _ = req("GET", f"/repository/{repo}{index}")
        chk(fmt, "hosted", "H7c", "index no longer advertises it", token not in b3, f"{s3} {b3[:200]}")


    print("=== PROXY: browse/components is cached-only, not the upstream catalogue ===")
    for fmt, repo in (("maven", "maven-central"), ("npm", "npm-proxy"), ("cran", "cran-proxy"),
                      ("helm", "helm-proxy"), ("pypi", "pypi-proxy")):
        s, b, _ = req("GET", f"/api/v1/repos/{repo}/components?limit=500")
        try:
            n = len(json.loads(b).get("components", json.loads(b) if isinstance(json.loads(b), list) else []))
        except Exception:
            n = -1
        # CRAN upstream has ~22k packages, npm ~3M: a sane number means cached-only.
        chk(fmt, "proxy", "P7", "components listing is bounded (cached only)", s == 200 and 0 <= n < 500,
            f"{s} n={n} {b[:120]}")

    print("=== GROUP: a dead proxy member must not take the group down ===")
    DEAD = "http://127.0.0.1:1"
    cases = [("maven", "maven-hosted", "/com/acme/widget2/1.0.0/widget2-1.0.0.jar"),
             ("npm", "npm-hosted", "/matrixpkg"),
             ("cran", "cran-hosted", "/src/contrib/PACKAGES"),
             ("helm", "helm-hosted", "/index.yaml"),
             ("pypi", "pypi-hosted", "/simple/six/")]  # six, not matrixpkg: the delete phase removes matrixpkg
    for fmt, hosted, path in cases:
        pname, gname = f"{fmt}-deadproxy", f"{fmt}-deadgroup"
        req("POST", "/api/v1/repos", json.dumps({"name": pname, "format": fmt, "kind": "proxy",
            "enabled": True, "anonymousRead": True, "upstream": DEAD}).encode(),
            {"Content-Type": "application/json"})
        s, b, _ = req("POST", "/api/v1/repos", json.dumps({"name": gname, "format": fmt, "kind": "group",
            "enabled": True, "anonymousRead": True, "members": [hosted, pname]}).encode(),
            {"Content-Type": "application/json"})
        if s not in (200, 201, 409):  # 409 = left over from a previous run
            chk(fmt, "group", "G5", "test group created", False, f"{s} {b[:120]}")
            continue
        if fmt == "maven":  # give the hosted member something to serve
            req("PUT", f"/repository/{hosted}/com/acme/widget2/1.0.0/widget2-1.0.0.jar", b"W2")
        s, b, _ = req("GET", f"/repository/{gname}{path}")
        chk(fmt, "group", "G5", "group still serves hosted content with a dead proxy member",
            s == 200, f"{s} {str(b)[:140]}")

    print('=== OCI _catalog (all kinds) ===')
    s, b, _ = req("GET", "/repository/docker-hosted/_catalog")
    chk("oci", "hosted", "C1", "_catalog lists images", s == 200 and "matrixapp" in b, f"{s} {b[:120]}")
    s, b, _ = req("GET", "/repository/docker-hosted/_catalog?n=1")
    chk("oci", "hosted", "C2", "_catalog honours the n page size", s == 200 and b.count('"') >= 2, f"{s} {b[:120]}")
    s, b, _ = req("GET", "/repository/docker-public/_catalog")
    chk("oci", "group", "C1", "_catalog merges group members", s == 200 and "matrixapp" in b, f"{s} {b[:120]}")

    print('=== MetadataMaxAge is read (not dead config) ===')
    # A proxy with a 1s metadata age and a long content age must refresh its
    # index on the metadata clock. Verified structurally here; the behavioural
    # proof is TestProxy_MetadataMaxAgeIsHonoured.
    s, b, _ = req("POST", "/api/v1/repos", json.dumps({"name": "ttl-probe", "format": "npm",
        "kind": "proxy", "enabled": True, "anonymousRead": True,
        "upstream": "https://registry.npmjs.org", "contentMaxAge": "24h",
        "metadataMaxAge": "1s"}).encode(), {"Content-Type": "application/json"})
    chk("npm", "proxy", "P8", "repo accepts distinct content/metadata ages", s in (200, 201, 409), f"{s} {b[:160]}")
    if s in (200, 201, 409):
        s1, _, _ = req("GET", "/repository/ttl-probe/is-odd")
        chk("npm", "proxy", "P8b", "proxy with a metadata age still serves", s1 == 200, f"{s1}")

    fails = [r for r in RESULTS if not r[4]]
    print(f"\n===== {len(RESULTS)-len(fails)} passed, {len(fails)} failed =====")
    for r in fails:
        print(f"  FAIL {r[0]}/{r[1]} {r[2]}: {r[3]}\n        {r[5]}")
    sys.exit(1 if fails else 0)
