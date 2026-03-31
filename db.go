package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS observations (
    id           INTEGER PRIMARY KEY,
    source       TEXT NOT NULL,
    source_id    TEXT NOT NULL,
    timestamp    TEXT NOT NULL,
    repo         TEXT,
    work         INTEGER NOT NULL,
    data         TEXT NOT NULL,
    collected_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE(source, source_id)
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
`

type Observation struct {
	Source   string          `json:"source"`
	SourceID string          `json:"source_id"`
	Time     time.Time       `json:"time"`
	Repo     string          `json:"repo"`
	Work     bool            `json:"work"`
	Data     json.RawMessage `json:"data"`
}

func openDB() (*sql.DB, error) {
	dbPath := dbFilePath()
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}

	db, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(wal)&_pragma=foreign_keys(on)")
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}

	return db, nil
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
		`INSERT OR IGNORE INTO observations (source, source_id, timestamp, repo, work, data)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		o.Source,
		o.SourceID,
		o.Time.UTC().Format(time.RFC3339Nano),
		o.Repo,
		boolToInt(o.Work),
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
		`INSERT OR IGNORE INTO observations (source, source_id, timestamp, repo, work, data)
		 VALUES (?, ?, ?, ?, ?, ?)`,
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
			boolToInt(o.Work),
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
		`SELECT source, source_id, timestamp, repo, work, data
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
		var ts string
		var work int
		if err := rows.Scan(&o.Source, &o.SourceID, &ts, &o.Repo, &work, &o.Data); err != nil {
			return nil, err
		}
		o.Time, _ = time.Parse(time.RFC3339Nano, ts)
		o.Work = work != 0
		observations = append(observations, o)
	}
	return observations, rows.Err()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
