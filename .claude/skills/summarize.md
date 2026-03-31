# Summarize journal events

Generate AI summaries for journal events that lack them.

## When to use

Invoke with `/summarize` when the user wants to enrich their journal report with AI-generated descriptions. The skill processes events in batches, most recent first.

## How it works

1. Run `./journal pending --since 1w --limit 10` to get events needing summaries
2. For each event, run `./journal observations --event <id>` to get the raw observations
3. For session observations with an `archive_path`, read excerpts from the archived JSONL to understand what was done
4. Generate a concise, one-line summary that describes *what was accomplished*, not just *what happened*
5. Store each summary with `./journal set-summary --repo <repo> --number <number> [--period <date>] <summary>`
6. Report how many events were summarized and how many remain

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

Process one day at a time, most recent first. After each batch:
- Report how many summaries were generated
- Report how many events still need summaries
- Ask the user if they want to continue with the next batch
