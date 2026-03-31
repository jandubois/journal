package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func main() {
	// Check for subcommands before flag parsing.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "pending":
			cmdPending(os.Args[2:])
			return
		case "set-summary":
			cmdSetSummary(os.Args[2:])
			return
		case "observations":
			cmdObservations(os.Args[2:])
			return
		}
	}

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

	collect(*user, *repos, cfg, db)

	observations, err := queryObservations(db, cutoff)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: query observations: %v\n", err)
		os.Exit(1)
	}
	activities := observationsToActivities(observations, cfg, *user)
	topics := groupActivities(activities)

	enrichTopicsWithHistory(db, topics, observations, cfg, *user)
	inferNextSteps(topics)

	if err := processEvents(db, topics); err != nil {
		fmt.Fprintf(os.Stderr, "warning: process events: %v\n", err)
	}

	// Apply AI summaries where available.
	applyAISummaries(db, topics)

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

// cmdPending outputs events that lack AI summaries, as JSON for the /summarize skill.
func cmdPending(args []string) {
	fs := flag.NewFlagSet("pending", flag.ExitOnError)
	since := fs.String("since", "1w", "how far back to look")
	limit := fs.Int("limit", 10, "maximum events to return")
	fs.Parse(args)

	db, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	cutoff, err := parseSince(*since)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid --since: %v\n", err)
		os.Exit(1)
	}

	pending, total, err := queryPendingSummaries(db, cutoff, *limit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	output := map[string]any{
		"events":          pending,
		"returned":        len(pending),
		"total_pending":   total,
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.Encode(output)
}

// cmdSetSummary stores an AI-generated summary for a topic.
func cmdSetSummary(args []string) {
	fs := flag.NewFlagSet("set-summary", flag.ExitOnError)
	repo := fs.String("repo", "", "repository slug")
	number := fs.Int("number", 0, "PR/issue number (0 for repo-level)")
	period := fs.String("period", "", "time period for repo-level events")
	fs.Parse(args)

	summary := strings.Join(fs.Args(), " ")
	if *repo == "" || summary == "" {
		fmt.Fprintf(os.Stderr, "usage: journal set-summary --repo OWNER/REPO [--number N] [--period DATE] SUMMARY\n")
		os.Exit(1)
	}

	db, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	if err := storeAISummary(db, *repo, *number, *period, summary); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// cmdObservations outputs raw observations for a specific event, for the /summarize skill.
func cmdObservations(args []string) {
	fs := flag.NewFlagSet("observations", flag.ExitOnError)
	eventID := fs.Int("event", 0, "event ID to show observations for")
	fs.Parse(args)

	if *eventID == 0 {
		fmt.Fprintf(os.Stderr, "usage: journal observations --event ID\n")
		os.Exit(1)
	}

	db, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	obs, err := queryEventObservations(db, int64(*eventID))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.Encode(obs)
}

// applyAISummaries looks up AI summaries for each topic and applies them.
func applyAISummaries(db *sql.DB, topics []*Topic) {
	for _, t := range topics {
		period := topicPeriod(t)
		summary, err := getAISummary(db, t.Repo, t.Number, period)
		if err != nil || summary == "" {
			continue
		}
		t.AISummary = summary
	}
}

func topicPeriod(t *Topic) string {
	if t.Number > 0 {
		return ""
	}
	if len(t.Activities) == 0 {
		return ""
	}
	return t.Activities[0].Time.Format("2006-01-02")
}

func enrichTopicsWithHistory(db *sql.DB, topics []*Topic, observations []Observation, cfg *Config, user string) {
	rawRepoByObsID := make(map[int64]string)
	for _, o := range observations {
		rawRepoByObsID[o.ID] = o.Repo
	}

	var repoNumbers [][2]string
	seen := make(map[string]bool)
	for _, t := range topics {
		if t.Number == 0 {
			continue
		}
		numStr := fmt.Sprintf("%d", t.Number)
		for _, a := range t.Activities {
			rawRepo := rawRepoByObsID[a.ObservationID]
			if rawRepo == "" {
				rawRepo = t.Repo
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
	case 'm':
		return now.AddDate(0, -n, 0), nil
	default:
		return time.Time{}, fmt.Errorf("unknown unit %q (use d, w, or m)", string(unit))
	}
}

func detectGitHubUser() (string, error) {
	out, err := runCommand("gh", "api", "user", "--jq", ".login")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

