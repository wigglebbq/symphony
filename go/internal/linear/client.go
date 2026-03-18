package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/openai/symphony/go/internal/config"
)

type BlockerRef struct {
	ID         string `json:"id,omitempty"`
	Identifier string `json:"identifier,omitempty"`
	State      string `json:"state,omitempty"`
}

type Issue struct {
	ID          string       `json:"id"`
	Identifier  string       `json:"identifier"`
	Title       string       `json:"title"`
	Description string       `json:"description,omitempty"`
	Priority    *int         `json:"priority,omitempty"`
	State       string       `json:"state"`
	StateID     string       `json:"state_id,omitempty"`
	BranchName  string       `json:"branch_name,omitempty"`
	URL         string       `json:"url,omitempty"`
	Labels      []string     `json:"labels,omitempty"`
	BlockedBy   []BlockerRef `json:"blocked_by,omitempty"`
	ProjectID   string       `json:"project_id,omitempty"`
	TeamID      string       `json:"team_id,omitempty"`
	CreatedAt   *time.Time   `json:"created_at,omitempty"`
	UpdatedAt   *time.Time   `json:"updated_at,omitempty"`
}

type Client struct {
	cfg        config.Config
	httpClient *http.Client
}

func NewClient(cfg config.Config) *Client {
	return &Client{
		cfg: cfg,
		httpClient: &http.Client{
			Timeout: 45 * time.Second,
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
				ForceAttemptHTTP2:     false,
				MaxIdleConns:          100,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ExpectContinueTimeout: time.Second,
			},
		},
	}
}

func (c *Client) FetchCandidateIssues(ctx context.Context) ([]Issue, error) {
	var out []Issue
	var after *string
	for {
		vars := map[string]any{
			"projectSlug": c.cfg.Tracker.ProjectSlug,
			"stateNames":  c.cfg.Tracker.ActiveStates,
			"first":       50,
			"after":       after,
		}
		payload, err := c.graphql(ctx, candidateIssuesQuery, vars)
		if err != nil {
			return nil, err
		}
		issuesNode, ok := digMap(payload, "data", "issues")
		if !ok {
			return nil, fmt.Errorf("linear_unknown_payload")
		}
		nodes := sliceMap(issuesNode["nodes"])
		for _, node := range nodes {
			issue, ok := normalizeIssue(node)
			if ok {
				out = append(out, issue)
			}
		}
		pageInfo, ok := digMap(issuesNode, "pageInfo")
		if !ok {
			return nil, fmt.Errorf("linear_unknown_payload")
		}
		hasNext, _ := pageInfo["hasNextPage"].(bool)
		if !hasNext {
			break
		}
		next, _ := pageInfo["endCursor"].(string)
		if strings.TrimSpace(next) == "" {
			return nil, fmt.Errorf("linear_missing_end_cursor")
		}
		after = &next
	}
	return sortIssues(out), nil
}

func (c *Client) FetchIssuesByStates(ctx context.Context, states []string) ([]Issue, error) {
	payload, err := c.graphql(ctx, issuesByStatesQuery, map[string]any{
		"projectSlug": c.cfg.Tracker.ProjectSlug,
		"stateNames":  states,
		"first":       250,
	})
	if err != nil {
		return nil, err
	}
	issuesNode, ok := digMap(payload, "data", "issues")
	if !ok {
		return nil, fmt.Errorf("linear_unknown_payload")
	}
	out := make([]Issue, 0)
	for _, node := range sliceMap(issuesNode["nodes"]) {
		issue, ok := normalizeIssue(node)
		if ok {
			out = append(out, issue)
		}
	}
	return out, nil
}

func (c *Client) FetchIssueStatesByIDs(ctx context.Context, ids []string) ([]Issue, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	payload, err := c.graphql(ctx, issueStatesByIDsQuery, map[string]any{"ids": ids})
	if err != nil {
		return nil, err
	}
	data, ok := digMap(payload, "data")
	if !ok {
		return nil, fmt.Errorf("linear_unknown_payload")
	}
	issuesNode, ok := data["issues"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("linear_unknown_payload")
	}
	nodes := sliceMap(issuesNode["nodes"])
	out := make([]Issue, 0, len(nodes))
	for _, node := range nodes {
		issue, ok := normalizeIssue(node)
		if ok {
			out = append(out, issue)
		}
	}
	return out, nil
}

