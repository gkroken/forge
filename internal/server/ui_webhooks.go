package server

import (
	"net/http"

	"forge/internal/queue"
	"forge/internal/repo"
	"forge/internal/webhook"
)

type webhooksPage struct {
	Title       string
	ActiveNav   string
	Count       int
	ActiveCount int
	EventType   string
	Delivery    string
	Endpoints   []webhookRow
	Repos       []string
}

type webhookRow struct {
	ID          string
	Name        string
	URL         string
	Repo        string
	Status      string
	StatusClass string
}

// uiWebhooks renders the webhook endpoints admin page.
func (s *Server) uiWebhooks(w http.ResponseWriter, r *http.Request) {
	if !s.Enforcer.RequireAdminUI(w, r) {
		return
	}

	page := webhooksPage{
		Title:     "Webhooks",
		ActiveNav: "webhooks",
		EventType: webhook.EventArtifactPublished,
		Delivery:  s.deliveryMode(),
	}

	// Repository options for the filter (everything but groups, which own no
	// storage and never publish).
	for _, rp := range s.Repos.All() {
		if rp.Kind != repo.Group {
			page.Repos = append(page.Repos, rp.Name)
		}
	}

	if s.Webhooks != nil {
		subs, err := s.Webhooks.Store().List()
		if err == nil {
			for _, sub := range subs {
				status, cls := "Paused", "pill-muted"
				if sub.Enabled {
					status, cls = "Active", "pill-ok"
					page.ActiveCount++
				}
				repoLabel := sub.Repo
				if repoLabel == "" || repoLabel == "*" {
					repoLabel = "All repositories"
				}
				page.Endpoints = append(page.Endpoints, webhookRow{
					ID: sub.ID, Name: sub.Name, URL: sub.URL,
					Repo: repoLabel, Status: status, StatusClass: cls,
				})
			}
			page.Count = len(subs)
		}
	}

	render(w, tmplWebhooks, "admin_shell.html", page)
}

// deliveryMode reports how webhook jobs are persisted, mirroring the queue
// backend: durable when Postgres-backed, in-memory in eval/single-node mode.
func (s *Server) deliveryMode() string {
	if _, ok := s.Queue.(*queue.PG); ok {
		return "durable (Postgres)"
	}
	return "in-memory (eval)"
}
