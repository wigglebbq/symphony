package httpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/openai/symphony/go/internal/orchestrator"
)

type Server struct {
	httpServer *http.Server
	logger     *slog.Logger
}

func Start(ctx context.Context, host string, port int, orch *orchestrator.Orchestrator, logger *slog.Logger) (*Server, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			writeJSONError(w, http.StatusNotFound, "not_found", "Route not found")
			return
		}
		if r.Method != http.MethodGet {
			writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed")
			return
		}
		state := orch.Snapshot()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = dashboardTemplate.Execute(w, state)
	})
	mux.HandleFunc("/api/v1/state", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed")
			return
		}
		writeJSON(w, http.StatusOK, orch.Snapshot())
	})
	mux.HandleFunc("/api/v1/refresh", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed")
			return
		}
		go func() { _ = orch.Refresh() }()
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "accepted"})
	})
	mux.HandleFunc("/api/v1/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed")
			return
		}
		identifier := strings.TrimPrefix(r.URL.Path, "/api/v1/")
		if identifier == "" || identifier == "state" || identifier == "refresh" {
			writeJSONError(w, http.StatusNotFound, "not_found", "Route not found")
			return
		}
		payload, ok := orch.IssueDetails(identifier)
		if !ok {
			writeJSONError(w, http.StatusNotFound, "issue_not_found", "Issue not found")
			return
		}
		writeJSON(w, http.StatusOK, payload)
	})

	addr := fmt.Sprintf("%s:%d", host, port)
	srv := &http.Server{Addr: addr, Handler: mux}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	server := &Server{httpServer: srv, logger: logger}
	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
	}()
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			logger.Error("http server stopped", "error", err)
		}
	}()
	logger.Info("http server listening", "addr", ln.Addr().String())
	return server, nil
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeJSONError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"code":    code,
			"message": message,
		},
	})
}

var dashboardTemplate = template.Must(template.New("dashboard").Parse(`<!doctype html>
<html>
<head>
  <meta charset="utf-8" />
  <title>Symphony</title>
  <style>
    body { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; margin: 24px; background: #f4f1ea; color: #17202a; }
    h1 { margin: 0 0 12px 0; }
    pre { background: #fff; padding: 16px; border: 1px solid #d8d2c5; overflow: auto; }
  </style>
</head>
<body>
  <h1>Symphony</h1>
  <pre>{{ printf "%+v" . }}</pre>
</body>
</html>`))
