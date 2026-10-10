package config_test

import (
	"strings"
	"testing"

	"forge/internal/config"
	"forge/internal/cranplatform"
	"forge/internal/repo"
)

func cranPlatform(key string) cranplatform.Platform {
	return cranplatform.Platform{
		Key:           key,
		RPlatform:     "x86_64-pc-linux-gnu",
		BuilderImage:  "ghcr.io/acme/r-builder:4.6.1-noble",
		BuilderDigest: "sha256:" + strings.Repeat("ef", 32),
		MaxGlibc:      "2.39",
		Libraries:     []string{"libc.so.6", "libR.so"},
	}
}

func TestApply_CRANPlatforms(t *testing.T) {
	a := newAppliers(t)
	f := config.File{Prune: true, CRANPlatforms: []cranplatform.Platform{cranPlatform("noble-x86_64/4.6")}}

	res, err := config.Apply(f, a)
	if err != nil || res.CRANPlatforms.Created != 1 {
		t.Fatalf("create: %+v %v", res.CRANPlatforms, err)
	}
	if !config.LoadOwnership(a.Meta).Owns(config.KindCRANPlatform, "noble-x86_64/4.6") {
		t.Fatal("applied platform is not config-owned")
	}
	if res, _ := config.Apply(f, a); res.CRANPlatforms.Noop != 1 {
		t.Fatalf("re-apply: %+v", res.CRANPlatforms)
	}

	f.CRANPlatforms[0].MaxGlibc = "2.40"
	if res, _ := config.Apply(f, a); res.CRANPlatforms.Updated != 1 {
		t.Fatalf("update: %+v", res.CRANPlatforms)
	}
	if got, _, _ := a.CRANPlatforms.Get("noble-x86_64/4.6"); got.MaxGlibc != "2.40" {
		t.Fatalf("update not written: %+v", got)
	}

	// A registration made through the API is not the file's to overwrite...
	adhoc := cranPlatform("jammy-x86_64/4.5")
	if err := a.CRANPlatforms.Put(adhoc); err != nil {
		t.Fatal(err)
	}
	differs := cranPlatform("jammy-x86_64/4.5")
	differs.MaxGlibc = "2.35"
	if _, err := config.Apply(config.File{CRANPlatforms: []cranplatform.Platform{f.CRANPlatforms[0], differs}}, a); err == nil {
		t.Fatal("apply silently adopted a differing API-made registration")
	}
	// ...nor to prune.
	f.CRANPlatforms = nil
	res, err = config.Apply(f, a)
	if err != nil || res.CRANPlatforms.Deleted != 1 {
		t.Fatalf("prune: %+v %v", res.CRANPlatforms, err)
	}
	if _, ok, _ := a.CRANPlatforms.Get("noble-x86_64/4.6"); ok {
		t.Error("pruned registration still present")
	}
	if _, ok, _ := a.CRANPlatforms.Get("jammy-x86_64/4.5"); !ok {
		t.Error("prune removed a registration the file never managed")
	}

	exp, err := config.Export(a)
	if err != nil || len(exp.CRANPlatforms) != 1 || exp.CRANPlatforms[0].Key != "jammy-x86_64/4.5" {
		t.Fatalf("export: %+v %v", exp.CRANPlatforms, err)
	}
}

func TestValidate_CRANPlatforms(t *testing.T) {
	a := newAppliers(t)
	bad := cranPlatform("noble-amd64/4.6")
	if _, err := config.Plan(config.File{CRANPlatforms: []cranplatform.Platform{bad}}, a); err == nil ||
		!strings.Contains(err.Error(), "noble-amd64/4.6") {
		t.Fatalf("invalid platform not reported: %v", err)
	}
	a.CRANPlatforms = nil
	if _, err := config.Plan(config.File{CRANPlatforms: []cranplatform.Platform{cranPlatform("noble-x86_64/4.6")}}, a); err == nil {
		t.Fatal("platforms declared with no registry were silently dropped")
	}
}

func TestCheckKeys_CRANPlatforms(t *testing.T) {
	doc := `cranPlatforms:
- key: noble-x86_64/4.6
  rPlatform: x86_64-pc-linux-gnu
  builderImage: ghcr.io/acme/r-builder:4.6.1-noble
  builderDigest: sha256:abc
  maxGlibc: "2.39"
  libraries: [libc.so.6, libR.so]
`
	if err := config.CheckKeys([]byte(doc), true); err != nil {
		t.Fatalf("valid keys rejected: %v", err)
	}
	if err := config.CheckKeys([]byte(strings.Replace(doc, "maxGlibc", "maxGlib", 1)), true); err == nil {
		t.Fatal("misspelled key accepted")
	}
}

func TestValidate_SourceRepoMustExist(t *testing.T) {
	a := newAppliers(t)
	f := config.File{Repositories: []repo.Repository{
		{Name: "cran-linux", Format: "cran", Kind: repo.Hosted, Enabled: true, SourceRepo: "cran-public"},
	}}
	if _, err := config.Plan(f, a); err == nil || !strings.Contains(err.Error(), "sourceRepo") {
		t.Fatalf("dangling sourceRepo not reported: %v", err)
	}
	f.Repositories = append(f.Repositories, repo.Repository{Name: "cran-public", Format: "cran", Kind: repo.Group, Enabled: true})
	if _, err := config.Plan(f, a); err != nil {
		t.Fatalf("sourceRepo declared in the same file refused: %v", err)
	}
}
