package format

import (
	"fmt"
	"io"
)

// MaxMetadataBytes caps how much of a member inside an uploaded archive forge
// will read into memory. Chart.yaml, DESCRIPTION and friends are a few
// kilobytes; a megabyte is far past any legitimate one.
const MaxMetadataBytes = 1 << 20

// ErrMetadataTooLarge is returned when an archive member exceeds the cap.
var ErrMetadataTooLarge = fmt.Errorf("archive member exceeds %d bytes", MaxMetadataBytes)

// ReadMetadata reads one archive member with a hard ceiling.
//
// The size has to be enforced on the read itself, not on the upload or the
// header. An uploaded archive is bounded, but the DECOMPRESSED member is not:
// a ~1 MB .tgz holding a 1 GiB Chart.yaml is a 1000x amplification, and reading
// it with io.ReadAll took a server from 27 MB to 3.7 GB of RSS on two requests
// — an out-of-memory kill available to anyone who can publish. A tar header's
// declared size cannot be trusted either, since the archive is attacker-built.
func ReadMetadata(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxMetadataBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > MaxMetadataBytes {
		return nil, ErrMetadataTooLarge
	}
	return data, nil
}
