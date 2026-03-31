package main

import (
	"fmt"
	"io"
	"strings"
	"time"
)

func renderText(w io.Writer, topics []*Topic) {
	work, personal := partitionTopics(topics)

	if len(work) > 0 {
		fmt.Fprintln(w, "=== Work ===")
		fmt.Fprintln(w)
		renderTextSection(w, work)
	}

	if len(personal) > 0 {
		if len(work) > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintln(w, "=== Personal ===")
		fmt.Fprintln(w)
		renderTextSection(w, personal)
	}

	if len(work) == 0 && len(personal) == 0 {
		fmt.Fprintln(w, "No activity found.")
	}
}

func renderTextSection(w io.Writer, topics []*Topic) {
	// Group topics by repo for display.
	type repoGroup struct {
		repo   string
		topics []*Topic
	}
	var groups []repoGroup
	seen := make(map[string]int)

	for _, t := range topics {
		if idx, ok := seen[t.Repo]; ok {
			groups[idx].topics = append(groups[idx].topics, t)
		} else {
			seen[t.Repo] = len(groups)
			groups = append(groups, repoGroup{repo: t.Repo, topics: []*Topic{t}})
		}
	}

	for i, g := range groups {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "## %s\n", g.repo)
		fmt.Fprintln(w)

		// Separate PR/issue topics from repo-level topics.
		var numbered []*Topic
		var repoLevel []*Topic
		for _, t := range g.topics {
			if t.Number > 0 {
				numbered = append(numbered, t)
			} else {
				repoLevel = append(repoLevel, t)
			}
		}

		for _, t := range numbered {
			renderTextTopic(w, t)
		}

		// Render repo-level activities (commits, sessions).
		for _, t := range repoLevel {
			renderTextRepoLevel(w, t)
		}
	}
}

func renderTextTopic(w io.Writer, t *Topic) {
	prefix := "#"
	if isPRTopic(t) {
		prefix = "PR #"
	}

	// Compact rendering for done topics: one line per topic.
	if t.NextStep == "Done" {
		action := summarizeActions(t)
		if t.Title != "" {
			fmt.Fprintf(w, "%s%d: %s — %s\n", prefix, t.Number, t.Title, action)
		} else {
			fmt.Fprintf(w, "%s%d — %s\n", prefix, t.Number, action)
		}
		return
	}

	if t.Title != "" {
		fmt.Fprintf(w, "%s%d: %s\n", prefix, t.Number, t.Title)
	} else {
		fmt.Fprintf(w, "%s%d\n", prefix, t.Number)
	}

	for _, a := range t.Activities {
		if a.Kind == "session" {
			continue
		}
		timeStr := a.Time.Local().Format("15:04")
		desc := activityDescription(a)
		fmt.Fprintf(w, "  - %s  %s\n", timeStr, desc)
	}

	// Sessions for this topic.
	for _, a := range t.Activities {
		if a.Kind == "session" {
			renderTextSession(w, a)
		}
	}

	if t.NextStep != "" {
		fmt.Fprintf(w, "  → %s\n", t.NextStep)
	}
	fmt.Fprintln(w)
}

func renderTextRepoLevel(w io.Writer, t *Topic) {
	var nonSession, sessions []Activity
	for _, a := range t.Activities {
		if a.Kind == "session" {
			sessions = append(sessions, a)
		} else {
			nonSession = append(nonSession, a)
		}
	}

	for _, a := range nonSession {
		timeStr := a.Time.Local().Format("15:04")
		desc := activityDescription(a)
		fmt.Fprintf(w, "  - %s  %s\n", timeStr, desc)
	}

	if len(sessions) > 0 {
		fmt.Fprintln(w, "Claude sessions:")
		for _, a := range sessions {
			renderTextSession(w, a)
		}
	}

	if t.NextStep != "" {
		fmt.Fprintf(w, "  → %s\n", t.NextStep)
	}
	if len(nonSession)+len(sessions) > 0 {
		fmt.Fprintln(w)
	}
}

func renderTextSession(w io.Writer, a Activity) {
	timeStr := a.Time.Local().Format("15:04")
	durStr := formatDuration(a.Duration)
	if a.Details != "" {
		fmt.Fprintf(w, "  - %s  (%s) %s\n", timeStr, durStr, a.Details)
	} else {
		fmt.Fprintf(w, "  - %s  (%s)\n", timeStr, durStr)
	}
}

func renderMarkdown(w io.Writer, topics []*Topic) {
	work, personal := partitionTopics(topics)

	if len(work) > 0 {
		fmt.Fprintln(w, "# Work")
		fmt.Fprintln(w)
		renderMarkdownSection(w, work)
	}

	if len(personal) > 0 {
		if len(work) > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintln(w, "# Personal")
		fmt.Fprintln(w)
		renderMarkdownSection(w, personal)
	}

	if len(work) == 0 && len(personal) == 0 {
		fmt.Fprintln(w, "No activity found.")
	}
}

