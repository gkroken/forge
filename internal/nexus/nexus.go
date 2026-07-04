// Package nexus is a read-only client for the Sonatype Nexus Repository 3
// REST API, used by the migration importer. It speaks the /service/rest/v1
// endpoints with basic auth and continuation-token pagination.
//
// The client is deliberately read-only: migration never mutates the source
// Nexus instance. Importing from an on-disk Nexus blobstore is out of scope —
// the blobstore layout is internal to Nexus (its metadata database is needed
// to interpret it), while the REST API works against any deployment,
// including S3-backed ones, and reports per-asset checksums that double as
// transfer verification.
package nexus

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client talks to one Nexus 3 server.
type Client struct {
	BaseURL  string // e.g. "http://nexus.example:8081", no trailing slash
	Username string
	Password string
	HTTP     *http.Client
}

// New returns a Client for the given base URL and credentials.
func New(baseURL, username, password string) *Client {
	return &Client{
		BaseURL:  strings.TrimRight(baseURL, "/"),
		Username: username,
		Password: password,
		HTTP:     &http.Client{Timeout: 5 * time.Minute},
	}
}

// Repository is one repository as reported by /service/rest/v1/repositorySettings
// (preferred; includes full attributes) or /v1/repositories (fallback).
type Repository struct {
	Name   string `json:"name"`
	Format string `json:"format"` // maven2, npm, helm, r, docker, pypi, ...
	Type   string `json:"type"`   // hosted, proxy, group
	URL    string `json:"url"`

	// The two listing endpoints disagree on shape: /v1/repositorySettings
	// returns typed per-kind blocks at the TOP level (proxy.remoteUrl,
	// group.memberNames), while /v1/repositories nests the same data under
	// "attributes" (admin only). Both are parsed; normalize() lifts whichever
	// is present into RemoteURL/Members.
	ProxyBlock *struct {
		RemoteURL string `json:"remoteUrl"`
	} `json:"proxy,omitempty"`
	GroupBlock *struct {
		MemberNames []string `json:"memberNames"`
	} `json:"group,omitempty"`
	Attributes map[string]json.RawMessage `json:"attributes,omitempty"`

	RemoteURL string   `json:"-"`
	Members   []string `json:"-"`
}

// Component is one component (GAV, package, chart, image) with its assets.
type Component struct {
	ID      string  `json:"id"`
	Repo    string  `json:"repository"`
	Format  string  `json:"format"`
	Group   string  `json:"group"`
	Name    string  `json:"name"`
	Version string  `json:"version"`
	Assets  []Asset `json:"assets"`
}

// Asset is one stored file belonging to a component.
type Asset struct {
	ID          string            `json:"id"`
	Path        string            `json:"path"`
	DownloadURL string            `json:"downloadUrl"`
	ContentType string            `json:"contentType"`
	FileSize    int64             `json:"fileSize"`
	Checksum    map[string]string `json:"checksum"` // sha1, sha256, md5 (as available)
}

// User is a Nexus security user (source=default only is migrated).
type User struct {
	UserID    string   `json:"userId"`
	FirstName string   `json:"firstName"`
	LastName  string   `json:"lastName"`
	Email     string   `json:"emailAddress"`
	Source    string   `json:"source"`
	Status    string   `json:"status"` // active, disabled
	Roles     []string `json:"roles"`
}

// Role is a Nexus security role: a named bundle of privileges and nested roles.
type Role struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Privileges  []string `json:"privileges"`
	Roles       []string `json:"roles"` // nested role IDs
}

// Privilege is one Nexus privilege definition.
type Privilege struct {
	Name            string   `json:"name"`
	Type            string   `json:"type"` // repository-view, repository-admin, repository-content-selector, application, wildcard, script
	Format          string   `json:"format,omitempty"`
	Repository      string   `json:"repository,omitempty"`
	Actions         []string `json:"actions,omitempty"` // BROWSE, READ, EDIT, ADD, DELETE, RUN, ASSOCIATE, DISASSOCIATE, ALL/*
	ContentSelector string   `json:"contentSelector,omitempty"`
}

// ContentSelector is a Nexus content selector (a CSEL/JEXL expression).
type ContentSelector struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Expression  string `json:"expression"`
}

// AnonymousStatus reports whether anonymous access is enabled on the source.
type AnonymousStatus struct {
	Enabled bool   `json:"enabled"`
	UserID  string `json:"userId"` // usually "anonymous"
}

// --- HTTP plumbing -----------------------------------------------------------

