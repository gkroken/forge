package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"forge/internal/auth"
	"forge/internal/blob"
	"forge/internal/format"
	"forge/internal/format/cran"
	"forge/internal/format/helm"
	"forge/internal/format/maven"
	"forge/internal/format/npm"
	"forge/internal/format/oci"
	"forge/internal/integrity"
	"forge/internal/meta"
	"forge/internal/queue"
	"forge/internal/repo"
)

// --- fixtures ------------------------------------------------------------------

func tgzWithFile(t *testing.T, name, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0644, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	tw.Write([]byte(content)) //nolint:errcheck
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// fakeNexusServer serves a small but complete Nexus 3: five migratable hosted
// repos (one per forge format), an unsupported one, a proxy, a group, and a
// security model.
type fakeNexusServer struct {
	t   *testing.T
	srv *httptest.Server

	jarBytes, pomBytes            []byte
	npmTarball                    []byte
	chartBytes, cranBytes         []byte
	ociConfig, ociLayer, ociManif []byte
	ociConfigDgst, ociLayerDgst   string
	ociManifDgst                  string
}

func newFakeNexus(t *testing.T) *fakeNexusServer {
	f := &fakeNexusServer{t: t}
	f.jarBytes = []byte("fake-jar-bytes")
	f.pomBytes = []byte("<project/>")
	f.npmTarball = tgzWithFile(t, "package/package.json", `{"name":"left-pad"}`)
	f.chartBytes = tgzWithFile(t, "web/Chart.yaml", "name: web\nversion: 1.2.3\napiVersion: v2\ndescription: x\n")
	f.cranBytes = tgzWithFile(t, "jsonlite/DESCRIPTION", "Package: jsonlite\nVersion: 2.0.0\nLicense: MIT\n")

	f.ociConfig = []byte(`{"architecture":"amd64","os":"linux"}`)
	f.ociLayer = []byte("layer-bytes-are-opaque")
	f.ociConfigDgst = "sha256:" + sha256Hex(f.ociConfig)
	f.ociLayerDgst = "sha256:" + sha256Hex(f.ociLayer)
	manifest := map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.docker.distribution.manifest.v2+json",
		"config": map[string]any{
			"mediaType": "application/vnd.docker.container.image.v1+json",
			"digest":    f.ociConfigDgst, "size": len(f.ociConfig),
		},
		"layers": []map[string]any{{
			"mediaType": "application/vnd.docker.image.rootfs.diff.tar.gzip",
			"digest":    f.ociLayerDgst, "size": len(f.ociLayer),
		}},
	}
	f.ociManif, _ = json.Marshal(manifest)
	f.ociManifDgst = "sha256:" + sha256Hex(f.ociManif)

	f.srv = httptest.NewServer(f.handler())
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeNexusServer) url() string { return f.srv.URL }

