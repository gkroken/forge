package format_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"forge/internal/format"
	"forge/internal/integrity"
)

// declining is a format that answers nothing — the one line a real format writes
// when a seam does not apply to it. That it satisfies format.Handler at all is
// the point of Unsupported; that it compiles is asserted below.
type declining struct{ format.Unsupported }

func (declining) Format() string                                            { return "declining" }
func (declining) Serve(http.ResponseWriter, *http.Request, *format.Context) {}

// TestUnsupported_SatisfiesHandler is the guarantee the whole seam design rests
// on: embedding Unsupported is enough to build, so declining a seam costs one
// line, while forgetting one entirely is a compile error rather than a feature
// that silently is not there.
func TestUnsupported_SatisfiesHandler(t *testing.T) {
	var h format.Handler = declining{}
	if h.Format() != "declining" {
		t.Fatalf("Format() = %q", h.Format())
	}
}

// TestUnsupported_AnswersEverySeam — every declined seam must report itself,
// never return a zero value that reads as a real answer.
func TestUnsupported_AnswersEverySeam(t *testing.T) {
	h := declining{}

	if _, err := h.BrowseRepo(nil); !errors.Is(err, format.ErrNotSupported) {
		t.Errorf("BrowseRepo err = %v, want ErrNotSupported", err)
	}
	if _, err := h.ReferencedImages(nil, "c", "v"); !errors.Is(err, format.ErrNotSupported) {
		t.Errorf("ReferencedImages err = %v, want ErrNotSupported", err)
	}
	if _, err := h.VerifyIntegrity(nil, integrity.ModeQuick); !errors.Is(err, format.ErrNotSupported) {
		t.Errorf("VerifyIntegrity err = %v, want ErrNotSupported", err)
	}
	if _, err := h.Reindex(context.Background(), nil); !errors.Is(err, format.ErrNotSupported) {
		t.Errorf("Reindex err = %v, want ErrNotSupported", err)
	}
	if _, found := h.Inspect(nil, "", "c"); found {
		t.Error("Inspect reported found on a format that declines it")
	}
	if eco := h.OSVEcosystem(); eco != "" {
		t.Errorf("OSVEcosystem = %q, want empty", eco)
	}
	if _, _, ok := h.OSVCoordinates("c"); ok {
		t.Error("OSVCoordinates reported ok on a format that declines it")
	}
	if _, _, ok := h.VulnGateTarget("sub"); ok {
		t.Error("VulnGateTarget reported ok — the gate would fire on an unscannable format")
	}
	if _, ok := h.ClaimPath("sub"); ok {
		t.Error("ClaimPath reported ok — dependency-confusion would guard a format with no component model")
	}
	if h.OwnsComponent(nil, "c") {
		t.Error("OwnsComponent reported true on a format that declines claims")
	}
}
