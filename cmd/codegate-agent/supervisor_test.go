package main

import (
    "encoding/json"
    "os"
    "path/filepath"
    "runtime"
    "testing"
)

func TestConsumePendingRejectsEscapeAndConsumesTicket(t *testing.T) {
    state := t.TempDir()
    updates := filepath.Join(state, "updates")
    if err := os.Mkdir(updates, 0700); err != nil { t.Fatal(err) }
    req := pendingAgent{Path: filepath.Join(state, "outside"), Version: "v1.2.3"}
    b, _ := json.Marshal(req)
    path := filepath.Join(updates, "pending.json")
    if err := os.WriteFile(path, b, 0600); err != nil { t.Fatal(err) }
    if _, err := consumePending(state); err == nil { t.Fatal("accepted path outside updates") }
    if _, err := os.Stat(path); !os.IsNotExist(err) { t.Fatalf("pending ticket not consumed: %v", err) }
}

func TestValidUpdatePathRequiresExactPlatformArtifact(t *testing.T) {
    state := t.TempDir()
    name := "codegate-agent-v1.2.3-" + runtime.GOOS + "-" + runtime.GOARCH
    if runtime.GOOS == "windows" { name += ".exe" }
    if !validUpdatePath(state, filepath.Join(state, "updates", name), "v1.2.3") { t.Fatal("expected platform artifact rejected") }
    for _, path := range []string{
        filepath.Join(state, name),
        filepath.Join(state, "updates", "..", name),
        filepath.Join(state, "updates", "other"),
    } {
        if validUpdatePath(state, path, "v1.2.3") { t.Errorf("accepted unexpected path %q", path) }
    }
}
