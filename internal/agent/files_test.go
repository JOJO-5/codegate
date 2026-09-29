package agent

import (
	"context"
	"encoding/base64"
	"log/slog"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/jojo/codegate/internal/protocol"
	"github.com/jojo/codegate/internal/session"
	"github.com/jojo/codegate/internal/terminal"
)

type filesTestTerminal struct{ done chan struct{} }
func (f *filesTestTerminal) Start(context.Context, terminal.StartConfig) error { return nil }
func (f *filesTestTerminal) Read([]byte) (int,error) { <-f.done; return 0,io.EOF }
func (f *filesTestTerminal) Write(p []byte) (int,error) { return len(p),nil }
func (f *filesTestTerminal) Resize(uint16,uint16) error { return nil }
func (f *filesTestTerminal) Signal(terminal.Signal) error { return nil }
func (f *filesTestTerminal) Wait() terminal.ExitResult { return terminal.ExitResult{} }
func (f *filesTestTerminal) Close() error { select { case <-f.done: default: close(f.done) }; return nil }
func (f *filesTestTerminal) PID() int { return 123 }

func TestFilePathConfinedToSessionWorkspace(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil { t.Fatal(err) }
	one, two := filepath.Join(root,"one"), filepath.Join(root,"two")
	if err := os.Mkdir(one,0700); err != nil { t.Fatal(err) }
	if err := os.Mkdir(two,0700); err != nil { t.Fatal(err) }
	mgr := session.NewManager(func(ctx context.Context, cfg terminal.StartConfig) (terminal.Terminal,error) {
		return &filesTestTerminal{done: make(chan struct{})},nil
	}, session.Config{})
	defer mgr.CloseAll("test")
	device, user := uuid.New(), uuid.New()
	s, err := mgr.Create(context.Background(), session.CreateRequest{
		DeviceID:device, UserID:user, Cwd:one, Command:"test", Cols:80, Rows:24,
	})
	if err != nil { t.Fatal(err) }
	a := &Agent{ws:NewWorkspace([]string{root}),mgr:mgr}
	if p, err := a.filePath(s.ID.String(),"hello.txt"); err != nil || p != filepath.Join(one,"hello.txt") {
		t.Fatalf("file path = %q, err = %v",p,err)
	}
	if _, err := a.filePath(s.ID.String(),"../two/secret.txt"); !errors.Is(err,ErrPathNotAllowed) {
		t.Fatalf("sibling workspace escaped session cwd: %v",err)
	}
	if err := os.Symlink(two,filepath.Join(one,"link")); err == nil {
		if _, err := a.filePath(s.ID.String(),"link/secret.txt"); !errors.Is(err,ErrPathNotAllowed) {
			t.Fatalf("symlink escaped session cwd: %v",err)
		}
	}
}

func TestFileUploadChunksAtomically(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil { t.Fatal(err) }
	mgr := session.NewManager(func(context.Context, terminal.StartConfig) (terminal.Terminal,error) {
		return &filesTestTerminal{done: make(chan struct{})},nil
	}, session.Config{})
	defer mgr.CloseAll("test")
	sess, err := mgr.Create(context.Background(), session.CreateRequest{
		DeviceID:uuid.New(), UserID:uuid.New(), Cwd:root, Command:"test", Cols:80, Rows:24,
	})
	if err != nil { t.Fatal(err) }
	a := &Agent{ws:NewWorkspace([]string{root}), mgr:mgr, uploads:make(map[string]*fileUpload), log:slog.Default()}
	defer a.closeUploads()
	id := uuid.NewString()
	for i, piece := range []string{"abc","def"} {
		req, err := protocol.NewRequest(uuid.NewString(), protocol.TypeFileWrite, sess.ID.String(),
			protocol.FileWritePayload{
				SessionID:sess.ID.String(), UploadID:id, Path:"test.txt", Size:6,
				Offset:int64(i*3), Data:base64.StdEncoding.EncodeToString([]byte(piece)), Final:i==1,
			})
		if err != nil { t.Fatal(err) }
		a.onFileWrite(req)
		if i==0 {
			if _, err := os.Stat(filepath.Join(root,"test.txt")); !errors.Is(err,os.ErrNotExist) {
				t.Fatalf("partial upload published early: %v",err)
			}
		}
	}
	got, err := os.ReadFile(filepath.Join(root,"test.txt"))
	if err != nil || string(got)!="abcdef" { t.Fatalf("uploaded = %q, err=%v",got,err) }
	if len(a.uploads)!=0 { t.Fatalf("completed upload left state: %d",len(a.uploads)) }
}
