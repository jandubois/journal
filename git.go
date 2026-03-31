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

// scanGitRepos scans all git repos under dir for recent commits and stores raw observations.
func scanGitRepos(dir string, since time.Time, cfg *Config, db *sql.DB) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read %s: %w", dir, err)
	}

	author := cfg.authorName()
	if author == "" {
		out, err := runCommand("git", "config", "--global", "user.name")
		if err != nil {
			return fmt.Errorf("no author name configured")
		}
		author = strings.TrimSpace(out)
	}

	sinceStr := since.Format("2006-01-02T15:04:05")

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		repoPath := filepath.Join(dir, entry.Name())
		gitDir := filepath.Join(repoPath, ".git")
		if _, err := os.Stat(gitDir); err != nil {
			continue
		}

		obs := scanOneRepo(repoPath, author, sinceStr)
		if len(obs) > 0 {
			if err := insertObservations(db, obs); err != nil {
				fmt.Fprintf(os.Stderr, "warning: store git observations for %s: %v\n", entry.Name(), err)
			}
		}
	}

	return nil
}

// scanOneRepo scans a single git repo and returns raw observations.
// Uses the origin remote slug (no upstream/fork resolution).
func scanOneRepo(repoPath, author, since string) []Observation {
	out, err := runCommand("git", "-C", repoPath, "log",
		"--author="+author,
		"--since="+since,
		"--all",
		"--no-merges",
		"--format=%H|%aI|%s",
	)
	if err != nil {
		return nil
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return nil
	}

	// Use origin slug (raw, no upstream preference).
	repo := originSlug(repoPath)

	var observations []Observation
	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(line, "|", 3)
		if len(parts) < 3 {
			continue
		}
		t, err := time.Parse(time.RFC3339, parts[1])
		if err != nil {
			continue
		}
		data, err := json.Marshal(map[string]any{
			"hash":    parts[0],
			"message": parts[2],
		})
		if err != nil {
			continue
		}
		observations = append(observations, Observation{
			Source:   "git",
			SourceID: parts[0], // commit hash
			Time:     t,
			Repo:     repo,
			Data:     data,
		})
	}

	return observations
}

// originSlug returns the repo slug from the origin remote (no fork resolution).
func originSlug(repoPath string) string {
	out, err := runCommand("git", "-C", repoPath, "remote", "get-url", "origin")
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

