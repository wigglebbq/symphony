package orchestrator

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/openai/symphony/go/internal/agent"
	"github.com/openai/symphony/go/internal/config"
	"github.com/openai/symphony/go/internal/linear"
	"github.com/openai/symphony/go/internal/workspace"
)

type runningEntry struct {
	issue        linear.Issue
	attempt      *int
	startedAt    time.Time
	lastEventAt  time.Time
	sessionID    string
	lastEvent    string
	lastMessage  string
	turnCount    int
	tokens       tokenTotals
	lastReported tokenTotals
	cancel       context.CancelFunc
	workspace    string
	workerHost   string
}

type retryEntry struct {
	issue   linear.Issue
	attempt int
	dueAt   time.Time
	error   string
}

type tokenTotals struct {
	Input  int64 `json:"input_tokens"`
	Output int64 `json:"output_tokens"`
	Total  int64 `json:"total_tokens"`
}

type Orchestrator struct {
	logger       *slog.Logger
	loader       *config.Loader
	cfg          config.Config
	linear       *linear.Client
	workspaces   *workspace.Manager
	runner       *agent.Runner
	results      chan agent.RunResult
	retryCh      chan retryEntry
	mu           sync.Mutex
	running      map[string]*runningEntry
	claimed      map[string]struct{}
	retrying     map[string]retryEntry
	codexTotals  tokenTotals
	secondsEnded float64
	rateLimits   map[string]any
}

func New(loader *config.Loader, logger *slog.Logger) (*Orchestrator, error) {
	cfg, err := loader.Load()
	if err != nil {
		return nil, err
	}
	lc := linear.NewClient(cfg)
	wm := workspace.NewManager(cfg)
	return &Orchestrator{
		logger:     logger,
		loader:     loader,
		cfg:        cfg,
		linear:     lc,
		workspaces: wm,
		runner:     agent.NewRunner(cfg, lc, wm, logger),
		results:    make(chan agent.RunResult, 128),
		retryCh:    make(chan retryEntry, 128),
		running:    map[string]*runningEntry{},
		claimed:    map[string]struct{}{},
		retrying:   map[string]retryEntry{},
	}, nil
}

func (o *Orchestrator) Run(ctx context.Context) error {
	_ = o.loader.Watch(ctx, func() {
		if cfg, changed, err := o.loader.ReloadIfChanged(); err == nil && changed {
			o.applyConfig(cfg)
			o.logger.Info("workflow reloaded", "path", cfg.Workflow.Path)
		}
	})
	o.cleanupTerminalWorkspaces(ctx)
	ticker := time.NewTicker(o.cfg.Polling.Interval)
	defer ticker.Stop()
	if err := o.tick(ctx); err != nil {
		o.logger.Error("initial tick failed", "error", err)
	}
	for {
		select {
		case <-ctx.Done():
			o.stopAll()
			return ctx.Err()
		case <-ticker.C:
			if cfg, changed, err := o.loader.ReloadIfChanged(); err == nil && changed {
				o.applyConfig(cfg)
				ticker.Reset(o.cfg.Polling.Interval)
				o.logger.Info("workflow reloaded", "path", cfg.Workflow.Path)
			} else if err != nil {
				o.logger.Error("workflow reload failed", "error", err)
			}
			if err := o.tick(ctx); err != nil {
				o.logger.Error("poll tick failed", "error", err)
			}
		case result := <-o.results:
			o.handleResult(result)
		case retry := <-o.retryCh:
			o.mu.Lock()
			delete(o.retrying, retry.issue.ID)
			o.mu.Unlock()
			o.handleRetry(ctx, retry)
		}
	}
}

func (o *Orchestrator) tick(ctx context.Context) error {
	o.reconcile(ctx)
	if err := config.Validate(o.cfg); err != nil {
		return err
	}
	issues, err := o.linear.FetchCandidateIssues(ctx)
	if err != nil {
		return err
	}
	for _, issue := range issues {
		if o.availableSlots() <= 0 {
			break
		}
		_ = o.dispatch(ctx, issue, nil)
	}
	return nil
}

