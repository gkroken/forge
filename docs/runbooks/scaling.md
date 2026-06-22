# Scaling forge horizontally

How to run forge as N replicas behind a load balancer, and what state is
shared vs. per-pod. Design rationale lives in [`WORKPLAN-SCALING.md`](../../WORKPLAN-SCALING.md).

## Prerequisite: external storage

Multiple replicas **require `storage.type=external`** — S3 for blobs and Postgres
for meta. The `fs` backend (PVC + local JSON/blob files) cannot be shared across
pods; the Helm chart will refuse to render `replicaCount > 1` or `autoscaling.enabled`
while `storage.type=fs`:

```
storage.type=fs supports only a single replica. Set storage.type=external …
```

With external storage the data plane is already replica-safe:

| Plane | Backend | Shared across pods? |
|-------|---------|---------------------|
| Blobs | S3 (`S3_ENDPOINT`, `S3_BUCKET`, …) | yes |
| Metadata | Postgres (`POSTGRES_DSN`) | yes |
| Index-regen queue | Postgres (`FOR UPDATE SKIP LOCKED`) | yes — auto when `POSTGRES_DSN` set |
| Audit log | Postgres (`audit_log` table) | yes — auto when `POSTGRES_DSN` set |

## Deploy

```bash
helm upgrade --install forge deploy/helm/forge \
  --set storage.type=external \
  --set replicaCount=3 \
  --set-string extraEnv.POSTGRES_DSN="postgres://forge:secret@pg:5432/forge?sslmode=require" \
  --set-string extraEnv.S3_ENDPOINT="https://s3.amazonaws.com" \
  --set-string extraEnv.S3_BUCKET="forge-artifacts" \
  --set extraEnvFrom[0].secretRef.name=forge-s3-credentials
```

Or enable the HPA (`autoscaling.enabled=true`, `minReplicas`/`maxReplicas`) instead
of a fixed `replicaCount`. A PodDisruptionBudget (`podDisruptionBudget.enabled=true`)
is recommended so rolling node drains keep a quorum serving.

Schema (including the `audit_log` table) is created automatically on first connect
by `migrate.Up`; no manual migration step.

## What is coherent across the fleet vs. per-pod

This is the **hybrid observability model** — know which numbers are fleet-wide:

| State | Scope | Where to view the fleet view |
|-------|-------|------------------------------|
| Artifacts, metadata, repos, tokens, users | **Fleet-wide** (S3 + PG) | Any pod / the UI |
| Index-regen queue | **Fleet-wide** (PG) | — |
| **Audit log** (Activity tab) | **Fleet-wide** (PG) — survives restarts | UI Observability → Audit log |
| Request/cache/latency metrics (Dashboard & Observability charts) | **Per-pod** in-memory; reset on restart | **Prometheus / Grafana** (scrape `/metrics`) |
| Circuit breakers / upstream health dot | **Per-pod** — each replica protects its own upstream calls | Per-pod; aggregate health in Grafana |

The Dashboard/Observability charts show a `REPLICA <pod>` label and a note that the
numbers are per-pod. **Do not** read them as fleet totals — the load balancer routes
each page load to an arbitrary replica, so the figures will jump between pods. The
authoritative fleet view is Prometheus.

### Wire up Prometheus + Grafana

- ServiceMonitor: `--set metrics.serviceMonitor.enabled=true` (needs the Prometheus
  Operator CRD). Match your operator's selector via `metrics.serviceMonitor.labels`.
- Grafana dashboard: `--set grafana.dashboard.enabled=true` (k8s-sidecar), or import
  `deploy/helm/forge/dashboards/forge-overview.json` manually.

Key metrics: `forge_http_requests_total`, `forge_http_request_duration_seconds`,
`forge_proxy_cache_hits_total` / `_misses_total`, `forge_artifact_downloads_total`,
`forge_queue_jobs_total` — all labelled, so `sum without (pod)` gives fleet totals.

## Gotchas

- **Per-pod circuit breakers are intentional.** Each replica independently fast-fails
  a flapping upstream; there is no shared breaker. One pod may be probing while another
  is open — expected, not a bug.
- **Audit retention is not yet automated.** The `audit_log` table grows unbounded.
  Until retention ships (scheduled `DELETE` / monthly partitions — see WORKPLAN-SCALING),
  prune manually: `DELETE FROM audit_log WHERE ts < now() - interval '90 days';`
- **Drain on scale-down.** `terminationGracePeriodSeconds` (35s) must exceed
  `-drain-timeout` (30s) so in-flight requests finish on rollout / scale-in. The chart
  keeps these aligned; preserve the gap if you override either.
