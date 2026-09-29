package agent

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jojo/codegate/internal/protocol"
)

const fileChunkLimit = 192 << 10
const fileUploadLimit = 100 << 20

type fileUpload struct {
	file *os.File
	temp string
	target string
	size int64
	written int64
	overwrite bool
	sessionID string
	createdAt time.Time
}

// filePath confines file access to both the configured workspace and this
// particular session's cwd. The browser never supplies an absolute root.
func (a *Agent) filePath(sessionID, rel string) (string, error) {
	id, err := uuid.Parse(sessionID)
	if err != nil {
		return "", fmt.Errorf("%w: 无效会话", protocol.ErrInvalidPayload)
	}
	sess, err := a.mgr.MustGet(id)
	if err != nil {
		return "", err
	}
	p, err := a.ws.ResolveIn(sess.Cwd, rel)
	if err != nil {
		return "", err
	}
	root, err := a.ws.Resolve(sess.Cwd)
	if err != nil {
		return "", err
	}
	if !pathWithin(root, p) {
		return "", ErrPathNotAllowed
	}
	return p, nil
}

func fileEntry(rel string, e os.DirEntry) (protocol.FileEntry, error) {
	info, err := e.Info()
	if err != nil {
		return protocol.FileEntry{}, err
	}
	return protocol.FileEntry{
		Name: e.Name(), Path: filepath.ToSlash(filepath.Join(rel, e.Name())),
		IsDir: e.IsDir(), Size: info.Size(), Modified: info.ModTime().UnixMilli(),
		Mode: info.Mode().String(), Symlink: e.Type()&os.ModeSymlink != 0,
	}, nil
}

func (a *Agent) onFileList(env *protocol.Envelope) {
	req, err := protocol.DecodePayload[protocol.FileListPayload](env)
	if err != nil { a.replyError(env, err); return }
	p, err := a.filePath(req.SessionID, req.Path)
	if err != nil { a.replyError(env, err); return }
	entries, err := os.ReadDir(p)
	if err != nil { a.replyError(env, err); return }
	if len(entries) > 1000 {
		a.replyError(env, fmt.Errorf("%w: 目录超过 1000 项", protocol.ErrInvalidPayload)); return
	}
	out := make([]protocol.FileEntry, 0, len(entries))
	for _, e := range entries {
		entry, err := fileEntry(req.Path, e)
		if err != nil { a.replyError(env, err); return }
		out = append(out, entry)
	}
	a.reply(env, protocol.TypeFileListed, protocol.FileListedPayload{Path: req.Path, Entries: out})
}

func (a *Agent) onFileStat(env *protocol.Envelope) {
	req, err := protocol.DecodePayload[protocol.FileStatPayload](env)
	if err != nil { a.replyError(env, err); return }
	p, err := a.filePath(req.SessionID, req.Path)
	if err != nil { a.replyError(env, err); return }
	info, err := os.Stat(p)
	if err != nil { a.replyError(env, err); return }
	result := protocol.FileStatResultPayload{
		Path: req.Path, Size: info.Size(), Modified: info.ModTime().UnixMilli(), IsDir: info.IsDir(),
	}
	if link, err := os.Lstat(p); err == nil { result.Symlink = link.Mode()&os.ModeSymlink != 0 }
	if !info.IsDir() {
		f, err := os.Open(p)
		if err != nil { a.replyError(env, err); return }
		buf := make([]byte, 64<<10)
		n, readErr := f.Read(buf)
		_ = f.Close()
		if readErr != nil && readErr != io.EOF { a.replyError(env, readErr); return }
		result.IsBinary = !utf8.Valid(buf[:n]) || strings.ContainsRune(string(buf[:n]), 0)
		if result.IsBinary { result.Encoding = "binary" } else { result.Encoding = "utf-8" }
	}
	a.reply(env, protocol.TypeFileStatResult, result)
}

func (a *Agent) onFileRead(env *protocol.Envelope) {
	req, err := protocol.DecodePayload[protocol.FileReadPayload](env)
	if err != nil { a.replyError(env, err); return }
	if req.Offset < 0 || req.Length <= 0 || req.Length > fileChunkLimit {
		a.replyError(env, fmt.Errorf("%w: 文件块必须在 1 到 196608 字节之间", protocol.ErrInvalidPayload)); return
	}
	p, err := a.filePath(req.SessionID, req.Path)
	if err != nil { a.replyError(env, err); return }
	f, err := os.Open(p)
	if err != nil { a.replyError(env, err); return }
	defer f.Close()
	info, err := f.Stat()
	if err != nil { a.replyError(env, err); return }
	if !info.Mode().IsRegular() || req.Offset > info.Size() {
		a.replyError(env, fmt.Errorf("%w: 文件偏移或类型不合法", protocol.ErrInvalidPayload)); return
	}
	buf := make([]byte, req.Length)
	n, err := f.ReadAt(buf, req.Offset)
	if err != nil && err != io.EOF { a.replyError(env, err); return }
	a.reply(env, protocol.TypeFileReadResult, protocol.FileReadResultPayload{
		Path: req.Path, Size: info.Size(), Offset: req.Offset,
		Data: base64.StdEncoding.EncodeToString(buf[:n]),
	})
}

