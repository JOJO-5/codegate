package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/jojo/codegate/internal/protocol"
)

func gitMutation(ctx context.Context, ws *Workspace, req protocol.GitPayload) (protocol.GitResult, error) {
	out := protocol.GitResult{Files: []protocol.GitFile{}}
	invalid := func(message string) (protocol.GitResult, error) {
		return out, fmt.Errorf("%w: %s", protocol.ErrInvalidPayload, message)
	}
	if !req.Confirm || req.ExpectedHead == "" {
		return invalid("请刷新仓库并明确确认操作")
	}
	root, err := gitRoot(ctx, ws, req.Path)
	if err != nil {
		return out, err
	}
	head, _, err := runGit(ctx, root, "rev-parse", "--verify", "HEAD")
	if err != nil || strings.TrimSpace(head) != req.ExpectedHead {
		return invalid("分支提交已变化，请刷新后重试")
	}
	branch, _, err := runGit(ctx, root, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil || strings.TrimSpace(branch) == "" {
		return invalid("分离 HEAD 状态不能提交或推送，请先切换分支")
	}
	if req.Action == "commit" {
		message := strings.TrimSpace(req.Message)
		if message == "" || len(message) > 4096 || strings.ContainsRune(message, 0) {
			return invalid("请填写有效提交说明（最多 4096 字节）")
		}
		files, truncated, err := gitFiles(ctx, root)
		if err != nil {
			return out, err
		}
		if truncated {
			return invalid("改动清单过大，请在终端审查并提交")
		}
		if len(files) == 0 {
			return invalid("没有待提交改动")
		}
		for _, f := range files {
			if strings.ContainsAny(f.Status, "U") || f.Status == "AA" || f.Status == "DD" {
				return invalid("请先在终端解决合并冲突")
			}
			if _, err := ws.ResolveIn(root, f.Path); err != nil {
				return out, err
			}
		}
		// A real configured identity is required; never invent one for the user.
		if _, _, err := runGit(ctx, root, "var", "GIT_AUTHOR_IDENT"); err != nil {
			return invalid("请先在目标电脑配置 Git user.name 和 user.email")
		}
		_, truncated, err = runGit(ctx, root, "add", "--all")
		if err != nil || truncated {
			return invalid("暂存未完成，请刷新并在终端检查")
		}
		_, truncated, err = runGit(ctx, root, "commit", "-m", message)
		if err != nil || truncated {
			return invalid("提交未确认成功，请刷新并检查 Git 身份、钩子或冲突；已暂存文件会保留")
		}
		out, err = gitReview(ctx, ws, protocol.GitPayload{Path: root, Action: "status"})
		if err == nil {
			out.Message = "已提交全部改动"
		}
		return out, err
	}
	if req.Action == "push" {
		// Respect the exact configured upstream rather than push.default=matching,
		// which could otherwise push unrelated branches. Never force-push.
		branch = strings.TrimSpace(branch)
		remote, _, err := runGit(ctx, root, "config", "--get", "branch."+branch+".remote")
		if err != nil {
			return invalid("当前分支未配置上游，请先在终端使用 git push -u 建立上游")
		}
		target, _, err := runGit(ctx, root, "config", "--get", "branch."+branch+".merge")
		remote = strings.TrimSpace(remote)
		target = strings.TrimSpace(target)
		if err != nil || remote == "" || strings.HasPrefix(remote, "-") || !strings.HasPrefix(target, "refs/heads/") {
			return invalid("当前分支上游配置无效，请在终端检查")
		}
		// Ensure the selected remote is an existing config name, not an injected URL.
		remotes, _, err := runGit(ctx, root, "remote")
		found := false
		for _, name := range strings.Split(strings.TrimSpace(remotes), "\n") {
			if name == remote {
				found = true
			}
		}
		if err != nil || !found {
			return invalid("当前分支上游 remote 不存在，请在终端检查")
		}
		_, truncated, err := runGit(ctx, root, "push", "--porcelain", "--no-follow-tags", "--recurse-submodules=no", remote, "HEAD:"+target)
		if err != nil || truncated {
			return invalid("推送未确认成功，请检查上游、网络和 Git 凭据；不会强制推送，请在终端核对远端状态")
		}
		out, err = gitReview(ctx, ws, protocol.GitPayload{Path: root, Action: "status"})
		if err == nil {
			out.Message = "已推送当前分支到配置的上游"
		}
		return out, err
	}
	return invalid("不支持的 Git 操作")
}
