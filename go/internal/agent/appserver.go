package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/openai/symphony/go/internal/config"
	"github.com/openai/symphony/go/internal/linear"
	sshclient "github.com/openai/symphony/go/internal/ssh"
)

type Event struct {
	Event             string           `json:"event"`
	Timestamp         time.Time        `json:"timestamp"`
	CodexAppServerPID int              `json:"codex_app_server_pid,omitempty"`
	SessionID         string           `json:"session_id,omitempty"`
	ThreadID          string           `json:"thread_id,omitempty"`
	TurnID            string           `json:"turn_id,omitempty"`
	Message           string           `json:"message,omitempty"`
	Usage             map[string]int64 `json:"usage,omitempty"`
	RateLimits        map[string]any   `json:"rate_limits,omitempty"`
	Payload           map[string]any   `json:"payload,omitempty"`
}

type Session struct {
	cmd        *exec.Cmd
	stdin      io.WriteCloser
	msgs       chan map[string]any
	errs       chan error
	cfg        config.Config
	logger     *slog.Logger
	linear     *linear.Client
	threadID   string
	workspace  string
	workerHost string
	pid        int
	msgsClosed sync.Once
}

func StartSession(ctx context.Context, cfg config.Config, workspace, workerHost string, lc *linear.Client, logger *slog.Logger) (*Session, error) {
	absWorkspace := workspace
	var err error
	if workerHost == "" {
		absWorkspace, err = filepath.Abs(workspace)
		if err != nil {
			return nil, fmt.Errorf("invalid_workspace_cwd: %w", err)
		}
	}
	var cmd *exec.Cmd
	if workerHost == "" {
		cmd = exec.CommandContext(ctx, "bash", "-lc", cfg.Codex.Command)
		cmd.Dir = absWorkspace
	} else {
		cmd = sshclient.CommandContext(ctx, workerHost, "cd "+shellQuote(absWorkspace)+" && exec "+cfg.Codex.Command)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("codex_not_found: %w", err)
	}
	s := &Session{
		cmd:        cmd,
		stdin:      stdin,
		msgs:       make(chan map[string]any, 256),
		errs:       make(chan error, 4),
		cfg:        cfg,
		logger:     logger,
		linear:     lc,
		workspace:  absWorkspace,
		workerHost: workerHost,
		pid:        cmd.Process.Pid,
	}
	go s.readStdout(stdout)
	go s.readStderr(stderr)
	go func() { s.errs <- cmd.Wait() }()
	if err := s.send(map[string]any{
		"id":     1,
		"method": "initialize",
		"params": map[string]any{
			"clientInfo": map[string]any{
				"name":    "symphony-go",
				"version": "0.1.0",
			},
			"capabilities": map[string]any{
				"experimentalApi": true,
			},
		},
	}); err != nil {
		return nil, err
	}
	if _, err := s.awaitResponse(ctx, 1); err != nil {
		return nil, err
	}
	if err := s.send(map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
		return nil, err
	}
	if err := s.send(map[string]any{
		"id":     2,
		"method": "thread/start",
		"params": map[string]any{
			"approvalPolicy": cfg.Codex.ApprovalPolicy,
			"sandbox":        cfg.Codex.ThreadSandbox,
			"cwd":            absWorkspace,
			"dynamicTools": []map[string]any{{
				"name":        "linear_graphql",
				"description": "Execute one raw GraphQL operation against Linear using Symphony auth.",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"query":     map[string]any{"type": "string"},
						"variables": map[string]any{"type": "object"},
					},
					"required": []string{"query"},
				},
			}},
		},
	}); err != nil {
		return nil, err
	}
	resp, err := s.awaitResponse(ctx, 2)
	if err != nil {
		return nil, err
	}
	thread, _ := resp["thread"].(map[string]any)
	threadID, _ := thread["id"].(string)
	if threadID == "" {
		return nil, fmt.Errorf("response_error: missing thread id")
	}
	s.threadID = threadID
	return s, nil
}

func (s *Session) Stop() {
	if s.stdin != nil {
		_ = s.stdin.Close()
	}
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
}

