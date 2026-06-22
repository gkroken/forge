package server

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"forge/internal/obs"
)

// auditHistoryPageSize is the number of rows shown per history page.
const auditHistoryPageSize = 50

// auditTimeLayout is used for history rows, which can span days (the recent
// dashboard/observability views use a time-only layout).
const auditTimeLayout = "2006-01-02 15:04:05"

type auditHistoryPage struct {
	Title     string
	ActiveNav string
	Replica   string
	Rows      []auditRow
	Actor     string // echoed filter value
	Path      string // echoed filter value
	Durable   bool   // true when the sink is the Postgres querier (full history)
	HasMore   bool
	OlderURL  string // keyset link to the next (older) page, preserving filters
	ResetURL  string // back to the newest page with current filters
}

// uiAuditHistory renders GET /ui/admin/audit — a filtered, keyset-paginated view
// of the durable audit log. With the in-memory sink it shows the recent window.
func (s *Server) uiAuditHistory(w http.ResponseWriter, r *http.Request) {
	if !s.Enforcer.RequireAdminUI(w, r) {
		return
	}
	actor := r.URL.Query().Get("actor")
	pathLike := r.URL.Query().Get("path")

	page := auditHistoryPage{
		Title:     "Audit history",
		ActiveNav: "observability",
		Replica:   replicaID(),
		Actor:     actor,
		Path:      pathLike,
		ResetURL:  auditHistoryURL(actor, pathLike, obs.AuditCursor{}),
	}

	if q, ok := s.AuditLog.(obs.AuditQuerier); ok {
		page.Durable = true
		// Fetch one extra row to detect whether an older page exists.
		recs, err := q.Query(r.Context(), obs.AuditFilter{
			Actor:    actor,
			PathLike: pathLike,
			Cursor:   parseAuditCursor(r),
			Limit:    auditHistoryPageSize + 1,
		})
		if err != nil {
			http.Error(w, "audit query failed", http.StatusInternalServerError)
			return
		}
		page.HasMore = len(recs) > auditHistoryPageSize
		if page.HasMore {
			recs = recs[:auditHistoryPageSize]
		}
		for _, rec := range recs {
			page.Rows = append(page.Rows, buildAuditRow(rec.AuditEntry, auditTimeLayout))
		}
		if page.HasMore && len(recs) > 0 {
			last := recs[len(recs)-1]
			page.OlderURL = auditHistoryURL(actor, pathLike,
				obs.AuditCursor{Timestamp: last.Timestamp, ID: last.ID})
		}
	} else if s.AuditLog != nil {
		for _, e := range s.AuditLog.Recent(200) {
			if actor != "" && e.Actor != actor {
				continue
			}
			if pathLike != "" && !strings.Contains(strings.ToLower(e.Path), strings.ToLower(pathLike)) {
				continue
			}
			page.Rows = append(page.Rows, buildAuditRow(e, auditTimeLayout))
		}
	}

	render(w, tmplAuditHistory, "admin_shell.html", page)
}

// parseAuditCursor reads the keyset cursor (before_ts / before_id) from a request.
func parseAuditCursor(r *http.Request) obs.AuditCursor {
	id, _ := strconv.ParseInt(r.URL.Query().Get("before_id"), 10, 64)
	var ts time.Time
	if v := r.URL.Query().Get("before_ts"); v != "" {
		if parsed, err := time.Parse(time.RFC3339Nano, v); err == nil {
			ts = parsed
		}
	}
	return obs.AuditCursor{Timestamp: ts, ID: id}
}

// auditHistoryURL builds a /ui/admin/audit link preserving filters and, when the
// cursor is set, the keyset position.
func auditHistoryURL(actor, pathLike string, cur obs.AuditCursor) string {
	v := url.Values{}
	if actor != "" {
		v.Set("actor", actor)
	}
	if pathLike != "" {
		v.Set("path", pathLike)
	}
	if !cur.IsZero() {
		v.Set("before_id", strconv.FormatInt(cur.ID, 10))
		v.Set("before_ts", cur.Timestamp.UTC().Format(time.RFC3339Nano))
	}
	if enc := v.Encode(); enc != "" {
		return "/ui/admin/audit?" + enc
	}
	return "/ui/admin/audit"
}
