package main

import (
	"database/sql"
	"fmt"
	"time"
)

// processEvents converts grouped topics into events and stores them in the database.
// Clears all existing events and rewrites from scratch within a single transaction,
// ensuring consistency regardless of the --since window used.
func processEvents(db *sql.DB, topics []*Topic) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Clear all existing events. The events table is a derived view over
	// observations — it's rebuilt from scratch on each run. This avoids
	// stale events from topics that dropped out of the query window.
	if _, err := tx.Exec("DELETE FROM event_observations"); err != nil {
		return fmt.Errorf("clear event links: %w", err)
	}
	if _, err := tx.Exec("DELETE FROM events"); err != nil {
		return fmt.Errorf("clear events: %w", err)
	}

	insertEvent, err := tx.Prepare(
		`INSERT INTO events (timestamp, duration_seconds, repo, number, title, url, kind, summary, work, next_step)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
	)
	if err != nil {
		return err
	}
	defer insertEvent.Close()

	insertLink, err := tx.Prepare(
		`INSERT INTO event_observations (event_id, observation_id) VALUES (?, ?)`,
	)
	if err != nil {
		return err
	}
	defer insertLink.Close()

	for _, t := range topics {
		summary := buildEventSummary(t)
		kind := topicKind(t)
		duration := topicDuration(t)

		var durSeconds *int
		if duration > 0 {
			d := int(duration.Seconds())
			durSeconds = &d
		}

		earliest := t.Activities[0].Time
		result, err := insertEvent.Exec(
			earliest.UTC().Format(time.RFC3339Nano),
			durSeconds,
			t.Repo,
			t.Number,
			t.Title,
			t.URL,
			kind,
			summary,
			boolToInt(t.Work),
			t.NextStep,
		)
		if err != nil {
			return fmt.Errorf("insert event for %s#%d: %w", t.Repo, t.Number, err)
		}

		eventID, err := result.LastInsertId()
		if err != nil {
			return err
		}

		for _, a := range t.Activities {
			if a.ObservationID > 0 {
				if _, err := insertLink.Exec(eventID, a.ObservationID); err != nil {
					return fmt.Errorf("link event %d to observation %d: %w", eventID, a.ObservationID, err)
				}
			}
		}
	}

	return tx.Commit()
}

// buildEventSummary produces a one-line summary of a topic.
func buildEventSummary(t *Topic) string {
	if t.Number == 0 {
		return repoLevelSummary(t)
	}

	prefix := "Issue"
	if isPRTopic(t) {
		prefix = "PR"
	}

	action := summarizeActions(t)
	if t.Title != "" {
		return fmt.Sprintf("%s #%d: %s — %s", prefix, t.Number, t.Title, action)
	}
	return fmt.Sprintf("%s #%d — %s", prefix, t.Number, action)
}

func repoLevelSummary(t *Topic) string {
	var commitCount, sessionCount int
	for _, a := range t.Activities {
		switch a.Kind {
		case "commit":
			commitCount++
		case "session":
			sessionCount++
		}
	}

	parts := make([]string, 0, 2)
	if commitCount > 0 {
		parts = append(parts, fmt.Sprintf("%d commits", commitCount))
	}
	if sessionCount > 0 {
		parts = append(parts, fmt.Sprintf("%d sessions", sessionCount))
	}

	if len(parts) == 0 {
		return summarizeActions(t)
	}

	result := ""
	for i, p := range parts {
		if i > 0 {
			result += ", "
		}
		result += p
	}
	return result
}

// topicKind classifies the topic for the events table, using the same
// activity kind vocabulary as the rest of the system.
func topicKind(t *Topic) string {
	if t.NextStep == "Done" {
		for _, a := range t.Activities {
			switch a.Kind {
			case "pr_review_merged":
				return "pr_review_merged"
			case "pr_merged":
				return "pr_merged"
			case "issue_closed":
				return "issue_closed"
			}
		}
		return "completed"
	}

	// Use the most significant activity kind.
	for _, a := range t.Activities {
		switch a.Kind {
		case "pr_opened", "pr_reviewed", "issue_opened",
			"issue_commented", "pr_commented":
			return a.Kind
		}
	}

	if t.Number == 0 {
		return "development"
	}

	return "activity"
}

// topicDuration returns the total session duration within a topic.
func topicDuration(t *Topic) time.Duration {
	var total time.Duration
	for _, a := range t.Activities {
		total += a.Duration
	}
	return total
}
