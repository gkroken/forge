package format

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"forge/internal/repo"
)

// watcher records what reached the client and when, so a test can tell
// streaming from buffering.
type watcher struct {
	hdr     http.Header
	code    int
	written []string
}

func newWatcher() *watcher { return &watcher{hdr: make(http.Header)} }

func (x *watcher) Header() http.Header { return x.hdr }
func (x *watcher) WriteHeader(c int)   { x.code = c }
func (x *watcher) Write(b []byte) (int, error) {
	x.written = append(x.written, string(b))
	return len(b), nil
}

// A member that answers successfully must stream: its bytes reach the client
// as it writes them, not after it returns. A group download of a large
// artifact otherwise sits in memory in full, per concurrent request.
func TestSink_StreamsSuccessfulResponse(t *testing.T) {
	w := newWatcher()
	s := NewSink(w)
	s.Header().Set("Content-Type", "application/gzip")
	s.Write([]byte("first"))
	if len(w.written) != 1 || w.written[0] != "first" {
		t.Fatalf("first chunk did not reach the client while the handler was still writing: %v", w.written)
	}
	if w.code != http.StatusOK || w.hdr.Get("Content-Type") != "application/gzip" {
		t.Errorf("commit did not carry status/headers: %d %q", w.code, w.hdr.Get("Content-Type"))
	}
	s.Write([]byte("second"))
	if !s.Served() {
		t.Error("Served() = false after a successful write")
	}
	if strings.Join(w.written, "") != "firstsecond" {
		t.Errorf("body = %q", strings.Join(w.written, ""))
	}
}

// A member that does not have the artifact must reach the client with nothing
// at all — no status, no headers, no error body — so the next member can serve.
func TestSink_FailedMemberWritesNothing(t *testing.T) {
	w := newWatcher()
	s := NewSink(w)
	s.Header().Set("Content-Type", "text/plain")
	http.Error(s, "not found", http.StatusNotFound)
	if s.Served() {
		t.Error("Served() = true for a 404 member")
	}
	if w.code != 0 || len(w.written) != 0 || len(w.hdr) != 0 {
		t.Errorf("a failed member leaked to the client: code=%d hdr=%v body=%v", w.code, w.hdr, w.written)
	}
}

type memberHandler struct {
	Unsupported
	has map[string]string
}

func (memberHandler) Format() string { return "test" }
func (m memberHandler) Serve(w http.ResponseWriter, r *http.Request, c *Context) {
	body, ok := m.has[c.Repo.Name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("X-Served-By", c.Repo.Name)
	fmt.Fprint(w, body)
}

// GroupFetch serves the first member that has the artifact, and nothing from
// the members that did not.
func TestGroupFetch_FirstMemberWithTheArtifactWins(t *testing.T) {
	mgr := repo.NewManager()
	for _, r := range []repo.Repository{
		{Name: "empty", Format: "test", Kind: repo.Hosted},
		{Name: "holder", Format: "test", Kind: repo.Hosted},
		{Name: "later", Format: "test", Kind: repo.Hosted},
		{Name: "grp", Format: "test", Kind: repo.Group, Members: []string{"empty", "holder", "later"}},
	} {
		if err := mgr.Add(r); err != nil {
			t.Fatal(err)
		}
	}
	h := memberHandler{has: map[string]string{"holder": "chart-bytes", "later": "wrong"}}
	grp, _ := mgr.Get("grp")
	rec := httptest.NewRecorder()
	c := &Context{Repo: grp, Sub: "x.tgz", Repos: mgr}
	if !GroupFetch(h, rec, httptest.NewRequest("GET", "/x.tgz", nil), c) {
		t.Fatal("GroupFetch reported no member served the artifact")
	}
	if rec.Body.String() != "chart-bytes" {
		t.Errorf("body = %q, want the first holding member's", rec.Body.String())
	}
	if got := rec.Header().Get("X-Served-By"); got != "holder" {
		t.Errorf("X-Served-By = %q, want holder", got)
	}

	rec = httptest.NewRecorder()
	c2 := &Context{Repo: grp, Sub: "missing.tgz", Repos: mgr}
	h2 := memberHandler{}
	if GroupFetch(h2, rec, httptest.NewRequest("GET", "/missing.tgz", nil), c2) {
		t.Error("GroupFetch reported success when no member had the artifact")
	}
	if rec.Body.Len() != 0 {
		t.Errorf("failed members wrote to the client: %q", rec.Body.String())
	}
}
