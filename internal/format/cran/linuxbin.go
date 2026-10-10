package cran

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"debug/elf"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"

	"forge/internal/cranplatform"
	"forge/internal/format"
)

// Static validation of a Linux binary package upload (WORKPLAN-CRAN-LINUX L1).
//
// A wrong-platform binary fails at load time on every client of that platform,
// and renv does not fall back to source when it does — so forge refuses
// anything it can show is wrong without running it: the wrong package, a build
// for another R or another platform, a build of different source, or compiled
// code for another CPU or needing a library or glibc the environment lacks.
// What it cannot show (a builder deliberately lying) is left to the write
// grant, provenance and one-call removal.

// Bounds on what validation will unpack. An installed package is mostly R
// code and data; the largest real ones (arrow, duckdb) carry one shared object
// of some hundreds of MB.
const (
	maxLinuxBinaryUnpacked = 4 << 30
	maxLinuxBinaryELF      = 512 << 20
)

// errBadBinary marks a refusal: the upload is wrong, not the server.
var errBadBinary = errors.New("rejected Linux binary")

func rejectf(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{errBadBinary}, args...)...)
}

// linuxBinary is what validation learned about an accepted binary.
type linuxBinary struct {
	Fields map[string]string // its DESCRIPTION
	// Needed is every shared library its ELF objects need from the
	// environment (DT_NEEDED, less those the package ships itself), sorted —
	// the binary's system requirements.
	Needed []string
	// MaxGlibc is the newest GLIBC_x.y symbol version any object needs, or "".
	MaxGlibc string
	// Objects lists the package's ELF objects by path within the package.
	Objects []string
}

// validateLinuxBinary checks tgz as the binary of pkg at ver for platform p,
// built from the source whose DESCRIPTION is srcDesc.
func validateLinuxBinary(tgz []byte, pkg, ver string, p cranplatform.Platform, srcDesc []byte) (linuxBinary, error) {
	key, err := cranplatform.ParseKey(p.Key)
	if err != nil {
		return linuxBinary{}, err
	}
	gz, err := gzip.NewReader(bytes.NewReader(tgz))
	if err != nil {
		return linuxBinary{}, rejectf("not a gzip tarball: %v", err)
	}
	defer gz.Close()

	// ELF objects are checked as they stream past and only their library
	// needs are kept — never every object's bytes at once. The needs are
	// resolved at the end, because a library the package ships itself may
	// come later in the tarball than an object that needs it.
	var (
		desc     []byte
		hasMeta  bool
		shipped  = map[string]bool{}     // basenames of every file in the package
		needs    = map[string][]string{} // ELF object -> DT_NEEDED
		out      linuxBinary
		unpacked int64
	)
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return linuxBinary{}, rejectf("corrupt tarball: %v", err)
		}
		name := strings.TrimPrefix(hdr.Name, "./")
		rel, ok := strings.CutPrefix(name, pkg+"/")
		if !ok && strings.TrimSuffix(name, "/") != pkg {
			return linuxBinary{}, rejectf("%q is outside %s/: an installed-package tarball holds exactly one package directory", hdr.Name, pkg)
		}
		if path.IsAbs(name) || path.Clean("/"+name) != "/"+strings.TrimSuffix(name, "/") {
			return linuxBinary{}, rejectf("unsafe path %q", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			continue
		case tar.TypeReg:
		case tar.TypeSymlink, tar.TypeLink:
			return linuxBinary{}, rejectf("%q is a link; pack the installed directory with links dereferenced (tar -h) — renv libraries are symlinks into its cache", hdr.Name)
		default:
			return linuxBinary{}, rejectf("%q is not a regular file or directory", hdr.Name)
		}
		if unpacked += hdr.Size; unpacked > maxLinuxBinaryUnpacked {
			return linuxBinary{}, rejectf("unpacks to more than %d bytes", int64(maxLinuxBinaryUnpacked))
		}
		shipped[path.Base(rel)] = true
		switch rel {
		case "DESCRIPTION":
			if desc, err = format.ReadMetadata(tr); err != nil {
				return linuxBinary{}, rejectf("DESCRIPTION: %v", err)
			}
			continue
		case "Meta/package.rds":
			hasMeta = true
		}
		head := make([]byte, 4)
		n, _ := io.ReadFull(tr, head)
		if n < 4 || string(head) != elf.ELFMAG {
			continue
		}
		if hdr.Size > maxLinuxBinaryELF {
			return linuxBinary{}, rejectf("%q: ELF object larger than %d bytes", rel, int64(maxLinuxBinaryELF))
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			return linuxBinary{}, rejectf("corrupt tarball: %v", err)
		}
		libs, err := checkELF(rel, append(head, body...), key, p, &out.MaxGlibc)
		if err != nil {
			return linuxBinary{}, err
		}
		needs[rel] = libs
	}

	if desc == nil || !hasMeta {
		return linuxBinary{}, rejectf("not an installed package: %s/DESCRIPTION and %s/Meta/package.rds are required (upload the installed library directory or R CMD INSTALL --build output, not a source tarball)", pkg, pkg)
	}
	fields := dcfFields(desc)
	if fields["Package"] != pkg || fields["Version"] != ver {
		return linuxBinary{}, rejectf("DESCRIPTION declares %s_%s but the path names %s_%s", fields["Package"], fields["Version"], pkg, ver)
	}
	if err := checkBuilt(fields["Built"], key, p, len(needs) > 0); err != nil {
		return linuxBinary{}, err
	}
	if err := matchSource(fields, dcfFields(srcDesc)); err != nil {
		return linuxBinary{}, err
	}

	out.Fields = fields
	needed := map[string]bool{}
	for rel, libs := range needs {
		out.Objects = append(out.Objects, rel)
		for _, l := range libs {
			if shipped[l] {
				continue
			}
			if !p.Provides(l) {
				return linuxBinary{}, rejectf("%s needs %s, which %s does not provide (not in the registered library manifest)", rel, l, key)
			}
			needed[l] = true
		}
	}
	sort.Strings(out.Objects)
	for n := range needed {
		out.Needed = append(out.Needed, n)
	}
	sort.Strings(out.Needed)
	return out, nil
}