func (a *Agent) onFileWrite(env *protocol.Envelope) {
	req, err := protocol.DecodePayload[protocol.FileWritePayload](env)
	if err != nil { a.replyError(env, err); return }
	if _, err = uuid.Parse(req.UploadID); err != nil || req.Size < 0 || req.Size > fileUploadLimit || req.Offset < 0 {
		a.replyError(env, fmt.Errorf("%w: 无效上传参数或文件超过 100 MB", protocol.ErrInvalidPayload)); return
	}
	data, err := base64.StdEncoding.DecodeString(req.Data)
	if err != nil || len(data) > fileChunkLimit {
		a.replyError(env, fmt.Errorf("%w: 上传块无效", protocol.ErrInvalidPayload)); return
	}
	target, err := a.filePath(req.SessionID, req.Path)
	if err != nil { a.replyError(env, err); return }
	if req.Path == "" || req.Path == "." || req.Offset+int64(len(data)) > req.Size {
		a.replyError(env, fmt.Errorf("%w: 上传路径或大小无效", protocol.ErrInvalidPayload)); return
	}
	a.uploadMu.Lock()
	defer a.uploadMu.Unlock()
	u := a.uploads[req.UploadID]
	if u == nil {
		if req.Offset != 0 { a.replyError(env, fmt.Errorf("%w: 上传必须从 0 开始", protocol.ErrInvalidPayload)); return }
		info, statErr := os.Lstat(target)
		if statErr == nil && (info.IsDir() || !req.Overwrite) {
			a.replyError(env, fmt.Errorf("%w: 目标已存在", protocol.ErrInvalidPayload)); return
		}
		if statErr != nil && !errors.Is(statErr, os.ErrNotExist) { a.replyError(env, statErr); return }
		temp, err := os.CreateTemp(filepath.Dir(target), ".codegate-upload-*")
		if err != nil { a.replyError(env, err); return }
		u = &fileUpload{file: temp, temp: temp.Name(), target: target, size: req.Size, overwrite: req.Overwrite, sessionID: req.SessionID, createdAt: time.Now()}
		a.uploads[req.UploadID] = u
	}
	if u.sessionID != req.SessionID || u.target != target || u.size != req.Size || u.written != req.Offset || u.overwrite != req.Overwrite {
		a.replyError(env, fmt.Errorf("%w: 上传块顺序或目标不匹配", protocol.ErrInvalidPayload)); return
	}
	n, err := u.file.Write(data)
	if err != nil || n != len(data) {
		a.cancelUpload(req.UploadID)
		a.replyError(env, fmt.Errorf("%w: 上传写入失败", protocol.ErrInvalidPayload)); return
	}
	u.written += int64(n)
	if !req.Final {
		a.reply(env, protocol.TypeFileWriteReady, protocol.FileWriteReadyPayload{TransferID: req.UploadID, Path: req.Path})
		return
	}
	if u.written != u.size {
		a.replyError(env, fmt.Errorf("%w: 上传字节数不匹配", protocol.ErrInvalidPayload)); return
	}
	if err := u.file.Sync(); err != nil { a.cancelUpload(req.UploadID); a.replyError(env, err); return }
	if err := u.file.Close(); err != nil { a.cancelUpload(req.UploadID); a.replyError(env, err); return }
	u.file = nil
	// Link creates the destination atomically without replacing an existing
	// file; an explicit overwrite uses Rename on the same filesystem.
	if u.overwrite { err = os.Rename(u.temp, u.target) } else { err = os.Link(u.temp, u.target) }
	if err != nil { a.cancelUpload(req.UploadID); a.replyError(env, err); return }
	if !u.overwrite { _ = os.Remove(u.temp) }
	delete(a.uploads, req.UploadID)
	a.reply(env, protocol.TypeFileWriteDone, protocol.FileWriteDonePayload{Path: req.Path, Size: u.size})
}

func (a *Agent) cancelUpload(id string) {
	u := a.uploads[id]
	if u == nil { return }
	if u.file != nil { _ = u.file.Close() }
	_ = os.Remove(u.temp)
	delete(a.uploads, id)
}

func (a *Agent) onFileCancel(env *protocol.Envelope) {
	req, err := protocol.DecodePayload[protocol.FileCancelPayload](env)
	if err != nil { a.replyError(env, err); return }
	if _, err := a.filePath(req.SessionID, ""); err != nil { a.replyError(env, err); return }
	a.uploadMu.Lock()
	if u := a.uploads[req.TransferID]; u != nil && u.sessionID == req.SessionID {
		a.cancelUpload(req.TransferID)
	}
	a.uploadMu.Unlock()
	a.reply(env, protocol.TypeFileWriteDone, protocol.FileWriteDonePayload{})
}

func (a *Agent) reapUploads() {
	a.uploadMu.Lock()
	defer a.uploadMu.Unlock()
	for id, u := range a.uploads {
		if time.Since(u.createdAt) > 10*time.Minute { a.cancelUpload(id) }
	}
}

func (a *Agent) closeUploads() {
	a.uploadMu.Lock()
	defer a.uploadMu.Unlock()
	for id := range a.uploads { a.cancelUpload(id) }
}
