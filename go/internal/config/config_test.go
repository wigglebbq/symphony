package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadWorkflowAndParse(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "token-123")
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	content := `---
tracker:
  kind: linear
  project_slug: test
workspace:
  root: ./work
polling:
  interval_ms: "1500"
---
You are working on {{ issue.identifier }}.
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	def, err := LoadWorkflow(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Parse(def)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Tracker.APIKey != "token-123" {
		t.Fatalf("expected env token, got %q", cfg.Tracker.APIKey)
	}
	if cfg.Polling.Interval.String() != "1.5s" {
		t.Fatalf("unexpected polling interval: %s", cfg.Polling.Interval)
	}
}

func TestParseNormalizesLegacyApprovalPolicy(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "token-123")
	def := WorkflowDefinition{
		RawConfig: map[string]any{
			"tracker": map[string]any{
				"kind":         "linear",
				"project_slug": "test",
			},
			"codex": map[string]any{
				"approval_policy": map[string]any{
					"reject": map[string]any{
						"sandbox_approval": true,
					},
				},
			},
		},
	}

	cfg, err := Parse(def)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Codex.ApprovalPolicy != "never" {
		t.Fatalf("expected normalized approval policy 'never', got %q", cfg.Codex.ApprovalPolicy)
	}
	if err := Validate(cfg); err != nil {
		t.Fatalf("expected config to validate, got %v", err)
	}
}

func TestParseNormalizesLegacySandboxValues(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "token-123")
	def := WorkflowDefinition{
		RawConfig: map[string]any{
			"tracker": map[string]any{
				"kind":         "linear",
				"project_slug": "test",
			},
			"codex": map[string]any{
				"thread_sandbox": "workspace-write",
				"turn_sandbox_policy": map[string]any{
					"type": "workspace-write",
					"root": "/tmp/workspace",
				},
			},
		},
	}

	cfg, err := Parse(def)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Codex.ThreadSandbox != "workspaceWrite" {
		t.Fatalf("expected normalized thread sandbox 'workspaceWrite', got %q", cfg.Codex.ThreadSandbox)
	}
	if got := cfg.RuntimeSandboxPolicy("/tmp/workspace")["type"]; got != "workspaceWrite" {
		t.Fatalf("expected normalized runtime sandbox type 'workspaceWrite', got %#v", got)
	}
	if err := Validate(cfg); err != nil {
		t.Fatalf("expected config to validate, got %v", err)
	}
}
