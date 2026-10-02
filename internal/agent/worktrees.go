package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jojo/codegate/internal/protocol"
)

// Git creates each worktree in a uniquely reserved task directory within the
// authorized repository. Keeping it under the repo also works when allowed_roots
// contains exactly that repo. Never delete a successful task on session exit.
func createWorktree(ctx context.Context, ws *Workspace, path string) (string, func(), error) {
	root, err := gitRoot(ctx, ws, path)
	if err != nil {
		return "", nil, err
	}
	if _, _, err = runGit(ctx, root, "rev-parse", "--verify", "HEAD"); err != nil {
		return "", nil, fmt.Errorf("%w: 仓库须有初始提交才能创建独立工作区", protocol.ErrInvalidPayload)
	}
	taskID := uuid.NewString()
	parent, err := ws.ResolveIn(root, ".codegate-worktrees")
	if err != nil {
		return "", nil, err
	}
	if err = os.MkdirAll(parent, 0700); err != nil {
		return "", nil, err
	}
	parent, err = ws.Resolve(parent)
	if err != nil {
		return "", nil, err
	}
	// A symlink even to another allowed directory is not a valid managed parent.
	if info, err := os.Lstat(filepath.Join(root, ".codegate-worktrees")); err != nil || !info.IsDir() {
		return "", nil, fmt.Errorf("%w: 独立工作区目录必须是本仓库的普通目录", protocol.ErrInvalidPayload)
	}
	target, err := ws.ResolveIn(parent, taskID)
	if err != nil {
		return "", nil, err
	}
	branch := "codegate/task-" + taskID
	// Exclude only our reserved container locally, without changing tracked .gitignore.
	common, _, err := runGit(ctx, root, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", nil, err
	}
	// Trim Git's final newline before composing a path.
	commonPath := filepath.Join(root, trimGitPath(common))
	if filepath.IsAbs(trimGitPath(common)) {
		commonPath = trimGitPath(common)
	}
	exclude, err := ws.Resolve(filepath.Join(commonPath, "info", "exclude"))
	if err != nil {
		return "", nil, err
	}
	if err = os.MkdirAll(filepath.Dir(exclude), 0700); err != nil {
		return "", nil, err
	}
	if err = ensureWorktreeExclude(exclude); err != nil {
		return "", nil, err
	}
	rollback := func() {
		cleanCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		_, _, _ = runGit(cleanCtx, root, "worktree", "remove", target)
		// No force removal: preserve files if a failed CLI launch left user changes.
		if _, err := os.Stat(target); os.IsNotExist(err) {
			_, _, _ = runGit(cleanCtx, root, "branch", "-d", branch)
		}
	}
	_, _, err = runGit(ctx, root, "worktree", "add", "-b", branch, target, "HEAD")
	if err != nil {
		rollback()
		return "", nil, err
	}
	if _, err = ws.Resolve(target); err != nil {
		rollback()
		return "", nil, err
	}
	return target, rollback, nil
}

var worktreeExcludeMu sync.Mutex

func trimGitPath(s string) string { return strings.TrimSpace(s) }
func ensureWorktreeExclude(path string) error {
	worktreeExcludeMu.Lock()
	defer worktreeExcludeMu.Unlock()
	content, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, line := range strings.Split(string(content), "\n") {
		if strings.TrimSpace(line) == ".codegate-worktrees/" {
			return nil
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString("\n# CodeGate isolated task directories\n.codegate-worktrees/\n")
	return err
}
