package cran

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"os"
	"strings"
	"testing"

	"forge/internal/cranplatform"
)

// The source DESCRIPTION as CRAN ships it, with continuation lines.
const lbSrcDesc = `Package: lb
Version: 1.0.2
Title: A Test Package
Authors@R: c(
    person("Ann", "Author", role = c("aut", "cre"), email = "ann@example.org"))
Description: Exercises the Linux binary validator. It has a description
    long enough to wrap onto a second line.
License: MIT
NeedsCompilation: yes
Packaged: 2026-01-02 10:00:00 UTC; ann
Repository: CRAN
Date/Publication: 2026-01-03 08:00:00 UTC
`

const lbBuilt = "Built: R 4.6.1; x86_64-pc-linux-gnu; 2026-10-10 07:19:05 UTC; unix\n"

func lbPlatform() cranplatform.Platform {
	return cranplatform.Platform{
		Key: "noble-x86_64/4.6", RPlatform: "x86_64-pc-linux-gnu",
		BuilderImage: "img", BuilderDigest: "sha256:" + strings.Repeat("ab", 32),
		MaxGlibc:  "2.39",
		Libraries: []string{"libc.so.6", "libm.so.6", "libR.so"},
	}
}

func elfFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/elf/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type tarEntry struct {
	name string
	body []byte
	typ  byte
}

