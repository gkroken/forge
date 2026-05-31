# Token management

Auth is optional. It is enabled by passing `-auth` at startup. Without `-auth`, all requests are permitted (eval mode).

## Bootstrap

On first startup with `-auth`, forge creates one admin token and prints the secret **once**:

```
INFO auth enabled: bootstrap admin token created  id=tok_abc123  secret=forge_xxxxx…
WARN store the bootstrap secret; it will not be shown again
```

Store that secret in a password manager or secrets vault immediately. It cannot be recovered — if lost, use the steps under [Recovering admin access](#recovering-admin-access).

## Create a token

```bash
# Requires an existing admin token
curl -sf -X POST http://localhost:8080/api/v1/tokens \
  -H "Authorization: Bearer $ADMIN_SECRET" \
  -H "Content-Type: application/json" \
  -d '{
    "description": "CI pipeline",
    "grants": [
      {"repo": "npm-hosted", "role": "write"},
      {"repo": "maven-hosted", "role": "write"}
    ]
  }'
```

The response includes `secret` — shown once, store it immediately.

**Roles:** `read` (GET/HEAD), `write` (read + PUT/POST/DELETE on artifacts), `admin` (write + token and repo management).

**Wildcard grant:** `{"repo": "*", "role": "admin"}` grants admin on all repos.

**Expiry:** add `"expires_at": "2027-01-01T00:00:00Z"` for a time-limited token.

## List tokens

```bash
curl -sf http://localhost:8080/api/v1/tokens \
  -H "Authorization: Bearer $ADMIN_SECRET" | jq .
```

Returns id, description, grants, and expiry for every token. Secrets are never returned after creation.

## Revoke a token

```bash
TOKEN_ID="tok_abc123"
curl -sf -X DELETE http://localhost:8080/api/v1/tokens/$TOKEN_ID \
  -H "Authorization: Bearer $ADMIN_SECRET"
# → HTTP 204
```

Revocation takes effect immediately for all subsequent requests.

## Rotate a token

There is no in-place rotation. The procedure is:

1. Create a new token with the same grants.
2. Update the consumer (CI secret, `.npmrc`, etc.) with the new secret.
3. Verify the consumer works.
4. Revoke the old token.

## Recovering admin access

If all admin tokens are lost or revoked:

1. Stop forge.
2. Delete the token records from the meta store:
   - **Filesystem:** `rm data/meta/forge\:tokens/*.json`
   - **Postgres:** `DELETE FROM meta WHERE ns = 'forge:tokens';`
3. Restart forge with `-auth`. A new bootstrap admin token will be printed.

> **Warning:** this invalidates all existing tokens, not just admin ones. All consumers must be updated.

## Audit trail

Every token creation and revocation is logged with `audit=true`:

```bash
journalctl -u forge | jq 'select(.audit == true and (.event == "token.create" or .event == "token.revoke"))'
```
