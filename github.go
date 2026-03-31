package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// fetchGitHubEvents retrieves the user's recent GitHub events and converts them to activities.
func fetchGitHubEvents(user string, since time.Time, cfg *Config) ([]Activity, error) {
	var allEvents []ghEvent
	for page := 1; page <= 3; page++ {
		out, err := runCommand("gh", "api",
			fmt.Sprintf("/users/%s/events?per_page=100&page=%d", user, page),
		)
		if err != nil {
			if page == 1 {
				return nil, err
			}
			break
		}

		var events []ghEvent
		if err := json.Unmarshal([]byte(out), &events); err != nil {
			return nil, fmt.Errorf("parse events page %d: %w", page, err)
		}
		if len(events) == 0 {
			break
		}
		allEvents = append(allEvents, events...)

		// Stop paginating if the oldest event on this page is before our cutoff.
		oldest := events[len(events)-1].CreatedAt
		if oldest.Before(since) {
			break
		}
	}

	var activities []Activity
	titleCache := make(map[string]string) // "owner/repo#123" -> title
	forkCache := make(map[string]string)  // "owner/repo" -> parent slug or ""

	for _, e := range allEvents {
		if e.CreatedAt.Before(since) {
			continue
		}
		acts := convertEvent(e, cfg, user, forkCache)
		for i := range acts {
			// Fetch missing titles.
			if acts[i].Number > 0 && acts[i].Title == "" {
				acts[i].Title = fetchTitle(acts[i].Repo, acts[i].Number, titleCache)
			}
		}
		activities = append(activities, acts...)
	}

	return activities, nil
}

func convertEvent(e ghEvent, cfg *Config, user string, forkCache map[string]string) []Activity {
	repo, work := resolveGitHubRepo(e.Repo.Name, user, cfg, forkCache)
	base := Activity{
		Time: e.CreatedAt,
		Repo: repo,
		Work: work,
	}

	switch e.Type {
	case "PullRequestEvent":
		a := base
		a.Number = e.Payload.Number
		a.URL = e.Payload.PullRequest.URL
		a.Title = e.Payload.PullRequest.Title
		a.IsAuthor = true
		switch e.Payload.Action {
		case "opened":
			a.Kind = "pr_opened"
		case "closed":
			if e.Payload.PullRequest.Merged {
				a.Kind = "pr_merged"
			} else {
				a.Kind = "pr_closed"
			}
		case "reopened":
			a.Kind = "pr_reopened"
		default:
			return nil
		}
		return []Activity{a}

	case "PullRequestReviewEvent":
		a := base
		a.Number = e.Payload.PullRequest.Number
		a.URL = e.Payload.PullRequest.URL
		a.Title = e.Payload.PullRequest.Title
		a.Kind = "pr_reviewed"
		a.Details = e.Payload.Review.State
		a.IsAuthor = false
		return []Activity{a}

	case "IssueCommentEvent":
		a := base
		a.Number = e.Payload.Issue.Number
		a.URL = e.Payload.Issue.URL
		a.Title = e.Payload.Issue.Title
		if e.Payload.Issue.PullRequest != nil {
			a.Kind = "pr_commented"
		} else {
			a.Kind = "issue_commented"
		}
		body := e.Payload.Comment.Body
		if idx := strings.IndexByte(body, '\n'); idx > 0 {
			body = body[:idx]
		}
		if len(body) > 120 {
			body = body[:120] + "..."
		}
		a.Details = body
		return []Activity{a}

	case "IssuesEvent":
		a := base
		a.Number = e.Payload.Issue.Number
		a.URL = e.Payload.Issue.URL
		a.Title = e.Payload.Issue.Title
		a.IsAuthor = true
		switch e.Payload.Action {
		case "opened":
			a.Kind = "issue_opened"
		case "closed":
			a.Kind = "issue_closed"
		case "reopened":
			a.Kind = "issue_reopened"
		default:
			return nil
		}
		return []Activity{a}

	case "PushEvent":
		a := base
		a.Kind = "pushed"
		ref := e.Payload.Ref
		if strings.HasPrefix(ref, "refs/heads/") {
			ref = ref[len("refs/heads/"):]
		}
		commits := make([]string, 0, len(e.Payload.Commits))
		for _, c := range e.Payload.Commits {
			msg := c.Message
			if idx := strings.IndexByte(msg, '\n'); idx > 0 {
				msg = msg[:idx]
			}
			commits = append(commits, msg)
		}
		if len(commits) == 0 {
			a.Details = fmt.Sprintf("pushed to %s", ref)
		} else if len(commits) == 1 {
			a.Details = fmt.Sprintf("pushed to %s: %s", ref, commits[0])
		} else {
			a.Details = fmt.Sprintf("pushed %d commits to %s", len(commits), ref)
		}
		a.IsAuthor = true
		return []Activity{a}

	case "CreateEvent":
		a := base
		switch e.Payload.RefType {
		case "branch":
			a.Kind = "branch_created"
			a.Details = e.Payload.Ref
		case "tag":
			a.Kind = "tag_created"
			a.Details = e.Payload.Ref
		default:
			return nil
		}
		a.IsAuthor = true
		return []Activity{a}

	case "DeleteEvent":
		a := base
		a.Kind = "branch_deleted"
		a.Details = e.Payload.Ref
		a.IsAuthor = true
		return []Activity{a}
	}

	return nil
}

