package agent

import (
	"context"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/jojo/codegate/internal/protocol"
)

var githubPart = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

func githubRepositoryURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "git@github.com:") {
		raw = "ssh://git@github.com/" + strings.TrimPrefix(raw, "git@github.com:")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() != "github.com" || u.Port() != "" || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" {
		return ""
	}
	if u.Scheme != "https" && u.Scheme != "ssh" {
		return ""
	}
	if u.User != nil {
		if u.Scheme != "ssh" || u.User.Username() != "git" {
			return ""
		}
		if _, has := u.User.Password(); has {
			return ""
		}
	}
	path := strings.TrimSuffix(strings.TrimSuffix(u.Path, "/"), ".git")
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) != 2 {
		return ""
	}
	for _, p := range parts {
		if p == "." || p == ".." || !githubPart.MatchString(p) {
			return ""
		}
	}
	return "https://github.com/" + parts[0] + "/" + parts[1]
}

// All comparisons are against locally fetched refs. This never performs an
// implicit fetch or sends repository credentials to the browser.
func gitDelivery(ctx context.Context, root string, out *protocol.GitResult) {
	upstream, cut, err := runGit(ctx, root, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}")
	if err == nil && !cut {
		out.Upstream = strings.TrimSpace(upstream)
		counts, cut, err := runGit(ctx, root, "rev-list", "--left-right", "--count", "HEAD...@{upstream}")
		fields := strings.Fields(counts)
		if err == nil && !cut && len(fields) == 2 {
			a, e1 := strconv.Atoi(fields[0])
			b, e2 := strconv.Atoi(fields[1])
			if e1 == nil && e2 == nil {
				out.Ahead = a
				out.Behind = b
				out.SyncKnown = true
			}
		}
	}
	remote := "origin"
	if out.Branch != "分离 HEAD" {
		if value, cut, err := runGit(ctx, root, "config", "--get", "branch."+out.Branch+".remote"); err == nil && !cut && strings.TrimSpace(value) != "" {
			remote = strings.TrimSpace(value)
		}
	}
	if remote != "." && !strings.HasPrefix(remote, "-") {
		if value, cut, err := runGit(ctx, root, "config", "--get", "remote."+remote+".url"); err == nil && !cut {
			out.RepositoryURL = githubRepositoryURL(value)
		}
	}
	if out.Head == "" {
		return
	}
	raw, cut, err := runGit(ctx, root, "log", "-n", "8", "-z", "--format=%H%x00%s%x00%ct", out.Head, "--")
	if err != nil || cut {
		return
	}
	fields := strings.Split(raw, "\x00")
	for i := 0; i+2 < len(fields); i += 3 {
		stamp, err := strconv.ParseInt(fields[i+2], 10, 64)
		if err != nil {
			break
		}
		subject := fields[i+1]
		if len(subject) > 512 {
			subject = string([]rune(subject)[:min(len([]rune(subject)), 128)])
		}
		out.Commits = append(out.Commits, protocol.GitCommit{Hash: fields[i], Subject: subject, CommittedAt: stamp * 1000})
	}
}
