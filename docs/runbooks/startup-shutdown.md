# Startup & shutdown

## Start (eval / local)

```bash
# Filesystem storage, no auth, text logs
./forge -addr :8080 -data ./data -log-format text

# With auth enabled (creates bootstrap token on first run)
./forge -addr :8080 -data ./data -auth
```

## Start (Docker Compose)

```bash
docker compose up -d
docker compose logs -f forge
```

## Start (Kubernetes / Helm)

```bash
helm upgrade --install forge deploy/helm/forge \
  --namespace forge --create-namespace \
  --set image.tag=<version>

kubectl -n forge rollout status deployment/forge
```

## Verify it's up

```bash
curl -sf http://localhost:8080/healthz   # → "ok"
curl -sf http://localhost:8080/readyz    # → "ok"
```

Both probes return HTTP 200 + body `ok\n`. Any other response means forge is not ready to serve traffic.

## Graceful shutdown

Forge drains in-flight requests on **SIGTERM** before exiting. The drain window defaults to 30 s and is configurable:

```bash
./forge -drain-timeout 60s ...
```

In Kubernetes this maps to `terminationGracePeriodSeconds`. The Helm chart default is 35 s (30 s drain + 5 s buffer). To extend:

```yaml
# values.yaml
drainTimeout: "60s"

# also extend the pod grace period
terminationGracePeriodSeconds: 65
```

Send SIGTERM manually:

```bash
kill -TERM $(pgrep forge)
# or in k8s:
kubectl -n forge delete pod <pod-name>   # triggers graceful drain
```

## Forced stop

Only use SIGKILL if the graceful drain is stuck (check logs for "draining in-flight requests"):

```bash
kill -KILL $(pgrep forge)
```

In-flight requests that haven't completed will be dropped.

## Checking logs after a crash

```bash
# systemd
journalctl -u forge -n 200 --no-pager

# k8s — previous container
kubectl -n forge logs deployment/forge --previous

# k8s — current
kubectl -n forge logs deployment/forge
```

Look for `level=ERROR` entries. The last few lines before exit usually identify the cause.

## Rolling restart (zero-downtime, Kubernetes)

```bash
kubectl -n forge rollout restart deployment/forge
kubectl -n forge rollout status deployment/forge
```

Requires `replicaCount >= 2` and `podDisruptionBudget.enabled: true`.
