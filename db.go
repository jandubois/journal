package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const schemaVersion = 3

const schema = `
CREATE TABLE IF NOT EXISTS schema_version (
    version INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS observations (
    id           INTEGER PRIMARY KEY,
    source       TEXT NOT NULL,
    source_id    TEXT NOT NULL,
    timestamp    TEXT NOT NULL,
    repo         TEXT,
    data         TEXT NOT NULL,
    collected_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE(source, source_id, repo)
);

CREATE TABLE IF NOT EXISTS events (
    id               INTEGER PRIMARY KEY,
    timestamp        TEXT NOT NULL,
    duration_seconds INTEGER,
    repo             TEXT NOT NULL,
    number           INTEGER,
    title            TEXT,
    url              TEXT,
    kind             TEXT NOT NULL,
    summary          TEXT NOT NULL,
    work             INTEGER NOT NULL,
    next_step        TEXT,
    processed_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE TABLE IF NOT EXISTS event_observations (
    event_id       INTEGER NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    observation_id INTEGER NOT NULL REFERENCES observations(id),
    PRIMARY KEY (event_id, observation_id)
);

CREATE INDEX IF NOT EXISTS idx_observations_timestamp ON observations(timestamp);
CREATE INDEX IF NOT EXISTS idx_observations_repo ON observations(repo);
CREATE INDEX IF NOT EXISTS idx_events_timestamp ON events(timestamp);
CREATE INDEX IF NOT EXISTS idx_events_repo ON events(repo);

-- AI-generated summaries, keyed by (repo, number) to survive event rebuilds.
-- number=0 entries use the timestamp to distinguish repo-level events from
-- different time periods.
CREATE TABLE IF NOT EXISTS ai_summaries (
    repo       TEXT NOT NULL,
    number     INTEGER NOT NULL,
    period     TEXT NOT NULL DEFAULT '',
    summary    TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (repo, number, period)
);
`

type Observation struct {
	ID       int64           `json:"id"`
	Source   string          `json:"source"`
	SourceID string          `json:"source_id"`
	Time     time.Time       `json:"time"`
	Repo     string          `json:"repo"`
	Data     json.RawMessage `json:"data"`
}

func openDB() (*sql.DB, error) {
	dbPath := dbFilePath()
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}

	// Check if existing DB has a different schema version and delete if so.
	if _, err := os.Stat(dbPath); err == nil {
		if needsReset(dbPath) {
			os.Remove(dbPath)
			// Also remove WAL and SHM files.
			os.Remove(dbPath + "-wal")
			os.Remove(dbPath + "-shm")
		}
	}

	db, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(wal)&_pragma=foreign_keys(on)")
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}

	// Set schema version within a transaction.
	if _, err := db.Exec("BEGIN; DELETE FROM schema_version; INSERT INTO schema_version (version) VALUES (?); COMMIT;", schemaVersion); err != nil {
		db.Close()
		return nil, fmt.Errorf("set schema version: %w", err)
	}

	return db, nil
}

func needsReset(dbPath string) bool {
	db, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(wal)&_pragma=foreign_keys(on)")
	if err != nil {
		return true
	}
	defer db.Close()

	var version int
	err = db.QueryRow("SELECT version FROM schema_version LIMIT 1").Scan(&version)
	if err != nil || version != schemaVersion {
		return true
	}
	return false
}

func dbFilePath() string {
	dataDir := os.Getenv("XDG_DATA_HOME")
	if dataDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			dataDir = "."
		} else {
			dataDir = filepath.Join(home, ".local", "share")
		}
	}
	return filepath.Join(dataDir, "journal", "journal.db")
}

func insertObservation(db *sql.DB, o Observation) error {
	_, err := db.Exec(
		`INSERT OR IGNORE INTO observations (source, source_id, timestamp, repo, data)
		 VALUES (?, ?, ?, ?, ?)`,
		o.Source,
		o.SourceID,
		o.Time.UTC().Format(time.RFC3339Nano),
		o.Repo,
		string(o.Data),
	)
	return err
}