func (c *Client) FindIssueByIdentifier(ctx context.Context, identifier string) (*Issue, error) {
	if strings.TrimSpace(identifier) == "" {
		return nil, nil
	}
	payload, err := c.graphql(ctx, issueByIdentifierQuery, map[string]any{"identifier": identifier})
	if err != nil {
		return nil, err
	}
	issuesNode, ok := digMap(payload, "data", "issues")
	if !ok {
		return nil, fmt.Errorf("linear_unknown_payload")
	}
	nodes := sliceMap(issuesNode["nodes"])
	if len(nodes) == 0 {
		return nil, nil
	}
	issue, ok := normalizeIssue(nodes[0])
	if !ok {
		return nil, nil
	}
	return &issue, nil
}

func (c *Client) FindIssueByTitle(ctx context.Context, title string) (*Issue, error) {
	if strings.TrimSpace(title) == "" {
		return nil, nil
	}
	payload, err := c.graphql(ctx, issueByTitleQuery, map[string]any{
		"projectSlug": c.cfg.Tracker.ProjectSlug,
		"title":       title,
	})
	if err != nil {
		return nil, err
	}
	issuesNode, ok := digMap(payload, "data", "issues")
	if !ok {
		return nil, fmt.Errorf("linear_unknown_payload")
	}
	nodes := sliceMap(issuesNode["nodes"])
	if len(nodes) == 0 {
		return nil, nil
	}
	issue, ok := normalizeIssue(nodes[0])
	if !ok {
		return nil, nil
	}
	return &issue, nil
}

func (c *Client) CreateIssue(ctx context.Context, source Issue, title, description, stateName string) (*Issue, error) {
	stateID, err := c.stateIDForIssue(ctx, source, stateName)
	if err != nil {
		return nil, err
	}
	input := map[string]any{
		"title":       title,
		"description": description,
		"teamId":      source.TeamID,
		"projectId":   source.ProjectID,
	}
	if stateID != "" {
		input["stateId"] = stateID
	}
	payload, err := c.graphql(ctx, issueCreateMutation, map[string]any{"input": input})
	if err != nil {
		return nil, err
	}
	issueNode, ok := digMap(payload, "data", "issueCreate", "issue")
	if !ok {
		return nil, fmt.Errorf("linear_unknown_payload")
	}
	issue, ok := normalizeIssue(issueNode)
	if !ok {
		return nil, fmt.Errorf("linear_unknown_payload")
	}
	return &issue, nil
}

func (c *Client) UpdateIssueState(ctx context.Context, issue Issue, stateName string) error {
	stateID, err := c.stateIDForIssue(ctx, issue, stateName)
	if err != nil {
		return err
	}
	if stateID == "" {
		return fmt.Errorf("linear_state_not_found")
	}
	_, err = c.graphql(ctx, issueUpdateMutation, map[string]any{
		"id": issue.ID,
		"input": map[string]any{
			"stateId": stateID,
		},
	})
	return err
}

func (c *Client) UpdateIssueDescription(ctx context.Context, issueID, description string) error {
	if strings.TrimSpace(issueID) == "" {
		return fmt.Errorf("missing_issue_id")
	}
	_, err := c.graphql(ctx, issueUpdateMutation, map[string]any{
		"id": issueID,
		"input": map[string]any{
			"description": description,
		},
	})
	return err
}

func (c *Client) CreateComment(ctx context.Context, issueID, body string) error {
	if strings.TrimSpace(issueID) == "" || strings.TrimSpace(body) == "" {
		return nil
	}
	_, err := c.graphql(ctx, commentCreateMutation, map[string]any{
		"input": map[string]any{
			"issueId": issueID,
			"body":    body,
		},
	})
	return err
}

func (c *Client) GraphQL(ctx context.Context, query string, variables map[string]any) (map[string]any, error) {
	return c.graphql(ctx, query, variables)
}

func (c *Client) graphql(ctx context.Context, query string, variables map[string]any) (map[string]any, error) {
	body, _ := json.Marshal(map[string]any{"query": query, "variables": variables})
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		payload, retry, err := c.graphqlAttempt(ctx, body)
		if err == nil {
			return payload, nil
		}
		lastErr = err
		if !retry || attempt == 3 || ctx.Err() != nil {
			break
		}
		timer := time.NewTimer(time.Duration(attempt+1) * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, fmt.Errorf("linear_api_request: %w", ctx.Err())
		case <-timer.C:
		}
	}
	return nil, lastErr
}