func (f *fakeNexusServer) handler() http.Handler {
	mux := http.NewServeMux()
	u := func() string { return f.srv.URL } // deferred: srv not yet set at build time

	mux.HandleFunc("/service/rest/v1/repositorySettings", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[
		 {"name":"maven-releases","format":"maven2","type":"hosted"},
		 {"name":"npm-internal","format":"npm","type":"hosted"},
		 {"name":"helm-charts","format":"helm","type":"hosted"},
		 {"name":"r-packages","format":"r","type":"hosted"},
		 {"name":"docker-apps","format":"docker","type":"hosted"},
		 {"name":"pypi-internal","format":"pypi","type":"hosted"},
		 {"name":"npm-mirror","format":"npm","type":"proxy","attributes":{"proxy":{"remoteUrl":"https://registry.npmjs.org"}}},
		 {"name":"maven-all","format":"maven2","type":"group","attributes":{"group":{"memberNames":["maven-releases"]}}}
		]`)
	})

	comps := func(items string) string {
		return `{"items":[` + items + `],"continuationToken":""}`
	}
	mux.HandleFunc("/service/rest/v1/components", func(w http.ResponseWriter, r *http.Request) {
		base := u()
		switch r.URL.Query().Get("repository") {
		case "maven-releases":
			fmt.Fprint(w, comps(`
			 {"id":"m1","name":"org.acme:app","version":"1.0","assets":[
			   {"path":"org/acme/app/1.0/app-1.0.jar","downloadUrl":"`+base+`/dl/app.jar","fileSize":14},
			   {"path":"org/acme/app/1.0/app-1.0.jar.sha1","downloadUrl":"`+base+`/dl/app.jar.sha1"},
			   {"path":"org/acme/app/1.0/app-1.0.pom","downloadUrl":"`+base+`/dl/app.pom"},
			   {"path":"org/acme/app/maven-metadata.xml","downloadUrl":"`+base+`/dl/mm.xml"}
			 ]}`))
		case "npm-internal":
			fmt.Fprint(w, comps(`
			 {"id":"n1","name":"left-pad","version":"1.0.0","assets":[
			   {"path":"left-pad/-/left-pad-1.0.0.tgz","downloadUrl":"`+base+`/dl/left-pad-1.0.0.tgz"}]},
			 {"id":"n2","name":"left-pad","version":"1.1.0","assets":[
			   {"path":"left-pad/-/left-pad-1.1.0.tgz","downloadUrl":"`+base+`/dl/left-pad-1.1.0.tgz"}]}`))
		case "helm-charts":
			fmt.Fprint(w, comps(`
			 {"id":"h1","name":"web","version":"1.2.3","assets":[
			   {"path":"web-1.2.3.tgz","downloadUrl":"`+base+`/dl/web-1.2.3.tgz"}]}`))
		case "r-packages":
			fmt.Fprint(w, comps(`
			 {"id":"r1","name":"jsonlite","version":"2.0.0","assets":[
			   {"path":"src/contrib/jsonlite_2.0.0.tar.gz","downloadUrl":"`+base+`/dl/jsonlite_2.0.0.tar.gz"}]}`))
		case "docker-apps":
			fmt.Fprint(w, comps(`
			 {"id":"d1","name":"acme/app","version":"v1","assets":[
			   {"path":"v2/acme/app/manifests/v1","downloadUrl":"`+base+`/repository/docker-apps/v2/acme/app/manifests/v1"}]}`))
		default:
			fmt.Fprint(w, comps(``))
		}
	})

	// Asset bodies.
	serveBytes := func(b func() []byte) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { w.Write(b()) } //nolint:errcheck
	}
	mux.HandleFunc("/dl/app.jar", serveBytes(func() []byte { return f.jarBytes }))
	mux.HandleFunc("/dl/app.jar.sha1", func(w http.ResponseWriter, r *http.Request) {
		h := sha1Hex(f.jarBytes)
		w.Write([]byte(h)) //nolint:errcheck
	})
	mux.HandleFunc("/dl/app.pom", serveBytes(func() []byte { return f.pomBytes }))
	mux.HandleFunc("/dl/mm.xml", serveBytes(func() []byte { return []byte("<metadata/>") }))
	mux.HandleFunc("/dl/left-pad-1.0.0.tgz", serveBytes(func() []byte { return f.npmTarball }))
	mux.HandleFunc("/dl/left-pad-1.1.0.tgz", serveBytes(func() []byte { return f.npmTarball }))
	mux.HandleFunc("/dl/web-1.2.3.tgz", serveBytes(func() []byte { return f.chartBytes }))
	mux.HandleFunc("/dl/jsonlite_2.0.0.tar.gz", serveBytes(func() []byte { return f.cranBytes }))

	// npm packument (fetched by the migrator for version objects + dist-tags).
	mux.HandleFunc("/repository/npm-internal/left-pad", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{
		 "name":"left-pad",
		 "dist-tags":{"latest":"1.1.0","stable":"1.0.0"},
		 "versions":{
		   "1.0.0":{"name":"left-pad","version":"1.0.0","description":"pads left","dist":{"tarball":"%s/dl/left-pad-1.0.0.tgz"}},
		   "1.1.0":{"name":"left-pad","version":"1.1.0","description":"pads left","dist":{"tarball":"%s/dl/left-pad-1.1.0.tgz"}}
		 }}`, u(), u())
	})

	// docker registry endpoints (repository-path form, as Nexus downloadUrls use).
	mux.HandleFunc("/repository/docker-apps/v2/acme/app/manifests/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.docker.distribution.manifest.v2+json")
		w.Write(f.ociManif) //nolint:errcheck
	})
	mux.HandleFunc("/repository/docker-apps/v2/acme/app/blobs/", func(w http.ResponseWriter, r *http.Request) {
		dgst := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		switch dgst {
		case f.ociConfigDgst:
			w.Write(f.ociConfig) //nolint:errcheck
		case f.ociLayerDgst:
			w.Write(f.ociLayer) //nolint:errcheck
		default:
			http.NotFound(w, r)
		}
	})

	// Security model.
	mux.HandleFunc("/service/rest/v1/security/users", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[
		 {"userId":"admin","firstName":"Admin","lastName":"User","source":"default","status":"active","roles":["nx-admin"]},
		 {"userId":"anonymous","firstName":"Anonymous","lastName":"User","source":"default","status":"active","roles":["nx-anonymous"]},
		 {"userId":"alice","firstName":"Alice","lastName":"Ash","source":"default","status":"active","roles":["developers"]},
		 {"userId":"bob","firstName":"Bob","lastName":"Berg","source":"default","status":"active","roles":["developers","readers"]},
		 {"userId":"root2","firstName":"Second","lastName":"Admin","source":"default","status":"active","roles":["nx-admin"]}
		]`)
	})
	mux.HandleFunc("/service/rest/v1/security/roles", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[
		 {"id":"nx-admin","name":"nx-admin","privileges":["nx-all"],"roles":[]},
		 {"id":"nx-anonymous","name":"nx-anonymous","privileges":["nx-repository-view-maven2-maven-releases-read","nx-healthcheck-read"],"roles":[]},
		 {"id":"developers","name":"Developers","description":"dev team","privileges":["nx-repository-view-maven2-maven-releases-add","acme-scoped"],"roles":["readers"]},
		 {"id":"readers","name":"Readers","privileges":["nx-repository-view-*-*-read"],"roles":[]}
		]`)
	})
	mux.HandleFunc("/service/rest/v1/security/privileges", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[
		 {"name":"nx-all","type":"wildcard"},
		 {"name":"nx-healthcheck-read","type":"application","actions":["READ"]},
		 {"name":"nx-repository-view-maven2-maven-releases-read","type":"repository-view","format":"maven2","repository":"maven-releases","actions":["READ","BROWSE"]},
		 {"name":"nx-repository-view-maven2-maven-releases-add","type":"repository-view","format":"maven2","repository":"maven-releases","actions":["ADD","EDIT"]},
		 {"name":"nx-repository-view-*-*-read","type":"repository-view","format":"*","repository":"*","actions":["READ"]},
		 {"name":"acme-scoped","type":"repository-content-selector","repository":"maven-releases","contentSelector":"acme-only","actions":["READ","ADD"]}
		]`)
	})
	mux.HandleFunc("/service/rest/v1/security/content-selectors", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"name":"acme-only","expression":"path =~ \"^/com/acme/.*\""}]`)
	})
	mux.HandleFunc("/service/rest/v1/security/anonymous", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"enabled":true,"userId":"anonymous"}`)
	})
	mux.HandleFunc("/service/rest/v1/repositories", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[]`)
	})
	return mux
}

