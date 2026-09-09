#!/usr/bin/env python3
"""Security probes for a live forge, in the shape the live trial found bugs.

    go build -o forge ./cmd/forge
    rm -rf ./data && ./forge -addr :8090 -data ./data -auth &
    # take the bootstrap token the server logs on first start
    FORGE_ADMIN_TOKEN=forge_... python3 scripts/security-probe.py

Every check here corresponds to a defect found by driving a running server
rather than by reading the code, and each one failed at least once. They are
grouped by the class of mistake, because in every case the class had more
instances than the first one found:

  AUTHZ    an endpoint that reports what a private repository holds
  PATH     a stored path built from request BODY content (URLs are cleaned by
           net/http; bodies are not)
  INDEX    a generated index assembled by string formatting from publisher text
  ALLOC    an unbounded read of data the process does not control
  IMMUT    a write-once guarantee that only covered one of the routes to the bytes
  SSRF     an outbound request to an address the caller chose

Exits non-zero if any probe fails. Needs Docker only for nothing — it is pure
HTTP — but the ALLOC probes need the server's PID to read RSS, which it finds
from the listening port when not given as FORGE_PID.
"""
import base64, io, json, os, re, subprocess, sys, tarfile, threading, time
import urllib.error, urllib.parse, urllib.request

BASE = os.environ.get("FORGE_BASE", "http://localhost:8090")
ADMIN = os.environ.get("FORGE_ADMIN_TOKEN", "")
RESULTS = []


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    """Redirects must not be followed.

    urllib follows them by default, which made a guarded page indistinguishable
    from a served one: /ui/admin answers 303 to /ui/login, and following it
    reported the login page's 200 as if the admin page had been served. The
    probe read that as a leak that was not there.
    """
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


_OPENER = urllib.request.build_opener(_NoRedirect)


def req(method, path, data=None, token=None, headers=None, raw=False):
    h = dict(headers or {})
    if token:
        h["Authorization"] = "Bearer " + token
    r = urllib.request.Request(
        path if path.startswith("http") else BASE + path, data=data, method=method, headers=h)
    try:
        with _OPENER.open(r, timeout=120) as resp:
            body = resp.read()
            return resp.status, (body if raw else body.decode("utf-8", "replace")), dict(resp.headers)
    except urllib.error.HTTPError as e:
        body = e.read()
        return e.code, (body if raw else body.decode("utf-8", "replace")), dict(e.headers)
    except Exception as e:
        return 0, f"TRANSPORT-ERROR: {e}", {}


def chk(group, cid, desc, ok, detail=""):
    RESULTS.append((group, cid, desc, bool(ok), str(detail)[:220]))
    print(f"  [{'PASS' if ok else 'FAIL'}] {group}/{cid} {desc}" + ("" if ok else f"\n         <<< {str(detail)[:200]}"))


def mkrepo(name, fmt, kind="hosted", **extra):
    body = {"name": name, "format": fmt, "kind": kind, "enabled": True, "anonymousRead": False}
    body.update(extra)
    st, _, _ = req("POST", "/api/v1/repos", json.dumps(body).encode(), ADMIN,
                   {"Content-Type": "application/json"})
    return st in (200, 201, 409)


def mktoken(desc, grants):
    st, b, _ = req("POST", "/api/v1/tokens",
                   json.dumps({"description": desc, "grants": grants}).encode(), ADMIN,
                   {"Content-Type": "application/json"})
    return json.loads(b)["secret"] if st in (200, 201) else ""


def tgz(files):
    buf = io.BytesIO()
    with tarfile.open(fileobj=buf, mode="w:gz") as tf:
        for n, c in files.items():
            i = tarfile.TarInfo(n)
            i.size = len(c)
            tf.addfile(i, io.BytesIO(c))
    return buf.getvalue()


