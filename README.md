# journal

A standup report tool that gathers your activity from GitHub, local git repos, and Claude Code sessions, then produces a grouped summary split by work and personal projects.

## What it does

Journal collects raw observations from three sources, stores them in a local SQLite database, groups related activities by topic (PR, issue, or repo), classifies them as work or personal, infers the next step for each topic, and renders a report.

```
=== Work ===

## acme-corp/widget-server

PR #482: Fix rate limiter for bursty traffic
  - 09:14  Opened PR
  → Awaiting review

PR #479: bump redis from 7.2.0 to 7.2.1 — Reviewed and merged
PR #480: bump express from 4.19.0 to 4.19.2 — Reviewed and merged

#301: Timeout on large file uploads
  - 10:45  Commented
  → Monitor

  - 11:20  Add retry logic for S3 multipart uploads
Claude sessions:
  - 09:02  (45m) Fix the rate limiter to use a sliding window instead of fixed buckets

=== Personal ===

## dotfiles
  - 20:15  Update tmux config for nested sessions
  → Push
```

## Installation

```sh
go install github.com/jandubois/journal@latest
```

Requires the [GitHub CLI](https://cli.github.com/) (`gh`) to be installed and authenticated.

## Usage

```sh
journal                        # today's activity
journal --since 2d             # last 2 days
journal --since 1w             # last week
journal --since 2026-03-28     # since a specific date
journal --format markdown      # output as markdown with links
```

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--since` | `1d` | How far back to look. Accepts `Nd`, `Nw`, or an ISO date. |
| `--format` | `text` | Output format: `text` or `markdown`. |
| `--repos` | `~/git` | Directory containing local git repos to scan. |
| `--user` | *(auto-detected)* | GitHub username. Detected from `gh api user` if omitted. |

## Data sources

- **GitHub Events API** — PRs, reviews, comments, issues, pushes. Paginated until empty.
- **Local git repos** — Commits by the configured author in repos under `--repos`.
- **Claude Code sessions** — Session metadata and prompts from `~/.claude/projects/`. Session JSONL files are archived to `~/.local/share/journal/sessions/` before they expire.

## Work/personal classification

Journal reads `~/.config/git-lint/config.json` (shared with [git-lint](https://github.com/jandubois/git-lint)) for the `workOrgs` list and `identity.workEmail`. A repo is classified as work if its GitHub org matches a work org or if the repo is a fork of a work org's repo. Everything else is personal.

## Database

Observations are stored in `~/.local/share/journal/journal.db` (SQLite). This preserves data beyond the GitHub API's 90-day / 300-event limit and beyond Claude Code's session expiration window. Running journal again adds new observations without duplicating existing ones.

See [ARCHITECTURE.md](ARCHITECTURE.md) for the database schema and three-layer design.

## License

[Apache License 2.0](LICENSE)
