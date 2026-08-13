#!/usr/bin/env bash
# Seed job-store (:8311) with a starter set of PROPOSED sources, so the
# Suggestions panel is not empty on day one.
#
# Every source here is posted with status "proposed" and is therefore INERT by
# construction: UpsertSource sets enabled = (status != "proposed") on insert, and
# ListSources folds `status='active' AND enabled=1 AND next_run_at<=?` into one
# inseparable clause, so nothing below can be fetched or run until a human
# approves it. Nothing self-approves — the same rule the events radar runs under
# in ~/AGENTS.md, and it applies identically here. Do NOT add `"status":"active"`
# to a row in this file.
#
# Re-running this is safe: POST /sources upserts by name and its UPDATE path omits
# status, enabled, next_run_at and proposed_by entirely, so a second run refreshes
# targets and notes and can never resurrect something already rejected.
#
# ── Verification, 2026-08-13 ────────────────────────────────────────────────────
# Every board below was checked live on 2026-08-13 by calling its ATS JSON API and
# counting the openings it returned. Open-role counts on that date, in the order
# they appear below:
#   Greenhouse (boards-api.greenhouse.io/v1/boards/<token>/jobs):
#     anthropic 420 · databricks 807 · vercel 83 · temporaltechnologies 55 ·
#     grafanalabs 147 · sourcegraph91 8 · cloudflare 305 · gitlab 201
#   Ashby (api.ashbyhq.com/posting-api/job-board/<token>):
#     openai 731 · modal 30 · baseten 70 · anyscale 16 · langchain 104 ·
#     clickhouse 179 · supabase 55 · render 35 · cursor 114 · warp 16 ·
#     railway 8 · pinecone 8 · docker 57
#   Lever (api.lever.co/v0/postings/<token>?mode=json): shieldai 434
#   Workable (apply.workable.com/api/v1/widget/accounts/<token>?details=true):
#     huggingface 7
#
# DROPPED after checking — every one of these looks plausible and is not a source:
#   greenhouse/hashicorp, greenhouse/pinecone, greenhouse/docker,
#   greenhouse/weightsandbiases  → HTTP 200 with ZERO jobs (moved ATS; Pinecone and
#     Docker are on Ashby, and are seeded below under their Ashby tokens)
#   ashby/replicate, ashby/huggingface, ashby/groq, ashby/together, ashby/fal,
#   ashby/deno, ashby/hashicorp  → 404, wrong or retired token
#   ashby/astral, ashby/vercel  → 200 with zero openings (Vercel is on Greenhouse,
#     seeded below under that token)
#   lever/anyscale  → HTTP 200 and ONE "posting" whose title is
#     "We have moved our Careers Page to: https://jobs.ashbyhq.com/anyscale".
#     A 200 with rows in it is not proof of a live board — read the rows. Anyscale
#     is seeded below under its Ashby token.
#   lever/anduril, lever/brex, lever/attentive, lever/kandji, lever/sardine,
#   lever/scaleai, lever/circleci, lever/sentry, lever/postman, lever/harness,
#   lever/airbyte, lever/dagster, lever/prefect  → 404
#   lever/plaid, lever/tecton, lever/mistral, lever/voleon  → 200, zero postings
# Re-verify before trusting these counts: a board token dies quietly, and the
# poller cannot tell an empty board from a wrong token unless someone looks.
set -euo pipefail

STORE="${JOB_STORE_URL:-http://localhost:8311}"
PROPOSER="${SEED_PROPOSED_BY:-seed-sources.sh}"

if ! curl -sfS "$STORE/health" >/dev/null; then
  echo "seed-sources: cannot reach job-store at $STORE — nothing seeded" >&2
  exit 1
fi