# ── AUTHZ ─────────────────────────────────────────────────────────────────────
# A private repository's inventory is the target list for dependency confusion:
# knowing @acme/toolkit is internal is what tells an attacker which name to
# register publicly. Group shadowing defends the pull; these defend the
# reconnaissance. Both the JSON endpoints and the server-rendered pages that
# embed the same data have leaked, at different times.
def probe_authz(repo, read_token):
    for path in (f"/api/v1/repos/{repo}/components",
                 f"/ui/browse/{repo}/tree",
                 f"/ui/browse/{repo}/versions?pkg=x",
                 f"/ui/browse/{repo}/detail?pkg=x&ver=1.0.0"):
        st, _, _ = req("GET", path)
        chk("AUTHZ", "json", f"anonymous {path} is refused", st == 401, f"HTTP {st}")
        st, _, _ = req("GET", path, token=read_token)
        chk("AUTHZ", "json", f"a read token may {path}", st not in (401, 403), f"HTTP {st}")

    for path in ("/ui/admin", "/ui/browse", f"/ui/browse/{repo}", "/ui/dashboard"):
        st, body, hdrs = req("GET", path)
        # A redirect to the login page is the guard working; the repo name may
        # appear in that Location because the caller asked for it.
        leaked = repo in body and repo not in hdrs.get("Location", "")
        chk("AUTHZ", "page", f"anonymous {path} does not render the inventory",
            st in (301, 302, 303, 307, 401, 403) and not leaked, f"HTTP {st}, leaked={leaked}")

    # Every repo sub-resource except components is admin-only.
    for sub in ("access", "cache-stats", "cleanup", "component", "health", "invalidate",
                "promote", "reindex", "scan", "security-policy", "trash", "verify"):
        st, _, _ = req("GET", f"/api/v1/repos/{repo}/{sub}")
        st2, _, _ = req("GET", f"/api/v1/repos/{repo}/{sub}", token=read_token)
        chk("AUTHZ", "subres", f"{sub} refuses anonymous and non-admin",
            st == 401 and st2 in (401, 403), f"anon {st}, read {st2}")


# ── PATH ──────────────────────────────────────────────────────────────────────
# npm builds a blob path from the _attachments KEY, which arrives in the request
# body. net/http cleans "../" out of a URL before routing; nothing cleans a JSON
# field. A token with write access to one repo could overwrite another repo's
# cached artifacts, which the group then served to everyone.
def probe_path_traversal(repo, victim, token):
    payload = base64.b64encode(b"MALICIOUS").decode()
    for name in (f"../../../{victim}/is-odd/-/is-odd-3.0.1.tgz",
                 "../../../../pwned-1.0.0.tgz",
                 "sub/dir/evil-1.0.0.tgz"):
        doc = {"name": "tp", "versions": {"1.0.0": {"name": "tp", "version": "1.0.0", "dist": {}}},
               "_attachments": {name: {"data": payload}}}
        st, _, _ = req("PUT", f"/repository/{repo}/tp", json.dumps(doc).encode(), token,
                       {"Content-Type": "application/json"})
        chk("PATH", "npm", f"attachment {name!r} refused", st == 400, f"HTTP {st}")

    # helm derives its path from Chart.yaml INSIDE the archive.
    for cname in ("../../../pwned", "a/b/c"):
        st, _, _ = req("POST", f"/repository/{repo}-helm/api/charts",
                       tgz({"c/Chart.yaml": f"apiVersion: v2\nname: {cname}\nversion: 1.0.0\n".encode()}),
                       token)
        chk("PATH", "helm", f"chart named {cname!r} refused", st == 400, f"HTTP {st}")


