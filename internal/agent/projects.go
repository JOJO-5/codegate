package agent

import (
	"context"
	"errors"
	"github.com/jojo/codegate/internal/protocol"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// Portable single components avoid Windows device names and path traversal.
func validProjectName(name string) bool {
	if name == "" || name == "." || name == ".." || len(name) > 255 || !utf8.ValidString(name) || strings.TrimSpace(name) != name || strings.HasSuffix(name, ".") {
		return false
	}
	for _, r := range name {
		if r < 32 || strings.ContainsRune(`<>:"/\|?*`, r) {
			return false
		}
	}
	base := strings.ToUpper(strings.Split(name, ".")[0])
	if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" {
		return false
	}
	if len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9' {
		return false
	}
	if base == "COM¹" || base == "COM²" || base == "COM³" || base == "LPT¹" || base == "LPT²" || base == "LPT³" {
		return false
	}
	return true
}
func (a *Agent) createProject(ctx context.Context, p protocol.ProjectCreatePayload) (protocol.ProjectCreatedPayload, error) {
	var out protocol.ProjectCreatedPayload
	if (p.Kind != "file" && !validProjectName(p.Name)) || (p.FileName != "" && !validProjectName(p.FileName)) || len(p.Content) > 64<<10 || !utf8.ValidString(p.Content) {
		return out, errors.New("项目或文件名称无效，文本上限为64 KB")
	}
	if p.FileName == "" && p.Content != "" {
		return out, errors.New("请填写初始文件名称")
	}
	if p.Kind != "" && p.Kind != "project" && p.Kind != "file" {
		return out, errors.New("不支持的创建类型")
	}
	if p.Kind == "file" && p.FileName == "" {
		return out, errors.New("请填写文件名称")
	}
	parent, err := a.ws.Resolve(p.Parent)
	if err != nil {
		return out, err
	}
	info, err := os.Stat(parent)
	if err != nil || !info.IsDir() {
		return out, errors.New("父目录不存在或不是文件夹")
	}
	path := parent
	if p.Kind != "file" {
		path, err = a.ws.ResolveIn(parent, p.Name)
		if err != nil {
			return out, err
		}
		if err = os.Mkdir(path, 0755); err != nil {
			return out, errors.New("项目目录已存在或无法创建，请换一个名称")
		}
	}
	out.Path = path
	if p.FileName != "" {
		target := filepath.Join(path, p.FileName)
		f, e := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
		if e != nil {
			if p.Kind == "file" {
				return out, errors.New("文件已存在或无法创建，不会覆盖原文件")
			}
			out.Warning = "目录已创建，但初始文件未能创建；请在文件管理中重试"
			return out, nil
		}
		_, e = f.WriteString(p.Content)
		closeErr := f.Close()
		if e != nil || closeErr != nil {
			out.Warning = "目录已创建，但初始文件写入失败；请检查磁盘空间"
			return out, nil
		}
		out.FilePath = target
	}
	if p.InitGit && p.Kind != "file" {
		gitCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		_, _, e := runGit(gitCtx, path, "init")
		cancel()
		if e != nil {
			out.Warning = "项目已创建，但Git初始化失败，可在终端中重新初始化"
		} else {
			out.GitInitialized = true
		}
	}
	select {
	case a.repositoryRefresh <- struct{}{}:
	default:
	}
	return out, nil
}
func (a *Agent) onProjectCreate(req *protocol.Envelope) {
	finish, err := a.beginActivity()
	if err != nil {
		a.replyError(req, err)
		return
	}
	defer finish()
	p, err := protocol.DecodePayload[protocol.ProjectCreatePayload](req)
	if err != nil {
		a.replyError(req, err)
		return
	}
	a.workspaceMu.Lock()
	defer a.workspaceMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	result, err := a.createProject(ctx, p)
	if err != nil {
		a.replyError(req, protocol.NewError(protocol.CodeInvalidPayload, err.Error()))
		return
	}
	a.reply(req, protocol.TypeProjectCreated, result)
}
