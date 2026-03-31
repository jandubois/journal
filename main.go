package main

import (
	"database/sql"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	since := flag.String("since", "1d", "how far back to look (1d, 2d, 1w, or ISO date)")
	format := flag.String("format", "text", "output format: text or markdown")
	repos := flag.String("repos", defaultReposDir(), "directory containing git repos")
	user := flag.String("user", "", "GitHub username (default: from gh api user)")
	flag.Parse()

	cutoff, err := parseSince(*since)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid --since value %q: %v\n", *since, err)
		os.Exit(1)
	}

	if *user == "" {
		*user, err = detectGitHubUser()
		if err != nil {
			fmt.Fprintf(os.Stderr, "could not detect GitHub user: %v\n", err)
			os.Exit(1)
		}
	}

	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not load config: %v\n", err)
		cfg = &Config{}
	}

	db, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: could not open database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	// Collect: fetch latest data from all sources into the database.
	collect(*user, *repos, cfg, db)

	// Process: convert observations into grouped events.
	observations, err := queryObservations(db, cutoff)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: query observations: %v\n", err)
		os.Exit(1)
	}
	activities := observationsToActivities(observations, cfg, *user)
	topics := groupActivities(activities)
	inferNextSteps(topics)

	if err := processEvents(db, topics); err != nil {
		fmt.Fprintf(os.Stderr, "warning: process events: %v\n", err)
	}

	switch *format {
	case "text":
		renderText(os.Stdout, topics)
	case "markdown":
		renderMarkdown(os.Stdout, topics)
	default:
		fmt.Fprintf(os.Stderr, "unknown format %q\n", *format)
		os.Exit(1)
	}
}

// collect fetches latest data from all sources and writes observations to the database.
// It fetches broadly (not limited to the --since range) to capture as much as possible.
func collect(user, reposDir string, cfg *Config, db *sql.DB) {
	if err := fetchGitHubEvents(user, db); err != nil {
		fmt.Fprintf(os.Stderr, "warning: GitHub events: %v\n", err)
	}

	gitCutoff := time.Now().AddDate(0, -3, 0)
	if err := scanGitRepos(reposDir, gitCutoff, cfg, db); err != nil {
		fmt.Fprintf(os.Stderr, "warning: git repos: %v\n", err)
	}

	if err := scanSessions(db); err != nil {
		fmt.Fprintf(os.Stderr, "warning: Claude sessions: %v\n", err)
	}
}

func defaultReposDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "~/git"
	}
	return filepath.Join(home, "git")
}

func parseSince(s string) (time.Time, error) {
	now := time.Now()

	if t, err := time.ParseInLocation("2006-01-02", s, time.Local); err == nil {
		return t, nil
	}

	s = strings.TrimSpace(s)
	if len(s) < 2 {
		return time.Time{}, fmt.Errorf("too short")
	}

	unit := s[len(s)-1]
	numStr := s[:len(s)-1]
	var n int
	if _, err := fmt.Sscanf(numStr, "%d", &n); err != nil {
		return time.Time{}, fmt.Errorf("cannot parse number %q", numStr)
	}

	switch unit {
	case 'd':
		return now.AddDate(0, 0, -n), nil
	case 'w':
		return now.AddDate(0, 0, -n*7), nil
	default:
		return time.Time{}, fmt.Errorf("unknown unit %q (use d or w)", string(unit))
	}
}

func detectGitHubUser() (string, error) {
	out, err := runCommand("gh", "api", "user", "--jq", ".login")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}
