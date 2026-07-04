package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"forge/internal/auth"
	"forge/internal/integrity"
	"forge/internal/nexus"
	"forge/internal/queue"
	"forge/internal/repo"
)

// Nexus migration (admin API + async job).
//
// The importer pulls everything from a live Nexus 3 REST API: repositories
// (hosted content, proxy configs, group membership), and optionally security
// (roles → grant-carrying custom roles, users, anonymous read). Content lands
// through forge's own format handlers in-process, so every format-specific
// side effect of a real publish (checksums, packuments, index records)
// happens exactly as it would for a client upload.
//
// Flow: POST /api/v1/migration/plan (synchronous dry-run, persisted) →
// POST /api/v1/migration/apply (enqueues migration.run on the shared worker)
// → GET /api/v1/migration (live progress). After each hosted repo the job
// records source-vs-target counts and enqueues a FULL integrity verify — the
// acceptance loop that proves the migrated store intact.

const (
	migrationJobType    = "migration.run"
	migrationNS         = "admin:migration"
	migrationStaleAfter = 2 * time.Hour
)

// migrationSpec is the persisted connection + scope of the migration.
// The password is stored so the async job and re-runs can use it (same trust
// domain as webhook secrets in the meta store); it is blanked in every API
// response and deleted on reset.
type migrationSpec struct {
	URL             string   `json:"url"`
	Username        string   `json:"username"`
	Password        string   `json:"password,omitempty"`
	IncludeSecurity bool     `json:"includeSecurity"`
	Repos           []string `json:"repos,omitempty"`      // optional subset of source repo names
	PublicBase      string   `json:"publicBase,omitempty"` // forge's external base URL, captured at apply
}

// migrationPlan is the persisted dry-run: what will happen, honestly
// including everything that won't.
type migrationPlan struct {
	SourceURL string           `json:"sourceUrl"`
	CreatedAt time.Time        `json:"createdAt"`
	Repos     []nexus.RepoPlan `json:"repos"`
	Security  *securityPlan    `json:"security,omitempty"`
	Notes     []string         `json:"notes,omitempty"`
}

type securityPlan struct {
	Roles              []roleMigPlan `json:"roles"`
	Users              []userMigPlan `json:"users"`
	AnonymousReadRepos []string      `json:"anonymousReadRepos,omitempty"`
	Notes              []string      `json:"notes,omitempty"`
}

type roleMigPlan struct {
	ID          string            `json:"id"`
	Name        string            `json:"name,omitempty"`
	Description string            `json:"description,omitempty"`
	Action      string            `json:"action"` // create | exists | skip
	Reason      string            `json:"reason,omitempty"`
	BaseRole    string            `json:"baseRole,omitempty"`
	Grants      []auth.Grant      `json:"grants,omitempty"`
	Notes       []nexus.GrantNote `json:"notes,omitempty"`
}

type userMigPlan struct {
	Username    string   `json:"username"`
	DisplayName string   `json:"displayName,omitempty"`
	Role        string   `json:"role,omitempty"`
	SourceRoles []string `json:"sourceRoles,omitempty"`
	Action      string   `json:"action"` // create | exists | skip
	Reason      string   `json:"reason,omitempty"`
}

type assetFailure struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}

// repoMigState is the live per-repository progress document.
type repoMigState struct {
	Repo             string         `json:"repo"`
	Status           string         `json:"status"` // pending | running | complete | failed
	SourceComponents int            `json:"sourceComponents"`
	SourceAssets     int            `json:"sourceAssets"`
	Migrated         int            `json:"migrated"`
	Skipped          int            `json:"skipped"` // already present on target (resume) or generated-by-forge
	Failed           int            `json:"failed"`
	Failures         []assetFailure `json:"failures,omitempty"` // capped
	Note             string         `json:"note,omitempty"`
	StartedAt        time.Time      `json:"startedAt,omitempty"`
	FinishedAt       time.Time      `json:"finishedAt,omitempty"`
	VerifyEnqueued   bool           `json:"verifyEnqueued,omitempty"`
}

const maxStoredFailures = 100

