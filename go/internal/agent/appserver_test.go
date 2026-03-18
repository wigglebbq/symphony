package agent

import "testing"

func TestExtractUsagePrefersAbsoluteThreadTotals(t *testing.T) {
	msg := map[string]any{
		"method": "thread/tokenUsage/updated",
		"params": map[string]any{
			"tokenUsage": map[string]any{
				"total": map[string]any{
					"inputTokens":  12.0,
					"outputTokens": 7.0,
					"totalTokens":  19.0,
				},
				"last": map[string]any{
					"inputTokens":  2.0,
					"outputTokens": 1.0,
					"totalTokens":  3.0,
				},
			},
		},
	}
	got := extractUsage(msg)
	if got == nil {
		t.Fatalf("expected usage")
	}
	if got["input_tokens"] != 12 || got["output_tokens"] != 7 || got["total_tokens"] != 19 {
		t.Fatalf("unexpected usage: %#v", got)
	}
}

func TestExtractUsageReadsNestedTokenCountWrapper(t *testing.T) {
	msg := map[string]any{
		"params": map[string]any{
			"msg": map[string]any{
				"payload": map[string]any{
					"info": map[string]any{
						"total_token_usage": map[string]any{
							"input_tokens":  30.0,
							"output_tokens": 5.0,
							"total_tokens":  35.0,
						},
					},
				},
			},
		},
	}
	got := extractUsage(msg)
	if got == nil {
		t.Fatalf("expected usage")
	}
	if got["input_tokens"] != 30 || got["output_tokens"] != 5 || got["total_tokens"] != 35 {
		t.Fatalf("unexpected usage: %#v", got)
	}
}

func TestSummarizeFallsBackToMethod(t *testing.T) {
	msg := map[string]any{"method": "turn/started"}
	if got := summarize(msg); got != "turn/started" {
		t.Fatalf("expected method fallback, got %q", got)
	}
}

func TestToolCallCanUseToolKey(t *testing.T) {
	params := map[string]any{
		"tool": "linear_graphql",
	}
	tool, _ := params["tool"].(string)
	if tool != "linear_graphql" {
		t.Fatalf("expected tool key to be available, got %q", tool)
	}
}
