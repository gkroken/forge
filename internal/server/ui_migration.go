package server

import "net/http"

// migrationPage backs GET /ui/admin/migration. The page is state-driven
// (connect form → plan review → live progress), so the template renders
// scaffolding only and static/migration.js hydrates it from
// GET /api/v1/migration.
type migrationPage struct {
	Title     string
	ActiveNav string
	CanApply  bool // false when no async worker is wired
}

// uiMigration renders GET /ui/admin/migration — the Nexus import console.
func (s *Server) uiMigration(w http.ResponseWriter, r *http.Request) {
	if !s.Enforcer.RequireAdminUI(w, r) {
		return
	}
	render(w, tmplMigration, "admin_shell.html", migrationPage{
		Title:     "Nexus migration",
		ActiveNav: "migration",
		CanApply:  s.Queue != nil,
	})
}
