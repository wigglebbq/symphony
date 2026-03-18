package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"gopkg.in/yaml.v3"
)

var (
	ErrMissingWorkflowFile       = errors.New("missing_workflow_file")
	ErrWorkflowParse             = errors.New("workflow_parse_error")
	ErrWorkflowFrontMatterNotMap = errors.New("workflow_front_matter_not_a_map")
	ErrTemplateParse             = errors.New("template_parse_error")
	ErrTemplateRender            = errors.New("template_render_error")
)

type WorkflowDefinition struct {
	Path           string
	RawConfig      map[string]any
	PromptTemplate string
	ModTime        time.Time
}

type TrackerConfig struct {
	Kind           string
	Endpoint       string
	APIKey         string
	ProjectSlug    string
	ActiveStates   []string
	TerminalStates []string
}

type PollingConfig struct {
	Interval time.Duration
}

type WorkspaceConfig struct {
	Root string
}

type HooksConfig struct {
	AfterCreate  string
	BeforeRun    string
	AfterRun     string
	BeforeRemove string
	Timeout      time.Duration
}

type AgentConfig struct {
	MaxConcurrentAgents        int
	MaxTurns                   int
	MaxRetryBackoff            time.Duration
	MaxConcurrentAgentsByState map[string]int
}

type WorkerConfig struct {
	SSHHosts                   []string
	MaxConcurrentAgentsPerHost int
}

type CodexConfig struct {
	Command           string
	ApprovalPolicy    string
	ThreadSandbox     string
	TurnSandboxPolicy map[string]any
	TurnTimeout       time.Duration
	ReadTimeout       time.Duration
	StallTimeout      time.Duration
}

type ServerConfig struct {
	Port int
	Host string
}

type Config struct {
	Workflow  WorkflowDefinition
	Tracker   TrackerConfig
	Polling   PollingConfig
	Workspace WorkspaceConfig
	Hooks     HooksConfig
	Worker    WorkerConfig
	Agent     AgentConfig
	Codex     CodexConfig
	Server    ServerConfig
}

type Loader struct {
	path    string
	mu      sync.RWMutex
	current Config
	loaded  bool
}

func NewLoader(path string) *Loader {
	if strings.TrimSpace(path) == "" {
		path = filepath.Join(must(os.Getwd()), "WORKFLOW.md")
	}
	return &Loader{path: path}
}

func (l *Loader) Path() string {
	return l.path
}

func (l *Loader) Load() (Config, error) {
	cfg, err := loadConfig(l.path)
	if err != nil {
		return Config{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.current = cfg
	l.loaded = true
	return cfg, nil
}

func (l *Loader) Current() (Config, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.current, l.loaded
}

func (l *Loader) ReloadIfChanged() (Config, bool, error) {
	info, err := os.Stat(l.path)
	if err != nil {
		return Config{}, false, missingWorkflowError(l.path, err)
	}
	l.mu.RLock()
	current, loaded := l.current, l.loaded
	l.mu.RUnlock()
	if loaded && info.ModTime().Equal(current.Workflow.ModTime) {
		return current, false, nil
	}
	cfg, err := loadConfig(l.path)
	if err != nil {
		return Config{}, false, err
	}
	l.mu.Lock()
	l.current = cfg
	l.loaded = true
	l.mu.Unlock()
	return cfg, true, nil
}

func (l *Loader) Watch(ctx context.Context, onChange func()) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	if err := watcher.Add(filepath.Dir(l.path)); err != nil {
		_ = watcher.Close()
		return err
	}
	go func() {
		defer watcher.Close()
		target := filepath.Clean(l.path)
		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if filepath.Clean(event.Name) == target && event.Has(fsnotify.Write|fsnotify.Create|fsnotify.Rename) {
					onChange()
				}
			case <-watcher.Errors:
			}
		}
	}()
	return nil
}

func loadConfig(path string) (Config, error) {
	def, err := LoadWorkflow(path)
	if err != nil {
		return Config{}, err
	}
	cfg, err := Parse(def)
	if err != nil {
		return Config{}, err
	}
	return cfg, Validate(cfg)
}

