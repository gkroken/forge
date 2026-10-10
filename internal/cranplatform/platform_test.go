package cranplatform

import (
	"strings"
	"testing"

	"forge/internal/meta"
)

func valid() Platform {
	return Platform{
		Key:           "noble-x86_64/4.6",
		RPlatform:     "x86_64-pc-linux-gnu",
		BuilderImage:  "ghcr.io/acme/r-builder:4.6.1-noble",
		BuilderDigest: "sha256:" + strings.Repeat("ab", 32),
		MaxGlibc:      "2.39",
		Libraries:     []string{"libc.so.6", "libm.so.6", "libR.so", "libxml2.so.2", "libstdc++.so.6"},
	}
}

func TestParseKey(t *testing.T) {
	k, err := ParseKey("noble-x86_64/4.6")
	if err != nil || k != (Key{"noble", "x86_64", "4.6"}) || k.String() != "noble-x86_64/4.6" {
		t.Fatalf("got %+v, %v", k, err)
	}
	if k, err := ParseKey("bookworm-aarch64/4.5"); err != nil || k.Arch != "aarch64" {
		t.Fatalf("aarch64: %+v %v", k, err)
	}
	for _, bad := range []string{
		"", "noble", "noble-x86_64", "noble/4.6", "noble-x86_64/4", "noble-x86_64/4.6.1",
		"Noble-x86_64/4.6", "noble-amd64/4.6", "noble-arm64/4.6", "noble-x86_64-4.6",
		"ubuntu-24.04-x86_64/4.6", "noble-x86_64/4.6/extra", "../x-x86_64/4.6",
	} {
		if _, err := ParseKey(bad); err == nil {
			t.Errorf("ParseKey(%q) accepted", bad)
		}
	}
}

func TestValidate(t *testing.T) {
	if err := valid().Validate(); err != nil {
		t.Fatalf("valid registration rejected: %v", err)
	}
	cases := map[string]func(*Platform){
		"bad key":                func(p *Platform) { p.Key = "noble/4.6" },
		"platform of other arch": func(p *Platform) { p.RPlatform = "aarch64-unknown-linux-gnu" },
		"non-linux platform":     func(p *Platform) { p.RPlatform = "x86_64-w64-mingw32" },
		"no image":               func(p *Platform) { p.BuilderImage = " " },
		"short digest":           func(p *Platform) { p.BuilderDigest = "sha256:abc" },
		"digest without algo":    func(p *Platform) { p.BuilderDigest = strings.Repeat("ab", 32) },
		"glibc patch version":    func(p *Platform) { p.MaxGlibc = "2.39.1" },
		"no libraries":           func(p *Platform) { p.Libraries = nil },
		"path not soname":        func(p *Platform) { p.Libraries = []string{"/lib/x86_64-linux-gnu/libc.so.6"} },
		"not a shared library":   func(p *Platform) { p.Libraries = []string{"libc.a"} },
		"duplicate library":      func(p *Platform) { p.Libraries = []string{"libc.so.6", "libc.so.6"} },
	}
	for name, mut := range cases {
		p := valid()
		mut(&p)
		if err := p.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestStore_RoundTrip(t *testing.T) {
	m, err := meta.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := NewStore(m)
	if err := s.Put(Platform{Key: "noble-x86_64/4.6"}); err == nil {
		t.Fatal("Put stored an invalid registration")
	}
	a, b := valid(), valid()
	b.Key = "jammy-x86_64/4.5"
	for _, p := range []Platform{a, b} {
		if err := s.Put(p); err != nil {
			t.Fatal(err)
		}
	}
	got, ok, err := s.Get("noble-x86_64/4.6")
	if err != nil || !ok || got.MaxGlibc != "2.39" || !got.Provides("libR.so") || got.Provides("libR") {
		t.Fatalf("Get: %+v %v %v", got, ok, err)
	}
	list, err := s.List()
	if err != nil || len(list) != 2 || list[0].Key != "jammy-x86_64/4.5" || list[1].Key != "noble-x86_64/4.6" {
		t.Fatalf("List: %+v %v", list, err)
	}
	if err := s.Delete("noble-x86_64/4.6"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Get("noble-x86_64/4.6"); ok {
		t.Fatal("deleted registration still present")
	}
}