func insertObservations(db *sql.DB, observations []Observation) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(
		`INSERT OR IGNORE INTO observations (source, source_id, timestamp, repo, data)
		 VALUES (?, ?, ?, ?, ?)`,
	)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, o := range observations {
		_, err := stmt.Exec(
			o.Source,
			o.SourceID,
			o.Time.UTC().Format(time.RFC3339Nano),
			o.Repo,
			string(o.Data),
		)
		if err != nil {
			return err
		}
	}

	return tx.Commit()
}

func queryObservations(db *sql.DB, since time.Time) ([]Observation, error) {
	sinceStr := since.UTC().Format(time.RFC3339Nano)
	rows, err := db.Query(
		`SELECT id, source, source_id, timestamp, repo, data
		 FROM observations
		 WHERE timestamp >= ?
		 ORDER BY timestamp`,
		sinceStr,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var observations []Observation
	for rows.Next() {
		var o Observation
		var ts, dataStr string
		if err := rows.Scan(&o.ID, &o.Source, &o.SourceID, &ts, &o.Repo, &dataStr); err != nil {
			return nil, err
		}
		o.Time, _ = time.Parse(time.RFC3339Nano, ts)
		o.Data = json.RawMessage(dataStr)
		observations = append(observations, o)
	}
	return observations, rows.Err()
}

// queryTopicHistory fetches all observations for specific (repo, number) pairs,
// regardless of time range. This provides full history for next-step inference
// on topics that started before the --since window.
func queryTopicHistory(db *sql.DB, repoNumbers [][2]string) ([]Observation, error) {
	if len(repoNumbers) == 0 {
		return nil, nil
	}

	// Build a query with OR clauses for each (repo, number) pair.
	// We match on the raw repo slug in the observation, so we need to check
	// all repo slugs that might resolve to the same canonical repo.
	query := `SELECT id, source, source_id, timestamp, repo, data
		 FROM observations
		 WHERE source = 'github' AND (`
	args := make([]any, 0, len(repoNumbers)*2)
	for i, rn := range repoNumbers {
		if i > 0 {
			query += " OR "
		}
		query += "(repo = ? AND json_extract(data, '$.number') = ?)"
		args = append(args, rn[0], rn[1])
	}
	query += ") ORDER BY timestamp"

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var observations []Observation
	for rows.Next() {
		var o Observation
		var ts, dataStr string
		if err := rows.Scan(&o.ID, &o.Source, &o.SourceID, &ts, &o.Repo, &dataStr); err != nil {
			return nil, err
		}
		o.Time, _ = time.Parse(time.RFC3339Nano, ts)
		o.Data = json.RawMessage(dataStr)
		observations = append(observations, o)
	}
	return observations, rows.Err()
}

// observationsToActivities reconstructs Activity structs from database observations,
// applying all interpretation: fork resolution, work classification, PR merge detection.
func observationsToActivities(observations []Observation, cfg *Config, user string) []Activity {
	forkCache := make(map[string]string)
	prCache := make(map[string]*prInfo)

	var activities []Activity
	for _, o := range observations {
		a := observationToActivity(o, cfg, user, forkCache, prCache)
		if a != nil {
			activities = append(activities, *a)
		}
	}
	return activities
}

func observationToActivity(o Observation, cfg *Config, user string, forkCache map[string]string, prCache map[string]*prInfo) *Activity {
	var data map[string]json.RawMessage
	if err := json.Unmarshal(o.Data, &data); err != nil {
		return nil
	}

	// Resolve repo: fork resolution and work classification happen here.
	repo, work := resolveRepo(o.Repo, o.Source, user, cfg, forkCache)

	a := Activity{
		ObservationID: o.ID,
		Time:          o.Time,
		Repo:          repo,
		Work:          work,
	}

	switch o.Source {
	case "github":
		a.Kind = activityKindFromEvent(jsonString(data["event_type"]), jsonString(data["action"]), data)
		a.Title = jsonString(data["title"])
		a.URL = jsonString(data["url"])
		a.Details = jsonString(data["details"])
		a.Number = jsonInt(data["number"])
		a.IsAuthor = jsonBool(data["is_author"])

		// PR merge detection: check live state for review events.
		if a.Kind == "pr_reviewed" && a.Number > 0 {
			info := fetchPRInfo(repo, a.Number, prCache)
			if info != nil {
				if a.Title == "" {
					a.Title = info.Title
				}
				if info.Merged {
					a.Kind = "pr_review_merged"
				}
			}
		}
		// Fill missing titles for other PR/issue events.
		if a.Number > 0 && a.Title == "" {
			if info := fetchPRInfo(repo, a.Number, prCache); info != nil {
				a.Title = info.Title
			}
		}

		// For push events, build details from raw ref and commits.
		if a.Kind == "pushed" {
			ref := jsonString(data["ref"])
			// Skip pushes to default branches — merge side-effects.
			if ref == "main" || ref == "master" {
				return nil
			}
			var commits []string
			if raw, ok := data["commits"]; ok {
				json.Unmarshal(raw, &commits)
			}
			if len(commits) == 0 {
				a.Details = fmt.Sprintf("pushed to %s", ref)
			} else if len(commits) == 1 {
				a.Details = fmt.Sprintf("pushed to %s: %s", ref, commits[0])
			} else {
				a.Details = fmt.Sprintf("pushed %d commits to %s", len(commits), ref)
			}
		}

		// Skip branch deletes — noise in standup context.
		if a.Kind == "branch_deleted" {
			return nil
		}

		// For create events, set details from ref.
		if a.Kind == "branch_created" || a.Kind == "tag_created" {
			a.Details = jsonString(data["ref"])
		}

	case "git":
		a.Kind = "commit"
		a.Details = jsonString(data["message"])
		a.IsAuthor = true

	case "session":
		a.Kind = "session"
		a.Duration = time.Duration(jsonInt(data["duration_seconds"])) * time.Second
		// Use first line of first prompt as display summary.
		var prompts []string
		if raw, ok := data["prompts"]; ok {
			json.Unmarshal(raw, &prompts)
		}
		if len(prompts) > 0 {
			details := prompts[0]
			if idx := strings.IndexByte(details, '\n'); idx > 0 {
				details = details[:idx]
			}
			if len(details) > 120 {
				details = details[:120] + "..."
			}
			a.Details = details
		}

	default:
		return nil
	}

	return &a
}

// resolveRepo applies fork resolution and work classification to a raw repo slug.
// For GitHub events, uses the GitHub API to check fork parents.
// For git/session observations, uses local repo remotes.
func resolveRepo(rawRepo, source, user string, cfg *Config, forkCache map[string]string) (string, bool) {
	switch source {
	case "github":
		return resolveGitHubRepo(rawRepo, user, cfg, forkCache)
	case "git", "session":
		// For local sources, check if the repo slug is already resolved
		// (origin remote), then check if it's a fork via the GitHub API.
		if cfg.isWorkRepo(rawRepo) {
			return resolveGitHubRepo(rawRepo, user, cfg, forkCache)
		}
		// For user repos, check fork parent.
		owner, _, ok := strings.Cut(rawRepo, "/")
		if ok && strings.EqualFold(owner, user) {
			return resolveGitHubRepo(rawRepo, user, cfg, forkCache)
		}
		return rawRepo, false
	}
	return rawRepo, false
}

// activityKindFromEvent derives the Activity kind from raw GitHub event type and action.
func activityKindFromEvent(eventType, action string, data map[string]json.RawMessage) string {
	switch eventType {
	case "PullRequestEvent":
		switch action {
		case "opened":
			return "pr_opened"
		case "closed":
			if jsonBool(data["merged"]) {
				return "pr_merged"
			}
			return "pr_closed"
		case "reopened":
			return "pr_reopened"
		}
	case "PullRequestReviewEvent":
		return "pr_reviewed"
	case "IssueCommentEvent":
		if jsonBool(data["is_pull_request"]) {
			return "pr_commented"
		}
		return "issue_commented"
	case "IssuesEvent":
		switch action {
		case "opened":
			return "issue_opened"
		case "closed":
			return "issue_closed"
		case "reopened":
			return "issue_reopened"
		}
	case "PushEvent":
		return "pushed"
	case "CreateEvent":
		refType := jsonString(data["ref_type"])
		if refType == "tag" {
			return "tag_created"
		}
		return "branch_created"
	case "DeleteEvent":
		return "branch_deleted"
	}
	return eventType
}

func jsonString(raw json.RawMessage) string {
	var s string
	json.Unmarshal(raw, &s)
	return s
}

func jsonInt(raw json.RawMessage) int {
	var n int
	json.Unmarshal(raw, &n)
	return n
}

func jsonBool(raw json.RawMessage) bool {
	var b bool
	json.Unmarshal(raw, &b)
	return b
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// AI summary storage and retrieval.

func storeAISummary(db *sql.DB, repo string, number int, period, summary string) error {
	_, err := db.Exec(
		`INSERT OR REPLACE INTO ai_summaries (repo, number, period, summary)
		 VALUES (?, ?, ?, ?)`,
		repo, number, period, summary,
	)
	return err
}

func getAISummary(db *sql.DB, repo string, number int, period string) (string, error) {
	var summary string
	err := db.QueryRow(
		`SELECT summary FROM ai_summaries WHERE repo = ? AND number = ? AND period = ?`,
		repo, number, period,
	).Scan(&summary)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return summary, err
}

// PendingEvent represents an event that needs AI summarization.
type PendingEvent struct {
	EventID  int64  `json:"event_id"`
	Repo     string `json:"repo"`
	Number   int    `json:"number"`
	Title    string `json:"title"`
	Kind     string `json:"kind"`
	Summary  string `json:"mechanical_summary"`
	NextStep string `json:"next_step"`
	Period   string `json:"period"`
	Work     bool   `json:"work"`
}

// queryPendingSummaries finds events that lack AI summaries.
func queryPendingSummaries(db *sql.DB, since time.Time, limit int) ([]PendingEvent, int, error) {
	sinceStr := since.UTC().Format(time.RFC3339Nano)

	// Count total pending.
	var total int
	err := db.QueryRow(`
		SELECT count(*)
		FROM events e
		WHERE e.timestamp >= ?
		  AND NOT EXISTS (
		    SELECT 1 FROM ai_summaries a
		    WHERE a.repo = e.repo AND a.number = COALESCE(e.number, 0)
		      AND a.period = CASE WHEN COALESCE(e.number, 0) = 0 THEN substr(e.timestamp, 1, 10) ELSE '' END
		  )`,
		sinceStr,
	).Scan(&total)
	if err != nil {
		return nil, 0, err
	}

	// Fetch the most recent pending events.
	rows, err := db.Query(`
		SELECT e.id, e.repo, COALESCE(e.number, 0), COALESCE(e.title, ''), e.kind, e.summary,
		       COALESCE(e.next_step, ''), e.work, e.timestamp
		FROM events e
		WHERE e.timestamp >= ?
		  AND NOT EXISTS (
		    SELECT 1 FROM ai_summaries a
		    WHERE a.repo = e.repo AND a.number = COALESCE(e.number, 0)
		      AND a.period = CASE WHEN COALESCE(e.number, 0) = 0 THEN substr(e.timestamp, 1, 10) ELSE '' END
		  )
		ORDER BY e.timestamp DESC
		LIMIT ?`,
		sinceStr, limit,
	)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var events []PendingEvent
	for rows.Next() {
		var e PendingEvent
		var work int
		var ts string
		if err := rows.Scan(&e.EventID, &e.Repo, &e.Number, &e.Title, &e.Kind, &e.Summary, &e.NextStep, &work, &ts); err != nil {
			return nil, 0, err
		}
		e.Work = work != 0
		if e.Number == 0 {
			e.Period = ts[:10] // date portion
		}
		events = append(events, e)
	}

	return events, total, rows.Err()
}

// queryEventObservations returns all observations linked to a specific event.
func queryEventObservations(db *sql.DB, eventID int64) ([]Observation, error) {
	rows, err := db.Query(`
		SELECT o.id, o.source, o.source_id, o.timestamp, o.repo, o.data
		FROM observations o
		JOIN event_observations eo ON o.id = eo.observation_id
		WHERE eo.event_id = ?
		ORDER BY o.timestamp`,
		eventID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var observations []Observation
	for rows.Next() {
		var o Observation
		var ts, dataStr string
		if err := rows.Scan(&o.ID, &o.Source, &o.SourceID, &ts, &o.Repo, &dataStr); err != nil {
			return nil, err
		}
		o.Time, _ = time.Parse(time.RFC3339Nano, ts)
		o.Data = json.RawMessage(dataStr)
		observations = append(observations, o)
	}
	return observations, rows.Err()
}