func (o *Orchestrator) dispatch(ctx context.Context, issue linear.Issue, attempt *int) error {
	if !o.eligible(issue) {
		return nil
	}
	workerHost := o.selectWorkerHost("")
	if workerHost == ":no_worker_capacity" {
		return nil
	}
	o.mu.Lock()
	o.claimed[issue.ID] = struct{}{}
	runCtx, cancel := context.WithCancel(ctx)
	workspacePath, _ := o.workspaces.Path(issue.Identifier, workerHost)
	o.running[issue.ID] = &runningEntry{
		issue:       issue,
		attempt:     attempt,
		startedAt:   time.Now().UTC(),
		lastEventAt: time.Now().UTC(),
		cancel:      cancel,
		workspace:   workspacePath,
		workerHost:  workerHost,
	}
	o.mu.Unlock()
	go func() {
		result := o.runner.Run(runCtx, issue, attempt, workerHost, func(event agent.Event) {
			o.integrateEvent(issue.ID, event)
		})
		o.results <- result
	}()
	o.logger.Info("agent dispatched", "issue_id", issue.ID, "issue_identifier", issue.Identifier, "attempt", attemptValue(attempt), "worker_host", workerHostValue(workerHost))
	return nil
}

func (o *Orchestrator) eligible(issue linear.Issue) bool {
	return o.eligibleWithClaim(issue, false)
}