func (s *Session) RunTurn(ctx context.Context, prompt string, issue linear.Issue, onEvent func(Event)) (string, error) {
	if err := s.send(map[string]any{
		"id":     3,
		"method": "turn/start",
		"params": map[string]any{
			"threadId":       s.threadID,
			"input":          []map[string]any{{"type": "text", "text": prompt}},
			"cwd":            s.workspace,
			"title":          fmt.Sprintf("%s: %s", issue.Identifier, issue.Title),
			"approvalPolicy": s.cfg.Codex.ApprovalPolicy,
			"sandboxPolicy":  s.cfg.RuntimeSandboxPolicy(s.workspace),
		},
	}); err != nil {
		return "", err
	}
	resp, err := s.awaitResponse(ctx, 3)
	if err != nil {
		return "", err
	}
	turn, _ := resp["turn"].(map[string]any)
	turnID, _ := turn["id"].(string)
	if turnID == "" {
		return "", fmt.Errorf("response_error: missing turn id")
	}
	sessionID := s.threadID + "-" + turnID
	onEvent(Event{
		Event:             "session_started",
		Timestamp:         time.Now().UTC(),
		CodexAppServerPID: s.pid,
		SessionID:         sessionID,
		ThreadID:          s.threadID,
		TurnID:            turnID,
	})
	turnCtx, cancel := context.WithTimeout(ctx, s.cfg.Codex.TurnTimeout)
	defer cancel()
	for {
		select {
		case <-turnCtx.Done():
			return sessionID, fmt.Errorf("turn_timeout")
		case err := <-s.errs:
			if err == nil {
				return sessionID, fmt.Errorf("port_exit")
			}
			return sessionID, fmt.Errorf("port_exit: %w", err)
		case msg := <-s.msgs:
			method, _ := msg["method"].(string)
			switch method {
			case "turn/completed":
				onEvent(s.eventFromMessage("turn_completed", sessionID, turnID, msg))
				return sessionID, nil
			case "turn/failed":
				onEvent(s.eventFromMessage("turn_failed", sessionID, turnID, msg))
				return sessionID, fmt.Errorf("turn_failed")
			case "turn/cancelled":
				onEvent(s.eventFromMessage("turn_cancelled", sessionID, turnID, msg))
				return sessionID, fmt.Errorf("turn_cancelled")
			case "item/commandExecution/requestApproval", "execCommandApproval", "applyPatchApproval", "item/fileChange/requestApproval":
				_ = s.send(map[string]any{"id": msg["id"], "result": map[string]any{"approved": true, "decision": "acceptForSession"}})
				onEvent(s.eventFromMessage("approval_auto_approved", sessionID, turnID, msg))
			case "item/tool/requestUserInput":
				onEvent(s.eventFromMessage("turn_input_required", sessionID, turnID, msg))
				return sessionID, fmt.Errorf("turn_input_required")
			case "item/tool/call":
				_ = s.handleToolCall(ctx, msg, sessionID, turnID, onEvent)
			default:
				onEvent(s.eventFromMessage("notification", sessionID, turnID, msg))
			}
		}
	}
}

func (s *Session) handleToolCall(ctx context.Context, msg map[string]any, sessionID, turnID string, onEvent func(Event)) error {
	id := msg["id"]
	params, _ := msg["params"].(map[string]any)
	tool, _ := params["name"].(string)
	args, _ := params["arguments"].(map[string]any)
	result := map[string]any{"success": false, "error": "unsupported_tool_call"}
	eventName := "unsupported_tool_call"
	if tool == "linear_graphql" && s.linear != nil {
		query, _ := args["query"].(string)
		if strings.TrimSpace(query) != "" && strings.Count(query, "query ")+strings.Count(query, "mutation ")+strings.Count(query, "subscription ") <= 1 {
			variables, _ := args["variables"].(map[string]any)
			payload, err := s.linear.GraphQL(ctx, query, variables)
			if err != nil {
				result = map[string]any{"success": false, "error": err.Error()}
				eventName = "tool_call_failed"
			} else {
				result = map[string]any{"success": true, "payload": payload}
				eventName = "tool_call_completed"
			}
		} else {
			result = map[string]any{"success": false, "error": "invalid_graphql_input"}
			eventName = "tool_call_failed"
		}
	}
	_ = s.send(map[string]any{"id": id, "result": result})
	onEvent(s.eventFromMessage(eventName, sessionID, turnID, msg))
	return nil
}

