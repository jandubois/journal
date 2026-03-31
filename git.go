package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// scanGitRepos scans all git repos under dir for recent commits by the configured author.
func scanGitRepos(dir string, since time.Time, cfg *Config, db *sql.DB) ([]Activity, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}

	author := cfg.authorName()
	if author == "" {
		// Fall back to global git config.
		out, err := runCommand("git", "config", "--global", "user.name")
		if err != nil {
			return nil, fmt.Errorf("no author name configured")
		}
		author = strings.TrimSpace(out)
	}

	sinceStr := since.Format("2006-01-02T15:04:05")
	var activities []Activity

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		repoPath := filepath.Join(dir, entry.Name())
		gitDir := filepath.Join(repoPath, ".git")
		if _, err := os.Stat(gitDir); err != nil {
			continue
		}

		acts, err := scanOneRepo(repoPath, author, sinceStr, cfg)
		if err != nil {
			continue
		}
		activities = append(activities, acts...)

		if db != nil {
			storeGitObservations(db, acts)
		}
	}

	return activities, nil
}

func scanOneRepo(repoPath, author, since string, cfg *Config) ([]Activity, error) {
	out, err := runCommand("git", "-C", repoPath, "log",
		"--author="+author,
		"--since="+since,
		"--all",
		"--no-merges",
		"--format=%H|%aI|%s",
	)
	if err != nil {
		return nil, err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return nil, nil
	}

	repo := repoSlug(repoPath, cfg)
	work := isWorkDir(repoPath, cfg)

	var activities []Activity
	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(line, "|", 3)
		if len(parts) < 3 {
			continue
		}
		t, err := time.Parse(time.RFC3339, parts[1])
		if err != nil {
			continue
		}
		activities = append(activities, Activity{
			Time:     t,
			Kind:     "commit",
			Repo:     repo,
			URL:      parts[0], // commit hash, used as source ID for observations
			Details:  parts[2],
			IsAuthor: true,
			Work:     work,
		})
	}

	return activities, nil
}

// repoSlug derives a repo slug from a local path.
// For forks, prefer the upstream remote to group with the parent repo.
// Otherwise, use origin. Falls back to the directory name.
func repoSlug(repoPath string, cfg *Config) string {
	// Check upstream remote first — for forks this points to the parent repo.
	out, err := runCommand("git", "-C", repoPath, "remote", "get-url", "upstream")
	if err == nil {
		if slug := slugFromURL(strings.TrimSpace(out)); slug != "" && cfg.isWorkRepo(slug) {
			return slug
		}
	}

	out, err = runCommand("git", "-C", repoPath, "remote", "get-url", "origin")
	if err == nil {
		if slug := slugFromURL(strings.TrimSpace(out)); slug != "" {
			return slug
		}
	}
	return filepath.Base(repoPath)
}

// slugFromURL extracts "owner/repo" from a GitHub URL.
func slugFromURL(url string) string {
	// SSH: git@github.com:owner/repo.git
	if strings.Contains(url, "github.com:") {
		parts := strings.SplitN(url, "github.com:", 2)
		if len(parts) == 2 {
			return strings.TrimSuffix(parts[1], ".git")
		}
	}
	// HTTPS: https://github.com/owner/repo.git
	if strings.Contains(url, "github.com/") {
		parts := strings.SplitN(url, "github.com/", 2)
		if len(parts) == 2 {
			return strings.TrimSuffix(parts[1], ".git")
		}
	}
	return ""
}

// isWorkDir checks if a local repo is work-related by examining its remote URLs
// and per-repo email configuration.
func isWorkDir(repoPath string, cfg *Config) bool {
	// Check remote URLs.
	out, _ := runCommand("git", "-C", repoPath, "remote", "--verbose")
	for _, line := range strings.Split(out, "\n") {
		for _, org := range cfg.WorkOrgs {
			if strings.Contains(line, "github.com/"+org+"/") ||
				strings.Contains(line, "github.com:"+org+"/") {
				return true
			}
		}
	}

	// Check per-repo user.email.
	email, err := runCommand("git", "-C", repoPath, "config", "user.email")
	if err == nil && cfg.isWorkEmail(strings.TrimSpace(email)) {
		return true
	}

	return false
}

func storeGitObservations(db *sql.DB, activities []Activity) {
	var observations []Observation
	for _, a := range activities {
		data, err := json.Marshal(map[string]any{
			"hash":    a.URL, // commit hash stored in URL field
			"message": a.Details,
		})
		if err != nil {
			continue
		}
		observations = append(observations, Observation{
			Source:   "git",
			SourceID: a.URL, // commit hash
			Time:     a.Time,
			Repo:     a.Repo,
			Work:     a.Work,
			Data:     data,
		})
	}
	if len(observations) > 0 {
		if err := insertObservations(db, observations); err != nil {
			fmt.Fprintf(os.Stderr, "warning: store git observations: %v\n", err)
		}
	}
}
