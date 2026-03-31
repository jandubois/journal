package main

// inferNextSteps sets the NextStep field on each topic based on its activity history.
func inferNextSteps(topics []*Topic) {
	for _, t := range topics {
		t.NextStep = inferNextStep(t)
	}
}

func inferNextStep(t *Topic) string {
	if t.Number == 0 {
		return inferRepoLevelNextStep(t)
	}

	last := t.Activities[len(t.Activities)-1]

	// Closed/merged items are done.
	for _, a := range t.Activities {
		switch a.Kind {
		case "pr_merged", "pr_closed", "issue_closed":
			return "Done"
		}
	}

	// PR topics.
	if isPRTopic(t) {
		return inferPRNextStep(t, last)
	}

	// Issue topics.
	return inferIssueNextStep(t, last)
}

func isPRTopic(t *Topic) bool {
	for _, a := range t.Activities {
		switch a.Kind {
		case "pr_opened", "pr_merged", "pr_closed", "pr_reopened",
			"pr_reviewed", "pr_commented":
			return true
		}
	}
	return false
}

func inferPRNextStep(t *Topic, last Activity) string {
	// Check if we authored or reviewed.
	authored := false
	reviewed := false
	var lastReviewState string

	for _, a := range t.Activities {
		switch a.Kind {
		case "pr_opened", "pr_reopened":
			authored = true
		case "pr_reviewed":
			reviewed = true
			lastReviewState = a.Details
		}
	}

	if authored {
		switch lastReviewState {
		case "changes_requested":
			return "Address feedback"
		case "approved":
			return "Merge"
		}
		return "Awaiting review"
	}

	if reviewed {
		switch lastReviewState {
		case "approved":
			return "Awaiting author"
		case "changes_requested":
			return "Awaiting author"
		}
		return "Monitor"
	}

	// Commented on a PR, not authored or reviewed.
	return "Monitor"
}

func inferIssueNextStep(t *Topic, last Activity) string {
	authored := false
	for _, a := range t.Activities {
		if a.Kind == "issue_opened" {
			authored = true
		}
	}

	if authored {
		return "Awaiting response"
	}

	return "Monitor"
}

func inferRepoLevelNextStep(t *Topic) string {
	hasCommit := false
	hasPush := false

	for _, a := range t.Activities {
		switch a.Kind {
		case "commit":
			hasCommit = true
		case "pushed":
			hasPush = true
		}
	}

	if hasCommit && !hasPush {
		return "Push"
	}

	return ""
}
