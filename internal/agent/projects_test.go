package agent

import (
	"context"
	"github.com/jojo/codegate/internal/protocol"
	"os"
	"path/filepath"
	"testing"
)

func TestProjectCreateAndFileBeforeCLI(t *testing.T) {
	root := t.TempDir()
	a := &Agent{ws: NewWorkspace([]string{root})}
	result, err := a.createProject(context.Background(), protocol.ProjectCreatePayload{Parent: root, Name: "my-project", FileName: "README.md", Content: "你好\n", InitGit: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Path != filepath.Join(root, "my-project") {
		t.Fatal(result)
	}
	data, err := os.ReadFile(result.FilePath)
	if err != nil || string(data) != "你好\n" {
		t.Fatal(string(data), err)
	}
	if _, err := a.createProject(context.Background(), protocol.ProjectCreatePayload{Parent: root, Name: "my-project"}); err == nil {
		t.Fatal("existing project overwritten")
	}
	file, err := a.createProject(context.Background(), protocol.ProjectCreatePayload{Kind: "file", Parent: result.Path, FileName: "main.py", Content: "print('hi')\n"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.createProject(context.Background(), protocol.ProjectCreatePayload{Kind: "file", Parent: result.Path, FileName: "main.py", Content: "overwrite"}); err == nil {
		t.Fatal("existing file overwritten")
	}
	data, _ = os.ReadFile(file.FilePath)
	if string(data) != "print('hi')\n" {
		t.Fatal("file changed")
	}
	for _, bad := range []string{"..", "../escape", "C:escape", "CON.txt", "COM¹.txt", "a/b", "a\\b", "tail.", "tail ", "NUL"} {
		if validProjectName(bad) {
			t.Fatal("bad name allowed", bad)
		}
	}
	for _, p := range []protocol.ProjectCreatePayload{{Parent: t.TempDir(), Name: "outside"}, {Parent: root, Name: "../escape"}, {Kind: "file", Parent: root, FileName: "../escape"}} {
		if _, err := a.createProject(context.Background(), p); err == nil {
			t.Fatal("root boundary bypass")
		}
	}
	private := filepath.Join(root, "agent-private")
	os.Mkdir(private, 0700)
	a.ws.ExcludePrivateState(private)
	if _, err := a.createProject(context.Background(), protocol.ProjectCreatePayload{Parent: private, Name: "no"}); err == nil {
		t.Fatal("private Agent state exposed")
	}
	outside := t.TempDir()
	link := filepath.Join(root, "escape-link")
	if os.Symlink(outside, link) == nil {
		if _, err := a.createProject(context.Background(), protocol.ProjectCreatePayload{Parent: link, Name: "no"}); err == nil {
			t.Fatal("symlink escape")
		}
	}
}