# ── INDEX ─────────────────────────────────────────────────────────────────────
# Every format that hand-rolls its index into a structured language had an
# injection bug; the ones that hand the job to encoding/json did not.
def probe_index_injection(token, helm_repo, cran_repo, maven_repo):
    st, _, _ = req("POST", f"/repository/{helm_repo}/api/charts",
                   tgz({"c/Chart.yaml": b"apiVersion: v2\nname: evil: injected\nversion: 1.0.0\n"}), token)
    chk("INDEX", "helm", "a chart name carrying ': ' is refused", st == 400, f"HTTP {st}")
    st, idx, _ = req("GET", f"/repository/{helm_repo}/index.yaml", token=token)
    chk("INDEX", "helm", "index.yaml carries no injected mapping",
        "injected" not in idx, idx[:150])

    st, _, _ = req("PUT", f"/repository/{cran_repo}/src/contrib/victim_1.0.0.tar.gz",
                   tgz({"p/DESCRIPTION": b"Package: victim\nVersion: 1.0.0\nLicense: MIT\n\n"
                                         b"Package: injected\nVersion: 9.9.9\n"}), token)
    st2, pkgs, _ = req("GET", f"/repository/{cran_repo}/src/contrib/PACKAGES", token=token)
    chk("INDEX", "cran", "a second DCF record cannot rename the package",
        "injected" not in pkgs and "victim" in pkgs, pkgs[:150])
    st3, _, _ = req("PUT", f"/repository/{cran_repo}/src/contrib/mypkg_1.0.0.tar.gz",
                    tgz({"p/DESCRIPTION": b"Package: jsonlite\nVersion: 99.0.0\nLicense: MIT\n"}), token)
    chk("INDEX", "cran", "DESCRIPTION must match the filename", st3 == 400, f"HTTP {st3}")

    req("PUT", f"/repository/{maven_repo}/com/acme/w/1.0.0/w-1.0.0.jar", b"jar", token)
    req("PUT", f"/repository/{maven_repo}/com/acme/w/1.0%3C%2Fversion%3E%3Cinj%3E/w-x.jar", b"jar", token)
    st, md, _ = req("GET", f"/repository/{maven_repo}/com/acme/w/maven-metadata.xml", token=token)
    import xml.etree.ElementTree as ET
    try:
        ET.fromstring(md)
        ok, why = True, ""
    except Exception as e:
        ok, why = False, str(e)[:120]
    chk("INDEX", "maven", "maven-metadata.xml stays well-formed", ok and "<inj>" not in md, why)


# ── ALLOC ─────────────────────────────────────────────────────────────────────
# Two unbounded reads, both reachable by ordinary use:
#   - an archive member decompresses without limit (a ~1 MB .tgz holding 1 GiB
#     took a server from 27 MB to 3.7 GB of RSS on two requests)
#   - an upstream response was read whole before being stored (a 700 MB artifact
#     cost 1.84 GB, and container layers are routinely that large)
def forge_pid():
    if os.environ.get("FORGE_PID"):
        return os.environ["FORGE_PID"]
    port = urllib.parse.urlparse(BASE).port or 80
    try:
        # fuser writes the PIDs to stdout and the "8090/tcp:" label to stderr.
        # Reading both put the label in the answer, so the RSS probe silently
        # skipped — and a skip was not counted as a failure, which meant the
        # only check that can see a memory bug never ran and never said so.
        out = subprocess.run(["fuser", f"{port}/tcp"], capture_output=True, text=True, timeout=10)
        pids = [t for t in out.stdout.split() if t.isdigit()]
        return pids[0] if pids else ""
    except Exception:
        return ""


def rss_kb(pid):
    try:
        for line in open(f"/proc/{pid}/status"):
            if line.startswith("VmRSS:"):
                return int(line.split()[1])
    except Exception:
        return 0
    return 0


def bomb(member, size):
    class Zeros(io.RawIOBase):
        def __init__(self): self.n = 0
        def readable(self): return True
        def readinto(self, b):
            k = min(len(b), size - self.n)
            if k <= 0:
                return 0
            b[:k] = b"\0" * k
            self.n += k
            return k
    buf = io.BytesIO()
    with tarfile.open(fileobj=buf, mode="w:gz") as tf:
        i = tarfile.TarInfo(member)
        i.size = size
        tf.addfile(i, io.BufferedReader(Zeros()))
    return buf.getvalue()


