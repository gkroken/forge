// Package format defines the plug-in contract for each package ecosystem.
//
// Adding a new ecosystem (Maven, npm, Helm, CRAN, Docker, ...) means writing
// one Handler. Everything else - storage, routing, repositories - is shared.
package format

import (
	"net/http"

	"forge/internal/blob"
	"forge/internal/meta"
	"forge/internal/repo"
)

// Context is everything a handler needs to serve one request.
type Context struct {
	Repo repo.Repository // the resolved repository
	Blob blob.Store      // raw bytes
	Meta meta.Store      // structured metadata
	HTTP *http.Client    // for proxy upstream fetches
	Sub  string          // request path *within* the repo (no leading slash)
}

// Key namespaces a blob key under the repo so repos never collide in storage.
func (c *Context) Key(sub string) string { return c.Repo.Name + "/" + sub }

// Handler implements one package format.
type Handler interface {
	Format() string
	Serve(w http.ResponseWriter, r *http.Request, c *Context)
}

// Registry maps a format name to its Handler.
type Registry struct{ byFormat map[string]Handler }

func NewRegistry() *Registry { return &Registry{byFormat: map[string]Handler{}} }

func (reg *Registry) Register(h Handler) { reg.byFormat[h.Format()] = h }

func (reg *Registry) For(format string) (Handler, bool) {
	h, ok := reg.byFormat[format]
	return h, ok
}
