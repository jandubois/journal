package main

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// scanSessions reads Claude Code session JSONL files and extracts activity metadata.
func scanSessions(since time.Time, cfg *Config, db *sql.DB) ([]Activity, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	projectsDir := filepath.Join(home, ".claude", "projects")

	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", projectsDir, err)
	}

	var activities []Activity
	var observations []Observation
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		projDir := filepath.Join(projectsDir, entry.Name())
		projName := projectDirToName(entry.Name())

		acts, obs, err := scanProjectSessions(projDir, projName, since, cfg)
		if err != nil {
			continue
		}
		activities = append(activities, acts...)
		observations = append(observations, obs...)
	}

	if db != nil && len(observations) > 0 {
		if err := insertObservations(db, observations); err != nil {
			fmt.Fprintf(os.Stderr, "warning: store session observations: %v\n", err)
		}
	}

	return activities, nil
}

func scanProjectSessions(projDir, projName string, since time.Time, cfg *Config) ([]Activity, []Observation, error) {
	entries, err := os.ReadDir(projDir)
	if err != nil {
		return nil, nil, err
	}

	var activities []Activity
	var observations []Observation
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}

		sessionID := strings.TrimSuffix(entry.Name(), ".jsonl")
		path := filepath.Join(projDir, entry.Name())
		a, prompts, err := parseSessionFile(path, projName, cfg)
		if err != nil || a == nil {
			continue
		}

		// Store observation for every session (regardless of since filter).
		obs := sessionToObservation(sessionID, projDir, a, prompts)
		if obs != nil {
			observations = append(observations, *obs)
		}

		// Only include in current report if within the time range.
		sessionEnd := a.Time.Add(a.Duration)
		if sessionEnd.Before(since) {
			continue
		}

		activities = append(activities, *a)
	}

	return activities, observations, nil
}

func parseSessionFile(path, projName string, cfg *Config) (*Activity, []string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()

	var firstTime, prevTime time.Time
	var activeDuration time.Duration
	var prompts []string

	// Gap threshold: if more than 10 minutes pass between messages,
	// assume the user was away. This gives a more accurate "active" duration
	// for sessions that span multiple days or have long idle periods.
	const gapThreshold = 10 * time.Minute

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 256*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var msg sessionMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}

		if msg.Timestamp == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339Nano, msg.Timestamp)
		if err != nil {
			// Try millisecond timestamp format.
			t, err = time.Parse("2006-01-02T15:04:05.000Z", msg.Timestamp)
			if err != nil {
				continue
			}
		}

		if firstTime.IsZero() {
			firstTime = t
		} else if !prevTime.IsZero() {
			gap := t.Sub(prevTime)
			if gap <= gapThreshold {
				activeDuration += gap
			}
		}
		prevTime = t

		if msg.Type == "user" {
			prompt := extractPromptText(msg.Message)
			if prompt != "" && !isSystemPrompt(prompt) {
				prompts = append(prompts, prompt)
			}
		}
	}

	if firstTime.IsZero() {
		return nil, nil, nil
	}

	// Build a summary from the first prompt.
	var details string
	if len(prompts) > 0 {
		details = prompts[0]
		if len(details) > 120 {
			details = details[:120] + "..."
		}
	}

	repo := projectNameToRepo(projName, cfg)
	work := projectIsWork(projName, cfg)

	return &Activity{
		Time:     firstTime,
		Duration: activeDuration,
		Kind:     "session",
		Repo:     repo,
		Details:  details,
		Work:     work,
	}, prompts, nil
}

// extractPromptText pulls the first text block from a message content array.
func extractPromptText(raw json.RawMessage) string {
	// The message field has {"role": "user", "content": [{"type": "text", "text": "..."}]}
	var msg struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return ""
	}
	for _, block := range msg.Content {
		if block.Type == "text" && block.Text != "" {
			text := block.Text
			// Take just the first line.
			if idx := strings.IndexByte(text, '\n'); idx > 0 {
				text = text[:idx]
			}
			return strings.TrimSpace(text)
		}
	}
	return ""
}