func sha1Hex(b []byte) string {
	// maven sidecars store hex sha1; reuse blob's helper if exported, else inline.
	return blob.SHA1(b)
}

// newMigrationServer wires a full server: all five format handlers, FS
// stores, user/role stores, and an in-memory queue (no worker — the tests
// drive the job synchronously).
func newMigrationServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	b, _ := blob.NewFS(filepath.Join(dir, "b"))
	m, _ := meta.NewFS(filepath.Join(dir, "m"))
	mgr := repo.NewManager()
	reg := format.NewRegistry()
	reg.Register(maven.New())
	reg.Register(npm.New())
	reg.Register(helm.New())
	reg.Register(cran.New())
	reg.Register(oci.New())
	s := New(mgr, reg, b, m, nil). // nil auth = eval mode (RequireAdmin passes)
					WithUsers(auth.NewUserStore(m)).
					WithRoles(auth.NewRoleStore(m))
	s.Queue = queue.NewMem(64)
	return s
}

func planViaAPI(t *testing.T, s *Server, nexusURL string, includeSecurity bool) migrationPlan {
	t.Helper()
	body, _ := json.Marshal(migrationPlanRequest{
		URL: nexusURL, Username: "admin", Password: "admin123", IncludeSecurity: includeSecurity,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/migration/plan", bytes.NewReader(body))
	rw := httptest.NewRecorder()
	s.handleMigration(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("plan: %d %s", rw.Code, rw.Body.String())
	}
	var plan migrationPlan
	if err := json.Unmarshal(rw.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	return plan
}

// --- tests -----------------------------------------------------------------------

func TestMigrationPlan(t *testing.T) {
	nx := newFakeNexus(t)
	s := newMigrationServer(t)

	plan := planViaAPI(t, s, nx.url(), true)

	actions := map[string]string{}
	for _, rp := range plan.Repos {
		actions[rp.Source] = rp.Action
	}
	for _, name := range []string{"maven-releases", "npm-internal", "helm-charts", "r-packages", "docker-apps", "npm-mirror", "maven-all"} {
		if actions[name] != "create" {
			t.Errorf("%s action = %q, want create", name, actions[name])
		}
	}
	if actions["pypi-internal"] != "skip" {
		t.Errorf("pypi-internal action = %q, want skip", actions["pypi-internal"])
	}
	// Groups must sort last so members exist before the group is created.
	if last := plan.Repos[len(plan.Repos)-1]; last.Source != "maven-all" {
		t.Errorf("last plan row = %s, want the group", last.Source)
	}
	// Hosted inventory counted (maven-metadata.xml still counts as a source
	// asset here; the transfer reports it as generated-skip).
	for _, rp := range plan.Repos {
		if rp.Source == "maven-releases" && (rp.Components != 1 || rp.Assets != 4) {
			t.Errorf("maven-releases inventory = %d comps %d assets", rp.Components, rp.Assets)
		}
		if rp.Source == "npm-internal" && rp.Components != 2 {
			t.Errorf("npm-internal components = %d", rp.Components)
		}
	}

	// Security plan.
	if plan.Security == nil {
		t.Fatal("security plan missing")
	}
	roleActions := map[string]roleMigPlan{}
	for _, r := range plan.Security.Roles {
		roleActions[r.ID] = r
	}
	if roleActions["nx-admin"].Action != "skip" || roleActions["nx-anonymous"].Action != "skip" {
		t.Errorf("builtin roles should skip: %+v", plan.Security.Roles)
	}
	dev := roleActions["developers"]
	if dev.Action != "create" {
		t.Fatalf("developers = %+v", dev)
	}
	// developers = add on maven-releases + selector-scoped read/add + nested
	// readers (read on *). The application privilege lands in notes.
	var sawSelector, sawStar bool
	for _, g := range dev.Grants {
		if len(g.Selectors) == 1 && g.Selectors[0] == "com/acme/**" {
			sawSelector = true
		}
		if g.Repo == "*" {
			sawStar = true
		}
	}
	if !sawSelector || !sawStar {
		t.Errorf("developers grants missing selector/star: %+v", dev.Grants)
	}
	if len(roleActions["nx-anonymous"].Notes) != 0 && roleActions["nx-anonymous"].Action != "skip" {
		t.Errorf("unexpected: %+v", roleActions["nx-anonymous"])
	}

	users := map[string]userMigPlan{}
	for _, u := range plan.Security.Users {
		users[u.Username] = u
	}
	if users["admin"].Action != "skip" || users["anonymous"].Action != "skip" {
		t.Errorf("admin/anonymous should skip: %+v", plan.Security.Users)
	}
	if users["alice"].Role != "developers" || users["alice"].Action != "create" {
		t.Errorf("alice = %+v", users["alice"])
	}
	if users["root2"].Role != "Administrator" {
		t.Errorf("root2 = %+v", users["root2"])
	}
	// bob has two roles → synthetic union role.
	if users["bob"].Role != "bob-roles" {
		t.Errorf("bob = %+v", users["bob"])
	}
	if _, ok := roleActions["bob-roles"]; !ok {
		var ids []string
		for _, r := range plan.Security.Roles {
			ids = append(ids, r.ID)
		}
		t.Errorf("union role bob-roles missing from plan roles: %v", ids)
	}
	// Anonymous read derived from nx-anonymous's read privilege.
	if len(plan.Security.AnonymousReadRepos) != 1 || plan.Security.AnonymousReadRepos[0] != "maven-releases" {
		t.Errorf("anonymousReadRepos = %v", plan.Security.AnonymousReadRepos)
	}
}

func TestMigrationRun_ContentAndSecurity(t *testing.T) {
	nx := newFakeNexus(t)
	s := newMigrationServer(t)
	planViaAPI(t, s, nx.url(), true)

	// Apply → spec gains PublicBase; then run the job synchronously.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/migration/apply", nil)
	req.Host = "forge.example:8080"
	rw := httptest.NewRecorder()
	s.handleMigration(rw, req)
	if rw.Code != http.StatusAccepted {
		t.Fatalf("apply: %d %s", rw.Code, rw.Body.String())
	}
	s.runMigration(context.Background())

	run, ok := s.migGetRun()
	if !ok || run.Status != "complete" {
		t.Fatalf("run = %+v", run)
	}

	// Repos created with the right shapes.
	for name, want := range map[string]struct {
		format string
		kind   repo.Kind
	}{
		"maven-releases": {"maven", repo.Hosted},
		"npm-internal":   {"npm", repo.Hosted},
		"helm-charts":    {"helm", repo.Hosted},
		"r-packages":     {"cran", repo.Hosted},
		"docker-apps":    {"oci", repo.Hosted},
		"npm-mirror":     {"npm", repo.Proxy},
		"maven-all":      {"maven", repo.Group},
	} {
		rp, ok := s.Repos.Get(name)
		if !ok || rp.Format != want.format || rp.Kind != want.kind {
			t.Errorf("repo %s = %+v ok=%v", name, rp, ok)
		}
	}
	if _, ok := s.Repos.Get("pypi-internal"); ok {
		t.Error("pypi repo must not be created")
	}
	if rp, _ := s.Repos.Get("npm-mirror"); rp.Upstream != "https://registry.npmjs.org" {
		t.Errorf("proxy upstream = %q", rp.Upstream)
	}
	if rp, _ := s.Repos.Get("maven-all"); len(rp.Members) != 1 || rp.Members[0] != "maven-releases" {
		t.Errorf("group members = %v", rp.Members)
	}
	// Anonymous read applied from the security plan.
	if rp, _ := s.Repos.Get("maven-releases"); !rp.AnonymousRead {
		t.Error("maven-releases should have anonymous read")
	}

	// maven: jar + sidecar + pom copied; maven-metadata.xml NOT copied.
	for _, key := range []string{
		"maven-releases/org/acme/app/1.0/app-1.0.jar",
		"maven-releases/org/acme/app/1.0/app-1.0.jar.sha1",
		"maven-releases/org/acme/app/1.0/app-1.0.pom",
	} {
		if _, ok, _ := s.Blob.Stat(key); !ok {
			t.Errorf("missing blob %s", key)
		}
	}
	if _, ok, _ := s.Blob.Stat("maven-releases/org/acme/app/maven-metadata.xml"); ok {
		t.Error("maven-metadata.xml must not be copied (forge generates it)")
	}

	// npm: both versions publish through the real handler → version records,
	// packument, tarballs, merged dist-tags with the migration host baked in.
	var pack struct {
		DistTags map[string]string          `json:"dist-tags"`
		Versions map[string]json.RawMessage `json:"versions"`
	}
	if ok, _ := s.Meta.GetJSON("npm-internal:npm", "left-pad", &pack); !ok {
		t.Fatal("packument missing")
	}
	if len(pack.Versions) != 2 || pack.DistTags["latest"] != "1.1.0" || pack.DistTags["stable"] != "1.0.0" {
		t.Errorf("packument = %+v", pack)
	}
	var vobj struct {
		Dist struct {
			Tarball string `json:"tarball"`
		} `json:"dist"`
	}
	json.Unmarshal(pack.Versions["1.1.0"], &vobj) //nolint:errcheck
	if !strings.Contains(vobj.Dist.Tarball, "forge.example:8080/repository/npm-internal/") {
		t.Errorf("tarball URL not rewritten to forge: %s", vobj.Dist.Tarball)
	}
	if _, ok, _ := s.Blob.Stat("npm-internal/left-pad/-/left-pad-1.1.0.tgz"); !ok {
		t.Error("npm tarball blob missing")
	}

	// helm: chart record written by the real upload path.
	var chart struct {
		Digest string `json:"digest"`
	}
	if ok, _ := s.Meta.GetJSON("helm-charts:helm", "web-1.2.3", &chart); !ok || chart.Digest == "" {
		t.Errorf("chart record = %+v ok=%v", chart, ok)
	}

	// cran: DESCRIPTION parsed by the real handler.
	if _, ok, _ := s.Blob.Stat("r-packages/src/contrib/jsonlite_2.0.0.tar.gz"); !ok {
		t.Error("cran tarball missing")
	}

	// oci: blobs + manifest + tag mapping.
	nxf := nx
	for _, key := range []string{
		"docker-apps/blobs/" + nxf.ociConfigDgst,
		"docker-apps/blobs/" + nxf.ociLayerDgst,
		"docker-apps/manifests/" + nxf.ociManifDgst,
	} {
		if _, ok, _ := s.Blob.Stat(key); !ok {
			t.Errorf("missing oci object %s", key)
		}
	}
	var tagDgst string
	if ok, _ := s.Meta.GetJSON("docker-apps:oci", "tags/acme/app/v1", &tagDgst); !ok || tagDgst != nxf.ociManifDgst {
		t.Errorf("tag mapping = %q ok=%v", tagDgst, ok)
	}

	// Per-repo states: no failures, counts consistent, verify enqueued.
	states := map[string]repoMigState{}
	keys, _ := s.Meta.List(migrationNS)
	for _, k := range keys {
		if strings.HasPrefix(k, "state:") {
			var st repoMigState
			s.Meta.GetJSON(migrationNS, k, &st) //nolint:errcheck
			states[st.Repo] = st
		}
	}
	for _, name := range []string{"maven-releases", "npm-internal", "helm-charts", "r-packages", "docker-apps"} {
		st := states[name]
		if st.Status != "complete" || st.Failed != 0 {
			t.Errorf("%s state = %+v", name, st)
		}
		if !st.VerifyEnqueued {
			t.Errorf("%s: verify not enqueued", name)
		}
		rep, ok, _ := integrity.NewStore(s.Meta).Get(name)
		if !ok || rep.Status != integrity.StatusQueued {
			t.Errorf("%s integrity report = %+v ok=%v", name, rep, ok)
		}
	}
	// maven: 3 assets transferred (metadata skipped entirely from counts).
	if st := states["maven-releases"]; st.Migrated != 3 || st.SourceAssets != 3 {
		t.Errorf("maven counts = %+v", st)
	}
	if st := states["npm-internal"]; st.Migrated != 2 {
		t.Errorf("npm counts = %+v", st)
	}
	// oci: 2 blobs + 1 manifest.
	if st := states["docker-apps"]; st.Migrated != 3 {
		t.Errorf("oci counts = %+v", st)
	}

	// Security: role with grants, disabled users, union role.
	role, ok, _ := s.Roles.Get("developers")
	if !ok || len(role.Grants) == 0 {
		t.Fatalf("developers role = %+v ok=%v", role, ok)
	}
	if err := auth.ValidateGrants(role.Grants); err != nil {
		t.Errorf("imported grants invalid: %v", err)
	}
	alice, ok, _ := s.Users.Get("alice")
	if !ok || !alice.Disabled || alice.Role != "developers" {
		t.Errorf("alice = %+v ok=%v", alice, ok)
	}
	if _, ok, _ := s.Roles.Get("bob-roles"); !ok {
		t.Error("union role bob-roles not created")
	}
	if run.SecurityApplied == nil || run.SecurityApplied.RolesCreated < 3 || run.SecurityApplied.UsersCreated != 3 {
		t.Errorf("securityApplied = %+v", run.SecurityApplied)
	}

	// The imported role actually mints working session grants.
	grants := auth.GrantsForRoleName(s.Roles, "developers")
	tok := auth.Token{Grants: grants}
	if !tok.Allows("maven-releases", "org/acme/app/2.0/app.jar", auth.ActionWrite) {
		t.Error("developers should write to maven-releases")
	}
	if !tok.Allows("npm-internal", "anything", auth.ActionRead) {
		t.Error("developers (via nested readers) should read everywhere")
	}
}

func TestMigrationRun_IdempotentResume(t *testing.T) {
	nx := newFakeNexus(t)
	s := newMigrationServer(t)
	planViaAPI(t, s, nx.url(), false)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/migration/apply", nil)
	req.Host = "forge.local"
	rw := httptest.NewRecorder()
	s.handleMigration(rw, req)
	s.runMigration(context.Background())

	// Second pass: everything already present → skipped, nothing re-copied.
	s.runMigration(context.Background())
	keys, _ := s.Meta.List(migrationNS)
	for _, k := range keys {
		if !strings.HasPrefix(k, "state:") {
			continue
		}
		var st repoMigState
		s.Meta.GetJSON(migrationNS, k, &st) //nolint:errcheck
		if st.Failed != 0 {
			t.Errorf("%s: failures on resume: %+v", st.Repo, st.Failures)
		}
		switch st.Repo {
		case "maven-releases", "npm-internal", "helm-charts", "r-packages", "docker-apps":
			if st.Migrated != 0 || st.Skipped == 0 {
				t.Errorf("%s: resume should skip everything: %+v", st.Repo, st)
			}
		}
	}
}

func TestMigrationAPI_GuardsAndReset(t *testing.T) {
	s := newMigrationServer(t)

	// Apply without a plan → 409.
	rw := httptest.NewRecorder()
	s.handleMigration(rw, httptest.NewRequest(http.MethodPost, "/api/v1/migration/apply", nil))
	if rw.Code != http.StatusConflict {
		t.Fatalf("apply without plan = %d", rw.Code)
	}

	// Status with nothing stored.
	rw = httptest.NewRecorder()
	s.handleMigration(rw, httptest.NewRequest(http.MethodGet, "/api/v1/migration", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d", rw.Code)
	}

	// Plan against an unreachable source → 502.
	body, _ := json.Marshal(migrationPlanRequest{URL: "http://127.0.0.1:1"})
	rw = httptest.NewRecorder()
	s.handleMigration(rw, httptest.NewRequest(http.MethodPost, "/api/v1/migration/plan", bytes.NewReader(body)))
	if rw.Code != http.StatusBadGateway {
		t.Fatalf("unreachable plan = %d %s", rw.Code, rw.Body.String())
	}

	// Reset clears state; the password never leaves via GET.
	nx := newFakeNexus(t)
	planViaAPI(t, s, nx.url(), false)
	rw = httptest.NewRecorder()
	s.handleMigration(rw, httptest.NewRequest(http.MethodGet, "/api/v1/migration", nil))
	if strings.Contains(rw.Body.String(), "admin123") {
		t.Fatal("password leaked in status response")
	}
	rw = httptest.NewRecorder()
	s.handleMigration(rw, httptest.NewRequest(http.MethodPost, "/api/v1/migration/reset", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("reset = %d", rw.Code)
	}
	if _, ok := s.migGetSpec(); ok {
		t.Fatal("spec survived reset")
	}
}

func TestUIMigrationPage(t *testing.T) {
	s := newMigrationServer(t)
	rw := httptest.NewRecorder()
	s.uiMigration(rw, httptest.NewRequest(http.MethodGet, "/ui/admin/migration", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d", rw.Code)
	}
	body := rw.Body.String()
	for _, want := range []string{"migration-root", "mig-plan-form", "migration.js", "admin-sidebar"} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q", want)
		}
	}
	if !strings.Contains(body, `data-can-apply="1"`) {
		t.Error("CanApply not reflected (queue is wired in this fixture)")
	}
}
