package agent

import (
	"path/filepath"
	"testing"

	"github.com/jojo/codegate/internal/session"
)

func TestSessionNamesPersistAndKeepExplicitNames(t *testing.T) {
	cfg := DefaultConfig()
	cfg.StateDir = t.TempDir()
	a := &Agent{cfg: cfg, mgr: session.NewManager(nil, session.Config{})}
	cwd := filepath.Join(t.TempDir(), "codegate")
	for _, want := range []string{"codegate · Codex", "codegate · Codex 2"} {
		got, err := a.sessionName("", cwd, "Codex")
		if err != nil || got != want {
			t.Fatalf("got %q, %v; want %q", got, err, want)
		}
	}
	a = &Agent{cfg: cfg, mgr: session.NewManager(nil, session.Config{})}
	got, err := a.sessionName("", cwd, "Codex")
	if err != nil || got != "codegate · Codex 3" {
		t.Fatal(got, err)
	}
	got, err = a.sessionName("修复登录", cwd, "Codex")
	if err != nil || got != "修复登录" {
		t.Fatal(got, err)
	}
	got, err = a.sessionName("", cwd, "OpenCode")
	if err != nil || got != "codegate · OpenCode" {
		t.Fatal(got, err)
	}
}
