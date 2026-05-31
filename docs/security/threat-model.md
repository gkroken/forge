# forge — Threat Model

Version: 1.0  
Date: 2026-05-31  
Phase: P7 (Security & GA hardening)

---

## 1. Architecture & trust boundaries

```
                    ┌───────────────────────────────────────┐
  Internet          │  forge process                        │
  ──────────────    │                                       │
  Anonymous         │  HTTP mux                            │
  client  ─────────►│  /healthz /readyz /v2/ (no auth)     │
                    │                                       │
  Auth'd  ─────────►│  /repository/ /v2/{repo}/ (RBAC)     │
  client            │  auth.Enforcer ──► auth.Store        │
  (Bearer token)    │                      │               │
                    │  /api/v1/       ──► RequireAdmin      │
  Admin    ─────────►│  (admin token)       │               │
  client            │                      │               │
                    └──────────┬───────────┴───────────────┘
                               │                │
              ┌────────────────┘                └────────────────┐
              ▼                                                   ▼
      ┌───────────────┐                              ┌──────────────────┐
      │  Postgres      │                              │  S3 / MinIO       │
      │  meta.Store    │                              │  blob.Store       │
      │  tokens, repos │                              │  artifact bytes   │
      │  packuments    │                              │                  │
      └───────────────┘                              └──────────────────┘
                                        ▲
                              Proxy repos│
                          ┌─────────────┘
                          ▼
                  Upstream registries
                  (npmjs.org, Maven Central,
                   CRAN, gcr.io, ...)
```

### Trust boundaries

| Boundary | Trust | Notes |
|----------|-------|-------|
| Anonymous client → forge | Untrusted | No credentials; limited to public endpoints |
| Authenticated client → forge | Partially trusted | Token verified before every request; role limits actions |
| Admin client → forge | Trusted | Admin role; can create repos and tokens |
| forge → Postgres/S3 | Trusted | Network-level isolation; TLS + service account credentials |
| forge → upstream registries | Untrusted external | Responses validated (checksums); negative caching limits blast radius |
| CI/CD → GHCR | Trusted | Keyless cosign signing; OIDC token exchange |

---

## 2. STRIDE analysis

### 2.1 Spoofing — token forgery / identity impersonation

| Threat | Control | Status |
|--------|---------|--------|
| Attacker forges a valid token without the secret | Tokens are `forge_` + 32 random bytes (256-bit entropy); verified by SHA-256 hash; brute-force infeasible | ✅ Mitigated |
| Attacker replays a revoked token | Revoke deletes the hash from the store; any subsequent Verify returns nil | ✅ Mitigated |
| Attacker replays an expired token | Verify checks `ExpiresAt` before returning the token | ✅ Mitigated |
| Attacker submits a malformed secret to cause a hash collision | `hashDisplay` returns `""` for malformed input; `""` never matches a stored hash | ✅ Mitigated |
| npm Basic auth confusion (empty username) | `bearerToken()` explicitly handles `Authorization: Basic :<token>` for npm compatibility; tested in `TestAuthzMatrix_BearerFormats` | ✅ Mitigated |

**Residual:** Token entropy is 256 bits — adequate. No risk of birthday attacks on the SHA-256 hash at realistic token counts.

---

### 2.2 Tampering — unauthorised data modification

| Threat | Control | Status |
|--------|---------|--------|
| Read-only token issues a PUT/POST/DELETE | `actionFor(method)` maps methods to `ActionRead`/`ActionWrite`; write role required | ✅ Mitigated |
| Attacker writes to a repo they have no token for | `decide()` checks `RoleFor(repoName)`; cross-repo grants not transferable | ✅ Mitigated |
| Attacker modifies an artifact in S3 directly | Out of scope (infrastructure); S3 versioning + IAM recommended in production | ⚠️ Deployment control |
| Artifact checksum bypass | blob.FS computes SHA-256/SHA1/MD5 on write; Maven sidecar responses are derived from stored checksums | ✅ Mitigated |
| Meta key injection via crafted ns (path traversal) | `meta.FS.resolve()` rejects any ns that produces a path outside root; tested with `TestFS_Traversal` + fuzzer | ✅ Mitigated |
| Blob key path traversal | `blob.FS.resolve()` prepends `/` then `filepath.Clean`; any `..` is neutralised; tested with `TestFS_TraversalContained` + fuzzer | ✅ Mitigated |

---

### 2.3 Repudiation — denial of actions

| Threat | Control | Status |
|--------|---------|--------|
| Writer denies publishing an artifact | Audit log emits `audit=true` slog entry with method, path, status for every successful PUT/POST/DELETE on `/repository/` and `/v2/` | ✅ Mitigated |
| Admin denies creating/revoking a token | `token.create` and `token.revoke` emit audit entries with `token_id` | ✅ Mitigated |
| Auth failure goes unrecorded | 401 and 403 responses emit audit entries | ✅ Mitigated |
| Audit log is writable by the app process | Log goes to stdout (structured slog); ship to an append-only SIEM in production | ⚠️ Deployment control |

