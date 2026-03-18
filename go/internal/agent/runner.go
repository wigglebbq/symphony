package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/openai/symphony/go/internal/config"
	"github.com/openai/symphony/go/internal/linear"
	"github.com/openai/symphony/go/internal/prompt"
	"github.com/openai/symphony/go/internal/workspace"
)

type RunResult struct {
	Issue      linear.Issue
	Workspace  string
	WorkerHost string
	SessionID  string
	NormalExit bool
	Err        error
}

type Runner struct {
	cfg        config.Config
	linear     *linear.Client
	workspaces *workspace.Manager
	logger     *slog.Logger
}

func NewRunner(cfg config.Config, lc *linear.Client, wm *workspace.Manager, logger *slog.Logger) *Runner {
	return &Runner{cfg: cfg, linear: lc, workspaces: wm, logger: logger}
}

func (r *Runner) Run(ctx context.Context, issue linear.Issue, attempt *int, workerHost string, onEvent func(Event)) RunResult {
	ws, err := r.workspaces.Ensure(ctx, issue, workerHost)
	if err != nil {
		return RunResult{Issue: issue, Err: err}
	}
	defer func() {
		if err := r.workspaces.RunAfterRun(context.Background(), ws); err != nil {
			r.logger.Warn("after_run hook failed", "issue_id", issue.ID, "issue_identifier", issue.Identifier, "error", err)
		}
	}()
	if err := r.workspaces.RunBeforeRun(ctx, ws); err != nil {
		return RunResult{Issue: issue, Workspace: ws.Path, WorkerHost: workerHost, Err: err}
	}
	session, err := StartSession(ctx, r.cfg, ws.Path, workerHost, r.linear, r.logger)
	if err != nil {
		return RunResult{Issue: issue, Workspace: ws.Path, WorkerHost: workerHost, Err: err}
	}
	defer session.Stop()
	current := issue
	var sessionID string
	for turn := 1; turn <= r.cfg.Agent.MaxTurns; turn++ {
		turnAttempt := attempt
		promptText, err := prompt.Build(r.cfg, current, turnAttempt)
		if err != nil {
			return RunResult{Issue: current, Workspace: ws.Path, WorkerHost: workerHost, Err: err}
		}
		if turn > 1 {
			promptText = continuationPrompt(turn, r.cfg.Agent.MaxTurns)
		}
		sessionID, err = session.RunTurn(ctx, promptText, current, onEvent)
		if err != nil {
			return RunResult{Issue: current, Workspace: ws.Path, WorkerHost: workerHost, SessionID: sessionID, Err: err}
		}
		refreshed, err := r.linear.FetchIssueStatesByIDs(ctx, []string{current.ID})
		if err != nil {
			return RunResult{Issue: current, Workspace: ws.Path, WorkerHost: workerHost, SessionID: sessionID, Err: fmt.Errorf("issue_state_refresh_failed: %w", err)}
		}
		if len(refreshed) == 0 || !isActive(r.cfg, refreshed[0].State) {
			return RunResult{Issue: current, Workspace: ws.Path, WorkerHost: workerHost, SessionID: sessionID, NormalExit: true}
		}
		current = refreshed[0]
	}
	return RunResult{Issue: current, Workspace: ws.Path, WorkerHost: workerHost, SessionID: sessionID, NormalExit: true}
}

func continuationPrompt(turn, maxTurns int) string {
	return fmt.Sprintf(strings.TrimSpace(`
Continuation guidance:

- The previous Codex turn completed normally, but the issue is still in an active state.
- This is continuation turn #%d of %d for the current worker run.
- Resume from the current workspace and thread state instead of restating the original task.
- Focus only on the remaining issue work.
`), turn, maxTurns)
}

func isActive(cfg config.Config, state string) bool {
	_, ok := cfg.ActiveStateSet()[strings.ToLower(strings.TrimSpace(state))]
	return ok
}
