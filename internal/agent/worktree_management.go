package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jojo/codegate/internal/protocol"
)

// Only UUID directories and matching CodeGate branches registered with Git are
// managed. Git removes the worktree without --force; branches are retained.
func managedWorktrees(ctx context.Context, ws *Workspace, req protocol.GitPayload, sessions []protocol.SessionSummary) (protocol.GitResult, error) {
	out := protocol.GitResult{Files: []protocol.GitFile{}, Worktrees: []protocol.WorktreeInfo{}}
	invalid := func(message string) (protocol.GitResult, error) {
		return out, fmt.Errorf("%w: %s", protocol.ErrInvalidPayload, message)
	}
	root, err := gitRoot(ctx, ws, req.Path)
	if err != nil {
		return out, err
	}
	common, cut, err := runGit(ctx, root, "rev-parse", "--git-common-dir")
	if err != nil || cut {
		return invalid("无法读取工作区元数据")
	}
	common = strings.TrimSpace(common)
	if !filepath.IsAbs(common) {
		common = filepath.Join(root, common)
	}
	common, err = ws.Resolve(common)
	if err != nil {
		return out, err
	}
	if filepath.Base(common) != ".git" {
		return invalid("仅支持普通 Git 仓库创建的独立工作区")
	}
	primary, err := gitRoot(ctx, ws, filepath.Dir(common))
	if err != nil {
		return out, err
	}
	out.Root = primary
	parent := filepath.Join(primary, ".codegate-worktrees")
	if info, e := os.Lstat(parent); e == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		return invalid("工作区容器必须为本仓库普通目录")
	}
	raw, cut, err := runGit(ctx, primary, "worktree", "list", "--porcelain", "-z")
	if err != nil || cut {
		return invalid("工作区列表过大或读取失败，请在终端查看")
	}
	primaryHead, _, err := runGit(ctx, primary, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return out, err
	}
	primaryHead = strings.TrimSpace(primaryHead)
	var item protocol.WorktreeInfo
	blocked := ""
	appendItem := func() error {
		if item.Path == "" {
			return nil
		}
		rel, e := filepath.Rel(parent, item.Path)
		if e != nil || strings.ContainsAny(rel, "/\\") {
			return nil
		}
		id, e := uuid.Parse(rel)
		if e != nil || id.String() != rel || item.Branch != "codegate/task-"+rel {
			return nil
		}
		if len(out.Worktrees) >= 40 {
			out.Truncated = true
			return nil
		}
		resolved, e := ws.Resolve(item.Path)
		if e != nil || resolved != filepath.Clean(item.Path) {
			item.BlockedReason = "目录不可访问或存在符号链接"
		} else if info, e := os.Lstat(item.Path); e != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			item.BlockedReason = "目录不是普通目录"
		}
		if item.BlockedReason == "" {
			// Include ignored files: dependencies and other local-only data must never
			// disappear merely because git status normally hides them.
			status, cut, e := runGit(ctx, item.Path, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignored")
			if e != nil || cut {
				item.BlockedReason = "改动状态不完整，无法安全清理"
			} else {
				item.Changed = status != ""
			}
			commits, cut, e := runGit(ctx, item.Path, "rev-list", "--count", item.Head, "--not", primaryHead)
			if e != nil || cut {
				item.BlockedReason = "提交状态不完整，无法安全清理"
			} else {
				item.UniqueCommits, e = strconv.Atoi(strings.TrimSpace(commits))
				if e != nil {
					item.BlockedReason = "提交状态无效"
				}
			}
		}
		for _, s := range sessions {
			if s.Status != "running" && s.Status != "starting" && s.Status != "detached" {
				continue
			}
			cwd, e := ws.Resolve(s.Cwd)
			if e != nil {
				continue
			}
			rel, e := filepath.Rel(item.Path, cwd)
			if e == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				item.Occupied = true
			}
		}
		if item.BlockedReason == "" {
			switch {
			case blocked != "":
				item.BlockedReason = blocked
			case item.Occupied:
				item.BlockedReason = "有会话正在使用此目录"
			case item.Changed:
				item.BlockedReason = "有未提交、未跟踪或忽略的文件"
			case item.UniqueCommits > 0:
				item.BlockedReason = "存在尚未合入主工作区的提交"
			}
		}
		out.Worktrees = append(out.Worktrees, item)
		return nil
	}
	for _, field := range strings.Split(raw, "\x00") {
		if field == "" {
			if err := appendItem(); err != nil {
				return out, err
			}
			item = protocol.WorktreeInfo{}
			blocked = ""
			continue
		}
		switch {
		case strings.HasPrefix(field, "worktree "):
			item.Path = strings.TrimPrefix(field, "worktree ")
		case strings.HasPrefix(field, "HEAD "):
			item.Head = strings.TrimPrefix(field, "HEAD ")
		case strings.HasPrefix(field, "branch refs/heads/"):
			item.Branch = strings.TrimPrefix(field, "branch refs/heads/")
		case strings.HasPrefix(field, "locked"):
			blocked = "Git 已锁定此工作区"
		case strings.HasPrefix(field, "prunable"):
			blocked = "Git 工作区状态异常，请在终端处理"
		}
	}
	if req.Action == "worktrees" {
		return out, nil
	}
	if req.Action != "worktree_remove" || !req.Confirm || req.ExpectedHead == "" {
		return invalid("请刷新工作区状态并明确确认清理")
	}
	var target *protocol.WorktreeInfo
	for i := range out.Worktrees {
		if out.Worktrees[i].Path == req.Target {
			target = &out.Worktrees[i]
			break
		}
	}
	if target == nil {
		return invalid("目标不属于本仓库的可管理工作区")
	}
	if target.BlockedReason != "" {
		return invalid(target.BlockedReason)
	}
	if target.Head != req.ExpectedHead {
		return invalid("提交已变化，请刷新后重试")
	}
	_, cut, err = runGit(ctx, primary, "worktree", "remove", "--", target.Path)
	if err != nil || cut {
		return invalid("Git 未确认清理成功；目录和分支状态请在终端检查")
	}
	out.Message = "已清理工作区目录，Git 分支保留"
	out.Worktrees = nil
	return out, nil
}
