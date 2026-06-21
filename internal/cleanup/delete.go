package cleanup

import (
	"fmt"
	"strings"

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
func DeleteVersion(repoName, format, component, version string, b blob.Store, m meta.Store) (Result, error) {
	if component == "" || version == "" {
		return Result{}, fmt.Errorf("cleanup: component and version are required")
	}
	switch format {
	case "maven":
		return deleteMavenVersion(repoName, component, version, b)
	case "cran":
		return deleteCRANVersion(repoName, component, version, b, m)
	case "helm":
		return deleteHelmVersion(repoName, component, version, b, m)
	case "npm":
		return deleteNPMVersion(repoName, component, version, b, m)
	}
	return Result{}, fmt.Errorf("cleanup: unsupported format %q", format)
}

func deleteBlob(b blob.Store, key string, res *Result) {
	info, exists, _ := b.Stat(key)
	if exists {
		res.FreedBytes += info.Size
		b.Delete(key) //nolint:errcheck
		res.Deleted++
	}
}

func deleteMavenVersion(repoName, ga, version string, b blob.Store) (Result, error) {
	// ga is "groupId:artifactId"; the blob layout is groupId-dots-as-slashes/artifactId.
	group, artifact, ok := strings.Cut(ga, ":")
	if !ok {
		return Result{}, fmt.Errorf("cleanup: invalid maven component %q (want groupId:artifactId)", ga)
	}
	gaPath := strings.ReplaceAll(group, ".", "/") + "/" + artifact
	prefix := repoName + "/" + gaPath + "/" + version + "/"
	keys, err := b.List(prefix)
	if err != nil {
		return Result{}, err
	}
	var res Result
	for _, k := range keys {
		deleteBlob(b, k, &res)
	}
	if res.Deleted == 0 {
		return res, fmt.Errorf("cleanup: %s %s not found", ga, version)
	}
	return res, nil
}

func deleteCRANVersion(repoName, pkg, version string, b blob.Store, m meta.Store) (Result, error) {
	var res Result
	deleteBlob(b, repoName+"/src/contrib/"+pkg+"_"+version+".tar.gz", &res)
	m.Delete(repoName+":cran", pkg+"_"+version) //nolint:errcheck
	if res.Deleted == 0 {
		return res, fmt.Errorf("cleanup: %s %s not found", pkg, version)
	}
	return res, nil
}

func deleteHelmVersion(repoName, chart, version string, b blob.Store, m meta.Store) (Result, error) {
	ns := repoName + ":helm"
	var rec helmRecord
	ok, _ := m.GetJSON(ns, chart+"-"+version, &rec)
	filename := rec.Filename
	if filename == "" {
		filename = chart + "-" + version + ".tgz"
	}
	var res Result
	deleteBlob(b, repoName+"/"+filename, &res)
	if ok {
		m.Delete(ns, chart+"-"+version) //nolint:errcheck
	}
	if res.Deleted == 0 {
		return res, fmt.Errorf("cleanup: %s %s not found", chart, version)
	}
	return res, nil
}

func deleteNPMVersion(repoName, pkg, version string, b blob.Store, m meta.Store) (Result, error) {
	versNS := repoName + ":npm:v"
	pkgNS := repoName + ":npm"
	var res Result
	deleteBlob(b, repoName+"/"+pkg+"/-/"+pkg+"-"+version+".tgz", &res)
	m.Delete(versNS, pkg+":"+version) //nolint:errcheck

	// Remove the version from the packument.
	var packument map[string]any
	if ok, _ := m.GetJSON(pkgNS, pkg, &packument); ok {
		if vers, ok := packument["versions"].(map[string]any); ok {
			delete(vers, version)
			packument["versions"] = vers
		}
		m.PutJSON(pkgNS, pkg, packument) //nolint:errcheck
	}
	if res.Deleted == 0 {
		return res, fmt.Errorf("cleanup: %s %s not found", pkg, version)
	}
	return res, nil
}
