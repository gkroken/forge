# forge runbooks

Operational procedures for running forge in production.

| Runbook | When to use |
|---------|-------------|
| [Startup & shutdown](startup-shutdown.md) | Deploying, restarting, draining |
| [Token management](token-management.md) | Auth setup, creating tokens, revoking compromised credentials |
| [Repository management](repository-management.md) | Adding, updating, or removing repositories |
| [Backup & restore](backup-restore.md) | Scheduled backup, disaster recovery |
| [Incident response](incident-response.md) | High error rate, upstream down, disk full, OOM |

## Quick orientation

```
forge -addr :8080 -data ./data [-auth] [-log-format json|text] [-drain-timeout 30s]
```

| Endpoint | Purpose |
|----------|---------|
| `GET /healthz` | Liveness — returns `ok` when the process is up |
| `GET /readyz` | Readiness — same check; split if you add a warmup gate |
| `GET /metrics` | Prometheus exposition (text + OpenMetrics) |
| `GET /ui/` | Web UI — repo browse, search, admin |
| `/api/v1/repos` | Admin REST API for repository CRUD |
| `/api/v1/tokens` | Admin REST API for token lifecycle |
| `/repository/{repo}/{path}` | Artifact serving (all formats) |
| `/v2/{repo}/...` | OCI Distribution Spec (Docker/Helm OCI) |

## Log filtering

Forge emits structured JSON logs. Useful `jq` filters:

```bash
# access log only
journalctl -u forge | jq 'select(.msg == "request")'

# audit events only (auth failures, writes, token lifecycle)
journalctl -u forge | jq 'select(.audit == true)'

# errors only
journalctl -u forge | jq 'select(.level == "ERROR")'
```
