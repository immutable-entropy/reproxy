package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// AgentProfile is one configured AI agent: a vendor, a model and how to
// reach it. Its API key is kept in the secrets file under ID.
type AgentProfile struct {
	// ID is a stable random identifier; it keys the API key and appears in
	// saved sessions.
	ID string `json:"id"`
	// Vendor names the provider implementation (see package agent).
	Vendor string `json:"vendor"`
	// Name is what the user calls it ("Claude", "Local Qwen").
	Name string `json:"name"`
	// Model is the vendor's model ID.
	Model string `json:"model"`
	// BaseURL overrides the vendor's API endpoint; empty uses its default.
	BaseURL string `json:"base_url,omitempty"`
	// MaxTokens caps each model response; 0 uses the vendor default.
	MaxTokens int `json:"max_tokens,omitempty"`
	// ReviewFindings asks a background agent session to watch for new findings
	// and score each one's confidence as it is recorded (the findings reviewer
	// in package app). Nil means the default — on — so a newly defined agent
	// reviews findings until it is turned off; the first profile with it on is
	// the one designated to review.
	ReviewFindings *bool `json:"review_findings,omitempty"`
}

// ReviewsFindings reports whether this agent should review new findings. The
// default (a nil flag) is on, so a newly defined agent, or one from before the
// toggle existed, reviews unless it is explicitly turned off.
func (p AgentProfile) ReviewsFindings() bool {
	return p.ReviewFindings == nil || *p.ReviewFindings
}

// Agents is the agent configuration, kept in agents.json beside the
// settings file. It is separate from Config because only the hub process
// writes it (other processes ask the hub to), whereas every process saves
// its own Config.
type Agents struct {
	Profiles []AgentProfile `json:"profiles"`
}

// Profile returns the profile with the given ID.
func (a Agents) Profile(id string) (AgentProfile, bool) {
	for _, p := range a.Profiles {
		if p.ID == id {
			return p, true
		}
	}
	return AgentProfile{}, false
}

// Validate reports whether a can be saved.
func (a Agents) Validate() error {
	seen := make(map[string]bool)
	for i, p := range a.Profiles {
		switch {
		case p.ID == "":
			return fmt.Errorf("agent %d has no ID", i+1)
		case seen[p.ID]:
			return fmt.Errorf("agent ID %q is used twice", p.ID)
		case p.Vendor == "":
			return fmt.Errorf("agent %q has no vendor", p.Name)
		case p.Name == "":
			return fmt.Errorf("an agent has no name")
		case p.MaxTokens < 0:
			return fmt.Errorf("agent %q: max tokens can't be negative", p.Name)
		}
		seen[p.ID] = true
	}
	return nil
}

// AgentsPath returns agents.json beside the settings file at configPath.
func AgentsPath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "agents.json")
}

// LoadAgents reads the agent configuration; a missing file means none.
func LoadAgents(path string) (Agents, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Agents{}, nil
	}
	if err != nil {
		return Agents{}, fmt.Errorf("reading agents: %w", err)
	}
	var a Agents
	if err := json.Unmarshal(data, &a); err != nil {
		return Agents{}, fmt.Errorf("parsing %s: %w", path, err)
	}
	return a, a.Validate()
}

// SaveAgents writes the agent configuration atomically.
func SaveAgents(path string, a Agents) error {
	if err := a.Validate(); err != nil {
		return err
	}
	return writeJSON(path, a)
}