propose() { # $1 = JSON object WITHOUT status/proposed_by; those are forced here
  local body
  body=$(printf '%s' "$1" | SEED_PROPOSED_BY="$PROPOSER" python3 -c '
import json, os, sys
source = json.load(sys.stdin)
# Forced here rather than trusted per-row, so a copy-paste of an existing row can
# never smuggle an approved source into the seed set.
for owned in ("status", "enabled", "proposed_by"):
    source.pop(owned, None)
source["status"] = "proposed"
source["proposed_by"] = os.environ["SEED_PROPOSED_BY"]
print(json.dumps(source))
')
  local out name status
  out=$(curl -sfS -X POST "$STORE/sources" -H 'Content-Type: application/json' -d "$body")
  read -r name status <<<"$(printf '%s' "$out" | python3 -c '
import json, sys
source = json.load(sys.stdin)
print(source.get("name"), source.get("status"))
')"
  # A row that comes back as anything but "proposed" was already in the store with
  # a decision on it — report that instead of hiding it.
  printf '    %-24s %s\n' "$name" "$status"
}

echo "==> seeding proposed sources into $STORE"

echo "-- board sources: AI infrastructure"
propose '{"name":"anthropic-board","kind":"board","target":"https://boards.greenhouse.io/anthropic","company":"Anthropic","location":"Remote (US) / San Francisco","role_families":["Engineering","AI"],"seniorities":["senior","staff","principal"],"keywords":[],"cadence_hours":24,"notes":"Greenhouse. Poll boards-api.greenhouse.io/v1/boards/anthropic/jobs?content=true — 420 open roles on 2026-08-13. Large board, so filter hard on role_families/seniorities; most rows are not engineering."}'
propose '{"name":"openai-board","kind":"board","target":"https://jobs.ashbyhq.com/openai","company":"OpenAI","location":"Remote (US) / San Francisco","role_families":["Engineering","AI"],"seniorities":["senior","staff","principal"],"keywords":[],"cadence_hours":24,"notes":"Ashby. Poll api.ashbyhq.com/posting-api/job-board/openai (add ?includeCompensation=true for bands) — 731 open roles on 2026-08-13. Verify remote eligibility per posting; many are SF-onsite despite the location string."}'
propose '{"name":"modal-board","kind":"board","target":"https://jobs.ashbyhq.com/modal","company":"Modal Labs","location":"Remote (US) / New York","role_families":["Engineering","AI"],"seniorities":["senior","staff"],"keywords":[],"cadence_hours":72,"notes":"Ashby, 30 open roles on 2026-08-13. Serverless GPU/inference infrastructure. Small board that turns over slowly — 72h is enough."}'
propose '{"name":"baseten-board","kind":"board","target":"https://jobs.ashbyhq.com/baseten","company":"Baseten","location":"Remote (US) / San Francisco","role_families":["Engineering","AI"],"seniorities":["senior","staff"],"keywords":[],"cadence_hours":72,"notes":"Ashby, 70 open roles on 2026-08-13. Model inference/deployment platform."}'
propose '{"name":"anyscale-board","kind":"board","target":"https://jobs.ashbyhq.com/anyscale","company":"Anyscale","location":"Remote (US) / San Francisco","role_families":["Engineering","AI","Data"],"seniorities":["senior","staff"],"keywords":["ray","distributed"],"cadence_hours":72,"notes":"Ashby, 16 open roles on 2026-08-13. Ray / distributed compute. NOTE: the old Lever board (api.lever.co/v0/postings/anyscale) still answers 200 with a single posting that is just a redirect notice — do not poll it."}'
propose '{"name":"langchain-board","kind":"board","target":"https://jobs.ashbyhq.com/langchain","company":"LangChain","location":"Remote (US) / San Francisco","role_families":["Engineering","AI"],"seniorities":["senior","staff"],"keywords":["agents","llm"],"cadence_hours":72,"notes":"Ashby, 104 open roles on 2026-08-13. Agent framework + LangSmith observability."}'
propose '{"name":"pinecone-board","kind":"board","target":"https://jobs.ashbyhq.com/pinecone","company":"Pinecone","location":"Remote (US) / New York","role_families":["Engineering","AI","Data"],"seniorities":["senior","staff"],"keywords":["vector","retrieval"],"cadence_hours":168,"notes":"Ashby, 8 open roles on 2026-08-13 — small and slow, hence the weekly cadence. The Greenhouse token boards.greenhouse.io/pinecone is dead (200, zero jobs); do not poll it."}'
propose '{"name":"scale-ai-board","kind":"board","target":"https://boards.greenhouse.io/scaleai","company":"Scale AI","location":"Remote (US) / San Francisco","role_families":["Engineering","AI","Data"],"seniorities":["senior","staff"],"keywords":[],"cadence_hours":24,"notes":"Greenhouse, 210 open roles on 2026-08-13. Heavy on operations and GTM roles — filter to Engineering/AI or it floods the list."}'
propose '{"name":"databricks-board","kind":"board","target":"https://boards.greenhouse.io/databricks","company":"Databricks","location":"Remote (US) / San Francisco","role_families":["Engineering","AI","Data"],"seniorities":["senior","staff","principal"],"keywords":[],"cadence_hours":24,"notes":"Greenhouse, 807 open roles on 2026-08-13 — the largest board in the seed set. Filter aggressively; expect most rows to be sales or non-US."}'
propose '{"name":"hugging-face-board","kind":"board","target":"https://apply.workable.com/huggingface","company":"Hugging Face","location":"Remote (Global)","role_families":["Engineering","AI"],"seniorities":["senior","staff"],"keywords":[],"cadence_hours":168,"notes":"Workable, 7 open roles on 2026-08-13. Poll apply.workable.com/api/v1/widget/accounts/huggingface?details=true — the widget API is the only reliable read; the rendered page is client-side. Mostly remote-global."}'

echo "-- board sources: developer tooling and data infrastructure"
propose '{"name":"clickhouse-board","kind":"board","target":"https://jobs.ashbyhq.com/clickhouse","company":"ClickHouse","location":"Remote (US)","role_families":["Engineering","Data"],"seniorities":["senior","staff","principal"],"keywords":[],"cadence_hours":24,"notes":"Ashby, 179 open roles on 2026-08-13. Remote-first company, so the remote flag on these is usually real — still read the posting for region locks."}'
propose '{"name":"supabase-board","kind":"board","target":"https://jobs.ashbyhq.com/supabase","company":"Supabase","location":"Remote (Global)","role_families":["Engineering"],"seniorities":["senior","staff"],"keywords":["postgres","go","typescript"],"cadence_hours":72,"notes":"Ashby, 55 open roles on 2026-08-13. Fully remote company."}'
propose '{"name":"vercel-board","kind":"board","target":"https://boards.greenhouse.io/vercel","company":"Vercel","location":"Remote (US)","role_families":["Engineering"],"seniorities":["senior","staff"],"keywords":[],"cadence_hours":72,"notes":"Greenhouse, 83 open roles on 2026-08-13. The Ashby token jobs.ashbyhq.com/vercel answers 200 with zero jobs — Greenhouse is the live one."}'
propose '{"name":"temporal-board","kind":"board","target":"https://boards.greenhouse.io/temporaltechnologies","company":"Temporal Technologies","location":"Remote (US)","role_families":["Engineering"],"seniorities":["senior","staff","principal"],"keywords":["distributed","workflow","go"],"cadence_hours":72,"notes":"Greenhouse, 55 open roles on 2026-08-13. Board token is temporaltechnologies, not temporal."}'
propose '{"name":"grafana-labs-board","kind":"board","target":"https://boards.greenhouse.io/grafanalabs","company":"Grafana Labs","location":"Remote (US)","role_families":["Engineering"],"seniorities":["senior","staff","principal"],"keywords":["observability","go"],"cadence_hours":72,"notes":"Greenhouse, 147 open roles on 2026-08-13. Remote-first; postings name the eligible countries explicitly, so read them rather than trusting the badge."}'
propose '{"name":"render-board","kind":"board","target":"https://jobs.ashbyhq.com/render","company":"Render","location":"Remote (US)","role_families":["Engineering"],"seniorities":["senior","staff"],"keywords":[],"cadence_hours":72,"notes":"Ashby, 35 open roles on 2026-08-13. Application hosting / platform-as-a-service."}'
propose '{"name":"docker-board","kind":"board","target":"https://jobs.ashbyhq.com/docker","company":"Docker","location":"Remote (US)","role_families":["Engineering"],"seniorities":["senior","staff"],"keywords":[],"cadence_hours":72,"notes":"Ashby, 57 open roles on 2026-08-13. The Greenhouse token is dead (200, zero jobs) — Ashby is the live board."}'
propose '{"name":"sourcegraph-board","kind":"board","target":"https://boards.greenhouse.io/sourcegraph91","company":"Sourcegraph","location":"Remote (US)","role_families":["Engineering","AI"],"seniorities":["senior","staff"],"keywords":["code search","go"],"cadence_hours":168,"notes":"Greenhouse, 8 open roles on 2026-08-13. The board token really is sourcegraph91 — boards.greenhouse.io/sourcegraph is not it."}'
propose '{"name":"cursor-board","kind":"board","target":"https://jobs.ashbyhq.com/cursor","company":"Cursor (Anysphere)","location":"San Francisco / Remote (US)","role_families":["Engineering","AI"],"seniorities":["senior","staff"],"keywords":[],"cadence_hours":72,"notes":"Ashby, 114 open roles on 2026-08-13. Heavily SF-onsite — check each posting before trusting a remote label."}'
propose '{"name":"warp-board","kind":"board","target":"https://jobs.ashbyhq.com/warp","company":"Warp","location":"Remote (US) / New York","role_families":["Engineering","AI"],"seniorities":["senior","staff"],"keywords":["terminal","rust"],"cadence_hours":168,"notes":"Ashby, 16 open roles on 2026-08-13. Small board, weekly cadence is plenty."}'
propose '{"name":"railway-board","kind":"board","target":"https://jobs.ashbyhq.com/railway","company":"Railway","location":"Remote (Global)","role_families":["Engineering"],"seniorities":["senior","staff"],"keywords":["infrastructure"],"cadence_hours":168,"notes":"Ashby, 8 open roles on 2026-08-13. Fully remote, very small board."}'
propose '{"name":"cloudflare-board","kind":"board","target":"https://boards.greenhouse.io/cloudflare","company":"Cloudflare","location":"Remote (US) / Austin","role_families":["Engineering","AI"],"seniorities":["senior","staff","principal"],"keywords":["edge","workers","rust","go"],"cadence_hours":24,"notes":"Greenhouse, 305 open roles on 2026-08-13. Large and global — the location filter does most of the work here."}'
propose '{"name":"gitlab-board","kind":"board","target":"https://boards.greenhouse.io/gitlab","company":"GitLab","location":"Remote (US)","role_families":["Engineering"],"seniorities":["senior","staff","principal"],"keywords":["go","ruby","ci"],"cadence_hours":72,"notes":"Greenhouse, 201 open roles on 2026-08-13. All-remote company; postings state the eligible countries."}'
propose '{"name":"shield-ai-board","kind":"board","target":"https://jobs.lever.co/shieldai","company":"Shield AI","location":"Remote (US) / San Diego","role_families":["Engineering","AI"],"seniorities":["senior","staff","principal"],"keywords":["autonomy","c++","python"],"cadence_hours":72,"notes":"Lever — the one Lever board in this seed set, kept partly to exercise the Lever path. Poll api.lever.co/v0/postings/shieldai?mode=json (createdAt is epoch MILLIseconds). 434 open roles on 2026-08-13, but the majority are hardware/aerospace and many need a US security clearance — filter to software autonomy and read the clearance line into the description."}'

echo "-- research sources: the roles, not the companies"
propose '{"name":"remote-staff-backend-research","kind":"research","target":"Senior/Staff/Principal backend engineer roles at US companies, Go or Rust, distributed systems, APIs and data infrastructure, fully remote","company":"","location":"Remote (US)","role_families":["Engineering"],"seniorities":["senior","staff","principal"],"keywords":["remote","backend"],"cadence_hours":48,"model":"sonnet","notes":"Follow every hit through to the real posting — a url pointing at a LinkedIn/Indeed search page is worse than no listing, since the sig is taken by something nobody can apply to. Prefer postings that publish a compensation band."}'
propose '{"name":"remote-ai-platform-research","kind":"research","target":"Staff/Principal AI platform, LLM infrastructure, inference and agent-tooling engineering roles at US companies, fully remote","company":"","location":"Remote (US)","role_families":["AI","Engineering"],"seniorities":["staff","principal"],"keywords":["llm","inference","agents","platform"],"cadence_hours":48,"model":"sonnet","notes":"Aimed at the seam between backend and AI infra — model serving, eval harnesses, agent orchestration — not research-scientist roles. Skip anything demanding a PhD or publications."}'
propose '{"name":"remote-devtools-platform-research","kind":"research","target":"Senior/Staff platform and developer-experience engineering roles at developer-tooling companies, fully remote in the US","company":"","location":"Remote (US)","role_families":["Engineering"],"seniorities":["senior","staff"],"keywords":["developer tools","platform","kubernetes","ci"],"cadence_hours":72,"model":"sonnet","notes":"Deliberately covers the companies too small to be worth their own board source. If a company here shows up repeatedly, propose it as a board source instead — one fetch beats a search sweep."}'

echo "-- scout: grows the board list"
propose '{"name":"ai-infra-board-scout","kind":"scout","target":"AI-infrastructure, agent-tooling and developer-tooling companies (roughly seed through series C) with a public ATS board — Greenhouse, Lever, Ashby or Workable — that are hiring senior or staff backend / AI-platform engineers remote in the US","company":"","location":"Remote (US)","role_families":["Engineering","AI"],"seniorities":["senior","staff","principal"],"keywords":[],"cadence_hours":168,"model":"sonnet","notes":"Posts NO listings. Reads GET /sources with no filter first (every status, rejected included) so it cannot re-propose something already turned down. VERIFY each candidate through its ATS JSON API and record the open-role count in the notes before proposing — a board can answer 200 and still be dead (Anyscale on Lever returns a single posting that is only a redirect notice). Never propose a LinkedIn or Indeed search URL as a board. Three or four good proposals per run; each approval is a recurring cost forever."}'

echo "==> done — every row above is proposed and inert until approved in the Suggestions panel"