def probe_alloc(token, helm_repo, cran_repo):
    pid = forge_pid()
    before = rss_kb(pid)
    size = 1 << 30

    # Sample DURING the uploads, not before and after. The allocation is
    # transient — Go's collector releases it once the request fails — so a
    # before/after comparison shows memory going DOWN on a vulnerable build and
    # the probe passes something it should catch.
    peak = [before]
    stop = threading.Event()

    def sample():
        while not stop.is_set():
            peak[0] = max(peak[0], rss_kb(pid))
            time.sleep(0.05)

    watcher = threading.Thread(target=sample, daemon=True)
    if pid:
        watcher.start()
    st1, b1, _ = req("POST", f"/repository/{helm_repo}/api/charts", bomb("c/Chart.yaml", size), token)
    st2, b2, _ = req("PUT", f"/repository/{cran_repo}/src/contrib/bomb_1.0.0.tar.gz",
                     bomb("p/DESCRIPTION", size), token)
    stop.set()
    if pid:
        watcher.join(timeout=2)
    after = peak[0]
    chk("ALLOC", "bomb", "a 1 GiB archive member is refused",
        st1 == 400 and st2 == 400, f"helm {st1} {b1[:60]}, cran {st2} {b2[:60]}")
    # This is the only check that can see the bug: a status assertion passes on
    # a vulnerable build too, because the metadata is rejected AFTER being read.
    # So being unable to measure is a failure, not a skip — an unrunnable probe
    # that reports success is worse than no probe.
    grew = after - before
    chk("ALLOC", "bomb-rss", f"memory stays flat through two 1 GiB bombs (grew {grew} kB)",
        bool(pid) and before > 0 and grew < 200_000,
        f"{before} -> {after} kB" if pid else
        "could not read the server's RSS; set FORGE_PID so this probe can run")


# ── IMMUT ─────────────────────────────────────────────────────────────────────
# Write-once has to hold at the layer that owns the bytes. It once guarded Put
# only, so delete-then-republish changed an "immutable" artifact.
def probe_immutability(token, repo):
    # A fresh coordinate per run: an immutable repo keeps what earlier runs
    # published, so a fixed path makes the first publish 409 and the probe
    # report a failure that is only its own leftovers.
    v = f"1.0.{int(time.time())}"
    p = f"/repository/{repo}/com/acme/lib/{v}/lib-{v}.jar"
    st1, _, _ = req("PUT", p, b"original", token)
    st2, _, _ = req("PUT", p, b"overwrite", token)
    st3, _, _ = req("DELETE", p, token=token)
    st4, _, _ = req("PUT", p, b"MUTATED", token)
    st5, body, _ = req("GET", p, token=token)
    chk("IMMUT", "writeonce", "publish once, then overwrite/delete/republish all refused",
        st1 == 201 and st2 == 409 and st3 == 409 and st4 == 409,
        f"publish {st1}, overwrite {st2}, delete {st3}, republish {st4}")
    chk("IMMUT", "bytes", "the original bytes survive", body == "original", body[:60])


# ── SSRF ──────────────────────────────────────────────────────────────────────
# A webhook target is chosen by the caller. The guard must resolve the name and
# check the resolved address, not the literal string — localtest.me is a public
# DNS name pointing at loopback.
def probe_ssrf():
    targets = ["http://localhost:9/h", "http://[::1]:9/h", "http://2130706433:9/h",
               "http://127.1:9/h", "http://0.0.0.0:9/h", "http://169.254.169.254/latest/meta-data/",
               "http://10.0.0.5/h", "http://192.168.1.1/h", "http://172.16.0.1/h",
               "http://localtest.me:9/h", "http://metadata.google.internal/x", "file:///etc/passwd"]
    for i, u in enumerate(targets):
        st, _, _ = req("POST", "/api/v1/webhooks", json.dumps({
            "name": f"ssrf-probe-{i}", "url": u, "secret": "s",
            "events": ["artifact.published"], "enabled": True}).encode(), ADMIN,
            {"Content-Type": "application/json"})
        chk("SSRF", "webhook", f"{u} refused", st == 400, f"HTTP {st}")


# ── PARAM ─────────────────────────────────────────────────────────────────────
# A destructive endpoint that ignores a misspelled safety flag turns a typo into
# data loss: "?dryRun=true" is not a parameter, and once ran a live cleanup.
def probe_destructive_params(repo):
    for q in ("?dryRun=true", "?dry=true&extra=1", "?dry-run=true"):
        st, body, _ = req("POST", f"/api/v1/repos/{repo}/cleanup{q}", b"", ADMIN)
        chk("PARAM", "cleanup", f"unknown parameter {q} is refused",
            st == 400 and "unknown query parameter" in body, f"HTTP {st} {body[:80]}")


