package config

import (
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// ReviewAgent pins one review-loop role to an explicit harness. Empty model or
// effort inherits agent_config for that harness; native argument overrides win.
type ReviewAgent struct {
	Agent  types.AgentName `yaml:"agent"`
	Model  string          `yaml:"model"`
	Effort agentcfg.Effort `yaml:"effort"`
}

func validateReviewAgents(roles map[string]ReviewAgent) error {
	for role, entry := range roles {
		if role != "reviewer" && role != "fixer" {
			return fmt.Errorf("invalid review_agents role %q (valid: reviewer, fixer)", role)
		}
		if !agentcfg.Known(entry.Agent) {
			return fmt.Errorf("review_agents.%s.agent must name an explicit harness, got %q", role, entry.Agent)
		}
		if err := agentcfg.Validate(entry.Agent, agentcfg.Profile{Model: strings.TrimSpace(entry.Model), Effort: entry.Effort}); err != nil {
			return fmt.Errorf("invalid review_agents.%s: %w", role, err)
		}
	}
	return nil
}

// ForReviewAgent returns an isolated configuration for a role without mutating
// shared per-harness profiles (both roles may use the same harness).
func (c *Config) ForReviewAgent(entry ReviewAgent) *Config {
	role := *c
	role.Agent = entry.Agent
	role.Agents = []types.AgentName{entry.Agent}
	profile := c.AgentProfileFor(entry.Agent)
	if entry.Model != "" {
		profile.Model = strings.TrimSpace(entry.Model)
	}
	if entry.Effort != "" {
		profile.Effort = entry.Effort
	}
	role.AgentConfig = map[string]agentcfg.Profile{string(entry.Agent): profile}
	role.ReviewAgents = nil
	return &role
}

// mergeReviewAgents overlays repository roles onto the operator's global roles,
// one role at a time: a repository that pins only the reviewer keeps the global
// fixer. repo is the EffectiveRepoConfig result, so it is already trusted-only.
func mergeReviewAgents(global, repo map[string]ReviewAgent) map[string]ReviewAgent {
	if len(repo) == 0 {
		return global
	}
	merged := copyReviewAgents(global)
	if merged == nil {
		merged = make(map[string]ReviewAgent, len(repo))
	}
	for role, entry := range repo {
		merged[role] = entry
	}
	return merged
}

func copyReviewAgents(roles map[string]ReviewAgent) map[string]ReviewAgent {
	if roles == nil {
		return nil
	}
	out := make(map[string]ReviewAgent, len(roles))
	for role, entry := range roles {
		out[role] = entry
	}
	return out
}