func LoadWorkflow(path string) (WorkflowDefinition, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return WorkflowDefinition{}, missingWorkflowError(path, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return WorkflowDefinition{}, missingWorkflowError(path, err)
	}
	front, body := splitFrontMatter(string(content))
	rawConfig := map[string]any{}
	if strings.TrimSpace(front) != "" {
		if err := yaml.Unmarshal([]byte(front), &rawConfig); err != nil {
			return WorkflowDefinition{}, fmt.Errorf("%w: %v", ErrWorkflowParse, err)
		}
		if rawConfig == nil {
			return WorkflowDefinition{}, ErrWorkflowFrontMatterNotMap
		}
	}
	return WorkflowDefinition{
		Path:           path,
		RawConfig:      rawConfig,
		PromptTemplate: strings.TrimSpace(body),
		ModTime:        info.ModTime(),
	}, nil
}

func Parse(def WorkflowDefinition) (Config, error) {
	raw := normalizeMap(def.RawConfig)
	cfg := Config{
		Workflow: def,
		Tracker: TrackerConfig{
			Endpoint:       "https://api.linear.app/graphql",
			ActiveStates:   []string{"Todo", "In Progress"},
			TerminalStates: []string{"Closed", "Cancelled", "Canceled", "Duplicate", "Done"},
		},
		Polling: PollingConfig{Interval: 30 * time.Second},
		Workspace: WorkspaceConfig{
			Root: filepath.Join(os.TempDir(), "symphony_workspaces"),
		},
		Hooks:  HooksConfig{Timeout: 60 * time.Second},
		Worker: WorkerConfig{},
		Agent: AgentConfig{
			MaxConcurrentAgents:        10,
			MaxTurns:                   20,
			MaxRetryBackoff:            5 * time.Minute,
			MaxConcurrentAgentsByState: map[string]int{},
		},
		Codex: CodexConfig{
			Command:        "codex app-server",
			ApprovalPolicy: "never",
			ThreadSandbox:  "workspaceWrite",
			TurnTimeout:    time.Hour,
			ReadTimeout:    5 * time.Second,
			StallTimeout:   5 * time.Minute,
		},
		Server: ServerConfig{
			Port: -1,
			Host: "127.0.0.1",
		},
	}
	if tracker, ok := childMap(raw, "tracker"); ok {
		cfg.Tracker.Kind = stringValue(tracker["kind"])
		if v := stringValue(tracker["endpoint"]); v != "" {
			cfg.Tracker.Endpoint = v
		}
		cfg.Tracker.APIKey = resolveToken(stringValue(tracker["api_key"]), "LINEAR_API_KEY")
		if cfg.Tracker.APIKey == "" && cfg.Tracker.Kind == "linear" {
			cfg.Tracker.APIKey = strings.TrimSpace(os.Getenv("LINEAR_API_KEY"))
		}
		cfg.Tracker.ProjectSlug = stringValue(tracker["project_slug"])
		if v := stringSlice(tracker["active_states"]); len(v) > 0 {
			cfg.Tracker.ActiveStates = v
		}
		if v := stringSlice(tracker["terminal_states"]); len(v) > 0 {
			cfg.Tracker.TerminalStates = v
		}
	}
	if polling, ok := childMap(raw, "polling"); ok {
		if n := intValue(polling["interval_ms"], 30000); n > 0 {
			cfg.Polling.Interval = time.Duration(n) * time.Millisecond
		}
	}
	if workspace, ok := childMap(raw, "workspace"); ok {
		if v := resolvePathLike(stringValue(workspace["root"])); v != "" {
			cfg.Workspace.Root = v
		}
	}
	if hooks, ok := childMap(raw, "hooks"); ok {
		cfg.Hooks.AfterCreate = stringValue(hooks["after_create"])
		cfg.Hooks.BeforeRun = stringValue(hooks["before_run"])
		cfg.Hooks.AfterRun = stringValue(hooks["after_run"])
		cfg.Hooks.BeforeRemove = stringValue(hooks["before_remove"])
		if n := intValue(hooks["timeout_ms"], 60000); n > 0 {
			cfg.Hooks.Timeout = time.Duration(n) * time.Millisecond
		}
	}
	if worker, ok := childMap(raw, "worker"); ok {
		cfg.Worker.SSHHosts = stringSlice(worker["ssh_hosts"])
		if n := intValue(worker["max_concurrent_agents_per_host"], 0); n > 0 {
			cfg.Worker.MaxConcurrentAgentsPerHost = n
		}
	}
	if agent, ok := childMap(raw, "agent"); ok {
		if n := intValue(agent["max_concurrent_agents"], 10); n > 0 {
			cfg.Agent.MaxConcurrentAgents = n
		}
		if n := intValue(agent["max_turns"], 20); n > 0 {
			cfg.Agent.MaxTurns = n
		}
		if n := intValue(agent["max_retry_backoff_ms"], 300000); n > 0 {
			cfg.Agent.MaxRetryBackoff = time.Duration(n) * time.Millisecond
		}
		if limits, ok := childMap(agent, "max_concurrent_agents_by_state"); ok {
			for k, v := range limits {
				if n := intValue(v, 0); n > 0 {
					cfg.Agent.MaxConcurrentAgentsByState[strings.ToLower(strings.TrimSpace(k))] = n
				}
			}
		}
	}
	if codex, ok := childMap(raw, "codex"); ok {
		if v := stringValue(codex["command"]); v != "" {
			cfg.Codex.Command = v
		}
		if v, ok := codex["approval_policy"]; ok && v != nil {
			cfg.Codex.ApprovalPolicy = normalizeApprovalPolicy(v)
		}
		if v := stringValue(codex["thread_sandbox"]); v != "" {
			cfg.Codex.ThreadSandbox = normalizeSandboxType(v)
		}
		if v, ok := childMap(codex, "turn_sandbox_policy"); ok {
			cfg.Codex.TurnSandboxPolicy = normalizeSandboxPolicy(v)
		}
		if n := intValue(codex["turn_timeout_ms"], 3600000); n > 0 {
			cfg.Codex.TurnTimeout = time.Duration(n) * time.Millisecond
		}
		if n := intValue(codex["read_timeout_ms"], 5000); n > 0 {
			cfg.Codex.ReadTimeout = time.Duration(n) * time.Millisecond
		}
		if n := intValue(codex["stall_timeout_ms"], 300000); n >= 0 {
			cfg.Codex.StallTimeout = time.Duration(n) * time.Millisecond
		}
	}
	if server, ok := childMap(raw, "server"); ok {
		if host := stringValue(server["host"]); host != "" {
			cfg.Server.Host = host
		}
		cfg.Server.Port = intValue(server["port"], -1)
	}
	return cfg, nil
}

