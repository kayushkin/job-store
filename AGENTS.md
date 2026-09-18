# About job-store

## What it owns

`:8311`. The job radar, and deliberately the same shape as event-store: `sources` (where to look), the `listings` they yield, the `documents` you apply with — resume and cover-letter variants, uploads, renders and tailoring requests — and the `applications` made from them, each with its events, emails and tasks. Routes are rooted at `/`, not `/api` (`GET /listings`, `GET /sources`). `CONTRACT.md` is the route table and the schema reference.

## Where this prompt lives

These sections are stored in agent-store as a project prompt collection and rendered, with identical text, to `AGENTS.md` and `CLAUDE.md` at the root of this repo, so that whichever file a harness reads it gets the same thing. Edit them on dash `/files`, or edit either rendered file: the 15-minute scan carries the edit back into the sections and out to the other file. The host prompt keeps one row for this repo with only what an agent elsewhere needs.

# How it works

## Sources, and nothing self-approves

A source has a `kind` — `board`, `research` or `scout` — its own `cadence_hours` and a `status` of `active`, `proposed` or `rejected`. **The service never branches on `kind`**: kind, cadence and model together are the effort tier, and every dispatch rule lives in the dispatcher's prompt (`scripts/job-radar-dispatch.sh`). The store enforces cadence through `next_run_at`, and the dispatcher reports a run with `POST /sources/{id}/ran`. Everything a scout finds must arrive `proposed`, and `PATCH /sources/{id}` is the decision channel. ⚠️ **The store does not do this for the caller**: `store.go` sets a blank status to `active`, so a source created without `"status":"proposed"` is live on the next dispatch. A proposed source is stored disabled and is enabled when it is patched to `active`.

## Role families are served, not observed

`GET /role-families` serves the six canonical families with a description each — Engineering, AI, Data, Product, Design, Leadership (`rolefamily.go`). A UI or an agent builds its filter from that route and never from the values it happens to see on listings.

## Deleting

`DELETE /listings/{id}` is a soft delete, undone by `POST /listings/{id}/restore`; `?hard=true` purges. `POST /listings/prune` sweeps stale candidates. **`DELETE /sources/{id}` is a hard delete.** `GET /listings?q=` is an fts5 index kept in step by triggers on `listings`.

## Application tasks are noteboard todos

`application_tasks` stores a noteboard item id and nothing else — no title, no body, no copy of the status. noteboard owns the content; a copy here would be a second source of truth. `POST /applications/{id}/tasks/standard` creates the standard set. Application emails arrive as *proposed* links, because matching a message to an application is a guess until a person confirms it; `POST /application-emails/{id}/stage` and `PATCH` are how one is confirmed.

# Access and operations

## Who may call it, and what runs against it

No authentication. The unit sets `JOB_STORE_ADDR=:8311`, so it listens on every interface. dash proxies it at `/api/jobs/*` behind dash's login to back its whole **Jobs** page — listings always on screen, with Wanted, Suggestions, Radar, Documents and Applications panels around it. ⚠️ dash's `/api/jobs/*` is this store, **not** the scheduler; the scheduler's own `/api/jobs/` is reached at dash `/api/scheduler/jobs/*`. Unit `job-store.service`, binary `~/bin/job-store`. Two enabled scheduler shell jobs drive it: `job-radar-dispatch` (57, daily 08:30, `scripts/job-radar-dispatch.sh`), which starts a budget-capped Claude session only when a source is due, and `job-tailor-dispatch` (58, `*/5`, `scripts/job-tailor-dispatch.sh`), which works the tailor requests.

# Working in this repo

## Build, test and deploy

Module `github.com/kayushkin/job-store`, root package `jobstore`, server in `cmd/job-store`. SQLite at `~/.config/job-store/job-store.db` (WAL, foreign keys on). ⚠️ **Build and test with `-tags sqlite_fts5`**, or the binary fails at boot with `no such module: fts5`; `deploy.sh` sets it. `scripts/e2e-smoke.sh` is the smoke test and `scripts/seed-sources.sh` seeds sources. The Jobs page has six Playwright specs in dash, `e2e/jobs-*.spec.ts`, sharing `e2e/jobsFixtures.ts`. Pushes to `github.com/kayushkin/job-store` (public).
