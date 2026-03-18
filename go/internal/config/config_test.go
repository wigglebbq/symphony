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
	policy := cfg.RuntimeSandboxPolicy("/tmp/workspace")
	if got := policy["type"]; got != "workspaceWrite" {
		t.Fatalf("expected normalized runtime sandbox type 'workspaceWrite', got %#v", got)
	}
	if got := policy["root"]; got != "/tmp/workspace" {
		t.Fatalf("expected explicit turn sandbox policy to pass through root, got %#v", got)
	}
	if err := Validate(cfg); err != nil {
		t.Fatalf("expected config to validate, got %v", err)
	}
}

func TestRuntimeSandboxPolicyDefaultsMatchWorkspaceWriteShape(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "token-123")
	def := WorkflowDefinition{
		RawConfig: map[string]any{
			"tracker": map[string]any{
				"kind":         "linear",
				"project_slug": "test",
			},
			"codex": map[string]any{
				"thread_sandbox": "workspace-write",
			},
		},
	}

	cfg, err := Parse(def)
	if err != nil {
		t.Fatal(err)
	}
	policy := cfg.RuntimeSandboxPolicy("/tmp/workspace")
	if got := policy["type"]; got != "workspaceWrite" {
		t.Fatalf("expected normalized runtime sandbox type 'workspaceWrite', got %#v", got)
	}
	if got, ok := policy["writableRoots"].([]string); !ok || len(got) != 1 || got[0] != "/tmp/workspace" {
		t.Fatalf("expected writableRoots to include workspace, got %#v", policy["writableRoots"])
	}
	if got := policy["networkAccess"]; got != false {
		t.Fatalf("expected default networkAccess=false, got %#v", got)
	}
	if err := Validate(cfg); err != nil {
		t.Fatalf("expected config to validate, got %v", err)
	}
}

func TestParseSupportsDangerFullAccessSandbox(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "token-123")
	def := WorkflowDefinition{
		RawConfig: map[string]any{
			"tracker": map[string]any{
				"kind":         "linear",
				"project_slug": "test",
			},
			"codex": map[string]any{
				"thread_sandbox": "danger-full-access",
				"turn_sandbox_policy": map[string]any{
					"type": "danger-full-access",
				},
			},
		},
	}

	cfg, err := Parse(def)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Codex.ThreadSandbox != "dangerFullAccess" {
		t.Fatalf("expected normalized thread sandbox 'dangerFullAccess', got %q", cfg.Codex.ThreadSandbox)
	}
	if got := cfg.RuntimeSandboxPolicy("/tmp/workspace")["type"]; got != "dangerFullAccess" {
		t.Fatalf("expected danger-full-access turn policy to pass through, got %#v", got)
	}
	if err := Validate(cfg); err != nil {
		t.Fatalf("expected config to validate, got %v", err)
	}
}

func TestParseSupportsTmuxSessionPrefix(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "token-123")
	def := WorkflowDefinition{
		RawConfig: map[string]any{
			"tracker": map[string]any{
				"kind":         "linear",
				"project_slug": "test",
			},
			"codex": map[string]any{
				"tmux_session_prefix": "symphony",
			},
		},
	}

	cfg, err := Parse(def)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Codex.TmuxSessionPrefix != "symphony" {
		t.Fatalf("expected tmux session prefix 'symphony', got %q", cfg.Codex.TmuxSessionPrefix)
	}
}

func TestParseFallsBackToLinearAPITokenEnv(t *testing.T) {
	t.Setenv("LINEAR_API_TOKEN", "token-from-alias")
	def := WorkflowDefinition{
		RawConfig: map[string]any{
			"tracker": map[string]any{
				"kind":         "linear",
				"project_slug": "test",
				"api_key":      "$LINEAR_API_KEY",
			},
		},
	}

	cfg, err := Parse(def)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Tracker.APIKey != "token-from-alias" {
		t.Fatalf("expected LINEAR_API_TOKEN fallback, got %q", cfg.Tracker.APIKey)
	}
}