func Validate(cfg Config) error {
	switch cfg.Tracker.Kind {
	case "":
		return fmt.Errorf("missing_tracker_kind")
	case "linear":
	default:
		return fmt.Errorf("unsupported_tracker_kind: %s", cfg.Tracker.Kind)
	}
	if strings.TrimSpace(cfg.Tracker.APIKey) == "" {
		return fmt.Errorf("missing_linear_api_token")
	}
	if strings.TrimSpace(cfg.Tracker.ProjectSlug) == "" {
		return fmt.Errorf("missing_linear_project_slug")
	}
	if strings.TrimSpace(cfg.Codex.Command) == "" {
		return fmt.Errorf("missing_codex_command")
	}
	switch cfg.Codex.ApprovalPolicy {
	case "untrusted", "on-failure", "on-request", "granular", "never":
	default:
		return fmt.Errorf("invalid_codex_approval_policy")
	}
	switch cfg.Codex.ThreadSandbox {
	case "dangerFullAccess", "readOnly", "externalSandbox", "workspaceWrite":
	default:
		return fmt.Errorf("invalid_codex_thread_sandbox")
	}
	return nil
}

func (c Config) ActiveStateSet() map[string]struct{} {
	out := make(map[string]struct{}, len(c.Tracker.ActiveStates))
	for _, state := range c.Tracker.ActiveStates {
		out[strings.ToLower(state)] = struct{}{}
	}
	return out
}

func (c Config) TerminalStateSet() map[string]struct{} {
	out := make(map[string]struct{}, len(c.Tracker.TerminalStates))
	for _, state := range c.Tracker.TerminalStates {
		out[strings.ToLower(state)] = struct{}{}
	}
	return out
}

func (c Config) MaxConcurrentForState(state string) int {
	if n, ok := c.Agent.MaxConcurrentAgentsByState[strings.ToLower(strings.TrimSpace(state))]; ok && n > 0 {
		return n
	}
	return c.Agent.MaxConcurrentAgents
}

