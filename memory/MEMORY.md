# Memory Index

- [User profile](user_profile.md) — senior engineer, Go-first, pragmatic about scope
- [Feedback & working style](feedback_style.md) — key preferences and corrections from past sessions
- [Phase progress](project_phase_progress.md) — all core phases code-complete (P0–P7, Phase K). GA blockers: pen test (unscheduled) + 24h soak run.
- [UI workplan](project_ui_workplan.md) — Foundry redesign. F0–F3 + BE-A/B/C/D done. Next: BE-E/F (parallel) + BE-G → F4/F2. WORKPLAN.md §10 is authoritative.
- [Design sweep](project_design_sweep.md) — per-page "instrument-panel" signature pass. COMPLETE (Waves 1–3): all pages on the readout voice, dead .kpi/.stat CSS swept, fake filter boxes trimmed.
- [Admin auth gotcha](project_admin_auth_gotcha.md) — admin UI uses HttpOnly forge_token cookie; browser-called /api/v1 endpoints need a cookie-aware guard, not Bearer-only.
- [Next steps](project_next_steps.md) — roadmap after the UI sweep: (1) finish OIDC SSO (~80% built), (2) proxy singleflight, (3) scaling design spike. Not started.
