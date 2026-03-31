package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// fetchGitHubEvents retrieves the user's recent GitHub events and converts them to activities.
func fetchGitHubEvents(user string, since time.Time, cfg *Config, db *sql.DB) ([]Activity, error) {
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
	var observations []Observation
	infoCache := make(map[string]*prInfo) // "owner/repo#123" -> info
	forkCache := make(map[string]string)  // "owner/repo" -> parent slug or ""

	for _, e := range allEvents {
		acts := convertEvent(e, cfg, user, forkCache)
		for i := range acts {
			if acts[i].Number == 0 {
				continue
			}
			info := fetchPRInfo(acts[i].Repo, acts[i].Number, infoCache)
			if info == nil {
				continue
			}
			if acts[i].Title == "" {
				acts[i].Title = info.Title
			}
			if acts[i].Kind == "pr_reviewed" && info.Merged {
				acts[i].Kind = "pr_review_merged"
			}
		}

		// Store observation for every event (regardless of since filter).
		if obs := ghEventToObservation(e, acts, forkCache, user, cfg); obs != nil {
			observations = append(observations, *obs)
		}

		// Only include in current report if within the time range.
		if !e.CreatedAt.Before(since) {
			activities = append(activities, acts...)
		}
	}

	if db != nil && len(observations) > 0 {
		if err := insertObservations(db, observations); err != nil {
			fmt.Fprintf(os.Stderr, "warning: store GitHub observations: %v\n", err)
		}
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
		ref := e.Payload.Ref
		if strings.HasPrefix(ref, "refs/heads/") {
			ref = ref[len("refs/heads/"):]
		}
		// Skip pushes to default branches — these are merge side-effects
		// already captured by PullRequestEvent.
		if ref == "main" || ref == "master" {
			return nil
		}
		a := base
		a.Kind = "pushed"
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
		// Branch deletes are noise in a standup context.
		return nil
	}

	return nil
}

type prInfo struct {
	Title  string
	State  string // "open", "closed"
	Merged bool
}

func fetchPRInfo(repo string, number int, cache map[string]*prInfo) *prInfo {
	key := fmt.Sprintf("%s#%d", repo, number)
	if info, ok := cache[key]; ok {
		return info
	}

	// Try as PR first (returns title, state, and merged status).
	out, err := runCommand("gh", "api",
		fmt.Sprintf("/repos/%s/pulls/%d", repo, number),
		"--jq", "[.title, .state, (if .merged then \"merged\" else \"\" end)] | join(\"|\")",
	)
	if err == nil {
		parts := strings.SplitN(strings.TrimSpace(out), "|", 3)
		if len(parts) == 3 {
			info := &prInfo{
				Title:  parts[0],
				State:  parts[1],
				Merged: parts[2] == "merged",
			}
			cache[key] = info
			return info
		}
	}

	// Fall back to issue (no merged state).
	out, err = runCommand("gh", "api",
		fmt.Sprintf("/repos/%s/issues/%d", repo, number),
		"--jq", "[.title, .state] | join(\"|\")",
	)
	if err == nil {
		parts := strings.SplitN(strings.TrimSpace(out), "|", 2)
		if len(parts) == 2 {
			info := &prInfo{Title: parts[0], State: parts[1]}
			cache[key] = info
			return info
		}
	}

	cache[key] = nil
	return nil
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

// ghEventToObservation converts a GitHub event into a database observation.
func ghEventToObservation(e ghEvent, acts []Activity, forkCache map[string]string, user string, cfg *Config) *Observation {
	if len(acts) == 0 {
		return nil
	}
	a := acts[0] // Use the first activity for repo/work classification.

	// Build source-specific data.
	data := map[string]any{
		"event_type": e.Type,
	}
	if e.Payload.Action != "" {
		data["action"] = e.Payload.Action
	}
	if a.Number > 0 {
		data["number"] = a.Number
	}
	if a.Title != "" {
		data["title"] = a.Title
	}
	if a.URL != "" {
		data["url"] = a.URL
	}
	if a.Details != "" {
		data["details"] = a.Details
	}
	if a.Kind != "" {
		data["kind"] = a.Kind
	}
	data["is_author"] = a.IsAuthor

	jsonData, err := json.Marshal(data)
	if err != nil {
		return nil
	}

	return &Observation{
		Source:   "github",
		SourceID: e.ID,
		Time:     e.CreatedAt,
		Repo:     a.Repo,
		Work:     a.Work,
		Data:     jsonData,
	}
}

// GitHub event JSON structures (only the fields we need).

type ghEvent struct {
	ID        string    `json:"id"`
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
