package format_test

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"forge/internal/format"
)

// An uploaded archive is size-limited; the members inside it are not. A ~1 MB
// .tgz holding a 1 GiB Chart.yaml is a 1000x amplification, and reading it with
// io.ReadAll took a live server from 27 MB to 3.7 GB of RSS on two requests —
// an out-of-memory kill available to anyone who can publish one package.
func TestReadMetadata(t *testing.T) {
	t.Run("ordinary metadata reads through", func(t *testing.T) {
		want := "apiVersion: v2\nname: webapp\nversion: 1.0.0\n"
		got, err := format.ReadMetadata(strings.NewReader(want))
		if err != nil || string(got) != want {
			t.Errorf("got %q, %v", got, err)
		}
	})

	t.Run("a member at the cap is still accepted", func(t *testing.T) {
		got, err := format.ReadMetadata(bytes.NewReader(make([]byte, format.MaxMetadataBytes)))
		if err != nil {
			t.Errorf("member exactly at the cap was refused: %v", err)
		}
		if int64(len(got)) != format.MaxMetadataBytes {
			t.Errorf("read %d bytes, want %d", len(got), format.MaxMetadataBytes)
		}
	})

	t.Run("one byte over is refused", func(t *testing.T) {
		_, err := format.ReadMetadata(bytes.NewReader(make([]byte, format.MaxMetadataBytes+1)))
		if !errors.Is(err, format.ErrMetadataTooLarge) {
			t.Errorf("err = %v, want ErrMetadataTooLarge", err)
		}
	})

	t.Run("an endless member is refused without exhausting memory", func(t *testing.T) {
		// zeros never returns EOF; an unbounded read here would not terminate.
		_, err := format.ReadMetadata(zeros{})
		if !errors.Is(err, format.ErrMetadataTooLarge) {
			t.Fatalf("err = %v, want ErrMetadataTooLarge", err)
		}
	})
}

// zeros is an infinite reader, standing in for a decompressing bomb.
type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

var _ io.Reader = zeros{}
