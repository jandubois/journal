package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

type Config struct {
	WorkOrgs []string       `json:"workOrgs"`
	Identity IdentityConfig `json:"identity"`
}

type IdentityConfig struct {
	Name          string `json:"name"`
	WorkEmail     string `json:"workEmail"`
	PersonalEmail string `json:"personalEmail"`
}

func loadConfig() (*Config, error) {
	configDir := os.Getenv("XDG_CONFIG_HOME")
	if configDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		configDir = filepath.Join(home, ".config")
	}
	path := filepath.Join(configDir, "git-lint", "config.json")

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// isWorkRepo reports whether the given repo slug (e.g. "rancher-sandbox/rancher-desktop")
// belongs to a work org.
func (c *Config) isWorkRepo(repo string) bool {
	if c == nil {
		return false
	}
	owner, _, ok := strings.Cut(repo, "/")
	if !ok {
		return false
	}
	for _, org := range c.WorkOrgs {
		if strings.EqualFold(owner, org) {
			return true
		}
	}
	return false
}

// isWorkEmail reports whether the given email matches the configured work email.
func (c *Config) isWorkEmail(email string) bool {
	if c == nil || c.Identity.WorkEmail == "" {
		return false
	}
	return strings.EqualFold(email, c.Identity.WorkEmail)
}

// authorName returns the configured identity name for git log filtering.
func (c *Config) authorName() string {
	if c == nil {
		return ""
	}
	return c.Identity.Name
}