func renderMarkdownSection(w io.Writer, topics []*Topic) {
	type repoGroup struct {
		repo   string
		topics []*Topic
	}
	var groups []repoGroup
	seen := make(map[string]int)

	for _, t := range topics {
		if idx, ok := seen[t.Repo]; ok {
			groups[idx].topics = append(groups[idx].topics, t)
		} else {
			seen[t.Repo] = len(groups)
			groups = append(groups, repoGroup{repo: t.Repo, topics: []*Topic{t}})
		}
	}

	for i, g := range groups {
		if i > 0 {
			fmt.Fprintln(w)
		}
		repoURL := fmt.Sprintf("https://github.com/%s", g.repo)
		fmt.Fprintf(w, "## [%s](%s)\n", g.repo, repoURL)
		fmt.Fprintln(w)

		var numbered []*Topic
		var repoLevel []*Topic
		for _, t := range g.topics {
			if t.Number > 0 {
				numbered = append(numbered, t)
			} else {
				repoLevel = append(repoLevel, t)
			}
		}

		for _, t := range numbered {
			renderMarkdownTopic(w, t)
		}

		for _, t := range repoLevel {
			renderTextRepoLevel(w, t) // Reuse text format for repo-level.
		}
	}
}

func renderMarkdownTopic(w io.Writer, t *Topic) {
	prefix := ""
	if isPRTopic(t) {
		prefix = "PR "
	}
	ref := fmt.Sprintf("%s#%d", prefix, t.Number)
	if t.URL != "" {
		ref = fmt.Sprintf("[%s#%d](%s)", prefix, t.Number, t.URL)
	}

	// Compact rendering for done topics.
	if t.NextStep == "Done" {
		action := summarizeActions(t)
		if t.Title != "" {
			fmt.Fprintf(w, "%s: %s — %s\n", ref, t.Title, action)
		} else {
			fmt.Fprintf(w, "%s — %s\n", ref, action)
		}
		return
	}

	if t.Title != "" {
		fmt.Fprintf(w, "%s: %s\n", ref, t.Title)
	} else {
		fmt.Fprintf(w, "%s\n", ref)
	}

	for _, a := range t.Activities {
		if a.Kind == "session" {
			continue
		}
		timeStr := a.Time.Local().Format("15:04")
		desc := activityDescription(a)
		fmt.Fprintf(w, "  - %s  %s\n", timeStr, desc)
	}

	for _, a := range t.Activities {
		if a.Kind == "session" {
			renderTextSession(w, a)
		}
	}

	if t.NextStep != "" {
		fmt.Fprintf(w, "  → %s\n", t.NextStep)
	}
	fmt.Fprintln(w)
}

func activityDescription(a Activity) string {
	switch a.Kind {
	case "pr_opened":
		return "Opened PR"
	case "pr_closed":
		return "Closed PR"
	case "pr_merged":
		return "Merged PR"
	case "pr_reopened":
		return "Reopened PR"
	case "pr_reviewed":
		return fmt.Sprintf("Reviewed (%s)", a.Details)
	case "pr_review_merged":
		return "Reviewed and merged"
	case "pr_commented":
		return "Commented on PR"
	case "issue_opened":
		return "Opened issue"
	case "issue_closed":
		return "Closed issue"
	case "issue_reopened":
		return "Reopened issue"
	case "issue_commented":
		return "Commented"
	case "pushed":
		return a.Details
	case "commit":
		return a.Details
	case "branch_created":
		return fmt.Sprintf("Created branch %s", a.Details)
	case "branch_deleted":
		return fmt.Sprintf("Deleted branch %s", a.Details)
	case "tag_created":
		return fmt.Sprintf("Created tag %s", a.Details)
	default:
		return a.Kind
	}
}

// summarizeActions produces a short description of what happened in a topic.
func summarizeActions(t *Topic) string {
	var parts []string
	seen := make(map[string]bool)
	for _, a := range t.Activities {
		if a.Kind == "session" {
			continue
		}
		desc := activityDescription(a)
		if !seen[desc] {
			seen[desc] = true
			parts = append(parts, desc)
		}
	}
	if len(parts) == 0 {
		return "Done"
	}
	return strings.Join(parts, ", ")
}

func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return "<1m"
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh%dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}

func partitionTopics(topics []*Topic) (work, personal []*Topic) {
	for _, t := range topics {
		if t.Work {
			work = append(work, t)
		} else {
			personal = append(personal, t)
		}
	}
	return
}

// kindLabel returns a human label for an activity kind.
func kindLabel(kind string) string {
	labels := map[string]string{
		"pr_opened":       "PR",
		"pr_closed":       "PR",
		"pr_merged":       "PR",
		"pr_reopened":     "PR",
		"pr_reviewed":     "Review",
		"pr_commented":    "PR comment",
		"issue_opened":    "Issue",
		"issue_closed":    "Issue",
		"issue_reopened":  "Issue",
		"issue_commented": "Comment",
		"pushed":          "Push",
		"commit":          "Commit",
		"branch_created":  "Branch",
		"branch_deleted":  "Branch",
		"tag_created":     "Tag",
		"session":         "Claude",
	}
	if l, ok := labels[kind]; ok {
		return l
	}
	if len(kind) == 0 {
		return kind
	}
	return strings.ToUpper(kind[:1]) + kind[1:]
}