// checkBuilt verifies DESCRIPTION's "Built: R 4.6.1; x86_64-pc-linux-gnu;
// 2026-10-10 07:19:05 UTC; unix" against the key. A package with no compiled
// code is built with an empty platform ("R 4.6.1; ; …"), which is accepted
// only when the package really carries no ELF object.
func checkBuilt(built string, key cranplatform.Key, p cranplatform.Platform, compiled bool) error {
	if built == "" {
		return rejectf("DESCRIPTION has no Built: field, so this is not a binary package")
	}
	parts := strings.Split(built, ";")
	if len(parts) != 4 {
		return rejectf("Built: %q is not R's format", built)
	}
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	rver, ok := strings.CutPrefix(parts[0], "R ")
	if !ok || rMinor(rver) != key.RMinor {
		return rejectf("built by %q, but key %s is for R %s", parts[0], key, key.RMinor)
	}
	switch {
	case parts[1] == "" && compiled:
		return rejectf("Built: names no platform but the package contains compiled code")
	case parts[1] != "" && parts[1] != p.RPlatform:
		return rejectf("built for %s, but key %s is %s", parts[1], key, p.RPlatform)
	}
	if parts[3] != "unix" {
		return rejectf("built for OS type %q, not unix", parts[3])
	}
	return nil
}

// rMinor returns "4.6" for "4.6.1".
func rMinor(v string) string {
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return ""
	}
	return parts[0] + "." + parts[1]
}

// matchSource checks that the binary was built from the source forge serves.
// Installing a package copies the source DESCRIPTION and adds Built; pak and
// renv also add Remote* fields, and renv rewrites the whole file (folding
// multi-line fields), so fields are compared with whitespace collapsed.
// Repository is skipped both ways: mirrors rewrite it.
func matchSource(bin, src map[string]string) error {
	if len(src) == 0 {
		return rejectf("the source package's DESCRIPTION is empty")
	}
	norm := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	for f, sv := range src {
		if f == "Repository" {
			continue
		}
		bv, ok := bin[f]
		if !ok {
			return rejectf("DESCRIPTION lacks %s, which the source has: not built from the source forge serves", f)
		}
		if norm(bv) != norm(sv) {
			return rejectf("DESCRIPTION %s is %q but the source's is %q: not built from the source forge serves", f, clip(bv), clip(sv))
		}
	}
	for f := range bin {
		if _, ok := src[f]; ok || f == "Built" || f == "Repository" || strings.HasPrefix(f, "Remote") {
			continue
		}
		return rejectf("DESCRIPTION adds %s, which the source does not have", f)
	}
	return nil
}

func clip(s string) string {
	if len(s) > 60 {
		return s[:60] + "…"
	}
	return s
}

// elfMachines maps a key's arch to the ELF machine its objects must target.
var elfMachines = map[string]elf.Machine{
	"x86_64":  elf.EM_X86_64,
	"aarch64": elf.EM_AARCH64,
}

// checkELF verifies one shared object or executable: built for the key's CPU
// and needing no glibc newer than the environment's. It returns the libraries
// the object needs (DT_NEEDED), which the caller checks against the manifest,
// and raises maxGlibc to the object's newest GLIBC version.
func checkELF(rel string, data []byte, key cranplatform.Key, p cranplatform.Platform, maxGlibc *string) ([]string, error) {
	f, err := elf.NewFile(bytes.NewReader(data))
	if err != nil {
		return nil, rejectf("%s: unreadable ELF object: %v", rel, err)
	}
	defer f.Close()
	if want := elfMachines[key.Arch]; f.Machine != want || f.Class != elf.ELFCLASS64 {
		return nil, rejectf("%s is built for %s/%s, but key %s needs %s", rel, f.Machine, f.Class, key, want)
	}
	libs, err := f.ImportedLibraries()
	if err != nil {
		return nil, rejectf("%s: unreadable dynamic section: %v", rel, err)
	}
	syms, err := f.ImportedSymbols()
	if err != nil && !errors.Is(err, elf.ErrNoSymbols) { // static objects have none
		return nil, rejectf("%s: unreadable symbol table: %v", rel, err)
	}
	for _, s := range syms {
		v, ok := strings.CutPrefix(s.Version, "GLIBC_")
		if !ok {
			continue
		}
		if !versionLE(v, p.MaxGlibc) {
			return nil, rejectf("%s needs %s (via %s), newer than %s's glibc %s", rel, s.Version, s.Name, key, p.MaxGlibc)
		}
		if *maxGlibc == "" || !versionLE(v, *maxGlibc) {
			*maxGlibc = v
		}
	}
	return libs, nil
}

// versionLE reports a <= b for dotted numeric versions ("2.38" <= "2.39").
// A non-numeric version (GLIBC_PRIVATE) is never <= anything: a binary that
// needs glibc internals is refused.
func versionLE(a, b string) bool {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var x, y int
		var err error
		if i < len(as) {
			if x, err = strconv.Atoi(as[i]); err != nil {
				return false
			}
		}
		if i < len(bs) {
			if y, err = strconv.Atoi(bs[i]); err != nil {
				return false
			}
		}
		if x != y {
			return x < y
		}
	}
	return true
}
