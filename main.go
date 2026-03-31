package main

import (
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
		fmt.Fprintf(os.Stderr, "warning: could not open database: %v\n", err)
	}
	if db != nil {
		defer db.Close()
	}

	var activities []Activity

	ghActivities, err := fetchGitHubEvents(*user, cutoff, cfg, db)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: GitHub events: %v\n", err)
	}
	activities = append(activities, ghActivities...)

	gitActivities, err := scanGitRepos(*repos, cutoff, cfg, db)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: git repos: %v\n", err)
	}
	activities = append(activities, gitActivities...)

	sessionActivities, err := scanSessions(cutoff, cfg, db)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: Claude sessions: %v\n", err)
	}
	activities = append(activities, sessionActivities...)

	topics := groupActivities(activities)
	inferNextSteps(topics)

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

func defaultReposDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "~/git"
	}
	return filepath.Join(home, "git")
}

func parseSince(s string) (time.Time, error) {
	now := time.Now()

	if t, err := time.Parse("2006-01-02", s); err == nil {
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
