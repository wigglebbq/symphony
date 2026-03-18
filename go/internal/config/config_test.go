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
