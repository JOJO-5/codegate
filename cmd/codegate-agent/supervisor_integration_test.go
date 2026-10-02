package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jojo/codegate/internal/protocol"
	"github.com/jojo/codegate/internal/updatefile"
)

// The test executable acts as a real subprocess Agent, exercising version
// probes, readiness, progress loss, exits, restart and rollback on every OS.
func TestMain(m *testing.M) {
	if os.Getenv("CODEGATE_SUPERVISOR_FIXTURE") == "1" && len(os.Args) > 1 && (os.Args[1] == "run" || os.Args[1] == "version") {
		fixtureAgent()
		return
	}
	os.Exit(m.Run())
}
func fixtureAgent() {
	self, _ := os.Executable()
	match := regexp.MustCompile(`codegate-agent-(v[0-9]+\.[0-9]+\.[0-9]+)`).FindStringSubmatch(filepath.Base(self))
	version := "v9.0.0"
	if len(match) > 1 {
		version = match[1]
	}
	marker := os.Getenv("CODEGATE_AGENT_HEALTH_FILE")
	dir := filepath.Dir(marker)
	modeBytes, _ := os.ReadFile(filepath.Join(dir, version+".behavior"))
	mode := string(modeBytes)
	if os.Args[1] == "version" {
		if mode == "wrong-version" {
			version = "v0.0.0"
		}
		fmt.Println("codegate-agent " + version)
		return
	}
	f, _ := os.OpenFile(filepath.Join(dir, "fixture-launches.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if f != nil {
		fmt.Fprintln(f, version)
		f.Close()
	}
	start := time.Now()
	for {
		if version == "v9.0.0" {
			if _, err := os.Stat(filepath.Join(dir, "exit-bootstrap")); err == nil {
				_ = os.Remove(filepath.Join(dir, "exit-bootstrap"))
				return
			}
		}
		authenticated := mode != "auth-fail"
		if mode == "offline" && time.Since(start) > 60*time.Millisecond {
			authenticated = false
		}
		if mode != "stalled" || time.Since(start) < 20*time.Millisecond {
			_ = updatefile.WriteJSON(marker, protocol.ManagedHealth{Version: version, Authenticated: authenticated, UpdatedAt: time.Now().UnixMilli()})
		}
		if mode == "late-crash" && time.Since(start) > 100*time.Millisecond {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}
func fixtureCopy(t *testing.T, target string) {
	t.Helper()
	source, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0700)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.Copy(out, in)
	closeErr := out.Close()
	if err != nil {
		t.Fatal(err)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
}
func fixtureWait(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("subprocess condition timed out")
}
func fixtureRun(t *testing.T, mode string) (string, string, context.CancelFunc, <-chan error) {
	t.Helper()
	t.Setenv("CODEGATE_SUPERVISOR_FIXTURE", "1")
	state, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = updatefile.EnsureDir(updatePath(state, "")); err != nil {
		t.Fatal(err)
	}
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}
	bootstrap := filepath.Join(state, "bootstrap"+suffix)
	fixtureCopy(t, bootstrap)
	candidate := updatePath(state, "codegate-agent-v9.0.1-"+runtime.GOOS+"-"+runtime.GOARCH+suffix)
	fixtureCopy(t, candidate)
	os.WriteFile(updatePath(state, "v9.0.1.behavior"), []byte(mode), 0600)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	policy := supervisorPolicy{2 * time.Second, 1500 * time.Millisecond, 5 * time.Second, 10 * time.Millisecond, 10 * time.Millisecond}
	go func() { done <- supervise(ctx, "fixture-config", state, bootstrap, "v9.0.0", policy) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("supervisor: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("supervisor did not stop")
		}
	})
	fixtureWait(t, func() bool {
		data, _ := os.ReadFile(updatePath(state, "fixture-launches.log"))
		return strings.Contains(string(data), "v9.0.0")
	})
	hash, size, err := artifactDigest(state, candidate, "v9.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if err = updatefile.WriteJSON(updatePath(state, "pending.json"), pendingAgent{candidate, "v9.0.1", hash, size}); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(updatePath(state, "exit-bootstrap"), []byte("exit"), 0600)
	return state, candidate, cancel, done
}
func TestSupervisorRealHealthyUpgradeAndOfflineProgress(t *testing.T) {
	state, candidate, _, _ := fixtureRun(t, "offline")
	fixtureWait(t, func() bool {
		var s installedAgent
		return updatefile.ReadJSON(updatePath(state, "current.json"), &s) == nil && s.Path == candidate
	})
	time.Sleep(3200 * time.Millisecond) // More than two local progress deadlines.
	var stateNow installedAgent
	if err := updatefile.ReadJSON(updatePath(state, "current.json"), &stateNow); err != nil || stateNow.Path != candidate {
		t.Fatalf("offline Agent rolled back: %+v %v", stateNow, err)
	}
	if retryBlocked(state, "v9.0.1") {
		t.Fatal("healthy offline Agent quarantined")
	}
}
func TestSupervisorRealAuthenticationFailureReturnsToOldAgent(t *testing.T) {
	state, _, _, _ := fixtureRun(t, "auth-fail")
	fixtureWait(t, func() bool { return retryBlocked(state, "v9.0.1") })
	fixtureWait(t, func() bool {
		data, _ := os.ReadFile(updatePath(state, "fixture-launches.log"))
		return strings.Count(string(data), "v9.0.0") >= 2
	})
	var failure protocol.AgentUpdateFailure
	if err := updatefile.ReadJSON(updatePath(state, "last-failure.json"), &failure); err != nil || failure.Attempts != 1 || failure.RetryAfter <= failure.OccurredAt {
		t.Fatalf("failure not quarantined: %+v %v", failure, err)
	}
}
func TestSupervisorRealStalledCandidateRollsBack(t *testing.T) {
	state, _, _, _ := fixtureRun(t, "stalled")
	fixtureWait(t, func() bool { return retryBlocked(state, "v9.0.1") })
	fixtureWait(t, func() bool {
		var s installedAgent
		return updatefile.ReadJSON(updatePath(state, "current.json"), &s) == nil && s.Version == "v9.0.0"
	})
}
func TestSupervisorRealLateCrashesCountTowardRollback(t *testing.T) {
	state, _, _, _ := fixtureRun(t, "late-crash")
	fixtureWait(t, func() bool { return retryBlocked(state, "v9.0.1") })
	data, _ := os.ReadFile(updatePath(state, "fixture-launches.log"))
	if count := strings.Count(string(data), "v9.0.1"); count != 3 {
		t.Fatalf("expected exactly 3 attempts, got %d: %s", count, data)
	}
}
func TestSupervisorRejectsTamperingAndBacksOffSameVersion(t *testing.T) {
	state, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	updatefile.EnsureDir(updatePath(state, ""))
	name := "codegate-agent-v9.0.1-" + runtime.GOOS + "-" + runtime.GOARCH
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := updatePath(state, name)
	os.WriteFile(path, []byte("original"), 0700)
	hash, size, err := artifactDigest(state, path, "v9.0.1")
	if err != nil {
		t.Fatal(err)
	}
	req := pendingAgent{path, "v9.0.1", hash, size}
	os.WriteFile(path, []byte("tampered"), 0700)
	if err := verifyArtifact(state, req); err == nil {
		t.Fatal("tampered candidate accepted")
	}
	if err := writeUpdateFailure(state, "v9.0.1", "failed"); err != nil {
		t.Fatal(err)
	}
	var first protocol.AgentUpdateFailure
	updatefile.ReadJSON(updatePath(state, "last-failure.json"), &first)
	writeUpdateFailure(state, "v9.0.1", "failed again")
	var next protocol.AgentUpdateFailure
	updatefile.ReadJSON(updatePath(state, "last-failure.json"), &next)
	if next.Attempts != 2 || next.RetryAfter <= first.RetryAfter || !retryBlocked(state, "v9.0.1") || retryBlocked(state, "v9.0.2") {
		t.Fatalf("bad retry isolation: %+v %+v", first, next)
	}
}
