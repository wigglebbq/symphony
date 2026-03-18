package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/openai/symphony/go/internal/config"
	"github.com/openai/symphony/go/internal/linear"
	sshclient "github.com/openai/symphony/go/internal/ssh"
)

var sanitizePattern = regexp.MustCompile(`[^A-Za-z0-9._-]`)

type Workspace struct {
	Path       string
	Key        string
	CreatedNow bool
	WorkerHost string
}

type Manager struct {
	cfg config.Config
}

func NewManager(cfg config.Config) *Manager {
	return &Manager{cfg: cfg}
}

func (m *Manager) Ensure(ctx context.Context, issue linear.Issue, workerHost string) (Workspace, error) {
	key := sanitizePattern.ReplaceAllString(issue.Identifier, "_")
	path, err := m.workspacePath(key, workerHost)
	if err != nil {
		return Workspace{}, err
	}
	var created bool
	if workerHost == "" {
		root, err := filepath.Abs(m.cfg.Workspace.Root)
		if err != nil {
			return Workspace{}, err
		}
		if err := ensureWithinRoot(root, path); err != nil {
			return Workspace{}, err
		}
		if err := os.MkdirAll(root, 0o755); err != nil {
			return Workspace{}, err
		}
		_, statErr := os.Stat(path)
		created = errors.Is(statErr, os.ErrNotExist)
		if err := os.MkdirAll(path, 0o755); err != nil {
			return Workspace{}, err
		}
	} else {
		created, err = m.ensureRemoteWorkspace(ctx, path, workerHost)
		if err != nil {
			return Workspace{}, err
		}
	}
	ws := Workspace{Path: path, Key: key, CreatedNow: created, WorkerHost: workerHost}
	if created && strings.TrimSpace(m.cfg.Hooks.AfterCreate) != "" {
		if err := m.runHook(ctx, path, workerHost, m.cfg.Hooks.AfterCreate); err != nil {
			return Workspace{}, fmt.Errorf("after_create failed: %w", err)
		}
	}
	return ws, nil
}

func (m *Manager) RunBeforeRun(ctx context.Context, ws Workspace) error {
	if strings.TrimSpace(m.cfg.Hooks.BeforeRun) == "" {
		return nil
	}
	return m.runHook(ctx, ws.Path, ws.WorkerHost, m.cfg.Hooks.BeforeRun)
}

func (m *Manager) RunAfterRun(ctx context.Context, ws Workspace) error {
	if strings.TrimSpace(m.cfg.Hooks.AfterRun) == "" {
		return nil
	}
	return m.runHook(ctx, ws.Path, ws.WorkerHost, m.cfg.Hooks.AfterRun)
}

func (m *Manager) Remove(ctx context.Context, identifier string, workerHost string) error {
	key := sanitizePattern.ReplaceAllString(identifier, "_")
	path, err := m.workspacePath(key, workerHost)
	if err != nil {
		return err
	}
	if strings.TrimSpace(m.cfg.Hooks.BeforeRemove) != "" {
		_ = m.runHook(ctx, path, workerHost, m.cfg.Hooks.BeforeRemove)
	}
	if workerHost != "" {
		cmd := sshclient.CommandContext(ctx, workerHost, "rm -rf "+shellQuote(path))
		return cmd.Run()
	}
	root, err := filepath.Abs(m.cfg.Workspace.Root)
	if err != nil {
		return err
	}
	if err := ensureWithinRoot(root, path); err != nil {
		return err
	}
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	return os.RemoveAll(path)
}

func (m *Manager) Path(identifier string, workerHost string) (string, error) {
	key := sanitizePattern.ReplaceAllString(identifier, "_")
	return m.workspacePath(key, workerHost)
}

func (m *Manager) workspacePath(key, workerHost string) (string, error) {
	if workerHost != "" {
		return filepath.Join(m.cfg.Workspace.Root, key), nil
	}
	root, err := filepath.Abs(m.cfg.Workspace.Root)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, key), nil
}

func ensureWithinRoot(root, path string) error {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return err
	}
	if rel == "." || (!strings.HasPrefix(rel, "..") && rel != "..") {
		if path == root {
			return fmt.Errorf("workspace path must not equal workspace root")
		}
		return nil
	}
	return fmt.Errorf("workspace outside root")
}

func (m *Manager) ensureRemoteWorkspace(ctx context.Context, path, workerHost string) (bool, error) {
	timeoutCtx, cancel := context.WithTimeout(ctx, m.cfg.Hooks.Timeout)
	defer cancel()
	script := "if [ -d " + shellQuote(path) + " ]; then echo 0; else mkdir -p " + shellQuote(path) + " && echo 1; fi"
	cmd := sshclient.CommandContext(timeoutCtx, workerHost, script)
	out, err := cmd.Output()
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) == "1", nil
}

func (m *Manager) runHook(ctx context.Context, cwd, workerHost, script string) error {
	timeoutCtx, cancel := context.WithTimeout(ctx, m.cfg.Hooks.Timeout)
	defer cancel()
	var cmd *exec.Cmd
	if workerHost == "" {
		cmd = exec.CommandContext(timeoutCtx, "bash", "-lc", script)
		cmd.Dir = cwd
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
	} else {
		cmd = sshclient.CommandContext(timeoutCtx, workerHost, "cd "+shellQuote(cwd)+" && "+script)
		cmd.Stdout = os.Stderr
	}
	return cmd.Run()
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}