func (o *Orchestrator) eligibleWithClaim(issue linear.Issue, allowClaimed bool) bool {
	if issue.ID == "" || issue.Identifier == "" || issue.Title == "" || issue.State == "" {
		return false
	}
	if !isActive(o.cfg, issue.State) || isTerminal(o.cfg, issue.State) {
		return false
	}
	if strings.EqualFold(issue.State, "Todo") {
		for _, blocker := range issue.BlockedBy {
			if !isTerminal(o.cfg, blocker.State) {
				return false
			}
		}
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if !allowClaimed {
		if _, ok := o.claimed[issue.ID]; ok {
			return false
		}
	}
	if _, ok := o.running[issue.ID]; ok {
		return false
	}
	if o.availableSlotsLocked() <= 0 {
		return false
	}
	if !o.workerSlotsAvailableLocked("") {
		return false
	}
	stateLimit := o.cfg.MaxConcurrentForState(issue.State)
	if stateLimit > 0 {
		count := 0
		for _, entry := range o.running {
			if strings.EqualFold(entry.issue.State, issue.State) {
				count++
			}
		}
		if count >= stateLimit {
			return false
		}
	}
	return true
}

func (o *Orchestrator) reconcile(ctx context.Context) {
	ids := []string{}
	now := time.Now().UTC()
	o.mu.Lock()
	for id, entry := range o.running {
		if o.cfg.Codex.StallTimeout > 0 && now.Sub(entry.lastEventAt) > o.cfg.Codex.StallTimeout {
			entry.cancel()
			if _, exists := o.retrying[id]; !exists {
				o.retrying[id] = retryEntry{issue: entry.issue, attempt: nextAttempt(entry.attempt), dueAt: now.Add(backoff(o.cfg, nextAttempt(entry.attempt))), error: "stalled"}
				go o.fireRetry(o.retrying[id])
			}
		}
		ids = append(ids, id)
	}
	o.mu.Unlock()
	states, err := o.linear.FetchIssueStatesByIDs(ctx, ids)
	if err != nil {
		o.logger.Warn("reconciliation state refresh failed", "error", err)
		return
	}
	byID := map[string]linear.Issue{}
	for _, issue := range states {
		byID[issue.ID] = issue
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	for id, entry := range o.running {
		refreshed, ok := byID[id]
		if !ok {
			continue
		}
		entry.issue = refreshed
		if isTerminal(o.cfg, refreshed.State) {
			entry.cancel()
			delete(o.claimed, id)
			delete(o.retrying, id)
			go o.workspaces.Remove(context.Background(), refreshed.Identifier, entry.workerHost)
		} else if !isActive(o.cfg, refreshed.State) {
			entry.cancel()
			delete(o.claimed, id)
			delete(o.retrying, id)
		}
	}
}

func (o *Orchestrator) integrateEvent(issueID string, event agent.Event) {
	o.mu.Lock()
	defer o.mu.Unlock()
	entry, ok := o.running[issueID]
	if !ok {
		return
	}
	entry.lastEventAt = event.Timestamp
	entry.lastEvent = event.Event
	entry.lastMessage = event.Message
	if event.SessionID != "" {
		entry.sessionID = event.SessionID
	}
	if event.Event == "session_started" {
		entry.turnCount++
	}
	if event.Usage != nil {
		abs := tokenTotals{Input: event.Usage["input_tokens"], Output: event.Usage["output_tokens"], Total: event.Usage["total_tokens"]}
		delta := tokenTotals{Input: abs.Input - entry.lastReported.Input, Output: abs.Output - entry.lastReported.Output, Total: abs.Total - entry.lastReported.Total}
		if delta.Input >= 0 {
			o.codexTotals.Input += delta.Input
			o.codexTotals.Output += delta.Output
			o.codexTotals.Total += delta.Total
			entry.tokens = abs
			entry.lastReported = abs
		}
	}
	if event.RateLimits != nil {
		o.rateLimits = event.RateLimits
	}
	if shouldLogEvent(event) {
		o.logger.Info("codex event",
			"issue_id", entry.issue.ID,
			"issue_identifier", entry.issue.Identifier,
			"session_id", event.SessionID,
			"event", event.Event,
			"message", event.Message,
		)
	}
}

func shouldLogEvent(event agent.Event) bool {
	switch event.Event {
	case "session_started", "turn_completed", "turn_failed", "turn_cancelled", "turn_input_required", "approval_auto_approved", "tool_call_completed", "tool_call_failed", "unsupported_tool_call":
		return true
	case "notification":
		return strings.HasPrefix(strings.TrimSpace(event.Message), "item/")
	default:
		return false
	}
}

func (o *Orchestrator) handleResult(result agent.RunResult) {
	o.mu.Lock()
	entry, ok := o.running[result.Issue.ID]
	if ok {
		o.secondsEnded += time.Since(entry.startedAt).Seconds()
		delete(o.running, result.Issue.ID)
	}
	if result.Err == nil && result.NormalExit {
		retry := retryEntry{issue: result.Issue, attempt: 1, dueAt: time.Now().UTC().Add(time.Second)}
		o.retrying[result.Issue.ID] = retry
		o.mu.Unlock()
		o.fireRetry(retry)
		o.logger.Info("agent completed", "issue_id", result.Issue.ID, "issue_identifier", result.Issue.Identifier, "session_id", result.SessionID)
		return
	}
	attempt := 1
	if entry != nil {
		attempt = nextAttempt(entry.attempt)
	}
	retry := retryEntry{issue: result.Issue, attempt: attempt, dueAt: time.Now().UTC().Add(backoff(o.cfg, attempt)), error: errString(result.Err)}
	o.retrying[result.Issue.ID] = retry
	o.mu.Unlock()
	o.fireRetry(retry)
	o.logger.Warn("agent failed", "issue_id", result.Issue.ID, "issue_identifier", result.Issue.Identifier, "session_id", result.SessionID, "error", result.Err)
}

func (o *Orchestrator) handleRetry(ctx context.Context, retry retryEntry) {
	issues, err := o.linear.FetchCandidateIssues(ctx)
	if err != nil {
		o.logger.Warn("retry candidate refresh failed", "issue_id", retry.issue.ID, "issue_identifier", retry.issue.Identifier, "error", err)
		o.rescheduleRetry(retry.issue, retry.attempt, "candidate refresh failed")
		return
	}
	for _, issue := range issues {
		if issue.ID != retry.issue.ID {
			continue
		}
		if !o.eligibleWithClaim(issue, true) {
			o.rescheduleRetry(issue, retry.attempt, "no available orchestrator slots")
			return
		}
		_ = o.dispatchRetry(ctx, issue, retry.attempt)
		return
	}
	o.mu.Lock()
	delete(o.claimed, retry.issue.ID)
	delete(o.retrying, retry.issue.ID)
	o.mu.Unlock()
}

func (o *Orchestrator) dispatchRetry(ctx context.Context, issue linear.Issue, attempt int) error {
	workerHost := o.selectWorkerHost("")
	if workerHost == ":no_worker_capacity" {
		o.rescheduleRetry(issue, attempt, "no available worker slots")
		return nil
	}
	o.mu.Lock()
	o.claimed[issue.ID] = struct{}{}
	runCtx, cancel := context.WithCancel(ctx)
	attemptCopy := attempt
	workspacePath, _ := o.workspaces.Path(issue.Identifier, workerHost)
	o.running[issue.ID] = &runningEntry{
		issue:       issue,
		attempt:     &attemptCopy,
		startedAt:   time.Now().UTC(),
		lastEventAt: time.Now().UTC(),
		cancel:      cancel,
		workspace:   workspacePath,
		workerHost:  workerHost,
	}
	o.mu.Unlock()
	go func() {
		result := o.runner.Run(runCtx, issue, &attemptCopy, workerHost, func(event agent.Event) {
			o.integrateEvent(issue.ID, event)
		})
		o.results <- result
	}()
	o.logger.Info("agent redispatched", "issue_id", issue.ID, "issue_identifier", issue.Identifier, "attempt", attempt, "worker_host", workerHostValue(workerHost))
	return nil
}

func (o *Orchestrator) rescheduleRetry(issue linear.Issue, attempt int, message string) {
	retry := retryEntry{issue: issue, attempt: attempt, dueAt: time.Now().UTC().Add(backoff(o.cfg, attempt)), error: message}
	o.mu.Lock()
	o.retrying[issue.ID] = retry
	o.mu.Unlock()
	o.fireRetry(retry)
}

func (o *Orchestrator) cleanupTerminalWorkspaces(ctx context.Context) {
	issues, err := o.linear.FetchIssuesByStates(ctx, o.cfg.Tracker.TerminalStates)
	if err != nil {
		o.logger.Warn("startup terminal workspace cleanup skipped", "error", err)
		return
	}
	for _, issue := range issues {
		_ = o.workspaces.Remove(ctx, issue.Identifier, "")
		for _, host := range o.cfg.Worker.SSHHosts {
			_ = o.workspaces.Remove(ctx, issue.Identifier, host)
		}
	}
}

func (o *Orchestrator) fireRetry(retry retryEntry) {
	go func() {
		time.Sleep(time.Until(retry.dueAt))
		o.retryCh <- retry
	}()
}

func (o *Orchestrator) applyConfig(cfg config.Config) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.cfg = cfg
	o.linear = linear.NewClient(cfg)
	o.workspaces = workspace.NewManager(cfg)
	o.runner = agent.NewRunner(cfg, o.linear, o.workspaces, o.logger)
}

func (o *Orchestrator) stopAll() {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, entry := range o.running {
		entry.cancel()
	}
}

func (o *Orchestrator) availableSlots() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.availableSlotsLocked()
}

