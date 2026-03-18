package orchestrator

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	githubclient "github.com/openai/symphony/go/internal/github"
	"github.com/openai/symphony/go/internal/linear"
)

const (
	workflowRoleSource = "source"
	workflowRoleReview = "review"

	workflowStateAwaitingReview   = "awaiting_review"
	workflowStateChangesRequested = "changes_requested"
	workflowStateMerged           = "merged"
	workflowStateReviewPending    = "review_pending"
	workflowStateReviewActive     = "review_active"
	workflowStateClosedUnmerged   = "closed_unmerged"
)

type workflowInfo struct {
	Role                  string
	WorkflowState         string
	CompletionGate        string
	SourceIssueID         string
	SourceIssueIdentifier string
	ReviewIssueID         string
	ReviewIssueIdentifier string
	Branch                string
	PRNumber              int
	PRURL                 string
	GitHubState           string
	ReviewDecision        string
	ReviewRound           int
	LastSyncAt            time.Time
	LastSyncError         string
	SuppressDispatch      bool
	SkipReason            string
}

type postRunAction int

const (
	postRunRetry postRunAction = iota
	postRunWait
	postRunComplete
)

func isReviewIssue(issue linear.Issue) bool {
	return parseSourceIdentifierFromReviewTitle(issue.Title) != ""
}

func parseSourceIdentifierFromReviewTitle(title string) string {
	trimmed := strings.TrimSpace(title)
	if !strings.HasPrefix(strings.ToUpper(trimmed), "REVIEW:") {
		return ""
	}
	rest := strings.TrimSpace(trimmed[len("REVIEW:"):])
	if rest == "" {
		return ""
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return ""
	}
	return strings.TrimSpace(fields[0])
}

func reviewIssueTitle(source linear.Issue) string {
	return "REVIEW: " + source.Identifier + " " + source.Title
}

func prBody(source linear.Issue) string {
	return strings.TrimSpace(fmt.Sprintf(`## Linear
- Source issue: %s

This pull request is managed by Symphony runtime automation. Review-ticket creation, PR status sync, and merge-state synchronization back to Linear are handled by Symphony rather than by the worker agent.`, source.Identifier))
}

func reviewIssueDescription(source linear.Issue, branch string, pr *githubclient.PullRequest, round int) string {
	return strings.TrimSpace(fmt.Sprintf(`This is the runtime-managed review issue for %s.

Source issue: %s
Branch: %s
PR: %s
PR number: %d
Review round: %d

Review this source pull request. Do not create a new branch or PR for this review issue.`, source.Identifier, source.Identifier, branch, pr.URL, pr.Number, round))
}

func (o *Orchestrator) syncWorkflowState(ctx context.Context, issues []linear.Issue) {
	if o.github == nil || !o.github.Enabled() {
		o.mu.Lock()
		for _, issue := range issues {
			delete(o.workflow, issue.ID)
		}
		o.mu.Unlock()
		return
	}

	candidateByIdentifier := make(map[string]linear.Issue, len(issues))
	activeReviews := map[string]linear.Issue{}
	for _, issue := range issues {
		candidateByIdentifier[issue.Identifier] = issue
		if sourceIdentifier := parseSourceIdentifierFromReviewTitle(issue.Title); sourceIdentifier != "" {
			activeReviews[sourceIdentifier] = issue
		}
	}

	for _, issue := range issues {
		if isReviewIssue(issue) {
			o.syncReviewIssueState(ctx, issue, candidateByIdentifier)
			continue
		}
		o.syncSourceIssueState(ctx, issue, activeReviews)
	}
}

func (o *Orchestrator) syncSourceIssueState(ctx context.Context, issue linear.Issue, activeReviews map[string]linear.Issue) {
	info := workflowInfo{
		Role:           workflowRoleSource,
		CompletionGate: "github+linear",
		LastSyncAt:     time.Now().UTC(),
	}
	workspacePath, _ := o.workspaces.Path(issue.Identifier, "")
	if reviewIssue, ok := activeReviews[issue.Identifier]; ok {
		info.SuppressDispatch = true
		info.SkipReason = "awaiting_review"
		info.WorkflowState = workflowStateAwaitingReview
		info.ReviewIssueID = reviewIssue.ID
		info.ReviewIssueIdentifier = reviewIssue.Identifier
		if branch, pr, err := o.resolveSourceBranchAndPR(ctx, workspacePath, issue); err == nil && pr != nil {
			info.Branch = branch
			info.PRNumber = pr.Number
			info.PRURL = pr.URL
			info.GitHubState = normalizePRState(pr)
			info.ReviewDecision = pr.ReviewDecision
		} else if err != nil {
			info.LastSyncError = err.Error()
		}
		o.setWorkflow(issue.ID, info)
		return
	}

	branch, pr, err := o.resolveSourceBranchAndPR(ctx, workspacePath, issue)
	if err != nil {
		info.LastSyncError = err.Error()
		o.setWorkflow(issue.ID, info)
		return
	}
	info.Branch = branch
	if pr != nil {
		info.PRNumber = pr.Number
		info.PRURL = pr.URL
		info.GitHubState = normalizePRState(pr)
		info.ReviewDecision = pr.ReviewDecision
		if pr.MergedAt != "" {
			info.WorkflowState = workflowStateMerged
			info.SuppressDispatch = true
			info.SkipReason = "merged"
			_ = o.transitionIssueToTerminal(ctx, issue)
		}
	}
	o.setWorkflow(issue.ID, info)
}

