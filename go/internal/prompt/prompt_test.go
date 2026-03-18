package prompt

import (
	"testing"

	"github.com/openai/symphony/go/internal/config"
	"github.com/openai/symphony/go/internal/linear"
)

func TestBuildPrompt(t *testing.T) {
	cfg := config.Config{
		Workflow: config.WorkflowDefinition{
			PromptTemplate: `Issue {{ issue.identifier }}{% if issue.description %}: {{ issue.description }}{% else %}: none{% endif %}`,
		},
	}
	out, err := Build(cfg, linear.Issue{Identifier: "ABC-1", Description: "Ship it"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out != "Issue ABC-1: Ship it" {
		t.Fatalf("unexpected prompt: %q", out)
	}
}

func TestBuildPromptReviewFields(t *testing.T) {
	cfg := config.Config{
		Workflow: config.WorkflowDefinition{
			PromptTemplate: `{% if issue.is_review %}review {{ issue.review_source_identifier }}{% else %}normal{% endif %}`,
		},
	}
	out, err := Build(cfg, linear.Issue{Identifier: "ABC-2", Title: "REVIEW: ABC-1 Ship it"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out != "review ABC-1" {
		t.Fatalf("unexpected prompt: %q", out)
	}
}
