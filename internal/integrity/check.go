package integrity

import (
	"crypto/md5"  // #nosec G501 -- MD5/SHA1 are what Maven/npm store; needed to verify them
	"crypto/sha1" // #nosec G505
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"io"
	"strings"

	"forge/internal/blob"
	"forge/internal/meta"
	"forge/internal/proxy"
)

// Hashes carries every digest a format might have stored an expectation for,
// computed in a single streaming pass over the blob.
type Hashes struct {
	Size   int64
	SHA256 string
	SHA1   string
	SHA512 []byte // raw, for npm's base64 "sha512-…" integrity strings
	MD5    string
}

// HashBlob streams one blob through all four digests at once. One read
// serves every sidecar/expectation check for that blob.
func HashBlob(b blob.Store, key string) (Hashes, error) {
	rc, err := b.Get(key)
	if err != nil {
		return Hashes{}, err
	}
	defer rc.Close()
	h256 := sha256.New()
	h1 := sha1.New() // #nosec G401
	h512 := sha512.New()
	hMD5 := md5.New() // #nosec G401
	n, err := io.Copy(io.MultiWriter(h256, h1, h512, hMD5), rc)
	if err != nil {
		return Hashes{}, err
	}
	return Hashes{
		Size:   n,
		SHA256: hex.EncodeToString(h256.Sum(nil)),
		SHA1:   hex.EncodeToString(h1.Sum(nil)),
		SHA512: h512.Sum(nil),
		MD5:    hex.EncodeToString(hMD5.Sum(nil)),
	}, nil
}

// VerifyProxyCache checks the standard proxy-cache convention shared by every
// format that caches through proxy.Fetch: each CacheEntry in "{repo}:proxy"
// is keyed by the blob key it describes.
//
// Only one direction is a finding: a blob under the repo with no CacheEntry
// (orphan — it will never be revalidated and only a cache miss overwrites
// it). The reverse — an entry whose blob is gone — is NOT reported: cache
// eviction (cleanup.EvictProxyCache) deletes blobs and deliberately leaves
// entries behind, so a dangling entry is routine operation and the proxy
// re-fetches on demand.
//
// Only blob-keyed entries (prefix "{repo}/") are considered; formats that
// also keep meta-keyed cache entries (npm packuments, keyed by package name)
// must verify those themselves. Checksum verification is not possible for
// proxy caches — upstream provenance records no digest — so mode has no
// effect here.
func VerifyProxyCache(repoName string, b blob.Store, m meta.Store) (Result, error) {
	var res Result
	cacheNS := repoName + ":proxy"
	prefix := repoName + "/"

	keys, err := m.List(cacheNS)
	if err != nil {
		return res, err
	}
	entryFor := make(map[string]bool, len(keys))
	for _, k := range keys {
		if k == proxy.HealthKey || !strings.HasPrefix(k, prefix) {
			continue
		}
		res.MetaChecked++
		entryFor[k] = true
	}

	blobs, err := b.List(prefix)
	if err != nil {
		return res, err
	}
	for _, k := range blobs {
		res.BlobsChecked++
		if !entryFor[k] {
			res.Add(KindOrphan, k, "", "",
				"cached blob has no proxy cache entry; it will never be revalidated or evicted by TTL")
		}
	}
	return res, nil
}
