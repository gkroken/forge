// Package cranplatform is the registry of Linux platform keys for CRAN binary
// packages (WORKPLAN-CRAN-LINUX.md).
//
// A Linux binary only works where it was built: the same distro release, R
// minor version and CPU architecture, and the shared libraries that
// environment provides. A platform key names one such environment —
// "noble-x86_64/4.6" — and its registration records what forge needs to judge
// an upload for it statically: the builder image it must come from, the
// R platform triple `Built:` must report, the newest glibc symbol version a
// binary may require, and the shared libraries the environment provides.
//
// Nothing is accepted or served for a key that is not registered. The registry
// is global rather than per repository: a key describes an environment, and
// every binary repository building for that environment means the same thing
// by it.
package cranplatform

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"forge/internal/meta"
)

// NS is the meta namespace holding one record per registered key.
const NS = "admin:cran-platforms"

// Arches maps the architectures forge accepts in a key to their ELF machine
// names, for the messages validation produces. Other architectures are refused
// at registration rather than half-supported.
var Arches = map[string]string{
	"x86_64":  "EM_X86_64",
	"aarch64": "EM_AARCH64",
}

// Platform is one registered key.
type Platform struct {
	// Key is "{distro}-{arch}/{rminor}", e.g. "noble-x86_64/4.6".
	Key string `json:"key"`
	// RPlatform is R.version$platform in the builder, which a binary's
	// `Built:` field reports — e.g. "x86_64-pc-linux-gnu".
	RPlatform string `json:"rPlatform"`
	// BuilderImage is the canonical builder image reference, for people;
	// BuilderDigest ("sha256:…") is what an upload must report.
	BuilderImage  string `json:"builderImage"`
	BuilderDigest string `json:"builderDigest"`
	// MaxGlibc is the builder's glibc version ("2.39"): no ELF object may need
	// a GLIBC_x.y symbol version newer than this.
	MaxGlibc string `json:"maxGlibc"`
	// Libraries lists the shared-library sonames the environment provides
	// ("libc.so.6", "libR.so", …), as scripts/cran-linux-manifest.sh prints
	// them. Every DT_NEEDED entry of an uploaded binary must be among them.
	Libraries []string `json:"libraries"`
}

// Key is a parsed platform key.
type Key struct {
	Distro, Arch, RMinor string
}

func (k Key) String() string { return k.Distro + "-" + k.Arch + "/" + k.RMinor }

var (
	// The distro is a release codename; pak only recognises [a-zA-Z0-9_]+ in
	// that URL segment, and forge keeps keys lower-case.
	distroRE = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	rminorRE = regexp.MustCompile(`^[0-9]+\.[0-9]+$`)
	digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	glibcRE  = regexp.MustCompile(`^[0-9]+\.[0-9]+$`)
	sonameRE = regexp.MustCompile(`^[A-Za-z0-9._+-]+\.so(\.[0-9A-Za-z._-]+)?$`)
)

// ParseKey parses "{distro}-{arch}/{rminor}". The distro cannot contain "-"
// and the arch may contain "_", so the first "-" separates them.
func ParseKey(s string) (Key, error) {
	da, rminor, ok := strings.Cut(s, "/")
	if !ok {
		return Key{}, fmt.Errorf("platform key %q: want {distro}-{arch}/{rminor}, e.g. noble-x86_64/4.6", s)
	}
	distro, arch, ok := strings.Cut(da, "-")
	if !ok {
		return Key{}, fmt.Errorf("platform key %q: want {distro}-{arch}/{rminor}, e.g. noble-x86_64/4.6", s)
	}
	switch {
	case !distroRE.MatchString(distro):
		return Key{}, fmt.Errorf("platform key %q: distro %q must be a lower-case release codename", s, distro)
	case Arches[arch] == "":
		return Key{}, fmt.Errorf("platform key %q: arch %q is not one of x86_64, aarch64", s, arch)
	case !rminorRE.MatchString(rminor):
		return Key{}, fmt.Errorf("platform key %q: R version %q must be a minor version like 4.6", s, rminor)
	}
	return Key{Distro: distro, Arch: arch, RMinor: rminor}, nil
}

// Validate reports every problem with a registration, so one round trip shows
// them all.
func (p Platform) Validate() error {
	var errs []string
	k, err := ParseKey(p.Key)
	if err != nil {
		errs = append(errs, err.Error())
	}
	if err == nil && (!strings.HasPrefix(p.RPlatform, k.Arch+"-") || !strings.HasSuffix(p.RPlatform, "-linux-gnu")) {
		errs = append(errs, fmt.Sprintf("rPlatform %q must be the builder's R.version$platform for %s, e.g. %s-pc-linux-gnu",
			p.RPlatform, k.Arch, k.Arch))
	}
	if strings.TrimSpace(p.BuilderImage) == "" {
		errs = append(errs, "builderImage is required")
	}
	if !digestRE.MatchString(p.BuilderDigest) {
		errs = append(errs, fmt.Sprintf("builderDigest %q must be sha256:<64 lower-case hex>", p.BuilderDigest))
	}
	if !glibcRE.MatchString(p.MaxGlibc) {
		errs = append(errs, fmt.Sprintf("maxGlibc %q must be a version like 2.39", p.MaxGlibc))
	}
	if len(p.Libraries) == 0 {
		errs = append(errs, "libraries is empty — generate it with scripts/cran-linux-manifest.sh in the builder image")
	}
	seen := make(map[string]bool, len(p.Libraries))
	for _, l := range p.Libraries {
		switch {
		case !sonameRE.MatchString(l):
			errs = append(errs, fmt.Sprintf("library %q is not a shared-library soname", l))
		case seen[l]:
			errs = append(errs, fmt.Sprintf("library %q is listed twice", l))
		}
		seen[l] = true
	}
	if len(errs) > 0 {
		return fmt.Errorf("invalid CRAN platform registration:\n  %s", strings.Join(errs, "\n  "))
	}
	return nil
}

// Provides reports whether the environment provides soname.
func (p Platform) Provides(soname string) bool {
	for _, l := range p.Libraries {
		if l == soname {
			return true
		}
	}
	return false
}

// Store persists registrations in a meta.Store.
type Store struct{ m meta.Store }

// NewStore returns a registry backed by m.
func NewStore(m meta.Store) *Store { return &Store{m: m} }

// Get returns the registration for key.
func (s *Store) Get(key string) (Platform, bool, error) {
	var p Platform
	ok, err := s.m.GetJSON(NS, key, &p)
	return p, ok, err
}

// List returns every registration, ordered by key.
func (s *Store) List() ([]Platform, error) {
	keys, err := s.m.List(NS)
	if err != nil {
		return nil, err
	}
	out := make([]Platform, 0, len(keys))
	for _, k := range keys {
		var p Platform
		if ok, err := s.m.GetJSON(NS, k, &p); err == nil && ok {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// Put validates and stores a registration, replacing any earlier one for the
// same key. Binaries already uploaded for the key are not re-judged; each
// keeps the provenance it was accepted with.
func (s *Store) Put(p Platform) error {
	if err := p.Validate(); err != nil {
		return err
	}
	return s.m.PutJSON(NS, p.Key, p)
}

// Delete removes a registration. Uploads for the key are refused from then on
// and, once serving lands, its binaries are no longer served (source is).
func (s *Store) Delete(key string) error { return s.m.Delete(NS, key) }