func fetchTitle(repo string, number int, cache map[string]string) string {
	key := fmt.Sprintf("%s#%d", repo, number)
	if title, ok := cache[key]; ok {
		return title
	}

	// Try as PR first, fall back to issue.
	out, err := runCommand("gh", "api",
		fmt.Sprintf("/repos/%s/pulls/%d", repo, number),
		"--jq", ".title",
	)
	if err != nil {
		out, err = runCommand("gh", "api",
			fmt.Sprintf("/repos/%s/issues/%d", repo, number),
			"--jq", ".title",
		)
	}
	title := strings.TrimSpace(out)
	if err != nil {
		title = ""
	}
	cache[key] = title
	return title
}

// resolveGitHubRepo maps a repo slug to its canonical name and work classification.
// Forks of work repos are remapped to the parent slug so activities group together.
func resolveGitHubRepo(repo, user string, cfg *Config, forkCache map[string]string) (string, bool) {
	if cfg.isWorkRepo(repo) {
		return repo, true
	}

	// If the repo owner is the user, check if it's a fork of a work repo.
	owner, _, ok := strings.Cut(repo, "/")
	if !ok || !strings.EqualFold(owner, user) {
		return repo, false
	}

	if parent, ok := forkCache[repo]; ok {
		if parent != "" {
			return parent, true
		}
		return repo, false
	}

	out, err := runCommand("gh", "api",
		fmt.Sprintf("/repos/%s", repo),
		"--jq", ".parent.full_name // empty",
	)
	if err != nil {
		forkCache[repo] = ""
		return repo, false
	}

	parent := strings.TrimSpace(out)
	if parent != "" && cfg.isWorkRepo(parent) {
		forkCache[repo] = parent
		return parent, true
	}
	forkCache[repo] = ""
	return repo, false
}

// GitHub event JSON structures (only the fields we need).

type ghEvent struct {
	Type      string    `json:"type"`
	CreatedAt time.Time `json:"created_at"`
	Repo      ghRepo    `json:"repo"`
	Payload   ghPayload `json:"payload"`
}

type ghRepo struct {
	Name string `json:"name"`
}

type ghPayload struct {
	Action      string      `json:"action"`
	Number      int         `json:"number"`
	Ref         string      `json:"ref"`
	RefType     string      `json:"ref_type"`
	PullRequest ghPR        `json:"pull_request"`
	Review      ghReview    `json:"review"`
	Issue       ghIssue     `json:"issue"`
	Comment     ghComment   `json:"comment"`
	Commits     []ghCommit  `json:"commits"`
}

type ghPR struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	URL    string `json:"html_url"`
	Merged bool   `json:"merged"`
}

type ghIssue struct {
	Number      int          `json:"number"`
	Title       string       `json:"title"`
	URL         string       `json:"html_url"`
	PullRequest *ghIssuePR   `json:"pull_request"`
}

type ghIssuePR struct {
	URL string `json:"url"`
}

type ghReview struct {
	State string `json:"state"`
}

type ghComment struct {
	Body string `json:"body"`
}

type ghCommit struct {
	Message string `json:"message"`
}
