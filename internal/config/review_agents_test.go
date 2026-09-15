package config

import (
	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"testing"
)

func TestReviewAgentsProfilesAreIndependent(t *testing.T) {
	global := writeGlobalConfig(t, `agent: codex
agent_config:
  pi: {model: default-model, effort: high}
review_agents:
  reviewer: {agent: pi, model: review-model, effort: max}
  fixer: {agent: pi, model: fix-model}
`)
	cfg := Merge(global, &RepoConfig{})
	reviewer := cfg.ForReviewAgent(cfg.ReviewAgents["reviewer"])
	fixer := cfg.ForReviewAgent(cfg.ReviewAgents["fixer"])
	if got := reviewer.AgentProfile(); got != (agentcfg.Profile{Model: "review-model", Effort: agentcfg.EffortMax}) {
		t.Fatalf("reviewer = %+v", got)
	}
	if got := fixer.AgentProfile(); got != (agentcfg.Profile{Model: "fix-model", Effort: agentcfg.EffortHigh}) {
		t.Fatalf("fixer = %+v", got)
	}
	if cfg.Agent != types.AgentCodex || cfg.AgentProfileFor(types.AgentPi).Model != "default-model" {
		t.Fatal("role selection mutated default configuration")
	}
}

func TestReviewAgentsRejectInvalidConfig(t *testing.T) {
	for _, input := range []string{
		"review_agents: {other: {agent: pi}}",
		"review_agents: {reviewer: {model: x}}",
		"review_agents: {reviewer: {agent: auto}}",
		"review_agents: {fixer: {agent: unknown}}",
		"review_agents: {reviewer: {agent: pi, effort: turbo}}",
		"review_agents: {fixer: {agent: cursor, effort: max}}",
		"review_agents: {fixer: {agent: rovodev, model: x}}",
		"review_agents: {reviewer: {agent: pi, typo: x}}",
	} {
		t.Run(input, func(t *testing.T) { loadGlobalConfigError(t, input) })
	}
}

func TestTrustedRepositorySelectsReviewAgentsPerRole(t *testing.T) {
	global := writeGlobalConfig(t, `review_agents:
  reviewer: {agent: pi, model: operator-review}
  fixer: {agent: pi, model: operator-fix}
`)
	trusted, err := LoadRepoFromBytes([]byte("review_agents: {reviewer: {agent: opencode, model: opencode/muse-spark}}"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := Merge(global, EffectiveRepoConfig(&RepoConfig{}, trusted, false))
	if got := cfg.ReviewAgents["reviewer"]; got.Agent != types.AgentOpenCode || got.Model != "opencode/muse-spark" {
		t.Fatalf("reviewer = %+v, want the repository role", got)
	}
	if got := cfg.ReviewAgents["fixer"]; got.Agent != types.AgentPi || got.Model != "operator-fix" {
		t.Fatalf("fixer = %+v, want the global role the repository left unset", got)
	}
	if global.ReviewAgents["reviewer"].Model != "operator-review" {
		t.Fatal("repository overlay mutated the global roles")
	}
}

func TestPushedBranchCannotSelectReviewAgents(t *testing.T) {
	pushed, err := LoadRepoFromBytes([]byte("review_agents: {reviewer: {agent: pi, model: attacker-model}}"))
	if err != nil {
		t.Fatal(err)
	}
	global := writeGlobalConfig(t, "review_agents: {reviewer: {agent: pi, model: operator-model}}")
	for name, trusted := range map[string]*RepoConfig{"trusted copy without roles": {}, "no trusted copy": nil} {
		t.Run(name, func(t *testing.T) {
			cfg := Merge(global, EffectiveRepoConfig(pushed, trusted, false))
			if cfg.ReviewAgents["reviewer"].Model != "operator-model" {
				t.Fatalf("reviewer = %+v, pushed branch changed the operator profile", cfg.ReviewAgents["reviewer"])
			}
		})
	}
	if got := EffectiveRepoConfig(pushed, nil, true).ReviewAgents["reviewer"].Model; got != "attacker-model" {
		t.Fatalf("allow_repo_commands opt-in reviewer model = %q, want the pushed value", got)
	}
}

func TestRepoReviewAgentsRejectInvalidConfig(t *testing.T) {
	for _, input := range []string{
		"review_agents: {other: {agent: pi}}",
		"review_agents: {reviewer: {agent: auto}}",
		"review_agents: {fixer: {agent: opencode, model: no-provider}}",
	} {
		t.Run(input, func(t *testing.T) {
			if _, err := LoadRepoFromBytes([]byte(input)); err == nil {
				t.Fatal("expected a parse error")
			}
		})
	}
}

func TestReviewAgentsOmitted(t *testing.T) {
	cfg := Merge(writeGlobalConfig(t, "agent: pi\n"), &RepoConfig{})
	if cfg.ReviewAgents != nil {
		t.Fatalf("unexpected roles: %+v", cfg.ReviewAgents)
	}
}