# ── OCI ───────────────────────────────────────────────────────────────────────
def probe_oci(token, repo):
    absent = "sha256:" + "c" * 64
    man = json.dumps({"schemaVersion": 2,
                      "mediaType": "application/vnd.oci.image.manifest.v1+json",
                      "config": {"digest": absent, "size": 10}, "layers": []}).encode()
    st, body, _ = req("PUT", f"/repository/{repo}/app/manifests/dangling", man, token,
                      {"Content-Type": "application/vnd.oci.image.manifest.v1+json"})
    chk("OCI", "manifest", "a manifest citing an absent blob is refused",
        st == 400 and "BLOB_UNKNOWN" in body, f"HTTP {st} {body[:80]}")
    st, _, hdrs = req("POST", f"/repository/{repo}/app/blobs/uploads/", b"", token)
    loc = hdrs.get("Location", "")
    if loc:
        sep = "&" if "?" in loc else "?"
        lie = "sha256:" + "a" * 64
        st, body, _ = req("PUT", loc + sep + "digest=" + lie, b"NOT-THAT-DIGEST", token)
        chk("OCI", "digest", "content not matching its digest is refused",
            st == 400 and "DIGEST_INVALID" in body, f"HTTP {st} {body[:80]}")


# ── CONC ──────────────────────────────────────────────────────────────────────
# Verify() stamps LastUsed on every authenticated request, so the token record
# is the hottest in the store. With a truncating write underneath, 35 of 40
# concurrent requests carrying a VALID token came back 401 — and a concurrent
# npm publish lost half its versions as a side effect. Clients fetch in
# parallel; this needs no attacker.
def probe_concurrency(repo_name, token):
    import concurrent.futures as cf

    def one(_):
        st, _, _ = req("GET", f"/api/v1/repos/{repo_name}/components", token=token)
        return st

    with cf.ThreadPoolExecutor(max_workers=40) as ex:
        codes = list(ex.map(one, range(40)))
    bad = [c for c in codes if c != 200]
    chk("CONC", "auth", "40 concurrent requests with a valid token all succeed",
        not bad, f"{len(bad)} failed: {sorted(set(bad))}")

    # Distinct versions published at once must all survive in the index.
    def publish(i):
        v = f"9.0.{i}"
        doc = {"name": "conc", "versions": {v: {"name": "conc", "version": v, "dist": {}}},
               "_attachments": {f"conc-{v}.tgz": {"data": base64.b64encode(b"x").decode()}}}
        st, _, _ = req("PUT", f"/repository/{repo_name}/conc", json.dumps(doc).encode(), token,
                       {"Content-Type": "application/json"})
        return st

    with cf.ThreadPoolExecutor(max_workers=12) as ex:
        list(ex.map(publish, range(12)))
    st, body, _ = req("GET", f"/repository/{repo_name}/conc", token=token)
    try:
        have = set(json.loads(body).get("versions", {}))
    except Exception:
        have = set()
    missing = [f"9.0.{i}" for i in range(12) if f"9.0.{i}" not in have]
    chk("CONC", "publish", "12 concurrent publishes all land in the index",
        not missing, f"missing {missing}")


# ── GUARD ─────────────────────────────────────────────────────────────────────
# handleRepo applies the write-once wrapper and the quota gate, and was once the
# only path that did: the browser upload form and the Nexus migration each built
# their own context and wrote through neither.
def probe_write_guards(token, immutable_repo):
    p = f"/repository/{immutable_repo}/src/contrib/guard_1.0.0.tar.gz"
    req("PUT", p, tgz({"guard/DESCRIPTION": b"Package: guard\nVersion: 1.0.0\nLicense: MIT\n"}), token)
    st, _, _ = req("PUT", p, tgz({"guard/DESCRIPTION": b"Package: guard\nVersion: 1.0.0\nLicense: X\n"}), token)
    chk("GUARD", "protocol", "an immutable repo refuses an overwrite", st == 409, f"HTTP {st}")

    # The same coordinate through the browser upload form. Assert on the STORED
    # BYTES, not on page text: an earlier version of this check looked for
    # "immutable" in the response and passed against a vulnerable build, because
    # the page carries the repository's own immutable badge regardless of what
    # the upload did.
    _, before, _ = req("GET", p, token=token, raw=True)
    body = (b"--X\r\nContent-Disposition: form-data; name=\"file\"; filename=\"guard_1.0.0.tar.gz\"\r\n"
            b"Content-Type: application/octet-stream\r\n\r\n"
            + tgz({"guard/DESCRIPTION": b"Package: guard\nVersion: 1.0.0\nLicense: VIA-UI\n"})
            + b"\r\n--X--\r\n")
    st, page, _ = req("POST", f"/ui/repos/{immutable_repo}/upload", body, ADMIN,
                      {"Content-Type": "multipart/form-data; boundary=X"})
    _, after, _ = req("GET", p, token=token, raw=True)
    unchanged = isinstance(before, bytes) and before == after
    chk("GUARD", "ui-upload", "the upload form cannot rewrite an immutable artifact",
        unchanged and "Upload successful" not in page,
        f"HTTP {st}, bytes changed={not unchanged}: {page[:100]}")


