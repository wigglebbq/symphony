package linear

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/openai/symphony/go/internal/config"
)

func TestGraphQLRetriesTransientStatus(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusGatewayTimeout)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"viewer": map[string]any{"id": "viewer-1"},
			},
		})
	}))
	defer srv.Close()

	client := NewClient(config.Config{
		Tracker: config.TrackerConfig{
			Endpoint: srv.URL,
			APIKey:   "token",
		},
	})

	payload, err := client.GraphQL(context.Background(), "query { viewer { id } }", nil)
	if err != nil {
		t.Fatalf("GraphQL returned error: %v", err)
	}
	if got := attempts.Load(); got != 3 {
		t.Fatalf("expected 3 attempts, got %d", got)
	}
	data, ok := payload["data"].(map[string]any)
	if !ok {
		t.Fatalf("missing data payload: %#v", payload)
	}
	viewer, ok := data["viewer"].(map[string]any)
	if !ok || viewer["id"] != "viewer-1" {
		t.Fatalf("unexpected payload: %#v", payload)
	}
}

func TestGraphQLDoesNotRetryGraphQLErrors(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"errors": []map[string]any{{"message": "bad query"}},
		})
	}))
	defer srv.Close()

	client := NewClient(config.Config{
		Tracker: config.TrackerConfig{
			Endpoint: srv.URL,
			APIKey:   "token",
		},
	})

	if _, err := client.GraphQL(context.Background(), "query { viewer { id } }", nil); err == nil {
		t.Fatal("expected GraphQL error")
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("expected 1 attempt, got %d", got)
	}
}
