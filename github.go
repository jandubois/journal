package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// fetchGitHubEvents retrieves the user's recent GitHub events and stores them as raw observations.
func fetchGitHubEvents(user string, db *sql.DB) error {
	var observations []Observation
	for page := 1; ; page++ {
		out, err := runCommand("gh", "api",
			fmt.Sprintf("/users/%s/events?per_page=100&page=%d", user, page),
		)
		if err != nil {
			if page == 1 {
				return err
			}
			break
		}

		var events []ghEvent
		if err := json.Unmarshal([]byte(out), &events); err != nil {
			return fmt.Errorf("parse events page %d: %w", page, err)
		}
		if len(events) == 0 {
			break
		}

		for _, e := range events {
			if obs := ghEventToObservation(e); obs != nil {
				observations = append(observations, *obs)
			}
		}
	}

	if len(observations) > 0 {
		if err := insertObservations(db, observations); err != nil {
			return fmt.Errorf("store observations: %w", err)
		}
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
// Forks are remapped to the parent slug so activities group together.
func resolveGitHubRepo(repo, user string, cfg *Config, forkCache map[string]string) (string, bool) {
	// Check cache first.
	if parent, ok := forkCache[repo]; ok {
		if parent != "" {
			return parent, cfg.isWorkRepo(parent)
		}
		return repo, cfg.isWorkRepo(repo)
	}

	// Only check for fork parents on repos owned by the user or a work org.
	// Repos from unknown orgs are kept as-is.
	owner, _, ok := strings.Cut(repo, "/")
	if !ok {
		return repo, false
	}
	isUserOrWork := strings.EqualFold(owner, user) || cfg.isWorkRepo(repo)
	if !isUserOrWork {
		return repo, false
	}

	out, err := runCommand("gh", "api",
		fmt.Sprintf("/repos/%s", repo),
		"--jq", ".parent.full_name // empty",
	)
	if err != nil {
		forkCache[repo] = ""
		return repo, cfg.isWorkRepo(repo)
	}

	parent := strings.TrimSpace(out)
	if parent != "" {
		forkCache[repo] = parent
		return parent, cfg.isWorkRepo(parent)
	}
	forkCache[repo] = ""
	return repo, cfg.isWorkRepo(repo)
}

// ghEventToObservation converts a raw GitHub event into a database observation.
// Stores raw event data without interpretation — no fork resolution, no work
// classification, no PR state enrichment.
func ghEventToObservation(e ghEvent) *Observation {
	data := map[string]any{
		"event_type": e.Type,
	}
	if e.Payload.Action != "" {
		data["action"] = e.Payload.Action
	}

	// Extract fields based on event type.
	switch e.Type {
	case "PullRequestEvent":
		data["number"] = e.Payload.Number
		data["url"] = e.Payload.PullRequest.URL
		data["title"] = e.Payload.PullRequest.Title
		data["merged"] = e.Payload.PullRequest.Merged
		data["is_author"] = true

	case "PullRequestReviewEvent":
		data["number"] = e.Payload.PullRequest.Number
		data["url"] = e.Payload.PullRequest.URL
		data["title"] = e.Payload.PullRequest.Title
		data["details"] = e.Payload.Review.State
		data["is_author"] = false

	case "IssueCommentEvent":
		data["number"] = e.Payload.Issue.Number
		data["url"] = e.Payload.Issue.URL
		data["title"] = e.Payload.Issue.Title
		data["is_pull_request"] = e.Payload.Issue.PullRequest != nil
		body := e.Payload.Comment.Body
		if idx := strings.IndexByte(body, '\n'); idx > 0 {
			body = body[:idx]
		}
		if len(body) > 120 {
			body = body[:120] + "..."
		}
		data["details"] = body

	case "IssuesEvent":
		data["number"] = e.Payload.Issue.Number
		data["url"] = e.Payload.Issue.URL
		data["title"] = e.Payload.Issue.Title
		data["is_author"] = true

	case "PushEvent":
		ref := e.Payload.Ref
		if strings.HasPrefix(ref, "refs/heads/") {
			ref = ref[len("refs/heads/"):]
		}
		data["ref"] = ref
		commits := make([]string, 0, len(e.Payload.Commits))
		for _, c := range e.Payload.Commits {
			msg := c.Message
			if idx := strings.IndexByte(msg, '\n'); idx > 0 {
				msg = msg[:idx]
			}
			commits = append(commits, msg)
		}
		data["commits"] = commits
		data["is_author"] = true

	case "CreateEvent":
		data["ref"] = e.Payload.Ref
		data["ref_type"] = e.Payload.RefType
		data["is_author"] = true

	case "DeleteEvent":
		data["ref"] = e.Payload.Ref
		data["ref_type"] = e.Payload.RefType
		data["is_author"] = true
	}

	jsonData, err := json.Marshal(data)
	if err != nil {
		return nil
	}

	return &Observation{
		Source:   "github",
		SourceID: e.ID,
		Time:     e.CreatedAt,
		Repo:     e.Repo.Name, // Raw repo slug, no fork resolution.
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
