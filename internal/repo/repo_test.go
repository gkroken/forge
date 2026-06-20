package repo_test

import (
	"encoding/json"
	"testing"
	"time"

	"forge/internal/repo"
)

func ptr[T any](v T) *T { return &v }

// TestRepository_JSONRoundTrip verifies all BE-D fields survive a
// Marshal → Unmarshal cycle without loss.
func TestRepository_JSONRoundTrip(t *testing.T) {
	cma := 48 * time.Hour
	mma := 12 * time.Hour
	r := repo.Repository{
		Name:           "test",
		Format:         "maven",
		Kind:           repo.Hosted,
		Enabled:        true,
		BlobStore:      "s3-primary",
		ContentMaxAge:  &cma,
		MetadataMaxAge: &mma,
		NegativeCache:  ptr(false),
		AutoBlock:      ptr(true),
		TimeoutSecs:    ptr(60),
		Retries:        ptr(3),
		QuotaGB:        ptr(100.5),
	}
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got repo.Repository
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Name != r.Name || got.Format != r.Format || got.Kind != r.Kind {
		t.Errorf("base fields: got %+v", got)
	}
	if !got.Enabled {
		t.Error("Enabled: want true")
	}
	if got.BlobStore != "s3-primary" {
		t.Errorf("BlobStore: got %q", got.BlobStore)
	}
	if got.ContentMaxAge == nil || *got.ContentMaxAge != cma {
		t.Errorf("ContentMaxAge: got %v want %v", got.ContentMaxAge, cma)
	}
	if got.MetadataMaxAge == nil || *got.MetadataMaxAge != mma {
		t.Errorf("MetadataMaxAge: got %v want %v", got.MetadataMaxAge, mma)
	}
	if got.NegativeCache == nil || *got.NegativeCache != false {
		t.Errorf("NegativeCache: got %v want false", got.NegativeCache)
	}
	if got.AutoBlock == nil || *got.AutoBlock != true {
		t.Errorf("AutoBlock: got %v want true", got.AutoBlock)
	}
	if got.TimeoutSecs == nil || *got.TimeoutSecs != 60 {
		t.Errorf("TimeoutSecs: got %v want 60", got.TimeoutSecs)
	}
	if got.Retries == nil || *got.Retries != 3 {
		t.Errorf("Retries: got %v want 3", got.Retries)
	}
	if got.QuotaGB == nil || *got.QuotaGB != 100.5 {
		t.Errorf("QuotaGB: got %v want 100.5", got.QuotaGB)
	}
}

// TestRepository_EnabledDefault: old JSON without "enabled" → Enabled=true.
func TestRepository_EnabledDefault(t *testing.T) {
	data := []byte(`{"name":"npm-proxy","format":"npm","kind":"proxy","anonymousRead":true}`)
	var r repo.Repository
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatal(err)
	}
	if !r.Enabled {
		t.Error("expected Enabled=true for legacy JSON without 'enabled' field")
	}
}

// TestRepository_ExplicitlyDisabled: "enabled":false → Enabled=false.
func TestRepository_ExplicitlyDisabled(t *testing.T) {
	data := []byte(`{"name":"npm-proxy","format":"npm","kind":"proxy","enabled":false}`)
	var r repo.Repository
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatal(err)
	}
	if r.Enabled {
		t.Error("expected Enabled=false when explicitly set")
	}
}

// TestRepository_BackwardCompatProxyTTL: legacy "proxyTTL" JSON populates
// ContentMaxAge so existing persisted repos keep their TTL.
func TestRepository_BackwardCompatProxyTTL(t *testing.T) {
	data := []byte(`{"name":"r","format":"npm","kind":"proxy","proxyTTL":"24h"}`)
	var r repo.Repository
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatal(err)
	}
	if r.ProxyTTL != 24*time.Hour {
		t.Errorf("ProxyTTL: got %v want 24h", r.ProxyTTL)
	}
	if r.ContentMaxAge == nil || *r.ContentMaxAge != 24*time.Hour {
		t.Errorf("ContentMaxAge not set from proxyTTL: got %v", r.ContentMaxAge)
	}
}

// TestRepository_ContentMaxAgeTakesPrecedence: when both proxyTTL and
// contentMaxAge are present, contentMaxAge wins.
func TestRepository_ContentMaxAgeTakesPrecedence(t *testing.T) {
	data := []byte(`{"name":"r","format":"npm","kind":"proxy","proxyTTL":"12h","contentMaxAge":"48h"}`)
	var r repo.Repository
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatal(err)
	}
	if r.ContentMaxAge == nil || *r.ContentMaxAge != 48*time.Hour {
		t.Errorf("ContentMaxAge: got %v want 48h", r.ContentMaxAge)
	}
}

// TestRepository_NilPointerDefaults: pointer fields absent in JSON remain nil.
func TestRepository_NilPointerDefaults(t *testing.T) {
	data := []byte(`{"name":"r","format":"maven","kind":"hosted"}`)
	var r repo.Repository
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatal(err)
	}
	if r.ContentMaxAge != nil {
		t.Errorf("ContentMaxAge: want nil, got %v", r.ContentMaxAge)
	}
	if r.NegativeCache != nil || r.AutoBlock != nil ||
		r.TimeoutSecs != nil || r.Retries != nil || r.QuotaGB != nil {
		t.Error("pointer fields should be nil when absent from JSON")
	}
}