func (o *Orchestrator) availableSlotsLocked() int {
	available := o.cfg.Agent.MaxConcurrentAgents - len(o.running)
	if available < 0 {
		return 0
	}
	return available
}

func (o *Orchestrator) selectWorkerHost(preferred string) string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.selectWorkerHostLocked(preferred)
}

func (o *Orchestrator) selectWorkerHostLocked(preferred string) string {
	if len(o.cfg.Worker.SSHHosts) == 0 {
		return ""
	}
	hosts := make([]string, 0, len(o.cfg.Worker.SSHHosts))
	for _, host := range o.cfg.Worker.SSHHosts {
		if o.workerSlotsAvailableOnHostLocked(host) {
			hosts = append(hosts, host)
		}
	}
	if len(hosts) == 0 {
		return ":no_worker_capacity"
	}
	if preferred != "" {
		for _, host := range hosts {
			if host == preferred {
				return host
			}
		}
	}
	best := hosts[0]
	bestCount := o.runningWorkerHostCountLocked(best)
	for _, host := range hosts[1:] {
		if count := o.runningWorkerHostCountLocked(host); count < bestCount {
			best = host
			bestCount = count
		}
	}
	return best
}

func (o *Orchestrator) workerSlotsAvailableLocked(preferred string) bool {
	return o.selectWorkerHostLocked(preferred) != ":no_worker_capacity"
}

func (o *Orchestrator) workerSlotsAvailableOnHostLocked(host string) bool {
	if o.cfg.Worker.MaxConcurrentAgentsPerHost <= 0 {
		return true
	}
	return o.runningWorkerHostCountLocked(host) < o.cfg.Worker.MaxConcurrentAgentsPerHost
}

func (o *Orchestrator) runningWorkerHostCountLocked(host string) int {
	count := 0
	for _, entry := range o.running {
		if entry.workerHost == host {
			count++
		}
	}
	return count
}

