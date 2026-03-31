package main

import (
	"database/sql"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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

	// Load full history for numbered topics so next-step inference
	// sees the complete picture (e.g. a PR opened last week but reviewed today).
	enrichTopicsWithHistory(db, topics, observations, cfg, *user)
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
// enrichTopicsWithHistory loads full observation history for numbered topics
// (PRs, issues) so that next-step inference sees events from before the --since
// window. Without this, a PR opened last week but reviewed today would appear
// as if the user is only a reviewer, yielding wrong next steps.
func enrichTopicsWithHistory(db *sql.DB, topics []*Topic, observations []Observation, cfg *Config, user string) {
	// Build a map from ObservationID → raw repo slug. Activities carry the
	// resolved slug; we need the raw slug to query the observations table.
	rawRepoByObsID := make(map[int64]string)
	for _, o := range observations {
		rawRepoByObsID[o.ID] = o.Repo
	}

	// Collect (raw repo slug, number) pairs for numbered topics.
	var repoNumbers [][2]string
	seen := make(map[string]bool)
	for _, t := range topics {
		if t.Number == 0 {
			continue
		}
		numStr := fmt.Sprintf("%d", t.Number)

		// Find all raw repo slugs from this topic's observations.
		for _, a := range t.Activities {
			rawRepo := rawRepoByObsID[a.ObservationID]
			if rawRepo == "" {
				rawRepo = t.Repo // fallback to resolved
			}
			key := fmt.Sprintf("%s#%s", rawRepo, numStr)
			if seen[key] {
				continue
			}
			seen[key] = true
			repoNumbers = append(repoNumbers, [2]string{rawRepo, numStr})
		}
	}

	if len(repoNumbers) == 0 {
		return
	}

	histObs, err := queryTopicHistory(db, repoNumbers)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: load topic history: %v\n", err)
		return
	}
	if len(histObs) == 0 {
		return
	}

	histActivities := observationsToActivities(histObs, cfg, user)

	// Merge historical activities into existing topics.
	existingIDs := make(map[int64]bool)
	for _, t := range topics {
		for _, a := range t.Activities {
			existingIDs[a.ObservationID] = true
		}
	}

	topicMap := make(map[string]*Topic)
	for _, t := range topics {
		topicMap[topicKey(Activity{Repo: t.Repo, Number: t.Number})] = t
	}

	for _, a := range histActivities {
		if existingIDs[a.ObservationID] {
			continue
		}
		key := topicKey(a)
		if t, ok := topicMap[key]; ok {
			t.Activities = append(t.Activities, a)
		}
	}

	// Re-sort activities within enriched topics.
	for _, t := range topics {
		sort.Slice(t.Activities, func(i, j int) bool {
			return t.Activities[i].Time.Before(t.Activities[j].Time)
		})
	}
}

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
