---
name: summarize
description: Generate AI summaries for journal events that lack them. Invoke with /summarize or /summarize 2w.
argument-hint: [time-range]
---

# Summarize journal events

Generate AI summaries for journal events that lack them.

## When to use

Invoke with `/summarize` or `/summarize 2w` to enrich journal reports with AI-generated descriptions. The argument sets the time range (default: `1w`). The skill processes events in batches, most recent first.

## How it works

1. Run `./journal pending --since <range> --limit 30` to get events needing summaries. If the result shows 0 events, the user may need to run `journal collect` first, or generate a report with `journal --since <range>` to populate the events table.
3. For each event, run `./journal observations --event <id>` to get the raw observations.
4. For session observations with an `archive_path`, read excerpts from the archived JSONL to understand what was done.
5. Generate a concise, one-line summary that describes *what was accomplished*, not just *what happened*.
6. Store each summary with `./journal set-summary --repo <repo> --number <number> [--period <date>] <summary>`.
7. After processing, report progress: "Summaries for the last N days complete, M events pending."

## Summary style guide

- One sentence, active voice, present tense for completed actions
- Focus on the outcome, not the process: "Bump certManager to 1.20.1" not "Reviewed and merged a PR that bumps certManager"
- For development topics: describe what was built or changed, not individual commits
- For reviews: mention what was reviewed and the outcome
- For issue comments: summarize the substance of the discussion
- Keep summaries under 100 characters when possible

## Example summaries

- "Bump certManager to 1.20.1"
- "Fix docker proxy response handling for slow clients"
- "Design and implement standup report tool with SQLite backend"
- "Approve 6 dependabot dependency updates"
- "Discuss maintainer promotion process for lima project"

## Reading session archives

When an observation has `archive_path`, you can read the JSONL file to understand what happened in a Claude session. Focus on:
- User prompts (lines with `"type":"user"`)
- The first user prompt gives the session's goal
- Look for patterns: planning, debugging, code review, implementation

Keep archive reads brief — scan the first few user messages, don't read the entire file.

## Batch processing

Process one day at a time, most recent first. After completing a batch:
- Report how many summaries were generated in this batch
- Report how many events still need summaries (from the `total_pending` field in the pending output)
- Format as: "Summaries for the last N days complete, M events pending."
- Ask the user if they want to continue with the next batch
