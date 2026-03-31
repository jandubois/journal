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

	authors := authorIdentities(cfg)
	if len(authors) == 0 {
		return fmt.Errorf("no author identity configured")
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

		obs := scanOneRepo(repoPath, authors, sinceStr)
		if len(obs) > 0 {
			if err := insertObservations(db, obs); err != nil {
				fmt.Fprintf(os.Stderr, "warning: store git observations for %s: %v\n", entry.Name(), err)
			}
		}
	}

	return nil
}

// authorIdentities returns all name and email strings to match commits against.
func authorIdentities(cfg *Config) []string {
	seen := make(map[string]bool)
	var authors []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s != "" && !seen[s] {
			seen[s] = true
			authors = append(authors, s)
		}
	}

	add(cfg.authorName())
	add(cfg.Identity.WorkEmail)
	add(cfg.Identity.PersonalEmail)

	// Fall back to global git config.
	if len(authors) == 0 {
		if out, err := runCommand("git", "config", "--global", "user.name"); err == nil {
			add(strings.TrimSpace(out))
		}
		if out, err := runCommand("git", "config", "--global", "user.email"); err == nil {
			add(strings.TrimSpace(out))
		}
	}

	return authors
}

// scanOneRepo scans a single git repo and returns raw observations.
// Searches for commits matching any of the given author identities (name or email).
func scanOneRepo(repoPath string, authors []string, since string) []Observation {
	repo := originSlug(repoPath)
	seen := make(map[string]bool) // deduplicate by commit hash

	var observations []Observation
	for _, author := range authors {
		out, err := runCommand("git", "-C", repoPath, "log",
			"--author="+author,
			"--since="+since,
			"--all",
			"--no-merges",
			"--format=%H|%aI|%s",
		)
		if err != nil || strings.TrimSpace(out) == "" {
			continue
		}

		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			parts := strings.SplitN(line, "|", 3)
			if len(parts) < 3 {
				continue
			}
			hash := parts[0]
			if seen[hash] {
				continue
			}
			seen[hash] = true

			t, err := time.Parse(time.RFC3339, parts[1])
			if err != nil {
				continue
			}
			data, err := json.Marshal(map[string]any{
				"hash":    hash,
				"message": parts[2],
			})
			if err != nil {
				continue
			}
			observations = append(observations, Observation{
				Source:   "git",
				SourceID: hash,
				Time:     t,
				Repo:     repo,
				Data:     data,
			})
		}
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