---

### 2.4 Information disclosure

| Threat | Control | Status |
|--------|---------|--------|
| Unauthenticated enumeration of private artifact names | Unknown repo → `decisionAllow` (handler returns 404, not 401); prevents existence oracle for repo names | ✅ By design |
| Token secret exposed in logs | `slog.Info` for token creation logs `token_id` only; the raw secret is never stored or logged | ✅ Mitigated |
| Bootstrap admin token logged in plaintext | `slog.Info("…secret=…")` on first start — this is intentional and documented; operators must capture stderr at boot | ⚠️ Operator responsibility |
| Stack traces in HTTP error responses | Go's `net/http` does not add stack traces to HTTP responses; forge uses `http.Error` / `jsonError` with controlled messages | ✅ Mitigated |
| Prometheus metrics expose internal state | `/metrics` is served without auth (same as Nexus/Artifactory defaults); can be restricted by network policy in production | ⚠️ Deployment control |
| Packument contains upstream tarball URLs rewritten to forge | npm proxy rewrites tarball URLs to point back at forge; the upstream URL is not exposed to clients | ✅ Mitigated |

---

### 2.5 Elevation of privilege

| Threat | Control | Status |
|--------|---------|--------|
| Read token gains write access | `decide()` enforces `role >= RoleWrite` for write actions; read role is strictly below write | ✅ Mitigated |
| Write token gains admin access | Admin role is checked separately via `RequireAdmin()`; `RoleFor("*")` must return `>= RoleAdmin` | ✅ Mitigated |
| Token with no grants gains any access | `RoleFor()` returns `RoleNone` (0) when no grant matches; any role check `>= RoleRead` fails | ✅ Mitigated |
| Wildcard grant (`Repo: "*"`) created by non-admin | Token creation requires admin role after the first token exists | ✅ Mitigated |
| **Bootstrap token window** (⚠️ residual) | `createToken` is unauthenticated when `Count() == 0`. An attacker who can reach port 8080 before the admin can mint an admin token. | ⚠️ See §3.1 |
| **Eval mode AllowAll** (⚠️ residual) | `Enforcer` with `store == nil` allows every request. Deploying without `-auth` silently disables all access control. | ⚠️ See §3.2 |
| Group repo bypasses member auth | Group repos read from members; auth is checked on the group's own policy, not the member's. A public group that includes a private member leaks the member's content. | ⚠️ See §3.3 |

---

### 2.6 Denial of service

| Threat | Control | Status |
|--------|---------|--------|
| Oversized artifact upload exhausting disk | No upload size limit is enforced; storage fills up | ⚠️ Known gap — add max body size middleware |
| Recursive proxy fetch loop | Upstream URL is admin-configured; group members are repo names (resolved at serve time, not recursively) | ✅ No recursion possible |
| Circuit breaker on upstream failures | `proxy.Fetcher` has a configurable circuit breaker; after `cbFailureThreshold` consecutive upstream failures, requests are rejected fast | ✅ Mitigated |
| Concurrent publish index regeneration race | Job queue with idempotent workers; `internal/indexer` uses per-namespace locking | ✅ Mitigated |
| Rate limiting | No rate limiting is implemented | ⚠️ Known gap — out of scope for P7 |

---

### 2.7 SSRF — server-side request forgery

| Threat | Control | Status |
|--------|---------|--------|
| **Admin-controlled upstream SSRF** (⚠️ residual) | `POST /api/v1/repos` lets an admin set any `Upstream` URL. forge fetches from it on proxy cache misses triggered by any authenticated user. Admins are already highly privileged, but forge may have network access to internal services the admin cannot reach directly. | ⚠️ See §3.4 |
| User-controlled upstream URL | Users cannot set `Upstream`; only admins via the repo admin API | ✅ Mitigated |
| `ProxyAuth` credential forwarding | The `ProxyAuth` field is forwarded verbatim to the upstream. An admin can use it to send arbitrary credentials to the upstream, but cannot redirect to an attacker-controlled URL without setting `Upstream`. | ✅ Admin-gated; same privilege level |

---

## 3. Accepted risks & mitigations

### 3.1 Bootstrap token window

**Risk:** The very first `POST /api/v1/tokens` is unauthenticated. An attacker who can reach port 8080 during the brief window between forge starting and the operator creating the first token can mint an admin token.

**Accepted because:**
- The window is seconds, not minutes (forge logs the bootstrap token immediately on start).
- Production deployments run behind a load balancer / ingress that is not publicly accessible until the operator confirms setup.
- The Helm chart's `NOTES.txt` warns about this window.

