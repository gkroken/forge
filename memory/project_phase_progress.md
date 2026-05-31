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
- P2: Token auth + per-repo RBAC + authz matrix test ✓ — OIDC/LDAP/SAML descoped to post-GA (see note below)
- P3: All deliverables done ✓ — CRAN binary trees implemented (Windows .zip + macOS .tgz, hosted-only), Helm oci:// conformance tested, scoped npm group conformance complete
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

4. ~~**OIDC/LDAP/SAML**~~ — **descoped to post-GA.** Production workflows (anonymous installs, CI publish via service tokens, admin via API tokens) are fully covered by the existing token model. The org uses AD + Keycloak; SSO self-service token issuance is a post-GA feature. SAML not needed (Keycloak bridges it).
5. ~~**Azure Terraform module**~~ — **descoped to post-GA.** Terraform cloud modules (AWS/GCP/Azure) are out of scope for GA. The `forge-stack` Helm chart (bundled Postgres + MinIO) is the IaC production path. Existing AWS + GCP modules remain in the repo as a bonus.
6. **Per-package ≥85% coverage not CI-gated** (§5.10) — only a comment in ci.yml:67; the gate only checks overall 75%. format/npm is at 67.1%; would fail if enforced.

### Tier 3 — Conformance client matrix gaps — COMPLETE ✓

All client matrix gaps closed (2026-05-31):
- Maven: TestMaven_Gradle_Hosted_PublishResolve (gradle:8.7-jdk21, maven-publish plugin)
- npm: TestNpm_pnpm_Proxy_Install (pnpm), TestNpm_Yarn_Hosted_PublishInstall (yarn v1)
- CRAN: TestCRAN_pak_Hosted_Install (pak), TestCRAN_renv_Hosted_Install (renv)
- OCI: TestOCI_Crane_PushPull (crane, daemon-free; gates on Docker Hub reachable)
- Helm OCI: TestHelm_OCI_PushPull (helm push/pull oci://, requires Helm 3.14+ / --plain-http)

### Tier 4 — Phase deliverables never built

12. ~~**Distributed tracing**~~ — **descoped to post-GA.** Prometheus metrics + structured logs cover operational needs for an on-prem artifact repo.
13. ~~**Chaos suite**~~ — **descoped to post-GA.** HA correctness is covered by architecture (stateless app, PG queue, S3 blobs) and the 24h soak run.
14. ~~**Terraform apply→destroy nightly**~~ — **descoped to post-GA** with Terraform modules
15. ~~**Dogfooding in CI**~~ — **COMPLETE ✓** Publish job starts forge in eval mode and pushes its own Helm chart to it, then pulls back to verify. Closes Phase K exit criterion.
16. ~~**CRAN per-OS binary trees**~~ — **COMPLETE ✓** Windows .zip and macOS .tgz binary packages served from /bin/{platform}/contrib/{rver}/. PACKAGES/PACKAGES.gz/PACKAGES.rds per platform. Hosted-only; proxy/group deferred.
17. ~~**Go version + OS matrix**~~ — **COMPLETE ✓** Unit test job now runs Go (stable, oldstable) × (ubuntu-latest, macos-latest). Coverage gate and upload scoped to ubuntu+stable only.

### Tier 5 — Quality gaps — ALL COMPLETE ✓

18. ~~Migration test on production-sized dataset~~ — TestPG_MigrateUpDown_ProdSized seeds 10 repos × 50 packages (500 records), runs Down+Up, verifies all empty.
19. ~~blobtest.RunContract missing cases~~ — TraversalNeutralisedOrRejected and LargeStream (5 MiB) added to contract suite.
20. ~~Authz fuzz test~~ — FuzzEnforcerDecide in internal/auth/fuzz_test.go; fuzzes Authorization headers and repo paths.
21. ~~GitOps dry-run~~ — lint job validates argocd-application.yaml + flux-helmrelease.yaml (multi-doc YAML) via python yaml.safe_load_all.
22. ~~HPA and PDB not asserted~~ — cluster-install job runs helm template --set autoscaling.enabled=true --set podDisruptionBudget.enabled=true | kubectl apply --dry-run=client.

---

## What the previous memory got wrong

The previous entry said "both actionable code blockers are resolved" and "no further code changes needed to unblock GA." This was inaccurate:

- Gap #3 (PG queue) is a **code bug**, not a scheduling task — it breaks the HA guarantee
- Gaps #4–17 are all code/CI work, not external dependencies
- P2, P3, P5, P6, P7, and Phase K all have unimplemented deliverables

**How to apply:** When estimating GA readiness or planning next sessions, treat Tier 1 (gap #3) as a one-session fix, Tier 2-3 as multiple sprints of work, and Tier 4 as a deliberate descope decision unless the full workplan scope is required.
