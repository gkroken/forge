---
name: project-phase-progress
description: Current phase completion status and GA gap analysis for the forge artifact repository project — verified against codebase 2026-05-31
metadata:
  type: project
---

Progress as of 2026-05-31. All findings verified by direct codebase inspection.

**Why:** Building forge from prototype → GA per WORKPLAN.md. Phases are sequential but Phase K is cross-cutting.

---

## Phase completion (code-verified)

**Genuinely complete:**
- P0: CI, testcontainers harness, golden-file framework (maven-metadata, snapshot, helm index, CRAN PACKAGES text+RDS), coverage gate (75% overall)
- P1: Postgres meta.Store + S3/MinIO blob.Store; contract suites run against both (integration tag); up/down migration test
- P4: Full proxy/cache correctness — TTL, ETag revalidation, negative cache, stale-on-error, retries, upstream auth, circuit breaker (all in internal/proxy)
- P6 (partial — see gap #10): Admin API, web UI, metrics (Prometheus), audit log, Grafana dashboard, runbooks, search — but **tracing is missing**

**Partially complete (gaps below):**
- P2: Token auth + per-repo RBAC + authz matrix test ✓ — **OIDC/LDAP/SAML never implemented**
- P3: Most deliverables done — **CRAN binary trees (/bin/) not implemented; Helm oci:// untested in conformance; scoped npm and group-mode conformance absent**
- P5: Job queue + idempotent indexer ✓ — **queue.NewPG never wired in main.go; production always uses MemQueue** (critical HA bug)
- P7: SAST, DAST, dep scan, container scan, SBOM, signing ✓ — **chaos suite not implemented; pen test pending (external)**
- Phase K: Container image, Helm chart, forge-stack bundled chart, GitOps assets, cluster-install test, quickstart gate ✓ — **dogfooding not implemented** *(Terraform cloud modules descoped to post-GA — see §1a.B)*

---

## GA blockers — full list (verified 2026-05-31)

### Tier 0 — External (cannot code-fix)

1. **Third-party pen test** — hard §1 blocker; scope doc at docs/security/pentest-scope.md; engagement not scheduled
2. **24h soak run** — soft §1 blocker; load/soak.js exists; needs persistent deployment + manual trigger

### Tier 1 — Code bug that breaks a stated §1 guarantee

3. **PG queue not wired in main.go** — `cmd/forge/main.go:161` always uses `queue.NewMem(256)`, even when `POSTGRES_DSN` is set. The comment on line 158 documents the fix ("pass a `queue.NewPG(metaPG.DB())` here instead") but the code never does it. In HA multi-pod mode: index-regen jobs are lost on pod restart and not shared across pods. Directly breaks §1 HA claim and P5 "no lost updates" exit criterion. **This is a code fix, not a scheduling task.**

### Tier 2 — §1/§1a criteria violations

4. **OIDC/LDAP/SAML not implemented** (P2) — only token auth exists. P2 exit criterion requires OIDC + LDAP/SAML. Must either implement or formally descope for initial GA.
5. ~~**Azure Terraform module**~~ — **descoped to post-GA.** Terraform cloud modules (AWS/GCP/Azure) are out of scope for GA. The `forge-stack` Helm chart (bundled Postgres + MinIO) is the IaC production path. Existing AWS + GCP modules remain in the repo as a bonus.
6. **Per-package ≥85% coverage not CI-gated** (§5.10) — only a comment in ci.yml:67; the gate only checks overall 75%. format/npm is at 67.1%; would fail if enforced.

### Tier 3 — Conformance client matrix gaps (§5.4 / §1 first bullet)

7. No Gradle 7/8 conformance test (Maven)
8. No pnpm/yarn conformance test (npm)
9. No renv/pak conformance test (CRAN)
10. No docker CLI conformance test (OCI — only oras tested)
11. No `helm push oci://` conformance test (Helm OCI mode)

### Tier 4 — Phase deliverables never built

12. **Distributed tracing** (P6) — no OpenTelemetry/Jaeger anywhere; obs package is Prometheus + slog only
13. **Chaos suite** (§5.8/P7) — no pod-kill or S3/PG-blip tests; not in CI
14. ~~**Terraform apply→destroy nightly**~~ — **descoped to post-GA** with Terraform modules
15. **Dogfooding in CI** (Phase K exit) — CI publishes to GHCR, not to a forge instance
16. **CRAN per-OS binary trees** under /bin/ (P3) — only src/contrib/ is served
17. **Go version + OS matrix** (§7) — CI uses only ubuntu-latest + single go.mod version; no Go stable-1 or macOS runners

### Tier 5 — Quality gaps (not hard blockers)

18. Migration test against production-sized dataset (§5.9) — only writes one record before rollback
19. blobtest.RunContract missing "traversal-reject" and "large-stream" cases (§5.3 workplan comment)
20. No authz fuzz test (§5.6 requirement)
21. GitOps dry-run (ArgoCD/Flux reconcile) not in CI (§5.12)
22. HPA and PDB disabled by default; not asserted in cluster-install CI test

---

## What the previous memory got wrong

The previous entry said "both actionable code blockers are resolved" and "no further code changes needed to unblock GA." This was inaccurate:

- Gap #3 (PG queue) is a **code bug**, not a scheduling task — it breaks the HA guarantee
- Gaps #4–17 are all code/CI work, not external dependencies
- P2, P3, P5, P6, P7, and Phase K all have unimplemented deliverables

**How to apply:** When estimating GA readiness or planning next sessions, treat Tier 1 (gap #3) as a one-session fix, Tier 2-3 as multiple sprints of work, and Tier 4 as a deliberate descope decision unless the full workplan scope is required.
