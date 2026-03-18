package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"slices"
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
	workflowStateApproved         = "approved"
	workflowStateCommentOnly      = "comment_only"
	workflowStateBlocked          = "blocked"
	workflowStateClosedUnmerged   = "closed_unmerged"

	reviewModeLinearArtifact = "linear_artifact"
)

type workflowInfo struct {
	Role                  string
	WorkflowState         string
	CompletionGate        string
	ReviewMode            string
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
	ReviewSummary         string
	ReviewedSHA           string
	ReviewArtifactPath    string
	LastSyncAt            time.Time
	LastReviewSyncedAt    time.Time
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

type reviewResult struct {
	Decision         string
	Summary          string
	RequiredChanges  []string
	ResidualRisks    []string
	ReviewedSHA      string
	CommentID        string
	CommentCreatedAt time.Time
}

var reviewResultPattern = regexp.MustCompile("(?s)SYMPHONY_REVIEW_RESULT.*?```json\\s*(\\{.*?\\})\\s*```")

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
		ReviewMode:            reviewModeLinearArtifact,
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
	result, err := o.latestReviewResult(ctx, issue.ID)
	if err != nil {
		info.LastSyncError = err.Error()
		o.setWorkflow(issue.ID, info)
		return
	}
	if result != nil {
		info.ReviewDecision = result.Decision
		info.ReviewSummary = result.Summary
		info.ReviewedSHA = result.ReviewedSHA
		info.ReviewArtifactPath = "Linear issue comment " + result.CommentID
		info.LastReviewSyncedAt = result.CommentCreatedAt
	}
	prev, _ := o.workflowInfo(issue.ID)
	switch {
	case pr.MergedAt != "":
		info.WorkflowState = workflowStateMerged
		info.SuppressDispatch = true
		info.SkipReason = "merged"
		_ = o.transitionIssueToTerminal(ctx, issue)
		_ = o.transitionIssueToTerminal(ctx, sourceIssue)
	case result != nil && result.Decision == workflowStateApproved:
		if err := o.github.MergePR(ctx, workspacePath, pr.Number); err != nil {
			info.WorkflowState = workflowStateReviewActive
			info.LastSyncError = err.Error()
		} else {
			info.WorkflowState = workflowStateMerged
			info.SuppressDispatch = true
			info.SkipReason = "merged"
			_ = o.transitionIssueToTerminal(ctx, issue)
			_ = o.transitionIssueToTerminal(ctx, sourceIssue)
			o.syncReviewDecisionSummary(ctx, prev, info, sourceIssue, issue, pr, result)
		}
	case result != nil && result.Decision == workflowStateChangesRequested:
		info.WorkflowState = workflowStateChangesRequested
		info.SuppressDispatch = true
		info.SkipReason = "changes_requested_recorded"
		_ = o.transitionIssueToTerminal(ctx, issue)
		o.syncReviewDecisionSummary(ctx, prev, info, sourceIssue, issue, pr, result)
	case result != nil && result.Decision == workflowStateCommentOnly:
		info.WorkflowState = workflowStateCommentOnly
		info.SuppressDispatch = true
		info.SkipReason = "comment_recorded"
		_ = o.transitionIssueToTerminal(ctx, issue)
		o.syncReviewDecisionSummary(ctx, prev, info, sourceIssue, issue, pr, result)
	case result != nil && result.Decision == workflowStateBlocked:
		info.WorkflowState = workflowStateBlocked
		info.SuppressDispatch = true
		info.SkipReason = "review_blocked"
		_ = o.transitionIssueToTerminal(ctx, issue)
		o.syncReviewDecisionSummary(ctx, prev, info, sourceIssue, issue, pr, result)
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
			case workflowStateMerged, workflowStateChangesRequested, workflowStateApproved, workflowStateCommentOnly, workflowStateBlocked:
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

func (o *Orchestrator) latestReviewResult(ctx context.Context, issueID string) (*reviewResult, error) {
	comments, err := o.linear.ListIssueComments(ctx, issueID, 50)
	if err != nil {
		return nil, err
	}
	return parseLatestReviewResult(comments), nil
}

func parseLatestReviewResult(comments []linear.Comment) *reviewResult {
	if len(comments) == 0 {
		return nil
	}
	sorted := slices.Clone(comments)
	slices.SortStableFunc(sorted, func(a, b linear.Comment) int {
		at := commentSortTime(a)
		bt := commentSortTime(b)
		switch {
		case at.Before(bt):
			return -1
		case at.After(bt):
			return 1
		default:
			return strings.Compare(a.ID, b.ID)
		}
	})
	for i := len(sorted) - 1; i >= 0; i-- {
		if result := parseReviewResultComment(sorted[i]); result != nil {
			return result
		}
	}
	return nil
}

func parseReviewResultComment(comment linear.Comment) *reviewResult {
	match := reviewResultPattern.FindStringSubmatch(comment.Body)
	if len(match) < 2 {
		return nil
	}
	var payload struct {
		Decision        string   `json:"decision"`
		Summary         string   `json:"summary"`
		RequiredChanges []string `json:"required_changes"`
		ResidualRisks   []string `json:"residual_risks"`
		ReviewedSHA     string   `json:"reviewed_sha"`
	}
	if err := json.Unmarshal([]byte(match[1]), &payload); err != nil {
		return nil
	}
	decision := normalizeReviewDecision(payload.Decision)
	if decision == "" {
		return nil
	}
	return &reviewResult{
		Decision:         decision,
		Summary:          strings.TrimSpace(payload.Summary),
		RequiredChanges:  compactStrings(payload.RequiredChanges),
		ResidualRisks:    compactStrings(payload.ResidualRisks),
		ReviewedSHA:      strings.TrimSpace(payload.ReviewedSHA),
		CommentID:        comment.ID,
		CommentCreatedAt: commentSortTime(comment),
	}
}

func normalizeReviewDecision(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "approve", workflowStateApproved:
		return workflowStateApproved
	case "request_changes", "changes_requested":
		return workflowStateChangesRequested
	case "comment_only", "comment":
		return workflowStateCommentOnly
	case "blocked":
		return workflowStateBlocked
	default:
		return ""
	}
}

func compactStrings(in []string) []string {
	out := make([]string, 0, len(in))
	for _, item := range in {
		item = strings.TrimSpace(item)
		if item != "" {
			out = append(out, item)
		}
	}
	return out
}

func commentSortTime(comment linear.Comment) time.Time {
	if comment.UpdatedAt != nil {
		return comment.UpdatedAt.UTC()
	}
	if comment.CreatedAt != nil {
		return comment.CreatedAt.UTC()
	}
	return time.Time{}
}

func (o *Orchestrator) syncReviewDecisionSummary(ctx context.Context, prev, next workflowInfo, sourceIssue, reviewIssue linear.Issue, pr *githubclient.PullRequest, result *reviewResult) {
	if result == nil {
		return
	}
	if prev.WorkflowState == next.WorkflowState && prev.ReviewDecision == next.ReviewDecision && prev.ReviewedSHA == next.ReviewedSHA {
		return
	}
	body := reviewSummaryComment(sourceIssue, reviewIssue, pr, result)
	if strings.TrimSpace(body) == "" {
		return
	}
	_ = o.linear.CreateComment(ctx, sourceIssue.ID, body)
}

func reviewSummaryComment(sourceIssue, reviewIssue linear.Issue, pr *githubclient.PullRequest, result *reviewResult) string {
	if result == nil {
		return ""
	}
	lines := []string{
		fmt.Sprintf("Symphony review result for %s from %s.", sourceIssue.Identifier, reviewIssue.Identifier),
		"",
		fmt.Sprintf("- Decision: %s", result.Decision),
	}
	if pr != nil {
		lines = append(lines, fmt.Sprintf("- PR: #%d %s", pr.Number, pr.URL))
	}
	if result.ReviewedSHA != "" {
		lines = append(lines, fmt.Sprintf("- Reviewed commit: `%s`", result.ReviewedSHA))
	}
	if result.Summary != "" {
		lines = append(lines, "", "Summary:", result.Summary)
	}
	if len(result.RequiredChanges) > 0 {
		lines = append(lines, "", "Required changes:")
		for _, item := range result.RequiredChanges {
			lines = append(lines, "- "+item)
		}
	}
	if len(result.ResidualRisks) > 0 {
		lines = append(lines, "", "Residual risks:")
		for _, item := range result.ResidualRisks {
			lines = append(lines, "- "+item)
		}
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
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
