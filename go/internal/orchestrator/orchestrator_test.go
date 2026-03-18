package orchestrator

import (
	"testing"

	"github.com/openai/symphony/go/internal/config"
	"github.com/openai/symphony/go/internal/linear"
)

func TestEligibleWithReasonBlocksTodoWithOpenDependency(t *testing.T) {
	orch := &Orchestrator{
		cfg: config.Config{
			Tracker: config.TrackerConfig{
				ActiveStates:   []string{"Todo", "In Progress"},
				TerminalStates: []string{"Done", "Closed"},
			},
			Agent: config.AgentConfig{
				MaxConcurrentAgents:        10,
				MaxConcurrentAgentsByState: map[string]int{},
			},
		},
		running:  map[string]*runningEntry{},
		claimed:  map[string]struct{}{},
		retrying: map[string]retryEntry{},
	}

	ok, reason := orch.eligibleWithReason(linear.Issue{
		ID:         "1",
		Identifier: "WIG-28",
		Title:      "Backlog",
		State:      "Todo",
		BlockedBy: []linear.BlockerRef{{
			ID:         "2",
			Identifier: "WIG-29",
			State:      "In Progress",
		}},
	}, false)

	if ok {
		t.Fatalf("expected issue to be blocked")
	}
	if reason != "blocked_by_open_dependency" {
		t.Fatalf("unexpected reason: %q", reason)
	}
}

func TestSnapshotIncludesSchedulerDiagnostics(t *testing.T) {
	orch := &Orchestrator{
		running:  map[string]*runningEntry{},
		claimed:  map[string]struct{}{},
		retrying: map[string]retryEntry{},
	}

	snapshot := orch.Snapshot()
	scheduler, ok := snapshot["scheduler"].(map[string]any)
	if !ok {
		t.Fatalf("expected scheduler diagnostics in snapshot")
	}
	if scheduler["last_poll_candidate_count"] != 0 {
		t.Fatalf("unexpected last_poll_candidate_count: %#v", scheduler["last_poll_candidate_count"])
	}
}
