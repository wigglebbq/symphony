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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/openai/symphony/go/internal/config"
	"github.com/openai/symphony/go/internal/linear"
	sshclient "github.com/openai/symphony/go/internal/ssh"
)

type Event struct {
	Event             string           `json:"event"`
	Timestamp         time.Time        `json:"timestamp"`
	CodexAppServerPID int              `json:"codex_app_server_pid,omitempty"`
	TmuxSession       string           `json:"tmux_session,omitempty"`
	SessionID         string           `json:"session_id,omitempty"`
	ThreadID          string           `json:"thread_id,omitempty"`
	TurnID            string           `json:"turn_id,omitempty"`
	Message           string           `json:"message,omitempty"`
	Usage             map[string]int64 `json:"usage,omitempty"`
	RateLimits        map[string]any   `json:"rate_limits,omitempty"`
	Payload           map[string]any   `json:"payload,omitempty"`
}

type Session struct {
	cmd         *exec.Cmd
	stdin       io.WriteCloser
	msgs        chan map[string]any
	errs        chan error
	cfg         config.Config
	logger      *slog.Logger
	linear      *linear.Client
	threadID    string
	workspace   string
	workerHost  string
	pid         int
	tmuxSession string
	stopCh      chan struct{}
	stopOnce    sync.Once
	msgsClosed  sync.Once
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
	if workerHost == "" && strings.TrimSpace(cfg.Codex.TmuxSessionPrefix) != "" {
		return startLocalTmuxSession(ctx, cfg, absWorkspace, lc, logger)
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
		stopCh:     make(chan struct{}),
	}
	workerLabel := workerHost
	if workerLabel == "" {
		workerLabel = "local"
	}
	s.logger.Info("codex session configured",
		"workspace", absWorkspace,
		"worker_host", workerLabel,
		"tmux_session", s.tmuxSession,
		"thread_sandbox", threadSandboxValue(cfg.Codex.ThreadSandbox),
		"turn_sandbox_policy", jsonString(cfg.RuntimeSandboxPolicy(absWorkspace)),
	)
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
			"sandbox":        threadSandboxValue(cfg.Codex.ThreadSandbox),
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

func startLocalTmuxSession(ctx context.Context, cfg config.Config, absWorkspace string, lc *linear.Client, logger *slog.Logger) (*Session, error) {
	if _, err := exec.LookPath("tmux"); err != nil {
		return nil, fmt.Errorf("tmux_not_found: %w", err)
	}
	sessionName := tmuxSessionName(cfg.Codex.TmuxSessionPrefix, filepath.Base(absWorkspace))
	logDir := filepath.Join(absWorkspace, ".symphony")
	stdinPath := filepath.Join(logDir, "codex-stdin.fifo")
	stdoutPath := filepath.Join(logDir, "codex-stdout.log")
	stderrPath := filepath.Join(logDir, "codex-stderr.log")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return nil, err
	}
	_ = os.Remove(stdinPath)
	if err := syscall.Mkfifo(stdinPath, 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(stdoutPath, nil, 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(stderrPath, nil, 0o644); err != nil {
		return nil, err
	}
	_ = exec.CommandContext(ctx, "tmux", "kill-session", "-t", sessionName).Run()
	shellCommand := "bash -lc " + shellQuote("exec "+cfg.Codex.Command+" <"+shellQuote(stdinPath)+" 2>>"+shellQuote(stderrPath))
	cmd := exec.CommandContext(ctx, "tmux", "new-session", "-d", "-s", sessionName, "-c", absWorkspace, shellCommand)
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("tmux_session_start_failed: %w", err)
	}
	pipeCommand := "cat >> " + shellQuote(stdoutPath)
	if err := exec.CommandContext(ctx, "tmux", "pipe-pane", "-o", "-t", sessionName, pipeCommand).Run(); err != nil {
		_ = exec.CommandContext(ctx, "tmux", "kill-session", "-t", sessionName).Run()
		return nil, fmt.Errorf("tmux_pipe_failed: %w", err)
	}
	pid := 0
	if out, err := exec.CommandContext(ctx, "tmux", "display-message", "-p", "-t", sessionName, "#{pane_pid}").Output(); err == nil {
		if parsed, parseOK := asInt(strings.TrimSpace(string(out))); parseOK {
			pid = parsed
		}
	}
	stdin, err := os.OpenFile(stdinPath, os.O_WRONLY, 0o600)
	if err != nil {
		_ = exec.CommandContext(ctx, "tmux", "kill-session", "-t", sessionName).Run()
		return nil, fmt.Errorf("tmux_fifo_open_failed: %w", err)
	}
	s := &Session{
		stdin:       stdin,
		msgs:        make(chan map[string]any, 256),
		errs:        make(chan error, 4),
		cfg:         cfg,
		logger:      logger,
		linear:      lc,
		workspace:   absWorkspace,
		workerHost:  "",
		pid:         pid,
		tmuxSession: sessionName,
		stopCh:      make(chan struct{}),
	}
	s.logger.Info("codex session configured",
		"workspace", absWorkspace,
		"worker_host", "local",
		"tmux_session", s.tmuxSession,
		"thread_sandbox", threadSandboxValue(cfg.Codex.ThreadSandbox),
		"turn_sandbox_policy", jsonString(cfg.RuntimeSandboxPolicy(absWorkspace)),
	)
	go s.readStdoutFile(stdoutPath)
	go s.readStderrFile(stderrPath)
	go s.waitForTmuxExit(ctx)
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
			"sandbox":        threadSandboxValue(cfg.Codex.ThreadSandbox),
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

func threadSandboxValue(value string) string {
	switch strings.TrimSpace(value) {
	case "readOnly", "read-only":
		return "read-only"
	case "dangerFullAccess", "danger-full-access":
		return "danger-full-access"
	case "workspaceWrite", "workspace-write":
		return "workspace-write"
	default:
		return "workspace-write"
	}
}

func (s *Session) Stop() {
	s.stopOnce.Do(func() {
		if s.stopCh != nil {
			close(s.stopCh)
		}
		if s.stdin != nil {
			_ = s.stdin.Close()
		}
		if s.tmuxSession != "" {
			_ = exec.Command("tmux", "kill-session", "-t", s.tmuxSession).Run()
		}
		if s.cmd != nil && s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
	})
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
		TmuxSession:       s.tmuxSession,
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
			case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
				_ = s.send(map[string]any{"id": msg["id"], "result": map[string]any{"approved": true, "decision": "acceptForSession"}})
				onEvent(s.eventFromMessage("approval_auto_approved", sessionID, turnID, msg))
			case "execCommandApproval", "applyPatchApproval":
				_ = s.send(map[string]any{"id": msg["id"], "result": map[string]any{"approved": true, "decision": "approved_for_session"}})
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
	tool, _ := params["tool"].(string)
	if strings.TrimSpace(tool) == "" {
		tool, _ = params["name"].(string)
	}
	args, _ := params["arguments"].(map[string]any)
	result := dynamicToolResult(false, "unsupported_tool_call")
	eventName := "unsupported_tool_call"
	if tool == "linear_graphql" && s.linear != nil {
		query, _ := args["query"].(string)
		if strings.TrimSpace(query) != "" && strings.Count(query, "query ")+strings.Count(query, "mutation ")+strings.Count(query, "subscription ") <= 1 {
			variables, _ := args["variables"].(map[string]any)
			payload, err := s.linear.GraphQL(ctx, query, variables)
			if err != nil {
				result = dynamicToolResult(false, err.Error())
				eventName = "tool_call_failed"
			} else {
				result = dynamicToolResult(true, jsonString(payload))
				eventName = "tool_call_completed"
			}
		} else {
			result = dynamicToolResult(false, "invalid_graphql_input")
			eventName = "tool_call_failed"
		}
	}
	_ = s.send(map[string]any{"id": id, "result": result})
	onEvent(s.eventFromMessage(eventName, sessionID, turnID, msg))
	return nil
}

func dynamicToolResult(success bool, output string) map[string]any {
	return map[string]any{
		"success": success,
		"output":  output,
		"contentItems": []map[string]any{{
			"type": "inputText",
			"text": output,
		}},
	}
}

func jsonString(value any) string {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Sprint(value)
	}
	return string(raw)
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
		TmuxSession:       s.tmuxSession,
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
	if text, ok := pathString(msg, "params", "msg", "payload", "text"); ok {
		return text
	}
	if text, ok := pathString(msg, "params", "msg", "text"); ok {
		return text
	}
	if method, _ := msg["method"].(string); strings.TrimSpace(method) != "" {
		return method
	}
	return ""
}

func extractUsage(msg map[string]any) map[string]int64 {
	for _, path := range [][]string{
		{"params", "msg", "payload", "info", "total_token_usage"},
		{"params", "msg", "info", "total_token_usage"},
		{"params", "tokenUsage", "total"},
		{"tokenUsage", "total"},
		{"usage"},
		{"total_token_usage"},
		{"tokenUsage"},
		{"params", "usage"},
		{"params", "total_token_usage"},
		{"params", "tokenUsage"},
	} {
		if usage, ok := pathMap(msg, path...); ok {
			if out, ok := normalizeUsageMap(usage); ok {
				return out
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
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(t))
		if err == nil {
			return n, true
		}
		return 0, false
	default:
		return 0, false
	}
}

func pathMap(m map[string]any, path ...string) (map[string]any, bool) {
	var current any = m
	for _, part := range path {
		next, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = next[part]
		if !ok {
			return nil, false
		}
	}
	out, ok := current.(map[string]any)
	return out, ok
}

func pathString(m map[string]any, path ...string) (string, bool) {
	var current any = m
	for _, part := range path {
		next, ok := current.(map[string]any)
		if !ok {
			return "", false
		}
		current, ok = next[part]
		if !ok {
			return "", false
		}
	}
	out, ok := current.(string)
	return out, ok
}

func normalizeUsageMap(usage map[string]any) (map[string]int64, bool) {
	input := asFloat(usage["input_tokens"])
	if input == 0 {
		input = asFloat(usage["inputTokens"])
	}
	output := asFloat(usage["output_tokens"])
	if output == 0 {
		output = asFloat(usage["outputTokens"])
	}
	total := asFloat(usage["total_tokens"])
	if total == 0 {
		total = asFloat(usage["totalTokens"])
	}
	if total == 0 {
		total = asFloat(usage["total"])
	}
	if input == 0 && output == 0 && total == 0 {
		return nil, false
	}
	return map[string]int64{
		"input_tokens":  int64(input),
		"output_tokens": int64(output),
		"total_tokens":  int64(total),
	}, true
}

func truncate(s string) string {
	if len(s) > 1000 {
		return s[:1000]
	}
	return s
}

func (s *Session) readStdoutFile(path string) {
	file, err := os.Open(path)
	if err != nil {
		s.errs <- err
		return
	}
	defer file.Close()
	reader := bufio.NewReader(file)
	for {
		select {
		case <-s.stopCh:
			return
		default:
		}
		line, err := reader.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				time.Sleep(100 * time.Millisecond)
				continue
			}
			s.errs <- err
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if strings.TrimSpace(line) == "" {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(line), &payload); err != nil {
			s.logger.Warn("malformed codex line", "payload", truncate(line))
			continue
		}
		s.msgs <- payload
	}
}

func (s *Session) readStderrFile(path string) {
	file, err := os.Open(path)
	if err != nil {
		s.errs <- err
		return
	}
	defer file.Close()
	reader := bufio.NewReader(file)
	for {
		select {
		case <-s.stopCh:
			return
		default:
		}
		line, err := reader.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				time.Sleep(100 * time.Millisecond)
				continue
			}
			s.errs <- err
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if strings.TrimSpace(line) == "" {
			continue
		}
		s.logger.Info("codex stderr", "tmux_session", s.tmuxSession, "line", truncate(line))
	}
}

func (s *Session) waitForTmuxExit(ctx context.Context) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			if exec.Command("tmux", "has-session", "-t", s.tmuxSession).Run() != nil {
				s.errs <- nil
				return
			}
		}
	}
}

func tmuxSessionName(prefix, key string) string {
	var b strings.Builder
	for _, r := range prefix + "-" + key {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	name := strings.Trim(b.String(), "-")
	if name == "" {
		return "symphony-session"
	}
	return name
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}