func (c *Client) get(ctx context.Context, path string, query url.Values, out any) error {
	u := c.BaseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.Username, c.Password)
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("nexus: GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &APIError{Status: resp.StatusCode, Path: path, Body: strings.TrimSpace(string(body))}
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// APIError is a non-200 response from the Nexus API.
type APIError struct {
	Status int
	Path   string
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("nexus: GET %s: status %d: %s", e.Path, e.Status, e.Body)
}

// IsStatus reports whether err is an APIError with the given status code.
func IsStatus(err error, status int) bool {
	if ae, ok := err.(*APIError); ok {
		return ae.Status == status
	}
	return false
}

// Ping verifies connectivity and credentials (any authenticated endpoint works;
// the repositories listing is readable by any user that can browse).
func (c *Client) Ping(ctx context.Context) error {
	var out []Repository
	return c.get(ctx, "/service/rest/v1/repositories", nil, &out)
}

// ListRepositories returns all repositories with their settings. It prefers
// /v1/repositorySettings (admin-only, includes proxy remoteUrl and group
// members) and falls back to /v1/repositories when the caller lacks that
// permission or the endpoint predates the Nexus version.
func (c *Client) ListRepositories(ctx context.Context) ([]Repository, error) {
	var repos []Repository
	err := c.get(ctx, "/service/rest/v1/repositorySettings", nil, &repos)
	if err != nil {
		if !IsStatus(err, http.StatusForbidden) && !IsStatus(err, http.StatusNotFound) {
			return nil, err
		}
		if err2 := c.get(ctx, "/service/rest/v1/repositories", nil, &repos); err2 != nil {
			return nil, err2
		}
	}
	for i := range repos {
		repos[i].normalize()
	}
	return repos, nil
}

// normalize lifts the per-kind settings blocks into typed fields, whichever
// endpoint shape they arrived in.
func (r *Repository) normalize() {
	if r.ProxyBlock != nil {
		r.RemoteURL = r.ProxyBlock.RemoteURL
	}
	if r.GroupBlock != nil {
		r.Members = r.GroupBlock.MemberNames
	}
	if r.RemoteURL == "" {
		if raw, ok := r.Attributes["proxy"]; ok {
			var p struct {
				RemoteURL string `json:"remoteUrl"`
			}
			if json.Unmarshal(raw, &p) == nil {
				r.RemoteURL = p.RemoteURL
			}
		}
	}
	if len(r.Members) == 0 {
		if raw, ok := r.Attributes["group"]; ok {
			var g struct {
				MemberNames []string `json:"memberNames"`
			}
			if json.Unmarshal(raw, &g) == nil {
				r.Members = g.MemberNames
			}
		}
	}
}

// ListComponents pages through every component in the named repository,
// calling fn for each. It follows continuationToken until exhausted.
func (c *Client) ListComponents(ctx context.Context, repoName string, fn func(Component) error) error {
	token := ""
	for {
		q := url.Values{"repository": {repoName}}
		if token != "" {
			q.Set("continuationToken", token)
		}
		var page struct {
			Items             []Component `json:"items"`
			ContinuationToken string      `json:"continuationToken"`
		}
		if err := c.get(ctx, "/service/rest/v1/components", q, &page); err != nil {
			return err
		}
		for _, comp := range page.Items {
			if err := fn(comp); err != nil {
				return err
			}
		}
		if page.ContinuationToken == "" {
			return nil
		}
		token = page.ContinuationToken
	}
}

// CountComponents returns the number of components and assets in a repository.
func (c *Client) CountComponents(ctx context.Context, repoName string) (components, assets int, err error) {
	err = c.ListComponents(ctx, repoName, func(comp Component) error {
		components++
		assets += len(comp.Assets)
		return nil
	})
	return
}

// Download fetches one asset by its downloadUrl, streaming the body to the
// caller. The caller must Close the returned ReadCloser.
func (c *Client) Download(ctx context.Context, downloadURL string, accept ...string) (io.ReadCloser, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return nil, nil, err
	}
	req.SetBasicAuth(c.Username, c.Password)
	for _, a := range accept {
		req.Header.Add("Accept", a)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("nexus: download %s: %w", downloadURL, err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		return nil, nil, &APIError{Status: resp.StatusCode, Path: downloadURL, Body: strings.TrimSpace(string(body))}
	}
	return resp.Body, resp.Header, nil
}

// --- security listings -------------------------------------------------------

// ListUsers returns local (source=default) users.
func (c *Client) ListUsers(ctx context.Context) ([]User, error) {
	var users []User
	q := url.Values{"source": {"default"}}
	if err := c.get(ctx, "/service/rest/v1/security/users", q, &users); err != nil {
		return nil, err
	}
	return users, nil
}

// ListRoles returns all role definitions (source=default).
func (c *Client) ListRoles(ctx context.Context) ([]Role, error) {
	var roles []Role
	q := url.Values{"source": {"default"}}
	if err := c.get(ctx, "/service/rest/v1/security/roles", q, &roles); err != nil {
		return nil, err
	}
	return roles, nil
}

// ListPrivileges returns all privilege definitions.
func (c *Client) ListPrivileges(ctx context.Context) ([]Privilege, error) {
	var privs []Privilege
	if err := c.get(ctx, "/service/rest/v1/security/privileges", nil, &privs); err != nil {
		return nil, err
	}
	return privs, nil
}

// ListContentSelectors returns all content selectors.
func (c *Client) ListContentSelectors(ctx context.Context) ([]ContentSelector, error) {
	var sels []ContentSelector
	if err := c.get(ctx, "/service/rest/v1/security/content-selectors", nil, &sels); err != nil {
		return nil, err
	}
	return sels, nil
}

// AnonymousAccess reports the source's anonymous-access setting. A 403 (the
// migration account lacks the settings privilege) is reported as disabled
// with no error so a content-only migration can proceed.
func (c *Client) AnonymousAccess(ctx context.Context) (AnonymousStatus, error) {
	var st AnonymousStatus
	err := c.get(ctx, "/service/rest/v1/security/anonymous", nil, &st)
	if err != nil && IsStatus(err, http.StatusForbidden) {
		return AnonymousStatus{}, nil
	}
	return st, err
}
