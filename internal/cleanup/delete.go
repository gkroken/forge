package cleanup

import (
	"fmt"
	"forge/internal/ledger"
	"forge/internal/repo"

	"forge/internal/blob"
	"forge/internal/meta"
)

// DeleteVersion removes exactly one component+version from a hosted repository,
// deleting its blobs and meta records. It centralises the per-format blob-key
// layout already used by Run, so UI deletes work for every format rather than
// assuming the npm tarball path. Returns the number of blobs removed and the
// bytes freed.
//
// component is the identifier shown in the UI:
//   - maven: "groupId:artifactId" (e.g. "com.example:app")
//   - npm/helm/cran: the package/chart name
func DeleteVersion(r repo.Repository, res Resolver, component, version string, b blob.Store, m meta.Store) (Result, error) {
	if component == "" || version == "" {
		return Result{}, fmt.Errorf("cleanup: component and version are required")
	}
	h, c, ok := resolve(r, res, b, m)
	if !ok {
		return Result{}, fmt.Errorf("cleanup: no handler for format %q", r.Format)
	}
	freed, err := h.DeleteVersion(c, component, version)
	if err != nil {
		return Result{}, err
	}
	if freed == 0 {
		return Result{}, fmt.Errorf("cleanup: %s %s not found", component, version)
	}
	ledger.Forget(m, r.Name, component, version)
	return Result{Deleted: 1, FreedBytes: freed}, nil
}