func (s *Session) send(payload map[string]any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = io.WriteString(s.stdin, string(raw)+"\n")
	return err
}

func (s *Session) awaitResponse(ctx context.Context, id int) (map[string]any, error) {
	timeout := s.cfg.Codex.ReadTimeout
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			return nil, fmt.Errorf("response_timeout")
		case err := <-s.errs:
			if err == nil {
				return nil, fmt.Errorf("port_exit")
			}
			return nil, err
		case msg := <-s.msgs:
			if msgID, ok := asInt(msg["id"]); ok && msgID == id {
				if result, ok := msg["result"].(map[string]any); ok {
					return result, nil
				}
				if errPayload, ok := msg["error"].(map[string]any); ok {
					return nil, fmt.Errorf("response_error: %v", errPayload)
				}
			}
		}
	}
}

func (s *Session) readStdout(r io.Reader) {
	scanner := bufio.NewScanner(r)
	buf := make([]byte, 0, 1024*1024)
	scanner.Buffer(buf, 10*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		var payload map[string]any
		if err := json.Unmarshal([]byte(line), &payload); err != nil {
			s.logger.Warn("malformed codex line", "payload", truncate(line))
			continue
		}
		s.msgs <- payload
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, os.ErrClosed) {
		s.errs <- err
	}
}

func (s *Session) readStderr(r io.Reader) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		s.logger.Info("codex stderr", "line", truncate(scanner.Text()))
	}
}

func (s *Session) eventFromMessage(name, sessionID, turnID string, msg map[string]any) Event {
	event := Event{
		Event:             name,
		Timestamp:         time.Now().UTC(),
		CodexAppServerPID: s.pid,
		SessionID:         sessionID,
		ThreadID:          s.threadID,
		TurnID:            turnID,
		Payload:           msg,
	}
	if usage := extractUsage(msg); usage != nil {
		event.Usage = usage
	}
	if limits := extractRateLimits(msg); limits != nil {
		event.RateLimits = limits
	}
	event.Message = summarize(msg)
	return event
}

func summarize(msg map[string]any) string {
	params, _ := msg["params"].(map[string]any)
	if text, ok := params["text"].(string); ok {
		return text
	}
	if message, ok := params["message"].(string); ok {
		return message
	}
	return ""
}

func extractUsage(msg map[string]any) map[string]int64 {
	for _, key := range []string{"usage", "total_token_usage", "tokenUsage"} {
		if usage, ok := msg[key].(map[string]any); ok {
			return map[string]int64{
				"input_tokens":  int64(asFloat(usage["input_tokens"])),
				"output_tokens": int64(asFloat(usage["output_tokens"])),
				"total_tokens":  int64(asFloat(usage["total_tokens"])),
			}
		}
		params, _ := msg["params"].(map[string]any)
		if usage, ok := params[key].(map[string]any); ok {
			return map[string]int64{
				"input_tokens":  int64(asFloat(usage["input_tokens"])),
				"output_tokens": int64(asFloat(usage["output_tokens"])),
				"total_tokens":  int64(asFloat(usage["total_tokens"])),
			}
		}
	}
	return nil
}

func extractRateLimits(msg map[string]any) map[string]any {
	params, _ := msg["params"].(map[string]any)
	if limits, ok := params["rate_limits"].(map[string]any); ok {
		return limits
	}
	if limits, ok := params["rateLimits"].(map[string]any); ok {
		return limits
	}
	return nil
}

func asFloat(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case int:
		return float64(t)
	case int64:
		return float64(t)
	default:
		return 0
	}
}

func asInt(v any) (int, bool) {
	switch t := v.(type) {
	case float64:
		return int(t), true
	case int:
		return t, true
	default:
		return 0, false
	}
}

func truncate(s string) string {
	if len(s) > 1000 {
		return s[:1000]
	}
	return s
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}
