package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jojo/codegate/internal/processutil"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jojo/codegate/internal/protocol"
)

const gitOutputLimit = 24 * 1024

// Stop commands whose output exceeds the bounded control message size.
type gitOutput struct {
	bytes.Buffer
	limit     int
	truncated bool
}

func (b *gitOutput) Write(p []byte) (int, error) {
	n := len(p)
	if b.Len()+n > b.limit {
		_, _ = b.Buffer.Write(p[:b.limit-b.Len()])
		b.truncated = true
		return n, errors.New("Git output too large")
	}
	return b.Buffer.Write(p)
}

func runGit(ctx context.Context, cwd string, args ...string) (string, bool, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"--no-pager", "-c", "core.quotepath=false", "-c", "core.fsmonitor=false", "-c", "core.untrackedCache=false", "-C", cwd}, args...)...)
	processutil.Background(cmd)
	// Repository selection must never be overridden by the Agent's environment.
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "GIT_") {
			cmd.Env = append(cmd.Env, e)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0", "LC_ALL=C")
	cmd.WaitDelay = time.Second
	out := &gitOutput{limit: gitOutputLimit}
	cmd.Stdout = out
	cmd.Stderr = io.Discard
	err := cmd.Run()
	if out.truncated {
		return out.String(), true, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("%w: Git 操作失败，请检查仓库、Git 安装和本机配置", protocol.ErrInvalidPayload)
	}
	return out.String(), false, nil
}

