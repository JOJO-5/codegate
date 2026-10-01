package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jojo/codegate/internal/protocol"
)

// Scan only directory trees authorized on this Agent. Git metadata and symlink
// directories are never traversed; a .git file also identifies a worktree/submodule.
func scanRepositories(ctx context.Context, workspace *Workspace) protocol.RepositoryListResult {
	out := protocol.RepositoryListResult{State: "ready", Repositories: []protocol.Repository{}, ScannedAt: time.Now().UnixMilli()}
	visited := map[string]bool{}
	found := map[string]bool{}
	warn := func(path string, err error) {
		out.Partial = true
		out.ErrorCount++
		if len(out.Warnings) < 20 {
			message := []rune(path + ": " + err.Error())
			if len(message) > 256 {
				message = append(message[:255], '…')
			}
			out.Warnings = append(out.Warnings, string(message))
		}
	}
	for _, root := range workspace.Roots() {
		if ctx.Err() != nil {
			warn(root, ctx.Err())
			break
		}
		resolved, err := workspace.Resolve(root)
		if err != nil {
			warn(root, err)
			continue
		}
		err = filepath.WalkDir(resolved, func(path string, entry fs.DirEntry, walkErr error) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if walkErr != nil {
				warn(path, walkErr)
				if entry != nil && entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if !entry.IsDir() {
				return nil
			}
			if entry.Name() == ".git" {
				return filepath.SkipDir
			}
			key := normCase(path)
			if visited[key] {
				return filepath.SkipDir
			}
			visited[key] = true
			real, err := workspace.Resolve(path)
			if err != nil {
				warn(path, err)
				return filepath.SkipDir
			}
			if !pathEqual(real, path) {
				return filepath.SkipDir
			}
			marker := filepath.Join(path, ".git")
			info, err := os.Lstat(marker)
			if err != nil {
				if !errors.Is(err, os.ErrNotExist) {
					warn(marker, err)
				}
				return nil
			}
			isRepository := info.IsDir()
			if info.Mode().IsRegular() && info.Size() <= 4096 {
				data, readErr := os.ReadFile(marker)
				if readErr != nil {
					warn(marker, readErr)
				} else {
					isRepository = strings.HasPrefix(strings.TrimSpace(string(data)), "gitdir: ")
				}
			}
			if isRepository && !found[key] {
				found[key] = true
				out.Repositories = append(out.Repositories, protocol.Repository{Name: filepath.Base(path), Path: path, Root: root})
			}
			return nil
		})
		if err != nil {
			warn(root, err)
		}
	}
	sort.Slice(out.Repositories, func(i, j int) bool { return out.Repositories[i].Path < out.Repositories[j].Path })
	out.Total = len(out.Repositories)
	out.ScannedAt = time.Now().UnixMilli()
	return out
}

func (a *Agent) repositoryLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		a.repositoryMu.Lock()
		a.repositories.State = "scanning"
		a.repositoryMu.Unlock()
		scanCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		result := scanRepositories(scanCtx, a.ws)
		cancel()
		a.repositoryMu.Lock()
		a.repositories = result
		a.repositoryMu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-a.repositoryRefresh:
		}
	}
}

func (a *Agent) onRepositoryList(req *protocol.Envelope) {
	p, err := protocol.DecodePayload[protocol.RepositoryListPayload](req)
	if err != nil {
		a.replyError(req, err)
		return
	}
	if p.Offset < 0 || p.Limit < 0 || p.Limit > 50 {
		a.replyError(req, errors.New("无效仓库分页参数"))
		return
	}
	if p.Limit == 0 {
		p.Limit = 50
	}
	a.repositoryMu.Lock()
	result := a.repositories
	if p.Refresh && result.State != "scanning" {
		select {
		case a.repositoryRefresh <- struct{}{}:
		default:
		}
		a.repositories.State = "scanning"
		result.State = "scanning"
	}
	start := min(p.Offset, len(result.Repositories))
	end := start
	bytes := 0
	for end < len(result.Repositories) && end-start < p.Limit {
		encoded, _ := json.Marshal(result.Repositories[end])
		if end > start && bytes+len(encoded) > 24*1024 {
			break
		}
		bytes += len(encoded)
		end++
	}
	result.Repositories = append([]protocol.Repository{}, result.Repositories[start:end]...)
	if end < result.Total {
		next := end
		result.NextOffset = &next
	}
	a.repositoryMu.Unlock()
	a.reply(req, protocol.TypeRepositoryListed, result)
}
