# Incident response

## High error rate (5xx spike)

**Signal:** `forge_http_requests_total{status=~"5.."}` climbing, or alerts on the Grafana dashboard Error Rate panel.

**Steps:**

1. Check recent logs for the error pattern:
   ```bash
   journalctl -u forge | jq 'select(.status >= 500)' | tail -20
   ```

2. Check if the problem is format-specific (route label in the metric) or global.

3. If `status=502` on proxy routes — upstream is likely down. See [Upstream registry unreachable](#upstream-registry-unreachable).

4. If `status=500` on hosted routes — check for storage errors:
   ```bash
   journalctl -u forge | jq 'select(.level == "ERROR")' | tail -20
   ```
   Look for `blob:` or `meta:` errors suggesting disk full or permission issues.

5. Rolling restart often clears transient issues:
   ```bash
   kubectl -n forge rollout restart deployment/forge
   ```

---

## Upstream registry unreachable

**Signal:** proxy repos returning 502, `forge_proxy_cache_hits_total` flat, `forge_proxy_cache_misses_total` rising.

**Behaviour:** forge has a per-upstream circuit breaker. After 5 consecutive failures it opens and serves stale cached content for 30 s before retrying. Clients are not broken as long as forge has a cached copy.

**Steps:**

1. Confirm the upstream is down (not a forge issue):
   ```bash
   curl -sf https://registry.npmjs.org/lodash
   ```

2. Check the circuit-breaker state in logs:
   ```bash
   journalctl -u forge | jq 'select(.msg | test("circuit"))' | tail -5
   ```

3. If forge has stale content, clients continue to work — monitor and wait for upstream recovery. No action required unless stale TTL expires.

4. If forge has no cached copy and clients are failing, consider:
   - Increasing `proxyTTL` on the affected repo (extends freshness window for next time)
   - Pointing the proxy upstream at a mirror

5. Once upstream recovers, the circuit resets automatically on the next successful probe.

---

## Disk full

**Signal:** `process_resident_memory_bytes` or blob write errors in logs; forge starts returning 500 on PUT.

**Steps:**

1. Check disk usage:
   ```bash
   df -h data/
   du -sh data/blobs/ data/meta/
   ```

2. Identify the largest repos:
   ```bash
   du -sh data/blobs/*/ | sort -rh | head -10
   ```

3. Immediate relief — remove proxy cache blobs (safe to delete; will refetch on demand):
   ```bash
   # Example: clear npm proxy cache
   rm -rf data/blobs/npm-proxy/
   rm -rf data/meta/npm-proxy\:proxy/
   ```

4. Longer term — add storage capacity, or implement cleanup policies to enforce retention limits.

---

## Pod OOMKilled (Kubernetes)

**Signal:** `kubectl -n forge get pods` shows `OOMKilled`; `kubectl -n forge describe pod <name>` confirms.

**Steps:**

1. Check recent memory usage on the Grafana dashboard (Heap Memory / Resident Memory panels).

2. Increase memory limit:
   ```yaml
   # values.yaml
   resources:
     limits:
       memory: 1Gi
   ```
   ```bash
   helm upgrade forge deploy/helm/forge -n forge -f values.yaml
   ```

3. If memory is growing unboundedly (not just a spike), check goroutine count on the dashboard. A goroutine leak will also cause memory growth.

4. Collect a heap profile before the OOM if possible:
   ```bash
   # Add -pprof flag to forge (not yet wired — add /debug/pprof in a future release)
   ```

---

## Pod crash-looping

**Signal:** `kubectl -n forge get pods` shows `CrashLoopBackOff`.

**Steps:**

1. Get logs from the previous container:
   ```bash
   kubectl -n forge logs deployment/forge --previous
   ```

2. Common causes:
   - **Bad config / env var** — look for `level=ERROR msg=fatal` near the end of the log.
   - **Port already in use** — `bind: address already in use`.
   - **Storage unreachable** — Postgres DSN wrong, S3 credentials missing.
   - **Data directory not writable** — check PVC mount and security context.

3. Fix the root cause in the Helm values or Kubernetes secrets, then:
   ```bash
   helm upgrade forge deploy/helm/forge -n forge -f values.yaml
   ```

---

## Auth failures spiking

**Signal:** `forge_http_requests_total{status="401"}` or `{status="403"}` rising unexpectedly.

**Steps:**

1. Identify which path and which remote:
   ```bash
   journalctl -u forge | jq 'select(.audit == true and .status == 401)' | tail -20
   ```

2. If a single source IP is generating many 401s, a token may have been rotated without updating the consumer — notify the team and check CI secrets.

3. If many sources are affected, the token store may be corrupt or the `-auth` flag may have been accidentally added/removed. Check the startup log.

4. To check whether a specific secret is still valid:
   ```bash
   curl -sf http://localhost:8080/api/v1/tokens \
     -H "Authorization: Bearer $SECRET" | jq length
   # 0 or error = invalid; array with entries = valid admin token
   ```
