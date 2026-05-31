# forge — Authentication & Authorization

---

## Overview

forge uses **long-lived API tokens** for authentication. Tokens carry
**per-repository grants** that define what the holder can do. Anonymous
access can be enabled per repository for read operations.

In **eval mode** (no `-auth` flag), all requests are permitted without a
token. This is the default for local evaluation. For any persistent or
shared deployment, enable auth.

---

## Enabling authentication

Start forge with the `-auth` flag:

```bash
./forge -addr :8080 -data ./data -auth
```

On first start with a clean data directory, forge mints a **bootstrap admin
token** and prints it to stderr:

```
INFO auth enabled: bootstrap admin token created id=a1b2c3d4 secret=forge_<64hex>
WARN store the bootstrap secret; it will not be shown again
```

**The secret is shown exactly once.** Store it in a password manager or
secrets vault before proceeding. It cannot be recovered — only revoked and
replaced.

In Kubernetes (Helm), auth is always enabled. The bootstrap token is printed
in the pod logs on first start:

```bash
kubectl logs -n forge -l app.kubernetes.io/name=forge | grep "bootstrap admin"
```

---

## Token model

### Token format

All tokens have the prefix `forge_` followed by 64 lowercase hex characters
(32 random bytes):

```
forge_99e25eabc1ef7461eea7fdd9bcbdd274e10dd8481c93551d3ce150bc1dd096e8
```

Tokens are stored by their SHA-256 hash. The raw secret is never stored and
cannot be recovered after creation.

### Roles

| Role | Value | Permissions |
|------|-------|-------------|
| `read` | 1 | GET and HEAD requests |
| `write` | 2 | Everything in read + PUT, POST, DELETE, PATCH |
| `admin` | 3 | Everything in write + token management + repo CRUD |

Roles are hierarchical — a `write` token can also read; an `admin` token can
also write.

### Grants

Each token carries one or more **grants**, each of which assigns a role to a
specific repository. Use `"repo": "*"` as a wildcard to match all
repositories.

```json
{
  "description": "ci-bot",
  "grants": [
    { "repo": "npm-hosted", "role": "write" },
    { "repo": "maven-hosted", "role": "write" },
    { "repo": "helm-hosted", "role": "read" }
  ]
}
```

A request is allowed if **any** grant matches the target repository and the
grant's role is sufficient for the HTTP method.

---

## Creating tokens

The token API requires an admin token (or is unauthenticated for the very
first call when no tokens exist yet — the bootstrap).

### Bootstrap (first token, no credentials needed)

```bash
curl -s -X POST http://localhost:8080/api/v1/tokens \
  -H "Content-Type: application/json" \
  -d '{
    "description": "admin",
    "grants": [{ "repo": "*", "role": "admin" }]
  }'
```

Response:

```json
{
  "id": "a1b2c3d4e5f6a7b8",
  "description": "admin",
  "grants": [{ "repo": "*", "role": 3 }],
  "created_at": "2026-05-31T12:00:00Z",
  "secret": "forge_99e25eabc1ef7461..."
}
```

The `secret` field is only present in the creation response.

### Subsequent tokens (admin credentials required)

```bash
ADMIN_TOKEN=forge_<your-admin-token>

# CI token: write to npm-hosted only
curl -s -X POST http://localhost:8080/api/v1/tokens \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "description": "jenkins-ci",
    "grants": [{ "repo": "npm-hosted", "role": "write" }]
  }'

# Read-only token: all repos
curl -s -X POST http://localhost:8080/api/v1/tokens \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "description": "developer-read",
    "grants": [{ "repo": "*", "role": "read" }]
  }'
```

### Token with expiry

```bash
curl -s -X POST http://localhost:8080/api/v1/tokens \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "description": "temp-30-days",
    "grants": [{ "repo": "npm-hosted", "role": "write" }],
    "expires_at": "2026-06-30T00:00:00Z"
  }'
```

Expired tokens are rejected at verification time with `401 Unauthorized`.

---

## Listing and revoking tokens