func (st *repoMigState) fail(path string, err error) {
	st.Failed++
	if len(st.Failures) < maxStoredFailures {
		st.Failures = append(st.Failures, assetFailure{Path: path, Error: err.Error()})
	}
}

// migrationRun is the overall run document.
type migrationRun struct {
	Status          string          `json:"status"` // queued | running | complete | failed
	QueuedAt        time.Time       `json:"queuedAt,omitempty"`
	StartedAt       time.Time       `json:"startedAt,omitempty"`
	FinishedAt      time.Time       `json:"finishedAt,omitempty"`
	CurrentRepo     string          `json:"currentRepo,omitempty"`
	Error           string          `json:"error,omitempty"`
	SecurityApplied *securityResult `json:"securityApplied,omitempty"`
}

type securityResult struct {
	RolesCreated     int      `json:"rolesCreated"`
	RolesSkipped     int      `json:"rolesSkipped"`
	UsersCreated     int      `json:"usersCreated"`
	UsersSkipped     int      `json:"usersSkipped"`
	AnonymousReadSet []string `json:"anonymousReadSet,omitempty"`
	Notes            []string `json:"notes,omitempty"`
}

// --- persistence helpers -----------------------------------------------------

func (s *Server) migGetSpec() (migrationSpec, bool) {
	var sp migrationSpec
	ok, _ := s.Meta.GetJSON(migrationNS, "spec", &sp)
	return sp, ok
}

func (s *Server) migGetPlan() (migrationPlan, bool) {
	var p migrationPlan
	ok, _ := s.Meta.GetJSON(migrationNS, "plan", &p)
	return p, ok
}

func (s *Server) migGetRun() (migrationRun, bool) {
	var r migrationRun
	ok, _ := s.Meta.GetJSON(migrationNS, "run", &r)
	return r, ok
}

func (s *Server) migPutRun(r migrationRun) {
	if err := s.Meta.PutJSON(migrationNS, "run", r); err != nil {
		slog.Warn("migration: persist run state failed", "err", err)
	}
}

func (s *Server) migPutState(st *repoMigState) {
	if err := s.Meta.PutJSON(migrationNS, "state:"+st.Repo, *st); err != nil {
		slog.Warn("migration: persist repo state failed", "repo", st.Repo, "err", err)
	}
}

// --- API ----------------------------------------------------------------------

// handleMigration serves /api/v1/migration (global admin):
//
//	GET  ""      → {spec, plan, run, repos:[per-repo state]}
//	POST "plan"  → connect to Nexus, compute + persist the plan, return it
//	POST "apply" → enqueue the migration.run job
//	POST "reset" → drop all migration bookkeeping (content stays)
func (s *Server) handleMigration(w http.ResponseWriter, r *http.Request) {
	if !s.Enforcer.RequireAdmin(w, r) {
		return
	}
	sub := strings.TrimPrefix(r.URL.Path, "/api/v1/migration")
	sub = strings.Trim(sub, "/")
	switch {
	case r.Method == http.MethodGet && sub == "":
		s.migrationStatus(w)
	case r.Method == http.MethodPost && sub == "plan":
		s.migrationPlanAPI(w, r)
	case r.Method == http.MethodPost && sub == "apply":
		s.migrationApplyAPI(w, r)
	case r.Method == http.MethodPost && sub == "reset":
		s.migrationResetAPI(w)
	default:
		jsonError(w, "not found", http.StatusNotFound)
	}
}

type migrationStatusResponse struct {
	Spec  *migrationSpec `json:"spec,omitempty"` // password blanked
	Plan  *migrationPlan `json:"plan,omitempty"`
	Run   *migrationRun  `json:"run,omitempty"`
	Repos []repoMigState `json:"repos"`
}

