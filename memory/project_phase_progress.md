---
name: project-phase-progress
description: Current phase completion status and what's in progress for the forge artifact repository project
metadata:
  type: project
---

Progress as of 2026-05-31.

**Why:** Building forge from prototype → GA per WORKPLAN.md. Phases are sequential but Phase K is cross-cutting.

**Completed phases:**
- P0: CI, testcontainers harness, golden-file framework, coverage gates
- P1: Postgres meta.Store, S3/MinIO blob.Store
- P2: Token-based AuthN/AuthZ with per-repo RBAC
- P3: All format completeness (Maven SNAPSHOT, Gradle .module, npm dist-tags/deprecate/audit, OCI/Docker registry, CRAN PACKAGES.rds, group repos)
- P4: Proxy cache correctness — TTL, ETag, negative cache, stale-on-error, retries, upstream auth, circuit breaker (all in internal/proxy)
- P5: Job queue (internal/queue — Mem + PG impls), idempotent npm packument regen (internal/indexer), Maven SNAPSHOT race fix (per-artifact records), server wired with queue
- P6: Admin API, web UI, metrics/tracing, audit log, Grafana dashboard, runbooks (all exit criteria met)
- Phase K: Container image, Helm chart (forge + forge-stack), Terraform modules (AWS/GCP), GitOps assets, CI cluster-install + quickstart gate (all exit criteria met)

**Phase 7 — Security & GA hardening — COMPLETE (as of 2026-05-31):**
All P7 deliverables shipped. Remaining GA blockers are tracked separately below.

Delivered in P7:
- meta.FS path traversal fix: resolve()/resolveDir() reject ns inputs that escape root (was unfixed; blob.FS was already safe)
- CI security job (per-PR): gosec SAST (-severity medium), govulncheck dependency scan
- CI docker job extended: Trivy image scan (HIGH/CRITICAL, ignore-unfixed) on every PR build
- publish job extended: syft SPDX-JSON SBOM (source + image), cosign keyless image signing (Sigstore/Rekor), self-verification, SBOM attached to GitHub releases on vX.Y.Z tags
- Path-traversal fuzz suite: FuzzBlobFSKey, FuzzMetaFSNS, FuzzMetaFSKey (seed-corpus runs in go test ./...; engine mode via -fuzz flag)
- AuthZ matrix extended to 35 HTTP-level cases: OCI middleware (WWW-Authenticate, JSON error body), HEAD/DELETE/POST/PATCH method mapping, npm Basic-auth bearer format, RequireAdmin, expired token
- Security response headers middleware: X-Content-Type-Options, X-Frame-Options, Referrer-Policy, Content-Security-Policy (frame-ancestors 'none', no unsafe-eval) on every response
- DAST CI job (nightly): ZAP baseline passive scan via docker compose; .zap/rules.tsv tunes false positives; fails on Medium+
- k6 load suite: load/smoke.js (3-min nightly gate — metadata GET p99<50ms, 50 concurrent publishes zero-failure, index-verify 100% readable); load/soak.js (24h pre-release, not CI-gated)
- Pen test scope + threat model: docs/security/pentest-scope.md (55-route endpoint inventory, known-safe list, 7 priority targets), docs/security/threat-model.md (STRIDE table, 6 open findings)
- SEC-001 fixed: http.MaxBytesReader (5 GiB default) + Content-Length pre-check → 413 on PUT/POST/PATCH
- SEC-002 fixed: validateGroupPolicy + validateMemberPolicy in admin API; public group over private member rejected in both directions

**Open findings in threat model (not GA-blocking individually, but documented):**
- SEC-003 (Low): No rate limiting — backlog
- SEC-004 (Info): Admin SSRF via proxy upstream — accepted risk
- SEC-005 (Info): Bootstrap token window — accepted risk, deployment guidance written
- SEC-006 (Info): /metrics unauthenticated — accepted risk, network policy recommended

**GA blockers remaining (from §1 workplan):**

1. **Conformance suite incomplete (HARD BLOCKER)**
   - Only npm has real-client conformance tests (3 tests: proxy install, cache hit, hosted publish+install) in internal/conformance/
   - Maven (mvn 3.6/3.9, gradle 7/8), Helm (helm 3.x, OCI push/pull), CRAN (R 4.x, renv, pak), OCI (docker/oras) have ZERO conformance tests
   - Workplan §1 requires all formats × {hosted, proxy} × client matrix green
   - This is the largest remaining code gap; ~2-3 days per format

2. **Coverage gate not met (HARD BLOCKER)**
   - Current: 55.5% overall, target ≥75% (§5.10); core packages target ≥85%
   - Per-package: auth 82.6% ✓, proxy 85.5% ✓, server 69.9%, indexer 73.8%, meta 55.1%, blob 43.5%, format handlers 23-66%
   - s3store.go and pgstore.go show 0% in unit suite (they're integration-tagged; don't count toward gate)
   - CI coverage gate is still at the 35% lenient placeholder — needs ratcheting once coverage improves
   - Format handler coverage best raised in parallel with conformance (same HTTP paths exercised)

3. **Third-party pen test (HARD BLOCKER — external)**
   - Scope document ready (docs/security/pentest-scope.md)
   - Actual engagement not yet scheduled; must complete with no High/Critical open for GA
   - Not a code task — needs external scheduling

4. **24h soak test not run (SOFT — pre-release step)**
   - load/soak.js exists and is documented
   - Needs a persistent deployment and manual trigger before GA tag
   - Not CI-gated by design

5. **Multi-node HA not explicitly tested (SOFT)**
   - Architecture is stateless (P1 storage interfaces); CI tests single-node kind
   - "≥2 app nodes, no sticky state" from §1 not yet exercised end-to-end
   - Can be validated during soak test or as a separate chaos run

**How to apply:** Start the next session on the two actionable code blockers: conformance suite (Maven first, then Helm/CRAN/OCI) and coverage ratchet. Third-party pen test and soak are scheduling tasks, not code tasks.