```bash
# List all tokens (no secrets shown)
curl -s http://localhost:8080/api/v1/tokens \
  -H "Authorization: Bearer $ADMIN_TOKEN"

# Revoke a token by ID
curl -s -X DELETE http://localhost:8080/api/v1/tokens/<token-id> \
  -H "Authorization: Bearer $ADMIN_TOKEN"
```

Revocation is immediate and permanent.

---

## Using a token

Pass the token as a `Bearer` header on every request:

```bash
curl -H "Authorization: Bearer $TOKEN" \
  http://localhost:8080/repository/npm-hosted/mypackage
```

### Package manager configuration

See [Usage](usage.md) for per-client configuration. A summary:

| Client | Configuration |
|--------|--------------|
| Maven | `<server>` entry in `settings.xml` with `<password>` set to the token |
| npm / pnpm | `_authToken=<token>` in `.npmrc` for the registry URL |
| yarn | `_authToken=<token>` in `.npmrc` (yarn v1 reads `.npmrc`) |
| Helm | `helm registry login` with `--username ignored --password <token>` |
| R | `options(HTTPUserAgent)` or pass via `method="curl"` with custom headers |
| curl / oras / crane | `-H "Authorization: Bearer $TOKEN"` |

npm-style clients can also use HTTP Basic auth with an empty username and the
token as the password:

```
Authorization: Basic <base64(":" + token)>
```

forge accepts both forms.

---

## Anonymous read access

By default, hosted repositories created with auth enabled require a token for
all operations. To allow read access without a token (e.g. for a public
package mirror):

```bash
# Via Admin API
curl -s -X PUT http://localhost:8080/api/v1/repos/npm-hosted \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "npm-hosted",
    "format": "npm",
    "kind": "hosted",
    "anonymousRead": true
  }'
```

With `anonymousRead: true`:
- `GET` and `HEAD` requests succeed without a token
- `PUT`, `POST`, `DELETE`, `PATCH` still require a token with write or higher

Proxy and group repositories default to `anonymousRead: true` because they
are typically read-only consumer paths.

---

## Group repository security

A group repository merges results from its member repositories. forge enforces
a **consistency policy**: a group with `anonymousRead: true` cannot contain a
member with `anonymousRead: false`. This prevents anonymous clients from
reading private artifacts through the group.

Attempting to create or update such a group returns `400 Bad Request`:

```
group "npm-public" has anonymousRead=true but member "npm-private" has
anonymousRead=false: would expose private artifacts anonymously
```

---

## OCI / Docker registry auth

The OCI Distribution Spec requires a specific error format for auth failures.
forge returns the correct response so that `docker`, `helm`, `oras`, and
`crane` can perform auth discovery:

```
HTTP/1.1 401 Unauthorized
WWW-Authenticate: Bearer realm="forge"
Content-Type: application/json

{"errors":[{"code":"UNAUTHORIZED","message":"authentication required"}]}
```

Configure Docker clients:

```bash
docker login localhost:8080 -u ignored -p $TOKEN
```

Or pass the token inline:

```bash
oras push --plain-http --username ignored --password $TOKEN \
  localhost:8080/docker-hosted/myimage:v1 artifact.bin
```

---

## Security considerations

- **Bootstrap window:** `POST /api/v1/tokens` is unauthenticated when no
  tokens exist. In production, the first request after deployment should
  immediately create an admin token. On Kubernetes, network policies should
  restrict access to the forge service until bootstrap is complete.

- **Token rotation:** there is no automatic rotation. Rotate long-lived
  service tokens periodically by creating a replacement, updating the client
  configuration, then revoking the old token.

- **Admin SSRF:** admin tokens can configure proxy repositories with arbitrary
  upstream URLs. Restrict admin token issuance to trusted operators.

- **TLS:** forge does not terminate TLS itself. In production, terminate TLS at
  the ingress (Nginx, Traefik, etc.) and ensure clients connect over HTTPS.
  The token is a bearer credential — transmitting it over plain HTTP exposes it.