func (s *Server) migrationStatus(w http.ResponseWriter) {
	resp := migrationStatusResponse{Repos: []repoMigState{}}
	if sp, ok := s.migGetSpec(); ok {
		sp.Password = ""
		resp.Spec = &sp
	}
	if p, ok := s.migGetPlan(); ok {
		resp.Plan = &p
	}
	if run, ok := s.migGetRun(); ok {
		resp.Run = &run
	}
	keys, _ := s.Meta.List(migrationNS)
	for _, k := range keys {
		if !strings.HasPrefix(k, "state:") {
			continue
		}
		var st repoMigState
		if ok, _ := s.Meta.GetJSON(migrationNS, k, &st); ok {
			resp.Repos = append(resp.Repos, st)
		}
	}
	sort.Slice(resp.Repos, func(i, j int) bool { return resp.Repos[i].Repo < resp.Repos[j].Repo })
	writeJSON(w, resp)
}

type migrationPlanRequest struct {
	URL             string   `json:"url"`
	Username        string   `json:"username"`
	Password        string   `json:"password"`
	IncludeSecurity bool     `json:"includeSecurity"`
	Repos           []string `json:"repos,omitempty"`
}

func (s *Server) migrationPlanAPI(w http.ResponseWriter, r *http.Request) {
	var req migrationPlanRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.URL == "" {
		jsonError(w, "url is required", http.StatusBadRequest)
		return
	}
	if run, ok := s.migGetRun(); ok && run.Status == "running" &&
		time.Since(run.StartedAt) < migrationStaleAfter {
		jsonError(w, "a migration run is in progress; wait for it or reset", http.StatusConflict)
		return
	}

	client := nexus.New(req.URL, req.Username, req.Password)
	ctx := r.Context()
	if err := client.Ping(ctx); err != nil {
		jsonError(w, "cannot reach Nexus: "+err.Error(), http.StatusBadGateway)
		return
	}

	plan, err := s.buildMigrationPlan(ctx, client, req)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}

	spec := migrationSpec{
		URL: strings.TrimRight(req.URL, "/"), Username: req.Username, Password: req.Password,
		IncludeSecurity: req.IncludeSecurity, Repos: req.Repos,
	}
	if err := s.Meta.PutJSON(migrationNS, "spec", spec); err != nil {
		jsonError(w, "persist spec: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.Meta.PutJSON(migrationNS, "plan", plan); err != nil {
		jsonError(w, "persist plan: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, plan)
}

// buildMigrationPlan computes the full dry-run against a reachable source.
func (s *Server) buildMigrationPlan(ctx context.Context, client *nexus.Client, req migrationPlanRequest) (migrationPlan, error) {
	srcRepos, err := client.ListRepositories(ctx)
	if err != nil {
		return migrationPlan{}, fmt.Errorf("list source repositories: %w", err)
	}

	existing := map[string]repo.Repository{}
	for _, rp := range s.Repos.All() {
		existing[rp.Name] = rp
	}

	subset := map[string]bool{}
	for _, name := range req.Repos {
		subset[name] = true
	}

	plan := migrationPlan{
		SourceURL: strings.TrimRight(req.URL, "/"),
		CreatedAt: time.Now().UTC(),
		Notes: []string{
			"proxy repositories migrate configuration only — caches are self-healing and refill from upstream on demand",
			"Nexus cleanup policies, scheduled tasks, routing rules, webhooks, LDAP config and blob-store layout are not migrated",
		},
	}

	for _, sr := range srcRepos {
		if len(subset) > 0 && !subset[sr.Name] {
			continue
		}
		rp := nexus.MapRepo(sr, existing)
		if rp.Migratable() && rp.TargetKind == repo.Hosted {
			nComp, nAssets, err := client.CountComponents(ctx, sr.Name)
			if err != nil {
				return migrationPlan{}, fmt.Errorf("count components in %s: %w", sr.Name, err)
			}
			rp.Components, rp.Assets = nComp, nAssets
		}
		plan.Repos = append(plan.Repos, rp)
	}
	// Hosted and proxy repos first, groups last: a group's members must exist
	// before the group is created.
	sort.SliceStable(plan.Repos, func(i, j int) bool {
		gi, gj := plan.Repos[i].TargetKind == repo.Group, plan.Repos[j].TargetKind == repo.Group
		if gi != gj {
			return !gi
		}
		return plan.Repos[i].Source < plan.Repos[j].Source
	})

	if req.IncludeSecurity {
		sp, err := s.buildSecurityPlan(ctx, client, plan.Repos)
		if err != nil {
			return migrationPlan{}, err
		}
		plan.Security = sp
	}
	return plan, nil
}

func (s *Server) migrationApplyAPI(w http.ResponseWriter, r *http.Request) {
	if s.Queue == nil {
		jsonError(w, "async worker not configured", http.StatusServiceUnavailable)
		return
	}
	spec, ok := s.migGetSpec()
	if !ok {
		jsonError(w, "no migration plan — POST /api/v1/migration/plan first", http.StatusConflict)
		return
	}
	if _, ok := s.migGetPlan(); !ok {
		jsonError(w, "no migration plan — POST /api/v1/migration/plan first", http.StatusConflict)
		return
	}
	if run, found := s.migGetRun(); found &&
		(run.Status == "queued" || run.Status == "running") {
		ref := run.QueuedAt
		if run.StartedAt.After(ref) {
			ref = run.StartedAt
		}
		if time.Since(ref) < migrationStaleAfter {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			json.NewEncoder(w).Encode(map[string]string{"status": run.Status}) //nolint:errcheck
			return
		}
		// Stale queued/running marker — presume the worker died and re-enqueue.
	}

	// Capture forge's externally visible base URL: npm publishes bake the
	// tarball URL host into the stored version records.
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	spec.PublicBase = scheme + "://" + r.Host
	if err := s.Meta.PutJSON(migrationNS, "spec", spec); err != nil {
		jsonError(w, "persist spec: "+err.Error(), http.StatusInternalServerError)
		return
	}

	s.migPutRun(migrationRun{Status: "queued", QueuedAt: time.Now().UTC()})
	if err := s.Queue.Enqueue(r.Context(), migrationJobType, struct{}{}); err != nil {
		jsonError(w, "enqueue failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"status": "migration enqueued"}) //nolint:errcheck
}

func (s *Server) migrationResetAPI(w http.ResponseWriter) {
	if run, ok := s.migGetRun(); ok && run.Status == "running" &&
		time.Since(run.StartedAt) < migrationStaleAfter {
		jsonError(w, "a migration run is in progress", http.StatusConflict)
		return
	}
	keys, _ := s.Meta.List(migrationNS)
	for _, k := range keys {
		s.Meta.Delete(migrationNS, k) //nolint:errcheck
	}
	writeJSON(w, map[string]string{"status": "reset"})
}

// --- job -----------------------------------------------------------------------

// handleMigrationJob is the worker handler for migration.run. Always returns
// nil: the outcome lives in the run/state documents, and a queue retry would
// restart a multi-hour import for no reason (re-apply is the retry).
func (s *Server) handleMigrationJob(ctx context.Context, _ queue.Job) error {
	s.runMigration(ctx)
	return nil
}

func (s *Server) runMigration(ctx context.Context) {
	spec, okS := s.migGetSpec()
	plan, okP := s.migGetPlan()
	if !okS || !okP {
		slog.Warn("migration: job fired without spec/plan")
		return
	}
	prev, _ := s.migGetRun()
	run := migrationRun{Status: "running", QueuedAt: prev.QueuedAt, StartedAt: time.Now().UTC()}
	s.migPutRun(run)

	client := nexus.New(spec.URL, spec.Username, spec.Password)

	for i := range plan.Repos {
		rp := plan.Repos[i]
		if !rp.Migratable() {
			continue
		}
		if ctx.Err() != nil {
			run.Status, run.Error = "failed", "cancelled: "+ctx.Err().Error()
			run.FinishedAt = time.Now().UTC()
			s.migPutRun(run)
			return
		}
		run.CurrentRepo = rp.Source
		s.migPutRun(run)
		s.migrateOneRepo(ctx, client, spec, rp)
	}
	run.CurrentRepo = ""

	if plan.Security != nil {
		res := s.applySecurityPlan(plan.Security)
		run.SecurityApplied = &res
	}

	run.Status = "complete"
	run.FinishedAt = time.Now().UTC()
	s.migPutRun(run)
	slog.Info("migration: run complete", "source", spec.URL)
}

// migrateOneRepo ensures the target repo exists, transfers content for hosted
// repos, then records counts and enqueues the full integrity verify.
func (s *Server) migrateOneRepo(ctx context.Context, client *nexus.Client, spec migrationSpec, rp nexus.RepoPlan) {
	st := &repoMigState{Repo: rp.Target, Status: "running", StartedAt: time.Now().UTC()}
	s.migPutState(st)

	if _, ok := s.Repos.Get(rp.Target); !ok {
		if err := s.Repos.Add(rp.ToRepository()); err != nil {
			st.Status = "failed"
			st.Note = "create repository: " + err.Error()
			st.FinishedAt = time.Now().UTC()
			s.migPutState(st)
			return
		}
	}

	switch rp.TargetKind {
	case repo.Hosted:
		s.migrateRepoContent(ctx, client, spec, rp, st)
	case repo.Proxy:
		st.Note = "proxy configuration migrated; the cache refills from upstream on demand"
	case repo.Group:
		st.Note = "group membership migrated; groups own no content"
	}

	if st.Failed > 0 {
		st.Status = "failed"
	} else {
		st.Status = "complete"
	}
	st.FinishedAt = time.Now().UTC()

	// Acceptance loop: a FULL integrity verify per migrated hosted repo. It
	// re-hashes every stored checksum expectation over the transferred bytes.
	if rp.TargetKind == repo.Hosted && s.Queue != nil {
		istore := integrity.NewStore(s.Meta)
		if err := istore.Put(integrity.Report{
			Repo: rp.Target, Status: integrity.StatusQueued, Mode: integrity.ModeFull,
			QueuedAt: time.Now().UTC(),
			Findings: []integrity.Finding{}, Counts: map[string]int{},
		}); err == nil {
			if err := s.Queue.Enqueue(ctx, integrityJobType, integrityPayload{Repo: rp.Target, Mode: string(integrity.ModeFull)}); err == nil {
				st.VerifyEnqueued = true
			}
		}
	}
	s.migPutState(st)
	slog.Info("migration: repo done", "repo", rp.Target, "status", st.Status,
		"migrated", st.Migrated, "skipped", st.Skipped, "failed", st.Failed,
		"sourceAssets", st.SourceAssets)
}

// --- security ------------------------------------------------------------------

// builtinRoles are Nexus roles with forge-native equivalents; importing them
// would duplicate built-in behaviour.
var builtinNexusRoles = map[string]string{
	"nx-admin":     "use forge's built-in Administrator role",
	"nx-anonymous": "anonymous access maps to per-repository anonymous read",
}

func (s *Server) buildSecurityPlan(ctx context.Context, client *nexus.Client, repoPlans []nexus.RepoPlan) (*securityPlan, error) {
	users, err := client.ListUsers(ctx)
	if err != nil {
		return nil, fmt.Errorf("list source users: %w", err)
	}
	roles, err := client.ListRoles(ctx)
	if err != nil {
		return nil, fmt.Errorf("list source roles: %w", err)
	}
	privs, err := client.ListPrivileges(ctx)
	if err != nil {
		return nil, fmt.Errorf("list source privileges: %w", err)
	}
	sels, err := client.ListContentSelectors(ctx)
	if err != nil {
		return nil, fmt.Errorf("list source content selectors: %w", err)
	}
	anon, err := client.AnonymousAccess(ctx)
	if err != nil {
		return nil, fmt.Errorf("read source anonymous access: %w", err)
	}

	privByName := map[string]nexus.Privilege{}
	for _, p := range privs {
		privByName[p.Name] = p
	}
	selByName := map[string]nexus.ContentSelector{}
	for _, cs := range sels {
		selByName[cs.Name] = cs
	}
	roleByID := map[string]nexus.Role{}
	for _, r := range roles {
		roleByID[r.ID] = r
	}
	migratedByFormat := map[string][]string{}
	var allMigrated []string
	for _, rp := range repoPlans {
		if rp.Migratable() {
			migratedByFormat[rp.TargetFormat] = append(migratedByFormat[rp.TargetFormat], rp.Target)
			allMigrated = append(allMigrated, rp.Target)
		}
	}
	reposOf := func(f string) []string { return migratedByFormat[f] }

	sp := &securityPlan{Notes: []string{
		"imported users are created DISABLED and without a password — Nexus never exposes password hashes; enable each account and set a password (PUT /api/v1/users/{name}), or rely on LDAP/OIDC login",
		"application-level Nexus privileges (settings, tasks, script execution) have no forge equivalent and are listed per role below",
	}}

	// roleGrants caches translated grants per source role for user mapping.
	roleGrants := map[string][]auth.Grant{}
	rolePlanned := map[string]bool{}

	for _, r := range roles {
		if reason, isBuiltin := builtinNexusRoles[r.ID]; isBuiltin {
			sp.Roles = append(sp.Roles, roleMigPlan{ID: r.ID, Name: r.Name, Action: "skip", Reason: reason})
			continue
		}
		grants, notes := nexus.BuildGrants(nexus.FlattenRole(r.ID, roleByID), privByName, selByName, reposOf)
		rp := roleMigPlan{
			ID: r.ID, Name: r.Name, Description: r.Description,
			Grants: grants, Notes: notes, BaseRole: nexus.BaseTierFor(grants),
		}
		switch {
		case len(grants) == 0:
			rp.Action = "skip"
			rp.Reason = "no privilege in this role maps to a forge grant"
		default:
			rp.Action = "create"
			if s.Roles != nil {
				if _, ok, _ := s.Roles.Get(r.ID); ok {
					rp.Action = "exists"
				}
			}
			roleGrants[r.ID] = grants
			rolePlanned[r.ID] = true
		}
		sp.Roles = append(sp.Roles, rp)
	}

	for _, u := range users {
		up := userMigPlan{Username: u.UserID, SourceRoles: u.Roles,
			DisplayName: strings.TrimSpace(u.FirstName + " " + u.LastName)}
		switch {
		case u.UserID == "anonymous":
			up.Action, up.Reason = "skip", "anonymous access maps to per-repository anonymous read"
		case u.UserID == "admin":
			up.Action, up.Reason = "skip", "map the source admin to your existing forge administrator account"
		default:
			up.Action = "create"
			if s.Users != nil {
				if _, ok, _ := s.Users.Get(u.UserID); ok {
					up.Action, up.Reason = "exists", "forge already has this user; role and status left untouched"
				}
			}
			up.Role = s.pickUserRole(u, roleGrants, rolePlanned, sp)
			if up.Role == "" && up.Action == "create" {
				up.Action = "skip"
				up.Reason = "none of the user's source roles map to forge grants"
			}
		}
		sp.Users = append(sp.Users, up)
	}

	// Anonymous read: the source's anonymous role expands to repos with a
	// read grant.
	if anon.Enabled {
		grants, _ := nexus.BuildGrants(nexus.FlattenRole("nx-anonymous", roleByID), privByName, selByName, reposOf)
		anonRepos := map[string]bool{}
		for _, g := range grants {
			hasRead := false
			for _, a := range g.Actions {
				if a == auth.ActionRead {
					hasRead = true
				}
			}
			if !hasRead {
				continue
			}
			if g.Repo == "*" {
				for _, name := range allMigrated {
					anonRepos[name] = true
				}
			} else {
				anonRepos[g.Repo] = true
			}
		}
		for name := range anonRepos {
			sp.AnonymousReadRepos = append(sp.AnonymousReadRepos, name)
		}
		sort.Strings(sp.AnonymousReadRepos)
	}
	return sp, nil
}

// pickUserRole maps a Nexus user's role list onto one forge role name.
// nx-admin anywhere wins Administrator; a single migratable role is used
// directly; several are combined into a synthetic union role appended to the
// plan. Returns "" when nothing maps.
func (s *Server) pickUserRole(u nexus.User, roleGrants map[string][]auth.Grant, rolePlanned map[string]bool, sp *securityPlan) string {
	var mapped []string
	for _, rid := range u.Roles {
		if rid == "nx-admin" {
			return "Administrator"
		}
		if rolePlanned[rid] {
			mapped = append(mapped, rid)
		}
	}
	switch len(mapped) {
	case 0:
		return ""
	case 1:
		return mapped[0]
	}
	// Union role for multi-role users; forge users carry exactly one role.
	name := u.UserID + "-roles"
	var privUnion []auth.Grant
	for _, rid := range mapped {
		privUnion = append(privUnion, roleGrants[rid]...)
	}
	union := mergeGrants(privUnion)
	rp := roleMigPlan{
		ID: name, Name: name,
		Description: "union of Nexus roles " + strings.Join(mapped, ", ") + " for user " + u.UserID,
		Grants:      union, BaseRole: nexus.BaseTierFor(union), Action: "create",
	}
	if s.Roles != nil {
		if _, ok, _ := s.Roles.Get(name); ok {
			rp.Action = "exists"
		}
	}
	sp.Roles = append(sp.Roles, rp)
	return name
}

// mergeGrants unions grants with identical (repo, selectors) coordinates.
func mergeGrants(in []auth.Grant) []auth.Grant {
	type key struct{ repo, sels string }
	acc := map[key]map[auth.Action]bool{}
	for _, g := range in {
		k := key{g.Repo, strings.Join(g.Selectors, "\x00")}
		if acc[k] == nil {
			acc[k] = map[auth.Action]bool{}
		}
		for _, a := range g.Actions {
			acc[k][a] = true
		}
	}
	keys := make([]key, 0, len(acc))
	for k := range acc {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].repo != keys[j].repo {
			return keys[i].repo < keys[j].repo
		}
		return keys[i].sels < keys[j].sels
	})
	var out []auth.Grant
	for _, k := range keys {
		var actions []auth.Action
		for _, a := range auth.AllActions {
			if acc[k][a] {
				actions = append(actions, a)
			}
		}
		g := auth.Grant{Repo: k.repo, Actions: actions}
		if k.sels != "" {
			g.Selectors = strings.Split(k.sels, "\x00")
		}
		out = append(out, g)
	}
	return out
}

// applySecurityPlan executes the security half: roles first (users reference
// them), then users (disabled, no password), then anonymous read flags.
func (s *Server) applySecurityPlan(sp *securityPlan) securityResult {
	var res securityResult
	if s.Roles == nil || s.Users == nil {
		res.Notes = append(res.Notes, "user/role stores are not configured (eval mode) — security plan not applied")
		return res
	}
	for _, r := range sp.Roles {
		if r.Action != "create" {
			res.RolesSkipped++
			continue
		}
		desc := r.Description
		if desc == "" {
			desc = "imported from Nexus"
		} else {
			desc += " (imported from Nexus)"
		}
		err := s.Roles.Create(auth.CustomRole{
			Name: r.ID, Description: desc, BaseRole: r.BaseRole, Grants: r.Grants,
		})
		if err != nil {
			res.Notes = append(res.Notes, "role "+r.ID+": "+err.Error())
			res.RolesSkipped++
			continue
		}
		res.RolesCreated++
	}
	for _, u := range sp.Users {
		if u.Action != "create" {
			res.UsersSkipped++
			continue
		}
		err := s.Users.Upsert(auth.User{
			Username: u.Username, DisplayName: u.DisplayName, Role: u.Role,
			Disabled: true, CreatedAt: time.Now().UTC(),
		})
		if err != nil {
			res.Notes = append(res.Notes, "user "+u.Username+": "+err.Error())
			res.UsersSkipped++
			continue
		}
		res.UsersCreated++
	}
	for _, name := range sp.AnonymousReadRepos {
		rp, ok := s.Repos.Get(name)
		if !ok || rp.AnonymousRead {
			continue
		}
		rp.AnonymousRead = true
		if err := s.Repos.Update(rp); err != nil {
			res.Notes = append(res.Notes, "anonymous read on "+name+": "+err.Error())
			continue
		}
		res.AnonymousReadSet = append(res.AnonymousReadSet, name)
	}
	return res
}