**Mitigations available:**
- Start forge with a pre-generated bootstrap secret via environment variable rather than auto-generating (future feature).
- Restrict network access to port 8080 to the operator's host during initial setup using a network policy.
- Use the `-auth` flag only after standing up the ingress / firewall rules.

---

### 3.2 Eval mode AllowAll

**Risk:** When `-auth` is not passed (or `Auth.Store` is nil), `Enforcer` uses AllowAll — every request is permitted. There is no warning at request time, only at startup.

**Accepted because:**
- Eval mode is an explicitly documented and supported mode for local development.
- The startup log line `forge listening addr=:8080` does not indicate auth mode.

**Mitigations applied:**
- `CLAUDE.md` and `README.md` document that `-auth` is required for production.
- The Helm chart enables auth by default (`forge.auth.enabled: true`).
- **Recommended addition:** emit a `WARN` slog line on every request if `s.Auth == nil` and the request is not a probe. (Logged as a backlog item.)

---

### 3.3 Group repo membership and auth

**Risk:** A group repo's `AnonymousRead` setting applies to the group itself. If a group has `AnonymousRead: true` and one of its members has `AnonymousRead: false`, unauthenticated clients can read the private member's content through the group.

**Accepted because:**
- Group membership is admin-controlled; admins are responsible for not mixing public groups with private members.
- The default seeded repos do not do this (public groups contain only public members).

**Recommended mitigation:** enforce that group `AnonymousRead` must be `false` if any member has `AnonymousRead: false`. Log this as a validation error at startup and in `validateRepo()`.

---

### 3.4 Admin-controlled SSRF via proxy upstream

**Risk:** An admin can create a proxy repo with `Upstream: http://internal-service/` and then any authenticated user requesting an artifact from that repo causes forge to make an HTTP GET to the internal service. Forge may have network access to internal services (Postgres, MinIO, other pods) that the admin cannot reach directly from their workstation.

**Accepted because:**
- The admin role represents full trust in the forge system; if the admin is compromised, the entire forge installation is compromised regardless.
- Exploitation requires admin credentials, not just write credentials.

**Mitigations available (not yet implemented):**
- Upstream URL allowlisting: add an `allowed_upstreams` configuration that limits which URLs can be used as proxy upstreams.
- Block RFC 1918 / link-local addresses in the upstream HTTP client to prevent access to private network ranges.
- Both are recommended for deployments where forge runs in a privileged network position.

---

## 4. Security controls summary

| Control | Implementation | Test |
|---------|---------------|------|
| Token-based AuthN | `auth.Store` / SHA-256 hash | `TestTokenStore_*` |
| Per-repo RBAC | `auth.Enforcer.decide()` | `TestAuthzMatrix*` |
| OCI auth (WWW-Authenticate) | `auth.Enforcer.MiddlewareOCI()` | `TestAuthzMatrix_OCI` |
| Blob path traversal prevention | `blob.FS.resolve()` + fuzzer | `FuzzBlobFSKey` |
| Meta path traversal prevention | `meta.FS.resolve()` + fuzzer | `FuzzMetaFSNS`, `TestFS_Traversal` |
| Security response headers | `server.middleware()` | `TestSecurityHeaders_PresentOnAllRoutes` |
| Audit log | `slog` with `audit=true` tag | Log output inspection |
| Proxy circuit breaker | `proxy.Fetcher` | `TestProxy_CircuitBreaker` |
| Concurrent publish correctness | Job queue + idempotent indexer | `load/smoke.js` `index_verify` |
| SAST | gosec in CI (per-PR) | `.github/workflows/ci.yml` `security` job |
| Dependency vuln scan | govulncheck in CI (per-PR) | `.github/workflows/ci.yml` `security` job |
| Container scan | Trivy in CI (per-PR) | `.github/workflows/ci.yml` `docker` job |
| DAST | ZAP baseline (nightly) | `.github/workflows/ci.yml` `dast` job |
| SBOM | syft SPDX-JSON (on publish) | `publish` job |
| Release signing | cosign keyless Sigstore | `publish` job |

---

## 5. Open findings

| ID | Severity | Description | Status |
|----|----------|-------------|--------|
| SEC-001 | Medium | No upload size limit; large artifacts can exhaust storage | Backlog |
| SEC-002 | Low | Group repo auth model allows public group over private member | Backlog |
| SEC-003 | Low | No rate limiting on any endpoint | Backlog — out of P7 scope |
| SEC-004 | Info | Admin SSRF via proxy upstream (admin-only exploitation) | Accepted risk; document upstream URL allowlisting as recommendation |
| SEC-005 | Info | Bootstrap token window on first start | Accepted risk; mitigated by deployment guidance |
| SEC-006 | Info | `/metrics` unauthenticated | Accepted (industry standard default); mitigate with network policy |
