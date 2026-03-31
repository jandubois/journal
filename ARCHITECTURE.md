# Architecture

## Three-layer design

Journal separates collection, processing, and reporting into distinct layers. Each layer reads from the previous layer's output and writes its own.

```
Sources                   Database                    Output
───────                   ────────                    ──────
GitHub Events API ──┐
                    ├──→ observations ──→ events ──→ report
Local git repos ────┤       (raw)       (grouped)   (text/md)
Claude sessions ────┘
```

### Layer 1: Collect

Collectors fetch data from external sources and write raw observations to the database. They store original, uninterpreted data: the actual GitHub repo slug (not fork-resolved), the raw event type and action (not a derived activity kind), and no work/personal classification. Each observation is keyed by `(source, source_id, repo)` and deduplicated on insert.

Collectors run broadly — not limited to the report's `--since` range — to capture data before it expires. The GitHub Events API retains 90 days; Claude Code sessions expire after roughly 30 days.

Session JSONL files are copied to `~/.local/share/journal/sessions/` as a full archive, since the observation metadata alone (timestamps, prompts) is not sufficient for future AI summarization.

### Layer 2: Process

The processing step reads observations for the requested time range, interprets them into activities, groups related activities into topics, and writes the result to the `events` table.

Interpretation happens here, not in collectors:

- **Fork resolution**: `jandubois/lima` → `lima-vm/lima` (via GitHub API, cached per run)
- **Work classification**: check repo org against `workOrgs` config
- **PR state enrichment**: fetch current PR title, merged status
- **Filtering**: skip pushes to default branches, branch deletes
- **Next-step inference**: determine what action follows each topic

Events link back to their source observations through the `event_observations` junction table, enabling drill-down from a high-level summary to individual commits, reviews, and sessions.

### Layer 3: Report

The report layer renders topics as text or markdown, split into work and personal sections. Done topics (merged PRs, closed issues) render as compact one-liners. Active topics show a chronological activity list with timestamps and an inferred next step.

The report currently renders from in-memory Topics built during processing. A future change could render directly from the `events` table.

## Database schema

```sql
observations
├── id            INTEGER PRIMARY KEY
├── source        TEXT        -- "github", "git", "session"
├── source_id     TEXT        -- event ID, commit hash, session UUID
├── timestamp     TEXT        -- ISO 8601
├── repo          TEXT        -- raw repo slug from the source
├── data          TEXT        -- JSON with source-specific fields
├── collected_at  TEXT
└── UNIQUE(source, source_id, repo)

events
├── id               INTEGER PRIMARY KEY
├── timestamp        TEXT
├── duration_seconds INTEGER
├── repo             TEXT     -- fork-resolved repo slug
├── number           INTEGER  -- PR/issue number, 0 for repo-level
├── title            TEXT
├── url              TEXT
├── kind             TEXT     -- "reviewed_and_merged", "opened_pr", "development", ...
├── summary          TEXT     -- one-line description
├── work             INTEGER
├── next_step        TEXT
└── processed_at     TEXT

event_observations
├── event_id       INTEGER  →  events.id
└── observation_id INTEGER  →  observations.id
```

### Observation data by source

Each observation's `data` column holds a JSON object with source-specific fields.

**GitHub event:**
```json
{
  "event_type": "PullRequestReviewEvent",
  "action": "submitted",
  "number": 10071,
  "title": "bump certManager from 1.20.0 to 1.20.1",
  "url": "https://github.com/.../pull/10071",
  "details": "approved",
  "is_author": false
}
```

**Git commit:**
```json
{
  "hash": "a8f24d2...",
  "message": "Add journal tool for standup activity reports"
}
```

**Claude session:**
```json
{
  "session_id": "7b710492-...",
  "project_dir": "-Users-jan-git-journal",
  "project_name": "journal",
  "duration_seconds": 2700,
  "prompts": ["Design activity report tool", "Fix session duration bug"],
  "archive_path": "/Users/jan/.local/share/journal/sessions/journal/7b710492-....jsonl"
}
```

Subagent sessions include `parent_session_id` and `agent_id` fields linking them to their parent session.

## File layout

```
main.go        CLI entry point, orchestration (collect → process → report)
config.go      Load git-lint config, work/personal classification helpers
db.go          Database operations, schema, observation↔activity conversion
github.go      GitHub Events API collector
git.go         Local git repo scanner
sessions.go    Claude Code session scanner and archiver
activity.go    Activity and Topic types, grouping logic
nextstep.go    Next-step inference rules
process.go     Process observations into events, query events
report.go      Text and Markdown renderers
exec.go        Shell command helper
```

## Data flow detail

```
main()
  ├─ Parse flags
  ├─ Load config from ~/.config/git-lint/config.json
  ├─ Open database (~/.local/share/journal/journal.db)
  │
  ├─ Collect
  │  ├─ fetchGitHubEvents()     → observations table
  │  ├─ scanGitRepos()          → observations table
  │  └─ scanSessions()          → observations table + file archive
  │
  ├─ Process
  │  ├─ queryObservations(since) → []Observation
  │  ├─ observationsToActivities() — fork resolution, work classification, PR enrichment
  │  ├─ groupActivities()       — group by (repo, number)
  │  ├─ inferNextSteps()        — derive next action per topic
  │  └─ processEvents()         → events + event_observations tables
  │
  └─ Report
     └─ renderText() or renderMarkdown() → stdout
```

## Key design principles

**Raw observations, interpreted activities.** Collectors store exactly what the source provided. Fork resolution, work classification, and PR merge detection happen when observations are converted to activities. If the config changes (new work org added), re-running the tool applies the new classification to all stored observations.

**Idempotent collection.** Running journal multiple times produces no duplicate observations. The `UNIQUE(source, source_id, repo)` constraint with `INSERT OR IGNORE` ensures this.

**Archive before expiration.** Claude Code session files are copied to a local archive during collection. Session metadata in the observation (timestamps, prompts) supports basic reporting; the full archived JSONL supports future AI summarization.

## Planned extensions

**AI summarization (Phase 4).** Replace mechanical event summaries ("10 commits, 4 sessions") with AI-generated descriptions ("Designed database architecture for the journal tool"). The archived session files and full prompts in observation metadata provide the context for this.

**Multi-host collection.** The tool will eventually run as a service on a NAS, collecting observations from multiple computers. The schema supports this with a future `host` column on the observations table. Source IDs are content-based (commit hashes, GitHub event IDs, session UUIDs), so they remain unique across hosts.

**Schema migrations.** The current schema versioning deletes and recreates the database on version changes. This will be replaced with proper ALTER TABLE migrations once the data becomes long-lived.
