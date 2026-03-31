package main

import (
	"fmt"
	"sort"
	"time"
)

type Activity struct {
	Time     time.Time     `json:"time"`
	Duration time.Duration `json:"duration,omitempty"`
	Kind     string        `json:"kind"`
	Repo     string        `json:"repo"`
	Number   int           `json:"number,omitempty"`
	Title    string        `json:"title,omitempty"`
	URL      string        `json:"url,omitempty"`
	Details  string        `json:"details,omitempty"`
	IsAuthor bool          `json:"isAuthor,omitempty"`
	Work     bool          `json:"work,omitempty"`
}

type Topic struct {
	Repo       string
	Number     int
	Title      string
	URL        string
	Work       bool
	Activities []Activity
	NextStep   string
}

// topicKey returns a grouping key for an activity.
func topicKey(a Activity) string {
	return fmt.Sprintf("%s#%d", a.Repo, a.Number)
}

// groupActivities groups activities into topics, sorted by most recent activity.
func groupActivities(activities []Activity) []*Topic {
	byKey := make(map[string]*Topic)
	for _, a := range activities {
		key := topicKey(a)
		t, ok := byKey[key]
		if !ok {
			t = &Topic{
				Repo:   a.Repo,
				Number: a.Number,
				Work:   a.Work,
			}
			byKey[key] = t
		}
		t.Activities = append(t.Activities, a)

		// Keep the best title and URL we've seen.
		if a.Title != "" && t.Title == "" {
			t.Title = a.Title
		}
		if a.URL != "" && t.URL == "" {
			t.URL = a.URL
		}
	}

	topics := make([]*Topic, 0, len(byKey))
	for _, t := range byKey {
		// Sort activities within each topic chronologically.
		sort.Slice(t.Activities, func(i, j int) bool {
			return t.Activities[i].Time.Before(t.Activities[j].Time)
		})
		topics = append(topics, t)
	}

	// Sort topics by most recent activity, descending.
	sort.Slice(topics, func(i, j int) bool {
		ti := topics[i].Activities[len(topics[i].Activities)-1].Time
		tj := topics[j].Activities[len(topics[j].Activities)-1].Time
		return ti.After(tj)
	})

	return topics
}
