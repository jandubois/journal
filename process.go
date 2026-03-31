package main

import (
	"database/sql"
	"fmt"
	"os"
	"time"
)

// processEvents converts grouped topics into events and stores them in the database.
// Each event links back to its source observations via the event_observations table.
func processEvents(db *sql.DB, topics []*Topic) error {
	// Clear existing events for the time range covered by these topics,
	// then rewrite. This allows reprocessing as logic improves.
	if err := clearEventsForTopics(db, topics); err != nil {
		return fmt.Errorf("clear events: %w", err)
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

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
		kind := eventKind(t)
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

		// Link event to source observations.
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

func clearEventsForTopics(db *sql.DB, topics []*Topic) error {
	if len(topics) == 0 {
		return nil
	}

	// Find the time range covered by these topics.
	var earliest, latest time.Time
	for _, t := range topics {
		for _, a := range t.Activities {
			if earliest.IsZero() || a.Time.Before(earliest) {
				earliest = a.Time
			}
			if a.Time.After(latest) {
				latest = a.Time
			}
		}
	}

	_, err := db.Exec(
		`DELETE FROM events WHERE timestamp >= ? AND timestamp <= ?`,
		earliest.UTC().Format(time.RFC3339Nano),
		latest.UTC().Format(time.RFC3339Nano),
	)
	return err
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

// eventKind classifies the topic into a high-level event type.
func eventKind(t *Topic) string {
	if t.NextStep == "Done" {
		for _, a := range t.Activities {
			if a.Kind == "pr_review_merged" {
				return "reviewed_and_merged"
			}
			if a.Kind == "pr_merged" {
				return "merged"
			}
			if a.Kind == "issue_closed" {
				return "closed"
			}
		}
		return "completed"
	}

	for _, a := range t.Activities {
		switch a.Kind {
		case "pr_opened":
			return "opened_pr"
		case "pr_reviewed":
			return "reviewed"
		case "issue_opened":
			return "opened_issue"
		case "issue_commented", "pr_commented":
			return "commented"
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

// queryEvents retrieves events from the database for the given time range.
func queryEvents(db *sql.DB, since time.Time) ([]*Topic, error) {
	sinceStr := since.UTC().Format(time.RFC3339Nano)
	rows, err := db.Query(
		`SELECT id, timestamp, duration_seconds, repo, number, title, url, kind, summary, work, next_step
		 FROM events
		 WHERE timestamp >= ?
		 ORDER BY timestamp`,
		sinceStr,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var topics []*Topic
	for rows.Next() {
		var (
			id       int64
			ts       string
			durSecs  sql.NullInt64
			number   sql.NullInt64
			title    sql.NullString
			url      sql.NullString
			kind     string
			summary  string
			work     int
			nextStep sql.NullString
			repo     string
		)
		if err := rows.Scan(&id, &ts, &durSecs, &repo, &number, &title, &url, &kind, &summary, &work, &nextStep); err != nil {
			fmt.Fprintf(os.Stderr, "warning: scan event: %v\n", err)
			continue
		}

		t := &Topic{
			Repo:     repo,
			Number:   int(number.Int64),
			Title:    title.String,
			URL:      url.String,
			Work:     work != 0,
			NextStep: nextStep.String,
		}

		// For now, reconstruct a single synthetic activity so the renderer works.
		eventTime, _ := time.Parse(time.RFC3339Nano, ts)
		t.Activities = []Activity{{
			Time:    eventTime,
			Kind:    kind,
			Repo:    repo,
			Number:  int(number.Int64),
			Title:   title.String,
			Details: summary,
			Work:    work != 0,
		}}
		if durSecs.Valid {
			t.Activities[0].Duration = time.Duration(durSecs.Int64) * time.Second
		}

		topics = append(topics, t)
	}
	return topics, rows.Err()
}