func workerHostValue(host string) string {
	if host == "" {
		return "local"
	}
	return host
}

func (o *Orchestrator) Refresh() error {
	return o.tick(context.Background())
}

func (o *Orchestrator) Snapshot() map[string]any {
	o.mu.Lock()
	defer o.mu.Unlock()
	now := time.Now().UTC()
	running := make([]map[string]any, 0, len(o.running))
	for _, entry := range o.running {
		running = append(running, map[string]any{
			"issue_id":         entry.issue.ID,
			"issue_identifier": entry.issue.Identifier,
			"state":            entry.issue.State,
			"session_id":       entry.sessionID,
			"turn_count":       entry.turnCount,
			"last_event":       entry.lastEvent,
			"last_message":     entry.lastMessage,
			"started_at":       entry.startedAt.Format(time.RFC3339),
			"last_event_at":    entry.lastEventAt.Format(time.RFC3339),
			"workspace":        entry.workspace,
			"worker_host":      workerHostValue(entry.workerHost),
			"tokens": map[string]any{
				"input_tokens":  entry.tokens.Input,
				"output_tokens": entry.tokens.Output,
				"total_tokens":  entry.tokens.Total,
			},
		})
	}
	retrying := make([]map[string]any, 0, len(o.retrying))
	for _, retry := range o.retrying {
		retrying = append(retrying, map[string]any{
			"issue_id":         retry.issue.ID,
			"issue_identifier": retry.issue.Identifier,
			"attempt":          retry.attempt,
			"due_at":           retry.dueAt.Format(time.RFC3339),
			"error":            retry.error,
		})
	}
	secondsRunning := o.secondsEnded
	for _, entry := range o.running {
		secondsRunning += now.Sub(entry.startedAt).Seconds()
	}
	return map[string]any{
		"generated_at": now.Format(time.RFC3339),
		"counts": map[string]any{
			"running":  len(running),
			"retrying": len(retrying),
		},
		"running":  running,
		"retrying": retrying,
		"codex_totals": map[string]any{
			"input_tokens":    o.codexTotals.Input,
			"output_tokens":   o.codexTotals.Output,
			"total_tokens":    o.codexTotals.Total,
			"seconds_running": secondsRunning,
		},
		"rate_limits": o.rateLimits,
	}
}

func (o *Orchestrator) IssueDetails(identifier string) (map[string]any, bool) {
	snapshot := o.Snapshot()
	for _, raw := range snapshot["running"].([]map[string]any) {
		if raw["issue_identifier"] == identifier {
			return map[string]any{
				"issue_identifier": identifier,
				"issue_id":         raw["issue_id"],
				"status":           "running",
				"workspace":        map[string]any{"path": raw["workspace"]},
				"running":          raw,
				"retry":            nil,
			}, true
		}
	}
	for _, raw := range snapshot["retrying"].([]map[string]any) {
		if raw["issue_identifier"] == identifier {
			return map[string]any{
				"issue_identifier": identifier,
				"issue_id":         raw["issue_id"],
				"status":           "retrying",
				"retry":            raw,
			}, true
		}
	}
	return nil, false
}

func isActive(cfg config.Config, state string) bool {
	_, ok := cfg.ActiveStateSet()[strings.ToLower(strings.TrimSpace(state))]
	return ok
}

func isTerminal(cfg config.Config, state string) bool {
	_, ok := cfg.TerminalStateSet()[strings.ToLower(strings.TrimSpace(state))]
	return ok
}

func backoff(cfg config.Config, attempt int) time.Duration {
	if attempt <= 1 {
		return time.Second
	}
	delay := 10 * time.Second
	for i := 2; i < attempt; i++ {
		delay *= 2
		if delay >= cfg.Agent.MaxRetryBackoff {
			return cfg.Agent.MaxRetryBackoff
		}
	}
	if delay > cfg.Agent.MaxRetryBackoff {
		return cfg.Agent.MaxRetryBackoff
	}
	return delay
}

func nextAttempt(attempt *int) int {
	if attempt == nil || *attempt < 1 {
		return 1
	}
	return *attempt + 1
}

func attemptValue(attempt *int) any {
	if attempt == nil {
		return nil
	}
	return *attempt
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
