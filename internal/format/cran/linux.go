package cran

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"forge/internal/blob"
	"forge/internal/cranplatform"
	"forge/internal/format"
	"forge/internal/ledger"
	"forge/internal/meta"
	"forge/internal/repo"
)

// Linux binary packages (WORKPLAN-CRAN-LINUX.md). A binary repository — a
// hosted CRAN repository with noOverwrite and a sourceRepo — holds builds a
// trusted builder uploads, one per platform key:
//
//	bin/linux/{distro}-{arch}/contrib/{rminor}/{pkg}_{ver}.tar.gz
//
// so the platform segment is "linux/{distro}-{arch}" and the key is
// "{distro}-{arch}/{rminor}".

// BuilderDigestHeader carries the builder image digest an upload was built in.
// It is self-reported and so advisory, but it must equal the digest the key
// was registered with: a builder whose image moved on without the registration
// following is refused instead of quietly publishing builds from elsewhere.
const BuilderDigestHeader = "X-Forge-Builder-Digest"

// LinkingToHeader optionally carries the versions of the packages the build
// compiled against through LinkingTo ("Rcpp 1.0.14, BH 1.87.0-1"), as the
// builder had them. Recorded for diagnosis: a binary built against one
// package's headers can break when that package changes ABI (workplan §7).
const LinkingToHeader = "X-Forge-Linking-To"

// linuxProvenance is what forge records about how a Linux binary arrived.
type linuxProvenance struct {
	Key           string   `json:"key"`
	Uploader      string   `json:"uploader,omitempty"`
	BuilderImage  string   `json:"builderImage"`
	BuilderDigest string   `json:"builderDigest"`
	SourceRepo    string   `json:"sourceRepo"`
	SourceSHA256  string   `json:"sourceSha256"`
	SHA256        string   `json:"sha256"`
	LinkingTo     string   `json:"linkingTo,omitempty"`
	Needs         []string `json:"needs,omitempty"`
	MaxGlibc      string   `json:"maxGlibc,omitempty"`
}

// serveLinuxBinary handles every request under bin/linux/.
func (h *Handler) serveLinuxBinary(w http.ResponseWriter, r *http.Request, c *format.Context, platform, rver, file string) {
	switch {
	case r.Method == http.MethodGet && (file == "PACKAGES" || file == "PACKAGES.gz" || file == "PACKAGES.rds"):
		h.serveBinIndex(w, c, platform, rver, file)
	case !strings.HasSuffix(file, ".tar.gz"):
		http.Error(w, "Linux binary packages are {package}_{version}.tar.gz", http.StatusNotFound)
	case r.Method == http.MethodPut:
		h.publishLinuxBin(w, r, c, platform, rver, file)
	case r.Method == http.MethodGet || r.Method == http.MethodHead:
		h.downloadBin(w, r, c)
	case r.Method == http.MethodDelete:
		h.deleteBin(w, c, platform, rver, file)
	default:
		http.Error(w, "unsupported binary request", http.StatusMethodNotAllowed)
	}
}

