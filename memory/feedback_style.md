---
name: feedback-style
description: Working style preferences and corrections observed across sessions
metadata:
  type: feedback
---

## State the plan, wait for confirmation on large changes

For changes over ~30 lines or multiple files, state the plan first and wait for "go ahead" before writing code. User said "go ahead with BE-D" explicitly — they read the plan and approved it before execution started.

**Why:** Avoids rework if the approach is wrong. User invests attention in the plan step.
**How to apply:** One paragraph plan + file list + key design decisions. No essay. Wait for green light.

---

## Commit frequently, small units

Per CLAUDE.md: commit after each self-contained unit. Don't batch a whole phase into one commit.

**Why:** Explicit project convention.
**How to apply:** One commit per logical unit (one feature, one fix, one test suite).

---

## No trailing summaries

Don't end responses with "Here's what I did" recap paragraphs. State results inline or in one closing sentence.

**Why:** User can read the diff.
**How to apply:** End with what's next, not what just happened.

---

## Scope discipline

Don't add error handling, cleanup, or abstractions beyond what the task requires. When BE-D was defined, it was implemented exactly as specified — no extra fields, no "while I'm here" cleanups.

**Why:** User is pragmatic; scope creep wastes time and creates noise in PRs.
**How to apply:** Read the task spec literally. If something adjacent is broken, mention it; don't fix it silently.