func (o *Orchestrator) syncReviewIssueState(ctx context.Context, issue linear.Issue, candidateByIdentifier map[string]linear.Issue) {
	info := workflowInfo{
		Role:                  workflowRoleReview,
		CompletionGate:        "github+linear",
		SourceIssueIdentifier: parseSourceIdentifierFromReviewTitle(issue.Title),
		LastSyncAt:            time.Now().UTC(),
	}
	if info.SourceIssueIdentifier == "" {
		info.LastSyncError = "missing_source_identifier"
		o.setWorkflow(issue.ID, info)
		return
	}
	sourceIssue, ok := candidateByIdentifier[info.SourceIssueIdentifier]
	if !ok {
		found, err := o.linear.FindIssueByIdentifier(ctx, info.SourceIssueIdentifier)
		if err != nil {
			info.LastSyncError = err.Error()
			o.setWorkflow(issue.ID, info)
			return
		}
		if found == nil {
			info.LastSyncError = "source_issue_not_found"
			o.setWorkflow(issue.ID, info)
			return
		}
		sourceIssue = *found
	}
	info.SourceIssueID = sourceIssue.ID
	workspacePath, _ := o.workspaces.Path(sourceIssue.Identifier, "")
	branch, pr, err := o.resolveSourceBranchAndPR(ctx, workspacePath, sourceIssue)
	if err != nil {
		info.LastSyncError = err.Error()
		o.setWorkflow(issue.ID, info)
		return
	}
	info.Branch = branch
	if pr == nil {
		info.WorkflowState = workflowStateReviewPending
		o.setWorkflow(issue.ID, info)
		return
	}
	info.PRNumber = pr.Number
	info.PRURL = pr.URL
	info.GitHubState = normalizePRState(pr)
	info.ReviewDecision = pr.ReviewDecision
	switch {
	case pr.MergedAt != "":
		info.WorkflowState = workflowStateMerged
		info.SuppressDispatch = true
		info.SkipReason = "merged"
		_ = o.transitionIssueToTerminal(ctx, issue)
		_ = o.transitionIssueToTerminal(ctx, sourceIssue)
	case strings.EqualFold(pr.ReviewDecision, "APPROVED"):
		if err := o.github.MergePR(ctx, workspacePath, pr.Number); err != nil {
			info.WorkflowState = workflowStateReviewActive
			info.LastSyncError = err.Error()
		} else {
			info.WorkflowState = workflowStateMerged
			info.SuppressDispatch = true
			info.SkipReason = "merged"
			_ = o.transitionIssueToTerminal(ctx, issue)
			_ = o.transitionIssueToTerminal(ctx, sourceIssue)
		}
	case strings.EqualFold(pr.ReviewDecision, "CHANGES_REQUESTED"):
		info.WorkflowState = workflowStateChangesRequested
		info.SuppressDispatch = true
		info.SkipReason = "changes_requested_recorded"
		_ = o.transitionIssueToTerminal(ctx, issue)
	default:
		info.WorkflowState = workflowStateReviewActive
	}
	o.setWorkflow(issue.ID, info)
}