// publishLinuxBin accepts one validated Linux binary. The checks run cheapest
// first, so a builder racing another for the same key learns of the 409
// before forge fetches any source.
func (h *Handler) publishLinuxBin(w http.ResponseWriter, r *http.Request, c *format.Context, platform, rver, file string) {
	keyStr := strings.TrimPrefix(platform, "linux/") + "/" + rver
	// ParseKey also refuses a nested platform ("linux/a-x86_64/b"): the extra
	// segment lands in the R version, which must be a bare minor.
	if _, err := cranplatform.ParseKey(keyStr); err != nil {
		refuseLinux(w, c, "not a Linux binary path: want bin/linux/{distro}-{arch}/contrib/{rminor}/{pkg}_{ver}.tar.gz", http.StatusBadRequest)
		return
	}
	pkg, ver, ok := nameVersionFromFilename(file)
	if !ok || pkg == "" || ver == "" {
		refuseLinux(w, c, "cannot read package and version from "+file, http.StatusBadRequest)
		return
	}
	reg, ok, err := cranplatform.NewStore(c.Meta).Get(keyStr)
	if err != nil {
		refuseLinux(w, c, err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		refuseLinux(w, c, "platform key "+keyStr+" is not registered; an admin registers it at /api/v1/cran/platforms/"+keyStr,
			http.StatusForbidden)
		return
	}
	if !c.Repo.RefusesOverwrite() {
		refuseLinux(w, c, "repository "+c.Repo.Name+" does not set noOverwrite; a Linux binary must never be replaced in place, so this repository cannot hold them",
			http.StatusForbidden)
		return
	}
	if c.Repo.SourceRepo == "" {
		refuseLinux(w, c, "repository "+c.Repo.Name+" has no sourceRepo to check binaries against", http.StatusForbidden)
		return
	}
	switch digest := r.Header.Get(BuilderDigestHeader); {
	case digest == "":
		refuseLinux(w, c, BuilderDigestHeader+" is required: the digest of the builder image this was built in", http.StatusBadRequest)
		return
	case digest != reg.BuilderDigest:
		refuseLinux(w, c, fmt.Sprintf("built in %s, but %s is registered for builder image %s (%s)",
			digest, keyStr, reg.BuilderDigest, reg.BuilderImage), http.StatusForbidden)
		return
	}
	if _, exists, _ := c.Blob.Stat(c.Key(c.Sub)); exists {
		refuseLinux(w, c, blob.ErrExists.Error(), http.StatusConflict)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		refuseLinux(w, c, err.Error(), http.StatusBadRequest)
		return
	}
	linkingTo := r.Header.Get(LinkingToHeader)
	if len(linkingTo) > 4096 || strings.ContainsAny(linkingTo, "\r\n") {
		refuseLinux(w, c, LinkingToHeader+" must be one line under 4 KiB", http.StatusBadRequest)
		return
	}
	srcDesc, srcSum, status, msg := h.linuxSourceDescription(c, pkg, ver)
	if status != 0 {
		refuseLinux(w, c, msg, status)
		return
	}
	bin, err := validateLinuxBinary(body, pkg, ver, reg, srcDesc)
	if errors.Is(err, errBadBinary) {
		refuseLinux(w, c, err.Error(), http.StatusBadRequest)
		return
	}
	if err != nil {
		refuseLinux(w, c, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := c.Blob.Put(c.Key(c.Sub), bytes.NewReader(body)); err != nil {
		if blob.Refused(err) {
			refuseLinux(w, c, err.Error(), http.StatusConflict)
			return
		}
		refuseLinux(w, c, err.Error(), http.StatusInternalServerError)
		return
	}
	rec := recordFromFields(bin.Fields)
	rec.UploadedAt = time.Now().UTC()
	sum := sha256.Sum256(body)
	rec.Linux = &linuxProvenance{
		Key: keyStr, Uploader: c.Actor, BuilderImage: reg.BuilderImage, BuilderDigest: reg.BuilderDigest,
		SourceRepo: c.Repo.SourceRepo, SourceSHA256: srcSum, SHA256: hex.EncodeToString(sum[:]),
		LinkingTo: linkingTo, Needs: bin.Needed, MaxGlibc: bin.MaxGlibc,
	}
	c.Meta.PutJSON(h.binNS(c, platform, rver), rec.Package+"_"+rec.Version, rec) //nolint:errcheck
	ledger.RecordAt(c.Meta, c.Repo.Name, rec.Package, rec.Version, rec.UploadedAt)
	w.WriteHeader(http.StatusCreated)
	fmt.Fprintf(w, "stored Linux binary %s %s for %s\n", pkg, ver, keyStr)
}

// refuseLinux answers a refused Linux binary upload and, unless the refusal
// is a lost race (409) or forge's own trouble (5xx), reports it through
// c.Reject: a rejected build is something an operator needs to see, not only
// the builder that sent it.
func refuseLinux(w http.ResponseWriter, c *format.Context, msg string, status int) {
	if c.Reject != nil && status != http.StatusConflict && status < 500 {
		c.Reject(status, "cran-linux-binary", msg)
	}
	http.Error(w, msg, status)
}

// linuxSourceDescription returns the DESCRIPTION of the source package forge
// serves for pkg@ver through the binary repository's sourceRepo — the current
// path first, then CRAN's Archive. On failure it returns the HTTP status and
// message to refuse the upload with. It also returns the source tarball's
// sha256, for provenance.
func (h *Handler) linuxSourceDescription(c *format.Context, pkg, ver string) ([]byte, string, int, string) {
	if c.ReadRepo == nil {
		return nil, "", http.StatusInternalServerError, "source check unavailable: this server cannot read " + c.Repo.SourceRepo
	}
	file := pkg + "_" + ver + ".tar.gz"
	upstreamFailed := false
	for _, sub := range []string{"src/contrib/" + file, "src/contrib/Archive/" + pkg + "/" + file} {
		status, body := c.ReadRepo(c.Repo.SourceRepo, sub)
		switch {
		case status >= 200 && status < 300:
			desc, err := rawDescription(body)
			if err != nil {
				return nil, "", http.StatusBadGateway, "the source package " + file + " from " + c.Repo.SourceRepo + " is unreadable: " + err.Error()
			}
			sum := sha256.Sum256(body)
			return desc, hex.EncodeToString(sum[:]), 0, ""
		case status >= 500:
			upstreamFailed = true
		}
	}
	if upstreamFailed {
		return nil, "", http.StatusBadGateway, "could not fetch the source of " + file + " from " + c.Repo.SourceRepo + " to check the binary against; retry"
	}
	return nil, "", http.StatusBadRequest, c.Repo.SourceRepo + " serves no source package " + file + ", so there is nothing to check this binary against"
}

// --- Serving: one URL, binary or source ------------------------------------
//
// A group serves PPM's client-facing layout (docs/notes/cran-linux-clients.md):
//
//	__linux__/{distro}/latest/src/contrib/PACKAGES[.gz|.rds]       source index
//	__linux__/{distro}/latest/src/contrib/{pkg}_{ver}.tar.gz        binary or source
//	__linux__/{distro}/latest/src/contrib/Archive/{pkg}/{file}      binary or source
//	latest/bin/linux/{rminor}-{distro}/contrib/{rminor}/PACKAGES*   pak's binary list
//
// The distro comes from the path; the R minor version, arch and platform
// triple from the "R (x.y.z triple arch os)" part of the User-Agent, which
// renv, pak and a configured install.packages all send. resolveLinux is the
// only place that decides binary or source, and every path above asks it.

// rUserAgent matches the part of an R client's User-Agent that names its
// platform: "R (4.6.1 x86_64-pc-linux-gnu x86_64 linux-gnu)". renv prefixes
// it ("renv (1.3.1); R/4.6.1 R (…)"), rocker's profile adds "R/4.6.1 ".
var rUserAgent = regexp.MustCompile(`(?:^|[\s;])R \(([0-9]+)\.([0-9]+)\.[0-9]+ ([A-Za-z0-9_.-]+) ([A-Za-z0-9_]+) ([A-Za-z0-9_.-]+)\)`)

// linuxClient is a client's platform as its User-Agent reports it.
type linuxClient struct {
	RMinor, Platform, Arch string
}

// parseRUserAgent reads the client platform from ua. ok is false for anything
// that is not an R client on Linux saying which R and which CPU it is — a
// bare "libcurl/8.5.0" (what stock R sends, L0), curl, wget, a browser.
func parseRUserAgent(ua string) (linuxClient, bool) {
	m := rUserAgent.FindStringSubmatch(ua)
	if m == nil || m[5] != "linux-gnu" || !strings.HasPrefix(m[3], m[4]+"-") {
		return linuxClient{}, false
	}
	return linuxClient{RMinor: m[1] + "." + m[2], Platform: m[3], Arch: m[4]}, true
}

// linuxPlatform resolves the registered platform a request is for, or ok
// false when it cannot be resolved completely — and then the client gets
// source. Failing toward source is the rule: source is slow, a wrong binary
// is broken (and renv does not fall back from one).
func linuxPlatform(c *format.Context, r *http.Request, distro string) (cranplatform.Platform, bool) {
	cl, ok := parseRUserAgent(r.UserAgent())
	if !ok {
		return cranplatform.Platform{}, false
	}
	// The registration found must be for exactly this key. A meta store on a
	// case-insensitive filesystem (macOS, Windows) answers "Noble-x86_64/4.6"
	// with the noble registration, which would serve noble's binaries to a
	// distro forge never registered — found by macOS CI.
	key := distro + "-" + cl.Arch + "/" + cl.RMinor
	p, ok, err := cranplatform.NewStore(c.Meta).Get(key)
	if err != nil || !ok || p.Key != key || p.RPlatform != cl.Platform {
		return cranplatform.Platform{}, false
	}
	return p, true
}

// linuxBinSub is where a binary of pkg@ver for platform p lives in its repo.
func linuxBinSub(p cranplatform.Platform, pkg, ver string) string {
	k, _ := cranplatform.ParseKey(p.Key)
	return "bin/linux/" + k.Distro + "-" + k.Arch + "/contrib/" + k.RMinor + "/" + pkg + "_" + ver + ".tar.gz"
}

// linuxBinaryRepos returns the repositories whose binaries this group may
// serve: hosted members whose sourceRepo is this group. Membership keeps the
// group's read rules (a group is never more public than a member); the
// sourceRepo match means every binary was validated against exactly the
// source this group serves. Only groups have members, and only hosted
// repositories can name a sourceRepo, so anything but a group has none.
func linuxBinaryRepos(c *format.Context) []*format.Context {
	var out []*format.Context
	for _, name := range c.Repo.Members {
		mc, ok := c.MemberCtx(name)
		if ok && mc.Repo.SourceRepo == c.Repo.Name {
			out = append(out, mc)
		}
	}
	return out
}

// resolveLinux is the decision: the member context (with Sub set) holding the
// binary of pkg@ver for the platform this request is for, or nil to serve
// source.
func resolveLinux(c *format.Context, r *http.Request, distro, pkg, ver string) *format.Context {
	p, ok := linuxPlatform(c, r, distro)
	if !ok {
		return nil
	}
	return linuxBinaryIn(linuxBinaryRepos(c), p, pkg, ver)
}

func linuxBinaryIn(repos []*format.Context, p cranplatform.Platform, pkg, ver string) *format.Context {
	sub := linuxBinSub(p, pkg, ver)
	for _, mc := range repos {
		if _, ok, _ := mc.Blob.Stat(mc.Key(sub)); ok {
			mc.Sub = sub
			return mc
		}
	}
	return nil
}

// serveLinuxURL serves __linux__/{distro}/latest/… .
func (h *Handler) serveLinuxURL(w http.ResponseWriter, r *http.Request, c *format.Context) {
	parts := strings.SplitN(strings.TrimPrefix(c.Sub, "__linux__/"), "/", 3)
	if len(parts) < 3 || parts[1] != "latest" || !strings.HasPrefix(parts[2], "src/contrib/") {
		// forge keeps no snapshots, so a date is not quietly "latest".
		http.Error(w, "want __linux__/{distro}/latest/src/contrib/…", http.StatusNotFound)
		return
	}
	distro, srcSub := parts[0], parts[2]
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "read-only path: upload binaries to the binary repository", http.StatusMethodNotAllowed)
		return
	}
	pkg, ver, isTarball := linuxTarball(srcSub)
	if !isTarball {
		// The index is the plain source index, as PPM serves it (L0): every
		// package resolves, and each tarball request picks binary or source.
		h.serveSub(w, r, c, srcSub)
		return
	}
	w.Header().Set("Vary", "User-Agent")
	if mc := resolveLinux(c, r, distro, pkg, ver); mc != nil {
		w.Header().Set("X-Forge-Package-Type", "binary")
		h.downloadBin(w, r, mc)
		return
	}
	w.Header().Set("X-Forge-Package-Type", "source")
	h.serveSub(w, r, c, srcSub)
}

// linuxTarball recognises src/contrib/{pkg}_{ver}.tar.gz and
// src/contrib/Archive/{pkg}/{pkg}_{ver}.tar.gz.
func linuxTarball(srcSub string) (pkg, ver string, ok bool) {
	rest := strings.TrimPrefix(srcSub, "src/contrib/")
	if a, ok := strings.CutPrefix(rest, "Archive/"); ok {
		dir, file, found := strings.Cut(a, "/")
		if !found || strings.Contains(file, "/") {
			return "", "", false
		}
		pkg, ver, ok = nameVersionFromFilename(file)
		return pkg, ver, ok && pkg == dir
	}
	if strings.Contains(rest, "/") {
		return "", "", false
	}
	return nameVersionFromFilename(rest)
}

// serveSub serves sub of this repository as though it had been requested.
func (h *Handler) serveSub(w http.ResponseWriter, r *http.Request, c *format.Context, sub string) {
	cc := *c
	cc.Sub = sub
	h.Serve(w, r, &cc)
}

// servePakIndex serves latest/bin/linux/{rminor}-{distro}/contrib/{rminor}/
// PACKAGES[.gz|.rds]: the packages pak may treat as binary. pak marks a
// package binary by NAME (pkgcache: pkgs$package %in% bin$Package), for every
// version the source index lists — so a package is listed only when every
// one of those versions resolves to a binary. Listing it otherwise makes pak
// expect a binary, receive source, and fail where there is no compiler (L0).
func (h *Handler) servePakIndex(w http.ResponseWriter, r *http.Request, c *format.Context) {
	rest := strings.TrimPrefix(c.Sub, "latest/bin/linux/")
	seg, after, ok1 := strings.Cut(rest, "/contrib/")
	rminor, file, ok2 := strings.Cut(after, "/")
	pathMinor, distro, ok3 := strings.Cut(seg, "-")
	if !ok1 || !ok2 || !ok3 || pathMinor != rminor || (file != "PACKAGES" && file != "PACKAGES.gz" && file != "PACKAGES.rds") {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Vary", "User-Agent")
	var recs []pkgRecord
	if p, ok := linuxPlatform(c, r, distro); ok && strings.HasSuffix(p.Key, "/"+rminor) {
		recs = h.pakBinaryRecords(c, p)
	}
	switch file {
	case "PACKAGES":
		w.Header().Set("Content-Type", "text/plain")
		w.Write(buildPackages(recs)) //nolint:errcheck
	case "PACKAGES.gz":
		w.Header().Set("Content-Type", "application/gzip")
		gz := gzip.NewWriter(w)
		gz.Write(buildPackages(recs)) //nolint:errcheck
		gz.Close()
	case "PACKAGES.rds":
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(buildPackagesRDS(recs)) //nolint:errcheck
	}
}

// pakBinaryRecords lists the source records of packages whose every indexed
// version has a binary for p. Listing a repository's binaries only narrows the
// candidates; each version is decided by linuxBinaryIn, as a download would be.
func (h *Handler) pakBinaryRecords(c *format.Context, p cranplatform.Platform) []pkgRecord {
	repos := linuxBinaryRepos(c)
	k, _ := cranplatform.ParseKey(p.Key)
	prefix := "bin/linux/" + k.Distro + "-" + k.Arch + "/contrib/" + k.RMinor + "/"
	candidates := map[string]bool{}
	for _, mc := range repos {
		keys, _ := mc.Blob.List(mc.Key(prefix))
		for _, key := range keys {
			if pkg, _, ok := cranBinaryParts(key); ok {
				candidates[pkg] = true
			}
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	byName := map[string][]pkgRecord{}
	for _, rec := range h.allPkgRecords(c) {
		if candidates[rec.Package] {
			byName[rec.Package] = append(byName[rec.Package], rec)
		}
	}
	var out []pkgRecord
	for _, recs := range byName {
		all := true
		for _, rec := range recs {
			if linuxBinaryIn(repos, p, rec.Package, rec.Version) == nil {
				all = false
				break
			}
		}
		if all {
			out = append(out, recs...)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Package < out[j].Package })
	return out
}

// PPMStatus implements format.PPMStatusProvider: the distros and R minor
// versions with a registered platform key. Listing a distro makes renv rewrite
// {repo}/latest to {repo}/__linux__/{distro}/latest, which every CRAN
// repository serves — binaries where a group has them, source otherwise — so
// the rewrite can never point a client at a path that does not exist.
func (h *Handler) PPMStatus(m meta.Store) (distros, rVersions []string) {
	ps, err := cranplatform.NewStore(m).List()
	if err != nil {
		return nil, nil
	}
	seenD, seenR := map[string]bool{}, map[string]bool{}
	for _, p := range ps {
		k, err := cranplatform.ParseKey(p.Key)
		if err != nil {
			continue
		}
		if !seenD[k.Distro] {
			seenD[k.Distro] = true
			distros = append(distros, k.Distro)
		}
		if !seenR[k.RMinor] {
			seenR[k.RMinor] = true
			rVersions = append(rVersions, k.RMinor)
		}
	}
	sort.Strings(distros)
	sort.Sort(sort.Reverse(sort.StringSlice(rVersions)))
	return distros, rVersions
}

// linuxBuilds returns the Linux binaries of pkg this repository shows, by
// version: its own, for a binary repository, and for a group the binaries of
// the members it serves them from. meta is one of their index records, for a
// package that has no source here.
func (h *Handler) linuxBuilds(c *format.Context, baseURL, pkg string) (builds map[string][]format.BuildInfo, meta *pkgRecord) {
	repos := linuxBinaryRepos(c)
	if c.Repo.Kind == repo.Hosted {
		repos = append(repos, c)
	}
	builds = map[string][]format.BuildInfo{}
	for _, rc := range repos {
		if rc.Blob == nil { // metadata-only contexts (tests, meta-driven views)
			continue
		}
		keys, _ := rc.Blob.List(rc.Repo.Name + "/bin/linux/")
		for _, bk := range keys {
			p, ver, ok := cranBinaryParts(bk)
			if !ok || p != pkg {
				continue
			}
			platform, rver, ok := cranBinaryLocation(rc, bk)
			if !ok {
				continue
			}
			info, _, _ := rc.Blob.Stat(bk)
			var rec pkgRecord
			rc.Meta.GetJSON(h.binNS(rc, platform, rver), pkg+"_"+ver, &rec) //nolint:errcheck
			if meta == nil && rec.Package != "" {
				r := rec
				meta = &r
			}
			b := format.BuildInfo{
				Platform:    strings.TrimPrefix(platform, "linux/") + "/" + rver,
				DownloadURL: baseURL + "/repository/" + bk,
				SizeBytes:   info.Size,
				PublishedAt: rec.UploadedAt,
			}
			if pv := rec.Linux; pv != nil {
				b.SHA256 = pv.SHA256
				b.SystemRequirements = pv.Needs
				b.Provenance = pv.fields()
			}
			builds[ver] = append(builds[ver], b)
		}
	}
	for _, bs := range builds {
		sort.Slice(bs, func(i, j int) bool { return bs[i].Platform < bs[j].Platform })
	}
	return builds, meta
}

// fields presents provenance for people, in reading order.
func (p *linuxProvenance) fields() []format.Field {
	out := []format.Field{
		{Label: "Uploaded by", Value: p.Uploader},
		{Label: "Builder image", Value: p.BuilderImage},
		{Label: "Builder digest", Value: p.BuilderDigest},
		{Label: "Built from", Value: p.SourceRepo + " source, sha256 " + p.SourceSHA256},
	}
	if p.LinkingTo != "" {
		out = append(out, format.Field{Label: "LinkingTo", Value: p.LinkingTo})
	}
	if p.MaxGlibc != "" {
		out = append(out, format.Field{Label: "Needs glibc", Value: ">= " + p.MaxGlibc})
	}
	if out[0].Value == "" {
		out[0].Value = "unknown"
	}
	return out
}