func (c *Client) graphqlAttempt(ctx context.Context, body []byte) (map[string]any, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.Tracker.Endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, false, fmt.Errorf("linear_api_request: %w", err)
	}
	req.Header.Set("Authorization", c.cfg.Tracker.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, transientRequestError(err), fmt.Errorf("linear_api_request: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		retry := resp.StatusCode == http.StatusRequestTimeout ||
			resp.StatusCode == http.StatusTooManyRequests ||
			resp.StatusCode == http.StatusBadGateway ||
			resp.StatusCode == http.StatusServiceUnavailable ||
			resp.StatusCode == http.StatusGatewayTimeout
		return nil, retry, fmt.Errorf("linear_api_status: %d", resp.StatusCode)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, false, fmt.Errorf("linear_unknown_payload: %w", err)
	}
	if errs, ok := payload["errors"].([]any); ok && len(errs) > 0 {
		return nil, false, fmt.Errorf("linear_graphql_errors")
	}
	return payload, false, nil
}

func transientRequestError(err error) bool {
	if errors.Is(err, io.EOF) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && (netErr.Timeout() || netErr.Temporary()) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "connection reset by peer") ||
		strings.Contains(msg, "client.timeout exceeded") ||
		strings.Contains(msg, "server sent goaway") ||
		strings.Contains(msg, "tls handshake timeout")
}

func normalizeIssue(node map[string]any) (Issue, bool) {
	id, _ := node["id"].(string)
	identifier, _ := node["identifier"].(string)
	title, _ := node["title"].(string)
	stateNode, _ := node["state"].(map[string]any)
	state, _ := stateNode["name"].(string)
	if id == "" || identifier == "" || title == "" || state == "" {
		return Issue{}, false
	}
	issue := Issue{
		ID:          id,
		Identifier:  identifier,
		Title:       title,
		Description: stringOrEmpty(node["description"]),
		State:       state,
		StateID:     stringOrEmpty(stateNode["id"]),
		BranchName:  stringOrEmpty(node["branchName"]),
		URL:         stringOrEmpty(node["url"]),
		Labels:      normalizeLabels(node),
		BlockedBy:   normalizeBlockedBy(node),
		ProjectID:   nestedID(node, "project"),
		TeamID:      nestedID(node, "team"),
		CreatedAt:   parseTime(node["createdAt"]),
		UpdatedAt:   parseTime(node["updatedAt"]),
	}
	if p, ok := node["priority"].(float64); ok {
		priority := int(p)
		issue.Priority = &priority
	}
	return issue, true
}