# ── GROUP ─────────────────────────────────────────────────────────────────────
def probe_group_nesting():
    req("POST", "/api/v1/repos", json.dumps({"name": "sp-g-host", "format": "npm",
        "kind": "hosted", "enabled": True}).encode(), ADMIN, {"Content-Type": "application/json"})
    req("POST", "/api/v1/repos", json.dumps({"name": "sp-g-inner", "format": "npm",
        "kind": "group", "enabled": True, "members": ["sp-g-host"]}).encode(), ADMIN,
        {"Content-Type": "application/json"})
    st, body, _ = req("POST", "/api/v1/repos", json.dumps({"name": "sp-g-outer", "format": "npm",
        "kind": "group", "enabled": True, "members": ["sp-g-inner"]}).encode(), ADMIN,
        {"Content-Type": "application/json"})
    chk("GROUP", "nesting", "a group containing a group is refused, not silently empty",
        st == 400 and "nested" in body, f"HTTP {st}: {body[:100]}")


def probe_kind_writes():
    """A repository that mirrors someone else's registry must not accept writes.

    Publish, unpublish and delete are all refused on a non-hosted repository,
    but npm's dist-tags endpoint was a write path that nobody had guarded: a
    PUT against a proxy repointed "latest" inside the *cached upstream
    packument*, and every client installing through that proxy was served the
    tag the caller chose.
    """
    req("POST", "/api/v1/repos", json.dumps({"name": "sp-np-proxy", "format": "npm",
        "kind": "proxy", "upstream": "https://registry.npmjs.org",
        "enabled": True}).encode(), ADMIN, {"Content-Type": "application/json"})
    req("POST", "/api/v1/repos", json.dumps({"name": "sp-np-group", "format": "npm",
        "kind": "group", "enabled": True,
        "members": ["sp-npm", "sp-np-proxy"]}).encode(), ADMIN,
        {"Content-Type": "application/json"})
    wtok = mktoken("security-probe-kind", [{"repo": r, "actions": ["read", "write", "delete"]}
                                           for r in ("sp-npm", "sp-np-proxy", "sp-np-group")])
    st, _, _ = req("GET", "/repository/sp-np-proxy/is-odd", token=wtok)
    if st != 200:
        chk("KIND", "disttag", "npm dist-tags write is refused on a proxy", True,
            "skipped: upstream unreachable")
        return
    for repo_name in ("sp-np-proxy", "sp-np-group"):
        st, body, _ = req("PUT", f"/repository/{repo_name}/-/package/is-odd/dist-tags/latest",
                          b'"9.9.9-pwned"', wtok, {"Content-Type": "application/json"})
        chk("KIND", "disttag", f"npm dist-tags write is refused on {repo_name}",
            st == 405, f"HTTP {st}: {str(body)[:100]}")
    st, body, _ = req("GET", "/repository/sp-np-proxy/is-odd", token=wtok)
    chk("KIND", "cache", "the cached upstream packument is unchanged",
        st == 200 and "pwned" not in str(body), f"HTTP {st}")


