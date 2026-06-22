---
name: project-phase-progress
description: Current phase completion status and GA gap analysis for the forge artifact repository project — verified against codebase 2026-06-05
metadata:
  type: project
---

Progress as of 2026-06-05. All findings verified by direct codebase inspection.

**Why:** Building forge from prototype → GA per WORKPLAN.md. Phases are sequential but Phase K is cross-cutting.

---

## Phase completion (all code-complete)

- **P0** ✅ CI, testcontainers harness, golden-file framework, coverage gate (75% overall + per-package ≥85% for auth/proxy/indexer/server)
- **P1** ✅ Postgres meta.Store + S3/MinIO blob.Store; contract suites; up/down migration test
- **P2** ✅ Token auth + per-repo RBAC + authz matrix test. **OIDC SSO shipped post-GA** (2026-06-22, live-validated vs Keycloak 26 — see project-next-steps); LDAP/SAML still descoped to post-GA.
- **P3** ✅ All format deliverables: CRAN binary trees (Windows .zip + macOS .tgz), Helm OCI push/pull, scoped npm group conformance, full client matrix (Gradle, pnpm, yarn, pak, renv, crane)
- **P4** ✅ Proxy/cache correctness — TTL, ETag revalidation, negative cache, stale-on-error, retries, upstream auth, circuit breaker
- **P5** ✅ Job queue + idempotent indexer. `queue.NewPG` wired in `cmd/forge/main.go` when `POSTGRES_DSN` is set (fixed in `c374fb9`). Eval mode falls back to `queue.NewMem`.
- **P6** ✅ Admin API, web UI (fully complete per WORKPLAN-UI — all phases U0–U3 done), metrics (Prometheus), audit log, Grafana dashboard, runbooks, search. Distributed tracing descoped to post-GA.
- **P7** ✅ (code) SAST (gosec), DAST (ZAP), dep scan (govulncheck), container scan (Trivy), SBOM (syft), signing (cosign). Chaos suite descoped to post-GA. Pen test pending (external).
- **Phase K** ✅ Container image, Helm chart, forge-stack bundled chart, GitOps assets (ArgoCD + Flux), cluster-install CI test, timed quickstart gate, dogfooding (CI pushes forge's own chart to forge). Terraform cloud modules descoped to post-GA.

---

## GA blockers — remaining (2026-06-05)

### Tier 0 — External only (no code work remaining)

1. **Third-party pen test** — hard §1 blocker. Scope doc at `docs/security/pentest-scope.md`. Engagement not yet scheduled.
2. **24h soak run** — soft §1 blocker. `load/soak.js` exists and is tested in nightly CI smoke. Needs a persistent deployment and a manual trigger to run the full 24h scenario.

**Everything code-fixable is done.** All other GA criteria are met, including the full CRAN binary DoD (B0–B3 complete as of 2026-06-05).

---

## Nightly CI status (2026-06-05)

Current state: all nightly jobs green after recent fixes.

Recent fix history:
- **govulncheck**: bumped `go.mod` to `go 1.25.11` (fixes GO-2026-5037/5038/5039)
- **ZAP DAST**: added `Cross-Origin-Resource-Policy: same-site` header; suppressed false-positive rules 10049 + 90005; later suppressed rule 90004 false-positive (2026-06-04)
- **k6 load test** (cb0b576): `vu.idInScenario` is globally allocated in k6 v2 — `metadata_get`'s 10 VUs claim IDs 1-10, so `concurrent_publish`'s 50 VUs get IDs 11-60; neither 11-60 nor 12-61 match indexVerify's 1-50 range. Fixed by switching to `scenario.iterationInInstance + 1` (0-indexed unique per iteration, always 0-49 for 50 iterations).

---

## Completed gap reference

All previously open gaps are resolved. Key ones worth remembering:
- Gap #3 (PG queue not wired): fixed `c374fb9`
- Gap #6 (per-package coverage gate): fixed `f5303d5`
- Gap #15 (dogfooding): fixed `40bcb33`
- WORKPLAN-UI all phases: complete `6fc38ba`
- Per-package ≥85% coverage gate: ci.yml gates auth/proxy/indexer/server
- WORKPLAN-CRAN-BINARY all phases: complete `a0f391c` (B0 fields, B1 conformance, B2 ops, B3 proxy+group)

---

## Post-GA items (deliberately descoped)

- ~~OIDC federation~~ **DONE 2026-06-22** (Keycloak, live-validated); LDAP/SAML federation still pending
- Terraform cloud modules (AWS/GCP/Azure)
- Distributed tracing
- Chaos suite
- Terraform apply→destroy nightly