func (o *Orchestrator) evaluatePostRun(ctx context.Context, result linear.Issue, workspace string) postRunAction {
	if o.github == nil || !o.github.Enabled() {
		return postRunRetry
	}
	if isReviewIssue(result) {
		o.syncReviewIssueState(ctx, result, map[string]linear.Issue{})
		if info, ok := o.workflowInfo(result.ID); ok {
			switch info.WorkflowState {
			case workflowStateMerged, workflowStateChangesRequested:
				return postRunComplete
			}
		}
		return postRunRetry
	}
	branch, pr, err := o.resolveSourceBranchAndPR(ctx, workspace, result)
	if err != nil {
		o.logger.Warn("post-run github sync failed", "issue_identifier", result.Identifier, "error", err)
		return postRunRetry
	}
	if pr != nil && pr.MergedAt != "" {
		_ = o.transitionIssueToTerminal(ctx, result)
		return postRunComplete
	}
	if pr == nil && strings.TrimSpace(branch) != "" {
		pr, err = o.github.EnsurePR(ctx, workspace, branch, result.Identifier+" "+result.Title, prBody(result))
		if err != nil {
			o.logger.Warn("pr ensure failed", "issue_identifier", result.Identifier, "branch", branch, "error", err)
			return postRunRetry
		}
	}
	if pr == nil {
		return postRunRetry
	}
	if _, err := o.ensureReviewIssue(ctx, result, branch, pr); err != nil {
		o.logger.Warn("review issue ensure failed", "issue_identifier", result.Identifier, "branch", branch, "pr_number", pr.Number, "error", err)
		return postRunRetry
	}
	o.setWorkflow(result.ID, workflowInfo{
		Role:                  workflowRoleSource,
		WorkflowState:         workflowStateAwaitingReview,
		CompletionGate:        "github+linear",
		SourceIssueID:         result.ID,
		SourceIssueIdentifier: result.Identifier,
		Branch:                branch,
		PRNumber:              pr.Number,
		PRURL:                 pr.URL,
		GitHubState:           normalizePRState(pr),
		ReviewDecision:        pr.ReviewDecision,
		SuppressDispatch:      true,
		SkipReason:            "awaiting_review",
		LastSyncAt:            time.Now().UTC(),
	})
	return postRunWait
}

func (o *Orchestrator) ensureReviewIssue(ctx context.Context, source linear.Issue, branch string, pr *githubclient.PullRequest) (*linear.Issue, error) {
	title := reviewIssueTitle(source)
	existing, err := o.linear.FindIssueByTitle(ctx, title)
	if err != nil {
		return nil, err
	}
	description := reviewIssueDescription(source, branch, pr, 1)
	if existing == nil {
		reviewIssue, err := o.linear.CreateIssue(ctx, source, title, description, firstStateOrDefault(o.cfg.Tracker.ActiveStates, "Todo"))
		if err != nil {
			return nil, err
		}
		return reviewIssue, nil
	}
	if strings.TrimSpace(existing.Description) != description {
		if err := o.linear.UpdateIssueDescription(ctx, existing.ID, description); err != nil {
			return nil, err
		}
		existing.Description = description
	}
	return existing, nil
}

func (o *Orchestrator) transitionIssueToTerminal(ctx context.Context, issue linear.Issue) error {
	if isTerminal(o.cfg, issue.State) {
		return nil
	}
	for _, stateName := range o.cfg.Tracker.TerminalStates {
		if err := o.linear.UpdateIssueState(ctx, issue, stateName); err == nil {
			return nil
		}
	}
	return fmt.Errorf("failed_to_transition_issue_to_terminal")
}

func (o *Orchestrator) resolveSourceBranchAndPR(ctx context.Context, workspacePath string, issue linear.Issue) (string, *githubclient.PullRequest, error) {
	if o.github == nil || !o.github.Enabled() {
		return "", nil, nil
	}
	if strings.TrimSpace(workspacePath) == "" {
		return "", nil, nil
	}
	if _, err := os.Stat(workspacePath); err != nil {
		return "", nil, nil
	}
	branch, err := o.github.CurrentBranch(ctx, workspacePath)
	if err != nil {
		return "", nil, err
	}
	if branch == "" {
		branch = strings.TrimSpace(issue.BranchName)
	}
	baseBranch, err := o.github.DefaultBaseBranch(ctx, workspacePath)
	if err == nil && branch == baseBranch {
		return branch, nil, nil
	}
	if strings.TrimSpace(branch) == "" {
		return "", nil, nil
	}
	exists, err := o.github.RemoteBranchExists(ctx, workspacePath, branch)
	if err != nil || !exists {
		return branch, nil, err
	}
	pr, err := o.github.FindPRForBranch(ctx, workspacePath, branch)
	if err != nil {
		return branch, nil, err
	}
	return branch, pr, nil
}

func normalizePRState(pr *githubclient.PullRequest) string {
	if pr == nil {
		return ""
	}
	switch {
	case strings.TrimSpace(pr.MergedAt) != "":
		return "merged"
	case strings.EqualFold(pr.State, "CLOSED"):
		return "closed"
	default:
		return strings.ToLower(strings.TrimSpace(pr.State))
	}
}

func (o *Orchestrator) setWorkflow(issueID string, info workflowInfo) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.workflow == nil {
		o.workflow = map[string]workflowInfo{}
	}
	o.workflow[issueID] = info
	if entry, ok := o.running[issueID]; ok {
		entry.workflow = info
	}
}

func (o *Orchestrator) workflowInfo(issueID string) (workflowInfo, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	info, ok := o.workflow[issueID]
	return info, ok
}

func firstStateOrDefault(states []string, fallback string) string {
	if len(states) == 0 {
		return fallback
	}
	return states[0]
}