// isSystemPrompt detects system-injected messages like skill loading or slash commands.
func isSystemPrompt(prompt string) bool {
	systemPrefixes := []string{
		"Base directory for this skill:",
		"/init",
	}
	for _, prefix := range systemPrefixes {
		if strings.HasPrefix(prompt, prefix) {
			return true
		}
	}
	// Slash commands that start with / followed by a single word.
	if strings.HasPrefix(prompt, "/") && !strings.Contains(strings.TrimPrefix(prompt, "/"), " ") {
		return true
	}
	return false
}

// projectDirToName converts a project directory name to a readable project name.
// e.g. "-Users-jan-git-rancher-desktop-app" -> "rancher-desktop-app"
//      "-Users-jan--claude" -> ".claude"
//      "-Users-jan-Dropbox-git-omnifocus" -> "omnifocus"
//      "-Users-jan-git-git-lint" -> "git-lint"
func projectDirToName(dirName string) string {
	// The directory name encodes the filesystem path: dashes replace slashes,
	// and leading dots become leading dashes (e.g. .claude -> -claude).

	// Strip the home directory prefix, then look for the first "-git-" segment.
	home, _ := os.UserHomeDir()
	homePrefix := strings.ReplaceAll(home, "/", "-")
	rest := strings.TrimPrefix(dirName, homePrefix)
	rest = strings.TrimPrefix(rest, "-")

	// Look for "-git-" which indicates the git repos directory.
	// Use the part after the first occurrence as the project name.
	if idx := strings.Index(rest, "git-"); idx >= 0 {
		name := rest[idx+4:]
		if name != "" {
			return name
		}
	}

	// For paths like "git" (the ~/git directory itself), or dotfiles like ".claude".
	if strings.HasPrefix(rest, "-") {
		return "." + rest[1:]
	}

	return rest
}

// findLocalRepo tries to find a local git repo matching the project name.
// The Claude project directory encoding is lossy (both / and - become -),
// so we try the exact name first, then scan for a match with underscores.
func findLocalRepo(projName string) string {
	home, _ := os.UserHomeDir()

	// Try exact match first.
	repoPath := filepath.Join(home, "git", projName)
	if _, err := os.Stat(filepath.Join(repoPath, ".git")); err == nil {
		return repoPath
	}

	// Try replacing dashes with underscores (common mismatch).
	altName := strings.ReplaceAll(projName, "-", "_")
	if altName != projName {
		repoPath = filepath.Join(home, "git", altName)
		if _, err := os.Stat(filepath.Join(repoPath, ".git")); err == nil {
			return repoPath
		}
	}

	return ""
}

// projectNameToRepo maps a project name to a repo slug.
func projectNameToRepo(projName string, cfg *Config) string {
	if repoPath := findLocalRepo(projName); repoPath != "" {
		return repoSlug(repoPath, cfg)
	}
	return projName
}

// projectIsWork checks if a project is work-related.
func projectIsWork(projName string, cfg *Config) bool {
	if repoPath := findLocalRepo(projName); repoPath != "" {
		return isWorkDir(repoPath, cfg)
	}
	// If no local repo, check if the project name matches a work org.
	return cfg.isWorkRepo(projName)
}

func sessionToObservation(sessionID, projDir string, a *Activity, prompts []string) *Observation {
	data, err := json.Marshal(map[string]any{
		"session_id":       sessionID,
		"project_dir":      filepath.Base(projDir),
		"duration_seconds":  int(a.Duration.Seconds()),
		"prompts":          prompts,
	})
	if err != nil {
		return nil
	}
	return &Observation{
		Source:   "session",
		SourceID: sessionID,
		Time:     a.Time,
		Repo:     a.Repo,
		Work:     a.Work,
		Data:     data,
	}
}

type sessionMessage struct {
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp"`
	Message   json.RawMessage `json:"message"`
}
