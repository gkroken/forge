// Package format defines the plug-in contract for each package ecosystem.
//
// Adding a new ecosystem (Maven, npm, Helm, CRAN, Docker, ...) means writing
// one Handler. Everything else - storage, routing, repositories - is shared.
package format

import (
	"net/http"
	"sort"

	"forge/internal/blob"
	"forge/internal/meta"
	"forge/internal/obs"
	"forge/internal/queue"
	"forge/internal/repo"
)

// Context is everything a handler needs to serve one request.
type Context struct {
	Repo    repo.Repository // the resolved repository
	Blob    blob.Store      // raw bytes
	Meta    meta.Store      // structured metadata
	HTTP    *http.Client    // for proxy upstream fetches
	Sub     string          // request path *within* the repo (no leading slash)
	Repos   *repo.Manager   // non-nil; used by group handlers to look up members
	Queue   queue.Queue     // may be nil; if set, handlers enqueue async regen jobs
	Metrics *obs.Metrics    // may be nil; used to record per-repo cache counters
}

// Key namespaces a blob key under the repo so repos never collide in storage.
func (c *Context) Key(sub string) string { return c.Repo.Name + "/" + sub }

// MemberCtx returns a sub-context for the named member repository.
// Returns (nil, false) if the member doesn't exist or is itself a group
// (groups cannot nest).
func (c *Context) MemberCtx(name string) (*Context, bool) {
	if c.Repos == nil {
		return nil, false
	}
	r, ok := c.Repos.Get(name)
	if !ok || r.Kind == repo.Group {
		return nil, false
	}
	return &Context{Repo: r, Blob: c.Blob, Meta: c.Meta, HTTP: c.HTTP, Sub: c.Sub, Repos: c.Repos, Queue: c.Queue, Metrics: c.Metrics}, true
}

// Handler implements one package format.
type Handler interface {
	Format() string
	Serve(w http.ResponseWriter, r *http.Request, c *Context)
}

// BrowseEntry represents one component (package, chart, image, …) in a repo's
// browse view: a name and all known versions, newest-first where deterministic.
type BrowseEntry struct {
	Name     string
	Versions []string
}

// Browsable is an optional extension to Handler that powers the web UI browse
// and search views. Handlers that do not implement it show a fallback message.
type Browsable interface {
	BrowseRepo(c *Context) ([]BrowseEntry, error)
}

// GroupBrowse merges BrowseRepo results from every member of a group context.
// First member that contains a given Name wins; output is sorted by Name.
func GroupBrowse(h Browsable, c *Context) ([]BrowseEntry, error) {
	seen := map[string]struct{}{}
	var all []BrowseEntry
	for _, name := range c.Repo.Members {
		mc, ok := c.MemberCtx(name)
		if !ok {
			continue
		}
		entries, _ := h.BrowseRepo(mc)
		for _, e := range entries {
			if _, exists := seen[e.Name]; !exists {
				seen[e.Name] = struct{}{}
				all = append(all, e)
			}
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Name < all[j].Name })
	return all, nil
}

// Registry maps a format name to its Handler.
type Registry struct{ byFormat map[string]Handler }

func NewRegistry() *Registry { return &Registry{byFormat: map[string]Handler{}} }

func (reg *Registry) Register(h Handler) { reg.byFormat[h.Format()] = h }

func (reg *Registry) For(format string) (Handler, bool) {
	h, ok := reg.byFormat[format]
	return h, ok
}