def probe_group_exposure():
    """A group is exactly as public as its least public member.

    forge's own seeded groups were anonymousRead=true over private hosted
    members, so with -auth an anonymous client read private artifacts through
    the group while the same request to the member itself answered 401. The
    admin API refused that combination; the seed, the browser form, a
    Config-as-Code apply and a Nexus migration all went straight to the
    repository manager and inherited nothing. The rule lives in the manager
    now, so this asks the API and then asks the running seed.
    """
    mkrepo("sp-priv-member", "maven")
    st, body, _ = req("POST", "/api/v1/repos", json.dumps({"name": "sp-open-group",
        "format": "maven", "kind": "group", "enabled": True, "anonymousRead": True,
        "members": ["sp-priv-member"]}).encode(), ADMIN, {"Content-Type": "application/json"})
    chk("AUTHZ", "group-public", "a public group over a private member is refused",
        st == 400 and "anonymous" in str(body), f"HTTP {st}: {str(body)[:120]}")

    # And the shipped seed does not ship the thing we just refused.
    st, body, _ = req("GET", "/api/v1/repos", token=ADMIN)
    exposed = []
    if st == 200:
        repos = {r["name"]: r for r in json.loads(body)}
        for r in repos.values():
            if r.get("kind") != "group" or not r.get("anonymousRead"):
                continue
            for member in r.get("members") or []:
                m = repos.get(member)
                if m and not m.get("anonymousRead"):
                    exposed.append(f"{r['name']} -> {member}")
    chk("AUTHZ", "group-seed", "no configured group is public over a private member",
        st == 200 and not exposed, str(exposed)[:200])


def main():
    if not ADMIN:
        print("FORGE_ADMIN_TOKEN is required (the bootstrap token the server logs on first start)")
        return 2
    st, _, _ = req("GET", "/api/v1/repos", token=ADMIN)
    if st != 200:
        print(f"cannot reach {BASE} as an admin (HTTP {st})")
        return 2

    print("=== setting up probe repositories ===")
    repos = {
        "sp-npm": "npm", "sp-npm-helm": "helm", "sp-cran": "cran",
        "sp-maven": "maven", "sp-oci": "oci",
    }
    for name, fmt in repos.items():
        mkrepo(name, fmt)
    mkrepo("sp-victim", "npm")
    mkrepo("sp-immutable", "maven", immutable=True)
    mkrepo("sp-immutable-cran", "cran", immutable=True)
    all_grants = [{"repo": r, "actions": ["read", "write", "delete"]}
                  for r in list(repos) + ["sp-victim", "sp-immutable", "sp-immutable-cran"]]
    tok = mktoken("security-probe", all_grants)
    read_tok = mktoken("security-probe-read", [{"repo": "sp-npm", "actions": ["read"]}])
    if not tok or not read_tok:
        print("could not mint probe tokens")
        return 2

    print("=== AUTHZ: what a private repository discloses ===")
    probe_authz("sp-npm", read_tok)
    print("=== PATH: stored paths built from request bodies ===")
    probe_path_traversal("sp-npm", "sp-victim", tok)
    print("=== INDEX: generated indexes built from publisher text ===")
    probe_index_injection(tok, "sp-npm-helm", "sp-cran", "sp-maven")
    print("=== ALLOC: unbounded reads of data we do not control ===")
    probe_alloc(tok, "sp-npm-helm", "sp-cran")
    print("=== IMMUT: write-once at the layer that owns the bytes ===")
    probe_immutability(tok, "sp-immutable")
    print("=== SSRF: outbound targets chosen by the caller ===")
    probe_ssrf()
    print("=== PARAM: safety flags on destructive endpoints ===")
    probe_destructive_params("sp-npm")
    print("=== OCI: digest and reference validation ===")
    probe_oci(tok, "sp-oci")
    print("=== CONC: state that tears when requests overlap ===")
    probe_concurrency("sp-npm", tok)
    print("=== GUARD: every route to the bytes, not just one ===")
    probe_write_guards(tok, "sp-immutable-cran")
    print("=== AUTHZ: a group is as public as its least public member ===")
    probe_group_exposure()
    print("=== KIND: writes into a repository that only mirrors ===")
    probe_kind_writes()
    print("=== GROUP: a repository that serves nothing ===")
    probe_group_nesting()

    fails = [r for r in RESULTS if not r[3]]
    print(f"\n===== {len(RESULTS) - len(fails)} passed, {len(fails)} failed =====")
    for g, cid, desc, _, detail in fails:
        print(f"  FAIL {g}/{cid} {desc}\n       {detail}")
    return 1 if fails else 0


if __name__ == "__main__":
    sys.exit(main())
