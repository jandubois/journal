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

// scanSessions reads Claude Code session JSONL files and stores raw observations.
func scanSessions(db *sql.DB) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	projectsDir := filepath.Join(home, ".claude", "projects")

	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		return fmt.Errorf("read %s: %w", projectsDir, err)
	}

	var observations []Observation
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		projDir := filepath.Join(projectsDir, entry.Name())
		projName := projectDirToName(entry.Name())

		obs := scanProjectSessions(projDir, projName)
		observations = append(observations, obs...)
	}

	if len(observations) > 0 {
		if err := insertObservations(db, observations); err != nil {
			return fmt.Errorf("store session observations: %w", err)
		}
	}

	return nil
}

func scanProjectSessions(projDir, projName string) []Observation {
	entries, err := os.ReadDir(projDir)
	if err != nil {
		return nil
	}

	repo := projectOriginSlug(projName)
	archiveDir := sessionArchiveDir()

	var observations []Observation
	for _, entry := range entries {
		name := entry.Name()

		if !entry.IsDir() && strings.HasSuffix(name, ".jsonl") {
			// Main session file.
			sessionID := strings.TrimSuffix(name, ".jsonl")
			srcPath := filepath.Join(projDir, name)
			startTime, duration, prompts := parseSessionFile(srcPath)
			if startTime.IsZero() {
				continue
			}

			archivePath := archiveSessionFile(srcPath, projName, sessionID, archiveDir)

			data, err := json.Marshal(map[string]any{
				"session_id":       sessionID,
				"project_dir":      filepath.Base(projDir),
				"project_name":     projName,
				"duration_seconds":  int(duration.Seconds()),
				"prompts":          prompts,
				"archive_path":     archivePath,
			})
			if err != nil {
				continue
			}

			observations = append(observations, Observation{
				Source:   "session",
				SourceID: sessionID,
				Time:     startTime,
				Repo:     repo,
				Data:     data,
			})
		}

		if entry.IsDir() {
			// Session subdirectory — may contain subagent JSONL files.
			parentSessionID := name
			observations = append(observations,
				scanSubagentSessions(filepath.Join(projDir, name), parentSessionID, projName, repo, archiveDir)...)
		}
	}

	return observations
}

func scanSubagentSessions(sessionDir, parentSessionID, projName, repo, archiveDir string) []Observation {
	subagentsDir := filepath.Join(sessionDir, "subagents")
	entries, err := os.ReadDir(subagentsDir)
	if err != nil {
		return nil
	}

	var observations []Observation
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}

		agentID := strings.TrimSuffix(entry.Name(), ".jsonl")
		srcPath := filepath.Join(subagentsDir, entry.Name())
		startTime, duration, prompts := parseSessionFile(srcPath)
		if startTime.IsZero() {
			continue
		}

		// Archive under the same project, with parent session prefix.
		archiveSubDir := filepath.Join(projName, parentSessionID)
		archivePath := archiveSessionFile(srcPath, archiveSubDir, agentID, archiveDir)

		sourceID := parentSessionID + "/" + agentID

		data, err := json.Marshal(map[string]any{
			"session_id":        sourceID,
			"parent_session_id": parentSessionID,
			"agent_id":          agentID,
			"project_name":      projName,
			"duration_seconds":   int(duration.Seconds()),
			"prompts":           prompts,
			"archive_path":      archivePath,
		})
		if err != nil {
			continue
		}

		observations = append(observations, Observation{
			Source:   "session",
			SourceID: sourceID,
			Time:     startTime,
			Repo:     repo,
			Data:     data,
		})
	}

	return observations
}

func sessionArchiveDir() string {
	dataDir := os.Getenv("XDG_DATA_HOME")
	if dataDir == "" {
		home, _ := os.UserHomeDir()
		dataDir = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(dataDir, "journal", "sessions")
}

// archiveSessionFile copies a session JSONL file to the archive directory.
// Re-copies if the source is newer or larger than the archive (live sessions grow).
func archiveSessionFile(srcPath, projName, sessionID, archiveDir string) string {
	destDir := filepath.Join(archiveDir, projName)
	destPath := filepath.Join(destDir, sessionID+".jsonl")

	srcInfo, err := os.Stat(srcPath)
	if err != nil {
		return ""
	}

	// Skip if archive exists and is at least as large as the source.
	if destInfo, err := os.Stat(destPath); err == nil {
		if destInfo.Size() >= srcInfo.Size() {
			return destPath
		}
	}

	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return ""
	}

	src, err := os.ReadFile(srcPath)
	if err != nil {
		return ""
	}
	if err := os.WriteFile(destPath, src, 0o600); err != nil {
		return ""
	}

	return destPath
}

// parseSessionFile extracts raw metadata from a session JSONL file.
// Returns zero startTime for files with no timestamped messages (e.g. pure
// file-history snapshots). These are skipped by the caller — they contain
// no conversation content useful for summarization.
func parseSessionFile(path string) (startTime time.Time, duration time.Duration, prompts []string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	var prevTime time.Time

	// Gap threshold: if more than 10 minutes pass between messages,
	// assume the user was away.
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
			t, err = time.Parse("2006-01-02T15:04:05.000Z", msg.Timestamp)
			if err != nil {
				continue
			}
		}

		if startTime.IsZero() {
			startTime = t
		} else if !prevTime.IsZero() {
			gap := t.Sub(prevTime)
			if gap <= gapThreshold {
				duration += gap
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

	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: incomplete session parse %s: %v\n", path, err)
	}

	return
}

// extractPromptText pulls the full text from a message content array.
func extractPromptText(raw json.RawMessage) string {
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
			return strings.TrimSpace(block.Text)
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

// projectOriginSlug returns the origin remote slug for a project (raw, no fork resolution).
func projectOriginSlug(projName string) string {
	if repoPath := findLocalRepo(projName); repoPath != "" {
		return originSlug(repoPath)
	}
	return projName
}

type sessionMessage struct {
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp"`
	Message   json.RawMessage `json:"message"`
}
