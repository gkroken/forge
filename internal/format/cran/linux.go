package cran

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"forge/internal/blob"
	"forge/internal/cranplatform"
	"forge/internal/format"
	"forge/internal/ledger"
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
	key, err := cranplatform.ParseKey(keyStr)
	if err != nil || platform != "linux/"+key.Distro+"-"+key.Arch {
		http.Error(w, "not a Linux binary path: want bin/linux/{distro}-{arch}/contrib/{rminor}/{pkg}_{ver}.tar.gz", http.StatusBadRequest)
		return
	}
	pkg, ver, ok := nameVersionFromFilename(file)
	if !ok || pkg == "" || ver == "" {
		http.Error(w, "cannot read package and version from "+file, http.StatusBadRequest)
		return
	}
	reg, ok, err := cranplatform.NewStore(c.Meta).Get(keyStr)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "platform key "+keyStr+" is not registered; an admin registers it at /api/v1/cran/platforms/"+keyStr,
			http.StatusForbidden)
		return
	}
	if !c.Repo.RefusesOverwrite() {
		http.Error(w, "repository "+c.Repo.Name+" does not set noOverwrite; a Linux binary must never be replaced in place, so this repository cannot hold them",
			http.StatusForbidden)
		return
	}
	if c.Repo.SourceRepo == "" {
		http.Error(w, "repository "+c.Repo.Name+" has no sourceRepo to check binaries against", http.StatusForbidden)
		return
	}
	switch digest := r.Header.Get(BuilderDigestHeader); {
	case digest == "":
		http.Error(w, BuilderDigestHeader+" is required: the digest of the builder image this was built in", http.StatusBadRequest)
		return
	case digest != reg.BuilderDigest:
		http.Error(w, fmt.Sprintf("built in %s, but %s is registered for builder image %s (%s)",
			digest, keyStr, reg.BuilderDigest, reg.BuilderImage), http.StatusForbidden)
		return
	}
	if _, exists, _ := c.Blob.Stat(c.Key(c.Sub)); exists {
		http.Error(w, blob.ErrExists.Error(), http.StatusConflict)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	srcDesc, status, msg := h.linuxSourceDescription(c, pkg, ver)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	bin, err := validateLinuxBinary(body, pkg, ver, reg, srcDesc)
	if errors.Is(err, errBadBinary) {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := c.Blob.Put(c.Key(c.Sub), bytes.NewReader(body)); err != nil {
		if blob.Refused(err) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	rec := recordFromFields(bin.Fields)
	rec.UploadedAt = time.Now().UTC()
	c.Meta.PutJSON(h.binNS(c, platform, rver), rec.Package+"_"+rec.Version, rec) //nolint:errcheck
	ledger.RecordAt(c.Meta, c.Repo.Name, rec.Package, rec.Version, rec.UploadedAt)
	w.WriteHeader(http.StatusCreated)
	fmt.Fprintf(w, "stored Linux binary %s %s for %s\n", pkg, ver, keyStr)
}

// linuxSourceDescription returns the DESCRIPTION of the source package forge
// serves for pkg@ver through the binary repository's sourceRepo — the current
// path first, then CRAN's Archive. On failure it returns the HTTP status and
// message to refuse the upload with.
func (h *Handler) linuxSourceDescription(c *format.Context, pkg, ver string) ([]byte, int, string) {
	if c.ReadRepo == nil {
		return nil, http.StatusInternalServerError, "source check unavailable: this server cannot read " + c.Repo.SourceRepo
	}
	file := pkg + "_" + ver + ".tar.gz"
	upstreamFailed := false
	for _, sub := range []string{"src/contrib/" + file, "src/contrib/Archive/" + pkg + "/" + file} {
		status, body := c.ReadRepo(c.Repo.SourceRepo, sub)
		switch {
		case status >= 200 && status < 300:
			desc, err := rawDescription(body)
			if err != nil {
				return nil, http.StatusBadGateway, "the source package " + file + " from " + c.Repo.SourceRepo + " is unreadable: " + err.Error()
			}
			return desc, 0, ""
		case status >= 500:
			upstreamFailed = true
		}
	}
	if upstreamFailed {
		return nil, http.StatusBadGateway, "could not fetch the source of " + file + " from " + c.Repo.SourceRepo + " to check the binary against; retry"
	}
	return nil, http.StatusBadRequest, c.Repo.SourceRepo + " serves no source package " + file + ", so there is nothing to check this binary against"
}
