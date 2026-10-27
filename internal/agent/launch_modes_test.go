package agent

import (
	"reflect"
	"testing"
)

func TestNativeLaunchModesUseOnlyNativeCLIArguments(t *testing.T) {
	for _, tc := range []struct {
		tool, mode string
		want       []string
	}{
		{"codex", "codex.read-only", []string{"-c", "default_permissions=:read-only", "-a", "on-request"}},
		{"codex", "codex.workspace", []string{"-c", "default_permissions=:workspace", "-a", "on-request"}},
		{"codex", "codex.full-access", []string{"-c", "default_permissions=:danger-full-access", "-a", "never"}},
		{"claude", "claude.plan", []string{"--permission-mode", "plan"}},
		{"claude", "claude.auto", []string{"--permission-mode", "auto"}},
		{"claude", "claude.full-access", []string{"--permission-mode", "bypassPermissions"}},
		{"opencode", "opencode.auto", []string{"--auto"}},
	} {
		spec := CommandSpec{ID: tc.tool, Command: tc.tool, Kind: KindTUI}
		cfg := Config{AllowedCommands: []CommandSpec{spec}}
		cmd, err := cfg.ResolveCommand(tc.tool, "", nil, false)
		if err != nil {
			t.Fatal(err)
		}
		if err := cfg.applyNativeLaunchMode(tc.tool, tc.mode, &cmd); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(cmd.Args, tc.want) {
			t.Fatalf("%s: %v", tc.mode, cmd.Args)
		}
		inherited, _ := cfg.ResolveCommand(tc.tool, "", nil, false)
		before := append([]string(nil), inherited.Args...)
		if err := cfg.applyNativeLaunchMode(tc.tool, "", &inherited); err != nil || !reflect.DeepEqual(inherited.Args, before) {
			t.Fatal("inherit changed original invocation")
		}
		if err := cfg.applyNativeLaunchMode(tc.tool, "browser invented flags", &inherited); err == nil {
			t.Fatal("untrusted native mode accepted")
		}
	}
	cfg := Config{AllowedCommands: []CommandSpec{{ID: "restricted", Command: "codex", Args: []string{"-s", "read-only"}, Kind: KindTUI}}}
	cmd, _ := cfg.ResolveCommand("restricted", "", nil, false)
	if err := cfg.applyNativeLaunchMode("restricted", "codex.full-access", &cmd); err == nil {
		t.Fatal("local fixed flags overridden")
	}
}