func (c Config) EffectivePromptTemplate() string {
	if strings.TrimSpace(c.Workflow.PromptTemplate) != "" {
		return c.Workflow.PromptTemplate
	}
	return strings.TrimSpace(`
You are working on a Linear issue.

Identifier: {{ issue.identifier }}
Title: {{ issue.title }}

Body:
{% if issue.description %}
{{ issue.description }}
{% else %}
No description provided.
{% endif %}
`)
}

func (c Config) RuntimeSandboxPolicy(workspace string) map[string]any {
	if c.Codex.TurnSandboxPolicy != nil {
		return c.Codex.TurnSandboxPolicy
	}
	root := workspace
	if strings.TrimSpace(root) == "" {
		root = c.Workspace.Root
	}
	return map[string]any{
		"type": "workspaceWrite",
		"root": root,
	}
}

func splitFrontMatter(content string) (string, string) {
	lines := strings.Split(content, "\n")
	if len(lines) == 0 || strings.TrimRight(lines[0], "\r") != "---" {
		return "", content
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimRight(lines[i], "\r") == "---" {
			return strings.Join(lines[1:i], "\n"), strings.Join(lines[i+1:], "\n")
		}
	}
	return strings.Join(lines[1:], "\n"), ""
}

func missingWorkflowError(path string, err error) error {
	return fmt.Errorf("%w: %s: %v", ErrMissingWorkflowFile, path, err)
}

func normalizeMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = normalizeAny(v)
	}
	return out
}

func normalizeAny(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return normalizeMap(t)
	case map[any]any:
		out := map[string]any{}
		for k, v := range t {
			out[fmt.Sprint(k)] = normalizeAny(v)
		}
		return out
	case []any:
		for i := range t {
			t[i] = normalizeAny(t[i])
		}
		return t
	default:
		return v
	}
}

func childMap(m map[string]any, key string) (map[string]any, bool) {
	raw, ok := m[key]
	if !ok {
		return nil, false
	}
	out, ok := raw.(map[string]any)
	return out, ok
}

func stringValue(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	default:
		return ""
	}
}

func stringSlice(v any) []string {
	raw, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if s := stringValue(item); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func intValue(v any, fallback int) int {
	switch t := v.(type) {
	case int:
		return t
	case int64:
		return int(t)
	case float64:
		return int(t)
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(t))
		if err == nil {
			return n
		}
	}
	return fallback
}

func normalizeApprovalPolicy(v any) string {
	switch t := v.(type) {
	case string:
		switch strings.TrimSpace(t) {
		case "untrusted", "on-failure", "on-request", "granular", "never":
			return strings.TrimSpace(t)
		default:
			return "never"
		}
	case map[string]any:
		if _, ok := t["reject"]; ok {
			return "never"
		}
	case map[any]any:
		for key := range t {
			if fmt.Sprint(key) == "reject" {
				return "never"
			}
		}
	}
	return "never"
}

func normalizeSandboxType(value string) string {
	switch strings.TrimSpace(value) {
	case "dangerFullAccess", "danger-full-access":
		return "dangerFullAccess"
	case "readOnly", "read-only":
		return "readOnly"
	case "externalSandbox", "external-sandbox":
		return "externalSandbox"
	case "workspaceWrite", "workspace-write":
		return "workspaceWrite"
	default:
		return "workspaceWrite"
	}
}

func normalizeSandboxPolicy(v map[string]any) map[string]any {
	out := normalizeMap(v)
	if rawType, ok := out["type"]; ok {
		out["type"] = normalizeSandboxType(stringValue(rawType))
	}
	return out
}

func resolveToken(value, defaultEnv string) string {
	value = strings.TrimSpace(value)
	if value == "" && defaultEnv != "" {
		return strings.TrimSpace(os.Getenv(defaultEnv))
	}
	if strings.HasPrefix(value, "$") {
		return strings.TrimSpace(os.Getenv(strings.TrimPrefix(value, "$")))
	}
	return value
}

func resolvePathLike(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if strings.HasPrefix(value, "$") {
		value = os.Getenv(strings.TrimPrefix(value, "$"))
	}
	if strings.HasPrefix(value, "~") {
		home, _ := os.UserHomeDir()
		if home != "" {
			value = filepath.Join(home, strings.TrimPrefix(value, "~"))
		}
	}
	return filepath.Clean(value)
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