func normalizeLabels(node map[string]any) []string {
	labelNode, _ := digMap(node, "labels")
	names := []string{}
	for _, entry := range sliceMap(labelNode["nodes"]) {
		if name := strings.ToLower(strings.TrimSpace(stringOrEmpty(entry["name"]))); name != "" {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

func normalizeBlockedBy(node map[string]any) []BlockerRef {
	relationsNode, _ := digMap(node, "relations")
	out := []BlockerRef{}
	for _, rel := range sliceMap(relationsNode["nodes"]) {
		if typ := stringOrEmpty(rel["type"]); typ != "blocks" {
			continue
		}
		related, _ := rel["relatedIssue"].(map[string]any)
		stateNode, _ := related["state"].(map[string]any)
		out = append(out, BlockerRef{
			ID:         stringOrEmpty(related["id"]),
			Identifier: stringOrEmpty(related["identifier"]),
			State:      stringOrEmpty(stateNode["name"]),
		})
	}
	return out
}

func sortIssues(in []Issue) []Issue {
	slices.SortStableFunc(in, func(a, b Issue) int {
		ap, bp := 999, 999
		if a.Priority != nil {
			ap = *a.Priority
		}
		if b.Priority != nil {
			bp = *b.Priority
		}
		if ap != bp {
			return ap - bp
		}
		if a.CreatedAt != nil && b.CreatedAt != nil && !a.CreatedAt.Equal(*b.CreatedAt) {
			if a.CreatedAt.Before(*b.CreatedAt) {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Identifier, b.Identifier)
	})
	return in
}

func digMap(m map[string]any, keys ...string) (map[string]any, bool) {
	current := m
	for i, key := range keys {
		v, ok := current[key]
		if !ok {
			return nil, false
		}
		if i == len(keys)-1 {
			out, ok := v.(map[string]any)
			return out, ok
		}
		next, ok := v.(map[string]any)
		if !ok {
			return nil, false
		}
		current = next
	}
	return nil, false
}

func sliceMap(v any) []map[string]any {
	raw, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func stringOrEmpty(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

func parseTime(v any) *time.Time {
	s, _ := v.(string)
	if s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil
	}
	return &t
}

func nestedID(node map[string]any, key string) string {
	child, _ := node[key].(map[string]any)
	return stringOrEmpty(child["id"])
}

func (c *Client) stateIDForIssue(ctx context.Context, issue Issue, stateName string) (string, error) {
	if strings.TrimSpace(issue.ID) == "" || strings.TrimSpace(stateName) == "" {
		return "", nil
	}
	payload, err := c.graphql(ctx, workflowStatesForIssueQuery, map[string]any{"id": issue.ID})
	if err != nil {
		return "", err
	}
	stateNodes := []map[string]any{}
	if issueNode, ok := digMap(payload, "data", "issue"); ok {
		if teamNode, ok := digMap(issueNode, "team"); ok {
			if statesNode, ok := digMap(teamNode, "states"); ok {
				stateNodes = sliceMap(statesNode["nodes"])
			}
		}
	}
	target := strings.ToLower(strings.TrimSpace(stateName))
	for _, node := range stateNodes {
		if strings.ToLower(strings.TrimSpace(stringOrEmpty(node["name"]))) == target {
			return stringOrEmpty(node["id"]), nil
		}
	}
	return "", nil
}

const candidateIssuesQuery = `
query CandidateIssues($projectSlug: String!, $stateNames: [String!], $first: Int!, $after: String) {
  issues(
    filter: { project: { slugId: { eq: $projectSlug } }, state: { name: { in: $stateNames } } }
    first: $first
    after: $after
  ) {
    nodes {
      id identifier title description priority branchName url createdAt updatedAt
      state { id name }
      project { id }
      team { id }
      labels { nodes { name } }
      relations { nodes { type relatedIssue { id identifier state { name } } } }
    }
    pageInfo { hasNextPage endCursor }
  }
}`

const issuesByStatesQuery = `
query IssuesByStates($projectSlug: String!, $stateNames: [String!], $first: Int!) {
  issues(
    filter: { project: { slugId: { eq: $projectSlug } }, state: { name: { in: $stateNames } } }
    first: $first
  ) {
    nodes {
      id identifier title description priority branchName url createdAt updatedAt
      state { id name }
      project { id }
      team { id }
      labels { nodes { name } }
      relations { nodes { type relatedIssue { id identifier state { name } } } }
    }
  }
}`

const issueStatesByIDsQuery = `
query IssueStatesByIds($ids: [ID!]!) {
  issues(filter: { id: { in: $ids } }, first: 250) {
    nodes {
      id identifier title description priority branchName url createdAt updatedAt
      state { id name }
      project { id }
      team { id }
      labels { nodes { name } }
      relations { nodes { type relatedIssue { id identifier state { name } } } }
    }
  }
}`

const issueByIdentifierQuery = `
query IssueByIdentifier($identifier: String!) {
  issues(filter: { identifier: { eq: $identifier } }, first: 1) {
    nodes {
      id identifier title description priority branchName url createdAt updatedAt
      state { id name }
      project { id }
      team { id }
      labels { nodes { name } }
      relations { nodes { type relatedIssue { id identifier state { name } } } }
    }
  }
}`

const issueByTitleQuery = `
query IssueByTitle($projectSlug: String!, $title: String!) {
  issues(filter: { project: { slugId: { eq: $projectSlug } }, title: { eq: $title } }, first: 1) {
    nodes {
      id identifier title description priority branchName url createdAt updatedAt
      state { id name }
      project { id }
      team { id }
      labels { nodes { name } }
      relations { nodes { type relatedIssue { id identifier state { name } } } }
    }
  }
}`

const workflowStatesForIssueQuery = `
query WorkflowStatesForIssue($id: String!) {
  issue(id: $id) {
    id
    team {
      id
      states {
        nodes {
          id
          name
        }
      }
    }
  }
}`

const issueCreateMutation = `
mutation IssueCreate($input: IssueCreateInput!) {
  issueCreate(input: $input) {
    success
    issue {
      id identifier title description priority branchName url createdAt updatedAt
      state { id name }
      project { id }
      team { id }
      labels { nodes { name } }
      relations { nodes { type relatedIssue { id identifier state { name } } } }
    }
  }
}`

const issueUpdateMutation = `
mutation IssueUpdate($id: String!, $input: IssueUpdateInput!) {
  issueUpdate(id: $id, input: $input) {
    success
  }
}`

const commentCreateMutation = `
mutation CommentCreate($input: CommentCreateInput!) {
  commentCreate(input: $input) {
    success
  }
}`
