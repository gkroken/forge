package server

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"forge/internal/format"
	"forge/internal/obs"
	"forge/internal/repo"
	"forge/internal/selector"
	"forge/internal/webhook"
)

// depGuard is the per-request state of the dependency-confusion guard.
//
// The threat: a client resolves an internal package name through a group (or
// straight from a proxy), and the proxy side serves an attacker's same-named
// package from the public upstream. The guard enforces two ownership signals:
//
//   - explicit claims — selector patterns on hosted repos ("@acme/**",
//     "com/acme/**") protect names before anything is published under them;
//   - auto-derived ownership — a name any hosted group member actually
//     contains is protected in that group without configuration.
//
// Enforcement is split across two hooks. The request-level check (blocks)
// refuses claimed names outright when no hosted copy can serve them: always
// on direct proxy requests, and on group requests when no hosted member owns
// the component. The fan-out check (memberFilter, installed as
// Context.MemberFilter) excludes proxy members from group serving for any
// protected component, so a request that IS served never touches upstream.
// claimedName (installed as Context.NameClaimed) is the pure claims-only
// predicate group index merges apply per entry.
//
// A nil *depGuard means the guard is inactive for this request (hosted repo,
// guard disabled, format not Claimable, or no hosted authority exists) and
// every hook degrades to the unguarded behaviour.
type depGuard struct {
	s      *Server
	rp     repo.Repository // the group or proxy repo being served
	cl     format.Handler
	hosted []repo.Repository // authority set: hosted members (group) or same-format hosted repos (proxy)
	comp   string            // component addressed by this request; "" = not a component path
	owns   map[string]bool   // memoized OwnsComponent per "{repo}\x00{component}"
}

// newDepGuard builds the guard for one request, or nil when it does not
// apply. rp must be the resolved repository and h its format handler.
func (s *Server) newDepGuard(rp repo.Repository, h format.Handler, sub string) *depGuard {
	if rp.Kind == repo.Hosted || !rp.DepGuardEnabled() {
		return nil
	}
	// A format with no component model (oci) answers ClaimPath false for every
	// path via format.Unsupported, so the guard simply never matches.
	g := &depGuard{s: s, rp: rp, cl: h, owns: map[string]bool{}}
	switch rp.Kind {
	case repo.Group:
		for _, name := range rp.Members {
			if m, ok := s.Repos.Get(name); ok && m.Kind == repo.Hosted {
				g.hosted = append(g.hosted, m)
			}
		}
	case repo.Proxy:
		// A standalone proxy has no member list to scope authority; every
		// same-format hosted repo's claims apply. Claimed names are refused
		// even when cached: a cached copy is still upstream content.
		for _, m := range s.Repos.All() {
			if m.Kind == repo.Hosted && m.Format == rp.Format {
				g.hosted = append(g.hosted, m)
			}
		}
	}
	if len(g.hosted) == 0 {
		return nil
	}
	if comp, ok := g.cl.ClaimPath(sub); ok {
		g.comp = comp
	}
	return g
}

// blocks writes a 403 and returns true when this request must be refused: the
// component is explicitly claimed and no hosted copy can serve it. Requests
// for owned components proceed (memberFilter keeps them off upstream); missing
// versions of owned components stay honest 404s.
func (g *depGuard) blocks(w http.ResponseWriter, r *http.Request) bool {
	if g == nil || g.comp == "" {
		return false
	}
	pattern, owner := g.claimMatch(g.comp)
	if pattern == "" {
		return false
	}
	if g.rp.Kind == repo.Group && g.anyHostedOwns(g.comp) {
		return false
	}
	g.s.recordDepGuard(r, g.rp, g.comp, pattern, owner)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	json.NewEncoder(w).Encode(map[string]string{
		"error":     "blocked by dependency-confusion protection",
		"component": g.comp,
		"claim":     pattern,
		"claimedBy": owner,
		"detail": "the name is claimed by hosted repository \"" + owner +
			"\" and is never fetched from an upstream registry; publish it there or adjust its claims",
	})
	return true
}

// memberFilter implements format.Context.MemberFilter for group serving:
// proxy members may not serve a protected component. Hosted members always
// pass, so this cannot recurse through MemberCtx.
func (g *depGuard) memberFilter(member repo.Repository) bool {
	if member.Kind != repo.Proxy || g.comp == "" {
		return true
	}
	if pattern, _ := g.claimMatch(g.comp); pattern != "" {
		return false
	}
	return !g.anyHostedOwns(g.comp)
}

// claimedName implements format.Context.NameClaimed: the pure, claims-only
// predicate for group index merges (no storage I/O — merges call it per entry).
func (g *depGuard) claimedName(name string) bool {
	pattern, _ := g.claimMatch(name)
	return pattern != ""
}

// claimMatch returns the first hosted claim pattern matching name and the
// repo that owns it.
func (g *depGuard) claimMatch(name string) (pattern, owner string) {
	for _, hm := range g.hosted {
		for _, c := range hm.Claims {
			if selector.Match(c, name) {
				return c, hm.Name
			}
		}
	}
	return "", ""
}

func (g *depGuard) anyHostedOwns(comp string) bool {
	for _, hm := range g.hosted {
		key := hm.Name + "\x00" + comp
		owned, seen := g.owns[key]
		if !seen {
			owned = g.cl.OwnsComponent(&format.Context{
				Repo: hm, Blob: g.s.Blob, Meta: g.s.Meta, Repos: g.s.Repos,
			}, comp)
			g.owns[key] = owned
		}
		if owned {
			return true
		}
	}
	return false
}

// recordDepGuard records a refused request across the governance surfaces:
// durable audit log, policy.violation webhook, and the
// forge_depguard_blocked_total metric — same trio as the vulnerability gate.
// All side effects are best-effort and off the serving path.
func (s *Server) recordDepGuard(r *http.Request, rp repo.Repository, component, pattern, owner string) {
	actor := actorLabel(r, s.Auth)
	if s.AuditLog != nil {
		s.AuditLog.Append(obs.AuditEntry{
			Timestamp: time.Now().UTC(),
			Actor:     actor,
			Method:    r.Method,
			Path:      r.URL.Path,
			Status:    http.StatusForbidden,
			Detail:    "dep-guard: blocked " + component + " (claim " + pattern + " by " + owner + ")",
		})
	}
	if s.Metrics != nil && s.Metrics.DepGuardBlocked != nil {
		s.Metrics.DepGuardBlocked.WithLabelValues(rp.Name).Inc()
	}
	if s.Webhooks != nil {
		ev := webhook.Event{
			Type:      webhook.EventPolicyViolation,
			Repo:      rp.Name,
			Format:    rp.Format,
			Path:      component,
			Actor:     actor,
			Timestamp: time.Now().UTC(),
			Data: map[string]any{
				"kind":      "dependency-confusion",
				"component": component,
				"claim":     pattern,
				"claimedBy": owner,
			},
		}
		go s.Webhooks.Dispatch(context.Background(), ev)
	}
}
