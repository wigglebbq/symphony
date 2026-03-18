package orchestrator

import (
	"testing"
	"time"

	"github.com/openai/symphony/go/internal/linear"
)

func TestParseLatestReviewResult(t *testing.T) {
	older := time.Date(2026, 3, 18, 10, 0, 0, 0, time.UTC)
	newer := older.Add(time.Minute)
	result := parseLatestReviewResult([]linear.Comment{
		{
			ID:        "c1",
			Body:      "SYMPHONY_REVIEW_RESULT\n```json\n{\"decision\":\"request_changes\",\"summary\":\"needs tests\"}\n```",
			CreatedAt: &older,
		},
		{
			ID:        "c2",
			Body:      "SYMPHONY_REVIEW_RESULT\n```json\n{\"decision\":\"approve\",\"summary\":\"looks good\",\"reviewed_sha\":\"abc123\"}\n```",
			CreatedAt: &newer,
		},
	})
	if result == nil {
		t.Fatal("expected review result")
	}
	if result.Decision != workflowStateApproved {
		t.Fatalf("unexpected decision: %q", result.Decision)
	}
	if result.Summary != "looks good" {
		t.Fatalf("unexpected summary: %q", result.Summary)
	}
	if result.ReviewedSHA != "abc123" {
		t.Fatalf("unexpected reviewed sha: %q", result.ReviewedSHA)
	}
	if result.CommentID != "c2" {
		t.Fatalf("unexpected comment id: %q", result.CommentID)
	}
}

func TestParseReviewResultCommentRejectsInvalidPayload(t *testing.T) {
	comment := linear.Comment{
		ID:   "c1",
		Body: "SYMPHONY_REVIEW_RESULT\n```json\n{\"decision\":\"maybe\"}\n```",
	}
	if result := parseReviewResultComment(comment); result != nil {
		t.Fatalf("expected invalid review result to be ignored: %#v", result)
	}
}