func gitRoot(ctx context.Context, ws *Workspace, path string) (string, error) {
	path, err := ws.Resolve(path)
	if err != nil {
		return "", err
	}
	raw, _, err := runGit(ctx, path, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	root, err := ws.Resolve(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	// Linked worktrees are supported only when their shared metadata is authorized.
	for _, key := range []string{"--absolute-git-dir", "--git-common-dir"} {
		meta, _, err := runGit(ctx, root, "rev-parse", key)
		if err != nil {
			return "", err
		}
		p := strings.TrimSpace(meta)
		if !filepath.IsAbs(p) {
			p = filepath.Join(root, p)
		}
		if _, err := ws.Resolve(p); err != nil {
			return "", err
		}
	}
	return root, nil
}

func gitFiles(ctx context.Context, root string) ([]protocol.GitFile, bool, error) {
	raw, truncated, err := runGit(ctx, root, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return nil, false, err
	}
	parts := strings.Split(raw, "\x00")
	files := []protocol.GitFile{}
	for i := 0; i < len(parts)-1; i++ {
		entry := parts[i]
		if len(entry) < 4 {
			continue
		}
		f := protocol.GitFile{Status: entry[:2], Path: entry[3:]}
		if strings.ContainsAny(f.Status, "RC") {
			if i+1 >= len(parts)-1 {
				truncated = true
				break
			}
			i++
			f.Original = parts[i]
		}
		files = append(files, f)
		if len(files) >= 200 {
			truncated = true
			break
		}
	}
	return files, truncated, nil
}

func gitReview(ctx context.Context, ws *Workspace, req protocol.GitPayload) (protocol.GitResult, error) {
	out := protocol.GitResult{Files: []protocol.GitFile{}}
	if req.Action == "commit" || req.Action == "push" {
		return gitMutation(ctx, ws, req)
	}
	if req.Action != "status" && req.Action != "diff" {
		return out, fmt.Errorf("%w: 不支持的 Git 操作", protocol.ErrInvalidPayload)
	}
	root, err := gitRoot(ctx, ws, req.Path)
	if err != nil {
		return out, err
	}
	out.Root = root
	head, _, _ := runGit(ctx, root, "rev-parse", "--verify", "HEAD")
	out.Head = strings.TrimSpace(head)
	branch, _, err := runGit(ctx, root, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		branch = "分离 HEAD"
	}
	out.Branch = strings.TrimSpace(branch)
	out.Files, out.Truncated, err = gitFiles(ctx, root)
	if err == nil && req.Action == "status" {
		gitDelivery(ctx, root, &out)
	}
	if err != nil || req.Action == "status" {
		return out, err
	}
	if req.File == "" {
		return out, fmt.Errorf("%w: 请选择文件", protocol.ErrInvalidPayload)
	}
	// Only exact paths returned by Git status may be selected.
	var selected *protocol.GitFile
	for i := range out.Files {
		if out.Files[i].Path == req.File {
			selected = &out.Files[i]
			break
		}
	}
	if selected == nil {
		return out, fmt.Errorf("%w: 文件已变化，请刷新清单", protocol.ErrInvalidPayload)
	}
	resolved, err := ws.ResolveIn(root, req.File)
	if err != nil {
		return out, err
	}
	if selected.Status == "??" {
		info, err := os.Lstat(filepath.Join(root, req.File))
		if err != nil {
			return out, err
		}
		if !info.Mode().IsRegular() {
			out.Diff = "该文件不是普通文本文件，无法预览"
			return out, nil
		}
		f, err := os.Open(resolved)
		if err != nil {
			return out, err
		}
		defer f.Close()
		data, err := io.ReadAll(io.LimitReader(f, 12*1024+1))
		if err != nil {
			return out, err
		}
		out.Truncated = len(data) > 12*1024
		if out.Truncated {
			data = data[:12*1024]
		}
		if bytes.IndexByte(data, 0) >= 0 {
			out.Diff = "二进制文件，无法预览"
		} else {
			out.Diff = "新文件：" + req.File + "\n" + string(data)
		}
		return out, nil
	}
	args := []string{"diff", "--no-ext-diff", "--no-textconv", "--no-color"}
	if req.Staged {
		args = append(args, "--cached")
	}
	// Literal pathspecs prevent filenames such as :(glob)* from selecting other files.
	args = append(args, "--", ":(literal)"+req.File)
	if selected.Original != "" {
		args = append(args, ":(literal)"+selected.Original)
	}
	out.Diff, out.Truncated, err = runGit(ctx, root, args...)
	// Leave enough room for JSON escaping and file metadata in the 64 KiB envelope.
	if len(out.Diff) > 12*1024 {
		out.Diff = out.Diff[:12*1024]
		out.Truncated = true
	}
	return out, err
}

func (a *Agent) onGitRequest(env *protocol.Envelope) {
	if !a.gitMu.TryLock() {
		a.replyError(env, fmt.Errorf("%w: Git 正在处理其他请求，请稍后重试", protocol.ErrInvalidPayload))
		return
	}
	defer a.gitMu.Unlock()
	finish, activityErr := a.beginActivity()
	if activityErr != nil {
		a.replyError(env, activityErr)
		return
	}
	defer finish()
	req, err := protocol.DecodePayload[protocol.GitPayload](env)
	if err != nil {
		a.replyError(env, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var out protocol.GitResult
	if req.Action == "worktrees" || req.Action == "worktree_remove" {
		a.workspaceMu.Lock()
		out, err = managedWorktrees(ctx, a.ws, req, a.mgr.Snapshot())
		a.workspaceMu.Unlock()
		if err == nil && req.Action == "worktree_remove" {
			select {
			case a.repositoryRefresh <- struct{}{}:
			default:
			}
		}
	} else {
		out, err = gitReview(ctx, a.ws, req)
	}
	if err != nil {
		a.replyError(env, err)
		return
	}
	for {
		encoded, _ := json.Marshal(out)
		if len(encoded) <= 48*1024 {
			break
		}
		out.Truncated = true
		if len(out.Diff) > 1024 {
			out.Diff = out.Diff[:len(out.Diff)/2]
		} else if len(out.Files) > 0 {
			out.Files = out.Files[:len(out.Files)-1]
		} else if len(out.Worktrees) > 0 {
			out.Worktrees = out.Worktrees[:len(out.Worktrees)-1]
		} else if len(out.Commits) > 0 {
			out.Commits = out.Commits[:len(out.Commits)-1]
		} else {
			break
		}
	}
	a.reply(env, protocol.TypeGitResult, out)
}
