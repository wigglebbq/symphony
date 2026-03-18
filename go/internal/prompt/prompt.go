package prompt

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/openai/symphony/go/internal/config"
	"github.com/openai/symphony/go/internal/linear"
)

var (
	varPattern = regexp.MustCompile(`\{\{\s*([^}]+?)\s*\}\}`)
	ifPattern  = regexp.MustCompile(`(?s)\{%\s*if\s+([^%]+?)\s*%\}(.*?)((\{%\s*else\s*%\}(.*?))?)\{%\s*endif\s*%\}`)
)

func Build(cfg config.Config, issue linear.Issue, attempt *int) (string, error) {
	ctx := map[string]any{
		"issue": issueToMap(issue),
	}
	if attempt != nil {
		ctx["attempt"] = *attempt
	}
	rendered, err := render(cfg.EffectivePromptTemplate(), ctx)
	if err != nil {
		return "", fmt.Errorf("%w: %v", config.ErrTemplateRender, err)
	}
	return strings.TrimSpace(rendered), nil
}

func render(tpl string, ctx map[string]any) (string, error) {
	for {
		matches := ifPattern.FindStringSubmatchIndex(tpl)
		if matches == nil {
			break
		}
		condExpr := tpl[matches[2]:matches[3]]
		ifBody := tpl[matches[4]:matches[5]]
		elseBody := ""
		if matches[10] != -1 {
			elseBody = tpl[matches[10]:matches[11]]
		}
		cond, err := lookup(strings.TrimSpace(condExpr), ctx)
		if err != nil {
			return "", err
		}
		repl := elseBody
		if truthy(cond) {
			repl = ifBody
		}
		tpl = tpl[:matches[0]] + repl + tpl[matches[1]:]
	}
	var err error
	tpl = varPattern.ReplaceAllStringFunc(tpl, func(token string) string {
		if err != nil {
			return ""
		}
		match := varPattern.FindStringSubmatch(token)
		value, lookupErr := lookup(strings.TrimSpace(match[1]), ctx)
		if lookupErr != nil {
			err = lookupErr
			return ""
		}
		switch t := value.(type) {
		case nil:
			return ""
		case string:
			return t
		default:
			raw, marshalErr := json.Marshal(t)
			if marshalErr != nil {
				err = marshalErr
				return ""
			}
			if _, ok := t.([]any); ok {
				return string(raw)
			}
			return string(raw)
		}
	})
	return tpl, err
}

func lookup(expr string, ctx map[string]any) (any, error) {
	if strings.Contains(expr, "|") {
		return nil, fmt.Errorf("unknown filter in %q", expr)
	}
	parts := strings.Split(expr, ".")
	var current any = ctx
	for _, part := range parts {
		part = strings.TrimSpace(part)
		switch t := current.(type) {
		case map[string]any:
			v, ok := t[part]
			if !ok {
				return nil, fmt.Errorf("unknown variable %q", expr)
			}
			current = v
		default:
			return nil, fmt.Errorf("unknown variable %q", expr)
		}
	}
	return current, nil
}

func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(t) != ""
	case bool:
		return t
	default:
		return true
	}
}

func issueToMap(issue linear.Issue) map[string]any {
	out := map[string]any{
		"id":          issue.ID,
		"identifier":  issue.Identifier,
		"title":       issue.Title,
		"description": issue.Description,
		"state":       issue.State,
		"branch_name": issue.BranchName,
		"url":         issue.URL,
		"labels":      issue.Labels,
	}
	if issue.Priority != nil {
		out["priority"] = *issue.Priority
	} else {
		out["priority"] = nil
	}
	blockedBy := make([]map[string]any, 0, len(issue.BlockedBy))
	for _, blocker := range issue.BlockedBy {
		blockedBy = append(blockedBy, map[string]any{
			"id":         blocker.ID,
			"identifier": blocker.Identifier,
			"state":      blocker.State,
		})
	}
	out["blocked_by"] = blockedBy
	if issue.CreatedAt != nil {
		out["created_at"] = issue.CreatedAt.UTC().Format(timeLayout)
	}
	if issue.UpdatedAt != nil {
		out["updated_at"] = issue.UpdatedAt.UTC().Format(timeLayout)
	}
	return out
}

const timeLayout = "2006-01-02T15:04:05Z07:00"
