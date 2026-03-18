package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

type Client struct{}

type PullRequest struct {
	Number           int
	URL              string
	State            string
	IsDraft          bool
	MergedAt         string
	HeadRefName      string
	BaseRefName      string
	HeadRefOid       string
	ReviewDecision   string
	MergeStateStatus string
}

func NewClient() *Client {
	return &Client{}
}

func (c *Client) Enabled() bool {
	if strings.TrimSpace(os.Getenv("GH_TOKEN")) == "" && strings.TrimSpace(os.Getenv("GITHUB_TOKEN")) == "" {
		return false
	}
	_, err := exec.LookPath("gh")
	return err == nil
}

func (c *Client) CurrentBranch(ctx context.Context, workspace string) (string, error) {
	out, err := c.run(ctx, workspace, "git", "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", err
	}
	branch := strings.TrimSpace(string(out))
	switch branch {
	case "", "HEAD":
		return "", nil
	default:
		return branch, nil
	}
}

func (c *Client) RemoteBranchExists(ctx context.Context, workspace, branch string) (bool, error) {
	if strings.TrimSpace(branch) == "" {
		return false, nil
	}
	cmd := exec.CommandContext(ctx, "git", "ls-remote", "--exit-code", "--heads", "origin", branch)
	cmd.Dir = workspace
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (c *Client) DefaultBaseBranch(ctx context.Context, workspace string) (string, error) {
	type repoView struct {
		DefaultBranchRef struct {
			Name string `json:"name"`
		} `json:"defaultBranchRef"`
	}
	out, err := c.run(ctx, workspace, "gh", "repo", "view", "--json", "defaultBranchRef")
	if err != nil {
		return "", err
	}
	var payload repoView
	if err := json.Unmarshal(out, &payload); err != nil {
		return "", err
	}
	branch := strings.TrimSpace(payload.DefaultBranchRef.Name)
	if branch == "" {
		branch = "main"
	}
	return branch, nil
}

func (c *Client) FindPRForBranch(ctx context.Context, workspace, branch string) (*PullRequest, error) {
	if strings.TrimSpace(branch) == "" {
		return nil, nil
	}
	out, err := c.run(ctx, workspace, "gh", "pr", "list", "--head", branch, "--state", "all", "--limit", "1", "--json", "number,url,state,isDraft,mergedAt,headRefName,baseRefName,headRefOid,reviewDecision,mergeStateStatus")
	if err != nil {
		return nil, err
	}
	var prs []struct {
		Number           int    `json:"number"`
		URL              string `json:"url"`
		State            string `json:"state"`
		IsDraft          bool   `json:"isDraft"`
		MergedAt         string `json:"mergedAt"`
		HeadRefName      string `json:"headRefName"`
		BaseRefName      string `json:"baseRefName"`
		HeadRefOid       string `json:"headRefOid"`
		ReviewDecision   string `json:"reviewDecision"`
		MergeStateStatus string `json:"mergeStateStatus"`
	}
	if err := json.Unmarshal(out, &prs); err != nil {
		return nil, err
	}
	if len(prs) == 0 {
		return nil, nil
	}
	pr := prs[0]
	return &PullRequest{
		Number:           pr.Number,
		URL:              pr.URL,
		State:            strings.TrimSpace(pr.State),
		IsDraft:          pr.IsDraft,
		MergedAt:         strings.TrimSpace(pr.MergedAt),
		HeadRefName:      strings.TrimSpace(pr.HeadRefName),
		BaseRefName:      strings.TrimSpace(pr.BaseRefName),
		HeadRefOid:       strings.TrimSpace(pr.HeadRefOid),
		ReviewDecision:   strings.TrimSpace(pr.ReviewDecision),
		MergeStateStatus: strings.TrimSpace(pr.MergeStateStatus),
	}, nil
}

func (c *Client) EnsurePR(ctx context.Context, workspace, branch, title, body string) (*PullRequest, error) {
	if pr, err := c.FindPRForBranch(ctx, workspace, branch); err != nil || pr != nil {
		return pr, err
	}
	base, err := c.DefaultBaseBranch(ctx, workspace)
	if err != nil {
		return nil, err
	}
	args := []string{"pr", "create", "--head", branch, "--base", base, "--title", title, "--body", body}
	if _, err := c.run(ctx, workspace, "gh", args...); err != nil {
		return nil, err
	}
	return c.FindPRForBranch(ctx, workspace, branch)
}

func (c *Client) MergePR(ctx context.Context, workspace string, number int) error {
	if number <= 0 {
		return fmt.Errorf("invalid_pr_number")
	}
	_, err := c.run(ctx, workspace, "gh", "pr", "merge", strconv.Itoa(number), "--merge", "--delete-branch=false")
	return err
}

func (c *Client) run(ctx context.Context, workspace, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = workspace
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%s %s failed: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}
