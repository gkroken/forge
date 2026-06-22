package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"forge/internal/webhook"
)

// handleWebhooks is the admin JSON API for webhook subscriptions.
//
//	GET    /api/v1/webhooks            — list subscriptions
//	POST   /api/v1/webhooks            — create a subscription
//	DELETE /api/v1/webhooks/{id}       — delete a subscription
//	POST   /api/v1/webhooks/{id}/test  — send a test ping to the subscription
//
// Admin-only: subscription URLs are fetched server-side, so registration is a
// trusted operation.
func (s *Server) handleWebhooks(w http.ResponseWriter, r *http.Request) {
	if !s.Enforcer.RequireAdmin(w, r) {
		return
	}
	if s.Webhooks == nil {
		http.Error(w, "webhooks not configured", http.StatusServiceUnavailable)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/v1/webhooks")
	id = strings.TrimPrefix(id, "/")
	if id == "" {
		switch r.Method {
		case http.MethodGet:
			s.listWebhooks(w, r)
		case http.MethodPost:
			s.createWebhook(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}
	// /{id} and /{id}/test
	if sub, rest, found := strings.Cut(id, "/"); found && rest == "test" {
		s.testWebhook(w, r, sub)
		return
	}
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := s.Webhooks.Store().Delete(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listWebhooks(w http.ResponseWriter, _ *http.Request) {
	subs, err := s.Webhooks.Store().List()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Never leak secrets in the listing.
	for i := range subs {
		subs[i].Secret = ""
	}
	writeJSON(w, subs)
}

func (s *Server) createWebhook(w http.ResponseWriter, r *http.Request) {
	var in webhook.Subscription
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	// Clients don't set server-managed fields.
	in.ID = ""
	in.CreatedAt = time.Time{}
	sub, err := s.Webhooks.Store().Create(in)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, sub)
}

func (s *Server) testWebhook(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sub, ok, err := s.Webhooks.Store().Get(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "subscription not found", http.StatusNotFound)
		return
	}
	ev := webhook.Event{
		Type:      webhook.EventArtifactPublished,
		Repo:      sub.Repo,
		Actor:     actorLabel(r, s.Auth),
		Timestamp: time.Now().UTC(),
	}
	if _, derr := s.Webhooks.Deliver(r.Context(), sub, ev, webhook.NewID()); derr != nil {
		writeJSON(w, map[string]any{"ok": false, "error": derr.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}