func lbTarball(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.body)), Typeflag: e.typ}
		switch e.typ {
		case 0:
			h.Typeflag = tar.TypeReg
		case tar.TypeDir:
			h.Mode, h.Size = 0o755, 0
		case tar.TypeSymlink, tar.TypeLink:
			h.Linkname, h.Size = "target", 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 0 {
			tw.Write(e.body) //nolint:errcheck
		}
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// lbEntries is a well-formed installed package: R CMD INSTALL output for the
// source above, with one shared object.
func lbEntries(t *testing.T) []tarEntry {
	return []tarEntry{
		{name: "lb/", typ: tar.TypeDir},
		{name: "lb/DESCRIPTION", body: []byte(lbSrcDesc + lbBuilt)},
		{name: "lb/Meta/package.rds", body: []byte("rds")},
		{name: "lb/R/lb", body: []byte("lazy-load stub")},
		{name: "lb/libs/lb.so", body: elfFixture(t, "x86_64-plain.so")},
	}
}

func set(entries []tarEntry, name string, body []byte) []tarEntry {
	out := append([]tarEntry(nil), entries...)
	for i := range out {
		if out[i].name == name {
			out[i].body = body
			return out
		}
	}
	return append(out, tarEntry{name: name, body: body})
}

func without(entries []tarEntry, name string) []tarEntry {
	var out []tarEntry
	for _, e := range entries {
		if e.name != name {
			out = append(out, e)
		}
	}
	return out
}

func TestValidateLinuxBinary_Accepts(t *testing.T) {
	// renv rewrites DESCRIPTION: continuation lines folded (keeping inner
	// double spaces), Remote* fields added. Measured in L0.
	renvDesc := `Package: lb
Version: 1.0.2
Title: A Test Package
Authors@R: c( person("Ann", "Author", role = c("aut", "cre"), email = "ann@example.org"))
Description: Exercises the Linux binary validator. It has a description  long enough to wrap onto a second line.
License: MIT
NeedsCompilation: yes
Packaged: 2026-01-02 10:00:00 UTC; ann
Repository: RSPM
Date/Publication: 2026-01-03 08:00:00 UTC
` + lbBuilt + `RemoteType: standard
RemoteRepos: https://forge.example/repository/cran-public
RemotePkgPlatform: source
`
	pureR := strings.Replace(lbSrcDesc, "NeedsCompilation: yes", "NeedsCompilation: no", 1)
	cases := map[string][]tarEntry{
		"R CMD INSTALL output": lbEntries(t),
		"renv-installed":       set(lbEntries(t), "lb/DESCRIPTION", []byte(renvDesc)),
		"leading ./":           {{name: "./lb/DESCRIPTION", body: []byte(lbSrcDesc + lbBuilt)}, {name: "./lb/Meta/package.rds", body: []byte("x")}},
		"lib shipped in package": set(set(lbEntries(t), "lb/libs/lb.so", elfFixture(t, "x86_64-libfoo.so")),
			"lb/libs/libfoo.so.1", elfFixture(t, "x86_64-plain.so")),
	}
	for name, entries := range cases {
		src := lbSrcDesc
		if name == "pure R" {
			src = pureR
		}
		got, err := validateLinuxBinary(lbTarball(t, entries), "lb", "1.0.2", lbPlatform(), []byte(src))
		if err != nil {
			t.Errorf("%s: rejected: %v", name, err)
			continue
		}
		if name == "R CMD INSTALL output" &&
			(strings.Join(got.Needed, ",") != "libc.so.6" || got.MaxGlibc != "2.14" || strings.Join(got.Objects, ",") != "libs/lb.so") {
			t.Errorf("%s: learned %+v", name, got)
		}
	}

	// A package with no compiled code is built with an empty platform.
	pure := []tarEntry{
		{name: "lb/DESCRIPTION", body: []byte(pureR + "Built: R 4.6.1; ; 2026-10-10 07:19:08 UTC; unix\n")},
		{name: "lb/Meta/package.rds", body: []byte("rds")},
	}
	if _, err := validateLinuxBinary(lbTarball(t, pure), "lb", "1.0.2", lbPlatform(), []byte(pureR)); err != nil {
		t.Errorf("pure R: rejected: %v", err)
	}
}

func TestValidateLinuxBinary_Rejects(t *testing.T) {
	desc := func(d string) []tarEntry { return set(lbEntries(t), "lb/DESCRIPTION", []byte(d)) }
	tooNew := lbPlatform()
	tooNew.MaxGlibc = "2.37"
	noFoo := lbPlatform()
	type tc struct {
		body []byte
		p    cranplatform.Platform
		want string // substring of the reason
	}
	cases := map[string]tc{
		"not gzip": {body: []byte("PK\x03\x04 a zip"), want: "gzip"},
		"source tarball": {body: lbTarball(t, []tarEntry{{name: "lb/DESCRIPTION", body: []byte(lbSrcDesc)},
			{name: "lb/src/lb.c", body: []byte("int x;")}}), want: "installed package"},
		"no Meta/package.rds": {body: lbTarball(t, without(lbEntries(t), "lb/Meta/package.rds")), want: "installed package"},
		"other package":       {body: lbTarball(t, desc(strings.Replace(lbSrcDesc, "Package: lb", "Package: lc", 1)+lbBuilt)), want: "path names"},
		"other version":       {body: lbTarball(t, desc(strings.Replace(lbSrcDesc, "1.0.2", "1.0.3", 1)+lbBuilt)), want: "path names"},
		"no Built":            {body: lbTarball(t, desc(lbSrcDesc)), want: "no Built"},
		"other R minor":       {body: lbTarball(t, desc(lbSrcDesc+strings.Replace(lbBuilt, "R 4.6.1", "R 4.5.2", 1))), want: "R 4.6"},
		"other platform":      {body: lbTarball(t, desc(lbSrcDesc+strings.Replace(lbBuilt, "x86_64-pc-linux-gnu", "aarch64-unknown-linux-gnu", 1))), want: "built for aarch64"},
		"windows build":       {body: lbTarball(t, desc(lbSrcDesc+strings.Replace(lbBuilt, "unix", "windows", 1))), want: "OS type"},
		"malformed Built":     {body: lbTarball(t, desc(lbSrcDesc+"Built: R 4.6.1\n")), want: "format"},
		"empty platform but compiled": {body: lbTarball(t, desc(lbSrcDesc+"Built: R 4.6.1; ; 2026-10-10 07:19:08 UTC; unix\n")),
			want: "compiled code"},
		"different source": {body: lbTarball(t, desc(strings.Replace(lbSrcDesc, "License: MIT", "License: GPL-3", 1)+lbBuilt)),
			want: "License"},
		"source field missing": {body: lbTarball(t, desc(strings.Replace(lbSrcDesc, "Packaged: 2026-01-02 10:00:00 UTC; ann\n", "", 1)+lbBuilt)),
			want: "lacks Packaged"},
		"added field":     {body: lbTarball(t, desc(lbSrcDesc+lbBuilt+"Archs: x64\n")), want: "adds Archs"},
		"aarch64 code":    {body: lbTarball(t, set(lbEntries(t), "lb/libs/lb.so", elfFixture(t, "aarch64-plain.so"))), want: "EM_AARCH64"},
		"unknown library": {body: lbTarball(t, set(lbEntries(t), "lb/libs/lb.so", elfFixture(t, "x86_64-libfoo.so"))), p: noFoo, want: "libfoo.so.1"},
		"glibc too new":   {body: lbTarball(t, set(lbEntries(t), "lb/libs/lb.so", elfFixture(t, "x86_64-glibc238.so"))), p: tooNew, want: "GLIBC_2.38"},
		"corrupt ELF":     {body: lbTarball(t, set(lbEntries(t), "lb/libs/lb.so", []byte("\x7fELF garbage"))), want: "unreadable ELF"},
		"symlink":         {body: lbTarball(t, append(lbEntries(t), tarEntry{name: "lb/libs/link.so", typ: tar.TypeSymlink})), want: "tar -h"},
		"hard link":       {body: lbTarball(t, append(lbEntries(t), tarEntry{name: "lb/libs/link.so", typ: tar.TypeLink})), want: "link"},
		"outside package": {body: lbTarball(t, append(lbEntries(t), tarEntry{name: "other/x", body: []byte("x")})), want: "outside lb/"},
		"dot-dot":         {body: lbTarball(t, append(lbEntries(t), tarEntry{name: "lb/../../etc/x", body: []byte("x")})), want: "unsafe path"},
		"device":          {body: lbTarball(t, append(lbEntries(t), tarEntry{name: "lb/dev", typ: tar.TypeChar})), want: "regular file"},
	}
	for name, c := range cases {
		p := c.p
		if p.Key == "" {
			p = lbPlatform()
		}
		if name == "unknown library" {
			p.Libraries = []string{"libc.so.6"}
		}
		_, err := validateLinuxBinary(c.body, "lb", "1.0.2", p, []byte(lbSrcDesc))
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if !errors.Is(err, errBadBinary) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want a rejection mentioning %q", name, err, c.want)
		}
	}
}

func TestVersionLE(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"2.14", "2.39", true}, {"2.39", "2.39", true}, {"2.40", "2.39", false},
		{"2.9", "2.10", true}, {"2.3.4", "2.4", true}, {"3.0", "2.39", false},
		{"PRIVATE", "2.39", false},
	} {
		if got := versionLE(c.a, c.b); got != c.want {
			t.Errorf("versionLE(%q, %q) = %v", c.a, c.b, got)
		}
	}
}
