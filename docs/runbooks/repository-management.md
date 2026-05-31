# Repository management

Repositories can be managed via the **web UI** at `/ui/admin/` or the **REST API** at `/api/v1/repos`. Both require admin role when auth is enabled.

## List repositories

```bash
curl -sf http://localhost:8080/api/v1/repos \
  -H "Authorization: Bearer $ADMIN_SECRET" | jq .
```

## Create a repository

```bash
# Hosted npm repo
curl -sf -X POST http://localhost:8080/api/v1/repos \
  -H "Authorization: Bearer $ADMIN_SECRET" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "npm-internal",
    "format": "npm",
    "kind": "hosted",
    "anonymousRead": false
  }'

# Proxy Maven Central
curl -sf -X POST http://localhost:8080/api/v1/repos \
  -H "Authorization: Bearer $ADMIN_SECRET" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "maven-central",
    "format": "maven",
    "kind": "proxy",
    "upstream": "https://repo1.maven.org/maven2",
    "anonymousRead": true,
    "proxyTTL": "24h"
  }'

# Group (merge hosted + proxy)
curl -sf -X POST http://localhost:8080/api/v1/repos \
  -H "Authorization: Bearer $ADMIN_SECRET" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "maven-public",
    "format": "maven",
    "kind": "group",
    "members": ["maven-internal", "maven-central"],
    "anonymousRead": true
  }'
```

**Formats:** `maven`, `npm`, `helm`, `cran`, `oci`
**Kinds:** `hosted`, `proxy`, `group`

## Update a repository

```bash
curl -sf -X PUT http://localhost:8080/api/v1/repos/maven-central \
  -H "Authorization: Bearer $ADMIN_SECRET" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "maven-central",
    "format": "maven",
    "kind": "proxy",
    "upstream": "https://repo1.maven.org/maven2",
    "anonymousRead": true,
    "proxyTTL": "48h"
  }'
```

The full repository object must be sent (PUT semantics — partial updates are not supported).

## Delete a repository

```bash
curl -sf -X DELETE http://localhost:8080/api/v1/repos/old-repo \
  -H "Authorization: Bearer $ADMIN_SECRET"
# → HTTP 204
```

> **Note:** deleting a repository removes its configuration but does **not** purge stored blobs or meta records. To reclaim storage, manually remove `data/blobs/<repo-name>/` and `data/meta/<repo-name>:*/`.

## Change proxy TTL

```bash
curl -sf -X PUT http://localhost:8080/api/v1/repos/npm-proxy \
  -H "Authorization: Bearer $ADMIN_SECRET" \
  -H "Content-Type: application/json" \
  -d '{"name":"npm-proxy","format":"npm","kind":"proxy","upstream":"https://registry.npmjs.org","anonymousRead":true,"proxyTTL":"1h"}'
```

TTL takes effect for new cache entries only; existing cached blobs remain valid until their own TTL expires.

## Repositories persist across restarts

Repository configuration is stored in the meta store under `forge:repos`. Default repos are seeded only on first run (when the store is empty). Subsequent restarts load the persisted configuration.
